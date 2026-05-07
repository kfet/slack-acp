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
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/kfet/slack-acp/internal/debuglog"
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
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	sm := socketmode.New(api)
	return &Client{api: api, sm: sm, handler: h}, nil
}

// Run authenticates, captures the bot's user id, and processes events
// until ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	auth, err := c.api.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("slack auth.test: %w", err)
	}
	c.botUserID = auth.UserID
	debuglog.Logf("slack: connected as %s (%s)", auth.User, auth.UserID)

	go c.consume(ctx)
	return c.sm.RunContext(ctx)
}

// API returns the underlying *slack.Client (used by PostStreamer).
func (c *Client) API() *slack.Client { return c.api }

// BotUserID returns the cached bot user id (after Run).
func (c *Client) BotUserID() string { return c.botUserID }

func (c *Client) consume(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-c.sm.Events:
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
		debuglog.Logf("slack: %s", evt.Type)
	case socketmode.EventTypeEventsAPI:
		api, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		c.sm.Ack(*evt.Request)
		c.handleEventsAPI(ctx, api)
	case socketmode.EventTypeDisconnect:
		debuglog.Logf("slack: disconnected")
	default:
		debuglog.Logf("slack: ignoring %s", evt.Type)
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
	doneSuffix  string

	mu       sync.Mutex
	ts       string // ts of the message we own (after first post)
	full     strings.Builder
	pending  bool
	lastSent time.Time
	closed   bool
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
	}
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
	now := time.Now()
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
	body := s.body()
	if s.ts == "" {
		_, ts, err := s.api.PostMessageContext(ctx, s.channel,
			slack.MsgOptionText(body, false),
			slack.MsgOptionTS(s.threadTS),
			slack.MsgOptionDisableLinkUnfurl(),
		)
		if err != nil {
			return fmt.Errorf("post: %w", err)
		}
		s.mu.Lock()
		s.ts = ts
		s.lastSent = time.Now()
		s.pending = false
		s.mu.Unlock()
		return nil
	}
	_, _, _, err := s.api.UpdateMessageContext(ctx, s.channel, s.ts,
		slack.MsgOptionText(body, false),
		slack.MsgOptionDisableLinkUnfurl(),
	)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	s.mu.Lock()
	s.lastSent = time.Now()
	s.pending = false
	s.mu.Unlock()
	return nil
}

// FlushIfPending emits any buffered text. Useful as a watchdog tick.
func (s *PostStreamer) FlushIfPending(ctx context.Context) error {
	s.mu.Lock()
	pending := s.pending && !s.closed
	due := time.Since(s.lastSent) >= s.minInterval
	s.mu.Unlock()
	if pending && due {
		return s.flush(ctx)
	}
	return nil
}
