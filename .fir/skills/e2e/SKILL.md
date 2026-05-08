---
name: e2e
description: Run and extend slack-acp's end-to-end smoke tests — black-box exercises of the real slack-acp binary against a mock Slack Socket Mode server and a scriptable fake ACP agent. Use when changes touch the wire layers (Socket Mode framing, ACP framing, session lifecycle) where in-package unit tests aren't enough.
---

# e2e Skill

End-to-end testing for `slack-acp`. The bot has three wire surfaces:

```
Slack ── ws ── slackproto ── handler ── router ── acpclient ── stdio ── agent
```

The unit tests in `internal/*/...` cover each layer in isolation with
fakes. **e2e tests** drive the *real* `slack-acp` binary as a
black-box subprocess, with both the Slack side and the agent side
faked out, to catch wiring bugs that in-package tests can't see:

- Socket Mode envelope quirks (ack, retry, message vs app_mention).
- ACP `initialize` handshake edge cases (caps negotiation, version skew).
- Process-lifecycle bugs (signal handling, child cleanup, restart).
- State-dir layout regressions (per-thread cwd, reuse across restart).
- Permission-policy decisions returned to the agent.

When in doubt: if a unit test in `internal/handler/handler_test.go`
*could* express the case, write it there. e2e is the layer for
"the binary itself", not for handler logic.

## Layout

e2e tests live under `test/e2e/` with build tag `e2e` so they don't
run in the default `make test` (they spawn subprocesses, fake
listeners, etc.):

```
test/
  e2e/
    e2e_test.go           # //go:build e2e
    fakeslack/            # mock Slack Socket Mode server
      server.go
    fakeagent/            # subprocess that speaks ACP over stdio
      main.go             # small Go binary, scriptable via env vars
```

If this directory is empty: the harness has not been built yet.
That's fine; the skill teaches how to build it (see "Bootstrap" below).
Building it is a one-time effort — once it exists, the rest of the
skill is just "run / extend".

## Run

```bash
make e2e
```

Equivalent to (proposed Makefile target):

```bash
go test -tags=e2e -count=1 ./test/e2e/...
```

`-count=1` defeats the test cache: e2e tests depend on the binary
under test, which `go test` doesn't track for cache invalidation.

To run a single case:

```bash
go test -tags=e2e -run TestThreadedFollowupReusesSession ./test/e2e/...
```

`make e2e V=1` to see subprocess stdout/stderr live (useful when
debugging a hang — Socket Mode handshake stuck, ACP child not
spawning, etc.).

## Anatomy of an e2e test

```go
//go:build e2e

package e2e_test

import (
    "context"
    "os"
    "os/exec"
    "testing"
    "time"

    "github.com/kfet/slack-acp/test/e2e/fakeslack"
)

func TestDMRoundTrip(t *testing.T) {
    // 1. Start fake Slack.
    fs := fakeslack.Start(t)
    defer fs.Stop()

    // 2. Build / locate slack-acp + fakeagent binaries (helpers below).
    bot := buildBot(t)
    agent := buildFakeAgent(t)

    // 3. Spawn slack-acp pointing at fake slack + fake agent.
    cmd := exec.Command(bot,
        "--agent-cmd", agent+" --script reply-once",
        "--state-dir", t.TempDir(),
    )
    cmd.Env = append(os.Environ(),
        "SLACK_BOT_TOKEN=xoxb-test",
        "SLACK_APP_TOKEN=xapp-test",
        "SLACK_API_BASE="+fs.WebAPIURL(),  // see "Hooks" below
        "SLACK_SOCKET_URL="+fs.SocketURL(),
    )
    cmd.Stdout, cmd.Stderr = testWriter(t), testWriter(t)
    must(t, cmd.Start())
    defer cmd.Process.Kill()

    // 4. Wait for the bot to connect to the fake Socket Mode server.
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    fs.WaitForConnect(ctx, t)

    // 5. Drive: send a DM event into the websocket.
    fs.SendMessageIM(t, "U_HUMAN", "C_DM", "T1", "hello")

    // 6. Assert: a chat.postMessage / chat.update lands on the Web API
    //            with the expected text.
    msg := fs.WaitForMessage(ctx, t, "C_DM", "T1")
    if !strings.Contains(msg.Text, "hello back") {
        t.Fatalf("unexpected reply: %q", msg.Text)
    }
}
```

### Hooks the e2e harness needs

The bot reaches Slack at two URLs: the Web API (`https://slack.com/api/...`)
and Socket Mode (`wss://wss-primary.slack.com/...`). Both are hard-coded
in `slack-go/slack`. Two ways to redirect them in tests:

1. **`SLACK_API_BASE` env var.** Add support in
   `internal/slackproto` to override `slack.SLACK_API` at construction
   time. This is a cheap one-line change, well-scoped, and useful
   beyond e2e.
2. **`/etc/hosts`-style trickery** — don't. Brittle, requires
   privileges, breaks on macOS sandboxed test runners.

Pick (1). The env-var override should *only* take effect when set; in
production it's a no-op.

For Socket Mode, `slack-go/slack`'s `socketmode.New(api,
socketmode.OptionAppLevelToken(...))` calls `apps.connections.open`
on the Web API to discover the wss URL. The fake Slack server returns
its own wss URL from that endpoint, so redirecting only the Web API is
enough.

## Cases worth covering

Drive these from the e2e suite. They are not adequately covered by
in-package tests:

1. **DM round-trip.** Fake Slack injects `message.im`; expect a
   `chat.postMessage` then ≥1 `chat.update` carrying agent text.
2. **`@mention` in channel.** Inject `app_mention`; reply lands in
   the thread (`thread_ts == ts` of the mention).
3. **Threaded follow-up reuses session.** Send a second message in
   the same thread; the fake agent's `session/new` count must stay
   at 1 (use a counter exposed by `fakeagent`).
4. **Cancellation on follow-up.** Send a second message before the
   first prompt finishes; the fake agent must observe a
   `session/cancel` for the first prompt.
5. **State-dir persistence.** Stop and restart `slack-acp`. A new
   message in the same thread must reuse the existing per-thread cwd
   under `<state_dir>/threads/<channel>/<thread_ts>/`.
6. **Permission policy.** Run with `--policy read-only`; fake agent
   requests a write-tool permission; expect the bot's reply on
   `session/request_permission` to be a `denied` outcome.
7. **Bot's own messages ignored.** Inject a `message` event whose
   `bot_id` matches the bot; expect zero ACP traffic.
8. **Edits / subtype messages ignored.** Inject a message with
   `subtype: "message_changed"`; expect zero ACP traffic.
9. **Mid-text mention stripping.** Inject text containing a `<@Uxyz>`
   reference embedded mid-sentence; the prompt forwarded to the agent
   must have it removed.

## fakeagent: the scriptable ACP child

A small Go binary that speaks ACP over stdio. Driven by command-line
flags or env vars:

```
fakeagent --script reply-once          # reply once with "hello back"
fakeagent --script slow                # hold the prompt 2s before replying
fakeagent --script request-permission  # send session/request_permission
fakeagent --script panic-mid-prompt    # exit 1 mid-stream
```

It MUST honour `session/cancel` (drop the in-flight reply, return a
`cancelled` stop reason) so test #4 passes. Implementing it well is
the most useful single piece of e2e scaffolding — most tests share it.

## fakeslack: the mock Socket Mode + Web API server

httptest server with two surfaces:

- **Web API** — minimal: `auth.test`, `apps.connections.open`,
  `users.info`, `chat.postMessage`, `chat.update`. Records every
  posted message in a buffer that tests can `WaitForMessage` against.
- **Socket Mode (websocket)** — accepts the connection, responds to
  ping with pong, accepts ack envelopes, and exposes
  `SendMessageIM`/`SendAppMention`/`SendRaw` for tests to inject
  events.

Both use deterministic synchronisation (channels), not `time.Sleep`
— same rule as the rest of the codebase (see AGENTS.md "Testing —
avoid wall-clock timeouts").

## Bootstrap (when `test/e2e/` is empty)

If you find `test/e2e/` empty, create the harness in this order:

1. **Add `SLACK_API_BASE` override** in `internal/slackproto` so the
   Web API can be redirected. Unit-test the override.
2. **Build `test/e2e/fakeagent`** with one script (`reply-once`).
   Verify it works with the real `slack-acp` by running it locally
   against a sandbox Slack app (manual smoke).
3. **Build `test/e2e/fakeslack`** with the Web API surface only;
   first e2e test asserts `apps.connections.open` is called and the
   bot exits cleanly when the WS URL it returns is unreachable.
4. **Add Socket Mode** to `fakeslack`; first DM round-trip test
   passes.
5. **Add Makefile target** `e2e` that runs `go test -tags=e2e -count=1
   ./test/e2e/...`. Wire into `make all` once the suite is stable.

Each step is a separate commit. Don't merge the whole harness in one
commit — too much surface to review.

## Pitfalls

- **`go test` caches across binary changes.** Always `-count=1` for
  e2e, or invoke via `make e2e` which sets it.
- **Subprocess teardown.** Always `cmd.Process.Kill()` in defer plus
  a context-bounded `cmd.Wait` with a short timeout, or hung
  subprocesses will pile up between test runs.
- **Port allocation.** `httptest.NewServer` picks a free port; never
  hard-code one. Same for any auxiliary listener.
- **Wall-clock waits.** No `time.Sleep` in tests. Use channels,
  `WaitForX(ctx)` helpers, and pass test timeouts via `context.WithTimeout`.
- **State-dir leakage.** Always pass `--state-dir t.TempDir()`. Two
  parallel tests sharing a state dir will corrupt each other's
  `threads/` map.
- **Real Slack tokens.** Never put a real `xoxb-`/`xapp-` token in an
  e2e test, ever. The fake server accepts any string-shaped token.

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

Document any new bug found by live smoke as a regression case in the
automated e2e suite before fixing.

## Checklist for a new e2e case

- [ ] Build tag `//go:build e2e` set on the test file.
- [ ] Uses `t.TempDir()` for state dir; no shared global state.
- [ ] No `time.Sleep`; all waits are context-bounded.
- [ ] Subprocess killed in defer.
- [ ] Asserts on the *visible* surface (chat.update payload, agent
      stdin/stdout via fakeagent), not on internal state.
- [ ] Documents what failure mode it guards against (one-line
      comment at the top of the test).
