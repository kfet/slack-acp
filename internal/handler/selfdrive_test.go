package handler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/slack-acp/internal/slackproto"
)

// ---- loop guard 3: token-bucket rate cap ----

func TestSelfDriveRateCap(t *testing.T) {
	// Injected clock: no sleeping, no wall-clock polling.
	now := time.Unix(0, 0)
	b := newSelfDriveBucket(4, func() time.Time { return now })

	// Burst of 4 is allowed, the 5th is refused.
	for i := 0; i < 4; i++ {
		if !b.allow() {
			t.Fatalf("event %d refused inside the cap", i+1)
		}
	}
	if b.allow() {
		t.Fatal("5th event admitted past a cap of 4")
	}

	// A token bucket refills continuously, so the right assertion is
	// that not enough time has passed for a *single* token: at 4/min
	// one token takes 15s.
	now = now.Add(14 * time.Second)
	if b.allow() {
		t.Fatal("admitted before even one token had refilled")
	}

	// A full window restores the whole burst.
	now = now.Add(time.Minute)
	for i := 0; i < 4; i++ {
		if !b.allow() {
			t.Fatalf("event %d refused after refill", i+1)
		}
	}
	if b.allow() {
		t.Fatal("refill exceeded the cap")
	}
}

func TestSelfDriveRateCapPartialRefill(t *testing.T) {
	now := time.Unix(0, 0)
	b := newSelfDriveBucket(4, func() time.Time { return now })
	for i := 0; i < 4; i++ {
		b.allow()
	}
	// Quarter of a window → one token back, not four.
	now = now.Add(15 * time.Second)
	if !b.allow() {
		t.Fatal("quarter window should return one token")
	}
	if b.allow() {
		t.Fatal("quarter window returned more than one token")
	}
}

func TestSelfDriveRateCapDefaultsAndDisabled(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }

	// Non-positive rate falls back to the default rather than
	// admitting everything or nothing.
	b := newSelfDriveBucket(0, clock)
	for i := 0; i < defaultSelfDrivePerMinute; i++ {
		if !b.allow() {
			t.Fatalf("default cap refused event %d", i+1)
		}
	}
	if b.allow() {
		t.Fatal("default cap not enforced")
	}

	// A nil bucket (hatch off) never admits: fail closed.
	var nilBucket *selfDriveBucket
	if nilBucket.allow() {
		t.Fatal("nil bucket admitted an event")
	}
}

// ---- policy: which gate SelfDrive may bypass ----

func TestSelfDriveBypassesUserGateOnly(t *testing.T) {
	users := map[string]struct{}{"Uhuman": {}}
	channels := map[string]struct{}{"Cok": {}}

	cases := []struct {
		name string
		ev   slackproto.Event
		want bool
	}{
		{
			name: "human in allowlist, allowed channel",
			ev:   slackproto.Event{UserID: "Uhuman", ChannelID: "Cok"},
			want: true,
		},
		{
			name: "bot user not in allowlist, no self-drive → refused",
			ev:   slackproto.Event{UserID: "Ubot", ChannelID: "Cok"},
			want: false,
		},
		{
			name: "self-drive bypasses the user gate",
			ev:   slackproto.Event{UserID: "Ubot", ChannelID: "Cok", SelfDrive: true},
			want: true,
		},
		{
			name: "self-drive must NOT bypass the channel gate",
			ev:   slackproto.Event{UserID: "Ubot", ChannelID: "Cnope", SelfDrive: true},
			want: false,
		},
		{
			name: "human in a disallowed channel is still refused",
			ev:   slackproto.Event{UserID: "Uhuman", ChannelID: "Cnope"},
			want: false,
		},
	}

	h := New(Config{AllowedUserIDs: users, AllowedChannelIDs: channels})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.allowed(tc.ev); got != tc.want {
				t.Fatalf("allowed(%+v) = %v, want %v", tc.ev, got, tc.want)
			}
		})
	}
}

func TestSelfDriveNoAllowlistsConfigured(t *testing.T) {
	// With no allowlists at all, both humans and hatch events pass the
	// gate — the hatch is not a way to *tighten* anything.
	h := New(Config{})
	for _, ev := range []slackproto.Event{
		{UserID: "Uany", ChannelID: "Cany"},
		{UserID: "Ubot", ChannelID: "Cany", SelfDrive: true},
	} {
		if !h.allowed(ev) {
			t.Fatalf("allowed(%+v) = false, want true", ev)
		}
	}
}

// ---- end-to-end: Handle() on a hatch event ----

// selfDriveHandler builds a handler with the hatch on and a fake clock,
// wired to a fake agent + fake Slack so Handle can run for real.
func selfDriveHandler(t *testing.T, perMinute int, now func() time.Time) (*Handler, *int32) {
	t.Helper()
	var prompts int32
	fa := newFakeAgent()
	fa.promptHook = func(context.Context, acp.SessionId, []acp.ContentBlock) (acp.StopReason, error) {
		atomic.AddInt32(&prompts, 1)
		return "", nil
	}
	fs := newFakeSlack()
	t.Cleanup(fs.close)
	h := New(Config{
		Router:             newTestRouter(t, fa),
		API:                fs.client(),
		PromptTimeout:      5 * time.Second,
		SelfDrive:          slackproto.NewSelfDrive("drive-me-9f3a"),
		SelfDrivePerMinute: perMinute,
		Now:                now,
	})
	return h, &prompts
}

func TestHandleSelfDriveRunsAndIsRateCapped(t *testing.T) {
	now := time.Unix(0, 0)
	h, prompts := selfDriveHandler(t, 2, func() time.Time { return now })
	ctx := context.Background()

	// A top-level self-drive event must start a thread even though it
	// neither mentions the bot nor lands in a known thread — the
	// sentinel IS the addressing mechanism, so it summons like a
	// mention does.
	ev := slackproto.Event{
		UserID: "Ubot", BotUserID: "Ubot", ChannelID: "C1",
		ThreadTS: "100.0", TS: "100.0", Text: "do the thing", SelfDrive: true,
	}
	h.Handle(ctx, ev)
	if err := h.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(prompts); got != 1 {
		t.Fatalf("prompts = %d, want 1 (self-drive should summon)", got)
	}

	// Second is inside the cap of 2.
	ev.TS = "101.0"
	h.Handle(ctx, ev)
	if err := h.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(prompts); got != 2 {
		t.Fatalf("prompts = %d, want 2", got)
	}

	// Third exceeds the cap and must be dropped without prompting.
	ev.TS = "102.0"
	h.Handle(ctx, ev)
	if err := h.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(prompts); got != 2 {
		t.Fatalf("prompts = %d, want 2 (rate cap must drop the 3rd)", got)
	}

	// A full window later it works again.
	now = now.Add(time.Minute)
	ev.TS = "103.0"
	h.Handle(ctx, ev)
	if err := h.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(prompts); got != 3 {
		t.Fatalf("prompts = %d, want 3 after refill", got)
	}
}

func TestHandleSelfDriveDroppedWhenHatchOff(t *testing.T) {
	// Fail closed: a SelfDrive-marked event with no hatch configured
	// has a nil bucket and must never run.
	var prompts int32
	fa := newFakeAgent()
	fa.promptHook = func(context.Context, acp.SessionId, []acp.ContentBlock) (acp.StopReason, error) {
		atomic.AddInt32(&prompts, 1)
		return "", nil
	}
	fs := newFakeSlack()
	defer fs.close()
	h := New(Config{
		Router:        newTestRouter(t, fa),
		API:           fs.client(),
		PromptTimeout: time.Second,
	})
	ctx := context.Background()
	h.Handle(ctx, slackproto.Event{
		UserID: "Ubot", BotUserID: "Ubot", ChannelID: "C1",
		ThreadTS: "100.0", TS: "100.0", Text: "should not run", SelfDrive: true,
	})
	if err := h.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&prompts); got != 0 {
		t.Fatalf("prompts = %d, want 0 (hatch off must fail closed)", got)
	}
}

func TestSelfDriveBucketDefaultClock(t *testing.T) {
	// A nil Now falls back to time.Now; one immediate allow needs no
	// wall-clock time to pass.
	b := newSelfDriveBucket(1, nil)
	if !b.allow() {
		t.Fatal("default-clock bucket refused the first event")
	}
	if b.allow() {
		t.Fatal("cap of 1 admitted a second event")
	}
}

func TestSelfDrivePerMinuteDefaultedInNew(t *testing.T) {
	h := New(Config{SelfDrive: slackproto.NewSelfDrive("drive-me-9f3a")})
	if h.cfg.SelfDrivePerMinute != defaultSelfDrivePerMinute {
		t.Fatalf("SelfDrivePerMinute = %d, want default %d", h.cfg.SelfDrivePerMinute, defaultSelfDrivePerMinute)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 80); got != "short" {
		t.Errorf("truncate short = %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("truncate long = %q", got)
	}
}
