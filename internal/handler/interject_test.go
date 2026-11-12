package handler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/interject"
	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackproto"
)

// ijEnv runs a main turn that blocks until released, and answers fork
// prompts with forkOut. Main prompt texts are recorded in order.
type ijEnv struct {
	t       *testing.T
	fa      *fakeAgent
	fs      *fakeSlack
	h       *Handler
	started chan struct{}
	release chan struct{}

	mu      sync.Mutex
	mains   []string
	forkOut string
	// onFork runs inside the fork prompt, before it answers.
	onFork func()
}

func newIJEnv(t *testing.T, forkOut string) *ijEnv {
	t.Helper()
	e := &ijEnv{t: t, fa: newFakeAgent(), fs: newFakeSlack(), forkOut: forkOut,
		started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(e.fs.close)
	var once sync.Once
	e.fa.promptHook = func(ctx context.Context, sid acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
		if strings.HasPrefix(string(sid), "fork-") {
			if e.onFork != nil {
				e.onFork()
			}
			e.fa.mu.Lock()
			sink := e.fa.sinks[sid]
			e.fa.mu.Unlock()
			_ = sink.OnUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: acp.UpdateAgentMessageText(e.forkOut)})
			return acp.StopReasonEndTurn, nil
		}
		e.mu.Lock()
		e.mains = append(e.mains, blocks[0].Text.Text)
		n := len(e.mains)
		e.mu.Unlock()
		if n > 1 {
			return acp.StopReasonEndTurn, nil
		}
		once.Do(func() { close(e.started) })
		select {
		case <-ctx.Done():
			return acp.StopReasonCancelled, nil
		case <-e.release:
			return acp.StopReasonEndTurn, nil
		}
	}
	e.h = New(Config{Router: newTestRouter(t, e.fa), API: e.fs.client(), NoProgressTimeout: 5 * time.Second,
		Interject: interject.Config{Enabled: true}})
	return e
}

var ijEv = slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "first", IsDM: true}

// start runs the first message's turn and waits until it is prompting,
// then sends the mid-turn message and waits for the fork's handling.
func (e *ijEnv) interject() {
	e.send()
	e.h.bg.Wait()
}

// send starts the main turn and sends the mid-turn message, without
// waiting for the fork.
func (e *ijEnv) send() {
	e.h.Handle(context.Background(), ijEv)
	<-e.started
	ev := ijEv
	ev.TS, ev.Text = "2.0", "second"
	e.h.Handle(context.Background(), ev)
}

func (e *ijEnv) finish() {
	e.unblock()
	waitForIdle(e.t, e.h)
}

func (e *ijEnv) unblock() {
	select {
	case <-e.release:
	default:
		close(e.release)
	}
}

func (e *ijEnv) mainPrompts() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.mains...)
}

func (e *ijEnv) posted(sub string) bool {
	e.fs.mu.Lock()
	defer e.fs.mu.Unlock()
	for _, b := range e.fs.bodies {
		if strings.Contains(b, sub) {
			return true
		}
	}
	return false
}

func TestInterjectNoneLeavesMainRunning(t *testing.T) {
	e := newIJEnv(t, "it is going well\n1 9")
	e.interject()
	if n := atomic.LoadInt32(&e.fa.cancelCount); n != 0 {
		t.Fatalf("main turn cancelled %d times", n)
	}
	if !e.posted("it is going well") || !e.posted("quick reply · answered") {
		t.Fatalf("fork answer not posted: %q", e.fs.bodies)
	}
	if n := atomic.LoadInt32(&e.fa.closeCount); n != 1 {
		t.Fatalf("fork closed %d times", n)
	}
	e.finish()
	// The side Q&A is carried into the next main prompt.
	ev := ijEv
	ev.TS, ev.Text = "3.0", "third"
	e.h.Handle(context.Background(), ev)
	waitForIdle(t, e.h)
	m := e.mainPrompts()
	if len(m) != 2 || !strings.Contains(m[1], "Q: second\nA: it is going well") || !strings.HasSuffix(m[1], "third") {
		t.Fatalf("main prompts = %q", m)
	}
}

func TestInterjectSteerSupersedes(t *testing.T) {
	e := newIJEnv(t, "redirecting\n3 9")
	e.interject()
	e.finish()
	if atomic.LoadInt32(&e.fa.cancelCount) == 0 {
		t.Fatal("steer did not cancel the main turn")
	}
	m := e.mainPrompts()
	if len(m) != 2 || !strings.Contains(m[1], "[while you were working") || !strings.HasSuffix(m[1], "second") {
		t.Fatalf("main prompts = %q", m)
	}
}

func TestInterjectAbortStops(t *testing.T) {
	e := newIJEnv(t, "stopping\n4 9")
	e.interject()
	waitForIdle(t, e.h)
	if atomic.LoadInt32(&e.fa.cancelCount) == 0 {
		t.Fatal("abort did not cancel the main turn")
	}
	if m := e.mainPrompts(); len(m) != 1 {
		t.Fatalf("abort started a turn: %q", m)
	}
	e.finish()
}

func TestInterjectQueueWhileRunningWaits(t *testing.T) {
	e := newIJEnv(t, "noted\n2 9")
	// Main is released only once the queued message waits on it.
	e.h.queueWait = func() {
		if m := e.mainPrompts(); len(m) != 1 {
			t.Errorf("queue started a turn while main ran: %q", m)
		}
		e.unblock()
	}
	e.send()
	waitForIdle(t, e.h)
	// The queued message is delivered once the main turn ends.
	m := e.mainPrompts()
	if len(m) != 2 || !strings.Contains(m[1], "[queued messages from the user]\nsecond") {
		t.Fatalf("main prompts = %q", m)
	}
}

func TestInterjectQueueAfterMainEndedStartsTurn(t *testing.T) {
	e := newIJEnv(t, "noted\n2 9")
	e.onFork = func() {
		e.unblock()
		if err := e.h.convo.Active().WaitIdle(context.Background()); err != nil {
			t.Error(err)
		}
	}
	e.interject()
	waitForIdle(t, e.h)
	m := e.mainPrompts()
	if len(m) != 2 || !strings.Contains(m[1], "[queued messages from the user]\nsecond") {
		t.Fatalf("main prompts = %q", m)
	}
}

func TestInterjectCheckpointsAmbient(t *testing.T) {
	e := newIJEnv(t, "fine\n1 9")
	e.h.cfg.Ambient = true
	e.interject()
	e.finish()
	key := router.ConvKey{ChannelID: ijEv.ChannelID, ThreadTS: ijEv.ThreadTS}
	if got := e.h.cfg.Router.GetLastTS(key); got != "2.0" {
		t.Fatalf("last_ts = %q, want the interjected 2.0", got)
	}
}

func TestInterjectForkErrorSupersedes(t *testing.T) {
	e := newIJEnv(t, "")
	e.fa.forkErr = errors.New("no fork")
	e.interject()
	e.finish()
	if atomic.LoadInt32(&e.fa.cancelCount) == 0 {
		t.Fatal("fallback did not supersede")
	}
	if m := e.mainPrompts(); len(m) != 2 || m[1] != "second" {
		t.Fatalf("main prompts = %q", m)
	}
}

func TestInterjectVerdictOnlyPostsNothing(t *testing.T) {
	e := newIJEnv(t, "1 9")
	e.interject()
	if e.posted("quick reply") {
		t.Fatal("empty answer was posted")
	}
	e.finish()
}

func TestInterjectReplyErrorStillActs(t *testing.T) {
	e := newIJEnv(t, "stopping\n4 9")
	e.onFork = func() {
		e.fs.mu.Lock()
		e.fs.postErr = true
		e.fs.mu.Unlock()
	}
	e.interject()
	waitForIdle(t, e.h)
	if atomic.LoadInt32(&e.fa.cancelCount) == 0 {
		t.Fatal("abort did not cancel after a failed reply")
	}
	e.finish()
}

func TestInterjectableGates(t *testing.T) {
	e := newIJEnv(t, "")
	conv := router.ConvKey{ChannelID: "C1", ThreadTS: "1.0"}.String()
	if e.h.interjectable(ijEv, conv, "hi") {
		t.Fatal("no running turn: must not interject")
	}
	e.h.Handle(context.Background(), ijEv)
	<-e.started
	if !e.h.interjectable(ijEv, conv, "hi") {
		t.Fatal("running turn: should interject")
	}
	ambient := ijEv
	ambient.IsDM, ambient.BotUserID = false, "UBOT"
	if e.h.interjectable(ambient, conv, "hi") {
		t.Fatal("an unaddressed ambient reply must not get a fork's answer")
	}
	if e.h.interjectable(ijEv, conv, "!stop") {
		t.Fatal("commands must go to convo")
	}
	withFile := ijEv
	withFile.Files = []slackproto.File{{ID: "F1"}}
	if e.h.interjectable(withFile, conv, "hi") {
		t.Fatal("attachments must go to the main turn")
	}
	off := New(Config{Router: e.h.cfg.Router, API: e.fs.client()})
	if off.interjectable(ijEv, conv, "hi") {
		t.Fatal("disabled: must not interject")
	}
	e.finish()
}

func TestAskWithoutSessionFails(t *testing.T) {
	e := newIJEnv(t, "")
	key := router.ConvKey{ChannelID: "C9", ThreadTS: "9.0"}
	if _, err := e.h.ask(context.Background(), ijEv, key, "hi"); !errors.Is(err, errNoSession) {
		t.Fatalf("err = %v", err)
	}
}
