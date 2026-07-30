package probe_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kfet/slack-acp/internal/probe"
)

// fakeClock drives Models without any wall-clock time. now advances only
// when the code under test asks to wait, so a "5 minute budget" elapses
// in microseconds and the test is fully deterministic.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	waited []time.Duration
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Unix(0, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// After advances the clock by d and returns an already-fired channel, so
// the caller's select proceeds immediately.
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.waited = append(c.waited, d)
	fired := make(chan time.Time, 1)
	fired <- c.t
	c.mu.Unlock()
	return fired
}

func (c *fakeClock) Waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.waited...)
}

// blockingAfter records the wait but returns a channel that never
// fires, modelling "the backoff has not elapsed yet". It lets a test
// isolate whichever other select case it means to assert on.
func (c *fakeClock) blockingAfter(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.waited = append(c.waited, d)
	c.mu.Unlock()
	return make(chan time.Time)
}

// flakyProber fails the first n calls, then succeeds.
type flakyProber struct {
	failures int
	calls    int
	err      error
}

func (p *flakyProber) ProbeModels(ctx context.Context) error {
	p.calls++
	if p.calls <= p.failures {
		if p.err != nil {
			return p.err
		}
		return fmt.Errorf("probe: new session: attempt %d: context deadline exceeded", p.calls)
	}
	return nil
}

func TestModelsRetriesUntilAgentIsReady(t *testing.T) {
	// The production symptom: `fir --mode acp --wait-mcp` blocks on slow
	// MCP servers, so the first probes race agent readiness and fail.
	// A slow-but-healthy agent must still get probed.
	for _, failures := range []int{1, 3, 6} {
		t.Run(fmt.Sprintf("%d_failures", failures), func(t *testing.T) {
			clk := newClock()
			p := &flakyProber{failures: failures}

			err := probe.Models(context.Background(), probe.Config{
				Prober: p,
				Budget: 5 * time.Minute,
				Now:    clk.Now,
				After:  clk.After,
			})
			if err != nil {
				t.Fatalf("Models() = %v, want nil (agent became ready)", err)
			}
			if want := failures + 1; p.calls != want {
				t.Fatalf("prober called %d times, want %d", p.calls, want)
			}
			if got := len(clk.Waits()); got != failures {
				t.Fatalf("backed off %d times, want %d", got, failures)
			}
		})
	}
}

func TestModelsSucceedsFirstTryWithoutWaiting(t *testing.T) {
	clk := newClock()
	p := &flakyProber{}

	if err := probe.Models(context.Background(), probe.Config{
		Prober: p, Budget: time.Minute, Now: clk.Now, After: clk.After,
	}); err != nil {
		t.Fatalf("Models() = %v, want nil", err)
	}
	if p.calls != 1 {
		t.Fatalf("prober called %d times, want 1", p.calls)
	}
	if got := clk.Waits(); len(got) != 0 {
		t.Fatalf("healthy agent should not back off, waited %v", got)
	}
}

func TestModelsBackoffIsExponentialAndCapped(t *testing.T) {
	clk := newClock()
	p := &flakyProber{failures: 8}

	if err := probe.Models(context.Background(), probe.Config{
		Prober:  p,
		Budget:  time.Hour,
		Initial: time.Second,
		Max:     10 * time.Second,
		Now:     clk.Now,
		After:   clk.After,
	}); err != nil {
		t.Fatalf("Models() = %v, want nil", err)
	}
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		10 * time.Second, 10 * time.Second, 10 * time.Second, 10 * time.Second,
	}
	got := clk.Waits()
	if len(got) != len(want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("wait[%d] = %v, want %v (waits=%v)", i, got[i], want[i], got)
		}
	}
}

func TestModelsGivesUpWhenBudgetExhausted(t *testing.T) {
	// A permanently broken agent must not retry forever: the budget
	// bounds it, and the last real error is returned for the log line.
	clk := newClock()
	sentinel := errors.New("peer disconnected before response")
	p := &flakyProber{failures: 1 << 30, err: sentinel}

	err := probe.Models(context.Background(), probe.Config{
		Prober: p, Budget: 30 * time.Second, Initial: time.Second, Now: clk.Now, After: clk.After,
	})
	if err == nil {
		t.Fatal("Models() = nil, want error after budget exhaustion")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Models() = %v, want wrapped %v", err, sentinel)
	}
	if p.calls == 0 {
		t.Fatal("prober never called")
	}
	// Budget must actually bound the work, not merely be advisory.
	if elapsed := clk.Now().Sub(time.Unix(0, 0)); elapsed > 30*time.Second {
		t.Fatalf("kept retrying %v past a 30s budget", elapsed)
	}
}

func TestModelsStopsOnContextCancel(t *testing.T) {
	// Shutdown during a slow start must abandon the probe promptly
	// rather than burn the whole budget.
	clk := newClock()
	ctx, cancel := context.WithCancel(context.Background())
	p := &flakyProber{failures: 1 << 30}

	cancel()
	err := probe.Models(ctx, probe.Config{
		Prober: p, Budget: time.Hour, Now: clk.Now, After: clk.After,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Models() = %v, want context.Canceled", err)
	}
}

func TestModelsCancelDuringBackoff(t *testing.T) {
	// Cancel arrives while the retry is parked in backoff.
	//
	// The backoff channel must never fire here: if both select cases
	// were ready, Go would choose between them at random and this test
	// would only sometimes exercise the cancel path — enough to make
	// the 100% coverage gate flaky. blockingAfter leaves ctx.Done() as
	// the single ready case, so the assertion is deterministic.
	clk := newClock()
	ctx, cancel := context.WithCancel(context.Background())
	var p countingProber
	p.fn = func() error {
		cancel()
		return errors.New("not ready")
	}

	err := probe.Models(ctx, probe.Config{
		Prober: &p, Budget: time.Hour, Now: clk.Now, After: clk.blockingAfter,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Models() = %v, want context.Canceled", err)
	}
	if p.calls != 1 {
		t.Fatalf("prober called %d times, want 1", p.calls)
	}
}

type countingProber struct {
	calls int
	fn    func() error
}

func (p *countingProber) ProbeModels(context.Context) error {
	p.calls++
	return p.fn()
}

func TestModelsAppliesDefaults(t *testing.T) {
	// Zero-valued Config must be usable: real clock, sane budget and
	// backoff. One immediate success means no wall-clock time passes.
	p := &flakyProber{}
	if err := probe.Models(context.Background(), probe.Config{Prober: p}); err != nil {
		t.Fatalf("Models() = %v, want nil", err)
	}
	if p.calls != 1 {
		t.Fatalf("prober called %d times, want 1", p.calls)
	}
}

func TestModelsLogsEachRetry(t *testing.T) {
	clk := newClock()
	p := &flakyProber{failures: 2}
	var logged []string

	if err := probe.Models(context.Background(), probe.Config{
		Prober: p, Budget: time.Minute, Now: clk.Now, After: clk.After,
		Logf: func(format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		},
	}); err != nil {
		t.Fatalf("Models() = %v, want nil", err)
	}
	if len(logged) != 2 {
		t.Fatalf("logged %d lines, want 2: %v", len(logged), logged)
	}
}
