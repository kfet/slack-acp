---
builtin: true
name: update
description: Update slack-acp on a single host to the latest released version and restart its supervisor (systemd or launchd) so the new binary is live.
---

# Update Skill

Upgrade `slack-acp` on **one** host (local or remote) and restart the
supervisor. Use after a release publishes or when a specific host is
stale.

> Releasing lives in `internal/skills/bundle/release/SKILL.md`. For multi-host
> rollouts, repeat this skill per host.

## Inputs

Confirm with the user before acting:

1. **Host** — `local` or `user@host`. Default to local if omitted.
2. **Target version** — default: latest `vX.Y.Z` tag on `origin`.
   Override only if the user asks.

## Steps

### 1. Determine target version

```bash
git fetch --tags origin
git tag --sort=-v:refname | head -1
```

If `VERSION` is ahead of every pushed tag, an unpublished release
exists — stop and run the `release` skill first.

### 2. Probe the host

Detect installed version, install method, and supervisor. For remote,
prefix with `ssh <host>`; for local run directly.

```bash
slack-acp --version 2>/dev/null || echo not-installed
brew list --versions slack-acp 2>/dev/null       # brew install?
ls -l ~/.local/bin/slack-acp 2>/dev/null         # direct deploy?
ls -l "$(go env GOPATH)/bin/slack-acp" 2>/dev/null  # go-installed?
systemctl --user is-active slack-acp 2>/dev/null # Linux supervisor
launchctl list 2>/dev/null | grep -i slack-acp   # macOS supervisor
```

If installed version already equals target, tell the user and stop
unless they want a forced restart.

### 3. Pick the upgrade path

**Brew + launchd (typical macOS):**
```bash
brew update && brew upgrade slack-acp
launchctl kickstart -k gui/$UID/<label>
```
Find `<label>` in `~/Library/LaunchAgents/dev.*.slack-acp.plist`
(e.g. `dev.<user>.slack-acp`). On remote, use `gui/$(id -u)/<label>`
inside the ssh command.

**Brew + systemd (typical Linux):**
```bash
brew update && brew upgrade slack-acp
systemctl --user restart slack-acp
```

If `brew upgrade` reports "already up-to-date" but the version still
lags, the tap index is stale — re-run `brew update`. Persistent miss
→ fall back to `make deploy`.

**Direct deploy (`~/.local/bin`, hotfix or canonical path):**
From the repo:
```bash
make deploy HOST=<host>
ssh <host> 'systemctl --user restart slack-acp'   # Linux
ssh <host> 'launchctl kickstart -k gui/$(id -u)/dev.<you>.slack-acp'  # macOS
```

**Local install (`$GOBIN`, dev machine):**
```bash
make install
launchctl kickstart -k gui/$UID/dev.<you>.slack-acp
```

**`go install` direct from tag (any host with Go):**
```bash
ssh <host> 'go install github.com/kfet/slack-acp/cmd/slack-acp@vVERSION'
ssh <host> 'systemctl --user restart slack-acp'
```

The supervisor unit's `ExecStart` pins one binary path. Upgrade
whichever path it points at; mismatch → unit restarts the old binary.

### 4. Verify

```bash
slack-acp --version                       # must equal target
systemctl --user is-active slack-acp      # → active   (Linux)
launchctl print gui/$UID/<label> | grep state # → state = running  (macOS)
```

Tail the log for ~30 seconds after restart; expect to see the Socket
Mode handshake (`connected to Slack as <bot>`) and no error spam:

```bash
ssh <host> 'journalctl --user -u slack-acp -f'             # Linux
ssh <host> 'tail -f ~/Library/Logs/slack-acp.err.log'      # macOS
```

Then send a quick smoke message in Slack (DM or `@mention`) and
confirm a reply.

### 5. Report

One-line summary: `<host>: <old> → <new>, supervisor active`. If
anything failed, surface the error and stop — do not paper over.

## Finish on the FLEET, not on one host

**A deploy is done when the fleet is converged, not when a host is.** This relay
is listed in the fleet inventory (`~/sync/shared/fleet/inventory/slack-two.json`).
That inventory is shared across all three relays and belongs to none of
them; deploying is this repo's job, above. Close every
release/deploy/update with the read-only sweep:

```bash
~/sync/shared/fleet/fleet.sh status        # runs from any host
```

It reports every relay instance on every host — poe-acp, slack-acp and
zulip-acp — with wanted vs **running** version (read from the live process,
never the on-disk binary) and drift. Do not say "released" or "deployed" until
the `slack-two` row is `ok`. Paste the output into your reply.

Nothing to bump after a release: the sweep takes this relay's wanted
version from its latest git tag, so cutting the tag IS the declaration.
(An instance can hold back with `.pin` in its inventory entry, which
requires a `.notes` reason.)

Canonical note: `~/sync/shared/docs/notes/relays.md` (on a bot host:
`~/.local/state/poe-acp/notes/fleet/docs/notes/relays.md`).

### Two traps this sweep exists to catch

- **Never infer presence from a binary or a glob.** Every fleet host runs zsh,
  where `ls ~/.local/bin/*-acp` **aborts the whole command** when it matches
  nothing — and the empty output reads as "not installed". That is how a live
  `slack-acp` was declared absent. Ask the supervisor:
  `systemctl --user list-units --type=service --all --no-legend --plain | awk '$1 ~ /acp/'`
  or `launchctl list | awk '$3 ~ /acp/'`. Match the unit **name**, not the
  Description.
- **A repo can have two live clones and you will release from the stale one.**
  `git fetch origin`, then `git status -sb` and read *ahead/behind*, before you
  trust any clone. Release from the clone on the host that RUNS the relay —
  `kopitwo`. (2026-09-04: a `zulip-acp` release cut from the stale mikiserver
  clone produced two different v0.14.0s.)


## Pitfalls

- **Missed restart** — replacing the binary on disk does not reload
  the running process. Always restart the supervisor.
- **launchd label varies** — embeds the deploying user
  (`dev.<user>.slack-acp`). Read it from the plist, don't guess.
- **Mixed install paths** — a host may have both
  `~/.local/bin/slack-acp` and a `$GOBIN/slack-acp`; the supervisor's
  `ExecStart` pins one. Upgrade whichever the unit points at.
- **In-flight thread sessions drop** — restart kills the agent
  subprocess; any prompt currently streaming in Slack will end early.
  Threads themselves resume cleanly because each thread's cwd lives
  under `<state_dir>/threads/` and survives the restart, but the
  *current* response is lost. Avoid mid-conversation if possible.
- **Socket Mode reconnect** — after restart, the new process opens a
  fresh websocket to Slack. Brief gap (a few seconds) is normal.

## Checklist

- [ ] Target version confirmed (latest pushed tag).
- [ ] Install method + supervisor identified on the host.
- [ ] Binary upgraded via the matching path.
- [ ] Supervisor restarted.
- [ ] `slack-acp --version` matches target.
- [ ] Service active.
- [ ] Socket Mode handshake observed in logs.
- [ ] **`fleet.sh status` run, and the `slack-two` row is `ok`** (one host is not the job).
