# Self-verification

`slack-acp verify` drives real messages through real Slack and asserts
that every inbound path behaved as designed. One command, no
interactive input, PASS/FAIL/SKIP per named check.

```
slack-acp verify --public-channel C0BNJ53E1SQ --private-channel C0BMZ3ANC5V
```

It is meant to be run on the deployment host, against the running unit,
by an operator or an automated relay — nobody has to type in Slack.

## Why it needs a user token

Six of the seven checks post as a **human**, using a Slack user token
(`xoxp-`). This is not a convenience; it is the entire design.

`app_mention` carries an absolute guard in `internal/slackproto`:

```go
if ev.BotID != "" || ev.User == c.botUserID || ev.User == "" || ev.Edited != nil { drop }
```

There is deliberately no escape hatch on this path. The relay posts its
replies *as the same bot*, so any bot-authored mention that could
re-trigger the relay is a reply → trigger → reply loop with no natural
bound. The `self_drive_sentinel` hatch does not open it either.

The consequence is that **no bot-authored message can ever exercise
`app_mention`** — which is exactly why a real bug (the mention flag
being lost across the slackproto→handler boundary, fixed in v0.4.2)
survived to release and needed a human to find.

`chat.postMessage` with a user token produces a message from a genuine
human `user` — `user=U<the human>`, no `subtype`, no `edited` — delivered
over the same websocket, through Slack's own servers.

**But it is not enough on its own, and the reason matters.** Ground truth
from a live workspace, posting with a user token:

```
"user":    "U9EA2KLTH"      ← the human, as expected
"bot_id":  "B0BNE4AUS9L"    ← the posting app's own bot id, set anyway
"app_id":  "A0B3PMFHUSJ"
```

Slack stamps the posting app's identity onto **every** API message,
user token or not. There appears to be no way for any API caller to
produce a message Slack presents as "typed in a client". So `bot_id`
means *"sent through an app"*, not *"sent by a robot"* — and the guard
was using it as a proxy for the latter.

That proxy is what made `app_mention` untestable. The fix is to make
the guard precise rather than to weaken it; see below.

Rejected alternatives, for the record:

| Approach | Why not |
| --- | --- |
| Inject a synthetic Socket Mode envelope | Tests the dispatch code against our own *guess* of what Slack sends. Structurally blind to boundary bugs — the only class of bug found here so far. |
| Test-only ingest hook (build tag / env var) | A second front door. Turns "the guard is absolute" into "absolute unless X", and still never exercises Slack's delivery. Same trap the self-drive hatch falls into for `app_mention`. |
| A second bot app mentioning the first | Still dropped by `ev.BotID != ""` — correctly; two bots can loop through each other. |
| `username`/`icon_url` impersonation, incoming webhooks | `bot_id` is still set. |

**No test-only ingest affordance exists.** If the user token is absent,
the affected checks report `SKIP` with the reason — never a quiet pass,
and never a bot post substituted in.

## The guard, made precise

The guard has two clauses that are **not** equally negotiable:

```go
// 1. SELF-AUTHORSHIP — unconditional, no override, ever, evaluated first.
if user == "" || user == c.botUserID || edited { return ReasonBotAuthored }
if botID == "" { return "" }               // typed in a client: ordinary human

// 2. the bot_id PROXY — three conditions, ALL required:
if _, named := c.humanAuthors[user]; !named { return ReasonAPIAuthored }
if c.appID == "" || appID != c.appID       { return ReasonForeignApp }
if !c.humanAuthorRate.Allow()              { return ReasonHumanAuthorRateCap }
return ""
```

Clause 1 lives in `slackproto`, which runs strictly **before** the handler's
`allowed_user_ids` check — so no allowlist configuration can reorder it. There
is a test asserting the relay's own reply is still refused with *every*
override switched on at once, including its own id in the human list.

The three conditions in clause 2 each close a different hole:

| Condition | Hole it closes |
| --- | --- |
| author is named | any API post reclassified as human |
| **`app_id` is ours** | a *third-party* app posting as that user — a workflow or integration the person once installed — being trusted |
| rate cap (`human_author_per_minute`, default 30) | loop backstop of last resort, mirroring the self-drive hatch |

### Evidence — captured, not remembered

This key exists **because Slack always attaches app provenance to an API
post.** That claim was wrong the first time it was made from memory, and it
cost a release-blocking detour, so the raw capture lives here permanently.
Do not re-litigate it from recollection; re-measure it.

Captured off the wire from a Socket Mode connection (relay stopped, throwaway
probe owning the socket), for a `chat.postMessage` sent with an `xoxp-` **user**
token by a real human:

```json
{
  "type": "app_mention",
  "user": "U9EA2KLTH",
  "bot_id": "B0BNE4AUS9L",
  "app_id": "A0B3PMFHUSJ",
  "bot_profile": {
    "id": "B0BNE4AUS9L",
    "name": "slack-acp-test",
    "app_id": "A0B3PMFHUSJ",
    "team_id": "T9CF8H2EM"
  },
  "channel": "C0BNJ53E1SQ",
  "ts": "1786020999.037789",
  "text": "<@U0B3AAQ7HE3> RAWPROBE mention via USER token (auto-deleted)"
}
```

The `message` twin of the same post carries the same `bot_id` / `app_id` plus
`"channel_type": "channel"`. Neither event carries a `subtype`.

So a message authored by a human, posted through an app, is **indistinguishable
by `bot_id` alone** from one posted by a robot. That is the entire reason
clause 2 exists. Without `human_author_user_ids` the `app_mention` path cannot
be exercised by any automated means at all.

One token is charged per **message**, not per delivered envelope: Slack sends a
tagged message twice (`app_mention` + `message.channels`) and both reach the
guard, so charging each would make the configured number mean half what it
reads as. A `slack-acp verify` run costs about seven tokens.

**Why `app_id` and not `bot_id`.** Measured on the same workspace: an app's
user-token surface gets its **own** `bot_id`, distinct from its bot-token
`bot_id`, while both carry the same `app_id`.

```
BOT-token post   -> bot_id=B0B3VCV278U  app_id=A0B3PMFHUSJ  user=U0B3AAQ7HE3
USER-token post  -> bot_id=B0BNE4AUS9L  app_id=A0B3PMFHUSJ  user=U9EA2KLTH
```

So `bot_id` equality would have rejected the harness while *looking* correct.
`app_id` is the only field that identifies the posting app. slack-go does not
surface it on the typed event structs, so the relay reads it off the raw Socket
Mode envelope; it learns its own app id at startup via `bots.info` (no new
scope). **If that lookup fails the reclassification is inert** — a guard that
cannot verify its precondition refuses.

### The `edited` clause, measured

A mention introduced by *editing* an existing message is refused, for the same
reason the message path refuses subtypes: a mention must be an original post.
That had been asserted only against our own belief about what Slack sends —
the exact gap that let the original mention bug reach release — so it was
measured:

```json
{ "type": "app_mention", "user": "U9EA2KLTH",
  "edited": { "ts": "1786021953.000000", "user": "U9EA2KLTH" },
  "text": "<@U0B3AAQ7HE3> … a mention added by EDITING" }
```

`edited` is present, so clause 1 refuses it; the message twin arrives as
`subtype: message_changed` and is dropped by the subtype guard. The
`edited_mention_dropped` harness check now pins this against real Slack.

### A correctness note for whoever refactors this next

It is tempting to read the two distinct bot ids above and conclude that clause 1
could simply compare `bot_id` values — the relay's own posts carry
`B0B3VCV278U`, the harness's carry `B0BNE4AUS9L`, so they *look* cleanly
separable.

**Do not do that.** Clause 1 compares `user` against our bot's **user id**
(`U0B3AAQ7HE3`), and that is what makes it correct. It holds for every message
the relay could ever author, including ones with no `bot_id` at all, and it does
not depend on Slack's choice to mint separate bot ids per token surface — an
implementation detail we observed, not a contract we were given. Rewriting
clause 1 in terms of `bot_id` would make loop safety depend on that detail.
`TestSelfAuthorshipBeatsEveryOtherGate` and
`TestSelfAuthoredIsRefusedEvenWithTheAllowlistPopulated` exist to catch it.

Clause 1 is what makes a reply → trigger → reply loop **structurally
impossible**: the relay posts its replies as its own bot user, so it can
never act on its own message. It runs first and has no override — an
operator who writes the bot's own user id into the list changes nothing
(there is a test for exactly that).

Clause 2 is the proxy, and only it is overridable, via
`human_author_user_ids` in the config:

```json
{ "human_author_user_ids": ["U9EA2KLTH"] }
```

- **Empty (the default) is byte-for-byte the previous behaviour** and is
  the correct production setting.
- It cannot be on by accident: an operator must write a specific `U…`
  id down. Same trust level as `allowed_user_ids`.
- A non-empty list logs a loud startup warning.
- Listing a *bot's* user id would admit that bot — so don't. The relay
  can still never be driven by itself.

### What remains irreducible

A listed identity's messages are API-authored. At the Events API level
Slack offers no way to distinguish "this human typed it in a client"
from "an app posted it as this human". Naming an id in
`human_author_user_ids` accepts that: anyone holding a user token for
that person can drive the relay at machine speed. Note the loop is still
not self-sustaining — the relay's own replies can never re-trigger it —
so this is the same threat model as a human with a script, which the
guard was never meant to stop.

## One-time Slack setup (a human must do this in a browser)

1. Go to <https://api.slack.com/apps> and open the app.
2. **OAuth & Permissions** → **Scopes** → **User Token Scopes** (*not*
   Bot Token Scopes) → **Add an OAuth Scope** → `chat:write`.
3. A banner appears: *"You've changed the permission scopes… reinstall
   your app."* Click **reinstall**, review, **Allow** — **while logged
   in as the account the harness should post as.** Use a dedicated test
   user if the workspace can spare a seat; a user token can post as
   that person anywhere they are a member, so a test identity has a far
   smaller blast radius than the workspace owner.
4. Back on **OAuth & Permissions**, copy the **User OAuth Token**
   (`xoxp-…`).
5. On the deployment host, append it to the env file and confirm the
   mode:

   ```
   printf 'SLACK_USER_TOKEN=xoxp-…\n' >> ~/.config/slack-acp/env
   chmod 600 ~/.config/slack-acp/env
   ```

6. In Slack, invite that user to the test channels (both the public and
   the private one — a user token can only post where its owner is a
   member).
7. If the deployment sets `allowed_user_ids`, add that user's `U…` id,
   or every check fails at the allowlist rather than at the path under
   test.

Steps 2–3 are the only part that cannot be automated. The bot scopes
the harness needs (`chat:write`, `im:write`, `channels:history`,
`groups:history`) are already in
[`slack-app-manifest.json`](slack-app-manifest.json).

## Configuration

Tokens are read from the environment only — never from a flag, so they
never appear in `ps` output or shell history — and are never printed or
logged.

| Variable | Purpose |
| --- | --- |
| `SLACK_BOT_TOKEN` | `xoxb-…`, required. Bot-authored checks and cleanup. |
| `SLACK_USER_TOKEN` | `xoxp-…`. Absent → every human-authored check SKIPs. |
| `SLACK_VERIFY_PUBLIC_CHANNEL` | Default for `--public-channel`. |
| `SLACK_VERIFY_PRIVATE_CHANNEL` | Default for `--private-channel`. |

Flags:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--public-channel` | env | Public channel id. Required. |
| `--private-channel` | env | Private channel id. Empty → that check SKIPs. |
| `--unit` | `slack-acp` | systemd **user** unit whose journal carries the ingest records. |
| `--since` | `10 min ago` | `journalctl --since` window. |
| `--journal-cmd` | — | Override the journal reader with a **shell command line** (quoting preserved, e.g. `ssh host journalctl --user -u slack-acp --since "10 min ago"`). |
| `--self-drive-sentinel` | — | Empty → the self-drive check SKIPs. |
| `--timeout` | `3m` | Per-assertion wait budget. |

The bot must be a member of both channels.

### Preconditions

- **The running relay must be v0.4.2 or newer.** The checks assert on the
  ingest journal, which older builds do not emit; against an older binary
  every check fails with "the relay may not have seen the message at all".
  Deploy first, then verify.
- **`ambient: true` must be set** for `ambient_thread_reply_known` to pass.
  With ambient off, an un-mentioned follow-up is *correctly* dropped, and
  that check will report FAIL — it is asserting the configured behaviour of
  an ambient deployment, not a universal invariant. The
  `ambient_thread_reply_unknown_dropped` check passes either way.
- The harness must run where it can read the relay's journal — the same
  host, or with `--journal-cmd` pointed somewhere that can (`ssh host
  journalctl --user -u slack-acp …`).
- **journald rate limiting** (`RateLimitIntervalSec` / `RateLimitBurst`) can
  discard log lines under load. A discarded ingest record reads as "the relay
  never saw it" — a false FAIL, never a false pass. If checks fail
  inexplicably under load, check `journalctl --user -u slack-acp | grep -i
  suppressed`.

### Known limits

- **Assertions are keyed on `(channel, ts)`**, which is unique in Slack. A
  concurrent second run, or an earlier run inside the `--since` window,
  cannot satisfy this run's assertions.
- **Drop checks have a residual race.** Slack delivers a tagged message as
  *two* independent envelopes (`app_mention` and `message.channels`), so a
  `run` decision on the second path can in principle land after the harness
  has observed the `drop` on the first. The harness re-reads the journal once
  after the drop appears, which narrows the window to milliseconds but does
  not close it. A drop check therefore has a small chance of a false PASS
  under adversarial timing; a *deliver* check has none.
- **A check is only as real as its author.** Checks that post with the user
  token exercise the production guards exactly as a human does. Checks that
  post with the bot token (`bot_echo_dropped`, `self_drive_hatch`) are equally
  real, because being bot-authored is the property under test.

## The checks

| Name | What it does | Expected |
| --- | --- | --- |
| `app_mention_public` | User token posts `<@BOT> …` top-level in the public channel | delivered on `app_mention`/`mention`, prompt run, bot replies |
| `ambient_thread_reply_known` | User token posts an **un-mentioned** reply in the thread the previous check created | delivered on `message_channel`/`ambient_thread_reply`, prompt run, bot replies |
| `app_mention_private` | Same as the first, in the private channel | as above |
| `dm` | Bot opens the IM with the test user; user token posts into it | delivered on `message_im`/`dm`, prompt run, bot replies |
| `ambient_thread_reply_unknown_dropped` | User token starts an unrelated thread (no mention) and replies to it | **dropped** with `ambient_unknown_thread`, no reply |
| `edited_mention_dropped` | User token posts *without* a mention, then `chat.update`s one in | **dropped** (`bot_authored`), no reply |
| `bot_echo_dropped` | **Bot token** posts `<@BOT> …` into its own thread | **dropped** (`bot_authored` / `api_authored` / `self_drive_not_accepted` / `self_posted_ts`), no reply |
| `self_drive_hatch` | Bot token posts a sentinel-prefixed message | delivered on `self_drive`, prompt run, bot replies |

Everything the harness posts carries a per-run nonce and is deleted on
the way out — including the relay's own replies, which are deleted with
the bot token (Slack only lets a token delete its own messages).

## How a check asserts — and why it takes both halves

Every check asserts on **both**:

1. **The ingest journal** (`internal/journal`) — the relay's own record
   of what it decided about that exact `ts`, and why.
2. **Slack thread state** — `conversations.replies`.

The journal half is load-bearing for the negative checks. "The bot must
NOT answer this", asserted by silence alone, is unfalsifiable: it passes
just as happily when the relay is down, misconfigured, or still booting.
Waiting for the *drop record for that specific ts* turns it into
positive evidence that the relay saw the message and deliberately
declined it. Conversely, a journalled prompt with no reply in the thread
catches a silent failure downstream in the agent or the streamer — which
neither half would catch alone.

## The ingest journal

Every inbound event produces one line per stage at normal log level:

```
SLACK-ACP-INGEST {"stage":"slackproto","path":"app_mention","decision":"deliver","reason":"mention","channel":"C0BNJ53E1SQ","ts":"1754487001.123456","thread_ts":"…","user":"U…"}
SLACK-ACP-INGEST {"stage":"handler","path":"app_mention","decision":"run","reason":"prompt","channel":"C0BNJ53E1SQ","ts":"1754487001.123456",…}
```

This is a production observability feature, not a test hook: it answers
"why didn't the bot reply?" without turning on debug mode and
re-provoking the problem.

```
journalctl --user -u slack-acp | grep SLACK-ACP-INGEST
```

`decision` is `deliver` (slackproto passed it on), `run` (the handler
started a prompt), or `drop`. `reason` is drawn from a fixed vocabulary
— see the constants in `internal/journal`. Field names and the
path/decision/reason vocabularies are a **stability contract**: adding
values is fine, renaming one breaks the verifier and every operator
grep.

**Message text is deliberately not recorded.** The journal carries
routing metadata only, so enabling it can never spill conversation
content — or a token someone pasted into a thread — into journald.
