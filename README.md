# slack-acp

A Slack bot that relays each Slack thread to a spawned ACP-compatible agent
(`fir --mode acp`, claude-code, gemini-cli, etc.) over stdio.

One binary, no MCP surface, runs over Slack [Socket Mode] (no public webhook
URL needed).

## Status

v0 — DM and `@mention` work; threaded follow-ups reuse the same ACP session;
agent output streams back into a single Slack message that updates as the
agent thinks. Built on the same patterns as [poe-acp].

## How it works

```
 Slack ──ws (Socket Mode)──> slack-acp ──stdio (ACP)──> agent (fir --mode acp)
```

- Each Slack thread (`channel_id` + `thread_ts`) maps 1:1 to one ACP session.
- Each session gets its own working directory under `cwd_root` so per-agent
  state (skills, MCP, auth) stays isolated.
- A new message in the same thread reuses the existing session; a follow-up
  before the previous response finishes cancels the in-flight prompt.
- Streaming output is throttled to ~1 update/sec to stay inside Slack's
  `chat.update` rate limits.
- Permission requests from the agent are answered by the configured policy
  (`allow-all` | `read-only` | `deny-all`).

## Setup

1. Create a Slack app at <https://api.slack.com/apps>.
2. Enable **Socket Mode** and generate an app-level token (`xapp-…`) with
   `connections:write`.
3. Add bot scopes: `app_mentions:read`, `chat:write`, `im:history`,
   `im:read`, `im:write`, `users:read`. Install to workspace; grab the
   bot token (`xoxb-…`).
4. Subscribe to events: `app_mention`, `message.im`.

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
  "cwd_root": "/var/lib/slack-acp"
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
docs/                 design notes
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
