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
4. **Permission policy** — `allow-all` (default), `read-only`, `deny-all`.
5. **State directory** — default `$XDG_STATE_HOME/slack-acp`
   (`~/.local/state/slack-acp`). Must be writable; agent state and
   per-thread cwds live there and **must persist across restarts**
   to keep thread sessions resumable.
6. **Allowlist (optional)** — `allowed_user_ids` in `config.json` if
   you want to limit who can trigger the bot.

## Slack app prep (one-time, before deploying)

Done in the Slack admin UI at https://api.slack.com/apps:

1. Create app, **Enable Socket Mode** → generate app-level token
   (`xapp-…`) with `connections:write`.
2. Bot scopes: `app_mentions:read`, `chat:write`, `im:history`,
   `im:read`, `im:write`, `users:read`. Install to workspace; copy
   the bot token (`xoxb-…`).
3. Subscribe to events: `app_mention`, `message.im`.
4. Optional: enable DMs in **App Home → Messages Tab**.

## Steps

### 1. Ship the binary

Pick one:

**Homebrew (recommended, once the host has brew):**

```bash
ssh <host> 'brew install kfet/fir/slack-acp'
```

**Cross-built scp (`make deploy`):** detects remote arch, scp's the
right binary to `~/.local/bin/slack-acp`, and runs `--version`:

```bash
make deploy HOST=<host>
```

For `local` deploys, prefer `make install` (puts it in `$GOBIN`) and
then point the supervisor at `$(go env GOPATH)/bin/slack-acp`.

### 2. Confirm the ACP agent is on the host's PATH

```bash
ssh <host> 'command -v fir && fir --version'
```

Remember: supervisors do **not** inherit your shell PATH. The unit
file's PATH must include the agent's directory.

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
  "policy": "read-only",
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

- **Direct deploy:** `make deploy HOST=<host> && ssh <host> 'systemctl --user restart slack-acp'`.
- **Local launchd:** `make install && launchctl kickstart -k gui/$UID/dev.<you>.slack-acp`.

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
