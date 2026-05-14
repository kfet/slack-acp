package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/slack-go/slack"

	"github.com/kfet/slack-acp/internal/acpclient"
	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackproto"
)

// ---- fakeAgent: minimal router.Agent implementation for handler tests ----

type fakeAgent struct {
	mu    sync.Mutex
	sinks map[acp.SessionId]acpclient.SessionUpdateSink

	caps        acpclient.Caps
	promptStop  acp.StopReason
	promptErr   error
	promptHook  func(ctx context.Context, sid acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error)
	cancelCount int32
	dropCount   int32
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{sinks: map[acp.SessionId]acpclient.SessionUpdateSink{}}
}

func (f *fakeAgent) Caps() acpclient.Caps { return f.caps }

func (f *fakeAgent) NewSession(_ context.Context, _ string, sink acpclient.SessionUpdateSink, _ []acp.ContentBlock) (acp.SessionId, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sid := acp.SessionId("sid")
	f.sinks[sid] = sink
	return sid, nil
}

func (f *fakeAgent) ListSessions(_ context.Context, _ string) ([]acpclient.SessionInfo, error) {
	return nil, nil
}

func (f *fakeAgent) ResumeSession(_ context.Context, _ string, _ acp.SessionId, _ acpclient.SessionUpdateSink) error {
	return nil
}

func (f *fakeAgent) Prompt(ctx context.Context, sid acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
	if f.promptHook != nil {
		return f.promptHook(ctx, sid, blocks)
	}
	if f.promptErr != nil {
		return "", f.promptErr
	}
	return f.promptStop, nil
}

func (f *fakeAgent) Cancel(_ context.Context, _ acp.SessionId) error {
	atomic.AddInt32(&f.cancelCount, 1)
	return nil
}

func (f *fakeAgent) DropSession(sid acp.SessionId) {
	atomic.AddInt32(&f.dropCount, 1)
	f.mu.Lock()
	delete(f.sinks, sid)
	f.mu.Unlock()
}

func (f *fakeAgent) RebindSink(sid acp.SessionId, sink acpclient.SessionUpdateSink) {
	f.mu.Lock()
	f.sinks[sid] = sink
	f.mu.Unlock()
}

// emit synthesises a session/update from the agent side.
func (f *fakeAgent) emit(sid acp.SessionId, n acp.SessionNotification) {
	f.mu.Lock()
	sink := f.sinks[sid]
	f.mu.Unlock()
	if sink != nil {
		_ = sink.OnUpdate(context.Background(), n)
	}
}

// ---- fake Slack server (httptest) ----

type fakeSlack struct {
	srv *httptest.Server

	mu        sync.Mutex
	posts     int
	updates   int
	postErr   bool
	updateErr bool
	postedTS  string
	bodies    []string
}

func newFakeSlack() *fakeSlack {
	fs := &fakeSlack{postedTS: "1.0"}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.posts++
		_ = r.ParseForm()
		fs.bodies = append(fs.bodies, r.FormValue("text"))
		err := fs.postErr
		ts := fs.postedTS
		fs.mu.Unlock()
		if err {
			_, _ = w.Write([]byte(`{"ok":false,"error":"oops"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"` + ts + `","message":{"text":"x"}}`))
	})
	mux.HandleFunc("/chat.update", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.updates++
		_ = r.ParseForm()
		fs.bodies = append(fs.bodies, r.FormValue("text"))
		err := fs.updateErr
		fs.mu.Unlock()
		if err {
			_, _ = w.Write([]byte(`{"ok":false,"error":"nope"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1.0","text":"x"}`))
	})
	fs.srv = httptest.NewServer(mux)
	return fs
}

func (fs *fakeSlack) close() { fs.srv.Close() }

func (fs *fakeSlack) client() *slack.Client {
	return slack.New("xoxb-fake", slack.OptionAPIURL(fs.srv.URL+"/"))
}

// ---- helpers ----

func newTestRouter(t *testing.T, fa *fakeAgent) *router.Router {
	t.Helper()
	r, err := router.New(router.Config{Agent: fa, StateDir: t.TempDir(), IdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// ---- allowed gate ----

func TestAllowed(t *testing.T) {
	h := &Handler{cfg: Config{
		AllowedUserIDs:    map[string]struct{}{"U1": {}},
		AllowedChannelIDs: map[string]struct{}{"C1": {}},
		PromptTimeout:     time.Second,
	}}
	if !h.allowed(slackproto.Event{UserID: "U1", ChannelID: "C1"}) {
		t.Fatal("expected allowed")
	}
	if h.allowed(slackproto.Event{UserID: "U2", ChannelID: "C1"}) {
		t.Fatal("user not in allowlist should be denied")
	}
	if h.allowed(slackproto.Event{UserID: "U1", ChannelID: "C2"}) {
		t.Fatal("channel not in allowlist should be denied")
	}
	h2 := &Handler{cfg: Config{}}
	if !h2.allowed(slackproto.Event{UserID: "anyone", ChannelID: "any"}) {
		t.Fatal("empty allowlist should allow all")
	}
}

// Compile-time assertion that *Handler satisfies slackproto.Handler.
var _ slackproto.Handler = (*Handler)(nil)

func TestNewDefaultsTimeout(t *testing.T) {
	h := New(Config{})
	if h.cfg.PromptTimeout == 0 {
		t.Fatal("default timeout not set")
	}
}

func TestSetAPI(t *testing.T) {
	h := New(Config{})
	api := slack.New("xoxb-x")
	h.SetAPI(api)
	if h.cfg.API != api {
		t.Fatal("SetAPI did not install client")
	}
}

// ---- Handle: drop & happy paths ----

func TestHandleDropsDisallowed(t *testing.T) {
	h := New(Config{AllowedUserIDs: map[string]struct{}{"U1": {}}})
	h.Handle(context.Background(), slackproto.Event{UserID: "intruder"})
	if h.inflightCount() != 0 {
		t.Fatal("disallowed event should not start work")
	}
}

func TestHandleDropsEmptyText(t *testing.T) {
	h := New(Config{})
	h.Handle(context.Background(), slackproto.Event{UserID: "U", ChannelID: "C", Text: "   \n"})
	if h.inflightCount() != 0 {
		t.Fatal("empty text should not start work")
	}
}

func TestHandleDeliversPrompt(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	done := make(chan struct{})
	fa.promptHook = func(ctx context.Context, sid acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
		// Push some streaming output back through the sink to exercise
		// sink + post path.
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "hello"}}}},
		})
		close(done)
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second})
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "hi"})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt never invoked")
	}
	// Wait for the goroutine to finish.
	waitForIdle(t, h)
	if fs.posts == 0 {
		t.Fatal("expected at least one Slack post")
	}
}

func TestHandleAgentError(t *testing.T) {
	fa := newFakeAgent()
	fa.promptErr = errors.New("boom")
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second})
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "x"})
	waitForIdle(t, h)
	// Should have surfaced an error message.
	if !anyContains(fs.bodies, "_error:") {
		t.Fatalf("no error post; bodies=%q", fs.bodies)
	}
}

func TestHandleNonEndTurnSuffix(t *testing.T) {
	fa := newFakeAgent()
	fa.promptStop = acp.StopReasonMaxTokens
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second})
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "x"})
	waitForIdle(t, h)
	if !anyContains(fs.bodies, "(stopped:") {
		t.Fatalf("expected stop suffix; bodies=%q", fs.bodies)
	}
}

func TestHandleRouterCreateError(t *testing.T) {
	// Force GetOrCreate to fail by passing an invalid ConvKey
	// (validateKeyComponent rejects "..").
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second})
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "..", ThreadTS: "1.0", TS: "1.0", Text: "x"})
	waitForIdle(t, h)
	// We expect a Slack error post (the streamer's Close prepends
	// "_error: ..."). However the post may not have been triggered if
	// PostStreamer isn't usable; the important thing is no panic and
	// goroutine returns. Just assert the in-flight map drained.
	if h.inflightCount() != 0 {
		t.Fatal("inflight not drained on error")
	}
}

// ---- Cancel-on-followup: a second event for the same thread cancels
// the in-flight prompt before starting the new one. ----

func TestHandleCancelsOnFollowup(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	// First prompt blocks until ctx cancelled.
	startedCh := make(chan struct{})
	releaseCh := make(chan struct{})
	var firstStart sync.Once
	fa.promptHook = func(ctx context.Context, _ acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		firstStart.Do(func() { close(startedCh) })
		select {
		case <-ctx.Done():
			return acp.StopReasonCancelled, nil
		case <-releaseCh:
			return acp.StopReasonEndTurn, nil
		}
	}

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second})
	ev := slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "first"}
	h.Handle(context.Background(), ev)
	<-startedCh

	// Followup with same key: must cancel first.
	ev2 := ev
	ev2.TS = "2.0"
	ev2.Text = "second"
	h.Handle(context.Background(), ev2)
	close(releaseCh)
	waitForIdle(t, h)

	if atomic.LoadInt32(&fa.cancelCount) == 0 {
		t.Fatal("expected agent.Cancel to have been called")
	}
}

// ---- clearInflight idempotence: a stale cancel entry from a previous
// run must not delete the current entry. ----

func TestClearInflightIgnoresStale(t *testing.T) {
	h := New(Config{})
	key := router.ConvKey{ChannelID: "C", ThreadTS: "T"}
	_, cOld := context.WithCancel(context.Background())
	_, cCur := context.WithCancel(context.Background())
	old := &inflightEntry{cancel: cOld}
	cur := &inflightEntry{cancel: cCur}
	h.setInflight(key, cur)
	h.clearInflight(key, old)
	if h.inflightCount() != 1 {
		t.Fatalf("stale clear should not have removed entry; len=%d", h.inflightCount())
	}
	h.clearInflight(key, cur)
	if h.inflightCount() != 0 {
		t.Fatal("matching clear should remove entry")
	}
}

// ---- watchdog: covers the FlushIfPending path + ctx exit ----

func TestWatchdogExits(t *testing.T) {
	fs := newFakeSlack()
	defer fs.close()
	stream := slackproto.NewPostStreamer(fs.client(), "C1", "1.0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { watchdog(ctx, stream); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not exit")
	}
}

func TestWatchdogTickFlushes(t *testing.T) {
	fs := newFakeSlack()
	defer fs.close()
	stream := slackproto.NewPostStreamer(fs.client(), "C1", "1.0")
	// First post primes the streamer with a ts; subsequent appends
	// queue as pending until the watchdog flushes.
	if err := stream.Append(context.Background(), "first "); err != nil {
		t.Fatal(err)
	}
	// Now write more without flushing — buffered as pending.
	for i := 0; i < 3; i++ {
		if err := stream.Append(context.Background(), "x"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchdog(ctx, stream)
	// Tick is 1s. Wait for an update to land.
	for i := 0; i < 50; i++ {
		fs.mu.Lock()
		u := fs.updates
		fs.mu.Unlock()
		if u >= 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("watchdog never flushed")
}

// ---- helpers used above ----

func (h *Handler) inflightCount() int {
	h.inflightMu.Lock()
	defer h.inflightMu.Unlock()
	return len(h.inflight)
}

func waitForIdle(t *testing.T, h *Handler) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if h.inflightCount() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("handler never went idle")
}

func anyContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ---- silence unused imports if a refactor drops a code path ----
var _ = json.Marshal

// TestHandleInlinesSystemPromptOnFirstPrompt: when the router has a
// SystemPrompt and the agent doesn't advertise the cap, the FIRST user
// prompt for a thread must be prefixed with the system-prompt text;
// follow-up prompts on the same thread must not be.
func TestHandleInlinesSystemPromptOnFirstPrompt(t *testing.T) {
	fa := newFakeAgent()
	// caps zero — no SystemPrompt advertised.
	r, err := router.New(router.Config{
		Agent: fa, StateDir: t.TempDir(), IdleTimeout: time.Minute,
		SystemPrompt: "SP-HEADER",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })

	fs := newFakeSlack()
	defer fs.close()

	var firstText, secondText string
	var got int32
	gotCh := make(chan struct{}, 2)
	fa.promptHook = func(_ context.Context, sid acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
		n := atomic.AddInt32(&got, 1)
		if len(blocks) > 0 && blocks[0].Text != nil {
			if n == 1 {
				firstText = blocks[0].Text.Text
			} else {
				secondText = blocks[0].Text.Text
			}
		}
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "ok"}}}},
		})
		gotCh <- struct{}{}
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second})

	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "hello"})
	<-gotCh
	waitForIdle(t, h)
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "2.0", Text: "again"})
	<-gotCh
	waitForIdle(t, h)

	if !strings.HasPrefix(firstText, "SP-HEADER\n\n") || !strings.HasSuffix(firstText, "hello") {
		t.Fatalf("first prompt not prefixed: %q", firstText)
	}
	if secondText != "again" {
		t.Fatalf("second prompt mangled (must not re-prefix): %q", secondText)
	}
}
