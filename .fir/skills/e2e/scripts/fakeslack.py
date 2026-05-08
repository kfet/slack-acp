#!/usr/bin/env python3
"""fakeslack — mock Slack Web API + Socket Mode server for slack-acp e2e.

Runs two listeners:

  * HTTP (Web API + control plane), advertised via SLACK_API_BASE.
    - /api/auth.test                           → {ok, user_id, user, team_id}
    - /api/apps.connections.open               → {ok, url=ws://...}
    - /api/chat.postMessage                    → {ok, channel, ts, message}
    - /api/chat.update                         → {ok, channel, ts, text}
    - /control/send       (POST)               inject a Socket Mode event
    - /control/messages   (GET)                dump recorded chat.* calls
    - /control/connected  (GET)                returns {connected: bool}
    - /control/clear      (POST)               clear the message log

  * WS (Socket Mode), URL returned from apps.connections.open.
    Sends a `hello` envelope on connect; relays events injected via
    /control/send; records ACK envelopes from the bot.

Usage:
    fakeslack.py --http-port 0 --ws-port 0 --print-urls
        # prints two lines on stdout:
        #   http=http://127.0.0.1:NNN/api/
        #   ws=ws://127.0.0.1:MMM/

The harness reads those, exports SLACK_API_BASE=$http for slack-acp, and
drives /control/* over plain HTTP.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Optional

# Allow `python3 fakeslack.py` from anywhere.
_HERE = os.path.dirname(os.path.abspath(__file__))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)
import ws  # noqa: E402


# --------------------------- shared state ---------------------------


class State:
    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.ws_conn: Optional[ws.WSConn] = None
        self.connected = threading.Event()
        # Recorded outbound API calls (chat.postMessage, chat.update).
        self.messages: list[dict] = []
        # Recorded ACKs from the bot, by envelope_id.
        self.acks: set[str] = set()
        # Monotonic ts source for chat.postMessage replies.
        self._ts_seq = 0

    def next_ts(self) -> str:
        with self.lock:
            self._ts_seq += 1
            return f"{int(time.time())}.{self._ts_seq:06d}"

    def record(self, kind: str, payload: dict) -> None:
        with self.lock:
            self.messages.append({"kind": kind, **payload})


# --------------------------- HTTP handler ---------------------------


def make_http_handler(state: State, ws_url: str):
    class Handler(BaseHTTPRequestHandler):
        # Silence stderr access logs.
        def log_message(self, fmt, *args):
            pass

        # ---------- helpers ----------

        def _read_body(self) -> dict:
            length = int(self.headers.get("Content-Length", "0"))
            raw = self.rfile.read(length) if length else b""
            ctype = (self.headers.get("Content-Type") or "").lower()
            if "application/json" in ctype:
                try:
                    return json.loads(raw.decode("utf-8") or "{}")
                except json.JSONDecodeError:
                    return {}
            # form-urlencoded (slack-go uses this for chat.postMessage)
            from urllib.parse import parse_qs

            parsed = parse_qs(raw.decode("utf-8"), keep_blank_values=True)
            return {k: v[0] if v else "" for k, v in parsed.items()}

        def _send_json(self, obj: dict, status: int = 200) -> None:
            body = json.dumps(obj).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        # ---------- routes ----------

        def do_GET(self):
            if self.path == "/control/messages":
                with state.lock:
                    self._send_json({"messages": list(state.messages)})
                return
            if self.path == "/control/connected":
                self._send_json({"connected": state.connected.is_set()})
                return
            self._send_json({"ok": False, "error": "not_found"}, status=404)

        def do_POST(self):
            path = self.path
            body = self._read_body()

            # ---- Web API surface ----
            if path == "/api/auth.test":
                self._send_json(
                    {
                        "ok": True,
                        "url": "https://example.invalid/",
                        "team": "fake-team",
                        "user": "fake-bot",
                        "team_id": "T0FAKE",
                        "user_id": "UBOT",
                        "bot_id": "BBOT",
                    }
                )
                return

            if path == "/api/apps.connections.open":
                self._send_json({"ok": True, "url": ws_url})
                return

            if path == "/api/chat.postMessage":
                ts = state.next_ts()
                channel = body.get("channel", "")
                text = body.get("text", "")
                thread_ts = body.get("thread_ts", "") or ""
                state.record(
                    "postMessage",
                    {"channel": channel, "ts": ts, "thread_ts": thread_ts, "text": text},
                )
                self._send_json(
                    {
                        "ok": True,
                        "channel": channel,
                        "ts": ts,
                        "message": {"text": text, "ts": ts, "thread_ts": thread_ts},
                    }
                )
                return

            if path == "/api/chat.update":
                channel = body.get("channel", "")
                ts = body.get("ts", "")
                text = body.get("text", "")
                state.record(
                    "update",
                    {"channel": channel, "ts": ts, "text": text},
                )
                self._send_json(
                    {"ok": True, "channel": channel, "ts": ts, "text": text}
                )
                return

            # Generic ok for any other Slack API call we don't model.
            if path.startswith("/api/"):
                self._send_json({"ok": True})
                return

            # ---- control plane ----
            if path == "/control/send":
                self._handle_control_send(body)
                return

            if path == "/control/clear":
                with state.lock:
                    state.messages.clear()
                    state.acks.clear()
                self._send_json({"ok": True})
                return

            self._send_json({"ok": False, "error": "not_found"}, status=404)

        # ---------- control ----------

        def _handle_control_send(self, body: dict) -> None:
            if not state.connected.wait(timeout=10.0) or state.ws_conn is None:
                self._send_json({"ok": False, "error": "no_ws_connection"}, status=503)
                return
            kind = body.get("type", "")
            envelope_id = str(uuid.uuid4())

            if kind == "raw":
                env = body.get("envelope") or {}
                env.setdefault("envelope_id", envelope_id)
                state.ws_conn.send_text(json.dumps(env))
                self._send_json({"ok": True, "envelope_id": envelope_id})
                return

            if kind in ("app_mention", "message_im"):
                user = body.get("user", "U1")
                channel = body.get("channel", "C1")
                text = body.get("text", "hello")
                ts = body.get("ts") or state.next_ts()
                thread_ts = body.get("thread_ts", "") or ""

                if kind == "app_mention":
                    inner = {
                        "type": "app_mention",
                        "user": user,
                        "channel": channel,
                        "text": text,
                        "ts": ts,
                    }
                    if thread_ts:
                        inner["thread_ts"] = thread_ts
                else:
                    inner = {
                        "type": "message",
                        "user": user,
                        "channel": channel,
                        "channel_type": "im",
                        "text": text,
                        "ts": ts,
                    }
                    if thread_ts:
                        inner["thread_ts"] = thread_ts
                    # Allow tests to inject bot/edit/etc. via passthrough.
                    for k in ("bot_id", "subtype"):
                        if body.get(k):
                            inner[k] = body[k]

                env = {
                    "envelope_id": envelope_id,
                    "type": "events_api",
                    "accepts_response_payload": False,
                    "payload": {
                        "type": "event_callback",
                        "team_id": "T0FAKE",
                        "api_app_id": "A0FAKE",
                        "event": inner,
                        "event_id": "Ev" + envelope_id[:8],
                        "event_time": int(time.time()),
                    },
                }
                state.ws_conn.send_text(json.dumps(env))
                self._send_json({"ok": True, "envelope_id": envelope_id})
                return

            self._send_json({"ok": False, "error": "unknown_type"}, status=400)

    return Handler


# --------------------------- WS handler ---------------------------


def make_ws_on_connect(state: State):
    def on_connect(conn: ws.WSConn) -> None:
        with state.lock:
            state.ws_conn = conn
        state.connected.set()
        # Slack sends a "hello" envelope on connect.
        conn.send_text(json.dumps({"type": "hello", "num_connections": 1}))
        try:
            while True:
                msg = conn.recv_text(timeout=None)
                if msg is None:
                    return
                try:
                    parsed = json.loads(msg)
                except json.JSONDecodeError:
                    continue
                env_id = parsed.get("envelope_id")
                if env_id:
                    with state.lock:
                        state.acks.add(env_id)
        finally:
            with state.lock:
                if state.ws_conn is conn:
                    state.ws_conn = None
            state.connected.clear()

    return on_connect


# --------------------------- main ---------------------------


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--http-port", type=int, default=0)
    ap.add_argument("--ws-port", type=int, default=0)
    ap.add_argument(
        "--print-urls",
        action="store_true",
        help="Print http=URL and ws=URL on stdout, one per line, then continue.",
    )
    args = ap.parse_args()

    state = State()

    # Start WS first so we know its URL when answering apps.connections.open.
    ws_handle = ws.serve(args.host, args.ws_port, make_ws_on_connect(state))
    ws_url = ws_handle.url

    http_handler = make_http_handler(state, ws_url)
    httpd = ThreadingHTTPServer((args.host, args.http_port), http_handler)
    http_thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    http_thread.start()

    http_url = f"http://{args.host}:{httpd.server_address[1]}/api/"

    if args.print_urls:
        print(f"http={http_url}")
        print(f"ws={ws_url}")
        sys.stdout.flush()

    try:
        threading.Event().wait()
    except KeyboardInterrupt:
        pass
    finally:
        httpd.shutdown()
        ws_handle.stop()
    return 0


if __name__ == "__main__":
    sys.exit(main())
