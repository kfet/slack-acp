package slackproto

import (
	"strings"
	"sync"
)

// selfTSCapacity bounds the ring of ts values we posted ourselves. It
// only needs to cover the window between our own write and Slack
// echoing it back, so a few hundred is generous; the point of the bound
// is that a long-running relay must not accumulate them forever.
const selfTSCapacity = 256

// scrubbedSentinel replaces the sentinel in anything the relay posts.
const scrubbedSentinel = "[sentinel]"

// SelfDrive is the opt-in escape hatch that lets the operator drive the
// relay using the same bot token the relay itself posts with.
//
// The problem it solves: an operator testing the user → agent → reply
// round trip can only post as the bot, so the inbound event carries the
// bot's own user id and bot_id. Identity alone therefore cannot separate
// "a message I should answer" from "my own reply I must ignore", and any
// hatch keyed on identity alone is a reply → trigger → reply loop.
//
// The hatch is therefore keyed on a prefix-anchored text sentinel, not
// identity. Prefix rather than substring is the whole trick: the
// realistic loop path is the agent quoting the trigger back somewhere
// inside its answer, and agents rarely *begin* a reply with the exact
// token.
//
// SelfDrive is mechanism only — detection, stripping, and the two
// structural loop guards (outbound scrub, self-posted ts memory). The
// rate cap, allowlist policy, and logging live in the handler.
//
// The zero value is not usable; use NewSelfDrive. A nil *SelfDrive is
// valid and inert — that is how "hatch off" is represented, so every
// method must be nil-safe and fail closed.
type SelfDrive struct {
	sentinel string

	mu   sync.Mutex
	ring []string
	next int
	seen map[string]struct{}
}

// NewSelfDrive returns a hatch keyed on sentinel. An empty sentinel
// yields a disabled hatch (the default, and the only safe production
// setting).
func NewSelfDrive(sentinel string) *SelfDrive {
	return &SelfDrive{
		sentinel: sentinel,
		ring:     make([]string, selfTSCapacity),
		seen:     make(map[string]struct{}, selfTSCapacity),
	}
}

// Enabled reports whether the hatch is on.
func (d *SelfDrive) Enabled() bool { return d != nil && d.sentinel != "" }

// Accept reports whether text is a self-drive trigger and, if so,
// returns it with the sentinel stripped.
//
// The match is anchored at position 0 — not Contains, and not after
// trimming leading space — so that only a message deliberately *begun*
// with the token qualifies.
func (d *SelfDrive) Accept(text string) (string, bool) {
	if !d.Enabled() || !strings.HasPrefix(text, d.sentinel) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(text, d.sentinel)), true
}

// Scrub neutralises every occurrence of the sentinel in outbound text.
//
// This is the belt to Accept's braces: if the relay can never post the
// token, an echo loop is structurally impossible even if the
// prefix-anchoring above is later relaxed to a substring match. It runs
// on everything the relay posts, not just self-drive replies.
func (d *SelfDrive) Scrub(text string) string {
	if !d.Enabled() {
		return text
	}
	return strings.ReplaceAll(text, d.sentinel, scrubbedSentinel)
}

// RecordTS remembers a ts the relay just posted or updated.
//
// Best-effort by design: the memory is in-process and lost on restart,
// which is acceptable because it is the second layer, not the gate.
// Accept is the gate.
func (d *SelfDrive) RecordTS(ts string) {
	if d == nil || ts == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.seen[ts]; dup {
		return
	}
	if old := d.ring[d.next]; old != "" {
		delete(d.seen, old)
	}
	d.ring[d.next] = ts
	d.seen[ts] = struct{}{}
	d.next = (d.next + 1) % len(d.ring)
}

// SeenTS reports whether ts is one the relay posted itself.
func (d *SelfDrive) SeenTS(ts string) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.seen[ts]
	return ok
}

// Len returns how many self-posted ts values are remembered.
func (d *SelfDrive) Len() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}
