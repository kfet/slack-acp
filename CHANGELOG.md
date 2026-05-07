# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

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

  To make every other line genuinely 100%-covered, the
  `slackproto.Client.consume` loop now takes the events channel
  as a parameter (so a closed channel can be tested directly),
  and `acpclient.setupCmdPipes` was extracted into
  `internal/acpclient/unreachable.go`.
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
  `cmd/slack-acp` defers it.

### Fixed
- handler: `clearInflight` previously compared cancel funcs via
  `fmt.Sprintf("%p", c)`, which is unsafe — Go closures from the same
  source line share a code pointer. A stale clear could therefore
  evict the live entry of a follow-up prompt. Replaced with identity
  comparison on a per-call `*inflightEntry` pointer.

### Changed
- Per-thread cwd is now a stable path under `StateDir`
  (`<state_dir>/threads/<channel>/<thread_ts>`) instead of a random
  tempdir. Idle GC drops the in-memory ACP session but leaves the
  directory on disk, so agent state (e.g. `.fir/`) persists across
  restarts and enables future session resumption. Mirrors the
  approach used in sibling project `poe-acp`.
- Config field `cwd_root` renamed to `state_dir`; CLI flag
  `--cwd-root` renamed to `--state-dir`. Default is now
  `$XDG_STATE_HOME/slack-acp` (falling back to
  `~/.local/state/slack-acp`, then `$TMPDIR/slack-acp`) instead of
  `$TMPDIR/slack-acp`.
- The agent child process now runs with its cwd set to `StateDir`
  rather than `$TMPDIR`.

## [0.1.0] - 2026-05-06

### Added
- Initial v0: Socket Mode Slack client, ACP agent process wrapper,
  per-thread session routing, throttled streaming message updates,
  in-flight prompt cancellation on follow-up, per-session cwd.
- Permission policies: `allow-all`, `read-only`, `deny-all`.
- User and channel allowlists.
- JSON config (DisallowUnknownFields) + env overrides for tokens.
