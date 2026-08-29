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

// ListChannels returns the channels the bot is a member of, intersected
// with the allowlist when one is configured — the agent must not even
// learn the IDs of channels it may not read.
func (r *Relay) ListChannels(ctx context.Context, sessionKey string) (string, error) {
	if err := r.admitRead(ToolListChannels, sessionKey, ""); err != nil {
		return "", err
	}
	chans, _, err := r.cfg.API.GetConversationsForUserContext(ctx, &slack.GetConversationsForUserParameters{
		Types:           []string{"public_channel", "private_channel"},
		ExcludeArchived: true,
		Limit:           200,
	})
	if err != nil {
		return "", r.fail(ToolListChannels, sessionKey, "", fmt.Errorf("users.conversations: %w", err))
	}
	out := make([]channelInfo, 0, len(chans))
	for _, c := range chans {
		if !r.channelPermitted(c.ID) {
			continue
		}
		out = append(out, channelInfo{ID: c.ID, Name: c.Name})
	}
	r.cfg.Logf("slack-mcp: tool=%s session=%s outcome=ok channels=%d", ToolListChannels, sessionKey, len(out))
	return encode(out)
}

// Post posts a message as the bot, after four checks: the channel
// allowlist, the self-drive sentinel, the mass-ping strip, and the rate
// cap. Slack itself enforces the last one we rely on but do not
// implement — the app has no chat:write.public scope, so a post into a
// channel the bot has not been invited to simply fails.
func (r *Relay) Post(ctx context.Context, sessionKey, channel, threadTS, text string) (string, error) {
	if err := r.allowed(ToolPost, sessionKey, channel); err != nil {
		return "", err
	}
	if _, isDrive := r.cfg.SelfDrive.Accept(text); isDrive {
		return "", r.fail(ToolPost, sessionKey, channel,
			errors.New("refusing to post a message beginning with the operator's self-drive sentinel"))
	}
	if !r.posts.Allow() {
		return "", r.fail(ToolPost, sessionKey, channel,
			fmt.Errorf("slack_post rate cap exceeded; try again shortly"))
	}
	// Scrub is the structural belt to the prefix refusal above: if the
	// relay can never emit the sentinel at all, an agent-authored drive
	// message is impossible even in the forms the prefix check misses.
	clean := stripBroadcastPings(r.cfg.SelfDrive.Scrub(text))
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
	return r.fail(tool, sessionKey, channel,
		fmt.Errorf("channel %s is not in this bot's allowed_channel_ids; the operator must permit it", channel))
}

// channelPermitted reports whether channel is within the allowlist. An
// empty allowlist means "wherever the bot already is" — the same policy
// the message handler uses.
func (r *Relay) channelPermitted(channel string) bool {
	if len(r.cfg.AllowedChannelIDs) == 0 {
		return true
	}
	_, ok := r.cfg.AllowedChannelIDs[channel]
	return ok
}

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
