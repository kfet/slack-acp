package router

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/client"
)

func newAgentCwdRouter(t *testing.T, fa *fakeAgent) *Router {
	t.Helper()
	r, err := New(Config{Agent: fa, StateDir: t.TempDir(), IdleTimeout: time.Minute, AgentCwd: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestConvKeyUserScope(t *testing.T) {
	k := ConvKey{ChannelID: "C1", ThreadTS: "100.0", UserID: "U1"}
	if k.String() != "C1/100.0/U1" {
		t.Fatalf("String = %q", k.String())
	}
	if k.Thread() != (ConvKey{ChannelID: "C1", ThreadTS: "100.0"}) {
		t.Fatalf("Thread = %+v", k.Thread())
	}
	for _, bad := range []ConvKey{{ChannelID: "", ThreadTS: "1"}, {ChannelID: "C", ThreadTS: ".."}, {ChannelID: "C", ThreadTS: "1", UserID: "a/b"}} {
		if bad.validate() == nil {
			t.Errorf("validate(%+v) = nil", bad)
		}
	}
}

func TestPerUserSessionsAreSeparate(t *testing.T) {
	fa := newFakeAgent()
	r := newRouter(t, fa)
	ctx := context.Background()
	thread := ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	a, err := r.GetOrCreate(ctx, ConvKey{ChannelID: "C1", ThreadTS: "1.0", UserID: "UA"}, stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.GetOrCreate(ctx, ConvKey{ChannelID: "C1", ThreadTS: "1.0", UserID: "UB"}, stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	if a.SessionID == b.SessionID || a.Cwd == b.Cwd {
		t.Fatalf("users share a session: %+v %+v", a, b)
	}
	if want := filepath.Join(r.StateDir(), "threads", "C1", "1.0", "users", "UA"); a.Cwd != want {
		t.Fatalf("cwd = %q, want %q", a.Cwd, want)
	}
	if !r.Known(a.Key) || !r.Known(thread) {
		t.Fatal("thread not known")
	}
	if err := r.SetLastTS(a.Key, "2.0"); err != nil {
		t.Fatal(err)
	}
	if r.GetLastTS(b.Key) != "2.0" || r.GetLastTS(thread) != "2.0" {
		t.Fatal("checkpoint is not thread-wide")
	}
	if got := SessionKeyForCwd(a.Cwd); got != "C1/1.0/UA" {
		t.Fatalf("SessionKeyForCwd = %q", got)
	}
	if err := r.SetOrigin(a.Key, Origin{ChannelID: "C0", ThreadTS: "0.1"}); err != nil {
		t.Fatal(err)
	}
	if o, ok := r.OriginOf(b.Key); !ok || o.ChannelID != "C0" {
		t.Fatalf("origin is not thread-wide: %+v %v", o, ok)
	}
}

func TestAgentCwdSendsPlaceholderAndResumesRecorded(t *testing.T) {
	fa := newFakeAgent()
	fa.caps = client.Caps{ListSessions: true, ResumeSession: true}
	r := newAgentCwdRouter(t, fa)
	ctx := context.Background()
	key := ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	s, err := r.GetOrCreate(ctx, key, stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	if fa.lastNewSessCwd != PlaceholderCwd {
		t.Fatalf("new cwd = %q", fa.lastNewSessCwd)
	}
	if fa.listCalls != 0 {
		t.Fatal("listed sessions under the shared placeholder")
	}
	if strings.HasPrefix(s.Cwd, PlaceholderCwd) {
		t.Fatal("relay dir must stay local")
	}
	// GC + reopen: resumes the recorded session at the placeholder.
	r.Reset(key)
	r.mu.Lock()
	delete(r.fresh, key)
	r.mu.Unlock()
	s2, err := r.GetOrCreate(ctx, key, stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	if s2.SessionID != s.SessionID || fa.lastResumeCwd != PlaceholderCwd || fa.lastResumeSID != s.SessionID {
		t.Fatalf("resume: sid=%s cwd=%q", s2.SessionID, fa.lastResumeCwd)
	}
	if SessionKeyForCwd(PlaceholderCwd) != "" {
		t.Fatal("placeholder maps to a conversation")
	}
}

func TestAgentCwdNoRecordStartsFresh(t *testing.T) {
	fa := newFakeAgent()
	fa.caps = client.Caps{ResumeSession: true}
	r := newAgentCwdRouter(t, fa)
	if _, err := r.GetOrCreate(context.Background(), ConvKey{ChannelID: "C1", ThreadTS: "1.0"}, stubSink{}); err != nil {
		t.Fatal(err)
	}
	if fa.resumeCalls != 0 || fa.newSessCalls != 1 {
		t.Fatalf("resume=%d new=%d", fa.resumeCalls, fa.newSessCalls)
	}
}

func TestAgentCwdRecordWriteFails(t *testing.T) {
	fa := newFakeAgent()
	r := newAgentCwdRouter(t, fa)
	key := ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	if err := os.MkdirAll(filepath.Join(r.StateDir(), key.relDir(), sessionFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetOrCreate(context.Background(), key, stubSink{}); err != nil {
		t.Fatalf("a lost record must not fail the turn: %v", err)
	}
}

func TestAgentCwdForkUsesPlaceholderAndRecordedOrigin(t *testing.T) {
	fa := newFakeAgent()
	fa.caps = client.Caps{ResumeSession: true}
	var forkCwd string
	var parent acp.SessionId
	fa.forkHook = func(cwd string, p acp.SessionId) (acp.SessionId, error) {
		forkCwd, parent = cwd, p
		return "child-sess", nil
	}
	r := newAgentCwdRouter(t, fa)
	ctx := context.Background()
	origin := ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	s, err := r.GetOrCreate(ctx, origin, stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	r.Reset(origin) // not live: the fork must find it from the record
	child := ConvKey{ChannelID: "C1", ThreadTS: "2.0"}
	if _, err := r.Fork(ctx, origin, child, ""); err != nil {
		t.Fatal(err)
	}
	if forkCwd != PlaceholderCwd || parent != s.SessionID {
		t.Fatalf("fork cwd=%q parent=%s", forkCwd, parent)
	}
	if sid, err := r.storedSession(ctx, child, ""); err != nil || sid != "child-sess" {
		t.Fatalf("child record = %s, %v", sid, err)
	}
	// No record → no session to fork.
	if _, err := r.Fork(ctx, ConvKey{ChannelID: "C1", ThreadTS: "3.0"}, ConvKey{ChannelID: "C1", ThreadTS: "4.0"}, ""); err == nil || !strings.Contains(err.Error(), "no session to fork") {
		t.Fatalf("err = %v", err)
	}
}

func TestOriginSessionListErrorAndInvalid(t *testing.T) {
	fa := newFakeAgent()
	fa.caps = client.Caps{ListSessions: true}
	fa.listErr = errors.New("boom")
	r := newRouter(t, fa)
	if _, err := r.originSession(context.Background(), ConvKey{ChannelID: "C1", ThreadTS: "1.0"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if _, err := r.originSession(context.Background(), ConvKey{ChannelID: "C1", ThreadTS: "1.0", UserID: ".x"}); err == nil {
		t.Fatal("invalid origin accepted")
	}
}
