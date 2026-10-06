---
builtin: true
name: deploy
description: Deploy slack-acp to a remote host as a supervised service. Socket Mode means no public ingress; the bot connects out to Slack over websocket.
---

# Deploy Skill

Deploy `slack-acp` to a remote host. There is **no public HTTP endpoint** —
Slack Socket Mode opens an outbound websocket from the host to Slack, so
the host needs only outbound 443. Per Slack thread the relay spawns one
ACP session inside a long-lived agent process (`fir --mode acp`,
`claude-code --acp`, etc.).

## Confirm with the user before acting

1. **Host** — ssh target (`user@host`). `local` if deploying to the same machine.
2. **Slack tokens** —
   - `SLACK_BOT_TOKEN` (`xoxb-…`): bot user OAuth token (workspace install).
   - `SLACK_APP_TOKEN` (`xapp-…`): app-level token with `connections:write`.
3. **ACP agent command** — default `fir --mode acp`. Common alternatives:
   `claude-code --acp`, `gemini-cli --acp`.
4. **State directory** — default `$XDG_STATE_HOME/slack-acp`
   (`~/.local/state/slack-acp`). Must be writable; agent state and
   per-thread cwds live there and **must persist across restarts**
   to keep thread sessions resumable.
5. **Allowlist (optional)** — `allowed_user_ids` in `config.json` if
   you want to limit who can trigger the bot.

## Slack app prep (one-time, before deploying)

Done in the Slack admin UI at https://api.slack.com/apps:

1. Create app, **Enable Socket Mode** → generate app-level token
   (`xapp-…`) with `connections:write`.
2. Bot scopes: create the app from `docs/slack-app-manifest.json`,
   which carries the full set (`app_mentions:read`, `chat:write`,
   `channels:*`, `groups:*`, `im:*`, `reactions:read`, `users:read`).
   Install to workspace; copy the bot token (`xoxb-…`).
3. Events (also in the manifest): `app_mention`, `message.channels`,
   `message.groups`, `message.im`, `reaction_added` (the
   :fork_and_knife: branch trigger). Adding a scope or event to an
   installed app needs **Reinstall to Workspace**.
4. Optional: enable DMs in **App Home → Messages Tab**.

## Steps

### 1. Fleet bot? Converge, do not hand-deploy

If the bot has (or should have) an entry in the private fleet registry
(`$FLEET_BOTS_DIR`, default `~/sync/shared/fleet/bots/<bot>.json`, with
`"relay": "slack-acp"`), steps 1 and 4 are ONE command, run from a
slack-acp clone:

```bash
scripts/converge.sh <bot>            # dry run: binary, config.json, unit/plist, running image
scripts/converge.sh <bot> --apply    # install + restart + verify Slack connected
```

The bot file holds only what differs from `distro.json` (host, platform,
`config`, ...). converge downloads the release asset named in `dist.lock`
on the host, verifies its sha256 against `checksums.txt`, writes it to a
temp file in the same directory and renames it over the binary. The
rename is ETXTBSY-safe, so the service does **not** have to be stopped
before the copy; converge restarts it afterwards (slack-acp has no
graceful reload) and waits for `slack: connected` in the log. The host
needs no Go toolchain and no clone. Still do steps 2 and 3 (agent on
PATH, env file) — converge refuses to `--apply` without the env file.

### 1b. One-off host (not in the registry)

**Homebrew:** `ssh <host> 'brew install kfet/ai/slack-acp'`

**Release binary:** the root `install.sh` fetches the right asset and
verifies its checksum:

```bash
ssh <host> 'curl -fsSL https://raw.githubusercontent.com/kfet/slack-acp/main/install.sh | BIN_DIR=$HOME/.local/bin sh'
```

Later upgrades on such a host: `slack-acp update` (same checksum +
atomic rename), then restart the supervisor.

### 2. Confirm the ACP agent is on the host's PATH

```bash
ssh <host> 'command -v fir && fir --version'
```

Remember: supervisors do **not** inherit your shell PATH. Either put the
agent's absolute path in `config.json` `agent_cmd` (e.g.
`["/home/<user>/.local/bin/fir", "--mode", "acp"]`), or set the unit's
PATH — for a fleet bot, `"systemd": {"env_path": "%h/.local/bin:/usr/local/bin:/usr/bin:/bin"}`
in its registry entry. The agent does not need Go; nothing here does.

### 3. Install secrets + config

Write `~/.config/slack-acp/env` (mode `0600`) on the host:

```
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...
```

Optionally `~/.config/slack-acp/config.json`:

```json
{
  "agent_cmd": ["fir", "--mode", "acp"],
  "allowed_user_ids": ["U0123ABC"],
  "state_dir": "/home/<user>/.local/state/slack-acp"
}
```

Tokens may live in either `env` or `config.json`. Prefer `env` so
secrets stay out of the JSON file (and out of `git diff`s if the
config is ever checked in).

### 4. Service supervisor

Prefer systemd (Linux) or launchd (macOS) over nohup/tmux.

#### Linux: systemd user unit

`~/.config/systemd/user/slack-acp.service`:

```ini
[Unit]
Description=slack-acp
After=network-online.target

[Service]
EnvironmentFile=%h/.config/slack-acp/env
ExecStart=%h/.local/bin/slack-acp --config %h/.config/slack-acp/config.json
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=default.target
```

Enable:

```bash
ssh <host> 'systemctl --user daemon-reload && \
            systemctl --user enable --now slack-acp && \
            loginctl enable-linger $USER'
```

`enable-linger` keeps the user unit running across logouts/reboots.

#### macOS: launchd user agent

launchd plists can't load `EnvironmentFile`; wrap in `sh -c` that
sources the env file. `~/Library/LaunchAgents/dev.<you>.slack-acp.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.<you>.slack-acp</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>set -a; . "$HOME/.config/slack-acp/env"; set +a; exec /opt/homebrew/bin/slack-acp --config "$HOME/.config/slack-acp/config.json"</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>/Users/<you>/go/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    <key>HOME</key><string>/Users/<you></string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/Users/<you>/Library/Logs/slack-acp.out.log</string>
  <key>StandardErrorPath</key><string>/Users/<you>/Library/Logs/slack-acp.err.log</string>
</dict>
</plist>
```

PATH must contain the ACP agent's directory (e.g. `fir` in `~/go/bin`,
or Node-based agents under your nvm dir).

```bash
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/dev.<you>.slack-acp.plist
launchctl kickstart -k gui/$UID/dev.<you>.slack-acp     # restart
launchctl bootout   gui/$UID/dev.<you>.slack-acp        # stop
launchctl print     gui/$UID/dev.<you>.slack-acp | head # status
```

### 5. Verify

```bash
ssh <host> 'journalctl --user -u slack-acp -n 50 --no-pager'   # Linux
ssh <host> 'tail -n 50 ~/Library/Logs/slack-acp.err.log'        # macOS
```

Look for the Socket Mode handshake (`connected to Slack as <bot>`).
A failed handshake means the tokens are wrong or the app-level token
lacks `connections:write`.

Smoke test in Slack:

1. **DM** the bot with a one-line prompt; expect a streaming reply.
2. **Mention** the bot in a public channel (`@<bot> hi`); reply lands
   in the thread.
3. **Reply in the thread**; the same ACP session is reused (verify by
   inspecting `<state_dir>/threads/<channel_id>/<thread_ts>/` — only
   one such directory should appear per thread).

### 6. Tail logs during first conversations

```bash
ssh <host> 'journalctl --user -u slack-acp -f'
```

Look for: Socket Mode connect, per-thread cwd creation, ACP
`initialize` handshake, and `session/prompt` traffic.

## Upgrading

See `update` skill (`internal/skills/bundle/update/SKILL.md`). Quick reference:

- **Fleet bot:** `scripts/converge.sh --tot`, commit `dist.lock`, then
  `scripts/converge.sh <bot> --apply`.
- **One-off host:** `slack-acp update && systemctl --user restart slack-acp`
  (Linux) or `launchctl kickstart -k gui/$UID/dev.<you>.slack-acp` (macOS).

## Pitfalls

- **No public URL needed** — Socket Mode is outbound-only. Don't
  configure inbound webhooks; if the user mentions Funnel/ngrok/etc.,
  it's the wrong skill.
- **Missing `connections:write`** — symptom: handshake immediately
  fails with auth error. Regenerate the app-level token with the scope.
- **Bot doesn't see DMs** — ensure App Home → Messages Tab → "Allow
  users to send messages" is enabled, and `message.im` is subscribed.
- **Bot doesn't see channel mentions** — ensure `app_mention` is
  subscribed and the bot is invited to the channel.
- **Agent not found** — supervisor PATH must include the agent's
  directory. Shell PATH is not inherited.
- **state_dir wiped on restart** — don't put it under `/tmp` or other
  ephemeral paths; thread resumption depends on `<state_dir>/threads/`
  surviving across restarts.
- **Mixed install methods** — if both `~/.local/bin/slack-acp` and a
  go-installed copy exist, the unit's `ExecStart` pins one. Upgrade
  whichever the unit points at.
- **Bot feedback loop** — handled in code (filters its own messages by
  `BotID` + `User == botUserID`). If you see the bot replying to its
  own posts, that filter regressed — file a bug.

## Handoff checklist

- [ ] `slack-acp --version` on the host matches the intended release.
- [ ] `~/.config/slack-acp/env` exists, mode `0600`, holds both tokens.
- [ ] `~/.config/slack-acp/config.json` exists (or all flags set
      explicitly in the unit).
- [ ] Supervisor enabled: systemd user unit + `loginctl enable-linger`
      (Linux) **or** launchd user agent with `RunAtLoad` + `KeepAlive`
      (macOS).
- [ ] Logs show successful Socket Mode handshake.
- [ ] DM smoke test round-trips.
- [ ] Channel `@mention` smoke test round-trips.
- [ ] Threaded follow-up reuses the same session (check `state_dir`).

## Finish on the FLEET, not on one host

**A deploy is done when the fleet is converged, not when a host is.**
This repo is public: it holds the distro spec (`distro.json`, the defaults
every slack-acp bot shares) and the resolution (`dist.lock`), never bot
instances. Each bot is one JSON file in the private fleet registry —
`$FLEET_BOTS_DIR`, default `~/sync/shared/fleet/bots` — with
`"relay": "slack-acp"` and only the fields that differ from `distro.json`.
That registry is shared by every relay (poe-acp, slack-acp, zulip-acp).

```bash
scripts/converge.sh --tot            # resolve the newest release into dist.lock; commit it
scripts/converge.sh <bot>            # dry run: binary, config.json, unit, running image
scripts/converge.sh <bot> --apply    # make the host match, restart, verify
```

Then close with the read-only sweep (runs from any fleet host):

```bash
~/sync/shared/fleet/fleet.sh status
```

It reports every relay instance on every host with wanted vs **running**
version (read from the live process, never the on-disk binary) and drift.
Do not say "released" or "deployed" until every slack-acp row is `ok`.
Paste the output into your reply.

### Two traps this sweep exists to catch

- **Never infer presence from a binary or a glob.** Under zsh,
  `ls ~/.local/bin/*-acp` **aborts the whole command** when it matches
  nothing — and the empty output reads as "not installed". Ask the supervisor:
  `systemctl --user list-units --type=service --all --no-legend --plain | awk '$1 ~ /acp/'`
  or `launchctl list | awk '$3 ~ /acp/'`. Match the unit **name**, not the
  Description.
- **A repo can have two live clones and you will release from the stale one.**
  `git fetch origin`, then `git status -sb` and read *ahead/behind*, before you
  trust any clone. Release from one canonical clone only. (A relay release once
  cut from a stale clone produced two different builds of the same tag.)


## Multi-bot on one host

Run several Slack apps from one host by giving each its own config dir,
supervisor unit, and state dir. Sockets are outbound, so there's no
port allocation to coordinate.

```
~/.config/slack-acp/
  bot-foo/
    env             # SLACK_BOT_TOKEN + SLACK_APP_TOKEN for foo, mode 0600
    config.json
    state/
  bot-bar/
    env
    config.json
    state/
```

One unit per bot (`slack-acp-foo.service`, `slack-acp-bar.service`),
each with its own `EnvironmentFile` and `--config`. Each `config.json`
must point `state_dir` at its own directory or threads will collide
across bots.
