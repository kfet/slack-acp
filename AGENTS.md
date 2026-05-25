Use idiomatic Go. Keep it simple.

Prefer `sync/atomic`, `sync.Once`, and channels over manual mutex management when appropriate.

Do not ignore any issues, address them promptly, even if preexisting. Do not postpone any work, even if it seems daunting — just break it down into smaller tasks. **Never dismiss a problem as "pre-existing" or "out of scope" — you own this entire codebase. If you see it, you fix it.**

Do not leave incomplete or stubbed code. Ensure all code is functional and tested.

## What this is

`slack-acp` is a standalone Slack bot that relays each Slack thread to a spawned ACP-speaking agent (`fir --mode acp`, claude-code, etc.) over stdio. One binary, Socket Mode, no MCP surface. Each Slack thread (`channel_id` + `thread_ts`) maps 1:1 to an ACP session inside a shared agent process.

See [docs/design.md](docs/design.md) for the full design, goals, non-goals, and roadmap.

## Repository layout

```
cmd/slack-acp/        entry point: flags + wiring
internal/config/      JSON config loader (DisallowUnknownFields)
internal/handler/     Slack event → ACP prompt + streaming sink
internal/initcmd/     `slack-acp init` first-run wizard
internal/installsvc/  systemd / launchd supervisor unit generator
internal/router/      (channel,thread_ts) → ACP session map + GC
internal/skills/      embedded skill bundle + fir-style catalog (wraps `acp-kit/skills`)
internal/slackproto/  Socket Mode client + throttled message streamer
internal/sysprompt/   Slack-mrkdwn sysprompt composer injected per session
```

Shared ACP primitives live in [`github.com/kfet/acp-kit`](https://github.com/kfet/acp-kit): `client` (acp.Client wrapper + stdio agent process + permission gates `AllowAll`/`ReadOnly`/`DenyAll`), `log` (debug logger), `skills` (skill loader + catalog formatter). The same primitives back `poe-acp`, so wire-level fixes land once.

The handler owns `(channel,thread_ts) → session` lifecycle. Agents are spawned via `--agent-cmd` (default `fir --mode acp`). Keep the split clean: Slack framing in `slackproto`, agent + ACP via `acp-kit/client`, session lifecycle in `router`, glue in `handler`.

## Think before you specialise

Before implementing a fix or feature inside a specific package, stop and ask: **is this actually unique to this layer, or does it belong elsewhere?**

For every non-trivial change, first ask the cross-repo question: **does this belong in `acp-kit`?** acp-kit is the shared home for primitives both `slack-acp` and `poe-acp` need — ACP client wrapper, permission gates, debug log, skill loader/catalog, attachments, sysprompt helpers. If the change is about how the relay talks to an ACP agent (handshake, capabilities, permission decisions, model probing, skill catalog shape, ACP framing), make it in `acp-kit` so both relays get it. Slack-specific surfaces (Block Kit shapes, Socket Mode framing, thread routing, `chat.update` rate limits, mrkdwn rendering) stay here.

- Slack protocol concerns (event shape, message framing, Block Kit) → `slackproto`.
- Agent-process concerns (spawn, stdio, ACP framing, permission gates) → `acp-kit/client` (upstream, not local).
- Session lifecycle (cwd, GC, cancel) → `router`.
- Operator-facing config (defaults, identity) → `config`.
- Sysprompt composition (Slack-mrkdwn guidance) → `sysprompt` here; generic ACP-side sysprompt helpers → `acp-kit`.
- When fixing a bug, check whether the same bug exists in sibling code paths — both within this repo *and* in `poe-acp` / `acp-kit`. Fix it at the root, not per-site.

## Build and test

Run `make test` to verify your changes. Always finish every task with `make all` to confirm the full build and test suite passes (vet, test-race, 5 cross-builds, native build, check-licenses).

When fixing a regression, **write the test first** so it fails before your fix, then make it pass.

## Testing — avoid wall-clock timeouts

Prefer deterministic synchronization over `time.Sleep` and wall-clock polling. Use channels, `sync.WaitGroup`, or callbacks instead of `require.Eventually` with arbitrary timeouts. No `time.Sleep` in tests.

## Coverage

`make all` runs a hard 100% statement-coverage gate (`make coverage`) over a profile filtered through `.covignore`. Any uncovered statement that isn't excluded by the patterns in `.covignore` fails the build.

The gate is implemented by [`covgate`](https://github.com/kfet/covgate), registered as a Go tool in `go.mod` and invoked as `go tool covgate`. Run it directly for ad-hoc checks:

```
go tool covgate -profile=bin/coverage.tmp.out -out=bin/coverage.out -ignore=.covignore -min=100
```

`.covignore` uses **file-level patterns only** — never line numbers, never per-function regexes. Line numbers shift the moment anyone edits the file above them, and per-function regexes mask new untested code added to the same function. Anything coarser than that — a whole file, a whole subpackage, a whole generated bundle — is fine.

Two things to **avoid**:

1. **A dedicated `unreachable.go` (or `untestable.go`) file** as a place to dump hard-to-test code. That's the same shape as a `utils.go` grab-bag — code organised by a meta-property ("hard to test") rather than by what it *is*. If a branch genuinely cannot be reached from a test, prefer restructuring (panic on the impossible-error branch so callers have nothing to cover); only as a last resort exclude the file the code naturally lives in.

2. **Per-line / per-function regexes**, for the rot reason above.

To add a new exclusion: identify the file or package boundary that captures the untested code and add a single anchored pattern. See `covgate`'s README for syntax details.

## Agent-process concerns

The bot spawns the agent as a long-lived child and talks ACP over its stdio:

- **Cold-start budget** — agents like `fir --mode acp` can take seconds to be ready. Use the request context for readiness gates; don't bake in wall-clock deadlines.
- **Per-thread cwd** — each session runs in a stable working directory at `<StateDir>/threads/<channel_id>/<thread_ts>` so `.fir/` (or other agent) state stays isolated *and* persists across idle GC / restarts. Idle GC drops the in-memory ACP session but does **not** delete the directory; future resumption reuses the same path. Don't share cwds across threads, and don't reintroduce `RemoveAll` on GC.
- **Streaming throttle** — Slack `chat.update` rate limits hard at >1/sec per channel. The PostStreamer enforces this; don't bypass.
- **Cancel on follow-up** — a new message in the same thread cancels the in-flight prompt via context + `session/cancel`. Don't regress this.
- **GC** — stale sessions get reaped by idle timeout. Anything holding a session reference must check liveness.

## Slack-specific traps

- **Bot's own messages** — filter on `BotID != ""` and `User == botUserID` in the `MessageEvent` path or you'll feedback-loop.
- **`thread_ts` vs `ts`** — for top-level messages Slack omits `thread_ts`; we synthesise it from `ts` so subsequent thread replies map to the same session.
- **Mentions inside text** — strip `<@U…>` references (including mid-text) before passing to the agent.
- **Edits & subtype messages** — ignore `SubType != ""` to avoid acting on edits, deletes, channel-join events, etc.
- **App install / scopes** — Slack apps are configured from [`docs/slack-app-manifest.json`](docs/slack-app-manifest.json). When adding a new Slack API call, update the manifest's `oauth_config.scopes.bot` list (and any new `bot_events`) in the same change — operators have to reinstall the app for added scopes to take effect, so undocumented scope drift surfaces as `missing_scope` errors in production.
- **DM compose box** — requires `features.app_home.messages_tab_enabled: true` in the manifest; without it, users land on a DM with no input field. Don't remove it.
- **No native typing indicator** — bots can't trigger the "user is typing" dots over the public API. The streaming placeholder message *is* the indicator; `assistant.threads.setStatus` exists but is gated behind the Agents & AI Apps feature and only works in assistant threads.

## Changelog

When making non-trivial changes, add an entry under `## [Unreleased]` in `CHANGELOG.md` using the appropriate subsection (`### Added`, `### Fixed`, `### Changed`, `### Removed`). Keep entries concise.
