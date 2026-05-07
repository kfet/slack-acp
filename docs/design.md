# slack-acp design

## Goals

- One Go binary that turns any [ACP] agent into a Slack bot.
- DM and `@mention` triggered. Threaded conversations are first-class:
  one Slack thread = one persistent ACP session.
- No public HTTP endpoint required: uses Slack Socket Mode (websocket).
- Streaming output: agent text + thoughts + tool calls render into a
  single Slack message that updates progressively.
- Cancellation: a follow-up message in the same thread cancels the
  in-flight prompt before starting the new one.
- Per-conversation cwd, so the agent's per-session state (skills, MCP,
  auth, scratch files) is isolated between threads.

## Non-goals (v0)

- No multi-tenant Slack app (single workspace, single bot identity).
- No image / file attachment support yet (text only).
- No persistent session storage across restarts (sessions are in-memory;
  `cwd_root` directories are per-process and GC'd on idle).
- No interactive permission prompts surfaced into Slack — the policy
  decides server-side.
- No reaction/button UX (e.g. 👍 to approve a tool call). All policy is
  declarative.

## Architecture

```
   Slack                          slack-acp                       agent
   ─────                          ─────────                       ─────
  websocket  ─event→  slackproto.Client
                          │
                          ▼
                       handler.Handler ─────────────► router.Router ──► acpclient.AgentProc
                          │                                                   │
                          │  ◄── streamingSink ◄──── session/update ◄────────┤
                          ▼
                  PostStreamer (throttled chat.update)
                          │
                          ▼
                    Slack message
```

- **slackproto** owns the Slack wire layer. It normalises AppMention and
  message.im events into a single `Event` shape and exposes a
  `PostStreamer` for throttled message updates. Nothing in this package
  knows about ACP.
- **acpclient** owns the agent process and the ACP wire layer. It
  multiplexes one stdio child across many sessions via a `SessionId →
  Sink` map. Nothing here knows about Slack.
- **router** owns session lifecycle: `(channel, thread_ts) → SessionId`,
  per-session cwd, idle GC, cancel propagation.
- **handler** glues the two halves: each inbound event → cancel any
  in-flight prompt for that thread → fetch/create session → install a
  fresh streaming sink → call `Prompt` → stream updates back via the
  `PostStreamer`.
- **policy** answers `session/request_permission` callbacks; v0 ships
  declarative allow-all / read-only / deny-all.

## Conversation key

The router keys sessions by `ChannelID + ThreadTS`. Slack guarantees
`thread_ts` is the parent message's `ts` for any reply; for top-level
messages we treat `ts == thread_ts` so a single mention without a thread
still creates a session that subsequent threaded replies will reuse.

## Streaming

Slack rate-limits `chat.update` aggressively (effectively ~1/sec per
channel). `PostStreamer` accumulates text in a buffer and emits a single
`chat.update` per `minInterval`. A 1-second watchdog goroutine flushes
any pending text while a prompt is in flight, so users don't see the
output stall between agent chunks. On `Close` we always do one final
flush regardless of timing.

If the running buffer exceeds ~35k chars (well under Slack's hard limit
of 40k), we trim from the front and prepend an ellipsis marker. A future
version may switch to "rolling new message" behaviour for very long
responses.

## Cancellation & ordering

ACP allows at most one outstanding prompt per session. The handler
serialises with `Session.Mu`. When a new event arrives for a thread that
already has an in-flight prompt, we:

1. Cancel that prompt's context (kills the watchdog and the streaming
   call site).
2. Send `session/cancel` to the agent so it stops generating.
3. Once the prior `Prompt` returns (with `cancelled` stop reason), we
   acquire `Session.Mu` and run the new prompt.

This means a fast typist gets exactly one in-flight response per thread
at a time, and the agent isn't billed for tokens it'll never deliver.

## Security boundaries

- Token handling: bot/app tokens come from env or a config file —
  never logged.
- File system: the agent can call `fs/read_text_file` and
  `fs/write_text_file`. v0 enforces only "absolute path" as a sanity
  check; sandboxing to the session cwd is a v1 follow-up.
- Permission policy: tool calls go through `policy.Decide`; default is
  `allow-all`, suitable only when the bot is private.
- Allowlists: `allowed_user_ids` and `allowed_channel_ids` gate inbound
  events at the handler boundary.

## Package boundaries (think before specialising)

Before adding a feature, ask which layer it belongs to:

- Slack protocol concerns (event shape, message framing) → `slackproto`.
- Agent-process concerns (spawn, stdio, ACP framing) → `acpclient`.
- Session lifecycle (cwd, GC, cancel) → `router`.
- Policy (tool permission decisions) → `policy`.
- Operator-facing config → `config`.
- Plumbing (Slack event → ACP prompt → Slack message) → `handler`.

When fixing a bug, check whether the same bug exists in sibling code
paths and fix it at the root.

## Roadmap

- File / image upload support (Slack `files` API → ACP content blocks).
- Reaction-driven controls (👍/👎 to approve, ⏹ to cancel).
- Optional persistence: `cwd_root` lifecycles tied to thread lifetime
  rather than process lifetime.
- Multiple agents per process (e.g. `/cmd` to pick agent).
- Interactive permission prompts surfaced as ephemeral Slack messages.

[ACP]: https://agentclientprotocol.com/
