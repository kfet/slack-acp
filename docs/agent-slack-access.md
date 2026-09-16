# Agent Slack access

The spawned ACP agent can reach Slack beyond the thread it is answering
in — read another thread, read a channel, list the bot's channels, and
(opt-in) post as the bot — through a **relay-hosted MCP server**. It
never receives a Slack token.

This document covers the threat model, why the capability is mediated
rather than delegated, the tool surface, the config knob, and the OAuth
scopes it needs.

## Threat model

The agent is a general-purpose, tool-using process. In ambient threads
it is driven by text from people who are **not** the operator: anyone in
a channel the bot was invited to can put arbitrary instructions in front
of it. Treat every message body as attacker-controlled.

Two consequences shape the design.

**A token is not revocable in practice.** If the agent could read
`SLACK_BOT_TOKEN`, an injected instruction would let it post as the bot
anywhere the app can reach, read any channel the app can see, and call
API methods nobody audited — with no prompt-level guard able to take any
of it back. `internal/config/agentenv.go` therefore scrubs
`SLACK_BOT_TOKEN` and `SLACK_APP_TOKEN` (both by variable name and by
literal value, so a token copied into a bespoke variable is dropped too)
from the agent's environment. **That scrub is load-bearing. Do not
weaken it.**

**A tool call is.** Every call in this feature is made by the *relay*
process, with the relay's own `*slack.Client`, after the relay has
checked its own policy. The agent sends arguments; it does not send
credentials, and it cannot make a call the relay declines. That is the
whole difference between mediated and delegated access.

### The capability you are actually granting

Read access is not "nothing". **Inviting the bot to a channel makes that
channel's history readable by anyone who can talk to the bot**, because
they can ask the agent to read it and summarise it back into a thread
they can see. This is inherent: the relay cannot distinguish "the
operator wants a summary of #ops" from "a stranger talked the agent into
one".

If that is not acceptable in your workspace, set `allowed_channel_ids`.
It is enforced on **every** tool call, including the channel listing — a
channel outside the allowlist is not readable and its ID is not even
disclosed.

**DMs are excluded from the default reach.** With no allowlist set, the
tools reach the channels the bot is in *minus* direct messages (`D…`).
Without that subtraction, "nothing the bot cannot already see" would be
literally true and still wrong: the bot is in a 1:1 DM with every person
who has ever messaged it, and the audience for a DM's contents is one
person, not everyone who can prompt the agent in a channel. Group DMs
are out of reach by a different route — the app has no `mpim:history`
scope. An operator who names a `D…` id in `allowed_channel_ids` has said
so deliberately, and that wins.

In `read_write` mode you additionally grant **bot-voiced messages to
audiences the requester may not reach**. A convincing fake ops alert in
`#general`, carrying the bot's authority, is within reach of anyone who
can prompt the agent. That is why `read_write` is opt-in and `read` is
the default.

## What is mitigated, and how

| Attack | Mitigation |
| --- | --- |
| Read/post outside the operator's intent | `allowed_channel_ids` enforced on every call; denial names the channel and is logged |
| One user's DM with the bot surfacing in another user's thread | With no allowlist, `D…` conversations are refused outright; `mpim:history` is not granted |
| Channel-listing as a targeting oracle | Listing is intersected with the allowlist, and reports only `id` + `name` — never `is_private` |
| Context flooding / channel dump | `limit` clamped to 100 server-side; each message truncated to 2000 runes |
| Search as an unbounded history walk | `slack_search` scans allowed channels only, one 100-message page each, within a ≤30-day window, ≤50 channels per call — and labels every truncation |
| Slow-drip history enumeration across many calls | Shared read rate cap (60/min) across all read tools; a `slack_search` fanout spends one token per channel scanned |
| Impersonation via webhook `username` | Bot-authored messages are flagged `is_bot` in read output |
| Post into a channel nobody invited the bot to | The app has **no** `chat:write.public` scope, so Slack itself rejects it |
| `@channel` / `@here` / user-group mass pings | Stripped from posted text in every syntactic form Slack accepts, including the labelled `<!here\|@here>` and `<!subteam^…>` forms |
| Agent posting a self-drive trigger | A post beginning with `self_drive_sentinel` is refused; the sentinel is scrubbed from anything that does go out; the posted `ts` is recorded in the self-posted memory |
| Post spam / runaway loop | Global token bucket, `agent_posts_per_minute` (default 10) |
| A silently short answer read as "it does not exist" | Every bound — match limit, read budget, channel fanout, thread cap, an unreadable thread, and a channel listing longer than one page — sets `truncated` plus a `note` |
| Untraceable action | Every call logs tool, session key (`channel/thread_ts`), target channel, and outcome — including denials |

### Residual, by design

- **A summary of a channel the bot is in can reach anyone who can prompt
  the agent.** Bounded only by `allowed_channel_ids`.
- **In `read_write`, plausible bot-voiced text inside an allowed
  channel.** Irreducible once posting is permitted at all.
- **Read results are agent-visible text.** Anything written in a
  readable channel becomes input to the agent. Read mode makes every
  such channel a write-primitive into the agent's context.
- **Rate caps are global, not per-thread.** One busy thread can consume
  the budget. Acceptable: the caps exist to bound runaway behaviour, not
  to apportion fairness.

## Tool surface

Served as MCP server `slack`, spawned by the agent over stdio.

| Tool | Slack method | Mode |
| --- | --- | --- |
| `slack_read_thread(channel, thread_ts, limit?)` | `conversations.replies` | read |
| `slack_read_channel(channel, limit?, oldest?)` | `conversations.history` | read |
| `slack_list_channels()` | `users.conversations` | read |
| `slack_search(query, channel?, limit?, days?, include_threads?)` | `conversations.history` (+ `conversations.replies`) fanout | read |
| `slack_post(channel, thread_ts?, text)` | `chat.postMessage` | read_write |

Read tools return JSON: `ts`, `user` (resolved to a display name
relay-side, cached), `is_bot`, `text`, `thread_ts`. User IDs are
resolved by the relay precisely so there is no user-lookup tool — that
would double as a workspace enumeration primitive.

`slack_list_channels` also serves as name→ID resolution, so there is no
separate lookup tool for that either. It returns an *object* — `{
truncated, note?, channels[] }` — not a bare array, so a listing cut
short by `users.conversations`' single page says so. It is deliberately
one page and never walks the cursor: paging would turn "list my
channels" into an unbounded enumeration primitive.

### Search is a bounded local scan, not workspace search

`slack_search` exists, but it is **not** Slack's search. Slack's
`search.messages` (and `search.all` / `search.files`) accept **user
tokens only** — `search:read` cannot be granted to a bot token. Adding
an `xoxp-` user token would mean holding a strictly larger credential
than the bot token we already go out of our way to withhold from the
agent, so that route was declined (see [BACKLOG.md](../BACKLOG.md) for
what it would take).

Instead the relay fans out `conversations.history` over the channels the
session may already read and matches the substring itself. Consequences,
all of them load-bearing:

- **Scope is exactly `allowed_channel_ids`** — the same set
  `slack_list_channels` discloses and the read tools enforce. Search
  never widens it. With `channel` set, the ID goes through the ordinary
  allowlist check first.
- **No index.** Only the last `days` days (default 7, capped at 30), only
  one `conversations.history` page (100 messages) per channel, at most 50
  channels per call. Thread replies are scanned only with
  `include_threads`, and then at most 20 threads per call.
- **Rate-budgeted as a fanout.** Each channel scanned, and each thread
  fetched, spends one token from the shared 60/min read budget — a search
  costs N reads, not 1. It cannot bypass or silently blow the cap.
- **Truncation is always explicit.** Every bound above, when hit, sets
  `truncated: true` and a `note` naming the reason (match limit, read
  budget exhausted, too many channels, thread cap). Running out of budget
  mid-fanout returns the partial result plus the note rather than an
  error — but never a quietly short answer.
- **Same hygiene as the read tools.** Bot-authored messages are flagged
  `is_bot` (an attacker-chosen webhook `username` must not read as
  "operator"), bodies truncate at 2000 runes, user IDs resolve
  relay-side, and thread parents repeated by `conversations.replies` are
  deduplicated.

The tool description says all of this to the agent in as many words,
because the dangerous failure mode is an agent treating "no results" as
"the message does not exist".

A manifest test still pins that **no `search:*` scope** appears in the
app — that invariant is unchanged and is the security-relevant one. A
package test pins the other half: `slack_search` is registered *and* the
relay's Slack client interface carries no search method, so no code path
here can reach a `search.*` API.

## How it works

```
agent ──stdio──▶ `slack-acp mcp-serve` ──unix socket──▶ relay process ──▶ Slack
                 (dumb redirector)      (token auth)     (owns the client)
```

1. The relay creates an `acp-kit/mcphost` Host on a unix socket in a
   private `0700` directory (socket itself `0600`).
2. For each ACP session it mints a fresh random token bound to that
   session's key (`channel/thread_ts`) and advertises a stdio MCP server
   whose command is **the relay binary itself** with the `mcp-serve`
   subcommand, passing the socket path and token via env.
3. In `mcp-serve` mode the binary is a dumb pipe: it dials the socket,
   writes a one-line token preamble, and copies stdin↔socket. It has no
   MCP knowledge and holds no Slack credentials.
4. The relay runs the MCP state machine and resolves the token to a
   session key **server-side**. The agent never sends its session key,
   so it cannot claim to be a different thread.

Code: `internal/slackmcp/` (identity, tool schemas, `Relay`
implementation), wired in `cmd/slack-acp/main.go`. Transport lives
upstream in [`acp-kit/mcphost`](https://github.com/kfet/acp-kit), shared
with `poe-acp`.

## Configuration

```json
{
  "agent_slack_access": "read",
  "agent_posts_per_minute": 10,
  "allowed_channel_ids": ["C01234567"]
}
```

- `agent_slack_access`
  - `"off"` — no MCP server is registered at all. The agent is offered
    no Slack tools.
  - `"read"` *(default)* — read tools only. Exposes nothing the bot
    cannot already see, and no secret leaves the relay.
  - `"read_write"` — additionally exposes `slack_post`.
  - Any other value is a config **error** at load time; a typo must not
    silently select a different security posture.
- `agent_posts_per_minute` — global `slack_post` cap. Default 10.
  Only meaningful in `read_write`.

The read cap (60/min, shared across the read tools) is a constant,
not a knob — it exists to bound enumeration, and an operator who wants
more reach should widen `allowed_channel_ids` instead.

## OAuth scopes

This feature **adds two bot scopes** to
[`docs/slack-app-manifest.json`](slack-app-manifest.json):

- `channels:read` — public channels in `users.conversations`
- `groups:read` — private channels in `users.conversations`

Everything else it needs (`channels:history`, `groups:history`,
`im:history`, `chat:write`, `users:read`) was already granted.

> **Existing installs must reinstall the app.** Slack applies scope
> changes only at install time. Until you go to api.slack.com/apps →
> your app → **Install App** → *Reinstall to Workspace*,
> `slack_list_channels` will fail with `missing_scope`. Nothing else
> regresses.

`chat:write.public` is deliberately **not** requested, and a test
enforces its absence: bot membership is what bounds where an
agent-initiated post can land.
