// Package router maps a Slack thread (channel + thread_ts) to an ACP
// session inside a shared agent process. It owns session lifecycle: cwd
// allocation, idle GC, and cancel propagation.
//
// Per-thread cwd is a STABLE path under StateDir
// (<StateDir>/threads/<channel>/<thread_ts>), not a tempdir. Idle GC
// drops the in-memory agent session but leaves the directory on disk so
// agent state (e.g. .fir/) persists for future resumption or operator
// inspection. This mirrors the approach in sibling project poe-acp.
package router

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/slack-acp/internal/acpclient"
	"github.com/kfet/slack-acp/internal/debuglog"
)

// ConvKey identifies a conversation: a Slack thread.
type ConvKey struct {
	ChannelID string
	ThreadTS  string
}

func (k ConvKey) String() string { return k.ChannelID + "/" + k.ThreadTS }

// Session holds router-side state for one ACP session.
type Session struct {
	Key       ConvKey
	SessionID acp.SessionId
	Cwd       string
	// LastUsed is updated each Prompt/Update; used by GC.
	lastUsed time.Time
	// Mu serialises access to a session's prompt pipeline. ACP allows one
	// outstanding prompt per session at a time.
	Mu sync.Mutex
}

// Agent is the subset of *acpclient.AgentProc the router depends on.
// Exposed as an interface so tests can substitute a fake. The real
// *acpclient.AgentProc satisfies it.
type Agent interface {
	Caps() acpclient.Caps
	NewSession(ctx context.Context, cwd string, sink acpclient.SessionUpdateSink) (acp.SessionId, error)
	ListSessions(ctx context.Context, cwd string) ([]acpclient.SessionInfo, error)
	ResumeSession(ctx context.Context, cwd string, sid acp.SessionId, sink acpclient.SessionUpdateSink) error
	Prompt(ctx context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (acp.StopReason, error)
	Cancel(ctx context.Context, sid acp.SessionId) error
	DropSession(sid acp.SessionId)
	RebindSink(sid acp.SessionId, sink acpclient.SessionUpdateSink)
}

// Router owns the conv→session map and creates sessions on demand.
type Router struct {
	agent       Agent
	stateDir    string
	root        *os.Root // sandbox for per-thread cwd creation
	idleTimeout time.Duration

	mu    sync.Mutex
	byKey map[ConvKey]*Session
}

// Config configures a Router.
type Config struct {
	Agent       Agent
	StateDir    string
	IdleTimeout time.Duration // 0 → 30 minutes
}

// New constructs a Router. The caller should call Run(ctx) to start GC.
func New(cfg Config) (*Router, error) {
	if cfg.Agent == nil {
		return nil, fmt.Errorf("router: nil agent")
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir()
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir state dir: %w", err)
	}
	root, err := os.OpenRoot(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("open state dir as root: %w", err)
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	return &Router{
		agent:       cfg.Agent,
		stateDir:    cfg.StateDir,
		root:        root,
		idleTimeout: cfg.IdleTimeout,
		byKey:       make(map[ConvKey]*Session),
	}, nil
}

// Close releases the os.Root handle backing per-thread cwd creation.
// Safe to call multiple times.
func (r *Router) Close() error {
	if r.root == nil {
		return nil
	}
	err := r.root.Close()
	r.root = nil
	return err
}

// DefaultStateDir picks a sensible default state root.
//
// Order: $XDG_STATE_HOME/slack-acp → $HOME/.local/state/slack-acp →
// $TMPDIR/slack-acp.
func DefaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "slack-acp")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".local", "state", "slack-acp")
	}
	return filepath.Join(os.TempDir(), "slack-acp")
}

// StateDir returns the configured state root.
func (r *Router) StateDir() string { return r.stateDir }

// cwdFor returns the stable per-thread working directory for key and
// ensures it exists on disk.
//
// Two layers of defense, since ConvKey strings flow from Slack network
// input:
//
//  1. validateKeyComponent rejects empty / "." / ".." / leading-dot /
//     paths containing separators or null bytes — this is the primary
//     check, because filepath.Join would Clean traversal symbols away
//     before any later defense could see them.
//  2. *os.Root.MkdirAll performs the actual creation inside the
//     StateDir sandbox, so even a future refactor that bypassed step 1
//     could not escape the state root on disk.
func (r *Router) cwdFor(key ConvKey) (string, error) {
	if err := validateKeyComponent(key.ChannelID); err != nil {
		return "", fmt.Errorf("channel id %q: %w", key.ChannelID, err)
	}
	if err := validateKeyComponent(key.ThreadTS); err != nil {
		return "", fmt.Errorf("thread ts %q: %w", key.ThreadTS, err)
	}
	rel := filepath.Join("threads", key.ChannelID, key.ThreadTS)
	if err := r.root.MkdirAll(rel, 0o755); err != nil {
		return "", fmt.Errorf("mkdir thread cwd: %w", err)
	}
	return filepath.Join(r.stateDir, rel), nil
}

// validateKeyComponent rejects values that could escape or distort the
// state-dir layout when joined into a path.
func validateKeyComponent(s string) error {
	if s == "" {
		return fmt.Errorf("empty")
	}
	if s == "." || s == ".." {
		return fmt.Errorf("reserved name")
	}
	if strings.ContainsAny(s, `/\`) || strings.ContainsRune(s, 0) {
		return fmt.Errorf("contains path separator or null byte")
	}
	if s[0] == '.' {
		// No legitimate Slack id starts with a dot; refuse so the
		// layout stays predictable and we don't accidentally create
		// hidden directories.
		return fmt.Errorf("leading dot")
	}
	return nil
}

// GetOrCreate returns the existing session for key, or creates one with
// the given sink installed for streaming updates.
func (r *Router) GetOrCreate(ctx context.Context, key ConvKey, sink acpclient.SessionUpdateSink) (*Session, error) {
	r.mu.Lock()
	if s, ok := r.byKey[key]; ok {
		s.lastUsed = time.Now()
		r.mu.Unlock()
		// Hot path: session already exists. Each prompt installs a
		// fresh sink (the previous prompt's sink belongs to a now-
		// finished response). agent.RebindSink is an atomic swap.
		r.agent.RebindSink(s.SessionID, sink)
		return s, nil
	}
	r.mu.Unlock()

	cwd, err := r.cwdFor(key)
	if err != nil {
		return nil, err
	}

	// Tier 1: try to resume a prior agent-side session for this thread.
	// The cwd is stable across restarts, so on a cold start the agent
	// likely has a previous session indexed under it (e.g. fir's
	// .fir/sessions/). Best-effort: any failure falls through to a
	// fresh session below.
	sid, resumed := r.tryResume(ctx, cwd, sink)
	if !resumed {
		var nerr error
		sid, nerr = r.agent.NewSession(ctx, cwd, sink)
		if nerr != nil {
			// Stable cwd: leave it on disk for the next attempt.
			return nil, fmt.Errorf("new acp session: %w", nerr)
		}
	}
	s := &Session{Key: key, SessionID: sid, Cwd: cwd, lastUsed: time.Now()}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Lost the race? Another goroutine created one for this key — drop ours.
	if other, ok := r.byKey[key]; ok {
		r.agent.DropSession(sid)
		// cwd is shared/stable across attempts — do not remove it.
		other.lastUsed = time.Now()
		r.agent.RebindSink(other.SessionID, sink)
		return other, nil
	}
	r.byKey[key] = s
	if resumed {
		debuglog.Logf("router: resumed session %s for %s in %s", sid, key, cwd)
	} else {
		debuglog.Logf("router: new session %s for %s in %s", sid, key, cwd)
	}
	return s, nil
}

// tryResume attempts to reattach to a prior agent-side session for the
// given cwd via the unstable session/list + session/resume RPCs. Returns
// (sid, true) on success, ("", false) on any failure or when the agent
// doesn't advertise the caps. Always best-effort — the caller falls back
// to NewSession on false.
//
// Roadmap (docs/design.md): when only Caps().LoadSession is advertised,
// fall back to the standard session/load. That path needs a persisted
// (ConvKey → SessionId) map under StateDir, since session/load takes a
// sessionId and there is no standard list method.
func (r *Router) tryResume(ctx context.Context, cwd string, sink acpclient.SessionUpdateSink) (acp.SessionId, bool) {
	caps := r.agent.Caps()
	if !caps.ListSessions || !caps.ResumeSession {
		return "", false
	}
	sessions, err := r.agent.ListSessions(ctx, cwd)
	if err != nil {
		debuglog.Logf("router: list sessions for %s: %v", cwd, err)
		return "", false
	}
	if len(sessions) == 0 {
		return "", false
	}
	// Pick the head. The cwd is per-thread so there's typically just
	// one entry; if multiple exist we trust the agent's ordering
	// (fir lists most-recent-first) and let any mismatch fail through
	// to NewSession on the next message.
	sid := acp.SessionId(sessions[0].SessionId)
	if err := r.agent.ResumeSession(ctx, cwd, sid, sink); err != nil {
		debuglog.Logf("router: resume %s in %s: %v", sid, cwd, err)
		return "", false
	}
	return sid, true
}

// Touch marks the session as recently used.
func (r *Router) Touch(s *Session) {
	r.mu.Lock()
	s.lastUsed = time.Now()
	r.mu.Unlock()
}

// Cancel sends an ACP session/cancel for the given conv, if a session exists.
func (r *Router) Cancel(ctx context.Context, key ConvKey) {
	r.mu.Lock()
	s, ok := r.byKey[key]
	r.mu.Unlock()
	if !ok {
		return
	}
	_ = r.agent.Cancel(ctx, s.SessionID)
}

// Run drives idle GC until ctx is cancelled.
func (r *Router) Run(ctx context.Context) {
	t := time.NewTicker(r.idleTimeout / 4)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.gcOnce()
		}
	}
}

func (r *Router) gcOnce() {
	cutoff := time.Now().Add(-r.idleTimeout)
	r.mu.Lock()
	var stale []*Session
	for _, s := range r.byKey {
		if s.lastUsed.Before(cutoff) {
			stale = append(stale, s)
		}
	}
	for _, s := range stale {
		delete(r.byKey, s.Key)
	}
	r.mu.Unlock()
	for _, s := range stale {
		debuglog.Logf("router: GC session %s (%s); cwd %s retained", s.SessionID, s.Key, s.Cwd)
		r.agent.DropSession(s.SessionID)
		// Stable cwd: keep on disk so the agent's state (e.g. .fir/)
		// survives for future resumption.
	}
}

// Len returns the number of live sessions.
func (r *Router) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byKey)
}

// Agent returns the underlying agent. Handlers use this to send
// prompts directly while holding Session.Mu.
func (r *Router) Agent() Agent { return r.agent }
