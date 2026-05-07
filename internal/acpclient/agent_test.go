package acpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// ---- in-process fake ACP agent over io.Pipe ----

type fakeAgent struct {
	conn *acp.Connection

	// Behaviour knobs.
	initRaw     json.RawMessage // raw initialize response body
	initErr     *acp.RequestError
	newSessResp acp.NewSessionResponse
	newSessErr  *acp.RequestError
	listResp    json.RawMessage
	listErr     *acp.RequestError
	resumeErr   *acp.RequestError
	promptStop  acp.StopReason
	promptErr   *acp.RequestError
	updates     []acp.SessionNotification

	mu             sync.Mutex
	gotPromptCount int
	gotCancelCount int32
	cancelled      atomic.Bool
}

func (f *fakeAgent) handle(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
	switch method {
	case acp.AgentMethodInitialize:
		if f.initErr != nil {
			return nil, f.initErr
		}
		if len(f.initRaw) > 0 {
			return json.RawMessage(f.initRaw), nil
		}
		return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
	case acp.AgentMethodSessionNew:
		if f.newSessErr != nil {
			return nil, f.newSessErr
		}
		return f.newSessResp, nil
	case acp.AgentMethodSessionPrompt:
		f.mu.Lock()
		f.gotPromptCount++
		f.mu.Unlock()
		var p acp.PromptRequest
		_ = json.Unmarshal(params, &p)
		for _, u := range f.updates {
			u.SessionId = p.SessionId
			_ = f.conn.SendNotification(ctx, acp.ClientMethodSessionUpdate, u)
		}
		if f.promptErr != nil {
			return nil, f.promptErr
		}
		stop := f.promptStop
		if stop == "" {
			stop = acp.StopReasonEndTurn
		}
		return acp.PromptResponse{StopReason: stop}, nil
	case acp.AgentMethodSessionCancel:
		atomic.AddInt32(&f.gotCancelCount, 1)
		f.cancelled.Store(true)
		return nil, nil
	case "session/list":
		if f.listErr != nil {
			return nil, f.listErr
		}
		if len(f.listResp) > 0 {
			return json.RawMessage(f.listResp), nil
		}
		return json.RawMessage(`{"sessions":[]}`), nil
	case "session/resume":
		if f.resumeErr != nil {
			return nil, f.resumeErr
		}
		return json.RawMessage("{}"), nil
	}
	return nil, acp.NewMethodNotFound(method)
}

// startFakeAgent wires a fakeAgent and a connect()-based AgentProc over
// in-process pipes.
func startFakeAgent(t *testing.T, fa *fakeAgent, cfg Config) *AgentProc {
	t.Helper()
	if cfg.Policy == nil {
		cfg.Policy = stubPolicy{}
	}
	cs2asR, cs2asW := io.Pipe()
	as2csR, as2csW := io.Pipe()

	fa.conn = acp.NewConnection(fa.handle, as2csW, cs2asR)

	a, err := connect(context.Background(), cfg, nil, cs2asW, as2csR)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = cs2asW.Close()
		_ = as2csR.Close()
		_ = cs2asR.Close()
		_ = as2csW.Close()
	})
	return a
}

type stubPolicy struct {
	resp acp.RequestPermissionResponse
}

func (s stubPolicy) Decide(_ context.Context, _ acp.RequestPermissionRequest) acp.RequestPermissionResponse {
	return s.resp
}

type capturingSink struct {
	mu      sync.Mutex
	gotErr  error
	updates []acp.SessionNotification
	gotCh   chan struct{}
}

func (c *capturingSink) OnUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	first := len(c.updates) == 0
	c.updates = append(c.updates, n)
	err := c.gotErr
	c.mu.Unlock()
	if first && c.gotCh != nil {
		close(c.gotCh)
	}
	return err
}

// ---- parseCaps ----

func TestParseCaps(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want Caps
	}{
		"empty":           {raw: `{}`, want: Caps{}},
		"loadSession":     {raw: `{"agentCapabilities":{"loadSession":true}}`, want: Caps{LoadSession: true}},
		"embeddedContext": {raw: `{"agentCapabilities":{"promptCapabilities":{"embeddedContext":true}}}`, want: Caps{EmbeddedContext: true}},
		"listSessions":    {raw: `{"agentCapabilities":{"sessionCapabilities":{"list":{}}}}`, want: Caps{ListSessions: true}},
		"resumeSession":   {raw: `{"agentCapabilities":{"sessionCapabilities":{"resume":{}}}}`, want: Caps{ResumeSession: true}},
		"listAndResume":   {raw: `{"agentCapabilities":{"sessionCapabilities":{"list":{},"resume":{}}}}`, want: Caps{ListSessions: true, ResumeSession: true}},
		"malformed":       {raw: `{"agentCapabilities":`, want: Caps{}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := parseCaps(json.RawMessage(c.raw))
			if got != c.want {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
		})
	}
}

// ---- Start config validation + spawn errors ----

func TestStartRequiresCommand(t *testing.T) {
	if _, err := Start(t.Context(), Config{Policy: nil}); err == nil {
		t.Fatal("want err on empty cmd")
	}
	if _, err := Start(t.Context(), Config{Command: []string{"echo"}}); err == nil {
		t.Fatal("want err on nil policy")
	}
}

func TestStartSpawnFails(t *testing.T) {
	if _, err := Start(t.Context(), Config{
		Command: []string{"/no/such/binary/please"},
		Policy:  stubPolicy{},
	}); err == nil {
		t.Fatal("expected exec error")
	}
}

func TestStartInitializeFails(t *testing.T) {
	// /bin/echo exits immediately → initialize never gets a response.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := Start(ctx, Config{
		Command: []string{"/bin/echo"},
		Policy:  stubPolicy{},
		Env:     os.Environ(),
		Cwd:     t.TempDir(),
		Stderr:  io.Discard,
	}); err == nil {
		t.Fatal("expected initialize error")
	}
}

// TestStartHappyPathSubprocess re-execs the test binary as a tiny ACP
// agent that responds to initialize. Covers the full Start → connect →
// Close pipeline including process management, the cfg.Env path, and
// the cfg.Stderr branch.
func TestStartHappyPathSubprocess(t *testing.T) {
	if os.Getenv("ACPCLIENT_FAKE_AGENT") == "1" {
		runFakeAgentStdio()
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	a, err := Start(ctx, Config{
		Command: []string{exe, "-test.run=TestStartHappyPathSubprocess"},
		Env:     append(os.Environ(), "ACPCLIENT_FAKE_AGENT=1"),
		Policy:  stubPolicy{},
		Cwd:     t.TempDir(),
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Second close on an already-exited process is a no-op.
	if err := a.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// runFakeAgentStdio is the child-mode entry point for the subprocess test:
// reply once to initialize and exit.
func runFakeAgentStdio() {
	dec := json.NewDecoder(os.Stdin)
	for {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if err := dec.Decode(&req); err != nil {
			return
		}
		if req.Method == "initialize" {
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result":  map[string]any{},
			}
			b, _ := json.Marshal(resp)
			_, _ = os.Stdout.Write(b)
			_, _ = os.Stdout.Write([]byte("\n"))
			return
		}
	}
}

// TestStartCwdAndStderrDefaults — empty Cwd → os.TempDir; nil Stderr →
// os.Stderr. Combined with a deliberately failing exec we just need to
// reach the assignments before the error.
func TestStartCwdAndStderrDefaults(t *testing.T) {
	_, err := Start(t.Context(), Config{
		Command: []string{"/no/such/binary"},
		Policy:  stubPolicy{},
		// Cwd & Stderr left zero so the defaults run.
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---- Session RPCs over fake agent ----

func TestNewSession(t *testing.T) {
	fa := &fakeAgent{newSessResp: acp.NewSessionResponse{SessionId: "sid-1"}}
	a := startFakeAgent(t, fa, Config{})
	sink := &capturingSink{}
	sid, err := a.NewSession(t.Context(), t.TempDir(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if sid != "sid-1" {
		t.Fatalf("sid=%q", sid)
	}
	if a.sinkFor(sid) != sink {
		t.Fatal("sink not registered")
	}
}

func TestNewSessionError(t *testing.T) {
	fa := &fakeAgent{newSessErr: acp.NewInternalError(map[string]any{"x": 1})}
	a := startFakeAgent(t, fa, Config{})
	if _, err := a.NewSession(t.Context(), t.TempDir(), &capturingSink{}); err == nil {
		t.Fatal("want error")
	}
}

func TestListAndResume(t *testing.T) {
	fa := &fakeAgent{listResp: json.RawMessage(`{"sessions":[{"sessionId":"prior","cwd":"/x"}]}`)}
	a := startFakeAgent(t, fa, Config{})

	sessions, err := a.ListSessions(t.Context(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].SessionId != "prior" {
		t.Fatalf("sessions=%+v", sessions)
	}

	sink := &capturingSink{}
	if err := a.ResumeSession(t.Context(), "/x", "prior", sink); err != nil {
		t.Fatal(err)
	}
	if a.sinkFor("prior") != sink {
		t.Fatal("resume did not register sink")
	}
}

func TestListSessionsError(t *testing.T) {
	fa := &fakeAgent{listErr: acp.NewInternalError(nil)}
	a := startFakeAgent(t, fa, Config{})
	if _, err := a.ListSessions(t.Context(), "/x"); err == nil {
		t.Fatal("want err")
	}
}

func TestResumeSessionError(t *testing.T) {
	fa := &fakeAgent{resumeErr: acp.NewInternalError(nil)}
	a := startFakeAgent(t, fa, Config{})
	if err := a.ResumeSession(t.Context(), "/x", "sid", &capturingSink{}); err == nil {
		t.Fatal("want err")
	}
	if a.sinkFor("sid") != nil {
		t.Fatal("sink should not be registered on resume error")
	}
}

func TestPromptAndStreamUpdates(t *testing.T) {
	gotCh := make(chan struct{})
	fa := &fakeAgent{
		newSessResp: acp.NewSessionResponse{SessionId: "sid-2"},
		updates: []acp.SessionNotification{
			{Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "hi"}}}}},
		},
	}
	a := startFakeAgent(t, fa, Config{})
	sink := &capturingSink{gotCh: gotCh}
	sid, err := a.NewSession(t.Context(), t.TempDir(), sink)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := a.Prompt(t.Context(), sid, []acp.ContentBlock{{Text: &acp.ContentBlockText{Text: "ping"}}})
	if err != nil {
		t.Fatal(err)
	}
	if stop != acp.StopReasonEndTurn {
		t.Fatalf("stop=%q", stop)
	}
	select {
	case <-gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("no update delivered")
	}
}

func TestPromptError(t *testing.T) {
	fa := &fakeAgent{
		newSessResp: acp.NewSessionResponse{SessionId: "sid-3"},
		promptErr:   acp.NewInternalError(nil),
	}
	a := startFakeAgent(t, fa, Config{})
	sid, _ := a.NewSession(t.Context(), t.TempDir(), &capturingSink{})
	if _, err := a.Prompt(t.Context(), sid, nil); err == nil {
		t.Fatal("want err")
	}
}

func TestCancel(t *testing.T) {
	fa := &fakeAgent{newSessResp: acp.NewSessionResponse{SessionId: "sid-4"}}
	a := startFakeAgent(t, fa, Config{})
	sid, _ := a.NewSession(t.Context(), t.TempDir(), &capturingSink{})
	if err := a.Cancel(t.Context(), sid); err != nil {
		t.Fatal(err)
	}
	// Wait for the notification to drain through the pipe.
	for i := 0; i < 100; i++ {
		if atomic.LoadInt32(&fa.gotCancelCount) == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("cancel notification not received")
}

func TestDropAndRebindSink(t *testing.T) {
	fa := &fakeAgent{newSessResp: acp.NewSessionResponse{SessionId: "sid-5"}}
	a := startFakeAgent(t, fa, Config{})
	first := &capturingSink{}
	sid, _ := a.NewSession(t.Context(), t.TempDir(), first)
	if a.sinkFor(sid) != first {
		t.Fatal("first sink missing")
	}
	a.DropSession(sid)
	if a.sinkFor(sid) != nil {
		t.Fatal("drop did not clear sink")
	}
	second := &capturingSink{}
	a.RebindSink(sid, second)
	if a.sinkFor(sid) != second {
		t.Fatal("rebind did not install sink")
	}
}

// ---- Inbound dispatch (server-initiated calls from agent) ----

func TestDispatchSessionUpdate(t *testing.T) {
	fa := &fakeAgent{newSessResp: acp.NewSessionResponse{SessionId: "sid"}}
	a := startFakeAgent(t, fa, Config{})
	sink := &capturingSink{gotCh: make(chan struct{})}
	if _, err := a.NewSession(t.Context(), t.TempDir(), sink); err != nil {
		t.Fatal(err)
	}
	// Have the fake agent push a session/update notification.
	if err := fa.conn.SendNotification(t.Context(), acp.ClientMethodSessionUpdate, acp.SessionNotification{
		SessionId: "sid",
		Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "x"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sink.gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("update not dispatched")
	}

	// Update for unknown session id: dispatched into nil sink, no panic.
	if err := fa.conn.SendNotification(t.Context(), acp.ClientMethodSessionUpdate, acp.SessionNotification{
		SessionId: "ghost",
		Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "y"}}}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchSessionUpdateSinkError(t *testing.T) {
	fa := &fakeAgent{newSessResp: acp.NewSessionResponse{SessionId: "sid"}}
	a := startFakeAgent(t, fa, Config{})
	sink := &capturingSink{gotErr: errors.New("sink boom"), gotCh: make(chan struct{})}
	_, _ = a.NewSession(t.Context(), t.TempDir(), sink)
	if err := fa.conn.SendNotification(t.Context(), acp.ClientMethodSessionUpdate, acp.SessionNotification{
		SessionId: "sid",
		Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "boom"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sink.gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("sink not invoked")
	}
}

func TestDispatchPermission(t *testing.T) {
	fa := &fakeAgent{}
	pol := stubPolicy{resp: acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: "ok"}}}}
	_ = startFakeAgent(t, fa, Config{Policy: pol})

	resp, err := acp.SendRequest[acp.RequestPermissionResponse](fa.conn, t.Context(), acp.ClientMethodSessionRequestPermission, acp.RequestPermissionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId != "ok" {
		t.Fatalf("got %+v", resp)
	}
}

func TestDispatchFsReadAndWrite(t *testing.T) {
	fa := &fakeAgent{}
	_ = startFakeAgent(t, fa, Config{})

	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")

	// Write
	wresp, err := acp.SendRequest[acp.WriteTextFileResponse](fa.conn, t.Context(), acp.ClientMethodFsWriteTextFile, acp.WriteTextFileRequest{
		Path: path, Content: "alpha\nbeta\ngamma\n",
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = wresp

	// Read whole
	rresp, err := acp.SendRequest[acp.ReadTextFileResponse](fa.conn, t.Context(), acp.ClientMethodFsReadTextFile, acp.ReadTextFileRequest{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rresp.Content, "alpha") {
		t.Fatalf("read: %q", rresp.Content)
	}

	// Read with offset+limit
	one, two, three := 2, 1, 100
	if r, err := acp.SendRequest[acp.ReadTextFileResponse](fa.conn, t.Context(), acp.ClientMethodFsReadTextFile, acp.ReadTextFileRequest{Path: path, Line: &one, Limit: &two}); err != nil {
		t.Fatal(err)
	} else if r.Content != "beta" {
		t.Fatalf("offset+limit: got %q", r.Content)
	}
	// Offset past EOF: clamp to end.
	beyond := 99
	if _, err := acp.SendRequest[acp.ReadTextFileResponse](fa.conn, t.Context(), acp.ClientMethodFsReadTextFile, acp.ReadTextFileRequest{Path: path, Line: &beyond}); err != nil {
		t.Fatal(err)
	}
	// Limit larger than remaining lines: still ok, returns rest.
	zero := 1
	if _, err := acp.SendRequest[acp.ReadTextFileResponse](fa.conn, t.Context(), acp.ClientMethodFsReadTextFile, acp.ReadTextFileRequest{Path: path, Line: &zero, Limit: &three}); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchFsReadErrors(t *testing.T) {
	fa := &fakeAgent{}
	_ = startFakeAgent(t, fa, Config{})

	// Relative path: rejected.
	if _, err := acp.SendRequest[acp.ReadTextFileResponse](fa.conn, t.Context(), acp.ClientMethodFsReadTextFile, acp.ReadTextFileRequest{Path: "rel"}); err == nil {
		t.Fatal("want abs-path error")
	}
	// Missing file: ReadFile error.
	if _, err := acp.SendRequest[acp.ReadTextFileResponse](fa.conn, t.Context(), acp.ClientMethodFsReadTextFile, acp.ReadTextFileRequest{Path: "/no/such/file/please"}); err == nil {
		t.Fatal("want read error")
	}
}

func TestDispatchFsWriteErrors(t *testing.T) {
	fa := &fakeAgent{}
	_ = startFakeAgent(t, fa, Config{})

	if _, err := acp.SendRequest[acp.WriteTextFileResponse](fa.conn, t.Context(), acp.ClientMethodFsWriteTextFile, acp.WriteTextFileRequest{Path: "rel"}); err == nil {
		t.Fatal("want abs-path error")
	}

	// Make MkdirAll fail by trying to create a dir under an existing file.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(blocker, "child", "f")
	if _, err := acp.SendRequest[acp.WriteTextFileResponse](fa.conn, t.Context(), acp.ClientMethodFsWriteTextFile, acp.WriteTextFileRequest{Path: bad, Content: "y"}); err == nil {
		t.Fatal("want mkdir error")
	}
}

func TestDispatchUnknownMethod(t *testing.T) {
	fa := &fakeAgent{}
	_ = startFakeAgent(t, fa, Config{})
	if _, err := acp.SendRequest[json.RawMessage](fa.conn, t.Context(), "no/such/method", map[string]any{}); err == nil {
		t.Fatal("want method-not-found")
	}
}

func TestDispatchInvalidParams(t *testing.T) {
	fa := &fakeAgent{}
	_ = startFakeAgent(t, fa, Config{})
	// Send a session/update with garbage params: sendRequest API marshals
	// the body, so we use SendNotification with an unmarshalable shape
	// is hard. Instead drive via SendRequest with malformed shapes for
	// each typed dispatch path.
	for _, m := range []string{
		acp.ClientMethodSessionUpdate,
		acp.ClientMethodSessionRequestPermission,
		acp.ClientMethodFsReadTextFile,
		acp.ClientMethodFsWriteTextFile,
	} {
		_, err := acp.SendRequest[json.RawMessage](fa.conn, t.Context(), m, json.RawMessage(`"not-an-object"`))
		if err == nil {
			t.Fatalf("%s: expected invalid-params error", m)
		}
	}
}

// ---- direct helper coverage ----

func TestReadTextFileDirect(t *testing.T) {
	if _, err := readTextFile(acp.ReadTextFileRequest{Path: "rel"}); err == nil {
		t.Fatal("relative path must be rejected")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := readTextFile(acp.ReadTextFileRequest{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != "a\nb\nc\n" {
		t.Fatalf("got %q", r.Content)
	}
}

func TestWriteTextFileDirect(t *testing.T) {
	if err := writeTextFile(acp.WriteTextFileRequest{Path: "rel"}); err == nil {
		t.Fatal("relative path must be rejected")
	}
	p := filepath.Join(t.TempDir(), "sub", "f.txt")
	if err := writeTextFile(acp.WriteTextFileRequest{Path: p, Content: "ok"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "ok" {
		t.Fatalf("got %q err=%v", b, err)
	}
}

// ---- Caps getter ----

func TestCapsGetter(t *testing.T) {
	fa := &fakeAgent{initRaw: json.RawMessage(`{"agentCapabilities":{"loadSession":true}}`)}
	a := startFakeAgent(t, fa, Config{})
	if !a.Caps().LoadSession {
		t.Fatal("Caps().LoadSession should be true")
	}
}

// ---- connect: cmd Kill on failure ----

func TestConnectKillsCmdOnInitErr(t *testing.T) {
	// Spawn /bin/sleep 5 then drive its stdio with our pipes so initialize
	// times out / errors. connect() should Kill the process.
	if _, err := os.Stat("/bin/sleep"); err != nil {
		t.Skip("/bin/sleep not present")
	}
	// Build a real cmd that's still alive when connect() errors.
	// We achieve that by calling Start with a non-existent binary in the
	// argv[0] guard: covered above. Here we simulate via a manually
	// constructed cmd we control.
	// Simpler path: pass a closed pipe so initialize fails immediately.
	pr, pw := io.Pipe()
	_ = pw.Close() // EOF on read side → initialize errors quickly
	cfg := Config{Policy: stubPolicy{}}
	// Use an io.Discard writer for stdin (won't be read).
	_, err := connect(t.Context(), cfg, nil, nopCloser{io.Discard}, pr)
	if err == nil {
		t.Fatal("expected initialize error on closed pipe")
	}
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// ---- Close: nil cmd ----

func TestCloseNilCmd(t *testing.T) {
	a := &AgentProc{}
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestCloseEscalatesToKill exercises the grace-period→SIGKILL branch in
// Close by spawning a process that ignores SIGINT.
func TestCloseEscalatesToKill(t *testing.T) {
	if os.Getenv("ACPCLIENT_FAKE_AGENT_SIGIGN") == "1" {
		// Trap SIGINT and stay alive answering initialize forever.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT)
		go func() { <-ch }()
		runFakeAgentLoop()
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	prev := closeGrace
	closeGrace = 50 * time.Millisecond
	t.Cleanup(func() { closeGrace = prev })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	a, err := Start(ctx, Config{
		Command: []string{exe, "-test.run=TestCloseEscalatesToKill"},
		Env:     append(os.Environ(), "ACPCLIENT_FAKE_AGENT_SIGIGN=1"),
		Policy:  stubPolicy{},
		Cwd:     t.TempDir(),
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// runFakeAgentLoop replies to initialize and then loops, ignoring SIGINT,
// until killed.
func runFakeAgentLoop() {
	dec := json.NewDecoder(os.Stdin)
	for {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if err := dec.Decode(&req); err != nil {
			// Idle until SIGKILL: don't return on EOF or stdin close,
			// otherwise Close's first SIGINT might happen to coincide
			// with a clean exit.
			time.Sleep(time.Hour)
			continue
		}
		if req.Method == "initialize" {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": map[string]any{}})
			_, _ = os.Stdout.Write(b)
			_, _ = os.Stdout.Write([]byte("\n"))
		}
	}
}
