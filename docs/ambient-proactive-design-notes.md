# Ambient / proactive responses — design notes

Curated recap of the multi-person + proactive-response design for slack-acp:
how the bot handles a chat with **multiple people** and when it should
**respond unprompted**.

> **Provenance.** Distilled from the original design conversation on the
> `sea-fir` bot (host: sea-racknerd), Poe conversation
> `c-…kvs8l7kl8bg8k73vnrmiyt`, sessions spanning 2026-06-06 → 2026-06-14
> (anchor `2026-06-06T17-41-31…b2a5ba27.jsonl`). See also `docs/ambient-threads.md`.

## The premise

A bot that only answers when summoned is just a slash-command with extra steps.
The value is an agent that is **present** in the thread and chimes in when it
actually helps. So take "let it decide" seriously.

## Core decision: one path, the agent judges

- **No `off | addressed-only | helpful` knob.** An earlier proposal had three
  ambient modes plus a cheap pre-gate. Dropped.
- Every thread message the bot can hear goes through the **same path**. The
  agent replies, or emits a silent sentinel (`<<SILENT>>` / decline protocol).
- **The `@`-mention is a strong hint, not permission.** "Helpful-when-unaddressed"
  and "answer-when-tagged" become the same code path with different priors.
- `Addressed` stops being a branch and becomes just **context** — one more field
  in the prompt, not a gate.

## Handling multiple people

The agent can only decide well if it sees what a human in the thread sees:

- **Full thread context, always.** Feed every message through the session even
  on abstain, so by the time something *is* for it, it has been following along.
  A judge that only sees the current line misfires constantly.
- **Participants + sender + tagged handles** travel in the message text, so the
  agent can tell "addressed to me" from "addressed to Bob."
- **Its own recent activity** — did it just speak? Is the thread a mid-exchange
  between two humans it should stay out of?
- **The session *is* the thread memory** — no separate thread-context plumbing.

## Making "proactive" affordable

- Every ambient message = one LLM turn just to possibly say nothing. In a busy
  thread that is real money and latency.
- Not a reason to gate the *decision* — a reason to make the **abstain path
  cheap**: short max-tokens, no eager placeholder, optionally a fast/cheap model
  for the silent-or-answer call.

## Which threads does it listen to?

The only real gating question, and it answers all the edge cases:

> **A thread is ours iff its session dir exists on disk**
> (`threads/<channel>/<thread_ts>`). The `@`-mention is just what creates it.

Restart recovery, missed updates while down, "never got the first tag" — all
fall out of this single fact.

## Mechanics (Slack)

- Slack emits `AppMentionEvent` only on a real `@bot` tag.
- Un-tagged thread replies arrive as `MessageEvent` and were dropped for non-DMs.
- Fix: in `slackproto`, deliver non-DM `MessageEvent`s that are **thread replies**
  (`ThreadTimeStamp != ""`) from a human, **pre-filtered** to threads we already
  have a session for (`router.Known(key)`), so the bot doesn't ingest every
  channel message.
