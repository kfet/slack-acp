# Channel memory via ancestor `AGENTS.md`

> **Status**: backlog — nothing to build in `slack-acp`; this is a
> convention plus a docs/config change. Written down because the
> mechanism is non-obvious and the failure modes are real.

## The want

An agent that lives in a channel should remember the channel's standing
preferences — "we always use bullet points here", "deploy means staging,
never prod", "ping @carol before touching billing" — stated once, in
natural language, and honoured in *every* thread in that channel
thereafter.

Anthropic's Slack agent implements exactly this as **one markdown file
per channel**. We get it for free.

## The mechanism

`fir` loads project context files by walking **from `cwd` up to the
filesystem root**, collecting the first `AGENTS.md` (or `CLAUDE.md`) at
each level, ordered root-first so deeper files take precedence
(`pkg/resources/resourceloader.go`, `loadProjectContextFiles`). Other ACP
agents (claude-code) do the same with `CLAUDE.md`.

slack-acp already spawns each thread's session with
`cwd = <StateDir>/threads/<channel_id>/<thread_ts>`. So the hierarchy
falls out of the existing layout with no code:

```
<StateDir>/threads/AGENTS.md                        workspace-wide prefs
<StateDir>/threads/<channel>/AGENTS.md              CHANNEL MEMORY
<StateDir>/threads/<channel>/<thread_ts>/AGENTS.md  thread-local override
```

Drop a file at the channel level and every existing *and* future thread
in that channel picks it up at session start. Because the agent has a
shell with that directory as an ancestor of its cwd, it can also write
the file itself when a user states a preference — which is the whole
point: memory stated in natural language, persisted for the team.

## What this is NOT

Standing preferences are **push** memory: always in the window, cheap,
small. They do not replace **pull** memory — "what did we decide about X
three threads ago" — which is cross-session recall and wants an agentic
grep over sibling thread dirs (see fir's `session-stitch` design). Do not
let `AGENTS.md` become an episodic log; it is a preferences file.

The distinction has to be enforced by instruction (a skill line), not by
mechanism. Nothing stops it becoming a junk drawer.

## Before enabling this

- **The walk goes all the way to `/`.** With the default
  `$XDG_STATE_HOME/slack-acp`, a stray `~/AGENTS.md` on the host leaks
  into every Slack thread, as does the agent's own global context file.
  Operators running shared channels should set `state_dir` somewhere
  clean (e.g. `/var/lib/slack-acp`) — which the sample config already
  suggests — and check what sits above it.
- **Precedence is deepest-wins**, so a thread-local file silently
  overrides channel policy. That is usually what you want; it is also a
  way for one participant to locally undo a channel rule.
- **No access control.** Any participant in a channel who can get the
  agent to write a file can rewrite the channel's memory. That is a
  capability question, not a memory question — it belongs with the
  permission work, not here.

## Work items

1. Document the layout in `README.md` (Setup) and in the "State
   directory & per-thread cwd" section of `docs/design.md`.
2. Recommend a non-`$HOME` `state_dir` for multi-person deployments, and
   say why.
3. Add a bundled skill line telling the agent that
   `../AGENTS.md` is the channel's standing-preferences file, that it
   may write to it when a user states a durable preference, and that
   episodic detail does not belong there.
4. Optional, later: a `!prefs` command to print the resolved chain, so
   users can see what the agent thinks the channel rules are.
