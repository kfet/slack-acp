package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/client"
)

// Origin is where a branched thread came from: the origin thread and
// the message it was branched at. It is persisted in the CHILD's
// thread directory (origin.json), so history(origin=true) works across
// idle GC and relay restarts.
type Origin struct {
	ChannelID string `json:"channel"`
	ThreadTS  string `json:"thread_ts"`
	// BranchTS is the branch point: the origin message the branch was
	// cut at. history(origin=true) never reads past it.
	BranchTS string `json:"branch_ts"`
}

// Key is the origin thread's conversation key.
func (o Origin) Key() ConvKey { return ConvKey{ChannelID: o.ChannelID, ThreadTS: o.ThreadTS} }

const originFile = "origin.json"

// SetOrigin records that child was branched out of o. It creates the
// child's thread directory, which also makes the child Known.
func (r *Router) SetOrigin(child ConvKey, o Origin) error {
	if _, err := r.cwdFor(child); err != nil {
		return err
	}
	b, _ := json.Marshal(o) //nolint:errchkjson // struct of strings; cannot fail
	return r.root.WriteFile(filepath.Join("threads", child.ChannelID, child.ThreadTS, originFile), b, 0o644)
}

// OriginOf returns the origin recorded for child, if any.
func (r *Router) OriginOf(child ConvKey) (Origin, bool) {
	r.mu.Lock()
	root := r.root
	r.mu.Unlock()
	if root == nil || validateKeyComponent(child.ChannelID) != nil || validateKeyComponent(child.ThreadTS) != nil {
		return Origin{}, false
	}
	b, err := root.ReadFile(filepath.Join("threads", child.ChannelID, child.ThreadTS, originFile))
	if err != nil {
		return Origin{}, false
	}
	var o Origin
	if json.Unmarshal(b, &o) != nil || o.ChannelID == "" || o.ThreadTS == "" {
		return Origin{}, false
	}
	return o, true
}

// forkPoint is the agent entry id the fork is cut at (`_meta.at`).
//
// It is always empty for now. The relay sees Slack message ts values
// only; the agent never reports which of its session entries a Slack
// message became, so there is no mapping. Empty forks at the parent's
// leaf — for `!branch` and the tool that is "now", which is the branch
// point. For a :fork_and_knife: on an older message the fork carries
// more than the branch point; history(origin=true) is still clamped at
// it. For the `branch` tool the origin is mid-turn (the tool call is in
// flight); empty still forks at its last complete entry, which is
// right — do not "fix" it by guessing an entry id.
const forkPoint = ""

// Fork gives child a copy of origin's agent session (ACP session/fork),
// so the branch starts with the origin's context, and makes it child's
// live session.
//
// The fork is filed in the CHILD's cwd. That is where a later resume
// (after idle GC or a relay restart) looks for the child's session —
// filed in the origin's directory it would never reach the child, and
// a restart would resume it as the ORIGIN.
//
// Errors (client.ErrForkUnsupported included) are returned for the
// caller to log; the child then simply opens a fresh session on its
// first turn.
func (r *Router) Fork(ctx context.Context, origin, child ConvKey) (acp.SessionId, error) {
	r.mu.Lock()
	_, exists := r.byKey[child]
	r.mu.Unlock()
	if exists {
		return "", errors.New("the branch already has a session")
	}
	parent, err := r.originSession(ctx, origin)
	if err != nil {
		return "", err
	}
	cwd, err := r.cwdFor(child)
	if err != nil {
		return "", err
	}
	sid, err := r.agent.ForkSession(ctx, cwd, parent, forkPoint, discardSink{})
	if err != nil {
		return "", fmt.Errorf("forking session %s: %w", parent, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byKey[child]; ok {
		// A turn in the child raced the fork and opened its own session.
		r.agent.DropSession(sid)
		return "", errors.New("the branch opened a session while forking")
	}
	// No pending inline system prompt: the fork already carries the
	// origin's, along with the rest of its context.
	r.byKey[child] = &Session{Key: child, SessionID: sid, Cwd: cwd, lastUsed: time.Now()}
	delete(r.fresh, child)
	return sid, nil
}

// originSession names origin's agent session without touching it: the
// live one, else the newest in its directory — the one GetOrCreate
// would resume. It is NOT resumed here; that would rebind the sink of
// a turn that may be running there.
func (r *Router) originSession(ctx context.Context, origin ConvKey) (acp.SessionId, error) {
	if sid, _, ok := r.Live(origin); ok {
		return sid, nil
	}
	if !r.agent.Caps().ListSessions {
		return "", errors.New("the origin thread has no live session and the agent cannot list sessions")
	}
	if validateKeyComponent(origin.ChannelID) != nil || validateKeyComponent(origin.ThreadTS) != nil {
		return "", fmt.Errorf("invalid origin %s", origin)
	}
	list, err := r.agent.ListSessions(ctx, filepath.Join(r.stateDir, "threads", origin.ChannelID, origin.ThreadTS))
	if err != nil {
		return "", fmt.Errorf("listing the origin's sessions: %w", err)
	}
	if len(list) == 0 {
		return "", errors.New("the origin thread has no session to fork")
	}
	return acp.SessionId(list[0].SessionId), nil
}

// discardSink drops updates. A forked session runs no turn until the
// child's first prompt, and that turn rebinds its own sink.
type discardSink struct{}

func (discardSink) OnUpdate(context.Context, acp.SessionNotification) error { return nil }

var _ client.SessionUpdateSink = discardSink{}
