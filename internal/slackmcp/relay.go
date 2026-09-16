package slackmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/kfet/slack-acp/internal/ratelimit"
	"github.com/kfet/slack-acp/internal/slackproto"
)

// Read-limit clamps. The agent supplies `limit`, but it is untrusted
// input in the strong sense — an ambient thread participant can talk the
// agent into asking for anything — so the relay decides the real bound.
// Clamping also protects the agent's own context from a channel dump.
const (
	defaultReadLimit = 50
	maxReadLimit     = 100
)

// maxTextRunes truncates an individual message body in read output, for
// the same context-protection reason.
const maxTextRunes = 2000

// slack_search clamps. Search fans out over channels, so every bound
// here is doing double duty: protecting the agent's context, and keeping
// one tool call from eating the whole shared read budget.
const (
	defaultSearchLimit = 20
	maxSearchLimit     = 50
	defaultSearchDays  = 7
	maxSearchDays      = 30
	// searchScanPerChannel is how many recent messages are pulled per
	// channel. One conversations.history page, no paging: a search that
	// pages is an enumeration walk wearing a hat.
	searchScanPerChannel = 100
	// maxSearchChannels bounds the fanout independently of the rate
	// budget, so the shape of the failure does not depend on how much
	// budget happened to be left.
	maxSearchChannels = 50
	// maxSearchThreadFetches bounds include_threads, which is one extra
	// Slack call per threaded message.
	maxSearchThreadFetches = 20
)

// Truncation reasons reported to the agent. Silent truncation is the
// thing to avoid here: an agent that believes it searched everything
// will confidently report a message does not exist.
const (
	noteLimit   = "hit the match limit; more matches may exist"
	noteBudget  = "the shared Slack read budget ran out mid-scan; results are partial"
	noteFanout  = "too many channels to scan in one call; narrow with `channel`"
	noteThreads = "thread-reply scan hit its per-call cap; some replies were not scanned"
	// noteThreadFetchFailed covers a thread whose replies could not be
	// read at all (archived, deleted, permissions). The scan continues;
	// the shortfall is reported rather than swallowed.
	noteThreadFetchFailed = "one or more threads could not be read and were skipped"
)

// defaultPostsPerMinute is the slack_post rate cap when unset. Mirrors
// config.defaultAgentPostsPerMinute; the relay is constructed from
// config, which resolves the default before we see it, so this is the
// belt to that braces.
const defaultPostsPerMinute = 10

// defaultReadsPerMinute is the read-tool rate cap when unset. The
// per-call clamps bound one response; this bounds a *walk*. Without it
// "page back through #private-eng until January" is an unlimited number
// of 100-message calls, which is enumeration by another name.
const defaultReadsPerMinute = 60

// broadcastPing matches every form of Slack's channel-wide notification
// escape — bare (<!here>), labelled (<!here|@here>, which is the form
// Slack itself emits and happily re-parses), and user-group pings
// (<!subteam^S012|@oncall>).
//
// A literal-string blocklist looked sufficient and was not: posts go out
// with escape=false, so Slack parses the text, and the labelled form
// sailed straight through. An agent has no business emitting any of
// these, and "post @channel in #general saying…" is exactly the abuse
// this blocks. Stripped rather than rejected so a benign message still
// gets through.
var broadcastPing = regexp.MustCompile(`<!(?:here|channel|everyone|subteam\^[^>|]*)(?:\|[^>]*)?>`)

// API is the subset of *slack.Client the relay-hosted tools use.
// Narrowed to an interface so the Relay is testable without a network.
// *slack.Client satisfies it.
type API interface {
	GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error)
	GetConversationHistoryContext(ctx context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error)
	GetConversationsForUserContext(ctx context.Context, params *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error)
	PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
	GetUserInfoContext(ctx context.Context, user string) (*slack.User, error)
}

// RelayConfig configures a Relay.
type RelayConfig struct {
	// API is the relay's own Slack client. Required.
	API API
	// AllowedChannelIDs, when non-empty, is the exhaustive set of
	// channels the agent may touch through these tools — enforced on
	// every call, read or write, including the channel listing.
	AllowedChannelIDs map[string]struct{}
	// SelfDrive is the operator's self-drive hatch, or nil when it is
	// off (the default and the only safe production setting). Agent
	// posts are held to the same three guards the outbound streamer
	// uses: a post beginning with the sentinel is refused, the sentinel
	// is scrubbed from anything that does go out, and the resulting ts
	// is recorded so the relay can never re-consume its own message.
	//
	// Without this, read_write plus a live hatch would let injected
	// thread text make the agent post a message that drives another
	// session.
	SelfDrive *slackproto.SelfDrive
	// PostsPerMinute caps slack_post across all sessions. 0 → default.
	PostsPerMinute int
	// ReadsPerMinute caps the read tools across all sessions. 0 → default.
	ReadsPerMinute int
	// Logf receives one line per tool call. Required in practice;
	// defaults to a no-op so tests can stay quiet.
	Logf func(format string, v ...any)
	// Now is injected for the rate limiter's clock. nil → time.Now.
	Now func() time.Time
}

// Relay implements Controller by making every Slack call itself, with
// the relay's own client and the relay's own policy. The agent never
// sees a token; it sees only the results of calls the relay agreed to
// make.
type Relay struct {
	cfg   RelayConfig
	posts *ratelimit.Bucket
	reads *ratelimit.Bucket

	nameMu sync.Mutex
	names  map[string]string // user id -> display name
}

// NewRelay builds a Relay from cfg.
func NewRelay(cfg RelayConfig) *Relay {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Relay{
		cfg:   cfg,
		posts: ratelimit.New(cfg.PostsPerMinute, defaultPostsPerMinute, cfg.Now),
		reads: ratelimit.New(cfg.ReadsPerMinute, defaultReadsPerMinute, cfg.Now),
		names: make(map[string]string),
	}
}

// message is the per-message shape returned to the agent. User IDs are
// resolved to display names relay-side so the agent never needs a
// user-lookup tool (which would double as a workspace enumeration
// primitive).
type message struct {
	TS   string `json:"ts"`
	User string `json:"user"`
	// IsBot marks messages posted by an app rather than a human. Bot
	// messages carry an attacker-chosen `username`, so without this flag
	// anyone able to run a webhook into a readable channel could plant a
	// message the agent reads as coming from "operator" or "SYSTEM".
	IsBot    bool   `json:"is_bot,omitempty"`
	Text     string `json:"text"`
	ThreadTS string `json:"thread_ts,omitempty"`
}

// channelInfo is the per-channel shape returned by slack_list_channels.
//
// Deliberately minimal. An earlier version also reported is_private; the
// agent never branched on it, and it is a targeting oracle — it tells
// anyone who can prompt the agent exactly which of the bot's channels
// are worth exfiltrating first. Pure loss for an attacker, zero cost
// here.
type channelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ReadThread returns the messages of a thread as JSON.
func (r *Relay) ReadThread(ctx context.Context, sessionKey, channel, threadTS string, limit int) (string, error) {
	if err := r.allowed(ToolReadThread, sessionKey, channel); err != nil {
		return "", err
	}
	if err := r.admitRead(ToolReadThread, sessionKey, channel); err != nil {
		return "", err
	}
	msgs, _, _, err := r.cfg.API.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: threadTS,
		Limit:     clampLimit(limit),
		Inclusive: true,
	})
	if err != nil {
		return "", r.fail(ToolReadThread, sessionKey, channel, fmt.Errorf("conversations.replies: %w", err))
	}
	out := r.render(ctx, msgs)
	r.cfg.Logf("slack-mcp: tool=%s session=%s channel=%s thread_ts=%s outcome=ok messages=%d",
		ToolReadThread, sessionKey, channel, threadTS, len(out))
	return encode(out)
}

// ReadChannel returns recent channel messages as JSON.
func (r *Relay) ReadChannel(ctx context.Context, sessionKey, channel string, limit int, oldest string) (string, error) {
	if err := r.allowed(ToolReadChannel, sessionKey, channel); err != nil {
		return "", err
	}
	if err := r.admitRead(ToolReadChannel, sessionKey, channel); err != nil {
		return "", err
	}
	resp, err := r.cfg.API.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
		ChannelID: channel,
		Limit:     clampLimit(limit),
		Oldest:    oldest,
	})
	if err != nil {
		return "", r.fail(ToolReadChannel, sessionKey, channel, fmt.Errorf("conversations.history: %w", err))
	}
	out := r.render(ctx, resp.Messages)
	r.cfg.Logf("slack-mcp: tool=%s session=%s channel=%s outcome=ok messages=%d",
		ToolReadChannel, sessionKey, channel, len(out))
	return encode(out)
}

// channelListLimit is one users.conversations page. Deliberately not
// paged: paging turns "list my channels" into an unbounded enumeration
// walk. A bot in more channels than this gets a SHORT list — which is
// reported rather than silently returned, see permittedChannels.
const channelListLimit = 200

// noteChannelList is the truncation reason for a bot in more channels
// than one users.conversations page holds.
const noteChannelList = "the bot is in more channels than one listing page holds; some were not scanned — narrow with `channel`"

// channelList is slack_list_channels' envelope. It is an object rather
// than a bare array purely so truncation can be reported: the read
// tools' contract is that a short answer always says it is short.
type channelList struct {
	Truncated bool          `json:"truncated"`
	Note      string        `json:"note,omitempty"`
	Channels  []channelInfo `json:"channels"`
}

// ListChannels returns the channels the bot is a member of, intersected
// with the allowlist when one is configured — the agent must not even
// learn the IDs of channels it may not read.
func (r *Relay) ListChannels(ctx context.Context, sessionKey string) (string, error) {
	out, more, err := r.permittedChannels(ctx, ToolListChannels, sessionKey)
	if err != nil {
		return "", err
	}
	res := channelList{Truncated: more, Channels: out}
	if more {
		res.Note = noteChannelList
	}
	r.cfg.Logf("slack-mcp: tool=%s session=%s outcome=ok channels=%d truncated=%v",
		ToolListChannels, sessionKey, len(out), more)
	return encode(res)
}

// permittedChannels lists the bot's channels intersected with the
// allowlist, spending one read token. Shared by slack_list_channels and
// by slack_search's fanout so the two can never disagree about what the
// session may see.
//
// The second return reports that Slack had MORE channels than the one
// page we ask for. Both callers surface it: an unreported short list is
// exactly the silent truncation the rest of this package refuses to do,
// and it is worse here than anywhere else because it makes the search
// fanout quietly narrower than the operator's allowlist.
func (r *Relay) permittedChannels(ctx context.Context, tool, sessionKey string) ([]channelInfo, bool, error) {
	if err := r.admitRead(tool, sessionKey, ""); err != nil {
		return nil, false, err
	}
	chans, cursor, err := r.cfg.API.GetConversationsForUserContext(ctx, &slack.GetConversationsForUserParameters{
		Types:           []string{"public_channel", "private_channel"},
		ExcludeArchived: true,
		Limit:           channelListLimit,
	})
	if err != nil {
		return nil, false, r.fail(tool, sessionKey, "", fmt.Errorf("users.conversations: %w", err))
	}
	out := make([]channelInfo, 0, len(chans))
	for _, c := range chans {
		if !r.channelPermitted(c.ID) {
			continue
		}
		out = append(out, channelInfo{ID: c.ID, Name: c.Name})
	}
	return out, cursor != "", nil
}

// searchMatch is one hit: the rendered message plus where it was found.
// Search spans channels, so unlike the single-channel read tools the
// result has to say which one each message came from.
type searchMatch struct {
	Channel     string `json:"channel"`
	ChannelName string `json:"channel_name,omitempty"`
	message
}

// searchResult is slack_search's envelope. The bookkeeping fields exist
// so the agent can tell a real "no such message" from "the scan stopped
// early" — the tool has no index behind it, and an agent that assumes
// otherwise will report absence as fact.
type searchResult struct {
	Query           string        `json:"query"`
	ChannelsScanned int           `json:"channels_scanned"`
	ChannelsInScope int           `json:"channels_in_scope"`
	Oldest          string        `json:"oldest"`
	Truncated       bool          `json:"truncated"`
	Note            string        `json:"note,omitempty"`
	Matches         []searchMatch `json:"matches"`
}

// Search is a bounded local fanout, not Slack search. It pulls one page
// of conversations.history from each channel the session may read
// (exactly the allowlist the read tools enforce — never wider), matches
// the query substring relay-side, and returns hits with the same
// hygiene as the read tools.
//
// It calls no search.* Slack method and needs no search:read scope, so
// it introduces no user token. The cost of that choice is that it is a
// scan: bounded by channel count, page size, a time window, and the
// shared read budget. Every one of those bounds, when hit, sets
// `truncated` and a `note` — partial results are always labelled,
// because a silently truncated search is worse than no search.
func (r *Relay) Search(ctx context.Context, sessionKey string, p SearchParams) (string, error) {
	query := strings.TrimSpace(p.Query)
	if query == "" {
		return "", r.fail(ToolSearch, sessionKey, p.Channel, errors.New("query is required"))
	}
	targets, moreChannels, err := r.searchTargets(ctx, sessionKey, p.Channel)
	if err != nil {
		return "", err
	}
	limit := clampSearchLimit(p.Limit)
	oldest := fmt.Sprintf("%d.000000", r.now().Add(-time.Duration(clampSearchDays(p.Days))*24*time.Hour).Unix())

	res := searchResult{Query: query, ChannelsInScope: len(targets), Oldest: oldest, Matches: []searchMatch{}}
	if moreChannels {
		res.truncate(noteChannelList)
	}
	if len(targets) > maxSearchChannels {
		targets = targets[:maxSearchChannels]
		res.truncate(noteFanout)
	}
	needle := strings.ToLower(query)
	threadFetches := 0

	for _, t := range targets {
		if len(res.Matches) >= limit {
			res.truncate(noteLimit)
			break
		}
		// One token per channel: a fanout costs N reads, not 1.
		if !r.reads.Allow() {
			res.truncate(noteBudget)
			r.cfg.Logf("slack-mcp: tool=%s session=%s channel=%s outcome=denied err=%s",
				ToolSearch, sessionKey, t.ID, noteBudget)
			break
		}
		resp, err := r.cfg.API.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
			ChannelID: t.ID,
			Limit:     searchScanPerChannel,
			Oldest:    oldest,
		})
		if err != nil {
			return "", r.fail(ToolSearch, sessionKey, t.ID, fmt.Errorf("conversations.history: %w", err))
		}
		res.ChannelsScanned++
		pool := resp.Messages
		if p.IncludeThreads {
			replies, note := r.searchThreads(ctx, sessionKey, t.ID, resp.Messages, &threadFetches)
			pool = append(pool, replies...)
			if note != "" {
				res.truncate(note)
			}
		}
		for _, m := range r.matches(ctx, pool, needle) {
			if len(res.Matches) >= limit {
				res.truncate(noteLimit)
				break
			}
			res.Matches = append(res.Matches, searchMatch{Channel: t.ID, ChannelName: t.Name, message: m})
		}
	}
	r.cfg.Logf("slack-mcp: tool=%s session=%s channel=%s outcome=ok query=%q channels=%d/%d matches=%d truncated=%v",
		ToolSearch, sessionKey, p.Channel, truncate(query, 120), res.ChannelsScanned, res.ChannelsInScope,
		len(res.Matches), res.Truncated)
	return encode(res)
}

// truncate marks the result partial with a reason. Later reasons win:
// the last bound hit is the one that actually stopped the scan.
func (s *searchResult) truncate(note string) {
	s.Truncated, s.Note = true, note
}

// searchTargets resolves the channel set to scan. An explicit channel
// goes through the same allowlist check as any read; otherwise the scan
// covers exactly the channels slack_list_channels would disclose.
func (r *Relay) searchTargets(ctx context.Context, sessionKey, channel string) ([]channelInfo, bool, error) {
	if channel != "" {
		if err := r.allowed(ToolSearch, sessionKey, channel); err != nil {
			return nil, false, err
		}
		return []channelInfo{{ID: channel}}, false, nil
	}
	return r.permittedChannels(ctx, ToolSearch, sessionKey)
}

// searchThreads pulls replies for the threaded messages in a scanned
// page, so a match buried in a thread is findable. Each fetch is a read
// token and counts against a per-call cap; hitting either returns what
// was gathered plus the note explaining the shortfall.
//
// A FAILED fetch is a shortfall too, not a fatal error. An earlier
// version propagated it and aborted the whole search, throwing away
// every match already found in every other channel — one archived or
// deleted thread turned a good search into nothing. A thread that
// cannot be read is skipped, the result is marked truncated, and the
// failure is logged like any other refusal.
func (r *Relay) searchThreads(ctx context.Context, sessionKey, channel string, msgs []slack.Message, fetches *int) ([]slack.Message, string) {
	var out []slack.Message
	note := ""
	for _, m := range msgs {
		if m.ReplyCount <= 0 {
			continue
		}
		if *fetches >= maxSearchThreadFetches {
			return out, noteThreads
		}
		if !r.reads.Allow() {
			return out, noteBudget
		}
		*fetches++
		replies, _, _, err := r.cfg.API.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: channel,
			Timestamp: m.Timestamp,
			Limit:     maxReadLimit,
			Inclusive: true,
		})
		if err != nil {
			r.fail(ToolSearch, sessionKey, channel, fmt.Errorf("conversations.replies ts=%s: %w", m.Timestamp, err))
			note = noteThreadFetchFailed
			continue
		}
		out = append(out, replies...)
	}
	return out, note
}

// matches filters a scanned pool down to the substring hits and renders
// them. Deduplicated by ts because conversations.replies repeats the
// thread parent that conversations.history already returned.
func (r *Relay) matches(ctx context.Context, msgs []slack.Message, needle string) []message {
	seen := make(map[string]struct{}, len(msgs))
	hits := make([]slack.Message, 0, len(msgs))
	for _, m := range msgs {
		if _, dup := seen[m.Timestamp]; dup {
			continue
		}
		seen[m.Timestamp] = struct{}{}
		if !strings.Contains(strings.ToLower(m.Text), needle) {
			continue
		}
		hits = append(hits, m)
	}
	return r.render(ctx, hits)
}

// now is the relay's clock, injected in tests.
func (r *Relay) now() time.Time {
	if r.cfg.Now != nil {
		return r.cfg.Now()
	}
	return time.Now()
}

// Post posts a message as the bot, after five checks: the channel
// allowlist, the self-drive sentinel, the mass-ping strip, the
// resulting-body emptiness check, and the rate cap. Slack itself
// enforces the last one we rely on but do not implement — the app has
// no chat:write.public scope, so a post into a channel the bot has not
// been invited to simply fails.
func (r *Relay) Post(ctx context.Context, sessionKey, channel, threadTS, text string) (string, error) {
	if err := r.allowed(ToolPost, sessionKey, channel); err != nil {
		return "", err
	}
	if _, isDrive := r.cfg.SelfDrive.Accept(text); isDrive {
		return "", r.fail(ToolPost, sessionKey, channel,
			errors.New("refusing to post a message beginning with the operator's self-drive sentinel"))
	}
	// Scrub is the structural belt to the prefix refusal above: if the
	// relay can never emit the sentinel at all, an agent-authored drive
	// message is impossible even in the forms the prefix check misses.
	//
	// Both rewrites happen BEFORE the rate cap is charged. A post whose
	// entire body was a broadcast ping (`@channel`) strips to nothing,
	// and chat.postMessage rejects an empty text — so charging first
	// would spend one of ten posts per minute on a call that was never
	// going to land, and the agent would read the resulting `no_text` as
	// a mysterious Slack failure instead of the refusal it is.
	clean := stripBroadcastPings(r.cfg.SelfDrive.Scrub(text))
	if clean == "" {
		return "", r.fail(ToolPost, sessionKey, channel,
			errors.New("refusing to post an empty message; the text was nothing but channel-wide pings, which this bot strips"))
	}
	if !r.posts.Allow() {
		return "", r.fail(ToolPost, sessionKey, channel,
			errors.New("slack_post rate cap exceeded; try again shortly"))
	}
	// thread_ts is appended only when set: slack-go's MsgOptionTS writes
	// the parameter unconditionally, and an empty thread_ts is rejected
	// by Slack rather than treated as "top level".
	opts := []slack.MsgOption{slack.MsgOptionText(clean, false)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, ts, err := r.cfg.API.PostMessageContext(ctx, channel, opts...)
	if err != nil {
		return "", r.fail(ToolPost, sessionKey, channel, fmt.Errorf("chat.postMessage: %w", err))
	}
	// Every ts the relay posts goes into the self-posted memory, whatever
	// path posted it — the streamer's and this one must not diverge.
	r.cfg.SelfDrive.RecordTS(ts)
	r.cfg.Logf("slack-mcp: tool=%s session=%s channel=%s thread_ts=%s outcome=ok ts=%s text=%q",
		ToolPost, sessionKey, channel, threadTS, ts, truncate(clean, 120))
	return fmt.Sprintf("Posted to %s (ts %s).", channel, ts), nil
}

// allowed enforces the channel allowlist and logs a denial. The error
// names the channel so the agent can report something actionable rather
// than retrying blindly.
func (r *Relay) allowed(tool, sessionKey, channel string) error {
	if r.channelPermitted(channel) {
		return nil
	}
	if len(r.cfg.AllowedChannelIDs) == 0 && isDM(channel) {
		return r.fail(tool, sessionKey, channel,
			fmt.Errorf("channel %s is a direct message; these tools do not reach DMs unless the operator names one in allowed_channel_ids", channel))
	}
	return r.fail(tool, sessionKey, channel,
		fmt.Errorf("channel %s is not in this bot's allowed_channel_ids; the operator must permit it", channel))
}

// channelPermitted reports whether channel is within the allowlist.
//
// An explicit allowlist is the operator's exhaustive answer and decides
// on its own — if they name a DM conversation, they meant it.
//
// An EMPTY allowlist means "wherever the bot already is", the same
// policy the message handler uses, with one subtraction: DM
// conversations (D…) are refused. That subtraction is not cosmetic. The
// bot holds a 1:1 DM with every person who has ever messaged it, and
// these tools are driven by an agent steered by ambient thread text
// written by someone else entirely — so without it, anyone who can
// prompt the bot in a channel could have it read a *different* person's
// private conversation with the bot and paste it back. "Nothing the bot
// cannot already see" is true of the DM; it is the AUDIENCE that
// widens. Group DMs are unreachable for the same purpose by a different
// route: the app has no mpim:history scope.
func (r *Relay) channelPermitted(channel string) bool {
	if len(r.cfg.AllowedChannelIDs) == 0 {
		return !isDM(channel)
	}
	_, ok := r.cfg.AllowedChannelIDs[channel]
	return ok
}

// isDM reports whether a Slack conversation id names a 1:1 DM. Slack's
// id prefixes are stable and documented: C public channel, G private
// channel, D im.
func isDM(channel string) bool { return strings.HasPrefix(channel, "D") }

// admitRead applies the read rate cap. Denials are logged like any other
// refusal so an operator can see an enumeration walk being cut off.
func (r *Relay) admitRead(tool, sessionKey, channel string) error {
	if r.reads.Allow() {
		return nil
	}
	return r.fail(tool, sessionKey, channel,
		errors.New("slack read rate cap exceeded; try again shortly"))
}

// fail logs a refused or failed call and returns the error unchanged, so
// every non-ok outcome is auditable from one place.
func (r *Relay) fail(tool, sessionKey, channel string, err error) error {
	r.cfg.Logf("slack-mcp: tool=%s session=%s channel=%s outcome=denied err=%v", tool, sessionKey, channel, err)
	return err
}

// render converts Slack messages into the agent-facing shape, dropping
// empty ones and resolving user IDs to names.
func (r *Relay) render(ctx context.Context, msgs []slack.Message) []message {
	out := make([]message, 0, len(msgs))
	for _, m := range msgs {
		text := strings.TrimSpace(m.Text)
		if text == "" {
			continue
		}
		out = append(out, message{
			TS:       m.Timestamp,
			User:     r.userName(ctx, m),
			IsBot:    m.BotID != "",
			Text:     truncate(text, maxTextRunes),
			ThreadTS: m.ThreadTimestamp,
		})
	}
	return out
}

// userName resolves a message's author to a display name, caching
// lookups for the life of the process. Bot messages carry a username
// instead of a user id; fall back to the raw id when resolution fails so
// output is never empty.
func (r *Relay) userName(ctx context.Context, m slack.Message) string {
	if m.User == "" {
		if m.Username != "" {
			return m.Username
		}
		return "unknown"
	}
	r.nameMu.Lock()
	cached, ok := r.names[m.User]
	r.nameMu.Unlock()
	if ok {
		return cached
	}
	name := m.User
	if u, err := r.cfg.API.GetUserInfoContext(ctx, m.User); err == nil && u != nil {
		switch {
		case u.Profile.DisplayName != "":
			name = u.Profile.DisplayName
		case u.RealName != "":
			name = u.RealName
		case u.Name != "":
			name = u.Name
		}
	}
	r.nameMu.Lock()
	r.names[m.User] = name
	r.nameMu.Unlock()
	return name
}

// clampLimit bounds an agent-supplied read limit.
func clampLimit(n int) int {
	if n <= 0 {
		return defaultReadLimit
	}
	if n > maxReadLimit {
		return maxReadLimit
	}
	return n
}

// clampSearchLimit and clampSearchDays bound the two agent-supplied
// search knobs. Same reasoning as clampLimit: the argument arrives from
// a process steered by attacker-controlled thread text, so the relay,
// not the agent, decides how far a scan reaches.
func clampSearchLimit(n int) int {
	if n <= 0 {
		return defaultSearchLimit
	}
	if n > maxSearchLimit {
		return maxSearchLimit
	}
	return n
}

func clampSearchDays(n int) int {
	if n <= 0 {
		return defaultSearchDays
	}
	if n > maxSearchDays {
		return maxSearchDays
	}
	return n
}

// stripBroadcastPings removes @channel/@here/@everyone and user-group
// escapes in every syntactic form Slack accepts.
func stripBroadcastPings(text string) string {
	return strings.TrimSpace(broadcastPing.ReplaceAllString(text, ""))
}

// truncate shortens s to at most n runes, appending an ellipsis.
func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// encode renders the tool result as indented JSON. The concrete,
// string-only shapes above always marshal; a failure would mean someone
// passed a type that cannot, which is a programming error rather than a
// runtime condition any caller could handle — hence the panic. The
// signature keeps the (string, error) shape the Controller methods
// return so call sites stay one-line.
func encode(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic("slackmcp: marshal tool result: " + err.Error())
	}
	return string(b), nil
}
