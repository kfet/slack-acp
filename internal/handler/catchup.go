package handler

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/convo"
	kitlog "github.com/kfet/acp-kit/log"

	"github.com/kfet/slack-acp/internal/journal"
	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackproto"
)

// MaxCatchupPerThread bounds how many missed messages one thread's
// catch-up turn carries. The newest are kept: they are the ones a
// reader still waits on, and the header counts the rest.
const MaxCatchupPerThread = 50

// catchupThread is the missed traffic of one thread.
type catchupThread struct {
	key   router.ConvKey
	lines []string
	first string // ts of the first message
	last  slackproto.Event
	// files are the attachments of every collapsed message; the turn
	// carries them all.
	files []slackproto.File
	// The summoning flags across the batch, so the turn is never
	// abstain-eligible when any message addressed the bot.
	addressed, dm, selfDrive bool
}

// DefaultCatchupTurns is how many catch-up turns run at once. A long
// outage can leave many threads waiting, and starting all of their
// turns together would be a burst of agent prompts and Slack posts.
const DefaultCatchupTurns = 4

// CatchUp turns one conversation's missed messages into turns. evs is
// oldest first, as the catch-up reads them.
//
// Every event passes the same gates as a live one (admit). Then each
// thread's missed messages are COLLAPSED into one turn: a thread that
// had five messages while the relay was down gets one answer, not five
// turns that supersede each other. Relay commands are skipped — they
// were typed for a relay that was not there, and running `!new` or
// `!model` minutes later would surprise everybody. Messages older than
// CatchupMaxAge are not answered; each thread that had any gets a
// one-line notice instead.
//
// The turns start off the caller's goroutine, at most CatchupTurns at
// a time, so live delivery is never held up behind them.
func (h *Handler) CatchUp(ctx context.Context, evs []slackproto.Event) {
	threads := map[router.ConvKey]*catchupThread{}
	summoned := map[router.ConvKey]bool{}
	tooOld := map[router.ConvKey]int{}
	var order, oldOrder []router.ConvKey
	oldest := time.Time{}
	if h.cfg.CatchupMaxAge > 0 {
		oldest = h.now().Add(-h.cfg.CatchupMaxAge)
	}
	notice := map[router.ConvKey]slackproto.Event{}
	for _, ev := range evs {
		rec := handlerRecord(ev)
		key, text, ok := h.admit(ev, &rec, summoned)
		if !ok {
			continue
		}
		if strings.HasPrefix(text, command.DisplaySigil) || strings.HasPrefix(text, "/") {
			rec.Decision, rec.Reason = journal.DecisionDrop, journal.ReasonCatchUpCommand
			journal.Log(rec)
			continue
		}
		if addressed(ev) {
			summoned[key.Thread()] = true
		}
		if tsTime(ev.TS).Before(oldest) {
			rec.Decision, rec.Reason = journal.DecisionDrop, journal.ReasonCatchUpTooOld
			journal.Log(rec)
			if tooOld[key] == 0 {
				oldOrder = append(oldOrder, key)
				notice[key] = ev
			}
			tooOld[key]++
			continue
		}
		t := threads[key]
		if t == nil {
			t = &catchupThread{key: key, first: ev.TS}
			threads[key] = t
			order = append(order, key)
		}
		t.lines = append(t.lines, h.catchupLine(ctx, ev, text))
		t.files = append(t.files, ev.Files...)
		t.last = ev
		t.addressed = t.addressed || addressed(ev)
		t.dm = t.dm || ev.IsDM
		t.selfDrive = t.selfDrive || ev.SelfDrive
		rec.Decision, rec.Reason = journal.DecisionRun, journal.ReasonPrompt
		journal.Log(rec)
	}
	for _, key := range oldOrder {
		n := tooOld[key]
		text := fmt.Sprintf("[catch-up] %d %s older than %s arrived while the relay was offline and %s not answered.",
			n, plural(n, "message", "messages"), fmtAge(h.cfg.CatchupMaxAge), plural(n, "was", "were"))
		if err := h.reply(ctx, &convo.In{Conv: key.String(), Text: text, Meta: notice[key]}, text); err != nil {
			kitlog.Debugf("handler: catch-up notice in %s: %v", key, err)
		}
	}
	if len(order) == 0 {
		return
	}
	turns := make([]*catchupThread, len(order))
	for i, key := range order {
		turns[i] = threads[key]
	}
	h.bg.Add(1)
	go h.startCatchups(ctx, turns)
}

// startCatchups starts the catch-up turns, at most CatchupTurns at a
// time across every batch.
func (h *Handler) startCatchups(ctx context.Context, turns []*catchupThread) {
	defer h.bg.Done()
	for i, t := range turns {
		select {
		case h.catchupSlots <- struct{}{}:
		case <-ctx.Done():
			log.Printf("handler: WARN shutting down; %d catch-up turn(s) not started", len(turns)-i)
			return
		}
		release := func() { <-h.catchupSlots }
		if live, ok := h.liveTS.Load(t.key); ok && live.(string) > t.last.TS {
			// A live message started a turn here while this one waited.
			// Starting now would supersede, and so cancel, that newer
			// turn.
			release()
			kitlog.Debugf("handler: catch-up turn in %s dropped: a newer message already started a turn", t.key)
			continue
		}
		meta := t.last
		// Any addressed line makes the whole turn addressed (run never
		// abstains on it).
		meta.IsMention, meta.IsDM, meta.SelfDrive = t.addressed, t.dm, t.selfDrive
		meta.Files = t.files
		prompt := catchupPrompt(t)
		meta.Text = prompt
		key := t.key
		h.convo.Start(ctx, convo.Job{
			Conv:  key.String(),
			Run:   func(ctx context.Context, _ *convo.Turn) error { return h.run(ctx, meta, key, prompt) },
			After: func(*convo.Turn) { release() },
		})
	}
}

// fmtAge renders a duration without the zero tails: "24h", not
// "24h0m0s".
func fmtAge(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func (h *Handler) now() time.Time {
	if h.cfg.Now != nil {
		return h.cfg.Now()
	}
	return time.Now()
}

// catchupLine is one missed message as it appears in the turn. In
// ambient mode a live prompt is prefixed with its sender's name (see
// run); a collapsed turn has several senders, so each line carries its
// own and run adds none.
func (h *Handler) catchupLine(ctx context.Context, ev slackproto.Event, text string) string {
	if text == "" {
		// A file shared with no text; the files themselves ride on the
		// turn.
		text = fmt.Sprintf("(shared %d %s)", len(ev.Files), plural(len(ev.Files), "file", "files"))
	}
	// A per-user turn has a single sender (named in its origin _meta).
	if h.cfg.Ambient && !h.cfg.PerUserSessions && !ev.IsDM && ev.UserID != "" {
		return fmt.Sprintf("[%s] %s", h.getUserName(ctx, ev.UserID), text)
	}
	return text
}

// catchupPrompt builds the one prompt of a thread's catch-up turn.
func catchupPrompt(t *catchupThread) string {
	n := len(t.lines)
	var b strings.Builder
	fmt.Fprintf(&b, "[catch-up] %d %s arrived while the relay was offline (first at %s)",
		n, plural(n, "message", "messages"), tsTime(t.first).UTC().Format(time.RFC3339))
	kept := t.lines
	if n > MaxCatchupPerThread {
		kept = kept[n-MaxCatchupPerThread:]
		fmt.Fprintf(&b, "\n[catch-up] Only the newest %d are below; the oldest %d are left out.", MaxCatchupPerThread, n-MaxCatchupPerThread)
	}
	for _, l := range kept {
		b.WriteString("\n\n")
		b.WriteString(l)
	}
	return b.String()
}

// tsTime is the wall time of a Slack ts ("<seconds>.<micros>"). A
// malformed ts reads as the epoch; it only feeds a header.
func tsTime(ts string) time.Time {
	sec, _, _ := strings.Cut(ts, ".")
	n, _ := strconv.ParseInt(sec, 10, 64)
	return time.Unix(n, 0)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
