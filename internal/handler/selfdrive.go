package handler

import (
	"log"
	"sync"
	"time"

	"github.com/kfet/slack-acp/internal/slackproto"
)

// defaultSelfDrivePerMinute is the hatch's rate cap when unset.
const defaultSelfDrivePerMinute = 4

// selfDriveBucket is a token bucket capping how many self-drive events
// the relay will act on per minute.
//
// This is loop guard #3, the backstop. The prefix-anchored sentinel and
// the outbound scrub are meant to make a loop impossible; the cap is
// what bounds the damage if they ever fail, turning a runaway
// reply → trigger → reply spiral into at most a handful of wasted
// prompts and a loud log. It is the reason no recursion-depth counter
// is needed.
//
// now is injected so the window can be tested without sleeping.
type selfDriveBucket struct {
	mu       sync.Mutex
	capacity float64
	tokens   float64
	last     time.Time
	now      func() time.Time
}

func newSelfDriveBucket(perMinute int, now func() time.Time) *selfDriveBucket {
	if perMinute <= 0 {
		perMinute = defaultSelfDrivePerMinute
	}
	if now == nil {
		now = time.Now
	}
	return &selfDriveBucket{
		capacity: float64(perMinute),
		tokens:   float64(perMinute),
		last:     now(),
		now:      now,
	}
}

// allow consumes a token, reporting whether the event may proceed. A
// nil bucket means the hatch is off and never admits anything.
func (b *selfDriveBucket) allow() bool {
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

// admitSelfDrive applies the rate cap to a hatch event and logs the
// decision loudly. Every acceptance is logged: the hatch deliberately
// reopens the bot-message boundary, so an operator must be able to see
// exactly what it let through.
func (h *Handler) admitSelfDrive(ev slackproto.Event) bool {
	if !h.selfDrive.allow() {
		log.Printf("SELF-DRIVE REFUSED (rate cap %d/min exceeded): channel=%s ts=%s — dropping; check for a reply loop",
			h.cfg.SelfDrivePerMinute, ev.ChannelID, ev.TS)
		return false
	}
	log.Printf("SELF-DRIVE ACCEPTED: channel=%s ts=%s text=%q", ev.ChannelID, ev.TS, truncate(ev.Text, 80))
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
