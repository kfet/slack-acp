// Package probe makes the agent's startup model probe survive a slow
// agent.
//
// The probe opens a fresh ACP session purely to learn the agent's model
// list, and it runs immediately after the agent process starts. That is
// a race: an agent command like
//
//	fir --mode acp --mcp-config <big mcp.json> --wait-mcp
//
// blocks until every MCP server is up, which can take well past any
// single fixed deadline. Losing the race produced three faces of the
// same failure in production —
//
//	probe: new session: context deadline exceeded
//	probe: new session: peer disconnected before response
//	probe: new session: context canceled
//
// — on roughly three startups in seven. The one-shot probe treated a
// slow-but-healthy agent as a permanently broken one.
//
// Models therefore retries with exponential backoff inside a larger
// overall budget, so readiness is waited for rather than sampled once.
// It stays best-effort: the model list only drives a provider emoji in
// the status header, so exhausting the budget is logged and tolerated,
// never fatal.
//
// Retrying is safe because ProbeModels is idempotent — acp-kit returns
// early once the model list is cached, so a retry that races a
// concurrently-created real session costs nothing.
package probe

import (
	"context"
	"fmt"
	"time"
)

// Default tuning. The budget is generous because the failure it covers
// is "MCP servers are still starting", which is measured in minutes on
// a cold machine; the cost of waiting is only a late emoji.
const (
	DefaultBudget  = 5 * time.Minute
	DefaultAttempt = 30 * time.Second
	DefaultInitial = time.Second
	DefaultMax     = 15 * time.Second
)

// Prober is the slice of acp-kit's *client.AgentProc that this package
// needs. Narrow by design: it keeps the retry policy testable without a
// live agent subprocess.
type Prober interface {
	ProbeModels(ctx context.Context) error
}

// Config tunes Models. The zero value of every field is usable.
type Config struct {
	// Prober is the agent to probe.
	Prober Prober

	// Budget bounds the total time spent across all attempts.
	// Default DefaultBudget.
	Budget time.Duration

	// Attempt bounds a single probe RPC. Default DefaultAttempt.
	Attempt time.Duration

	// Initial and Max bound the exponential backoff between attempts.
	// Defaults DefaultInitial / DefaultMax.
	Initial, Max time.Duration

	// Logf receives one line per failed attempt. Optional.
	Logf func(format string, args ...any)

	// Now and After are injection points for tests, defaulting to
	// time.Now and time.After. They exist so the retry loop can be
	// exercised on a fake clock with no sleeping and no wall-clock
	// polling.
	Now   func() time.Time
	After func(d time.Duration) <-chan time.Time
}

func (c *Config) applyDefaults() {
	if c.Budget <= 0 {
		c.Budget = DefaultBudget
	}
	if c.Attempt <= 0 {
		c.Attempt = DefaultAttempt
	}
	if c.Initial <= 0 {
		c.Initial = DefaultInitial
	}
	if c.Max <= 0 {
		c.Max = DefaultMax
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.After == nil {
		c.After = time.After
	}
}

// Models probes the agent for its model list, retrying a failing probe
// with exponential backoff until it succeeds, the budget runs out, or
// ctx is cancelled.
//
// It returns nil once the agent has been probed. The error from the
// final attempt is returned when the budget is exhausted, and ctx's
// error when cancelled; callers are expected to log either and carry on
// without the provider emoji.
func Models(ctx context.Context, cfg Config) error {
	cfg.applyDefaults()

	deadline := cfg.Now().Add(cfg.Budget)
	backoff := cfg.Initial

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		attemptCtx, cancel := context.WithTimeout(ctx, cfg.Attempt)
		err := cfg.Prober.ProbeModels(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}

		// Stop if another attempt plus its backoff would run past the
		// budget; retrying beyond it only delays the inevitable log.
		if !cfg.Now().Add(backoff).Before(deadline) {
			return fmt.Errorf("probe models: giving up after %d attempt(s) within %s budget: %w", attempt, cfg.Budget, err)
		}

		cfg.Logf("probe models attempt %d failed, retrying in %s (agent may still be starting): %v", attempt, backoff, err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cfg.After(backoff):
		}

		if backoff *= 2; backoff > cfg.Max {
			backoff = cfg.Max
		}
	}
}
