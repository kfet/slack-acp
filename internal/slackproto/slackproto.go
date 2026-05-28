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
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	kitlog "github.com/kfet/acp-kit/log"
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
}

// New constructs a Client. botToken is xoxb-, appToken is xapp-.
func New(botToken, appToken string, h Handler) (*Client, error) {
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
	return &Client{api: api, sm: sm, handler: h}, nil
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
		c.sm.Ack(*evt.Request)
		c.handleEventsAPI(ctx, api)
	case socketmode.EventTypeDisconnect:
		kitlog.Debugf("slack: disconnected")
	default:
		kitlog.Debugf("slack: ignoring %s", evt.Type)
	}
}

func (c *Client) handleEventsAPI(ctx context.Context, api slackevents.EventsAPIEvent) {
	if api.Type != slackevents.CallbackEvent {
		return
	}
	switch ev := api.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		c.deliver(ctx, Event{
			UserID:    ev.User,
			ChannelID: ev.Channel,
			ThreadTS:  firstNonEmpty(ev.ThreadTimeStamp, ev.TimeStamp),
			TS:        ev.TimeStamp,
			Text:      stripMention(ev.Text, c.botUserID),
		})
	case *slackevents.MessageEvent:
		// DMs (channel.im) — Slack delivers without app_mention.
		// Ignore bot/sub events and edits; ignore our own messages.
		if ev.BotID != "" || ev.SubType != "" || ev.User == c.botUserID || ev.User == "" {
			return
		}
		if ev.ChannelType != "im" {
			return
		}
		c.deliver(ctx, Event{
			UserID:    ev.User,
			ChannelID: ev.Channel,
			ThreadTS:  firstNonEmpty(ev.ThreadTimeStamp, ev.TimeStamp),
			TS:        ev.TimeStamp,
			Text:      ev.Text,
			IsDM:      true,
		})
	}
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
		slack.MsgOptionText(body, false),
		slack.MsgOptionTS(threadTS),
		slack.MsgOptionDisableLinkUnfurl(),
	)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
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
		slack.MsgOptionText(body, false),
		slack.MsgOptionDisableLinkUnfurl(),
	)
	if uerr != nil {
		return true, fmt.Errorf("update: %w", uerr)
	}
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

// Close flushes any pending text and optionally appends suffix (e.g. "_done_").
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
			slack.MsgOptionText(body, false),
			slack.MsgOptionTS(threadTS),
			slack.MsgOptionDisableLinkUnfurl(),
		)
		if err != nil {
			return fmt.Errorf("post: %w", err)
		}
		s.mu.Lock()
		s.ts = newTS
		s.lastSent = s.now()
		s.pending = false
		s.mu.Unlock()
		return nil
	}
	_, _, _, err := s.api.UpdateMessageContext(ctx, channel, ts,
		slack.MsgOptionText(body, false),
		slack.MsgOptionDisableLinkUnfurl(),
	)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
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
