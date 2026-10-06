package router

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/client"
)

func TestOriginRoundTrip(t *testing.T) {
	r, _ := newTestRouter(t)
	child := ConvKey{ChannelID: "C1", ThreadTS: "2.0"}
	if _, ok := r.OriginOf(child); ok {
		t.Fatal("no origin recorded yet")
	}
	o := Origin{ChannelID: "C1", ThreadTS: "1.0", BranchTS: "1.5"}
	if err := r.SetOrigin(child, o); err != nil {
		t.Fatal(err)
	}
	got, ok := r.OriginOf(child)
	if !ok || got != o {
		t.Fatalf("OriginOf = %+v, %v", got, ok)
	}
	if got.Key() != (ConvKey{ChannelID: "C1", ThreadTS: "1.0"}) {
		t.Fatalf("Key = %v", got.Key())
	}
	if !r.Known(child) {
		t.Fatal("a branched thread must be Known so ambient replies reach it")
	}
}

func TestOriginRejectsBadInput(t *testing.T) {
	r, _ := newTestRouter(t)
	if err := r.SetOrigin(ConvKey{ChannelID: "..", ThreadTS: "1"}, Origin{}); err == nil {
		t.Fatal("SetOrigin must validate the key")
	}
	if _, ok := r.OriginOf(ConvKey{ChannelID: "C1", ThreadTS: "../x"}); ok {
		t.Fatal("OriginOf must validate the key")
	}
	child := ConvKey{ChannelID: "C1", ThreadTS: "3.0"}
	if err := r.SetOrigin(child, Origin{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.OriginOf(child); ok {
		t.Fatal("an origin with no thread must not count")
	}
	if err := r.root.WriteFile(filepath.Join("threads", "C1", "3.0", originFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.OriginOf(child); ok {
		t.Fatal("corrupt origin.json must not count")
	}
	_ = r.Close()
	if _, ok := r.OriginOf(child); ok {
		t.Fatal("closed router has no origins")
	}
}

func TestForkFromLiveOrigin(t *testing.T) {
	r, fa := newTestRouter(t)
	origin := ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	child := ConvKey{ChannelID: "C1", ThreadTS: "2.0"}
	s, err := r.GetOrCreate(context.Background(), origin, discardSink{})
	if err != nil {
		t.Fatal(err)
	}
	var gotCwd string
	var gotParent acp.SessionId
	fa.forkHook = func(cwd string, parent acp.SessionId) (acp.SessionId, error) {
		gotCwd, gotParent = cwd, parent
		return "child-sid", nil
	}
	sid, err := r.Fork(context.Background(), origin, child, "")
	if err != nil || sid != "child-sid" {
		t.Fatalf("Fork = %q, %v", sid, err)
	}
	if gotParent != s.SessionID {
		t.Fatalf("forked %q, want origin's %q", gotParent, s.SessionID)
	}
	if want := filepath.Join(r.StateDir(), "threads", "C1", "2.0"); gotCwd != want {
		t.Fatalf("fork cwd %q, want the CHILD's %q", gotCwd, want)
	}
	if live, _, ok := r.Live(child); !ok || live != "child-sid" {
		t.Fatalf("child live = %q, %v", live, ok)
	}
	cs, err := r.GetOrCreate(context.Background(), child, discardSink{})
	if err != nil || cs.SessionID != "child-sid" {
		t.Fatalf("child turn must use the fork, got %v %v", cs, err)
	}
	if r.TakePendingSystemPrompt(cs) != "" {
		t.Fatal("a fork carries the origin's system prompt; nothing to inline")
	}
	if _, err := r.Fork(context.Background(), origin, child, ""); err == nil {
		t.Fatal("forking into a child that has a session must fail")
	}
}

func TestForkFromListedOrigin(t *testing.T) {
	r, fa := newTestRouter(t)
	fa.caps = client.Caps{ListSessions: true}
	fa.listResult = []client.SessionInfo{{SessionId: "newest"}, {SessionId: "older"}}
	var gotParent acp.SessionId
	fa.forkHook = func(_ string, parent acp.SessionId) (acp.SessionId, error) {
		gotParent = parent
		return "c", nil
	}
	origin := ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	if _, err := r.Fork(context.Background(), origin, ConvKey{ChannelID: "C1", ThreadTS: "2.0"}, ""); err != nil {
		t.Fatal(err)
	}
	if gotParent != "newest" {
		t.Fatalf("parent %q, want newest", gotParent)
	}
	if want := filepath.Join(r.StateDir(), "threads", "C1", "1.0"); fa.lastListCwd != want {
		t.Fatalf("listed %q, want %q", fa.lastListCwd, want)
	}
}

func TestForkFailures(t *testing.T) {
	ctx := context.Background()
	origin := ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	child := ConvKey{ChannelID: "C1", ThreadTS: "2.0"}
	cases := []struct {
		name  string
		setup func(r *Router, fa *fakeAgent)
		org   ConvKey
		child ConvKey
		want  string
	}{
		{"no list caps", func(*Router, *fakeAgent) {}, origin, child, "cannot list"},
		{"bad origin", func(_ *Router, fa *fakeAgent) { fa.caps.ListSessions = true }, ConvKey{ChannelID: "..", ThreadTS: "1"}, child, "invalid origin"},
		{"list error", func(_ *Router, fa *fakeAgent) {
			fa.caps.ListSessions = true
			fa.listErr = errors.New("boom")
		}, origin, child, "boom"},
		{"empty list", func(_ *Router, fa *fakeAgent) { fa.caps.ListSessions = true }, origin, child, "no session"},
		{"bad child", func(r *Router, _ *fakeAgent) {
			_, _ = r.GetOrCreate(ctx, origin, discardSink{})
		}, origin, ConvKey{ChannelID: "C1", ThreadTS: ".x"}, "thread ts"},
		{"unsupported", func(r *Router, fa *fakeAgent) {
			_, _ = r.GetOrCreate(ctx, origin, discardSink{})
			fa.forkHook = func(string, acp.SessionId) (acp.SessionId, error) { return "", client.ErrForkUnsupported }
		}, origin, child, "not supported"},
		{"raced", func(r *Router, fa *fakeAgent) {
			_, _ = r.GetOrCreate(ctx, origin, discardSink{})
			fa.forkHook = func(string, acp.SessionId) (acp.SessionId, error) {
				fa.forkHook = nil
				_, _ = r.GetOrCreate(ctx, child, discardSink{})
				return "late", nil
			}
		}, origin, child, "while forking"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, fa := newTestRouter(t)
			tc.setup(r, fa)
			_, err := r.Fork(ctx, tc.org, tc.child, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func newTestRouter(t *testing.T) (*Router, *fakeAgent) {
	t.Helper()
	fa := newFakeAgent()
	r, err := New(Config{Agent: fa, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, fa
}

func TestDiscardSinkDrops(t *testing.T) {
	if err := (discardSink{}).OnUpdate(context.Background(), acp.SessionNotification{}); err != nil {
		t.Fatal(err)
	}
}
