# Catch-up

Socket Mode delivers events only while a websocket is open. A crash, a
restart, a host reboot or a dropped connection opens a gap, and Slack
never replays what was posted in it. The catch-up closes that gap.

It mirrors zulip-acp's catch-up (`internal/catchup` + `handler.CatchUp`
there), adapted to Slack's read APIs.

## What it does

Every time Socket Mode connects — the first connect at startup and every
reconnect after — the relay:

1. Lists the conversations the bot is a member of
   (`users.conversations`, types `public_channel,private_channel,im`;
   archived excluded).
2. For each, reads forward from that conversation's checkpoint:
   - `conversations.history` with `oldest=<checkpoint>`, following
     cursors — top-level posts, DMs, and thread parents posted in the gap;
   - `conversations.replies` for every thread parent it saw whose
     `latest_reply` is past the checkpoint;
   - `conversations.replies` for every thread the relay already has a
     session for in that channel (`router.KnownThreads`), parent no older
     than 30 days before the checkpoint — a reply in an older thread is
     invisible to `conversations.history`.
3. Routes each missed message through **the same gates as a live event**
   (`slackproto.ingestMention` / `ingestMessage`; the journal records
   carry `"catch_up": true`). A channel message that mentions the bot takes
   the `app_mention` path, as it would have live.
4. Hands one conversation's survivors to `handler.CatchUp`, which applies
   the handler's gates (self-authorship, allowlists, ambient known-thread)
   and **collapses each thread into one turn**: a thread that had five
   messages while the relay was down gets one answer, not five turns that
   supersede each other. The prompt is

   ```
   [catch-up] 3 messages arrived while the relay was offline (first at 2026-10-28T09:14:02Z)

   first message

   second message
   …
   ```

   In ambient mode each line carries its sender, as a live ambient prompt
   does. At most the newest 50 lines per thread are kept; the header
   counts the rest. Relay commands (`!…`) and slash commands (`/…`) are
   not run late — they were typed for a relay that was not there — and are
   journalled as `catch_up_command`. Messages older than
   `catchup_max_age_seconds` (default 24h) are not answered
   (`catch_up_too_old`); each thread that had any gets one notice:
   `[catch-up] 2 messages older than 24h arrived while the relay was
   offline and were not answered.` At most 4 catch-up turns run at once,
   and a catch-up turn is dropped if a live message has meanwhile started
   a newer turn in its thread — it must never cancel that newer turn.
5. Advances the conversation's checkpoint to the newest ts it read.

## Checkpoints and exactly-once

The checkpoint is one Slack ts per conversation, in
`<state_dir>/catchup.json`. Slack ts values increase in posting order
within a conversation — thread replies included — so one ts covers a
channel's top-level posts and all of its threads.

- **First run.** With no `catchup.json`, the relay writes one whose
  *floor* is "now". A DM with no checkpoint of its own reads from the
  floor, so nothing posted before the relay was installed is replayed.
- **New channels.** A channel with no checkpoint of its own — every
  channel on the first run, or one the bot was invited to since — is not
  read: its checkpoint starts at "now". Its history predates the bot's
  membership.
- **Every live message** the relay sees advances its conversation's
  checkpoint, delivered or dropped: it never needs reading again, and on a
  clamped install re-reading a busy channel's chatter would make every
  catch-up slow. Not while a run is in flight — that would skip what the
  run is about to read.
- **During a run**, live events are acknowledged as usual but **held**.
  When the run ends they are released against the checkpoints it left
  behind: an event at or below its conversation's checkpoint is a message
  the run already read and is dropped (journal reason `catch_up_seen`);
  anything newer is delivered. The checkpoints are snapshotted before the
  release, so two held events arriving out of order are both delivered.
- A connect that arrives during a run schedules one more run after it.
- A conversation whose read fails keeps its checkpoint and is read again
  on the next connect. A live message in it advances the checkpoint past
  the gap, as in zulip-acp — the catch-up is best effort, never a reason
  to hold live traffic.

## Slack rate limits

`conversations.history` and `conversations.replies` are where the cost is.

| App type | Limit | Page size |
| --- | --- | --- |
| Internal (customer-built) or Marketplace | Tier 3, ~50 calls/min | up to 1000 |
| Commercially distributed, not on the Marketplace | **1 call/min** | **15** |

The second row applies to new installs since 2025-05-29 and to all
existing installs since 2026-03-03 (Slack changelog, "Rate limit changes
for non-Marketplace apps"). `users.conversations` is Tier 2/3.

The relay asks for full pages (`limit=200`) and follows cursors, so it is
correct under either regime — under the clamp it is merely slow. On a
`429` it waits the `Retry-After` the response carries (1s if none), then
retries, up to 5 times per call; a conversation that exhausts that is
given up for this run and keeps its checkpoint. One conversation is read
at a time, never in parallel. Each conversation is capped at 1000 missed
messages (the newest are kept, a WARN names the cut).

### Measuring it

Every run logs what it cost:

```
slack-acp: catch-up: 4 conversation(s), 0 failed, 7 new message(s); 9 API call(s), 0 rate-limited, waited 0s; took 1.2s
```

`API call(s)` vs `rate-limited` vs `waited` is the measurement: on an
internal app a short outage costs one `users.conversations` call plus one
history call per conversation (plus one replies call per active thread)
and no 429s. Under the non-Marketplace clamp, expect (from the published
limit, not yet observed on a clamped install) about one 429 and ~60s of
waiting per history/replies call after the first each minute. `took` is the wall time the
live path was held for. Read it with

```
journalctl --user -u slack-acp | grep 'catch-up:'
```

The unit tests pin the accounting (`internal/catchup`
`TestRunWaitsOutRateLimits`); `slack-acp verify` exercises the path
against a real workspace (below).

## Scopes

No new scopes: `users.conversations` needs `channels:read`, `groups:read`,
`im:read`; the reads need `channels:history`, `groups:history`,
`im:history`. All were already in `docs/slack-app-manifest.json`. mpim is
not read, because the app subscribes to no mpim events and does not answer
there live either.

## Configuration

On by default. `catchup_max_age_seconds` (default 86400) is the age
bound; an explicit `0` turns the catch-up off. Same knob and semantics as
zulip-acp.

## Verifying it

`slack-acp verify --outage-stop-cmd 'systemctl --user stop slack-acp'
--outage-start-cmd 'systemctl --user start slack-acp'` stops the relay,
posts an `@`-mention and an `@bot !status` as the test user, starts the
relay, and asserts that the mention is answered once with `catch_up`
records (`catch_up_mention_answered`) and that the command is journalled
`catch_up_command` and not run (`catch_up_command_skipped`).
`catch_up_seen` needs a live event to race a run and cannot be provoked
deterministically from outside; it is covered by
`internal/slackproto` `TestCatchUpHoldsLiveEventsAndDeliversExactlyOnce`.
`catch_up_too_old` needs an outage longer than the age bound; it is
covered by `internal/handler` `TestCatchUpTooOldGetsANotice`.

## Limits

- A reply in an old thread the bot was **never** summoned into is not
  read: no Slack API lists "threads with new replies" to a bot token. That
  matches the live behaviour, which drops such replies anyway — except for
  a fresh `@`-mention inside one, which a catch-up cannot see.
- A channel the bot was invited to *during* an outage starts at the next
  connect, so a mention posted with the invite in the gap is not answered.
- Message edits made during the gap are read in their edited form, as the
  original post (live, the original was delivered and the edit ignored).
- The existing ambient `backfill` (per-thread `last_ts` gap fill on the
  next live message) still applies to live messages; a catch-up turn
  skips it, because it already carries the missed lines.
