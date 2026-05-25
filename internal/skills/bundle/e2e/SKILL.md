---
name: e2e
description: Run and extend slack-acp's end-to-end smoke tests — black-box exercises of the real slack-acp binary against a mock Slack Socket Mode server (`fakeslack.py`) and a scriptable fake ACP agent (`fakeagent.py`). Use when changes touch the wire layers (Socket Mode framing, ACP framing, session lifecycle) where in-package unit tests aren't enough.
---

# e2e Skill

End-to-end testing for `slack-acp`. The bot has three wire surfaces:

```
Slack ── ws ── slackproto ── handler ── router ── acp-kit/client ── stdio ── agent
```

Unit tests in `internal/*/...` cover each layer in isolation with
fakes. **e2e** drives the *real* `slack-acp` binary as a black-box
subprocess, with both the Slack side and the agent side faked out, to
catch wiring bugs in-package tests can't see:

- Socket Mode envelope quirks (ack, retry, message vs app_mention).
- ACP `initialize` handshake edge cases.
- Process-lifecycle bugs (signal handling, child cleanup, restart).
- State-dir layout regressions (per-thread cwd, reuse across restart).

If a unit test in `internal/handler/handler_test.go` *could* express
the case, write it there. e2e is for "the binary itself", not handler
logic.

## Layout

The harness is **all scripts** — no Go test plumbing, no build tags,
no `test/e2e/` directory:

```
internal/skills/bundle/e2e/
  SKILL.md                    # this file (cases described inline below)
  scripts/
    ws.py                     # ~280 LOC RFC 6455 server, stdlib-only
    fakeslack.py              # mock Slack Web API + Socket Mode
    fakeagent.py              # scriptable ACP child
```

Tests are **described in this skill, executed ad hoc** by the agent
running them. There is no test runner — pick the case you care about
from the [Cases](#cases) section and run the recipe. Each case is
self-contained: start the fakes, drive the bot, assert on
`/control/messages`, tear down.

If you find yourself writing a `cases/*.sh` or `run.sh` — stop and add
the case to this file instead. The skill body is the test plan.

## Prerequisites

- `slack-acp` built: `make build` (or `go build -o bin/slack-acp ./cmd/slack-acp`).
- macOS `python3` (ships with the OS — `python3.9`).
- `internal/slackproto` honouring `SLACK_API_BASE` env var (already
  wired; see [`slackproto.go`](../../../internal/slackproto/slackproto.go)).
- The fakes at `internal/skills/bundle/e2e/scripts/{ws,fakeslack,fakeagent}.py`
  are stdlib-only; nothing to install.
- `tmux-driver` skill loaded — long-lived processes (`fakeslack`,
  `slack-acp`) run in tmux windows so each agent step is a quick
  non-blocking `curl`, not a backgrounded shell that ties up the turn.

## How the wiring works

```
                    SLACK_API_BASE
                        │
slack-acp ──────────────┴────────────► fakeslack HTTP  (Web API)
   │                                       │
   │  ws://… (returned by                  │ /control/* (test driver)
   │   apps.connections.open)              │
   ▼                                       │
fakeslack WS  ◄─────────────────────────── │  (POST /control/send → push event)
   ▲
   │ stdio ACP (newline-delimited JSON-RPC)
   ▼
fakeagent.py
```

- `SLACK_API_BASE` redirects slack-go's Web API base from
  `https://slack.com/api/` to `http://127.0.0.1:NNN/api/`.
- `apps.connections.open` returns fakeslack's local `ws://` URL, so
  Socket Mode dials our fake too. No TLS, no `/etc/hosts` tricks.
- The test drives Slack-side events via `POST /control/send` and
  asserts on the recorded outbound calls via `GET /control/messages`.

## Drive the harness via tmux

The boilerplate every case shares. **Each step is a separate
foreground `Bash` call**; tmux holds the long-lived processes so
nothing blocks the agent's turn.

### 1. Build the binary (once per code change)

```sh
cd "$(git rev-parse --show-toplevel)"
go build -o bin/slack-acp ./cmd/slack-acp
```

### 2. Start fakeslack in its own tmux window

```sh
source "$SKILL_DIR/scripts/auto-helpers.sh"   # SKILL_DIR = tmux-driver skill dir
tm-new slack-acp-e2e fakeslack
tm-send slack-acp-e2e "cd $(pwd) && exec internal/skills/bundle/e2e/scripts/fakeslack.py --print-urls"
tm-wait slack-acp-e2e '^http=' 5
URLS=$(tm-capture slack-acp-e2e 50)
HTTP=$(printf '%s\n' "$URLS" | grep -m1 ^http= | cut -d= -f2-)
BASE=${HTTP%/api/}
echo "$HTTP" > /tmp/slack-acp-e2e.http   # stash for later steps
echo "$BASE" > /tmp/slack-acp-e2e.base
```

### 3. Start slack-acp in a second window

```sh
HTTP=$(cat /tmp/slack-acp-e2e.http)
STATE=$(mktemp -d) ; echo "$STATE" > /tmp/slack-acp-e2e.state
STATUS=$(mktemp)   ; echo "$STATUS" > /tmp/slack-acp-e2e.status
tm-win slack-acp-e2e bot
tm-send slack-acp-e2e "cd $(pwd) && SLACK_BOT_TOKEN=xoxb-test SLACK_APP_TOKEN=xapp-test SLACK_API_BASE='$HTTP' exec ./bin/slack-acp --agent-cmd '$(pwd)/internal/skills/bundle/e2e/scripts/fakeagent.py --script reply-once --status-file $STATUS' --state-dir '$STATE'"
```

### 4. Wait for the WS handshake (deterministic)

```sh
BASE=$(cat /tmp/slack-acp-e2e.base)
for i in $(seq 1 50); do
  curl -s "${BASE}/control/connected" | grep -q '"connected": true' && break
  sleep 0.1
done
curl -s "${BASE}/control/connected"   # should print {"connected": true}
```

### 5. Drive + assert (per-case; see [Cases](#cases))

```sh
BASE=$(cat /tmp/slack-acp-e2e.base)
STATUS=$(cat /tmp/slack-acp-e2e.status)
# inject + poll /control/messages or $STATUS — see each case below
```

### 6. Tear down

```sh
tm-kill slack-acp-e2e
rm -rf "$(cat /tmp/slack-acp-e2e.state)" \
       "$(cat /tmp/slack-acp-e2e.status)" \
       /tmp/slack-acp-e2e.{http,base,state,status}
```

### Inspect / debug

- `tm-capture slack-acp-e2e 200` — last 200 lines of whichever
  window is active.
- `tm-select slack-acp-e2e bot && tm-capture slack-acp-e2e 100` —
  bot logs.
- `tm-attach slack-acp-e2e` — print attach command for the user
  to watch live.

### Why tmux

- Each agent step (`tm-send`, `curl`, `tm-capture`) returns
  immediately. No backgrounded `&` jobs in a single Bash call,
  no risk of a hung subprocess freezing the turn.
- The user can `tm-attach slack-acp-e2e` and see fakeslack and
  bot logs scrolling live.
- Cleanup is one `tm-kill`; tmux reaps the whole tree.

Avoid wall-clock waits inside assertion loops — poll
`/control/*` or `$STATUS` with bounded retries, same rule as the
rest of the codebase
(see [AGENTS.md "Testing — avoid wall-clock timeouts"](../../../AGENTS.md)).

## fakeslack control plane

| route | purpose |
|---|---|
| `POST /control/send` | inject a Socket Mode event (`type`: `app_mention`, `message_im`, or `raw`) |
| `GET  /control/messages` | dump recorded `chat.postMessage` / `chat.update` calls |
| `GET  /control/connected` | `{connected: bool}` — true iff the bot's WS is up |
| `POST /control/clear` | clear the message log + ack set |

`POST /control/send` JSON shapes:

```json
{"type":"app_mention", "user":"U1", "channel":"C1", "text":"<@UBOT> hi", "ts":"100.0"}
{"type":"message_im",  "user":"U1", "channel":"D1", "text":"hi", "ts":"100.0", "thread_ts":""}
{"type":"message_im",  "user":"U1", "channel":"D1", "text":"hi", "ts":"100.0", "bot_id":"B1"}      // bot echo
{"type":"message_im",  "user":"U1", "channel":"D1", "text":"hi", "ts":"100.0", "subtype":"message_changed"}  // edit
{"type":"raw", "envelope": { ... }}                                                                // literal envelope
```

The bot's user id is **`UBOT`** (returned by `auth.test`) — use that
in `<@…>` mention strings.

## fakeagent scripts

```sh
internal/skills/bundle/e2e/scripts/fakeagent.py --script reply-once
internal/skills/bundle/e2e/scripts/fakeagent.py --script slow --delay-ms 500
internal/skills/bundle/e2e/scripts/fakeagent.py --script request-permission
internal/skills/bundle/e2e/scripts/fakeagent.py --script panic-mid-prompt
```

| script | behaviour |
|---|---|
| `reply-once` | one `session/update` text block, then `end_turn` |
| `slow` | hold the prompt `--delay-ms`; honors `session/cancel` → `cancelled` stop |
| `request-permission` | emit a `session/request_permission` then continue |
| `panic-mid-prompt` | one update, then `os._exit(1)` — exercises child-restart path |

Optional `--status-file PATH` exposes counters (`sessions_created`,
`prompts`, `cancels`, `last_prompt`) for tests that need to assert on
agent-side state. Use a path under `mktemp -d`.

If a future case needs new behaviour, **add a `--script` arm in
`fakeagent.py`** rather than reaching for flags or env vars — keep the
surface declarative.

## Cases

Each case below documents what failure mode it guards against and the
specific inject/assert step. The boilerplate from
[Drive the harness via tmux](#drive-the-harness-via-tmux) runs first;
each case then drives via foreground `curl` against `$BASE`.

### 1. DM round-trip

Guards against: Socket Mode → handler → ACP → PostStreamer wiring.

```sh
# inject
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"message_im","user":"U1","channel":"D1","text":"hello","ts":"100.0"}' \
  "${BASE}/control/send"

# assert: the agent's reply lands as a postMessage
for i in $(seq 1 50); do
  MSGS=$(curl -s "${BASE}/control/messages")
  echo "$MSGS" | grep -q "hello back from reply-once" && break
  sleep 0.1
done
echo "$MSGS" | grep -q '"kind": "postMessage"' || { echo FAIL; exit 1; }
```

### 2. `@mention` in channel

Guards against: `app_mention` event path; reply lands in the thread
(`thread_ts == ts` of the mention).

```sh
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"app_mention","user":"U1","channel":"C1","text":"<@UBOT> hi","ts":"200.0"}' \
  "${BASE}/control/send"

# assert thread_ts is the original ts
for i in $(seq 1 50); do
  MSGS=$(curl -s "${BASE}/control/messages")
  echo "$MSGS" | grep -q '"thread_ts": "200.0"' && break
  sleep 0.1
done
echo "$MSGS" | grep -q '"thread_ts": "200.0"' || { echo FAIL; exit 1; }
```

### 3. Threaded follow-up reuses session

Guards against: the router losing the (channel, thread_ts) → session
mapping on follow-ups.

Use `--status-file`:

```sh
STATUS=$(mktemp)
# (re-launch slack-acp with --agent-cmd "...fakeagent.py --script reply-once --status-file $STATUS")

# first message
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"message_im","user":"U1","channel":"D1","text":"one","ts":"100.0"}' \
  "${BASE}/control/send"
# wait for reply N=1
for i in $(seq 1 50); do grep -q '"prompts": 1' "$STATUS" && break; sleep 0.1; done

# second message in the same thread
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"message_im","user":"U1","channel":"D1","text":"two","ts":"100.1","thread_ts":"100.0"}' \
  "${BASE}/control/send"
for i in $(seq 1 50); do grep -q '"prompts": 2' "$STATUS" && break; sleep 0.1; done

# assert: only ONE session was created
grep -q '"sessions_created": 1' "$STATUS" || { cat "$STATUS"; echo FAIL; exit 1; }
```

### 4. Cancellation on follow-up

Guards against: regression of context-cancel + `session/cancel` on a
new message arriving mid-prompt.

Relaunch slack-acp with `--script slow --delay-ms 3000 --status-file $STATUS`,
then:

```sh
# first message — agent will hold for 3s
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"message_im","user":"U1","channel":"D1","text":"first","ts":"400.0"}' \
  "${BASE}/control/send"

# wait until prompt is in flight
for i in $(seq 1 30); do grep -q '"prompts": 1' "$STATUS" && break; sleep 0.1; done

# second message in same thread → must cancel the in-flight one
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"message_im","user":"U1","channel":"D1","text":"second","ts":"400.1","thread_ts":"400.0"}' \
  "${BASE}/control/send"

# assert: cancels bumps to 1
for i in $(seq 1 50); do grep -q '"cancels": 1' "$STATUS" && break; sleep 0.1; done
grep -q '"cancels": 1' "$STATUS" || { cat "$STATUS"; echo FAIL; exit 1; }
```

### 5. State-dir persistence across restart

Guards against: per-thread cwd being recreated (or worse, deleted) on
restart, breaking agent-side `.fir/` state.

```sh
# Step A: with slack-acp running, send a message and let it land.
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"message_im","user":"U1","channel":"D1","text":"hi","ts":"100.0"}' \
  "${BASE}/control/send"
for i in $(seq 1 50); do grep -q '"prompts": 1' "$STATUS" && break; sleep 0.1; done

THREAD_DIR="$STATE/threads/D1/100.0"
[ -d "$THREAD_DIR" ] || { echo "no per-thread cwd"; exit 1; }
INODE_BEFORE=$(stat -f %i "$THREAD_DIR")

# Step B: restart slack-acp with the SAME state-dir.
tm-killwin slack-acp-e2e bot
STATUS2=$(mktemp)
tm-win slack-acp-e2e bot
tm-send slack-acp-e2e "cd $(pwd) && SLACK_BOT_TOKEN=xoxb-test SLACK_APP_TOKEN=xapp-test SLACK_API_BASE='$HTTP' exec ./bin/slack-acp --agent-cmd '$(pwd)/internal/skills/bundle/e2e/scripts/fakeagent.py --script reply-once --status-file $STATUS2' --state-dir '$STATE'"
for i in $(seq 1 50); do curl -s "${BASE}/control/connected" | grep -q true && break; sleep 0.1; done

# Step C: follow-up in same thread reuses same dir (inode unchanged).
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"message_im","user":"U1","channel":"D1","text":"again","ts":"100.2","thread_ts":"100.0"}' \
  "${BASE}/control/send"
for i in $(seq 1 50); do grep -q '"prompts": 1' "$STATUS2" && break; sleep 0.1; done

INODE_AFTER=$(stat -f %i "$THREAD_DIR")
[ "$INODE_BEFORE" = "$INODE_AFTER" ] || { echo "cwd inode changed"; exit 1; }
```

### 6. Bot's own messages ignored

Guards against: feedback loops from the bot reading its own posts.

```sh
PROMPTS_BEFORE=$(python3 -c "import json; print(json.load(open('$STATUS'))['prompts'])")
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"message_im","user":"UBOT","channel":"D9","text":"loop","ts":"700.0","bot_id":"BBOT"}' \
  "${BASE}/control/send"

# absence-assertion: short bounded sleep, then confirm prompts didn't budge
sleep 0.5
PROMPTS_AFTER=$(python3 -c "import json; print(json.load(open('$STATUS'))['prompts'])")
[ "$PROMPTS_BEFORE" = "$PROMPTS_AFTER" ] || { echo "bot self-message leaked"; exit 1; }
```

(This is one of two places a small wall-clock wait is unavoidable:
we're asserting on the *absence* of an effect. Keep it bounded and
short.)

### 7. Edits / subtype messages ignored

Same shape as case 7, with `"subtype":"message_changed"` in place of
`bot_id`:

```sh
PROMPTS_BEFORE=$(python3 -c "import json; print(json.load(open('$STATUS'))['prompts'])")
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"message_im","user":"U1","channel":"D8","text":"edited","ts":"800.0","subtype":"message_changed"}' \
  "${BASE}/control/send"
sleep 0.5
PROMPTS_AFTER=$(python3 -c "import json; print(json.load(open('$STATUS'))['prompts'])")
[ "$PROMPTS_BEFORE" = "$PROMPTS_AFTER" ] || { echo "edit leaked"; exit 1; }
```

### 8. Mid-text mention stripping

Guards against: `<@U…>` references mid-sentence leaking into the agent
prompt.

```sh
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"app_mention","user":"U1","channel":"C9","text":"hey <@UBOT> what about <@UBOT> this","ts":"900.0"}' \
  "${BASE}/control/send"

# assert via fakeagent's --status-file: last_prompt == "hey  what about  this"
for i in $(seq 1 50); do
  LP=$(python3 -c "import json; print(json.load(open('$STATUS')).get('last_prompt',''))")
  [ "$LP" = "hey  what about  this" ] && break
  sleep 0.1
done
[ "$LP" = "hey  what about  this" ] || { echo "got '$LP'"; exit 1; }
```

### 9. Two distinct threads → two distinct sessions

Guards against: router collapsing distinct (channel, thread_ts) keys
into one session.

```sh
SC_BEFORE=$(python3 -c "import json; print(json.load(open('$STATUS'))['sessions_created'])")
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"message_im","user":"U1","channel":"D10A","text":"a","ts":"1010.0"}' \
  "${BASE}/control/send"
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"message_im","user":"U1","channel":"D10B","text":"b","ts":"1010.1"}' \
  "${BASE}/control/send"

# assert: sessions_created bumps by exactly 2
for i in $(seq 1 50); do
  SC=$(python3 -c "import json; print(json.load(open('$STATUS'))['sessions_created'])")
  [ "$SC" -ge $((SC_BEFORE+2)) ] && break
  sleep 0.1
done
[ "$SC" = "$((SC_BEFORE+2))" ] || { echo "sessions_created jumped from $SC_BEFORE to $SC"; exit 1; }
```

### 10. App-mention with explicit `thread_ts` replies in that thread

Guards against: app_mention path ignoring `thread_ts` when the user
mentioned the bot from inside an existing thread (replying at the
parent instead of in-thread).

```sh
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"type":"app_mention","user":"U1","channel":"C11","text":"<@UBOT> in thread","ts":"1100.5","thread_ts":"1100.0"}' \
  "${BASE}/control/send"

for i in $(seq 1 50); do
  MSGS=$(curl -s "${BASE}/control/messages")
  echo "$MSGS" | grep -q '"thread_ts": "1100.0"' && break
  sleep 0.1
done
echo "$MSGS" | grep -q '"thread_ts": "1100.0"' || { echo FAIL; exit 1; }
```

## Pitfalls

- **No wall-clock waits.** Use bounded `for i in $(seq …)` loops
  polling the same `/control/*` endpoint or `--status-file`. The one
  exception is asserting *absence* (cases 7 & 8).
- **Long-lived processes go in tmux**, not backgrounded `&` jobs.
  Backgrounding inside a single `Bash` call ties up the agent's
  turn until cleanup; tmux makes each step independent.
- **Subprocess teardown.** `tm-kill slack-acp-e2e` reaps the whole
  tree. If you do background something locally, trap EXIT.
- **Port allocation.** fakeslack picks a free port (`--http-port 0
  --ws-port 0`); never hard-code one.
- **State-dir leakage.** Always pass `--state-dir "$(mktemp -d)"`.
  Two parallel runs sharing a state dir corrupt each other's
  `threads/` map.
- **Real Slack tokens.** Never set a real `xoxb-`/`xapp-` token here.
  fakeslack accepts any string-shaped token.
- **`SLACK_API_BASE` trailing slash.** Must end with `/api/` —
  slack-go appends method names directly to it.

## Live smoke (optional, manual)

For a final manual check before a release, run against a sandbox
Slack workspace:

1. Install a test Slack app in a sandbox workspace (see `deploy`
   skill for the scopes).
2. Export `SLACK_BOT_TOKEN` + `SLACK_APP_TOKEN` for that app.
3. Run `slack-acp --agent-cmd 'fir --mode acp'` locally.
4. From the Slack client: DM the bot, mention the bot in a channel,
   reply in the thread, cancel a long-running prompt by sending a
   follow-up.

Document any new bug found by live smoke as a regression case in this
file before fixing.

## Checklist for a new case

- [ ] Added under [Cases](#cases) above with a one-line statement of
      the failure mode it guards against.
- [ ] Uses `mktemp -d` for state dir.
- [ ] No `time.Sleep` / `sleep N` outside the bounded poll-loop
      idiom (or absence-assertion in cases 7 & 8).
- [ ] Subprocesses killed in cleanup.
- [ ] Asserts on the *visible* surface (`/control/messages`,
      `--status-file`), not on internal state.
- [ ] If new agent behaviour is needed, add a `--script` arm to
      `fakeagent.py` rather than new flags or env vars.
