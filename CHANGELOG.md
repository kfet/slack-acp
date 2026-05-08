# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added
- `SLACK_API_BASE` environment variable in `internal/slackproto`
  redirects slack-go's Web API base URL. No-op when unset; used by
  the e2e harness to point the bot at a local mock Slack server.
- `e2e` skill harness: stdlib-only Python mocks for both wire
  surfaces — `.fir/skills/e2e/scripts/ws.py` (RFC 6455 server) and
  `.fir/skills/e2e/scripts/fakeslack.py` (Web API + Socket Mode).
  Tests are described as inline shell recipes in `.fir/skills/e2e/SKILL.md`,
  driven via tmux windows so each step is non-blocking. Eleven
  cases covering DM round-trip, app_mention, threaded follow-up,
  cancellation, state-dir persistence, policy, bot-loop guards,
  edit/subtype filtering, mention stripping, multi-thread isolation,
  and in-thread mention replies.

### Changed
- Coverage gate now uses the standalone
  [`covgate`](https://github.com/kfet/covgate) tool (MIT,
  github.com/kfet/covgate) registered via `go.mod`'s `tool`
  directive and invoked as `go tool covgate`. Replaces the
  in-tree `cmd/covcheck` + `internal/covcheck` (deleted) — same
  semantics, now reusable across projects.
- `make` with no arguments now defaults to `make all` (full build
  including the 100% coverage gate). `make build` remains for a
  quick native-only build.

### Added
- `make all` now enforces 100% statement coverage via a `make
  coverage` step that runs the suite, strips lines matching any
  regex in `.covignore` from the profile, and fails the build if
  the resulting total is not 100%. Mirrors sibling project
  `poe-acp`'s `.covignore` pattern.

  `.covignore` uses **only file-level patterns** — never line
  numbers, never per-function regexes — because both are brittle
  against any edit nearby. Two sanctioned exclusion shapes:

    1. `cmd/<binary>/main.go` (entry-point wiring; would need a
       subprocess smoke harness to exercise meaningfully).
    2. A per-package `unreachable.go` file that isolates
       structurally-unreachable defensive code, paired with a
       doc comment justifying the unreachability claim.

  When the unreachable code is an error branch the production
  caller can't trigger, the helper in `unreachable.go` panics
  rather than returning the error — that way the caller has no
  impossible `if err != nil` branch left over to cover.

  See `AGENTS.md` "Coverage" section for the full convention.
- Comprehensive test coverage across `internal/*`, mirroring the
  approach used in sibling project `poe-acp`. The router gained a
  thin `Agent` interface so a `fakeAgent` can drive every code path;
  acpclient grew a `connect()` helper extracted from `Start` so the
  ACP wire layer can be exercised over `io.Pipe` and a re-execed
  test-binary fake agent for `Start`/`Close` paths. All
  `internal/*` packages reach **100.0%** statement coverage.
- Cold-start session resumption via the unstable ACP `session/list` +
  `session/resume` RPCs. When the agent advertises
  `sessionCapabilities.{list,resume}` (e.g. `fir --mode acp`), the
  router reattaches to the prior session for a thread's stable cwd
  instead of starting fresh; falls back to `session/new` on any
  failure or when caps are missing. Mirrors the approach in sibling
  project `poe-acp`. A standard `session/load` fallback (for agents
  that advertise only `loadSession`) is tracked in the roadmap.
- Defense-in-depth on per-thread cwd construction: ConvKey components
  are validated (no empty / `.` / `..` / leading dot / path separator
  / null byte) and the directory is created via `*os.Root.MkdirAll`
  rooted at `StateDir`, so a malformed Slack id can't escape the
  state directory.
- `router.Router.Close()` releases the `os.Root` handle on shutdown;
  `cmd/slack-acp` defers it. Now documented as not safe to call
  concurrently with `GetOrCreate` / `Cancel` (shutdown-only).

### Fixed
- handler: `clearInflight` previously compared cancel funcs via
  `fmt.Sprintf("%p", c)`, which is unsafe — Go closures from the same
  source line share a code pointer. A stale clear could therefore
  evict the live entry of a follow-up prompt. Replaced with identity
  comparison on a per-call `*inflightEntry` pointer.
- `cmd/slack-acp` now validates required tokens before logging any
  state-directory progress, so an operator with missing
  `bot_token` / `app_token` fails fast instead of seeing a benign
  "state dir created" message before the real error.
- `acpclient` `closeGrace` (the SIGINT→SIGKILL escalation window) is
  now an `atomic.Int64` so the test override is race-free against
  the production read in `Close`. Previously a `t.Parallel()`
  inside the package would have raced.

### Changed
- Per-thread cwd is now a stable path under `StateDir`
  (`<StateDir>/threads/<channel_id>/<thread_ts>`) instead of a
  random tempdir. Idle GC drops the in-memory ACP session but
  leaves the directory on disk, so agent state (e.g. `.fir/`)
  persists across restarts and enables future session resumption.
  Mirrors the approach used in sibling project `poe-acp`.
- Config field `cwd_root` renamed to `state_dir`; CLI flag
  `--cwd-root` renamed to `--state-dir`. Default is now
  `$XDG_STATE_HOME/slack-acp` (falling back to
  `~/.local/state/slack-acp`, then `$TMPDIR/slack-acp`) instead of
  `$TMPDIR/slack-acp`.
- The agent child process now runs with its cwd set to `StateDir`
  rather than `$TMPDIR`.
- `slackproto.Client.consume` now takes the events channel as a
  parameter so a closed channel can be tested directly. Pure
  refactor; the production wiring in `Run` still passes
  `c.sm.Events` unchanged.
- `acpclient` extracted `setupCmdPipes` into
  `internal/acpclient/unreachable.go`. The helper panics on
  `cmd.{Stdin,Stdout}Pipe` errors that are structurally
  unreachable in production, so `Start` no longer carries an
  impossible `if err != nil` branch.

## [0.1.0] - 2026-05-06

### Added
- Initial v0: Socket Mode Slack client, ACP agent process wrapper,
  per-thread session routing, throttled streaming message updates,
  in-flight prompt cancellation on follow-up, per-session cwd.
- Permission policies: `allow-all`, `read-only`, `deny-all`.
- User and channel allowlists.
- JSON config (DisallowUnknownFields) + env overrides for tokens.
