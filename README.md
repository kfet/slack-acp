# slack-acp

[![CI](https://github.com/kfet/slack-acp/actions/workflows/ci.yml/badge.svg)](https://github.com/kfet/slack-acp/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/kfet/slack-acp.svg)](https://pkg.go.dev/github.com/kfet/slack-acp)
[![Go Report Card](https://goreportcard.com/badge/github.com/kfet/slack-acp)](https://goreportcard.com/report/github.com/kfet/slack-acp)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A Slack bot that relays each Slack thread to a spawned ACP-compatible agent
(`fir --mode acp`, claude-code, gemini-cli, etc.) over stdio.

One binary, no MCP surface, runs over Slack [Socket Mode] (no public webhook
URL needed).

## Status

Early — `v0.1.x`. DM and `@mention` work; threaded follow-ups reuse the same
ACP session; agent output streams back into a single Slack message that
updates as the agent thinks. Tested primarily against `fir --mode acp`;
other ACP agents (claude-code, gemini-cli) should work but have had less
shakeout. Built on the same patterns as [poe-acp].

## How it works

```
 Slack ──ws (Socket Mode)──> slack-acp ──stdio (ACP)──> agent (fir --mode acp)
```

- Each Slack thread (`channel_id` + `thread_ts`) maps 1:1 to one ACP session.
- Each session gets a stable working directory at
  `<StateDir>/threads/<channel_id>/<thread_ts>` so per-agent state
  (skills, MCP, auth, scratch files) stays isolated *and* persists
  across restarts.
- A new message in the same thread reuses the existing session; a follow-up
  before the previous response finishes cancels the in-flight prompt.
- Streaming output is throttled to ~1 update/sec to stay inside Slack's
  `chat.update` rate limits.
- Permission requests from the agent are answered by the configured policy
  (`allow-all` | `read-only` | `deny-all`).

## Setup

### Install

```bash
brew install kfet/fir/slack-acp
```

Or build from source: `go install github.com/kfet/slack-acp/cmd/slack-acp@latest`.

### Slack app

The fastest path is the bundled app manifest:

1. Go to <https://api.slack.com/apps> → **Create New App** → **From a manifest**.
2. Pick your workspace, paste [`docs/slack-app-manifest.json`](docs/slack-app-manifest.json), tweak the name if you want, **Create**.
3. **Basic Information** → **App-Level Tokens** → **Generate** a token with scope
   `connections:write`. Save the `xapp-…` token.
4. **Install App** → **Install to Workspace**. Save the `xoxb-…` bot token.

The manifest already enables Socket Mode, the Messages tab (so DMs have a
compose box), bot scopes, and the `app_mention` + `message.im` events.

## Run

```bash
SLACK_BOT_TOKEN=xoxb-… SLACK_APP_TOKEN=xapp-… \
  slack-acp --agent-cmd "fir --mode acp"
```

Or with a config file:

```json
{
  "bot_token": "xoxb-…",
  "app_token": "xapp-…",
  "agent_cmd": ["fir", "--mode", "acp"],
  "policy": "read-only",
  "allowed_user_ids": ["U0123456"],
  "state_dir": "/var/lib/slack-acp"
}
```

```bash
slack-acp --config /etc/slack-acp.json
```

## Repository layout

```
cmd/slack-acp/        entry point: flags + wiring
internal/acpclient/   acp.Client wrapper + stdio agent process
internal/config/      JSON config loader (DisallowUnknownFields)
internal/debuglog/    SLACK_ACP_DEBUG logger
internal/handler/     Slack event → ACP prompt + streaming sink
internal/policy/      allow-all / read-only / deny-all permission gates
internal/router/      (channel,thread_ts) → ACP session map + GC
internal/slackproto/  Socket Mode client + throttled message streamer
docs/                 design notes + Slack app manifest template
```

See [docs/design.md](docs/design.md) for goals, non-goals, and the
session-lifecycle model.

[Socket Mode]: https://api.slack.com/apis/connections/socket
[poe-acp]: https://github.com/kfet/poe-acp

## Build & test

```bash
make test        # go test ./...
make all         # vet + race + cross-builds + license check
```

## License

MIT — see [LICENSE](LICENSE).
