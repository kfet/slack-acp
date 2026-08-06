// Package ratelimit provides the token bucket the relay uses as a loop
// backstop wherever it deliberately reopens a boundary that normally
// keeps it from acting on machine-authored input.
//
// There are two such places — the self-drive hatch and the
// human-author reclassification — and both need the same property: if
// every other guard somehow fails, the damage is bounded to a handful
// of wasted prompts and a loud log rather than a runaway
// reply → trigger → reply spiral. Sharing one implementation is what
// keeps the second one from being a subtly weaker copy of the first.
package ratelimit

import (
	"sync"
	"time"
)

// Bucket is a token bucket refilling at a fixed per-minute rate.
//
// A nil *Bucket never admits anything. That is deliberate: every
// caller here guards an OFF-by-default affordance, so "not configured"
// and "refuse" must be the same thing, and a missing bucket can never
// read as permission.
type Bucket struct {
	mu       sync.Mutex
	capacity float64
	tokens   float64
	last     time.Time
	now      func() time.Time
}

// New builds a bucket admitting perMinute events per minute, starting
// full. perMinute <= 0 uses fallback. now is injected so the refill
// window can be tested without sleeping; nil uses time.Now.
func New(perMinute, fallback int, now func() time.Time) *Bucket {
	if perMinute <= 0 {
		perMinute = fallback
	}
	if now == nil {
		now = time.Now
	}
	return &Bucket{
		capacity: float64(perMinute),
		tokens:   float64(perMinute),
		last:     now(),
		now:      now,
	}
}

// Allow consumes a token, reporting whether the event may proceed.
func (b *Bucket) Allow() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	t := b.now()
	if elapsed := t.Sub(b.last); elapsed > 0 {
		b.tokens += b.capacity * (elapsed.Seconds() / 60)
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = t
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
