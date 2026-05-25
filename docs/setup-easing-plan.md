# Easing slack-acp operator setup — plan

Living plan for the "make initial setup easy" effort. See chat history
/ `git log` for rationale; this file is intentionally terse.

Today's UX, from zero:

```bash
brew install kfet/fir/slack-acp
slack-acp init             # prompts for tokens, verifies, writes 0600 files
slack-acp install-service  # writes systemd unit / launchd plist
# paste the printed systemctl/launchctl lines
```

Three commands plus the irreducible Slack app-manifest paste.

## Step 1 — Release infrastructure + `release` skill ✓

`git tag vX.Y.Z && make publish` produces a GitHub Release with prebuilt
binaries for darwin/linux × amd64/arm64 (+ linux/armv6) and auto-updates
the shared `kfet/homebrew-fir` tap.

- [x] `.goreleaser.yaml` — 5 cross-builds, brews block.
- [x] `.github/workflows/release.yml` — runs on `v*` tag (needs
      `HOMEBREW_TAP_TOKEN` secret in repo settings before first cut).
- [x] `homebrew/slack-acp.rb.template` — reference copy.
- [x] `internal/skills/bundle/release/SKILL.md` — drives the new pipeline.
- [x] README + CHANGELOG.

## Step 2 — Operator quality-of-life ✓

- [x] `docs/config.example.json` + README pointer.
- [x] Friendly token diagnostics (`config.ValidateTokens`); `auth.test`
      rejection wrapped with a hint in `cmd/slack-acp/main.go`.
- [x] `slack-acp --print-paths`.

## Step 3 — `deploy` + `update` skills ✓

Already in place from earlier work; updated in step 1 to mention the
brew install / upgrade path and in step 4 to reflect new bundle paths.

- [x] `internal/skills/bundle/deploy/SKILL.md`.
- [x] `internal/skills/bundle/update/SKILL.md`.

## Step 4 — Bundle skills into the binary ✓

- [x] `internal/skills/` package, `go:embed all:bundle`, per-content-hash
      extract to `$TMPDIR/slack-acp-<hash>/skills/`, `builtin: true`
      frontmatter filter, fir-style `<available_skills>` catalog.
- [x] `sysprompt.Resolve` extended with `catalog` arg; injected into
      every session via the existing system-prompt path.
- [x] 100% coverage gate parity (verify.go via .covignore).

## Step 5 — `slack-acp init` ✓

- [x] prompt for `xoxb-` / `xapp-` (or accept via `--bot-token` /
      `--app-token`); `--non-interactive` for scripted use.
- [x] validate via `config.ValidateTokens` (shape) + live `auth.test`
      (skippable with `--skip-verify`).
- [x] write `~/.config/slack-acp/config.json` + sibling `env` (both
      0600, parent dir 0700).
- [x] `--force` to overwrite; print next-step pointer.

App-level token validation via Socket Mode connect was descoped: the
shape check plus `auth.test` on the bot token catches every fat-finger
case operators actually hit; a real Socket Mode handshake here is
seconds of extra latency for negligible additional signal.

## Step 6 — `slack-acp install-service` ✓

- [x] detect platform (`runtime.GOOS`), home, label, agent PATH.
- [x] write systemd-user unit (Linux) or launchd LaunchAgent plist
      (macOS), pointing at the binary, config, and env file `init`
      wrote.
- [x] `--dry-run` previews; `--force` overwrites; `--goos` cross-renders
      (auto-switches to dry-run if cross-GOOS without `--out`).
- [x] does NOT shell out to systemctl/launchctl — prints the commands
      for the operator to paste. Smaller blast radius for `--force`.

## Step 7 — `slack-acp setup-app`  *(not started)*

- [ ] embed `docs/slack-app-manifest.json` so it's available without a
      checkout.
- [ ] `slack-acp setup-app` prints the manifest + a numbered checklist
      with direct api.slack.com URLs.

## Step 8 — `slack-acp doctor`  *(not started)*

- [ ] validate token shapes, `auth.test`, Socket Mode handshake.
- [ ] spawn agent + ACP `initialize` round-trip.
- [ ] state dir writable, agent on PATH.
- [ ] green/red checklist output.

## Step 9 — Auto-detect agent  *(not started)*

- [ ] If `--agent-cmd` unset, probe `fir`, `claude-code --acp`,
      `gemini-cli --acp` in order; pick first on PATH.

---

## Out of scope / decided against

- **Single-token mode** — Slack Socket Mode requires both an
  App-Level token and a Bot token. Cannot collapse.
- **OAuth install link** — would require a public HTTP callback,
  defeating the Socket-Mode "no public endpoint" property.
- **Per-line `.covignore`** — never. File-level only.

## Known backlog issues

- AGENTS.md says "No `time.Sleep` in tests" but 8 call sites pre-date
  this effort (handler, slackproto, acp-kit/client, router tests). Not
  blocking step-7+ work; should be retrofitted to channel-based
  synchronisation in a dedicated cleanup pass.
- The cross-repo `bundleHashFn`-not-swapped test-isolation bug we
  fixed in slack-acp's `internal/skills` (commit 8dc96aa) still exists
  in poe-acp. Port the same fix.
