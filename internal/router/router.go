// Package router maps a Slack thread (channel + thread_ts) to an ACP
// session inside a shared agent process. It owns session lifecycle: cwd
// allocation, idle GC, and cancel propagation.
package router

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// Router owns the conv→session map and creates sessions on demand.
type Router struct {
	agent       *acpclient.AgentProc
	cwdRoot     string
	idleTimeout time.Duration

	mu    sync.Mutex
	byKey map[ConvKey]*Session
	bySID map[acp.SessionId]*Session
}

// Config configures a Router.
type Config struct {
	Agent       *acpclient.AgentProc
	CwdRoot     string
	IdleTimeout time.Duration // 0 → 30 minutes
}

// New constructs a Router. The caller should call Run(ctx) to start GC.
func New(cfg Config) (*Router, error) {
	if cfg.Agent == nil {
		return nil, fmt.Errorf("router: nil agent")
	}
	if cfg.CwdRoot == "" {
		cfg.CwdRoot = filepath.Join(os.TempDir(), "slack-acp")
	}
	if err := os.MkdirAll(cfg.CwdRoot, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cwd root: %w", err)
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	return &Router{
		agent:       cfg.Agent,
		cwdRoot:     cfg.CwdRoot,
		idleTimeout: cfg.IdleTimeout,
		byKey:       make(map[ConvKey]*Session),
		bySID:       make(map[acp.SessionId]*Session),
	}, nil
}

// GetOrCreate returns the existing session for key, or creates one with
// the given sink installed for streaming updates.
func (r *Router) GetOrCreate(ctx context.Context, key ConvKey, sink acpclient.SessionUpdateSink) (*Session, error) {
	r.mu.Lock()
	if s, ok := r.byKey[key]; ok {
		s.lastUsed = time.Now()
		r.mu.Unlock()
		// Re-bind the sink for this prompt; the previous prompt's sink
		// belongs to a now-finished response. We do this by registering
		// a fresh ACP session is overkill — instead, the Router owns one
		// authoritative sink per session: a fanout. But for v0 we keep
		// it simple: the agent's last-registered sink wins. Routers that
		// reuse sessions across prompts must replace the sink before
		// each Prompt call.
		r.agent.DropSession(s.SessionID)
		_ = sink // sink will be installed by ReplaceSink below
		r.replaceSink(s, sink)
		return s, nil
	}
	r.mu.Unlock()

	cwd, err := os.MkdirTemp(r.cwdRoot, "conv-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir cwd: %w", err)
	}
	sid, err := r.agent.NewSession(ctx, cwd, sink)
	if err != nil {
		_ = os.RemoveAll(cwd)
		return nil, fmt.Errorf("new acp session: %w", err)
	}
	s := &Session{Key: key, SessionID: sid, Cwd: cwd, lastUsed: time.Now()}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Lost the race? Another goroutine created one for this key — drop ours.
	if other, ok := r.byKey[key]; ok {
		r.agent.DropSession(sid)
		_ = os.RemoveAll(cwd)
		other.lastUsed = time.Now()
		r.replaceSinkLocked(other, sink)
		return other, nil
	}
	r.byKey[key] = s
	r.bySID[sid] = s
	debuglog.Logf("router: new session %s for %s in %s", sid, key, cwd)
	return s, nil
}

// replaceSink swaps the agent-side sink for an existing session. The router
// uses one ACP session per Slack thread; each new prompt installs a fresh
// streaming sink that targets that prompt's PostStreamer.
func (r *Router) replaceSink(s *Session, sink acpclient.SessionUpdateSink) {
	// NewSession would create a new session; instead we re-register on
	// the same sid. acpclient exposes DropSession; we add via the
	// internal map by re-calling agent.NewSession would be wrong.
	// Use the unexported install path: agent has no public re-bind, so
	// here we touch through a small extension below.
	r.agent.RebindSink(s.SessionID, sink)
}

func (r *Router) replaceSinkLocked(s *Session, sink acpclient.SessionUpdateSink) {
	r.agent.RebindSink(s.SessionID, sink)
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
		delete(r.bySID, s.SessionID)
	}
	r.mu.Unlock()
	for _, s := range stale {
		debuglog.Logf("router: GC session %s (%s)", s.SessionID, s.Key)
		r.agent.DropSession(s.SessionID)
		_ = os.RemoveAll(s.Cwd)
	}
}

// Len returns the number of live sessions.
func (r *Router) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byKey)
}

// Agent returns the underlying agent process. Handlers use this to send
// prompts directly while holding Session.Mu.
func (r *Router) Agent() *acpclient.AgentProc { return r.agent }
