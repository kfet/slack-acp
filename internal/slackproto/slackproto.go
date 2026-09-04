// Package slackproto adapts slack-go's Socket Mode client to a small
// handler-shaped surface used by the relay.
//
// Inbound: Slack delivers AppMention and message.im events. The Client
// dispatches them to a Handler.
//
// Outbound: PostStreamer opens a thread reply and lets callers push
// incremental text. Updates are throttled (Slack chat.update rate limits
// hit hard at >1/sec per channel).
package slackproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	kitlog "github.com/kfet/acp-kit/log"

	"github.com/kfet/slack-acp/internal/journal"
	"github.com/kfet/slack-acp/internal/ratelimit"
)

// Event is a normalised inbound message the handler cares about.
type Event struct {
	UserID    string // sender
	BotUserID string // our bot's user id (for stripping mentions)
	ChannelID string
	// ThreadTS is the parent thread (== TS for a top-level message).
	ThreadTS string
	// TS of this incoming message.
	TS string
	// Text of the message, with our bot mention stripped.
	Text string
	// IsDM is true for direct-message conversations (channel.im).
	IsDM bool
	// IsMention is true when this event came from the app_mention path,
	// i.e. the sender explicitly @-mentioned the bot. It must be carried
	// as a flag because Text has the mention STRIPPED by the time the
	// handler sees it — re-deriving "was this a mention?" from Text is
	// what broke channel mentions before v0.4.2.
	IsMention bool
	// SelfDrive marks an event admitted by the self-drive escape hatch
	// (a bot-authored message carrying the configured sentinel prefix).
	// The handler uses it to bypass the *user* allowlist — and nothing
	// else; the channel allowlist and rate cap still apply.
	SelfDrive bool
}

// Handler processes a normalised event. Implementations should return
// quickly; long work belongs in goroutines they own.
type Handler interface {
	Handle(ctx context.Context, ev Event)
}

// Client is the Socket Mode client wrapper.
type Client struct {
	api       *slack.Client
	sm        *socketmode.Client
	botUserID string
	handler   Handler
	selfDrive *SelfDrive // nil = hatch off (the default)
	// humanAuthors names user ids whose API-authored (bot_id-stamped)
	// messages are treated as human. Nil/empty = the strict default.
	humanAuthors map[string]struct{}
	// humanAuthorPerMinute overrides the reclassification cap. 0 =
	// default.
	humanAuthorPerMinute int
	// humanAuthorRate bounds how often that reclassification may fire.
	// Nil until Run arms it, and a nil bucket admits nothing.
	humanAuthorRate *ratelimit.Bucket
	// chargedMu/chargedKey collapse the TWO envelopes Slack delivers
	// for one tagged message into a single charge. See charge().
	chargedMu  sync.Mutex
	chargedKey string
	// appID is OUR OWN Slack app id, learned at startup via bots.info.
	// The reclassification requires the posting app to be this one, so
	// a third-party app posting as a named user is still refused.
	// Empty means we never learned it — in which case the
	// reclassification refuses everything, i.e. it fails CLOSED.
	appID string
}

// Option customises a Client.
type Option func(*Client)

// WithSelfDrive enables the self-drive escape hatch. Omit it (or pass a
// hatch built from an empty sentinel) to keep the hatch off, which is
// the correct production configuration.
func WithSelfDrive(d *SelfDrive) Option {
	return func(c *Client) { c.selfDrive = d }
}

// WithHumanAuthors names user ids whose API-authored messages count as
// human. See Config.HumanAuthorUserIDs for why this is necessary and
// why it cannot open a self-loop. Omit it for production.
func WithHumanAuthors(ids map[string]struct{}) Option {
	return func(c *Client) { c.humanAuthors = ids }
}

// WithHumanAuthorRate overrides the per-minute cap on the human-author
// reclassification. 0 uses the default.
func WithHumanAuthorRate(perMinute int) Option {
	return func(c *Client) { c.humanAuthorPerMinute = perMinute }
}

// defaultHumanAuthorPerMinute bounds the reclassification. It is the
// loop backstop: if every other guard failed, this is what turns a
// runaway spiral into a handful of wasted prompts and a loud log.
//
// Sized against the only intended consumer, `slack-acp verify`, which
// costs about seven tokens per run — a backstop that throttles the
// legitimate user is a bug, not a safety feature.
const defaultHumanAuthorPerMinute = 30

// New constructs a Client. botToken is xoxb-, appToken is xapp-.
func New(botToken, appToken string, h Handler, opts ...Option) (*Client, error) {
	if botToken == "" || appToken == "" {
		return nil, errors.New("slackproto: bot_token and app_token required")
	}
	if !strings.HasPrefix(botToken, "xoxb-") {
		return nil, fmt.Errorf("slackproto: bot_token must start with xoxb-")
	}
	if !strings.HasPrefix(appToken, "xapp-") {
		return nil, fmt.Errorf("slackproto: app_token must start with xapp-")
	}
	api := slack.New(botToken, slackOptions(appToken)...)
	sm := socketmode.New(api)
	c := &Client{api: api, sm: sm, handler: h}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// slackOptions builds the slack.Client options, honouring the
// SLACK_API_BASE env var (test-only redirect of the Web API; production
// no-op when unset).
func slackOptions(appToken string) []slack.Option {
	opts := []slack.Option{slack.OptionAppLevelToken(appToken)}
	if base := os.Getenv("SLACK_API_BASE"); base != "" {
		opts = append(opts, slack.OptionAPIURL(base))
	}
	return opts
}

// Run authenticates, captures the bot's user id, and processes events
// until ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	auth, err := c.api.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("slack auth.test: %w", err)
	}
	c.botUserID = auth.UserID
	kitlog.Debugf("slack: connected as %s (%s)", auth.User, auth.UserID)

	// Learn our own app id, but only when the reclassification is
	// actually configured — no config, no extra API call.
	//
	// This must be OUR app: Slack mints a SEPARATE bot_id for an app's
	// user-token surface (measured: bot token posts carry
	// B0B3VCV278U, user token posts B0BNE4AUS9L, same app
	// A0B3PMFHUSJ), so bot_id equality is the wrong test and app_id is
	// the right one. If we cannot learn it, the reclassification stays
	// shut — failing closed is the only safe direction for a guard.
	if len(c.humanAuthors) > 0 {
		c.humanAuthorRate = ratelimit.New(c.humanAuthorPerMinute, defaultHumanAuthorPerMinute, nil)
		if info, ierr := c.api.GetBotInfoContext(ctx, slack.GetBotInfoParameters{Bot: auth.BotID}); ierr == nil {
			c.appID = info.AppID
			log.Printf("slack-acp: human_author_user_ids active for %s — reclassification is limited to app %s at %d/min; the relay still refuses its OWN messages unconditionally",
				strings.Join(sortedKeys(c.humanAuthors), ","), c.appID, c.humanAuthorPerMinuteOrDefault())
		} else {
			log.Printf("slack-acp: WARNING could not determine our own app id (bots.info: %v) — human_author_user_ids is INERT, every API-authored message stays refused. This fails closed on purpose.", ierr)
		}
	}

	go c.consume(ctx, c.sm.Events)
	return c.sm.RunContext(ctx)
}

// API returns the underlying *slack.Client (used by PostStreamer).
func (c *Client) API() *slack.Client { return c.api }

// BotUserID returns the cached bot user id (after Run).
func (c *Client) BotUserID() string { return c.botUserID }

func (c *Client) consume(ctx context.Context, events <-chan socketmode.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			c.dispatch(ctx, evt)
		}
	}
}

func (c *Client) dispatch(ctx context.Context, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeConnecting, socketmode.EventTypeConnected, socketmode.EventTypeHello:
		kitlog.Debugf("slack: %s", evt.Type)
	case socketmode.EventTypeEventsAPI:
		api, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		// Ack derefs Request; socketmode always sets it for an
		// EventsAPI event, but a nil here would panic the consume
		// goroutine and take the relay's whole ingest down. Cheap to
		// refuse instead.
		if evt.Request == nil {
			kitlog.Debugf("slack: events_api envelope with no request; ignoring")
			return
		}
		c.sm.Ack(*evt.Request)
		// slack-go does not surface app_id on the typed event structs,
		// but Slack does deliver it — and it is the field that tells
		// us WHICH app posted a message. Pull it off the raw envelope
		// rather than doing without.
		c.handleEventsAPI(ctx, api, appIDOf(evt.Request))
	case socketmode.EventTypeDisconnect:
		kitlog.Debugf("slack: disconnected")
	default:
		kitlog.Debugf("slack: ignoring %s", evt.Type)
	}
}

func (c *Client) handleEventsAPI(ctx context.Context, api slackevents.EventsAPIEvent, appID string) {
	if api.Type != slackevents.CallbackEvent {
		return
	}
	switch ev := api.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		rec := journal.Record{
			Stage:    journal.StageProto,
			Path:     journal.PathAppMention,
			Channel:  ev.Channel,
			TS:       ev.TimeStamp,
			ThreadTS: firstNonEmpty(ev.ThreadTimeStamp, ev.TimeStamp),
			User:     ev.User,
		}
		// ABSOLUTE GUARD — app_mention never accepts a message the
		// relay itself could have produced. Split into two clauses
		// that are NOT equally negotiable:
		//
		// 1. Self-authorship (below, in refuseAuthor): no author, our
		//    own bot user, or an edit. Unconditional, no override,
		//    ever. The relay posts its replies as this same bot, so
		//    admitting one is a reply → trigger → reply loop with no
		//    natural bound, and a deployment without an
		//    AllowedUserIDs allowlist has nothing else in the way.
		//
		// 2. bot_id as a proxy for "not a human". Slack stamps the
		//    posting app's bot_id onto EVERY API message, including
		//    one sent with a user token on behalf of a real person —
		//    so this clause is a proxy, not a fact, and it is
		//    overridable per named user id via HumanAuthorUserIDs.
		//    Overriding it cannot reach clause 1.
		//
		// AppMentionEvent has no SubType field; Edited is its
		// equivalent — it marks an event that is not an original human
		// post, which is what SubType screens for on the message path.
		if reason := c.refuseAuthor(ev.User, ev.BotID, appID, ev.Channel+"/"+ev.TimeStamp, ev.Edited != nil); reason != "" {
			rec.Decision, rec.Reason = journal.DecisionDrop, reason
			journal.Log(rec)
			kitlog.Debugf("slack: drop %s app_mention in %s ts=%s", reason, ev.Channel, ev.TimeStamp)
			return
		}
		rec.Decision, rec.Reason = journal.DecisionDeliver, journal.ReasonMention
		journal.Log(rec)
		c.deliver(ctx, Event{
			UserID:    ev.User,
			ChannelID: ev.Channel,
			ThreadTS:  firstNonEmpty(ev.ThreadTimeStamp, ev.TimeStamp),
			TS:        ev.TimeStamp,
			Text:      stripMention(ev.Text, c.botUserID),
			IsMention: true,
		})
	case *slackevents.MessageEvent:
		rec := journal.Record{
			Stage:    journal.StageProto,
			Path:     journal.PathMessage,
			Channel:  ev.Channel,
			TS:       ev.TimeStamp,
			ThreadTS: firstNonEmpty(ev.ThreadTimeStamp, ev.TimeStamp),
			User:     ev.User,
		}
		if ev.ChannelType == "im" {
			rec.Path = journal.PathMessageIM
		}
		// Subtype events are always dropped, hatch or no hatch. The
		// relay streams by *editing* its own message, so every
		// throttled chat.update arrives here as message_changed; if
		// those reached the hatch, one self-drive reply would
		// re-trigger on each update. Only original posts are matched.
		if ev.SubType != "" {
			rec.Decision, rec.Reason = journal.DecisionDrop, journal.ReasonSubType
			journal.Log(rec)
			return
		}

		text := ev.Text
		selfDrive := false
		if c.refuseAuthor(ev.User, ev.BotID, appID, ev.Channel+"/"+ev.TimeStamp, false) != "" {
			// Not a human post. The ONLY way through is the self-drive
			// hatch: a sentinel anchored at the start of the message.
			stripped, ok := c.selfDrive.Accept(text)
			if !ok {
				rec.Decision, rec.Reason = journal.DecisionDrop, journal.ReasonSelfDriveNotAccept
				journal.Log(rec)
				// Operator-visible, not debug: a sentinel that is
				// present but not leading is the exact shape of the
				// hatch "silently doing nothing", and this line is
				// what makes it a one-second diagnosis. It also
				// confirms the echo guard working as intended when the
				// agent quotes its own trigger back.
				if c.selfDrive.containsButNotPrefix(text) {
					log.Printf("SELF-DRIVE IGNORED: channel=%s ts=%s — message contains the sentinel but does not START with it; the sentinel must be a PREFIX (note Slack HTML-escapes < > & in inbound text)", ev.Channel, ev.TimeStamp)
				}
				return
			}
			// Second layer: never act on a ts we posted ourselves.
			if c.selfDrive.SeenTS(ev.TimeStamp) {
				rec.Decision, rec.Reason = journal.DecisionDrop, journal.ReasonSelfPostedTS
				journal.Log(rec)
				kitlog.Debugf("slack: drop self-posted ts=%s in %s", ev.TimeStamp, ev.Channel)
				return
			}
			text, selfDrive = stripMention(stripped, c.botUserID), true
			rec.Path = journal.PathSelfDrive
		}

		// Forward DMs unconditionally.
		if ev.ChannelType == "im" {
			rec.Decision, rec.Reason = journal.DecisionDeliver, journal.ReasonDM
			journal.Log(rec)
			c.deliver(ctx, Event{
				UserID:    ev.User,
				ChannelID: ev.Channel,
				ThreadTS:  firstNonEmpty(ev.ThreadTimeStamp, ev.TimeStamp),
				TS:        ev.TimeStamp,
				Text:      text,
				IsDM:      true,
				SelfDrive: selfDrive,
			})
			return
		}

		// Self-drive messages are accepted top-level as well as in a
		// thread (a top-level one starts a thread), and are exempt
		// from the mentionsBot suppression below: 3a guarantees their
		// app_mention twin is already dead, so suppressing here too
		// would drop a mention+sentinel message on *both* paths.
		if selfDrive {
			rec.Decision, rec.Reason = journal.DecisionDeliver, journal.ReasonSelfDrive
			journal.Log(rec)
			c.deliver(ctx, Event{
				UserID:    ev.User,
				ChannelID: ev.Channel,
				ThreadTS:  firstNonEmpty(ev.ThreadTimeStamp, ev.TimeStamp),
				TS:        ev.TimeStamp,
				Text:      text,
				SelfDrive: true,
			})
			return
		}

		// Forward thread replies in non-DM channels. The handler will
		// decide whether to process them based on ambient config and
		// whether the thread is known.
		//
		// Skip messages that @-mention the bot: Slack delivers those
		// via a *separate* app_mention event too, so forwarding here
		// as well would double-process the same (channel,ts) — the
		// second delivery cancels the first in-flight prompt and
		// restarts it. app_mention is the canonical path for tagged
		// messages; message.channels is only for *un-tagged* replies.
		switch {
		case ev.ThreadTimeStamp == "":
			rec.Decision, rec.Reason = journal.DecisionDrop, journal.ReasonNotThreadReply
			journal.Log(rec)
		case mentionsBot(ev.Text, c.botUserID):
			rec.Decision, rec.Reason = journal.DecisionDrop, journal.ReasonMentionDuplicate
			journal.Log(rec)
		default:
			rec.Decision, rec.Reason = journal.DecisionDeliver, journal.ReasonAmbientThreadReply
			journal.Log(rec)
			c.deliver(ctx, Event{
				UserID:    ev.User,
				ChannelID: ev.Channel,
				ThreadTS:  ev.ThreadTimeStamp,
				TS:        ev.TimeStamp,
				Text:      ev.Text,
			})
		}
	}
}

// refuseAuthor decides whether an inbound event's author disqualifies
// it from being processed, returning the journal reason to record (or
// "" when the author is acceptable).
//
// The two clauses are deliberately separate and deliberately ordered:
//
//   - ReasonBotAuthored is the SELF-AUTHORSHIP guard: no author at all
//     (webhooks, classic bots), our own bot user, or an edit. This is
//     unconditional. HumanAuthorUserIDs cannot reach it — listing our
//     own bot's user id changes nothing, because this clause is
//     evaluated first and has no override. It is what makes a
//     reply → trigger → reply loop structurally impossible.
//
//   - ReasonAPIAuthored is a PROXY: Slack stamps the posting app's
//     bot_id onto every API message, including a chat.postMessage sent
//     with a user (xoxp-) token on behalf of a real person. So bot_id
//     means "sent through an app", not "sent by a robot", and treating
//     the two as identical is what made the app_mention path
//     untestable. An operator may name specific human user ids to
//     exempt from this clause and nothing else.
func (c *Client) refuseAuthor(user, botID, appID, key string, edited bool) string {
	// CLAUSE 1 — SELF-AUTHORSHIP. Unconditional. Evaluated first.
	// No config reaches this: not humanAuthors, not the allowlist
	// (which lives downstream in the handler and therefore cannot run
	// before it), not the self-drive hatch on this path. It is the
	// single property that makes a reply → trigger → reply loop
	// structurally impossible, and it must stay that way.
	if user == "" || user == c.botUserID || edited {
		return journal.ReasonBotAuthored
	}
	if botID == "" {
		// Typed in a Slack client. Ordinary human.
		return ""
	}

	// CLAUSE 2 — the bot_id PROXY, narrowly overridable.
	//
	// Slack stamps a bot_id on every API-posted message, including one
	// sent with a user token on behalf of a real person, so bot_id
	// means "sent through an app", not "sent by a robot". Three
	// conditions must ALL hold to reclassify, and each closes a
	// different hole:
	if _, named := c.humanAuthors[user]; !named {
		return journal.ReasonAPIAuthored
	}
	// ...the posting app must be OURS. Without this the relay would
	// trust ANY third-party app that posts as the named user — a
	// workflow, an integration, anything that person ever installed —
	// which is far wider than the intent. c.appID is empty when we
	// could not learn it, and an empty appID matches nothing, so this
	// fails closed.
	if c.appID == "" || appID != c.appID {
		return journal.ReasonForeignApp
	}
	// ...and it is rate-capped, as the loop backstop of last resort.
	if !c.charge(key) {
		log.Printf("HUMAN-AUTHOR REFUSED (rate cap %d/min exceeded): user=%s — dropping; check for a reply loop",
			c.humanAuthorPerMinuteOrDefault(), user)
		return journal.ReasonHumanAuthorRateCap
	}
	return ""
}

// charge consumes one token per MESSAGE, not per envelope.
//
// Slack delivers a tagged message twice — once as app_mention, once as
// message.channels — and both reach refuseAuthor before the duplicate
// is recognised further down. Charging each would make the configured
// number mean half what an operator reads it to mean, and it measurably
// throttled the harness (a cap of 2 admitted one mention, not two).
//
// A single-slot memo is enough because the pair arrives back-to-back.
// It can only ever collapse genuine duplicates: the key includes the
// Slack-assigned ts, which is unique per message per channel, so two
// distinct messages can never share one charge. If some other event is
// interleaved between the pair the memo misses and we charge twice —
// degrading to the old behaviour, never to something more permissive.
func (c *Client) charge(key string) bool {
	c.chargedMu.Lock()
	if key != "" && key == c.chargedKey {
		c.chargedMu.Unlock()
		return true // same message, second envelope: already paid for
	}
	c.chargedMu.Unlock()

	if !c.humanAuthorRate.Allow() {
		return false
	}
	c.chargedMu.Lock()
	c.chargedKey = key
	c.chargedMu.Unlock()
	return true
}

// appIDOf extracts event.app_id from a raw Socket Mode envelope. A
// missing or unparseable payload yields "", which the guard treats as
// "not our app" — failing closed.
func appIDOf(req *socketmode.Request) string {
	if req == nil {
		return ""
	}
	var env struct {
		Event struct {
			AppID string `json:"app_id"`
		} `json:"event"`
	}
	if err := json.Unmarshal(req.Payload, &env); err != nil {
		return ""
	}
	return env.Event.AppID
}

// humanAuthorPerMinuteOrDefault reports the effective cap, for logging.
func (c *Client) humanAuthorPerMinuteOrDefault() int {
	if c.humanAuthorPerMinute > 0 {
		return c.humanAuthorPerMinute
	}
	return defaultHumanAuthorPerMinute
}

// sortedKeys gives a stable rendering of a set for log output.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mentionsBot reports whether text contains an <@botID> mention. Used
// to suppress the message.channels copy of a tagged message, which
// Slack also delivers as an app_mention event.
func mentionsBot(text, botID string) bool {
	if botID == "" {
		return false
	}
	return strings.Contains(text, "<@"+botID+">")
}

func (c *Client) deliver(ctx context.Context, ev Event) {
	ev.BotUserID = c.botUserID
	if c.handler != nil {
		c.handler.Handle(ctx, ev)
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// stripMention removes a leading <@U…> mention of botID.
func stripMention(text, botID string) string {
	if botID == "" {
		return strings.TrimSpace(text)
	}
	tag := "<@" + botID + ">"
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, tag) {
		t = strings.TrimSpace(t[len(tag):])
	}
	// Also strip mid-text occurrences (best-effort).
	t = strings.ReplaceAll(t, tag, "")
	return strings.TrimSpace(t)
}

// ---- Outbound streaming ----

// PostStreamer publishes a single Slack message and lets the caller push
// incremental text. Updates are throttled via minInterval; the final Close
// flush is unconditional.
type PostStreamer struct {
	api         *slack.Client
	channel     string
	threadTS    string
	minInterval time.Duration
	maxChars    int

	// now is the clock. Swapped in tests so throttle behaviour can be
	// exercised without wall-clock sleeps. Defaults to time.Now.
	now func() time.Time

	// selfDrive, when set, scrubs the self-drive sentinel out of every
	// outbound body and remembers the ts of everything we post. Nil
	// (the default) makes both no-ops. See SetSelfDrive.
	selfDrive *SelfDrive

	mu       sync.Mutex
	ts       string // ts of the message we own (after first post)
	full     strings.Builder
	pending  bool
	lastSent time.Time
	closed   bool
	// placeholderDone flips true once the streamer has committed to
	// real content (first user-driven Append, or explicit FirstChunk).
	// UpdatePlaceholder becomes a no-op afterwards so a slow spinner
	// goroutine can never overwrite the answer.
	placeholderDone bool

	// sendMu serializes every outbound Slack write (chat.postMessage
	// and chat.update — placeholder updates AND flushed content). The
	// concurrency model is: producers (Append, UpdatePlaceholder,
	// flush, FlushIfPending) drop s.mu before doing the actual API
	// call, and a slow update can otherwise race a fast one.
	//
	// Without this serialization the spinner goroutine's in-flight
	// chat.update can land AFTER the sink's FirstChunk+Append update,
	// clobbering real content with "Thinking..". UpdatePlaceholder
	// re-checks placeholderDone after acquiring sendMu so a
	// late-loser doesn't issue its update at all.
	sendMu sync.Mutex
}

// NewPostStreamer creates a streamer that will post in `channel` as a thread
// reply under `threadTS`. minInterval defaults to 1s. maxChars caps the
// rendered message body (Slack hard limit ~40k; default 35000).
func NewPostStreamer(api *slack.Client, channel, threadTS string) *PostStreamer {
	return &PostStreamer{
		api:         api,
		channel:     channel,
		threadTS:    threadTS,
		minInterval: time.Second,
		maxChars:    35000,
		now:         time.Now,
	}
}

// SetSelfDrive wires the self-drive hatch into the outbound path, so
// posts are scrubbed of the sentinel and their ts values remembered.
// Safe to omit; nil keeps both guards inert.
func (s *PostStreamer) SetSelfDrive(d *SelfDrive) {
	s.mu.Lock()
	s.selfDrive = d
	s.mu.Unlock()
}

// out prepares a body for transmission: the sentinel is neutralised so
// the relay can never post its own trigger. This is loop guard #1 — it
// makes an echo loop structurally impossible rather than merely
// unlikely, independent of how Accept matches.
func (s *PostStreamer) out(body string) string {
	s.mu.Lock()
	d := s.selfDrive
	s.mu.Unlock()
	return d.Scrub(body)
}

// recordTS remembers a ts we just wrote, so the inbound path can skip
// Slack's echo of our own message (loop guard #2).
func (s *PostStreamer) recordTS(ts string) {
	s.mu.Lock()
	d := s.selfDrive
	s.mu.Unlock()
	d.RecordTS(ts)
}

// Start posts an initial placeholder message *immediately* — used as
// the "Thinking…" indicator that replaces Slack's missing typing
// dots. The placeholder body is not added to the streamed buffer, so
// the first Append flush will *update* the message to the real
// content (with the placeholder cleanly overwritten).
//
// Idempotent: a second Start, or any flush that ran before Start
// because Append landed first, is a no-op.
func (s *PostStreamer) Start(ctx context.Context, body string) error {
	s.mu.Lock()
	if s.closed || s.ts != "" {
		s.mu.Unlock()
		return nil
	}
	if body == "" {
		body = "_thinking…_"
	}
	channel := s.channel
	threadTS := s.threadTS
	s.mu.Unlock()

	// Serialize against placeholder updates and flushes — see sendMu
	// docs. Holding it across the post ensures any concurrent
	// UpdatePlaceholder/flush waits for s.ts to be set before
	// running.
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	_, ts, err := s.api.PostMessageContext(ctx, channel,
		slack.MsgOptionText(s.out(body), false),
		slack.MsgOptionTS(threadTS),
		slack.MsgOptionDisableLinkUnfurl(),
	)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	s.recordTS(ts)
	s.mu.Lock()
	if s.ts == "" {
		s.ts = ts
		// Intentionally leave lastSent at zero so the first real
		// Append flushes immediately as a chat.update rather than
		// waiting for the watchdog tick. The placeholder post + one
		// content update inside the same second is well within
		// Slack's chat.update rate limit (~1/s); subsequent updates
		// still throttle.
	}
	s.mu.Unlock()
	return nil
}

// SetMinInterval overrides the throttle minimum (default 1s). Exposed
// for tests in other packages that need deterministic placeholder
// updates without wall-clock waits; production callers should not
// reach for this.
func (s *PostStreamer) SetMinInterval(d time.Duration) {
	s.mu.Lock()
	s.minInterval = d
	s.mu.Unlock()
}

// UpdatePlaceholder rewrites the message body in place — used by a
// spinner loop to animate the "Thinking…" frame between Start and the
// first real chunk. Returns alive=true if the update went out (the
// caller's next tick is worth running); alive=false means the
// placeholder window has closed (real content has begun, or the
// stream is closed) and the spinner should self-disarm.
//
// Throttle-aware: skips ticks that would land within minInterval of
// the previous send, returning alive=true without an IO call so the
// caller keeps ticking. Does NOT touch s.full — placeholder frames
// are explicitly outside the streamed buffer.
func (s *PostStreamer) UpdatePlaceholder(ctx context.Context, body string) (alive bool, err error) {
	s.mu.Lock()
	if s.closed || s.ts == "" {
		s.mu.Unlock()
		return false, nil
	}
	if s.now().Sub(s.lastSent) < s.minInterval {
		// Too soon — skip this tick, stay alive.
		s.mu.Unlock()
		return true, nil
	}
	channel := s.channel
	ts := s.ts
	s.mu.Unlock()

	// Serialize outbound Slack writes. The placeholderDone check is
	// done HERE (post-sendMu) rather than earlier so a concurrent
	// FirstChunk+Append that completes while we wait for sendMu still
	// disarms us — otherwise our chat.update would land after their
	// flush and clobber the real content with a stale spinner frame.
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.mu.Lock()
	done := s.placeholderDone || s.closed
	s.mu.Unlock()
	if done {
		return false, nil
	}

	_, _, _, uerr := s.api.UpdateMessageContext(ctx, channel, ts,
		slack.MsgOptionText(s.out(body), false),
		slack.MsgOptionDisableLinkUnfurl(),
	)
	if uerr != nil {
		return true, fmt.Errorf("update: %w", uerr)
	}
	s.recordTS(ts)
	s.mu.Lock()
	s.lastSent = s.now()
	s.mu.Unlock()
	return true, nil
}

// FirstChunk signals that real content is about to flow. Closes the
// placeholder window (subsequent UpdatePlaceholder calls return
// alive=false) and resets the throttle so the imminent Append flushes
// immediately rather than waiting up to minInterval behind a spinner
// tick. Idempotent.
func (s *PostStreamer) FirstChunk() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.placeholderDone {
		return
	}
	s.placeholderDone = true
	s.lastSent = time.Time{}
}

// HasContent reports whether the turn has produced any user-visible
// body text — i.e. whether anything has been Appended into the
// streamed buffer.
//
// It deliberately asks about the BUFFER, not about what has reached
// Slack. On this surface those differ: Append writes into s.full and
// only flushes when the ~1s throttle allows, and the abstain path
// buffers the whole answer inside the sink and flushes it in one go at
// Finalize. So a complete answer can be sitting in s.full, entirely
// unsent, at the moment the turn ends — and an answer that is about to
// be written is an answer. Close flushes unconditionally, so anything
// in the buffer WILL be posted.
//
// The status footer gates on this: a turn that produced nothing has
// nothing to sign, and the streamer's "_thinking…_" fallback body is
// not content.
func (s *PostStreamer) HasContent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.full.Len() > 0
}

// Append adds text to the buffer and flushes if enough time has elapsed.
func (s *PostStreamer) Append(ctx context.Context, chunk string) error {
	if chunk == "" {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.full.WriteString(chunk)
	now := s.now()
	due := s.ts == "" || now.Sub(s.lastSent) >= s.minInterval
	s.pending = !due
	s.mu.Unlock()
	if due {
		return s.flush(ctx)
	}
	return nil
}

// Close flushes any pending text and optionally appends suffix (e.g.
// the "_(stopped: …)_" marker or the italic status footer).
//
// The suffix goes into s.full — the streamed buffer — and NOT out as a
// separate chat.update. That matters on this surface: the relay
// streams by EDITING one message, so every flush re-posts the whole of
// s.full and any text that is not in the buffer is erased by the next
// edit. A suffix written side-band would survive only until something
// else flushed. Being in the buffer also means the suffix is
// permanently LAST, which is exactly what an end-of-answer footer
// needs.
//
// Idempotent: a second Close is a no-op, so a suffix can never be
// appended twice.
func (s *PostStreamer) Close(ctx context.Context, suffix string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if suffix != "" {
		s.full.WriteString(suffix)
	}
	s.mu.Unlock()
	return s.flush(ctx)
}

func (s *PostStreamer) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	body := s.full.String()
	if len(body) > s.maxChars {
		// Trim from the front and add an ellipsis marker.
		body = "…(truncated)…\n" + body[len(body)-s.maxChars:]
	}
	if body == "" {
		return "_thinking…_"
	}
	return body
}

func (s *PostStreamer) flush(ctx context.Context) error {
	// Serialize against placeholder updates and other flushes — see
	// the sendMu comment on PostStreamer. Without this lock, a slow
	// chat.update from the spinner can land after this flush and
	// clobber real content with a stale "Thinking.." frame.
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	body := s.body()
	s.mu.Lock()
	firstPost := s.ts == ""
	channel := s.channel
	threadTS := s.threadTS
	ts := s.ts
	s.mu.Unlock()
	if firstPost {
		_, newTS, err := s.api.PostMessageContext(ctx, channel,
			slack.MsgOptionText(s.out(body), false),
			slack.MsgOptionTS(threadTS),
			slack.MsgOptionDisableLinkUnfurl(),
		)
		if err != nil {
			return fmt.Errorf("post: %w", err)
		}
		s.recordTS(newTS)
		s.mu.Lock()
		s.ts = newTS
		s.lastSent = s.now()
		s.pending = false
		s.mu.Unlock()
		return nil
	}
	_, _, _, err := s.api.UpdateMessageContext(ctx, channel, ts,
		slack.MsgOptionText(s.out(body), false),
		slack.MsgOptionDisableLinkUnfurl(),
	)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	s.recordTS(ts)
	s.mu.Lock()
	s.lastSent = s.now()
	s.pending = false
	s.mu.Unlock()
	return nil
}

// FlushIfPending emits any buffered text. Useful as a watchdog tick.
func (s *PostStreamer) FlushIfPending(ctx context.Context) error {
	s.mu.Lock()
	pending := s.pending && !s.closed
	due := s.now().Sub(s.lastSent) >= s.minInterval
	s.mu.Unlock()
	if pending && due {
		return s.flush(ctx)
	}
	return nil
}
