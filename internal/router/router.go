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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/client"
	kitlog "github.com/kfet/acp-kit/log"
)

// ConvKey identifies a conversation: a Slack thread, or — under
// session_scope "thread_user" — one user's conversation in a thread.
type ConvKey struct {
	ChannelID string
	ThreadTS  string
	// UserID is set only under session_scope "thread_user": each sender
	// in a thread gets their own agent session.
	UserID string
}

func (k ConvKey) String() string {
	s := k.ChannelID + "/" + k.ThreadTS
	if k.UserID != "" {
		s += "/" + k.UserID
	}
	return s
}

// Thread is the thread k belongs to, without any per-user component.
// Thread-wide state (membership, checkpoint, branch origin) is keyed on it.
func (k ConvKey) Thread() ConvKey { return ConvKey{ChannelID: k.ChannelID, ThreadTS: k.ThreadTS} }

// validate rejects keys whose components could escape the state dir.
func (k ConvKey) validate() error {
	if err := validateKeyComponent(k.ChannelID); err != nil {
		return fmt.Errorf("channel id %q: %w", k.ChannelID, err)
	}
	if err := validateKeyComponent(k.ThreadTS); err != nil {
		return fmt.Errorf("thread ts %q: %w", k.ThreadTS, err)
	}
	if k.UserID != "" {
		if err := validateKeyComponent(k.UserID); err != nil {
			return fmt.Errorf("user id %q: %w", k.UserID, err)
		}
	}
	return nil
}

// relDir is k's directory relative to the state dir: the thread's, or
// a per-user subdirectory of it.
func (k ConvKey) relDir() string {
	d := filepath.Join("threads", k.ChannelID, k.ThreadTS)
	if k.UserID != "" {
		d = filepath.Join(d, "users", k.UserID)
	}
	return d
}

// PlaceholderCwd is the opaque cwd every ACP call carries under cwd_mode
// "agent", where the agent chooses the real working directory (typically
// from the origin _meta). It is a fixed constant, never a path on the
// relay host.
const PlaceholderCwd = "/slack-acp/agent-chooses-cwd"

// sessionFile, under cwd_mode "agent", records a conversation's agent
// session id: the placeholder cwd is shared by every session, so
// session/list cannot tell conversations apart.
const sessionFile = "session_id"

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
	// pendingSystemPromptInline, when true, causes the next handler-issued
	// prompt to prepend the router's SystemPrompt text to the user message.
	// Set when the router has a SystemPrompt but the agent doesn't advertise
	// session.systemPrompt; cleared after the first inlined prompt.
	// Also re-armed on resume (since the resumed session's context may
	// no longer carry the prior inlined prefix after compaction).
	pendingSystemPromptInline bool
}

// Agent is the subset of *client.AgentProc the router depends on.
// Exposed as an interface so tests can substitute a fake. The real
// *client.AgentProc satisfies it.
type Agent interface {
	Caps() client.Caps
	NewSession(ctx context.Context, cwd string, sink client.SessionUpdateSink, systemPromptBlocks []acp.ContentBlock) (acp.SessionId, error)
	ListSessions(ctx context.Context, cwd string) ([]client.SessionInfo, error)
	ResumeSession(ctx context.Context, cwd string, sid acp.SessionId, sink client.SessionUpdateSink) error
	ForkSession(ctx context.Context, cwd string, parent acp.SessionId, at string, sink client.SessionUpdateSink) (acp.SessionId, error)
	Prompt(ctx context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (acp.StopReason, error)
	// PromptTurn is Prompt that also returns the agent's leaf entry id
	// for the turn, recorded for branch fork points (see RecordTurn).
	PromptTurn(ctx context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (client.TurnResult, error)
	Cancel(ctx context.Context, sid acp.SessionId) error
	DropSession(sid acp.SessionId)
	RebindSink(sid acp.SessionId, sink client.SessionUpdateSink)
	// Models returns a snapshot of the agent's last-seen available
	// models and the current model id ("provider/model"). Empty until
	// a session has been created. Used by the relay to resolve the
	// provider emoji and short model name for the status line.
	Models() (models []client.ModelInfo, currentID string)
	// CurrentModel is the model one session is running. The status
	// line names it (via convo.EffectiveModel); Models' current is
	// process-wide and names whichever session was opened last.
	CurrentModel(sid acp.SessionId) (string, bool)
	// AvailableCommands is the agent's advertised command catalog; it
	// gates the `!command` passthrough allowlist.
	AvailableCommands() []client.CommandInfo
	// SetModel selects the model for one session. It backs `!model
	// <id>`, applied lazily at the start of the next turn.
	SetModel(ctx context.Context, sid acp.SessionId, modelID string) error
}

// Router owns the conv→session map and creates sessions on demand.
type Router struct {
	agent        Agent
	stateDir     string
	root         *os.Root // sandbox for per-thread cwd creation
	idleTimeout  time.Duration
	systemPrompt string
	agentCwd     bool // cwd_mode "agent"

	mu    sync.Mutex
	byKey map[ConvKey]*Session
	fresh map[ConvKey]bool // keys Reset since their last session: skip resume once

	// turnsMu serialises read-modify-write of the per-thread turns.json.
	turnsMu sync.Mutex
}

// Config configures a Router.
type Config struct {
	Agent       Agent
	StateDir    string
	IdleTimeout time.Duration // 0 → 30 minutes
	// SystemPrompt is durable per-session instruction text (e.g. "your
	// replies go to Slack, format using Slack mrkdwn"). If non-empty:
	//   - When the agent advertises Caps().SystemPrompt, the router
	//     passes it via session/new._meta["session.systemPrompt"].
	//   - Otherwise the router prefixes it to the FIRST user prompt of
	//     the session (and re-prefixes after a resume, since intra-
	//     session compaction may have dropped the prior inlined copy).
	SystemPrompt string
	// AgentCwd is cwd_mode "agent": send PlaceholderCwd, not the
	// conversation's directory.
	AgentCwd bool
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
		agent:        cfg.Agent,
		stateDir:     cfg.StateDir,
		root:         root,
		idleTimeout:  cfg.IdleTimeout,
		systemPrompt: cfg.SystemPrompt,
		agentCwd:     cfg.AgentCwd,
		byKey:        make(map[ConvKey]*Session),
		fresh:        make(map[ConvKey]bool),
	}, nil
}

// Close releases the os.Root handle backing per-thread cwd creation.
// Idempotent: the first call closes the root, subsequent calls are
// no-ops.
//
// Contract: callers must not invoke Close concurrently with
// GetOrCreate, Cancel, or any other Router method that touches the
// state directory. In practice Close is intended for shutdown after
// all incoming Slack events have drained — the cmd/slack-acp main
// wires it via `defer r.Close()` after the slackproto client returns.
func (r *Router) Close() error {
	r.mu.Lock()
	root := r.root
	r.root = nil
	r.mu.Unlock()
	if root == nil {
		return nil
	}
	return root.Close()
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

// Known returns true if a session directory exists on disk for the
// given key. Used to pre-filter ambient thread replies: only forward
// messages to threads the bot has already been summoned into.
// Membership check is disk-backed so it survives restarts.
func (r *Router) Known(key ConvKey) bool {
	r.mu.Lock()
	root := r.root
	r.mu.Unlock()
	if root == nil {
		return false
	}
	key = key.Thread()
	if key.validate() != nil {
		return false
	}
	_, err := root.Stat(key.relDir())
	return err == nil
}

// KnownThreads lists the thread ts of every thread in channel that has
// a session directory — the threads the bot has been summoned into.
// The catch-up re-reads them: a reply in a thread whose parent predates
// the checkpoint is otherwise invisible to conversations.history.
func (r *Router) KnownThreads(channel string) []string {
	r.mu.Lock()
	root := r.root
	r.mu.Unlock()
	if root == nil || validateKeyComponent(channel) != nil {
		return nil
	}
	entries, err := fs.ReadDir(root.FS(), path.Join("threads", channel))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && validateKeyComponent(e.Name()) == nil {
			out = append(out, e.Name())
		}
	}
	return out
}

// GetLastTS returns the last processed message timestamp for the given
// key, or "" if none recorded. Used for dedup (drop ts <= last) and gap
// detection (fetch history between last and new).
func (r *Router) GetLastTS(key ConvKey) string {
	r.mu.Lock()
	root := r.root
	r.mu.Unlock()
	if root == nil {
		return ""
	}
	key = key.Thread()
	if key.validate() != nil {
		return ""
	}
	b, err := root.ReadFile(filepath.Join(key.relDir(), "last_ts"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SetLastTS records the timestamp of the most recently processed message
// for the given key. Used to track checkpoint for dedup and gap detection.
func (r *Router) SetLastTS(key ConvKey, ts string) error {
	r.mu.Lock()
	root := r.root
	r.mu.Unlock()
	if root == nil {
		return fmt.Errorf("router closed")
	}
	key = key.Thread()
	if err := key.validate(); err != nil {
		return err
	}
	return root.WriteFile(filepath.Join(key.relDir(), "last_ts"), []byte(ts+"\n"), 0o644)
}

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
	if err := key.validate(); err != nil {
		return "", err
	}
	rel := key.relDir()
	if err := r.root.MkdirAll(rel, 0o755); err != nil {
		return "", fmt.Errorf("mkdir thread cwd: %w", err)
	}
	return filepath.Join(r.stateDir, rel), nil
}

// SessionKeyForCwd maps a per-thread working directory
// (<StateDir>/threads/<channel>/<thread_ts>) back to the "channel/ts"
// key used to identify the conversation in MCP tool-call logs. It
// matches router.ConvKey.String() so the two are greppable together.
//
// The key is bound server-side from the connection token by mcphost, so
// this value is what the tools see as the caller's identity; it is never
// supplied by the agent and cannot be spoofed.
//
// A per-user directory (<thread>/users/<user>) maps to "channel/ts/user".
// PlaceholderCwd (cwd_mode "agent") names no conversation and maps to "".
func SessionKeyForCwd(cwd string) string {
	if cwd == PlaceholderCwd {
		return ""
	}
	var user string
	if filepath.Base(filepath.Dir(cwd)) == "users" {
		user = "/" + filepath.Base(cwd)
		cwd = filepath.Dir(filepath.Dir(cwd))
	}
	thread := filepath.Base(cwd)
	channel := filepath.Base(filepath.Dir(cwd))
	return channel + "/" + thread + user
}

// wireCwd is the cwd the agent is told for a conversation whose relay
// directory is dir: dir itself, or PlaceholderCwd under cwd_mode "agent".
func (r *Router) wireCwd(dir string) string {
	if r.agentCwd {
		return PlaceholderCwd
	}
	return dir
}

// remember records sid as key's agent session under cwd_mode "agent",
// where it is the only way back to the session after GC or a restart.
// Best-effort: a lost record costs a fresh session, never correctness.
func (r *Router) remember(key ConvKey, sid acp.SessionId) {
	if !r.agentCwd {
		return
	}
	if err := r.root.WriteFile(filepath.Join(key.relDir(), sessionFile), []byte(string(sid)+"\n"), 0o644); err != nil {
		kitlog.Debugf("router: record session %s for %s: %v", sid, key, err)
	}
}

// storedSession names key's newest agent-side session without opening
// it: the recorded id under cwd_mode "agent", else the head of
// session/list for the conversation's directory.
func (r *Router) storedSession(ctx context.Context, key ConvKey, dir string) (acp.SessionId, error) {
	if r.agentCwd {
		b, err := r.root.ReadFile(filepath.Join(key.relDir(), sessionFile))
		sid := strings.TrimSpace(string(b))
		if err != nil || sid == "" {
			return "", errNoSession
		}
		return acp.SessionId(sid), nil
	}
	if !r.agent.Caps().ListSessions {
		return "", errNoList
	}
	sessions, err := r.agent.ListSessions(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("listing sessions: %w", err)
	}
	if len(sessions) == 0 {
		return "", errNoSession
	}
	// The cwd is per-conversation so there's typically just one entry;
	// if multiple exist we trust the agent's ordering (fir lists
	// most-recent-first).
	return acp.SessionId(sessions[0].SessionId), nil
}

var (
	errNoSession = errors.New("no session to resume")
	errNoList    = errors.New("the agent cannot list sessions")
)

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
func (r *Router) GetOrCreate(ctx context.Context, key ConvKey, sink client.SessionUpdateSink) (*Session, error) {
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
	fresh := r.fresh[key]
	r.mu.Unlock()

	cwd, err := r.cwdFor(key)
	if err != nil {
		return nil, err
	}

	// Tier 1: try to resume a prior agent-side session for this thread.
	// The cwd is stable across restarts, so on a cold start the agent
	// likely has a previous session indexed under it (e.g. fir's
	// .fir/sessions/). Best-effort: any failure falls through to a
	// fresh session below. Skipped once after Reset (`!new`), which
	// would otherwise just reload the session it discarded.
	var sid acp.SessionId
	resumed := false
	if !fresh {
		sid, resumed = r.tryResume(ctx, key, cwd, sink)
	}
	caps := r.agent.Caps()
	pendingInline := false
	if !resumed {
		var sysBlocks []acp.ContentBlock
		if r.systemPrompt != "" {
			if caps.SystemPrompt {
				sysBlocks = []acp.ContentBlock{{Text: &acp.ContentBlockText{Text: r.systemPrompt}}}
			} else {
				pendingInline = true
			}
		}
		var nerr error
		sid, nerr = r.agent.NewSession(ctx, r.wireCwd(cwd), sink, sysBlocks)
		if nerr != nil {
			// Stable cwd: leave it on disk for the next attempt.
			return nil, fmt.Errorf("new acp session: %w", nerr)
		}
	} else if r.systemPrompt != "" && !caps.SystemPrompt {
		// Resumed via the unstable list/resume path on a non-cap agent:
		// intra-session compaction may have lost the original inlined
		// prefix, so re-arm it on the next prompt. Cap-path agents are
		// trusted to restore system prompt on session/load themselves.
		pendingInline = true
	}
	s := &Session{Key: key, SessionID: sid, Cwd: cwd, lastUsed: time.Now(), pendingSystemPromptInline: pendingInline}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Lost the race? Another goroutine created one for this key — drop ours.
	if other, ok := r.byKey[key]; ok {
		r.agent.DropSession(sid)
		// cwd is shared/stable across attempts — do not remove it.
		other.lastUsed = time.Now()
		delete(r.fresh, key)
		r.agent.RebindSink(other.SessionID, sink)
		return other, nil
	}
	r.byKey[key] = s
	delete(r.fresh, key)
	r.remember(key, sid)
	if resumed {
		kitlog.Debugf("router: resumed session %s for %s in %s", sid, key, cwd)
	} else {
		kitlog.Debugf("router: new session %s for %s in %s", sid, key, cwd)
	}
	return s, nil
}

// tryResume attempts to reattach to a prior agent-side session for key
// via session/resume: the head of session/list for its directory, or
// under cwd_mode "agent" the recorded session id. Returns (sid, true)
// on success, ("", false) on any failure or when the agent doesn't
// advertise the caps. Always best-effort — the caller falls back to
// NewSession on false.
//
// Roadmap (docs/design.md): when only Caps().LoadSession is advertised,
// fall back to the standard session/load.
func (r *Router) tryResume(ctx context.Context, key ConvKey, cwd string, sink client.SessionUpdateSink) (acp.SessionId, bool) {
	if !r.agent.Caps().ResumeSession {
		return "", false
	}
	sid, err := r.storedSession(ctx, key, cwd)
	if err != nil {
		kitlog.Debugf("router: no session to resume for %s: %v", key, err)
		return "", false
	}
	if err := r.agent.ResumeSession(ctx, r.wireCwd(cwd), sid, sink); err != nil {
		kitlog.Debugf("router: resume %s in %s: %v", sid, cwd, err)
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

// Live reports key's live session id and last use without creating a
// session, so `!status` never spawns one.
func (r *Router) Live(key ConvKey) (acp.SessionId, time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byKey[key]
	if !ok {
		return "", time.Time{}, false
	}
	return s.SessionID, s.lastUsed, true
}

// Reset drops key's live session so the next GetOrCreate starts a NEW
// one — the resume tier is skipped once. The thread's cwd is kept.
func (r *Router) Reset(key ConvKey) {
	r.mu.Lock()
	s, ok := r.byKey[key]
	delete(r.byKey, key)
	r.fresh[key] = true
	r.mu.Unlock()
	if ok {
		r.agent.DropSession(s.SessionID)
	}
}

// Run drives idle GC until ctx is cancelled.
func (r *Router) Run(ctx context.Context) {
	t := time.NewTicker(r.idleTimeout / 4)
	defer t.Stop()
	r.runLoop(ctx, t.C)
}

// runLoop is the testable core: takes the tick channel as a parameter
// so tests can drive gcOnce deterministically (by sending on the
// channel) rather than racing against a real ticker.
func (r *Router) runLoop(ctx context.Context, tick <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
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
		kitlog.Debugf("router: GC session %s (%s); cwd %s retained", s.SessionID, s.Key, s.Cwd)
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

// TakePendingSystemPrompt returns the system-prompt text to prepend to
// the next user prompt for this session, or "" if none is pending. Must
// be called with Session.Mu held. Clears the pending flag.
//
// Used by the inline-fallback path when the agent doesn't advertise
// session.systemPrompt: the router's SystemPrompt is prefixed to the
// first user message of the session (and re-prefixed after a resume).
func (r *Router) TakePendingSystemPrompt(s *Session) string {
	if !s.pendingSystemPromptInline {
		return ""
	}
	s.pendingSystemPromptInline = false
	return r.systemPrompt
}
