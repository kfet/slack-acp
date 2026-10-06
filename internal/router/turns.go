package router

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
)

// Turn is one finished agent turn in a thread: the Slack messages that
// bound it and the agent's leaf entry id at its end (from the prompt
// response's _meta.leafId). A branch cut at a message forks the origin
// session at the leaf of the turn containing it.
type Turn struct {
	// PromptTS is the message that started the turn ("" when no single
	// message did).
	PromptTS string `json:"prompt_ts,omitempty"`
	// ReplyTS is the first message of the answer ("" when the agent
	// abstained).
	ReplyTS string `json:"reply_ts,omitempty"`
	// Leaf is valid as the session/fork `_meta.at`.
	Leaf string `json:"leaf"`
}

func (t Turn) start() string {
	if t.PromptTS != "" {
		return t.PromptTS
	}
	return t.ReplyTS
}

// MaxTurns bounds a thread's recorded turns: the file is rewritten
// whole on every turn. A branch at a message older than the oldest
// kept turn forks at the session leaf.
const MaxTurns = 64

// MaxLeafBytes bounds Turn.Leaf: the id comes from the agent.
const MaxLeafBytes = 256

const turnsFile = "turns.json"

// RecordTurn appends a finished turn to key's turn log (turns.json in
// the thread's directory). A turn with no leaf, no message ts, or an
// oversized leaf is ignored: nothing could look it up.
func (r *Router) RecordTurn(key ConvKey, t Turn) error {
	if t.Leaf == "" || len(t.Leaf) > MaxLeafBytes || t.start() == "" {
		return nil
	}
	if _, err := r.cwdFor(key); err != nil {
		return err
	}
	r.turnsMu.Lock()
	defer r.turnsMu.Unlock()
	turns := append(r.turns(key), t)
	if len(turns) > MaxTurns {
		turns = turns[len(turns)-MaxTurns:]
	}
	b, _ := json.Marshal(turns) //nolint:errchkjson // struct of strings; cannot fail
	return r.root.WriteFile(r.turnsPath(key), b, 0o644)
}

// TurnLeaf returns the leaf of the turn in key that contains message
// ts: the newest turn that started at or before ts, so a message posted
// between two turns maps to the earlier one. "" when none is known.
//
// A message in a turn still in flight (the branch tool's own turn)
// maps to the turn before it: that turn is recorded only when it ends.
func (r *Router) TurnLeaf(key ConvKey, ts string) string {
	if ts == "" {
		return ""
	}
	r.turnsMu.Lock()
	defer r.turnsMu.Unlock()
	leaf, best := "", ""
	for _, t := range r.turns(key) {
		if s := t.start(); !tsLess(ts, s) && (best == "" || tsLess(best, s)) {
			leaf, best = t.Leaf, s
		}
	}
	return leaf
}

func (r *Router) turnsPath(key ConvKey) string {
	return filepath.Join("threads", key.ChannelID, key.ThreadTS, turnsFile)
}

// turns reads key's turn log; missing or unreadable is empty.
// Caller holds turnsMu.
func (r *Router) turns(key ConvKey) []Turn {
	r.mu.Lock()
	root := r.root
	r.mu.Unlock()
	if root == nil || validateKeyComponent(key.ChannelID) != nil || validateKeyComponent(key.ThreadTS) != nil {
		return nil
	}
	b, err := root.ReadFile(r.turnsPath(key))
	if err != nil {
		return nil
	}
	var ts []Turn
	if json.Unmarshal(b, &ts) != nil {
		return nil
	}
	return ts
}

// tsLess orders Slack ts values ("1700000000.000100") numerically:
// seconds, then the fractional part. Malformed parts compare as 0.
func tsLess(a, b string) bool {
	as, af := splitTS(a)
	bs, bf := splitTS(b)
	if as != bs {
		return as < bs
	}
	return af < bf
}

func splitTS(ts string) (sec, frac int64) {
	s, f, _ := strings.Cut(ts, ".")
	sec, _ = strconv.ParseInt(s, 10, 64)
	f = (f + "000000")[:6]
	frac, _ = strconv.ParseInt(f, 10, 64)
	return sec, frac
}
