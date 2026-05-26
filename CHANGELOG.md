# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added
- `install.sh` — POSIX-sh installer for non-Homebrew boxes (Linux,
  containers, CI). Detects OS/arch, downloads the matching binary
  from the GitHub release, verifies its sha256 against
  `checksums.txt`, and installs to `/usr/local/bin` (or
  `$HOME/.local/bin`). Honours `VERSION`, `BIN_DIR`, `OS`, `ARCH`
  env overrides. Usage documented in README.

## [0.1.0] - 2026-05-25

### Removed
- `--policy` CLI flag and `policy` config key. Permission decisions
  now go through `acp-kit/client`'s default policy (auto-approve);
  gate access at `allowed_user_ids` / `allowed_channel_ids` instead.
  Configs that still set `"policy"` will fail to load
  (`DisallowUnknownFields`); drop the line.
- `internal/acpclient`, `internal/debuglog`, `internal/policy`
  packages deleted. The same primitives now come from
  [`github.com/kfet/acp-kit`](https://github.com/kfet/acp-kit)
  (`client`, `log`) so wire-level fixes can land once for both
  `slack-acp` and `poe-acp`.

### Changed
- `internal/skills` is now a thin per-repo bundle wrapper over
  `acp-kit/skills`; the per-content-hash extraction, frontmatter
  parsing, and catalog formatting all live upstream.

### Fixed
- `TestWaitIdleCancel` race: the helper goroutine's `ctx.Done` branch
  could run before the waiter parked in `Cond.Wait`. Added a
  `waitIdleWaits` counter so the test waits for the parked state
  before cancelling; the broadcast that wakes the waiter is now
  deterministically exercised.

### Fixed
- `session_idle_timeout_seconds` config now drives router idle GC
  instead of being parsed and ignored. Negative values are rejected at
  load time.
- `PostStreamer` dead `doneSuffix` field removed; was declared but
  neither set nor read since the package was introduced.
- `slack-acp --print-paths` help text now mentions `policy:` (which
  the command actually prints).
- All wall-clock `time.Sleep` calls removed from the codebase (per
  `AGENTS.md` "No time.Sleep in tests"). Replaced with:
  `handler.WaitIdle` (new public method, sync.Cond on the inflight
  map) for handler tests; `fakeSlack.updated` + `fakeAgent.cancelSig`
  signal channels for protocol-level tests; `PostStreamer.now`
  clock injection for the throttle test; an injectable tick channel
  in `router.runLoop` for the GC test (uses unbuffered chan +
  double-send for pure channel-sync); `select{}` in the fake-agent
  idle loop. `watchdog` split into `watchdogWithTick(dur)` so the
  flush-watchdog test runs at 1ms instead of 1s.
- `slack-acp install-service --goos <other>` now auto-switches to
  dry-run output and prints a "preview only — ssh to the target host
  and run there" banner. Previously it wrote the rendered unit to
  the local Mac's `~/.config/systemd/user/...`, which never matches
  the remote box's user/home/binary layout.
- `internal/skills` test isolation: `TestLoadBuiltin_FSErrorPaths`
  now also swaps `bundleHashFn` alongside `bundleSrc` so fixture
  `SKILL.md` files extract under the fixture FS's content hash
  rather than the production binary's, preventing test runs from
  leaking fixtures into `$TMPDIR/slack-acp-<production-hash>/`. Same
  fix should be applied to poe-acp.
- Stale doc reference in `internal/skills/skills.go` package comment
  (`docs/skill-injection-plan.md` doesn't exist in slack-acp).
- Streaming sink no longer emits a bare `*Plan:*` trailer when the
  agent sends an empty/cleared `plan` session update. Empty plans
  are now dropped instead of appended to the Slack message.

### Added
- `slack-acp install-service` writes a tailored systemd-user unit
  (Linux) or launchd LaunchAgent plist (macOS) pointing at the
  binary, `config.json`, and `env` file produced by `slack-acp init`.
  `--dry-run` previews; `--force` overwrites; `--goos linux|darwin`
  renders the other platform's unit (handy for cross-host setup
  from a dev machine). Refuses to clobber an existing unit by
  default. Prints the `systemctl` / `launchctl` lines needed to
  enable + start the service rather than running them itself
  (smaller blast radius for `--force`). New `internal/installsvc`
  package; logic is pure and fully tested under the 100% gate.
- `slack-acp init` first-run wizard. Prompts (or accepts via
  `--bot-token` / `--app-token` flags) for the two Slack tokens,
  verifies them with `auth.test` (skippable via `--skip-verify`),
  and writes `$XDG_CONFIG_HOME/slack-acp/config.json` + a sibling
  `env` file (both mode `0600`) shaped for systemd / launchd. New
  `internal/initcmd` package plus `config.DefaultConfigPath` /
  `DefaultEnvPath` helpers. `--force` opt-in for overwriting an
  existing config.
- **Skills bundled into the binary.** `internal/skills/` ports
  poe-acp's `go:embed`-driven skill catalog: SKILL.md files under
  `internal/skills/bundle/<name>/` declaring `builtin: true` in
  frontmatter are extracted to `$TMPDIR/slack-acp-<hash>/skills/` on
  startup and surfaced to spawned ACP agents as a fir-style
  `<available_skills>` block appended to the system prompt. Operators
  driving `fir --mode acp` through slack-acp now get the `deploy` and
  `update` skills automatically. Host-supplied skills under
  `<dirname(--config)>/skills/` override built-ins by name.
  `sysprompt.Resolve` extended with a third `catalog` argument.
- `docs/config.example.json` — full key reference; README points at it.
- `slack-acp --print-paths` resolves and prints the config file, state
  dir, agent command, and policy without starting the bot. Handy for
  verifying what a unit file or env will actually pick up.
- Friendly startup diagnostics for Slack tokens: missing tokens print
  a multi-line error pointing operators at the right api.slack.com
  page; a swapped pair (`xapp-` in `bot_token` and vice versa) is
  detected before any network round-trip. `auth.test` rejections at
  Socket-Mode connect are wrapped with a hint to re-check the bot
  token. Logic lives in `config.ValidateTokens` (tested).
- Release pipeline: `.goreleaser.yaml` cross-builds 5 targets and
  publishes a GitHub release on every `v*` tag push via
  `.github/workflows/release.yml`, and regenerates
  `Formula/slack-acp.rb` on the shared `kfet/homebrew-fir` tap so
  `brew install kfet/fir/slack-acp` works. Reference Formula at
  `homebrew/slack-acp.rb.template`.
- `skills/release/SKILL.md` (now `internal/skills/bundle/release/SKILL.md`)
  rewritten to drive the new pipeline and poll `gh run list` post-publish.
  Deploy and update skills document the brew install / upgrade path.
- README install section leads with `brew install kfet/fir/slack-acp`.
- `docs/setup-easing-plan.md` tracks the broader setup-easing roadmap.
- GitHub Actions CI workflow (`.github/workflows/ci.yml`) running
  `make all` (vet, race tests, 100% coverage gate, 5 cross-builds,
  native build, license check) on push to `main` and on PRs.
- README badges: CI, pkg.go.dev, Go report card, license.
- Durable system-prompt injection telling the agent that replies go to
  Slack and must use Slack mrkdwn (single-asterisk bold, `<url|label>`
  links, no Markdown headings/tables/bullets, etc.). Delivered via
  `session/new._meta["session.systemPrompt"]` when the agent advertises
  the cap, or prepended once to the first user prompt of each session
  on the fallback path. Mirrors sibling project `poe-acp`. New
  `internal/sysprompt` package owns the prompt text; new
  `system_prompt` (extra operator text) and `disable_system_prompt`
  config keys.

### Changed
- **Skills relocated** to `internal/skills/bundle/<name>/SKILL.md`
  (from the previous mixed history under `.fir/skills/` and top-level
  `skills/`), matching poe-acp's layout. `.fir/skills` (gitignored)
  is now a symlink into the bundle so fir running inside this repo
  still discovers them as project-local. Path references inside
  `e2e`/`deploy`/`update` SKILL bodies updated accordingly.
- README "Status" line clarified: `v0.1.x`, primary agent is
  `fir --mode acp`; other ACP agents are less shaken out.

### Added
- `docs/slack-app-manifest.json`: Slack app manifest template covering
  Socket Mode, bot scopes, event subscriptions, and the Messages tab
  toggle so DMs have a compose box on first install. README points at it
  as the recommended setup path.
- `SLACK_API_BASE` environment variable in `internal/slackproto`
  redirects slack-go's Web API base URL. No-op when unset; used by
  the e2e harness to point the bot at a local mock Slack server.
- `e2e` skill harness: stdlib-only Python mocks for both wire
  surfaces — `skills/e2e/scripts/ws.py` (RFC 6455 server) and
  `skills/e2e/scripts/fakeslack.py` (Web API + Socket Mode).
  Tests are described as inline shell recipes in `skills/e2e/SKILL.md`,
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
