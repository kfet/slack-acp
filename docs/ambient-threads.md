# Ambient threads

> **Design premise.** A bot that only answers when summoned is just a
> slash-command with extra steps. The value is an agent *present* in the thread
> that chimes in when it actually helps — handling **multiple people** and
> deciding for itself when to respond **unprompted**. Take "let it decide"
> seriously: `@`-mention is a strong prior, not a gate; the abstain path must be
> cheap (short max-tokens, no eager placeholder, optionally a fast/cheap model).
>
> Provenance: distilled from the original design conversation on the sea-fir bot
> (host sea-racknerd), Poe conversation c-…kvs8l7kl8bg8k73vnrmiyt, 2026-06-06 → 06-14.

Let the agent **live in the thread**: forward every message in a thread it's
part of into the ACP session; it replies, or stays silent. The `@`-mention
*summons* the bot into a thread — it doesn't gate individual replies.

- No addressed/ambient code branch.
- No separate thread-context plumbing — **the session is the thread memory**.
- Each Slack line already carries its sender + any tagged handles; just forward.

## Why follow-ups go silent today

Two delivery paths in `slackproto.handleEventsAPI`:

- `AppMentionEvent` → delivered (explicit `@bot`).
- `MessageEvent` → delivered **only for DMs** (`ChannelType == "im"`); everything
  else is dropped.

Slack emits `AppMention` only on a real tag. Un-tagged thread replies hit the
`MessageEvent` branch and get dropped. The per-thread **session survives** on
disk; it simply never hears the follow-ups.

```
@bot msg ──AppMention──┐
                       ├─ handleEventsAPI ─┬─ session ✓ (mention / DM)
thread reply ─Message──┘                   └─ dropped (non-DM)
```

## The move: one rule

> **A thread is ours iff its session dir exists on disk**
> (`threads/<channel>/<thread_ts>`).

The `@`-mention is just what *creates* that dir. Membership, restart recovery,
and every edge fall out of this single fact.

```
thread msg ─→ Known(key)?  ──exists──→ resume + forward
              (checks disk) ──else───→ ignore
```

## Edges

| Edge | Handling |
|------|----------|
| **Restart** | *Free.* `byKey` empty after restart, but dirs are on disk; `tryResume` already reattaches per-cwd. Only change: `Known` consults disk, not just memory. |
| **Missed while down** | *Backfill.* Socket Mode does **not** replay missed events. Track `last_ts` per thread; on the next message, fetch `conversations.replies` since the gap and feed the missed lines into the session before the new one. |
| **Duplicate delivery** | *Free.* Slack is at-least-once → drop any `ts <= last_ts`. Same state as backfill. |
| **First tag lost (bot down)** | *Accept.* No dir = blind to that thread, correctly — it was never summoned. A re-tag re-creates the dir. Exactly how an offline colleague behaves. |
| **Edits / deletes** | *Drop.* Already ignored via `SubType`. Keep ignoring — replaying edits is noise. |

Outage recovery is self-healing: one `conversations.replies` call on the next
message is the difference between "caught up" and "confidently wrong about the
thread."

## Decline protocol

The agent abstains by emitting a sentinel (e.g. exactly `<<SILENT>>`). The
relay buffers the stream; if the full output is the sentinel (or empty), it
posts **nothing**. The abstained message still flows through the session, so
the agent stays caught up on the thread even when it doesn't speak.

`@`-mention is a strong *prior* ("probably for me"), not a gate — addressed and
unaddressed messages take the same path; only the prior differs.

> **Implementation note (2026-07, review).** The "same path" ideal above
> conflicts with the eager "thinking…" placeholder the relay posts for the
> addressed/DM UX (Slack has no typing indicator). Abstain requires posting
> *nothing*, but a placeholder posted up-front cannot be cleanly retracted
> (there is no `chat.delete` in the streamer). To keep the addressed/DM path
> byte-for-byte unchanged and safe, abstain is currently gated to **ambient
> mode only** (`Ambient=true`): on the ambient path the relay skips the eager
> placeholder and the spinner, so an abstained turn posts nothing. On the
> addressed/DM path abstain is disabled and the sentinel is treated as
> ordinary text. Unifying the paths would require a no-eager-placeholder
> streaming mode (post lazily on first real chunk) for all messages — a
> follow-up, not done here.

## Change list

- `slackproto`: deliver non-DM thread replies (needs `message.channels` scope +
  event subscription). **Skip replies that already @-mention the bot** — Slack
  delivers those a second time as an `app_mention` event, so forwarding the
  `message.channels` copy too would double-process the same `(channel, ts)`.
- `router.Known(key)`: check the on-disk dir, not just `byKey`.
- `last_ts` file per thread → dedup + gap detection.
- backfill via `conversations.replies` on detected gap *(recommended)*.
- sysprompt: one line — "you're in a shared thread; each line names its sender;
  reply, or output `<<SILENT>>` if you have nothing useful to add."

No membership knob, no addressed/ambient split, no separate context feed.

---

## What's configuration — per deployment vs per channel

Guiding principle: **the agent decides whether to reply; config sets priors,
persona, cost, and reach — not reply rules.** Resist building a rules engine.

### Per deployment (global `config.json`, today)

- `bot_token` / `app_token`, `agent_cmd`, `state_dir`
- `session_idle_timeout_seconds`
- `model_probe_budget_seconds` (int, default `300`) — how long the startup
  model probe may keep retrying a not-yet-ready agent. The probe opens one
  throwaway ACP session to learn the model list, which only drives the
  provider emoji in the status header. Agents that block on external
  readiness — `fir --mode acp --wait-mcp` waits for every MCP server — can
  take minutes to answer, so the probe retries with exponential backoff
  inside this budget rather than sampling once. It runs in the background:
  the Slack connection is never gated on it, and exhausting the budget is
  logged and tolerated, never fatal.
- `system_prompt` (base persona), `disable_system_prompt`
- `allowed_user_ids`, `allowed_channel_ids`

### New global knobs

- `ambient` (bool) — master switch: follow un-tagged thread replies at all.
- `backfill` (bool) + `backfill_max_messages` — outage catch-up behaviour.
- `silent_sentinel` (string, default `<<SILENT>>`) — rarely overridden.
- `hide_thinking` (bool, default `false`) — suppress the agent's thought
  chunks (the italic one-liners) from the posted message. Note thoughts are
  always suppressed on the ambient/abstain path regardless of this knob, so
  a thinking agent can still emit the sentinel and stay silent.

### The bot-message boundary and the self-drive hatch

**Invariant: `app_mention` never accepts a bot-authored event.** No
exception, ever, and no conditional security logic on that path. The relay
posts its own replies as the same bot, so a bot-authored `app_mention` that
could re-trigger is an unbounded reply → trigger → reply loop. `AllowedUserIDs`
masks this in an allowlisted deployment; a deployment without one is exposed,
so the guard lives in `slackproto` where it cannot be configured away.

That leaves a real operator need unmet: testing the user → agent → reply round
trip is only possible by posting with the *same bot token*, hence the same
user id and `bot_id`. Identity alone cannot separate "answer this" from "this
is my own reply", so the hatch is keyed on text instead.

- `self_drive_sentinel` (string, default `""` = **OFF**, fail closed) — a
  bot-authored **channel** message whose text *begins* with this token is
  accepted, sentinel stripped. Must be ≥ 8 characters and must differ from
  `silent_sentinel`.
- `self_drive_per_minute` (int, default `4`) — token-bucket cap on
  hatch-accepted events.

> **⚠️ This deliberately reopens the bot-message boundary. Leave it empty in
> production.**

Design points, each load-bearing:

- **Prefix-anchored, not `Contains`.** The realistic loop is the agent echoing
  its trigger back mid-reply; agents rarely *begin* a reply with the token.
- **`MessageEvent` path only.** `app_mention` keeps the absolute guard above,
  so hatch triggers must not rely on @-mentioning the bot — the sentinel *is*
  the addressing mechanism. Correspondingly, the `mentionsBot` suppression is
  **not** applied to the hatch branch: its `app_mention` twin is already dead,
  and dropping it here too would lose the message on both paths.
- **`SubType != ""` always drops.** The relay streams by editing its own
  message, so each throttled `chat.update` arrives as `message_changed`; only
  original posts are matched.
- Accepted both top-level (starts a thread) and as a thread reply.

Four independent loop guards, in order of how much they are relied on:

1. **Outbound scrub (belt).** The relay neutralises the sentinel in
   *everything* it posts, making an echo loop structurally impossible rather
   than merely unlikely — independent of how the match is done.
2. **Self-posted `ts` suppression (braces).** A bounded (256) ring of ts values
   returned by `chat.postMessage` / `chat.update`; inbound events matching one
   are skipped. Best-effort and lost on restart — a second layer, not the gate.
3. **Rate cap (backstop).** Token bucket, default 4/min, bounding the damage if
   the first two ever fail. This is why no recursion-depth counter is needed.
4. **Channel allowlist still applies.** The hatch bypasses the *user* gate only
   (the bot is not in `AllowedUserIDs`), and only when the event is marked
   self-drive. It must never bypass `AllowedChannelIDs`.

Every hatch acceptance is logged loudly with channel, ts, and text prefix.

Layering follows the existing seam: `slackproto` is mechanism (detect, strip,
scrub, ts memory, the hard guard — all identity filtering in one file) and
`handler` is policy (rate cap, allowlist bypass, logging).

### Per channel (new `channels: { "C123…": { … } }` map, merged over defaults)

Slack channels have distinct cultures; the channel is the natural override unit
(`ConvKey` already carries `ChannelID`). Keep the surface **thin** — only what
shapes priors/cost/reach, never hard reply rules:

- `ambient` — some channels full-presence, others `@`-tag-only. **The one true
  gate worth exposing.**
- `system_prompt` / persona addendum — `@ops` bot reads differently in
  `#incidents` vs `#random`.
- `model` — cheap model in chatty channels, strong model where it matters.
- `allowed_user_ids` — per-channel access.
- `session_idle_timeout_seconds` — per-channel GC.

**Implementation impact:** per-channel persona/model means the single
`Router.SystemPrompt string` (and model selection) must become a resolver keyed
on `ConvKey.ChannelID`, applied at session creation. Everything else is a
straightforward merge-over-defaults lookup.

What stays the agent's call (not config): whether a given message is for it,
whether it can help unasked, when to abstain. That's the whole value prop.

---

## acp-kit reuse

`acp-kit` already generalises most of this: `state.Manager` (conv→session +
stable cwd + idle GC — `router.go` largely reimplements it), plus `sysprompt`,
`statusline`, `client`. The new ambient pieces split cleanly into generic
(push down to acp-kit; **poe-acp benefits too**) vs Slack-specific (stay here).

### Push down to acp-kit (generic, reusable)

- **Membership / `Known(key)`** — disk-backed "do we already have a session for
  this conv?" Belongs in `state.Manager`; poe-acp wants the same resume signal.
- **Per-conv checkpoint** — an opaque persisted string per key
  (`last processed external event id`). Generic; Slack stores a `ts`, another
  relay stores whatever. Manager owns persistence under the cwd; the relay
  owns the semantics.
- **Abstain sink** — a sink wrapper that suppresses output when the full stream
  equals a sentinel. Any relay may want an agent to decline (poe-acp already
  has a "discard response" notion for out-of-band turns).
- **Conv-keyed system prompt** — generalise `SystemPrompt string` →
  `func(ConvKey) string`. Enables per-channel persona here and per-conversation
  prompts in poe-acp.

### Stay in slack-acp (transport-specific)

- Slack `ts` semantics, `conversations.replies` backfill.
- `message.channels` event subscription + scope wiring.
- Slack mrkdwn formatting contract (already in `sysprompt`).

### Net

Most of ambient-threads is a small extension of `acp-kit/state` (membership +
checkpoint + abstain + conv-keyed prompt) plus a thin Slack transport layer.
Doing it in acp-kit pays off twice — poe-acp picks up membership, checkpointing,
and abstain for free.
