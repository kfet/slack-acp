package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/slack-go/slack"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackproto"
	"github.com/kfet/slack-acp/internal/statusline"
)

// ---- fakeAgent: minimal router.Agent implementation for handler tests ----

type fakeAgent struct {
	mu    sync.Mutex
	sinks map[acp.SessionId]client.SessionUpdateSink

	caps        client.Caps
	promptStop  acp.StopReason
	promptErr   error
	promptHook  func(ctx context.Context, sid acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error)
	newSessErr  error // when set, NewSession returns this error
	cancelCount int32
	dropCount   int32
	// currentModel is returned from Models(); tests set this to
	// exercise provider-emoji resolution. Empty by default → no
	// emoji segment.
	currentModel string
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{sinks: map[acp.SessionId]client.SessionUpdateSink{}}
}

func (f *fakeAgent) Caps() client.Caps { return f.caps }

func (f *fakeAgent) NewSession(_ context.Context, _ string, sink client.SessionUpdateSink, _ []acp.ContentBlock) (acp.SessionId, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newSessErr != nil {
		return "", f.newSessErr
	}
	sid := acp.SessionId("sid")
	f.sinks[sid] = sink
	return sid, nil
}

func (f *fakeAgent) ListSessions(_ context.Context, _ string) ([]client.SessionInfo, error) {
	return nil, nil
}

func (f *fakeAgent) ResumeSession(_ context.Context, _ string, _ acp.SessionId, _ client.SessionUpdateSink) error {
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

func (f *fakeAgent) RebindSink(sid acp.SessionId, sink client.SessionUpdateSink) {
	f.mu.Lock()
	f.sinks[sid] = sink
	f.mu.Unlock()
}

func (f *fakeAgent) Models() (models []client.ModelInfo, currentID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return nil, f.currentModel
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

	// updated is a non-blocking signal channel that receives once per
	// chat.update call. Tests wait on it instead of polling
	// fs.updates with sleeps. Buffered so the handler never blocks
	// when no one is listening.
	updated chan struct{}
}

func newFakeSlack() *fakeSlack {
	fs := &fakeSlack{postedTS: "1.0", updated: make(chan struct{}, 16)}
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
		// Non-blocking signal so the chat.update path never stalls
		// even if nobody is reading fs.updated.
		select {
		case fs.updated <- struct{}{}:
		default:
		}
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

// TestHandleResolvesModelIdentity verifies that when the agent reports
// a current model id, the handler resolves BOTH halves of the model
// identity — provider emoji and short display name — and pushes them
// into the sink, so the live spinner and the final footer both name
// the model.
func TestHandleResolvesModelIdentity(t *testing.T) {
	fa := newFakeAgent()
	fa.currentModel = "anthropic/claude-sonnet-4"
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	done := make(chan struct{})
	fa.promptHook = func(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "ok"}}}},
		})
		close(done)
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second})
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "hi"})
	<-done
	waitForIdle(t, h)
	// The vendor echo is dropped ("claude-" is what the 🏛️ already
	// says), so the short name is "sonnet-4", not "claude-sonnet-4".
	final := lastBody(t, fs)
	if !strings.HasSuffix(final, "\n\n_🏛️ sonnet-4_") {
		t.Fatalf("expected model identity footer; got %q", final)
	}
}

// TestHandleAppendsStatusFooter drives a whole turn through the
// handler and pins the rendered shape: the answer, then a blank line,
// then the status line in Slack italics — with mood/plan that arrived
// AFTER the first chunk, which is the reason the line is a footer.
func TestHandleAppendsStatusFooter(t *testing.T) {
	fa := newFakeAgent()
	fa.currentModel = "anthropic/claude-opus-4-5-20251001"
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	fa.promptHook = func(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "the answer"}}}},
		})
		// Mood/plan land only once the agent is under way — after the
		// first chunk. A prepended header could never have shown these.
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Meta: map[string]any{
				statusline.ExtensionID: map[string]any{"mood": "steady", "plan": "2/5"},
			},
		})
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second})
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "hi"})
	waitForIdle(t, h)

	final := lastBody(t, fs)
	if final != "the answer\n\n_🏛️ opus-4.5 • steady • 2/5_" {
		t.Fatalf("rendered shape wrong; got %q", final)
	}
	if n := strings.Count(final, "opus-4.5"); n != 1 {
		t.Fatalf("footer must appear exactly once in the posted message; got %d", n)
	}
}

// TestHandleNoStatusFooterOnErrorTurn: an error turn is not signed.
// The handler's error paths Close the stream with an "_error: …_"
// suffix and never reach the footer, which is what keeps this
// structural rather than a flag someone can forget to set.
func TestHandleNoStatusFooterOnErrorTurn(t *testing.T) {
	fa := newFakeAgent()
	fa.currentModel = "anthropic/claude-opus-4-5-20251001"
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	fa.promptHook = func(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		// The turn DID produce user-visible content and a full status
		// before failing — so the content gate cannot be what
		// suppresses the footer here. Only the error path can.
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "partial answer"}}}},
		})
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Meta: map[string]any{
				statusline.ExtensionID: map[string]any{"mood": "steady", "plan": "2/5"},
			},
		})
		return "", errors.New("boom")
	}

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second})
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "x"})
	waitForIdle(t, h)

	final := lastBody(t, fs)
	if !strings.Contains(final, "_error:") {
		t.Fatalf("expected the error post; got %q", final)
	}
	if !strings.Contains(final, "partial answer") {
		t.Fatalf("partial content must survive the error post; got %q", final)
	}
	if strings.Contains(final, "opus-4.5") || strings.Contains(final, "steady") {
		t.Fatalf("error turn must not carry a status footer; got %q", final)
	}
}

// TestHandleStatusFooterFollowsStopSuffix: when a turn stops for a
// non-end-turn reason the "_(stopped: …)_" marker is part of the
// answer, and the status footer is appended BELOW it — the footer is
// always the last thing in the message.
func TestHandleStatusFooterFollowsStopSuffix(t *testing.T) {
	fa := newFakeAgent()
	fa.currentModel = "openai/gpt-5-codex"
	fa.promptStop = acp.StopReasonMaxTokens
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	fa.promptHook = func(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "partial"}}}},
		})
		return acp.StopReasonMaxTokens, nil
	}

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second})
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "x"})
	waitForIdle(t, h)

	final := lastBody(t, fs)
	if final != "partial\n_(stopped: max_tokens)_\n\n_🌐 gpt-5-codex_" {
		t.Fatalf("footer must come last, below the stop marker; got %q", final)
	}
}

// TestHandleNoStatusFooterOnContentlessTurn: the agent produced no
// user-visible output, so the message is just the "_thinking…_"
// fallback body — there is no answer to sign.
func TestHandleNoStatusFooterOnContentlessTurn(t *testing.T) {
	fa := newFakeAgent()
	fa.currentModel = "anthropic/claude-opus-4-5-20251001"
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	fa.promptHook = func(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		// Status but no content at all.
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Meta: map[string]any{
				statusline.ExtensionID: map[string]any{"mood": "steady", "plan": "2/5"},
			},
		})
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second})
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "x"})
	waitForIdle(t, h)

	if final := lastBody(t, fs); strings.Contains(final, "opus-4.5") {
		t.Fatalf("contentless turn must not be signed; got %q", final)
	}
}

// lastBody returns the text of the most recent Slack write — the one
// the user is actually left looking at. Asserting on the JOIN of all
// bodies is not good enough on this surface: the relay EDITS one
// message, so intermediate bodies are superseded, and spinner frames
// carry the same model identity as the footer.
func lastBody(t *testing.T, fs *fakeSlack) string {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.bodies) == 0 {
		t.Fatal("no Slack writes at all")
	}
	return fs.bodies[len(fs.bodies)-1]
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
	// We expect a Slack error post (the streamer's Close appends
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
	go watchdogWithTick(ctx, stream, time.Millisecond)
	// Wait deterministically for the first chat.update via fs.updated.
	select {
	case <-fs.updated:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog never flushed")
	}
}

// ---- helpers used above ----

func (h *Handler) inflightCount() int {
	h.inflightMu.Lock()
	defer h.inflightMu.Unlock()
	return len(h.inflight)
}

// ---- spinner ----

func TestSpinnerExitsOnCtx(t *testing.T) {
	fs := newFakeSlack()
	defer fs.close()
	stream := slackproto.NewPostStreamer(fs.client(), "C1", "1.0")
	if err := stream.Start(context.Background(), "> _Thinking…_"); err != nil {
		t.Fatal(err)
	}
	sink := newStreamingSink(stream, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { spinner(ctx, stream, sink); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("spinner did not exit on ctx cancel")
	}
}

func TestSpinnerTicksAndSelfDisarms(t *testing.T) {
	fs := newFakeSlack()
	defer fs.close()
	stream := slackproto.NewPostStreamer(fs.client(), "C1", "1.0")
	stream.SetMinInterval(0) // tests don't gate placeholder updates
	if err := stream.Start(context.Background(), "> _Thinking…_"); err != nil {
		t.Fatal(err)
	}
	sink := newStreamingSink(stream, false)
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Meta: map[string]any{
			statusline.ExtensionID: map[string]any{"mood": "steady"},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { spinnerWithTick(ctx, stream, sink, time.Millisecond); close(done) }()

	select {
	case <-fs.updated:
	case <-time.After(2 * time.Second):
		t.Fatal("spinner never updated")
	}

	fs.mu.Lock()
	last := fs.bodies[len(fs.bodies)-1]
	fs.mu.Unlock()
	if !strings.Contains(last, "steady") || !strings.Contains(last, "Thinking") {
		t.Fatalf("frame missing expected content: %q", last)
	}

	stream.FirstChunk()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("spinner did not self-disarm after FirstChunk")
	}
}

// TestWaitIdleCancel covers the ctx-cancellation branch: an inflight
// entry is held; WaitIdle blocks; the caller cancels its ctx and the
// helper goroutine broadcasts to unblock the Cond.Wait loop.
func TestWaitIdleCancel(t *testing.T) {
	h := New(Config{})
	key := router.ConvKey{ChannelID: "C", ThreadTS: "T"}
	_, c := context.WithCancel(context.Background())
	h.setInflight(key, &inflightEntry{cancel: c})
	defer func() {
		h.inflightMu.Lock()
		delete(h.inflight, key)
		h.inflightCond.Broadcast()
		h.inflightMu.Unlock()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.WaitIdle(ctx) }()
	// Wait until WaitIdle has parked in Cond.Wait, so cancel is guaranteed
	// to wake it through the helper goroutine (no race where ctx is already
	// cancelled when WaitIdle's loop first checks it).
	for {
		h.inflightMu.Lock()
		w := h.waitIdleWaits
		h.inflightMu.Unlock()
		if w > 0 {
			break
		}
		runtime.Gosched()
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want ctx error from cancelled WaitIdle")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitIdle did not return after cancel")
	}
}

// TestWaitIdleAlreadyIdle covers the fast-path return when there's
// nothing inflight: WaitIdle returns immediately and the helper
// goroutine exits via the close(stop) signal (not via ctx).
func TestWaitIdleAlreadyIdle(t *testing.T) {
	h := New(Config{})
	if err := h.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle on empty handler: %v", err)
	}
}

func waitForIdle(t *testing.T, h *Handler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.WaitIdle(ctx); err != nil {
		t.Fatalf("handler never went idle: %v", err)
	}
}

func anyContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// TestHandleStartPlaceholderError covers the (non-fatal) error path
// when the immediate placeholder post fails — the handler must log
// and continue, and the normal flow still completes.
func TestHandleStartPlaceholderError(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	fs.postErr = true
	defer fs.close()

	done := make(chan struct{})
	fa.promptHook = func(ctx context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		// Emit one chunk so the streamer attempts another post (which
		// also fails, surfacing as a debug log) — flow still returns.
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "x"}}}},
		})
		close(done)
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second})
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "hi"})
	<-done
	waitForIdle(t, h)
}

// TestHandlePostsThinkingPlaceholder verifies the immediate "Thinking…"
// indicator is posted *before* the agent produces any output. The fake
// agent blocks until released, so the only message that can land
// during that window is the placeholder.
func TestHandlePostsThinkingPlaceholder(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	started := make(chan struct{})
	release := make(chan struct{})
	fa.promptHook = func(ctx context.Context, _ acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		close(started)
		<-release
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second})
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "hi"})

	// Once the agent's Prompt has been entered, Start must already
	// have posted the placeholder. Read the captured bodies under lock.
	<-started
	fs.mu.Lock()
	posts := fs.posts
	firstBody := ""
	if len(fs.bodies) > 0 {
		firstBody = fs.bodies[0]
	}
	fs.mu.Unlock()
	if posts < 1 || !strings.Contains(firstBody, "Thinking") {
		t.Fatalf("expected immediate Thinking placeholder; posts=%d first=%q", posts, firstBody)
	}

	close(release)
	waitForIdle(t, h)
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

// TestHandleAmbientModeDropsUnknownThread verifies that when Ambient is
// enabled, non-DM non-mention messages are only forwarded to threads
// the bot is already part of (Known returns true).
func TestHandleAmbientModeDropsUnknownThread(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	h := New(Config{
		Router:  r,
		API:     fs.client(),
		Ambient: true, // Enable ambient mode
	})

	// First message: no mention, no DM, thread is not known yet.
	// BotUserID is set (simulates production slackproto).
	h.Handle(context.Background(), slackproto.Event{
		UserID:    "U1",
		BotUserID: "BBOT",
		ChannelID: "C1",
		ThreadTS:  "1.0",
		TS:        "1.0",
		Text:      "hello",
		IsDM:      false,
	})

	// Give it a moment to process.
	time.Sleep(50 * time.Millisecond)

	// Should be dropped (thread not known).
	if len(fa.sinks) > 0 {
		t.Fatal("ambient message in unknown thread should be dropped")
	}
}

// TestHandleMentionFlagSummonsUnknownThread is the regression test for
// the v0.4.1 bug where an @-mention in a channel was silently dropped.
// slackproto strips the bot mention from Text on the app_mention path,
// so the handler's old strings.Contains(Text, "<@BOT>") check always
// saw false, classified a genuine summon as an ambient reply, and
// dropped it because the thread was not yet known. The IsMention flag
// carries the fact across that boundary.
func TestHandleMentionFlagSummonsUnknownThread(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	done := make(chan struct{})
	fa.promptHook = func(ctx context.Context, sid acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
		close(done)
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{
		Router:        r,
		API:           fs.client(),
		Ambient:       true,
		PromptTimeout: 5 * time.Second,
	})

	// A top-level @-mention in a channel: thread is NOT known, and Text
	// has the mention already stripped (exactly what slackproto sends).
	h.Handle(context.Background(), slackproto.Event{
		UserID:    "U1",
		BotUserID: "BBOT",
		ChannelID: "C1",
		ThreadTS:  "1.0",
		TS:        "1.0",
		Text:      "let's chat",
		IsDM:      false,
		IsMention: true,
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an @-mention must summon the bot even in an unknown thread")
	}
	waitForIdle(t, h)
}

// TestHandleAmbientModeForwardsKnownThread verifies that ambient replies
// are forwarded to threads the bot is already in.
func TestHandleAmbientModeForwardsKnownThread(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	promptCount := 0
	done := make(chan struct{})
	fa.promptHook = func(_ context.Context, _ acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		promptCount++
		if promptCount == 2 {
			close(done)
		}
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{
		Router:  r,
		API:     fs.client(),
		Ambient: true,
	})

	// First message: @-mention summons the bot.
	h.Handle(context.Background(), slackproto.Event{
		UserID:    "U1",
		BotUserID: "BBOT",
		ChannelID: "C1",
		ThreadTS:  "1.0",
		TS:        "1.0",
		Text:      "<@BBOT> hello",
		IsDM:      false,
	})

	// Wait for first prompt to complete.
	time.Sleep(100 * time.Millisecond)
	waitForIdle(t, h)

	// Second message: no mention, but thread is now known.
	h.Handle(context.Background(), slackproto.Event{
		UserID:    "U2",
		BotUserID: "BBOT",
		ChannelID: "C1",
		ThreadTS:  "1.0",
		TS:        "2.0",
		Text:      "follow-up",
		IsDM:      false,
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ambient reply in known thread should be forwarded")
	}
	waitForIdle(t, h)

	if promptCount != 2 {
		t.Fatalf("expected 2 prompts (mention + ambient), got %d", promptCount)
	}
}

// TestAbstainSuppressesPostOnSentinel verifies that in ambient mode,
// when the agent emits exactly the silent sentinel, nothing is posted
// to Slack (no message, no leaked placeholder).
func TestAbstainSuppressesPostOnSentinel(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	fa.promptHook = func(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "<<SILENT>>"}}}},
		})
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second, Ambient: true, SilentSentinel: "<<SILENT>>"})
	// Ambient reply in a known thread: first summon, then follow-up.
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", BotUserID: "BBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "<@BBOT> hi"})
	waitForIdle(t, h)
	// Reset counters after the (posted) summon turn.
	fs.mu.Lock()
	fs.posts = 0
	fs.updates = 0
	fs.mu.Unlock()

	h.Handle(context.Background(), slackproto.Event{UserID: "U2", BotUserID: "BBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "2.0", Text: "chatter"})
	waitForIdle(t, h)

	fs.mu.Lock()
	posts, updates := fs.posts, fs.updates
	fs.mu.Unlock()
	if posts != 0 || updates != 0 {
		t.Fatalf("abstain must post nothing; got posts=%d updates=%d", posts, updates)
	}
}

// TestAbstainDivergedTurnStillSigned is the abstain half of the
// buffered-answer hazard. On the abstain path NOTHING is written to
// the streamer while the turn runs — the sink holds every chunk back
// in case the whole answer turns out to be the silent sentinel — and
// releases the lot in one go at Finalize, which is just before the
// turn ends. A footer gated on "has anything reached Slack yet?"
// would therefore be dropped on every abstain-eligible turn that
// actually answered. It gates on the streamer's BUFFER instead, which
// Finalize has filled by then.
func TestAbstainDivergedTurnStillSigned(t *testing.T) {
	fa := newFakeAgent()
	fa.currentModel = "anthropic/claude-opus-4-5-20251001"
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	fa.promptHook = func(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		// A strict prefix of the sentinel — buffered, not forwarded —
		// followed by divergence. Nothing can be flushed until the
		// second chunk proves this is a real answer.
		for _, c := range []string{"<<SIL", "actually, here you go"} {
			fa.emit(sid, acp.SessionNotification{
				SessionId: sid,
				Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: c}}}},
			})
		}
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Meta: map[string]any{
				statusline.ExtensionID: map[string]any{"mood": "engaged", "plan": "4/7"},
			},
		})
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second, Ambient: true, SilentSentinel: "<<SILENT>>"})
	// Summon first so the thread is known, then send the ambient reply
	// that actually exercises the abstain sink.
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", BotUserID: "BBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "<@BBOT> hi"})
	waitForIdle(t, h)
	fs.mu.Lock()
	fs.bodies = nil
	fs.mu.Unlock()

	h.Handle(context.Background(), slackproto.Event{UserID: "U2", BotUserID: "BBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "2.0", Text: "chatter"})
	waitForIdle(t, h)

	final := lastBody(t, fs)
	if !strings.Contains(final, "actually, here you go") {
		t.Fatalf("diverged answer must be posted; got %q", final)
	}
	if !strings.HasSuffix(final, "\n\n_🏛️ opus-4.5 • engaged • 4/7_") {
		t.Fatalf("a released-at-Finalize answer must still be signed; got %q", final)
	}
}

// TestAbstainOffForAddressedMention is the regression test for the
// second half of the v0.4.1 channel bug: with Ambient enabled, an
// explicit @-mention was still routed through the abstain sink, so an
// agent that judged the (mention-stripped) text to be idle chatter
// could answer a direct summon with <<SILENT>> and post nothing.
// Addressed turns must bypass abstain entirely.
func TestAbstainOffForAddressedMention(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	done := make(chan struct{})
	fa.promptHook = func(ctx context.Context, sid acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
		// The agent emits exactly the silent sentinel. On an addressed
		// turn that must still be posted, not swallowed.
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "<<SILENT>>"}}}},
		})
		close(done)
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{
		Router:         r,
		API:            fs.client(),
		PromptTimeout:  5 * time.Second,
		Ambient:        true,
		SilentSentinel: "<<SILENT>>",
	})

	h.Handle(context.Background(), slackproto.Event{
		UserID:    "U1",
		BotUserID: "BBOT",
		ChannelID: "C1",
		ThreadTS:  "1.0",
		TS:        "1.0",
		Text:      "let's chat",
		IsMention: true,
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt never invoked for an addressed mention")
	}
	waitForIdle(t, h)
	if fs.posts == 0 {
		t.Fatal("an addressed @-mention must never be abstained away")
	}
}

// TestAbstainOffWhenNotAmbient verifies the addressed/DM path is
// unaffected by abstain: even if the agent emits the sentinel text,
// with Ambient=false it is posted normally (no suppression).
func TestAbstainOffWhenNotAmbient(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	done := make(chan struct{})
	fa.promptHook = func(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "<<SILENT>>"}}}},
		})
		close(done)
		return acp.StopReasonEndTurn, nil
	}

	// Ambient=false but SilentSentinel is still set (as main.go does):
	// abstain must NOT engage.
	h := New(Config{Router: r, API: fs.client(), PromptTimeout: 5 * time.Second, Ambient: false, SilentSentinel: "<<SILENT>>"})
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "hi"})
	<-done
	waitForIdle(t, h)

	fs.mu.Lock()
	body := strings.Join(fs.bodies, "")
	fs.mu.Unlock()
	if !strings.Contains(body, "<<SILENT>>") {
		t.Fatalf("addressed path must post sentinel text verbatim; got %q", body)
	}
}
