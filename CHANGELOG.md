# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added
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
