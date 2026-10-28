package handler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/slack-go/slack"

	"github.com/kfet/slack-acp/internal/journal"
	"github.com/kfet/slack-acp/internal/slackproto"
)

// promptLog records every prompt text the fake agent receives.
type promptLog struct {
	mu    sync.Mutex
	texts []string
}

func (p *promptLog) hook(_ context.Context, _ acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.texts = append(p.texts, blocks[0].Text.Text)
	return acp.StopReasonEndTurn, nil
}

func (p *promptLog) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.texts...)
}

func cev(user, thread, ts, text string) slackproto.Event {
	return slackproto.Event{UserID: user, BotUserID: "BBOT", ChannelID: "C1", ThreadTS: thread, TS: ts, Text: text, CatchUp: true}
}

// TestCatchUpCollapsesAThreadIntoOneTurn: the missed messages of one
// thread become ONE turn carrying each line with its sender; commands
// are not run late; an ambient reply in a thread the bot was never
// summoned into stays dropped.
func TestCatchUpCollapsesAThreadIntoOneTurn(t *testing.T) {
	records := captureJournal(t)
	fa := newFakeAgent()
	pl := &promptLog{}
	fa.promptHook = pl.hook
	rs := newRichSlack(t)
	// Backfill on, and replies on offer: the catch-up turn must NOT also
	// backfill the lines it already carries.
	rs.replies = []slack.Message{mkReply("U9", "1700000010.500000", "would be backfilled")}
	h := New(Config{Router: newTestRouter(t, fa), API: rs.client(), Ambient: true, Backfill: true, BackfillMaxMessages: 50})

	// The thread was summoned live before the outage.
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", BotUserID: "BBOT", ChannelID: "C1",
		ThreadTS: "1700000010.000000", TS: "1700000010.000000", Text: "hi", IsMention: true})
	waitForIdle(t, h)

	h.CatchUp(context.Background(), []slackproto.Event{
		cev("U1", "1700000010.000000", "1700000011.000000", "still there?"),
		cev("U2", "1700000010.000000", "1700000011.500000", "more context"),
		cev("U2", "1700000010.000000", "1700000012.000000", "!model opus"),
		cev("U2", "1700000010.000000", "1700000013.000000", "/giphy cat"),
		cev("U3", "1700000020.000000", "1700000021.000000", "unknown thread chatter"),
	})
	waitForIdle(t, h)

	got := pl.all()
	if len(got) != 2 {
		t.Fatalf("prompts = %q", got)
	}
	want := "[catch-up] 2 messages arrived while the relay was offline (first at 2023-11-14T22:13:31Z)\n\n[Name-U1] still there?\n\n[Name-U2] more context"
	if got[1] != want {
		t.Fatalf("prompt = %q\nwant %q", got[1], want)
	}
	if strings.Contains(got[1], "[Name-U2] [catch-up]") {
		t.Fatal("run must not prefix a catch-up turn with one sender")
	}

	reasons := map[string]string{}
	for _, r := range records() {
		if r.Stage == journal.StageHandler && r.TS != "1700000010.000000" {
			if !r.CatchUp {
				t.Fatalf("handler record without catch_up: %+v", r)
			}
			reasons[r.TS] = r.Reason
		}
	}
	for ts, want := range map[string]string{
		"1700000011.000000": journal.ReasonPrompt,
		"1700000011.500000": journal.ReasonPrompt,
		"1700000012.000000": journal.ReasonCatchUpCommand,
		"1700000013.000000": journal.ReasonCatchUpCommand,
		"1700000021.000000": journal.ReasonAmbientUnknownThrd,
	} {
		if reasons[ts] != want {
			t.Fatalf("ts %s = %q, want %q (%v)", ts, reasons[ts], want, reasons)
		}
	}
}

// TestCatchUpOneTurnPerThread: two threads (and a DM) give one turn
// each; a DM line carries no sender, as a live DM prompt carries none.
func TestCatchUpOneTurnPerThread(t *testing.T) {
	fa := newFakeAgent()
	pl := &promptLog{}
	fa.promptHook = pl.hook
	rs := newRichSlack(t)
	h := New(Config{Router: newTestRouter(t, fa), API: rs.client(), Ambient: true})

	a := cev("U1", "1700000010.000000", "1700000010.000000", "thread a")
	a.IsMention = true
	b := cev("U1", "1700000020.000000", "1700000020.000000", "<@BBOT> thread b")
	dm := cev("U1", "1700000030.000000", "1700000030.000000", "a dm")
	dm.ChannelID, dm.IsDM = "D1", true
	// Thread a was never seen before; the mention summons it, so the
	// ambient reply right after it in the same batch belongs to the turn.
	after := cev("U2", "1700000010.000000", "1700000011.000000", "and me")
	h.CatchUp(context.Background(), []slackproto.Event{a, after, b, dm})
	waitForIdle(t, h)

	got := strings.Join(pl.all(), "\n---\n")
	for _, want := range []string{"[Name-U1] thread a\n\n[Name-U2] and me", "[Name-U1] <@BBOT> thread b", "1 message arrived", "\n\na dm"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompts lack %q:\n%s", want, got)
		}
	}
	if n := len(pl.all()); n != 3 {
		t.Fatalf("%d prompts, want 3", n)
	}
}

func TestCatchUpDropsDisallowed(t *testing.T) {
	fa := newFakeAgent()
	pl := &promptLog{}
	fa.promptHook = pl.hook
	h := New(Config{Router: newTestRouter(t, fa), AllowedUserIDs: map[string]struct{}{"U9": {}}})
	ev := cev("U1", "1.0", "1.0", "hi")
	ev.IsDM = true
	h.CatchUp(context.Background(), []slackproto.Event{ev})
	waitForIdle(t, h)
	if len(pl.all()) != 0 {
		t.Fatal("a disallowed sender must not reach the agent through the catch-up")
	}
}

func TestCatchupPromptKeepsTheNewest(t *testing.T) {
	th := &catchupThread{first: "1700000000.000000"}
	for i := range MaxCatchupPerThread + 5 {
		th.lines = append(th.lines, "line"+string(rune('A'+i%26))+strings.Repeat("x", i/26))
	}
	p := catchupPrompt(th)
	if !strings.Contains(p, "55 messages") || !strings.Contains(p, "Only the newest 50 are below; the oldest 5 are left out.") {
		t.Fatalf("header: %q", p[:200])
	}
	if strings.Contains(p, "\n\nlineA\n") || !strings.HasSuffix(p, th.lines[len(th.lines)-1]) {
		t.Fatal("the oldest lines must be dropped, the newest kept")
	}
}

func TestTSTime(t *testing.T) {
	if got := tsTime("1700000000.123456"); !got.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("tsTime = %v", got)
	}
	if got := tsTime("garbage"); !got.Equal(time.Unix(0, 0)) {
		t.Fatalf("malformed = %v", got)
	}
}

// TestCatchUpTurnCarriesEveryMessagesFiles: files shared across the
// collapsed messages all ride on the one turn; a file shared with no
// text still gets its line.
func TestCatchUpTurnCarriesEveryMessagesFiles(t *testing.T) {
	host := fileHost(t, "xoxb-test", map[string]string{"/a.txt": "a", "/b.txt": "b"})
	fa := newFakeAgent()
	var mu sync.Mutex
	var got []acp.ContentBlock
	fa.promptHook = func(_ context.Context, _ acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
		mu.Lock()
		got = blocks
		mu.Unlock()
		return acp.StopReasonEndTurn, nil
	}
	fs := newFakeSlack()
	defer fs.close()
	h := New(Config{Router: newTestRouter(t, fa), API: fs.client(), FileClient: NewFileClient("xoxb-test")})

	first := cev("U1", "1700000010.000000", "1700000010.000000", "look at these")
	first.ChannelID, first.IsDM = "D1", true
	first.Files = []slackproto.File{{ID: "F1", Name: "a.txt", Size: 1, URL: host.URL + "/a.txt"}}
	second := cev("U1", "1700000010.000000", "1700000011.000000", "")
	second.ChannelID, second.IsDM = "D1", true
	second.Files = []slackproto.File{{ID: "F2", Name: "b.txt", Size: 1, URL: host.URL + "/b.txt"}}
	h.CatchUp(context.Background(), []slackproto.Event{first, second})
	waitForIdle(t, h)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || got[1].ResourceLink == nil || got[2].ResourceLink == nil {
		t.Fatalf("blocks = %+v", got)
	}
	if !strings.Contains(got[0].Text.Text, "look at these\n\n(shared 1 file)") {
		t.Fatalf("prompt = %q", got[0].Text.Text)
	}
}

// TestCatchUpTooOldGetsANotice: messages past CatchupMaxAge are not
// answered; their thread gets one notice counting them.
func TestCatchUpTooOldGetsANotice(t *testing.T) {
	records := captureJournal(t)
	fa := newFakeAgent()
	pl := &promptLog{}
	fa.promptHook = pl.hook
	fs := newFakeSlack()
	defer fs.close()
	now := time.Unix(1700100000, 0)
	h := New(Config{Router: newTestRouter(t, fa), API: fs.client(), CatchupMaxAge: 24 * time.Hour, Now: func() time.Time { return now }})

	dm := func(ch, ts, text string) slackproto.Event {
		ev := cev("U1", "1700000000.000000", ts, text)
		ev.ChannelID, ev.IsDM = ch, true
		return ev
	}
	h.CatchUp(context.Background(), []slackproto.Event{
		dm("D1", "1700000000.000000", "ancient"),
		dm("D1", "1700000001.000000", "also ancient"),
		dm("D2", "1700000002.000000", "old too"),
		dm("D3", "1700099000.000000", "fresh"),
	})
	waitForIdle(t, h)

	if got := pl.all(); len(got) != 1 || !strings.HasSuffix(got[0], "fresh") {
		t.Fatalf("prompts = %q", got)
	}
	fs.mu.Lock()
	bodies := strings.Join(fs.bodies, "\n")
	fs.mu.Unlock()
	for _, want := range []string{
		"[catch-up] 1 message older than 24h arrived while the relay was offline and was not answered.",
		"[catch-up] 2 messages older than 24h arrived while the relay was offline and were not answered.",
	} {
		if !strings.Contains(bodies, want) {
			t.Fatalf("no notice %q in %q", want, bodies)
		}
	}
	var old int
	for _, r := range records() {
		if r.Reason == journal.ReasonCatchUpTooOld {
			old++
		}
	}
	if old != 3 {
		t.Fatalf("catch_up_too_old records = %d", old)
	}
}

// TestCatchUpNoticeFailureIsNotFatal also exercises the real clock.
func TestCatchUpNoticeFailureIsNotFatal(t *testing.T) {
	fs := newFakeSlack()
	defer fs.close()
	fs.postErr = true
	h := New(Config{Router: newTestRouter(t, newFakeAgent()), API: fs.client(), CatchupMaxAge: time.Hour})
	ev := cev("U1", "1700000000.000000", "1700000000.000000", "2023 is long gone")
	ev.IsDM = true
	h.CatchUp(context.Background(), []slackproto.Event{ev})
	waitForIdle(t, h)
}

// TestCatchUpNeverSupersedesANewerLiveTurn: a catch-up turn waiting for
// a slot is dropped when a live message has started a newer turn in
// its thread meanwhile.
func TestCatchUpNeverSupersedesANewerLiveTurn(t *testing.T) {
	fa := newFakeAgent()
	pl := &promptLog{}
	fa.promptHook = pl.hook
	fs := newFakeSlack()
	defer fs.close()
	h := New(Config{Router: newTestRouter(t, fa), API: fs.client(), CatchupTurns: 1})

	h.catchupSlots <- struct{}{} // every slot busy
	missed := cev("U1", "1700000010.000000", "1700000010.000000", "missed")
	missed.ChannelID, missed.IsDM = "D1", true
	h.CatchUp(context.Background(), []slackproto.Event{missed})
	live := missed
	live.TS, live.Text, live.CatchUp = "1700000020.000000", "live", false
	h.Handle(context.Background(), live)
	<-h.catchupSlots
	waitForIdle(t, h)

	if got := pl.all(); len(got) != 1 || got[0] != "live" {
		t.Fatalf("prompts = %q", got)
	}
	if len(h.catchupSlots) != 0 {
		t.Fatal("the dropped turn must give its slot back")
	}
}

func TestCatchUpStopsStartingTurnsOnShutdown(t *testing.T) {
	fa := newFakeAgent()
	pl := &promptLog{}
	fa.promptHook = pl.hook
	h := New(Config{Router: newTestRouter(t, fa), CatchupTurns: 1})
	h.catchupSlots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ev := cev("U1", "1.0", "1.0", "hi")
	ev.IsDM = true
	h.CatchUp(ctx, []slackproto.Event{ev})
	h.bg.Wait()
	if len(pl.all()) != 0 {
		t.Fatal("no turn may start after shutdown")
	}
}

func TestFmtAge(t *testing.T) {
	for d, want := range map[time.Duration]string{24 * time.Hour: "24h", 90 * time.Minute: "1h30m", 30 * time.Second: "30s", 2 * time.Minute: "2m"} {
		if got := fmtAge(d); got != want {
			t.Errorf("fmtAge(%s) = %q, want %q", d, got, want)
		}
	}
}
