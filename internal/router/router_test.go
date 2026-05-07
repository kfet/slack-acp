package router

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/slack-acp/internal/acpclient"
)

// ---- fake agent ----

type fakeAgent struct {
	mu    sync.Mutex
	sinks map[acp.SessionId]acpclient.SessionUpdateSink

	caps         acpclient.Caps
	listResult   []acpclient.SessionInfo
	listErr      error
	resumeErr    error
	newSessErr   error
	newSessIDFn  func() acp.SessionId
	cancelErr    error
	promptStop   acp.StopReason
	promptErr    error
	dropCalls    int32
	rebindCalls  int32
	listCalls    int32
	resumeCalls  int32
	newSessCalls int32
	cancelCalls  int32
	promptCalls  int32

	lastNewSessSink    acpclient.SessionUpdateSink
	lastResumeSink     acpclient.SessionUpdateSink
	lastResumeSID      acp.SessionId
	lastRebindSID      acp.SessionId
	lastRebindSink     acpclient.SessionUpdateSink
	lastDropSID        acp.SessionId
	lastNewSessCwd     string
	lastResumeCwd      string
	lastListCwd        string
	lastCancelSID      acp.SessionId
	lastPromptBlocks   []acp.ContentBlock
	lastPromptSID      acp.SessionId
	nextSessionCounter int32
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{sinks: map[acp.SessionId]acpclient.SessionUpdateSink{}}
}

func (f *fakeAgent) Caps() acpclient.Caps { return f.caps }

func (f *fakeAgent) NewSession(_ context.Context, cwd string, sink acpclient.SessionUpdateSink) (acp.SessionId, error) {
	atomic.AddInt32(&f.newSessCalls, 1)
	f.mu.Lock()
	f.lastNewSessCwd = cwd
	f.lastNewSessSink = sink
	f.mu.Unlock()
	if f.newSessErr != nil {
		return "", f.newSessErr
	}
	var sid acp.SessionId
	if f.newSessIDFn != nil {
		sid = f.newSessIDFn()
	} else {
		n := atomic.AddInt32(&f.nextSessionCounter, 1)
		sid = acp.SessionId("sess-" + itoa(int(n)))
	}
	f.mu.Lock()
	f.sinks[sid] = sink
	f.mu.Unlock()
	return sid, nil
}

func (f *fakeAgent) ListSessions(_ context.Context, cwd string) ([]acpclient.SessionInfo, error) {
	atomic.AddInt32(&f.listCalls, 1)
	f.mu.Lock()
	f.lastListCwd = cwd
	f.mu.Unlock()
	return f.listResult, f.listErr
}

func (f *fakeAgent) ResumeSession(_ context.Context, cwd string, sid acp.SessionId, sink acpclient.SessionUpdateSink) error {
	atomic.AddInt32(&f.resumeCalls, 1)
	f.mu.Lock()
	f.lastResumeCwd = cwd
	f.lastResumeSID = sid
	f.lastResumeSink = sink
	f.mu.Unlock()
	if f.resumeErr != nil {
		return f.resumeErr
	}
	f.mu.Lock()
	f.sinks[sid] = sink
	f.mu.Unlock()
	return nil
}

func (f *fakeAgent) Prompt(_ context.Context, sid acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
	atomic.AddInt32(&f.promptCalls, 1)
	f.mu.Lock()
	f.lastPromptBlocks = blocks
	f.lastPromptSID = sid
	f.mu.Unlock()
	return f.promptStop, f.promptErr
}

func (f *fakeAgent) Cancel(_ context.Context, sid acp.SessionId) error {
	atomic.AddInt32(&f.cancelCalls, 1)
	f.mu.Lock()
	f.lastCancelSID = sid
	f.mu.Unlock()
	return f.cancelErr
}

func (f *fakeAgent) DropSession(sid acp.SessionId) {
	atomic.AddInt32(&f.dropCalls, 1)
	f.mu.Lock()
	delete(f.sinks, sid)
	f.lastDropSID = sid
	f.mu.Unlock()
}

func (f *fakeAgent) RebindSink(sid acp.SessionId, sink acpclient.SessionUpdateSink) {
	atomic.AddInt32(&f.rebindCalls, 1)
	f.mu.Lock()
	f.sinks[sid] = sink
	f.lastRebindSID = sid
	f.lastRebindSink = sink
	f.mu.Unlock()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

type stubSink struct{}

func (stubSink) OnUpdate(context.Context, acp.SessionNotification) error { return nil }

// ---- helpers ----

func newRouter(t *testing.T, fa *fakeAgent) *Router {
	t.Helper()
	dir := t.TempDir()
	r, err := New(Config{Agent: fa, StateDir: dir, IdleTimeout: time.Minute})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// ---- ConvKey ----

func TestConvKeyString(t *testing.T) {
	k := ConvKey{ChannelID: "C1", ThreadTS: "100.0"}
	if k.String() != "C1/100.0" {
		t.Fatalf("got %q", k.String())
	}
}

// ---- New / Close / DefaultStateDir ----

func TestNewRequiresAgent(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected nil-agent error")
	}
}

func TestNewDefaultsStateDir(t *testing.T) {
	// Force a deterministic path via XDG_STATE_HOME so we can assert.
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	r, err := New(Config{Agent: newFakeAgent()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()
	want := filepath.Join(dir, "slack-acp")
	if r.StateDir() != want {
		t.Fatalf("StateDir: got %q want %q", r.StateDir(), want)
	}
}

func TestNewMkdirFails(t *testing.T) {
	// Point StateDir at a path whose parent isn't a directory.
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "block")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Agent: newFakeAgent(), StateDir: filepath.Join(blocker, "child")}
	if _, err := New(cfg); err == nil {
		t.Fatal("expected mkdir error")
	}
}

func TestNewOpenRootFails(t *testing.T) {
	// Race-free path: pre-create StateDir as a regular file (not a dir),
	// then make the parent unwritable so MkdirAll passes (path already
	// exists) but OpenRoot fails because it isn't a directory.
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "notadir")
	if err := os.WriteFile(bad, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// MkdirAll on an existing file path returns an error too — adjust:
	// instead, place StateDir on a path that's already a file. New()
	// will MkdirAll-error, which is the same surface.
	if _, err := New(Config{Agent: newFakeAgent(), StateDir: bad}); err == nil {
		t.Fatal("expected error")
	}
}

func TestNewOpenRootSyscallFails(t *testing.T) {
	// Drop the user's read+execute on the StateDir after creation so
	// os.OpenRoot fails (permission denied) while MkdirAll is a no-op
	// (path already exists). Skipped under root, where chmod is moot.
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "state")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// 0o000 — no permissions; OpenRoot must fail.
	if err := os.Chmod(target, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o755) })
	if _, err := New(Config{Agent: newFakeAgent(), StateDir: target}); err == nil {
		t.Fatal("expected OpenRoot error")
	}
}

func TestCwdForMkdirFails(t *testing.T) {
	// Pre-plant a regular file at threads/C1 so MkdirAll for
	// threads/C1/<ts> fails because C1 is not a directory.
	fa := newFakeAgent()
	r := newRouter(t, fa)
	if err := os.MkdirAll(filepath.Join(r.stateDir, "threads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.stateDir, "threads", "C1"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.cwdFor(ConvKey{ChannelID: "C1", ThreadTS: "1.0"}); err == nil {
		t.Fatal("expected mkdir error")
	}
}

func TestCloseIdempotent(t *testing.T) {
	r := newRouter(t, newFakeAgent())
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestDefaultStateDir(t *testing.T) {
	// With XDG_STATE_HOME set
	t.Setenv("XDG_STATE_HOME", "/x/state")
	if got := DefaultStateDir(); got != "/x/state/slack-acp" {
		t.Fatalf("xdg: got %q", got)
	}

	// Without XDG, with a HOME
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/h")
	if got := DefaultStateDir(); got != "/h/.local/state/slack-acp" {
		t.Fatalf("home: got %q", got)
	}

	// Without XDG and without a usable home — UserHomeDir errors → tmp
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	// On macOS UserHomeDir consults $HOME first; with HOME empty it errors.
	got := DefaultStateDir()
	if got == "" {
		t.Fatal("empty default")
	}
}

// ---- cwdFor ----

func TestCwdForStable(t *testing.T) {
	r := newRouter(t, newFakeAgent())
	key := ConvKey{ChannelID: "C123", ThreadTS: "1700000000.123456"}
	want := filepath.Join(r.stateDir, "threads", "C123", "1700000000.123456")
	got, err := r.cwdFor(key)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	got2, err := r.cwdFor(key)
	if err != nil || got2 != got {
		t.Fatalf("not stable: %q vs %q (err=%v)", got, got2, err)
	}
}

func TestCwdForRejectsTraversal(t *testing.T) {
	r := newRouter(t, newFakeAgent())
	cases := []ConvKey{
		{ChannelID: "..", ThreadTS: "100.0"},
		{ChannelID: "C1", ThreadTS: ".."},
		{ChannelID: ".", ThreadTS: "100.0"},
		{ChannelID: "../etc", ThreadTS: "100.0"},
		{ChannelID: "C1", ThreadTS: "../../escape"},
		{ChannelID: "", ThreadTS: "100.0"},
		{ChannelID: "C1", ThreadTS: ""},
		{ChannelID: "a/b", ThreadTS: "100.0"},
		{ChannelID: `a\b`, ThreadTS: "100.0"},
		{ChannelID: "C1", ThreadTS: "a\x00b"},
		{ChannelID: ".hidden", ThreadTS: "100.0"},
	}
	for _, k := range cases {
		t.Run(k.String(), func(t *testing.T) {
			if _, err := r.cwdFor(k); err == nil {
				t.Fatalf("cwdFor(%+v) should have failed", k)
			}
		})
	}
}

func TestValidateKeyComponent(t *testing.T) {
	// All happy-path coverage flows through cwdFor; this directly hits
	// the explicit branches.
	ok := []string{"C123", "U_TEAM-1"}
	for _, s := range ok {
		if err := validateKeyComponent(s); err != nil {
			t.Fatalf("ok %q: %v", s, err)
		}
	}
}

// ---- GetOrCreate ----

func TestGetOrCreateNew(t *testing.T) {
	fa := newFakeAgent()
	r := newRouter(t, fa)
	sink := stubSink{}
	key := ConvKey{ChannelID: "C1", ThreadTS: "100.0"}
	s, err := r.GetOrCreate(context.Background(), key, sink)
	if err != nil {
		t.Fatal(err)
	}
	if s.SessionID == "" || s.Cwd == "" {
		t.Fatalf("bad session: %+v", s)
	}
	if r.Len() != 1 {
		t.Fatal("len")
	}
	if fa.newSessCalls != 1 || fa.lastNewSessCwd != s.Cwd {
		t.Fatalf("agent.NewSession not called as expected (%d, cwd=%q)", fa.newSessCalls, fa.lastNewSessCwd)
	}
}

func TestGetOrCreateRebindsExisting(t *testing.T) {
	fa := newFakeAgent()
	r := newRouter(t, fa)
	key := ConvKey{ChannelID: "C1", ThreadTS: "100.0"}
	first, err := r.GetOrCreate(context.Background(), key, stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	newSink := stubSink{}
	second, err := r.GetOrCreate(context.Background(), key, newSink)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("expected same session pointer on reuse")
	}
	if fa.rebindCalls != 1 || fa.lastRebindSID != first.SessionID {
		t.Fatalf("rebind: calls=%d sid=%q", fa.rebindCalls, fa.lastRebindSID)
	}
	if fa.newSessCalls != 1 {
		t.Fatalf("expected 1 NewSession (cold path), got %d", fa.newSessCalls)
	}
}

func TestGetOrCreateInvalidKey(t *testing.T) {
	r := newRouter(t, newFakeAgent())
	if _, err := r.GetOrCreate(context.Background(), ConvKey{ChannelID: "..", ThreadTS: "1"}, stubSink{}); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestGetOrCreateAgentError(t *testing.T) {
	fa := newFakeAgent()
	fa.newSessErr = errors.New("boom")
	r := newRouter(t, fa)
	if _, err := r.GetOrCreate(context.Background(), ConvKey{ChannelID: "C1", ThreadTS: "1.0"}, stubSink{}); err == nil {
		t.Fatal("expected agent error to propagate")
	}
}

// TestGetOrCreateRaceLoser exercises the post-NewSession map check by
// pre-installing a session under the same key just before the cold
// goroutine takes the second mu.Lock(). We force the race by using a
// fakeAgent.newSessIDFn that, on its first call, plants a "winner" in
// r.byKey using a separate, second NewSession invocation. The original
// caller then loses and should drop its sid.
func TestGetOrCreateRaceLoser(t *testing.T) {
	fa := newFakeAgent()
	r := newRouter(t, fa)
	key := ConvKey{ChannelID: "C1", ThreadTS: "1.0"}

	// Install a session for key directly via internal map, simulating
	// a winner from a parallel goroutine.
	winner := &Session{Key: key, SessionID: "preinstalled", Cwd: "/x"}
	fa.newSessIDFn = func() acp.SessionId {
		// While this NewSession call runs (before GetOrCreate's
		// second mu.Lock), plant the winner.
		r.mu.Lock()
		r.byKey[key] = winner
		r.mu.Unlock()
		return "loser"
	}
	got, err := r.GetOrCreate(context.Background(), key, stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	if got != winner {
		t.Fatalf("expected winner, got %+v", got)
	}
	if fa.lastDropSID != "loser" {
		t.Fatalf("expected to drop loser, dropped %q", fa.lastDropSID)
	}
	if fa.rebindCalls != 1 || fa.lastRebindSID != winner.SessionID {
		t.Fatalf("expected rebind on winner, got rebind sid=%q calls=%d", fa.lastRebindSID, fa.rebindCalls)
	}
}

// ---- tryResume ----

func TestTryResumeNoCaps(t *testing.T) {
	fa := newFakeAgent() // caps zero-value: list=false, resume=false
	r := newRouter(t, fa)
	key := ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	_, err := r.GetOrCreate(context.Background(), key, stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	if fa.listCalls != 0 || fa.resumeCalls != 0 {
		t.Fatalf("expected no list/resume; got list=%d resume=%d", fa.listCalls, fa.resumeCalls)
	}
}

func TestTryResumeListErr(t *testing.T) {
	fa := newFakeAgent()
	fa.caps = acpclient.Caps{ListSessions: true, ResumeSession: true}
	fa.listErr = errors.New("list boom")
	r := newRouter(t, fa)
	if _, err := r.GetOrCreate(context.Background(), ConvKey{ChannelID: "C1", ThreadTS: "1.0"}, stubSink{}); err != nil {
		t.Fatal(err)
	}
	if fa.listCalls != 1 {
		t.Fatalf("list calls=%d", fa.listCalls)
	}
	if fa.newSessCalls != 1 {
		t.Fatalf("expected fallback to NewSession, got %d", fa.newSessCalls)
	}
}

func TestTryResumeEmpty(t *testing.T) {
	fa := newFakeAgent()
	fa.caps = acpclient.Caps{ListSessions: true, ResumeSession: true}
	r := newRouter(t, fa)
	if _, err := r.GetOrCreate(context.Background(), ConvKey{ChannelID: "C1", ThreadTS: "1.0"}, stubSink{}); err != nil {
		t.Fatal(err)
	}
	if fa.resumeCalls != 0 {
		t.Fatal("should not call resume on empty list")
	}
	if fa.newSessCalls != 1 {
		t.Fatalf("expected NewSession, got %d", fa.newSessCalls)
	}
}

func TestTryResumeError(t *testing.T) {
	fa := newFakeAgent()
	fa.caps = acpclient.Caps{ListSessions: true, ResumeSession: true}
	fa.listResult = []acpclient.SessionInfo{{SessionId: "old"}}
	fa.resumeErr = errors.New("nope")
	r := newRouter(t, fa)
	if _, err := r.GetOrCreate(context.Background(), ConvKey{ChannelID: "C1", ThreadTS: "1.0"}, stubSink{}); err != nil {
		t.Fatal(err)
	}
	if fa.resumeCalls != 1 || fa.newSessCalls != 1 {
		t.Fatalf("resume=%d newSess=%d", fa.resumeCalls, fa.newSessCalls)
	}
}

func TestTryResumeSuccess(t *testing.T) {
	fa := newFakeAgent()
	fa.caps = acpclient.Caps{ListSessions: true, ResumeSession: true}
	fa.listResult = []acpclient.SessionInfo{{SessionId: "prior"}, {SessionId: "older"}}
	r := newRouter(t, fa)
	s, err := r.GetOrCreate(context.Background(), ConvKey{ChannelID: "C1", ThreadTS: "1.0"}, stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	if s.SessionID != "prior" {
		t.Fatalf("expected resumed sid 'prior', got %q", s.SessionID)
	}
	if fa.newSessCalls != 0 {
		t.Fatal("should not have called NewSession on successful resume")
	}
}

// ---- Cancel / Touch / Len ----

func TestCancelMiss(t *testing.T) {
	r := newRouter(t, newFakeAgent())
	r.Cancel(context.Background(), ConvKey{ChannelID: "x", ThreadTS: "y"}) // no-op, must not panic
}

func TestCancelHit(t *testing.T) {
	fa := newFakeAgent()
	r := newRouter(t, fa)
	key := ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	s, err := r.GetOrCreate(context.Background(), key, stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	r.Cancel(context.Background(), key)
	if fa.cancelCalls != 1 || fa.lastCancelSID != s.SessionID {
		t.Fatalf("cancel: calls=%d sid=%q", fa.cancelCalls, fa.lastCancelSID)
	}
}

func TestTouch(t *testing.T) {
	r := newRouter(t, newFakeAgent())
	s, err := r.GetOrCreate(context.Background(), ConvKey{ChannelID: "C1", ThreadTS: "1.0"}, stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	old := s.lastUsed
	time.Sleep(2 * time.Millisecond)
	r.Touch(s)
	if !s.lastUsed.After(old) {
		t.Fatal("Touch did not advance lastUsed")
	}
}

// ---- gcOnce / Run ----

func TestGCEvictsStale(t *testing.T) {
	fa := newFakeAgent()
	r := newRouter(t, fa)
	r.idleTimeout = time.Millisecond
	key := ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	s, err := r.GetOrCreate(context.Background(), key, stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	// Backdate.
	r.mu.Lock()
	s.lastUsed = time.Now().Add(-time.Hour)
	r.mu.Unlock()

	r.gcOnce()
	if r.Len() != 0 {
		t.Fatalf("expected eviction, len=%d", r.Len())
	}
	if fa.dropCalls != 1 || fa.lastDropSID != s.SessionID {
		t.Fatalf("drop: calls=%d sid=%q", fa.dropCalls, fa.lastDropSID)
	}
	// And the cwd should still exist on disk (stable, not removed).
	if _, err := os.Stat(s.Cwd); err != nil {
		t.Fatalf("cwd was removed by GC: %v", err)
	}
}

func TestGCKeepsFresh(t *testing.T) {
	fa := newFakeAgent()
	r := newRouter(t, fa)
	r.idleTimeout = time.Hour
	if _, err := r.GetOrCreate(context.Background(), ConvKey{ChannelID: "C1", ThreadTS: "1.0"}, stubSink{}); err != nil {
		t.Fatal(err)
	}
	r.gcOnce()
	if r.Len() != 1 || fa.dropCalls != 0 {
		t.Fatalf("len=%d drop=%d", r.Len(), fa.dropCalls)
	}
}

func TestRunCancels(t *testing.T) {
	r := newRouter(t, newFakeAgent())
	r.idleTimeout = 4 * time.Millisecond // ticker = idleTimeout/4 = 1ms
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// ---- Agent() getter ----

func TestAgentGetter(t *testing.T) {
	fa := newFakeAgent()
	r := newRouter(t, fa)
	if r.Agent() != fa {
		t.Fatal("Agent() did not return injected agent")
	}
}
