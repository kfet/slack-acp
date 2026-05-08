#!/usr/bin/env python3
"""A scriptable fake ACP agent for slack-acp's e2e harness.

Speaks the Agent Client Protocol (ACP) over stdio (newline-delimited
JSON-RPC 2.0). Used as the `--agent-cmd` target in e2e tests so
slack-acp talks to a deterministic, observable child instead of a
real `fir --mode acp`.

Usage (from a test):

    fakeagent.py --script reply-once
    fakeagent.py --script slow --delay-ms 500
    fakeagent.py --script request-permission
    fakeagent.py --script panic-mid-prompt

Behaviour by script:

  reply-once
      Respond to every session/prompt with one session/update text
      block ("hello back from <script>") and stop reason `end_turn`.

  slow
      Same as reply-once but holds the prompt for --delay-ms before
      replying. Honors session/cancel: returns `cancelled` immediately
      if the cancel arrives during the delay.

  request-permission
      On each prompt, send a session/request_permission RPC up to the
      bot, then either continue (if granted) or stop with `refusal`
      (if denied).

  panic-mid-prompt
      Send one session/update, then exit 1 mid-stream to simulate an
      agent crash. Useful for testing slack-acp's child-restart logic.

Observation hooks (read by tests via --status-file):

    {"sessions_created": N, "prompts": N, "cancels": N, "last_prompt": "..."}

Stdlib only — runs on macOS' built-in python3.9.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import threading
import uuid
from typing import Any


# ---- protocol helpers -----------------------------------------------------

def write_msg(msg: dict) -> None:
    """Write a JSON-RPC message + newline to stdout, flushed."""
    sys.stdout.write(json.dumps(msg, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def reply(req: dict, result: Any) -> None:
    write_msg({"jsonrpc": "2.0", "id": req["id"], "result": result})


def reply_error(req: dict, code: int, message: str) -> None:
    write_msg({"jsonrpc": "2.0", "id": req["id"],
               "error": {"code": code, "message": message}})


def notify(method: str, params: dict) -> None:
    write_msg({"jsonrpc": "2.0", "method": method, "params": params})


def text_block(s: str) -> dict:
    return {"type": "text", "text": s}


def session_update(session_id: str, content: str) -> None:
    notify("session/update", {
        "sessionId": session_id,
        "update": {
            "sessionUpdate": "agent_message_chunk",
            "content": text_block(content),
        },
    })


# ---- state ---------------------------------------------------------------

class State:
    def __init__(self, status_file: str | None) -> None:
        self.status_file = status_file
        self.lock = threading.Lock()
        self.sessions_created = 0
        self.prompts = 0
        self.cancels = 0
        self.last_prompt = ""
        # session_id → cancel-event for in-flight prompts
        self.in_flight: dict[str, threading.Event] = {}

    def _write_status(self) -> None:
        if not self.status_file:
            return
        with self.lock:
            data = {
                "sessions_created": self.sessions_created,
                "prompts": self.prompts,
                "cancels": self.cancels,
                "last_prompt": self.last_prompt,
            }
        tmp = self.status_file + ".tmp"
        with open(tmp, "w") as f:
            json.dump(data, f)
        os.replace(tmp, self.status_file)

    def bump_session(self) -> None:
        with self.lock:
            self.sessions_created += 1
        self._write_status()

    def bump_prompt(self, text: str) -> None:
        with self.lock:
            self.prompts += 1
            self.last_prompt = text
        self._write_status()

    def bump_cancel(self) -> None:
        with self.lock:
            self.cancels += 1
        self._write_status()


# ---- request handlers ----------------------------------------------------

def handle_initialize(req: dict, state: State) -> None:
    reply(req, {
        "protocolVersion": req.get("params", {}).get("protocolVersion", "1.0"),
        "agentCapabilities": {
            "promptCapabilities": {
                "image": False, "audio": False, "embeddedContext": False,
            },
        },
    })


def handle_session_new(req: dict, state: State) -> None:
    state.bump_session()
    sid = "fakeagent-" + uuid.uuid4().hex[:8]
    reply(req, {"sessionId": sid})


def extract_prompt_text(params: dict) -> str:
    blocks = params.get("prompt") or []
    chunks = []
    for b in blocks:
        if b.get("type") == "text":
            chunks.append(b.get("text", ""))
    return "".join(chunks)


def handle_session_prompt(req: dict, state: State, args: argparse.Namespace) -> None:
    params = req.get("params", {})
    sid = params.get("sessionId", "")
    text = extract_prompt_text(params)
    state.bump_prompt(text)

    cancel = threading.Event()
    with state.lock:
        state.in_flight[sid] = cancel

    def runner() -> None:
        try:
            stop = run_script(args, sid, text, cancel, state, req)
        finally:
            with state.lock:
                state.in_flight.pop(sid, None)
        reply(req, {"stopReason": stop})

    threading.Thread(target=runner, daemon=True).start()


def handle_session_cancel(req_or_notif: dict, state: State) -> None:
    params = req_or_notif.get("params", {})
    sid = params.get("sessionId", "")
    state.bump_cancel()
    with state.lock:
        ev = state.in_flight.get(sid)
    if ev:
        ev.set()
    if "id" in req_or_notif:
        reply(req_or_notif, None)


# ---- scripts -------------------------------------------------------------

def run_script(args, sid: str, prompt_text: str,
               cancel: threading.Event, state: State,
               req: dict) -> str:
    script = args.script
    if script == "reply-once":
        session_update(sid, f"hello back from {script}")
        return "end_turn"

    if script == "slow":
        if cancel.wait(timeout=args.delay_ms / 1000.0):
            return "cancelled"
        session_update(sid, f"hello back from {script}")
        return "end_turn"

    if script == "request-permission":
        # Send a request_permission RPC — we don't wait synchronously
        # in this minimal harness; assume granted.
        notify("session/request_permission", {
            "sessionId": sid,
            "options": [
                {"optionId": "allow_once", "name": "Allow once",
                 "kind": "allow_once"},
                {"optionId": "reject_once", "name": "Reject",
                 "kind": "reject_once"},
            ],
            "toolCall": {"name": "fake.tool", "kind": "execute"},
        })
        # In a richer harness, await the response and branch. For now,
        # just continue.
        session_update(sid, "permission requested")
        return "end_turn"

    if script == "panic-mid-prompt":
        session_update(sid, "starting...")
        sys.stderr.write("fakeagent: simulated panic\n")
        sys.stderr.flush()
        os._exit(1)

    sys.stderr.write(f"fakeagent: unknown script: {script}\n")
    return "refusal"


# ---- dispatch ------------------------------------------------------------

# Methods we reply to / handle. Anything else gets a generic OK or is dropped.
def dispatch(msg: dict, state: State, args) -> None:
    method = msg.get("method")
    if method is None:
        # Response to a notify (e.g. request_permission). Ignore.
        return
    if method == "initialize":
        handle_initialize(msg, state)
    elif method == "session/new":
        handle_session_new(msg, state)
    elif method == "session/prompt":
        handle_session_prompt(msg, state, args)
    elif method == "session/cancel":
        handle_session_cancel(msg, state)
    elif method == "session/load":
        # Accept any load; pretend the session exists.
        if "id" in msg:
            reply(msg, None)
    else:
        # Unknown but with id → empty result; otherwise drop.
        if "id" in msg:
            reply(msg, None)


def read_loop(state: State, args) -> None:
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError as e:
            sys.stderr.write(f"fakeagent: bad JSON: {e}\n")
            continue
        dispatch(msg, state, args)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--script",
                    choices=["reply-once", "slow", "request-permission",
                             "panic-mid-prompt"],
                    default="reply-once")
    ap.add_argument("--delay-ms", type=int, default=200,
                    help="for --script slow: delay before reply (default 200)")
    ap.add_argument("--status-file",
                    help="write JSON counters here after each event")
    args = ap.parse_args()

    state = State(args.status_file)
    state._write_status()  # create file with zeroes so tests can read it
    try:
        read_loop(state, args)
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
