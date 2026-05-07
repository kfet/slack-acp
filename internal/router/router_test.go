package router

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConvKeyString(t *testing.T) {
	k := ConvKey{ChannelID: "C1", ThreadTS: "100.0"}
	if k.String() != "C1/100.0" {
		t.Fatalf("got %q", k.String())
	}
}

// newTestRouter builds a Router rooted at t.TempDir() without going
// through New() (which requires a non-nil agent). The tests here only
// exercise cwdFor, which doesn't touch the agent.
func newTestRouter(t *testing.T) *Router {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	rt := &Router{stateDir: dir, root: root, byKey: map[ConvKey]*Session{}}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// TestCwdForStable pins the per-thread cwd layout and verifies repeat
// calls return the same path (no tempdir randomness).
func TestCwdForStable(t *testing.T) {
	r := newTestRouter(t)
	key := ConvKey{ChannelID: "C123", ThreadTS: "1700000000.123456"}
	want := filepath.Join(r.stateDir, "threads", "C123", "1700000000.123456")
	got, err := r.cwdFor(key)
	if err != nil {
		t.Fatalf("cwdFor: %v", err)
	}
	if got != want {
		t.Fatalf("cwdFor: got %q want %q", got, want)
	}
	got2, err := r.cwdFor(key)
	if err != nil || got2 != got {
		t.Fatalf("cwdFor not stable: %q vs %q (err=%v)", got, got2, err)
	}
}

// TestCwdForRejectsTraversal exercises the os.Root sandbox: any
// component that tries to escape the StateDir must be refused.
func TestCwdForRejectsTraversal(t *testing.T) {
	r := newTestRouter(t)
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

func TestDefaultStateDirNonEmpty(t *testing.T) {
	if DefaultStateDir() == "" {
		t.Fatal("DefaultStateDir returned empty string")
	}
}
