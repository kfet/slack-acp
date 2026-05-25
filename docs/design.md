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
  auth, scratch files) is isolated between threads, and persists across
  process restarts so a thread can be resumed.

## Non-goals (v0)

- No multi-tenant Slack app (single workspace, single bot identity).
- No image / file attachment support yet (text only).
- Session resumption across restarts is **partial**: the on-disk
  per-thread cwd survives, and the router opportunistically reattaches
  via the unstable `session/list` + `session/resume` RPCs when the
  agent advertises those caps. Agents that only advertise the standard
  `loadSession` cap currently get a fresh `session/new` instead — see
  Roadmap.
- No interactive permission prompts surfaced into Slack — `acp-kit`'s
  default policy auto-approves; the bot is meant to run as a trusted,
  private agent gated at the user/channel allowlist.
- No reaction/button UX (e.g. 👍 to approve a tool call). All access
  control is declarative (`allowed_user_ids`, `allowed_channel_ids`).

## Architecture

```
   Slack                          slack-acp                       agent
   ─────                          ─────────                       ─────
  websocket  ─event→  slackproto.Client
                          │
                          ▼
                       handler.Handler ─────────────► router.Router ──► acp-kit/client.AgentProc
                          │                                                  │
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
- **acp-kit/client** (upstream `github.com/kfet/acp-kit/client`) owns
  the agent process and the ACP wire layer. It multiplexes one stdio
  child across many sessions via a `SessionId → Sink` map. Nothing
  here knows about Slack.
- **router** owns session lifecycle: `(channel, thread_ts) → SessionId`,
  per-thread cwd allocation, idle GC, cancel propagation.
- **handler** glues the two halves: each inbound event → cancel any
  in-flight prompt for that thread → fetch/create session → install a
  fresh streaming sink → call `Prompt` → stream updates back via the
  `PostStreamer`.

## Conversation key

The router keys sessions by `ChannelID + ThreadTS`. Slack guarantees
`thread_ts` is the parent message's `ts` for any reply; for top-level
messages we treat `ts == thread_ts` so a single mention without a thread
still creates a session that subsequent threaded replies will reuse.

## State directory & per-thread cwd

slack-acp keeps all on-disk state under a single **state directory**
(`StateDir`, JSON `state_dir`, flag `--state-dir`). Default:
`$XDG_STATE_HOME/slack-acp` → `~/.local/state/slack-acp` →
`$TMPDIR/slack-acp`. The agent child process is also spawned with its
cwd set to `StateDir`.

Each Slack thread gets a **stable, deterministic** working directory:

```
<StateDir>/threads/<channel_id>/<thread_ts>/
```

Slack channel IDs (`[A-Z0-9]+`) and `thread_ts` values (`\d+\.\d+`) are
filesystem-safe by construction, so the path is used verbatim — no
escaping, no hashing. This mirrors `poe-acp`'s per-conversation state
layout.

Lifecycle rules:

- **Created** lazily by the router on first prompt for a thread, via
  `*os.Root.MkdirAll` (idempotent, sandboxed inside `StateDir`).
- **Reused** on every subsequent message in the same thread — the path
  is a pure function of the conv key.
- **NOT deleted on idle GC.** GC only drops the in-memory ACP
  `SessionId` and detaches the streaming sink; the directory and any
  agent state inside it (e.g. `.fir/`) remain on disk.
- **Survives restart.** A fresh `slack-acp` process pointed at the same
  `StateDir` recomputes the same path for the same thread, so future
  resume wiring (`session/load`) can reattach without operator
  intervention.

Resumption (cold path):

On the first message for a thread the router consults
`agent.Caps()`. If the agent advertises both
`sessionCapabilities.list` and `sessionCapabilities.resume` (the
unstable RFD methods that `fir --mode acp` ships), the router calls
`session/list` with the thread's stable cwd, picks the most-recent
returned `sessionId`, and calls `session/resume`. On any failure (caps
missing, list empty, resume errored) it falls back to `session/new`.
This is the same approach used in sibling project `poe-acp`. Agents
that only advertise the standard `loadSession` cap currently fall
through to `session/new`; see Roadmap.

Operators who want to reset a thread can just `rm -rf` the directory
while the bot is idle on it; the next message will recreate it empty.

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
- Permission policy: tool calls go through `acp-kit/client`'s default
  policy, which auto-approves. Suitable only when the bot is private —
  gate access at the `allowed_*` boundary instead.
- Allowlists: `allowed_user_ids` and `allowed_channel_ids` gate inbound
  events at the handler boundary.

## Package boundaries (think before specialising)

Before adding a feature, ask which layer it belongs to:

- Slack protocol concerns (event shape, message framing) → `slackproto`.
- Agent-process concerns (spawn, stdio, ACP framing) → `acp-kit/client` (upstream).
- Session lifecycle (cwd path, GC, cancel) → `router`.
- Operator-facing config → `config`.
- Plumbing (Slack event → ACP prompt → Slack message) → `handler`.

When fixing a bug, check whether the same bug exists in sibling code
paths and fix it at the root.

## Roadmap

- File / image upload support (Slack `files` API → ACP content blocks).
- Reaction-driven controls (👍/👎 to approve, ⏹ to cancel).
- Standard `session/load` fallback for resumption: when the agent
  advertises only the stable `agentCapabilities.loadSession` cap (and
  not the unstable `sessionCapabilities.{list,resume}` pair the router
  uses today), fall back to `session/load`. That path needs a
  persisted `(channel, thread_ts) → sessionId` map under `StateDir`,
  since the standard spec has no list method and `session/load` takes
  a sessionId as input.
- Multiple agents per process (e.g. `/cmd` to pick agent).
- Interactive permission prompts surfaced as ephemeral Slack messages.

[ACP]: https://agentclientprotocol.com/
