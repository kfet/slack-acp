# Backlog

Deferred work, with the reason it was deferred. Not a roadmap.

## Real workspace search via a relay-held user token

**Status:** deferred — blocked on the result-set enforcement below.

`slack_search` today is a bounded local fanout over
`conversations.history` (see
[`docs/agent-slack-access.md`](docs/agent-slack-access.md)). It has no
index: allowed channels only, one page per channel, a day window. Real
`search.messages` would replace the scan with a workspace-wide query.

**Shape.** `SLACK_USER_TOKEN` (`xoxp-`, scope `search:read`) held by the
relay only. Same mediated pattern as the bot token: scrubbed from the
agent's environment by name *and* by literal value
(`internal/config/agentenv.go`), never passed to a tool, every call made
by the relay with its own client.

**What it buys.** Genuine search across the workspace — no day window, no
per-channel page cap, no fanout cost, and messages older than the scan
horizon become findable. Removes the "absence of results is not absence
of the message" caveat.

**What it costs.** A user token carries the *operator's* identity and
*full* visibility: every DM, every private channel, every conversation
the bot was never invited to. It is a strictly larger credential than the
bot token, and Slack scopes it per-user, not per-channel.

**Blocking design requirement.** `allowed_channel_ids` must be enforced
on the **result set**, not just on the query. `search.messages` takes an
`in:` modifier, but that is a *hint the caller supplies* — anything that
influences the query string (agent-chosen `query`, Slack's own operator
parsing, a future API change) can widen it. Filter every returned match
by `channel.id ∈ allowed_channel_ids` after the call, drop the rest, and
treat a match in a DM or an uninvited private channel as a hard drop, not
a warning. Without that, search is an exfiltration path around the entire
allowlist: it reaches everything the operator can see, on behalf of
anyone who can talk to the bot.

Corollaries once the above is in place:

- Gate to the `read` scope (`agent_slack_access` ∈ {`read`,
  `read_write`}), same as the other read tools.
- Separate config key so it is opt-in and cannot arrive by upgrade —
  e.g. `slack_user_token` / `agent_slack_search: "workspace"`, defaulting
  off. Absent token ⇒ fall back to the local fanout, not to an error.
- With an empty `allowed_channel_ids`, "wherever the bot already is" no
  longer bounds anything. Either require a non-empty allowlist for this
  mode, or intersect results with `users.conversations` (the bot's own
  membership) first.
- Spend the shared read budget per call, and keep the `truncated`/`note`
  envelope so the two backends have one result shape.
- The manifest pin (`internal/slackproto/manifest_test.go`) forbidding
  `search:*` **bot** scopes stays regardless — the user token is not a
  bot scope, and nothing should ever put one on the app.
