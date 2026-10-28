// Package catchup reads the Slack messages that arrived while the relay
// was not connected.
//
// Socket Mode delivers events only while a connection is open. A crash,
// a restart or a dropped websocket opens a gap: anything posted in it is
// never delivered as an event. The catch-up closes that gap. The relay
// keeps a checkpoint per conversation (see Store) and, every time Socket
// Mode connects — at startup and on every reconnect — reads forward from
// it with conversations.history and conversations.replies.
//
// This package owns only the checkpoint and the reader. What a message
// MEANS — whether it is addressed to the bot, which session it belongs
// to — is decided by the same gates the live path uses, behind the
// Deliver callback.
//
// # Slack rate limits
//
// conversations.history and conversations.replies are Tier 3 (about 50
// calls a minute, up to 1000 messages a page) for internal and
// Marketplace apps. A commercially distributed app outside the
// Marketplace is clamped to 1 call a minute and 15 messages a page
// (since 2025-05-29 for new installs, 2026-03-03 for all). The reader
// asks for full pages and follows cursors, so it works under either
// regime; a 429 is waited out for its Retry-After and retried, and each
// run logs its call count, 429 count and total wait so the regime an
// installation is actually on can be read off the log.
package catchup

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/slack-go/slack"
)

// API is the Slack read surface the catch-up needs. *slack.Client
// satisfies it.
type API interface {
	GetConversationsForUserContext(ctx context.Context, p *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error)
	GetConversationHistoryContext(ctx context.Context, p *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error)
	GetConversationRepliesContext(ctx context.Context, p *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error)
}

// Conversation is a channel or DM the bot is a member of.
type Conversation struct {
	ID   string
	IsIM bool
}

// Defaults for Config's zero values.
const (
	// DefaultPageSize is the page size asked for. Slack clamps it to
	// 15 for a non-Marketplace distributed app.
	DefaultPageSize = 200
	// DefaultMaxMessages bounds one conversation's catch-up. A relay
	// that was down for days in a busy channel must not spend its
	// startup reading the whole history.
	DefaultMaxMessages = 1000
	// DefaultRetries is how many 429s one call waits out before the
	// conversation is given up on for this run.
	DefaultRetries = 5
	// DefaultThreadMaxAge bounds which known threads are re-read.
	DefaultThreadMaxAge = 30 * 24 * time.Hour
)

// minRetryAfter is the wait used when a 429 carries no Retry-After.
const minRetryAfter = time.Second

// Config configures a Scanner.
type Config struct {
	API   API
	Store *Store
	// Threads lists the thread ts values of threads the relay already
	// has a session for in channel. conversations.history only returns
	// parents posted after the checkpoint, so a reply in an older
	// thread is found only by re-reading that thread. Nil = none.
	Threads func(channel string) []string
	// ThreadMaxAge: known threads whose parent is older than this
	// (relative to the checkpoint) are not re-read. 0 = default.
	ThreadMaxAge time.Duration
	// PageSize, MaxMessages, Retries: 0 uses the defaults.
	PageSize    int
	MaxMessages int
	Retries     int
	// Sleep waits out a 429; nil uses the real clock.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now is the clock for Stats.Elapsed; nil uses time.Now.
	Now  func() time.Time
	Logf func(format string, args ...any)
}

// Scanner reads missed messages.
type Scanner struct {
	cfg Config
}

// New builds a Scanner, filling in defaults.
func New(cfg Config) *Scanner {
	if cfg.PageSize <= 0 {
		cfg.PageSize = DefaultPageSize
	}
	if cfg.MaxMessages <= 0 {
		cfg.MaxMessages = DefaultMaxMessages
	}
	if cfg.Retries <= 0 {
		cfg.Retries = DefaultRetries
	}
	if cfg.ThreadMaxAge <= 0 {
		cfg.ThreadMaxAge = DefaultThreadMaxAge
	}
	if cfg.Threads == nil {
		cfg.Threads = func(string) []string { return nil }
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Scanner{cfg: cfg}
}

// Store returns the checkpoint store.
func (s *Scanner) Store() *Store { return s.cfg.Store }

// Stats is what one run cost — the measurement the package doc refers
// to.
type Stats struct {
	Conversations int           // conversations scanned
	Failed        int           // conversations given up on this run
	Messages      int           // messages newer than their checkpoint
	Calls         int           // Slack Web API calls made
	RateLimited   int           // of which answered 429
	Waited        time.Duration // total Retry-After waited
	Elapsed       time.Duration
}

func (st Stats) String() string {
	return fmt.Sprintf("%d conversation(s), %d failed, %d new message(s); %d API call(s), %d rate-limited, waited %s; took %s",
		st.Conversations, st.Failed, st.Messages, st.Calls, st.RateLimited, st.Waited, st.Elapsed)
}

// Deliver receives one conversation's missed messages, oldest first.
// The conversation's checkpoint moves past them only after it returns.
type Deliver func(ctx context.Context, conv Conversation, msgs []slack.Message)

// Run performs one catch-up over every conversation the bot is a member
// of. A conversation that fails is skipped and keeps its checkpoint, so
// the next run reads it again; the error is returned only when the
// conversation list itself cannot be read.
func (s *Scanner) Run(ctx context.Context, deliver Deliver) (Stats, error) {
	start := s.cfg.Now()
	// New channels start at the run's start, not when the loop reaches
	// them: a live message posted there meanwhile is held, and is
	// released only if it is newer than this.
	stamp := TS(start)
	var st Stats
	convs, err := s.conversations(ctx, &st)
	if err != nil {
		st.Elapsed = s.cfg.Now().Sub(start)
		return st, fmt.Errorf("catchup: list conversations: %w", err)
	}
	for _, conv := range convs {
		if ctx.Err() != nil {
			break
		}
		st.Conversations++
		if !conv.IsIM && !s.cfg.Store.Has(conv.ID) {
			// A channel seen for the first time — the bot was invited
			// since the last run, or this is the first run. Its history
			// predates the bot's membership and must not be replayed;
			// it starts here. A DM keeps the floor: a person's first
			// message to the bot may well have arrived in the gap.
			if err := s.cfg.Store.Advance(conv.ID, stamp); err != nil {
				s.cfg.Logf("catchup: WARN checkpoint %s: %v", conv.ID, err)
			}
			continue
		}
		msgs, newest, err := s.scan(ctx, conv.ID, &st)
		if err != nil {
			st.Failed++
			s.cfg.Logf("catchup: WARN %s: %v; its checkpoint is kept and it is read again on the next connect", conv.ID, err)
			continue
		}
		if len(msgs) > 0 {
			st.Messages += len(msgs)
			deliver(ctx, conv, msgs)
		}
		if err := s.cfg.Store.Advance(conv.ID, newest); err != nil {
			s.cfg.Logf("catchup: WARN checkpoint %s: %v", conv.ID, err)
		}
	}
	st.Elapsed = s.cfg.Now().Sub(start)
	return st, nil
}

// conversations lists the channels and DMs the bot is a member of.
// mpim is left out: the app subscribes to no mpim events, so the live
// path never answers there either.
func (s *Scanner) conversations(ctx context.Context, st *Stats) ([]Conversation, error) {
	var out []Conversation
	cursor := ""
	for {
		var chans []slack.Channel
		var next string
		err := s.call(ctx, st, func() (err error) {
			chans, next, err = s.cfg.API.GetConversationsForUserContext(ctx, &slack.GetConversationsForUserParameters{
				Cursor:          cursor,
				Types:           []string{"public_channel", "private_channel", "im"},
				Limit:           s.cfg.PageSize,
				ExcludeArchived: true,
			})
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, c := range chans {
			out = append(out, Conversation{ID: c.ID, IsIM: c.IsIM})
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// scan reads one conversation forward from its checkpoint. It returns
// the messages newer than the checkpoint, oldest first, and the ts the
// checkpoint should move to.
func (s *Scanner) scan(ctx context.Context, channel string, st *Stats) ([]slack.Message, string, error) {
	mark := s.cfg.Store.Mark(channel)
	seen := map[string]slack.Message{}
	threads := map[string]bool{}
	add := func(m slack.Message) {
		if m.Timestamp > mark {
			seen[m.Timestamp] = m
		}
	}

	cursor := ""
	for len(seen) < s.cfg.MaxMessages {
		var resp *slack.GetConversationHistoryResponse
		err := s.call(ctx, st, func() (err error) {
			resp, err = s.cfg.API.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
				ChannelID: channel,
				Oldest:    mark,
				Cursor:    cursor,
				Limit:     s.cfg.PageSize,
			})
			return err
		})
		if err != nil {
			return nil, "", fmt.Errorf("conversations.history: %w", err)
		}
		for _, m := range resp.Messages {
			add(m)
			if m.ReplyCount > 0 && m.LatestReply > mark {
				threads[m.Timestamp] = true
			}
		}
		if !resp.HasMore || resp.ResponseMetaData.NextCursor == "" {
			break
		}
		cursor = resp.ResponseMetaData.NextCursor
	}
	if len(seen) >= s.cfg.MaxMessages {
		s.cfg.Logf("catchup: WARN %s: stopped at %d messages; older missed messages there are not replayed", channel, len(seen))
	}

	cutoff := s.threadCutoff(mark)
	for _, ts := range s.cfg.Threads(channel) {
		if ts >= cutoff {
			threads[ts] = true
		}
	}
	for _, ts := range sortedKeys(threads) {
		if err := s.replies(ctx, channel, ts, mark, add, st); err != nil {
			return nil, "", err
		}
	}

	msgs := make([]slack.Message, 0, len(seen))
	newest := mark
	for _, ts := range sortedKeys(seen) {
		msgs = append(msgs, seen[ts])
		newest = ts
	}
	return msgs, newest, nil
}

// replies reads one thread's replies newer than mark.
func (s *Scanner) replies(ctx context.Context, channel, thread, mark string, add func(slack.Message), st *Stats) error {
	cursor := ""
	for {
		var msgs []slack.Message
		var more bool
		var next string
		err := s.call(ctx, st, func() (err error) {
			msgs, more, next, err = s.cfg.API.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
				ChannelID: channel,
				Timestamp: thread,
				Oldest:    mark,
				Cursor:    cursor,
				Limit:     s.cfg.PageSize,
			})
			return err
		})
		if err != nil {
			// A thread that is gone has nothing to catch up on; it must
			// not hold the whole conversation's checkpoint back.
			if err.Error() == "thread_not_found" {
				return nil
			}
			return fmt.Errorf("conversations.replies %s: %w", thread, err)
		}
		for _, m := range msgs {
			add(m)
		}
		if !more || next == "" {
			return nil
		}
		cursor = next
	}
}

// threadCutoff is the oldest thread ts re-read for a checkpoint.
func (s *Scanner) threadCutoff(mark string) string {
	var sec, usec int64
	if _, err := fmt.Sscanf(mark, "%d.%d", &sec, &usec); err != nil {
		return ""
	}
	return TS(time.Unix(sec, 0).Add(-s.cfg.ThreadMaxAge))
}

// call runs one Web API call, waiting out 429s.
func (s *Scanner) call(ctx context.Context, st *Stats, fn func() error) error {
	for attempt := 0; ; attempt++ {
		st.Calls++
		err := fn()
		var rl *slack.RateLimitedError
		if !errors.As(err, &rl) {
			return err
		}
		st.RateLimited++
		if attempt >= s.cfg.Retries {
			return err
		}
		d := max(rl.RetryAfter, minRetryAfter)
		s.cfg.Logf("catchup: rate limited; waiting %s (Retry-After)", d)
		if serr := s.cfg.Sleep(ctx, d); serr != nil {
			return serr
		}
		st.Waited += d
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
