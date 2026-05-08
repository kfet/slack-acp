"""Minimal RFC 6455 WebSocket server. Stdlib-only.

Designed for fakeslack.py: a single concurrent connection per test is
fine, so we keep things synchronous and threaded — one thread per
accepted upgrade, one worker thread per send.

Surface:
    serve(host, port, on_connect) -> ServerHandle
        on_connect(conn: WSConn) -> None  (runs in its own thread)
    WSConn.send_text(s)
    WSConn.recv_text(timeout=None) -> str | None   (None on close)
    WSConn.close()

Limitations (intentional):
    - text frames only on the public API (binary frames are dropped)
    - no permessage-deflate
    - no TLS (use ws://, not wss://)
    - one upgrade per HTTP socket (no keep-alive HTTP requests reused)
"""

from __future__ import annotations

import base64
import hashlib
import queue
import socket
import struct
import threading
from dataclasses import dataclass
from typing import Callable, Optional

_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

# Opcodes
_OP_CONT = 0x0
_OP_TEXT = 0x1
_OP_BIN = 0x2
_OP_CLOSE = 0x8
_OP_PING = 0x9
_OP_PONG = 0xA


def _accept_key(client_key: str) -> str:
    sha = hashlib.sha1((client_key + _GUID).encode("ascii")).digest()
    return base64.b64encode(sha).decode("ascii")


def _read_exact(sock: socket.socket, n: int) -> Optional[bytes]:
    buf = bytearray()
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            return None
        buf.extend(chunk)
    return bytes(buf)


def _read_http_request(sock: socket.socket) -> Optional[dict]:
    """Read an HTTP/1.1 request line + headers. Returns dict or None on EOF."""
    sock.settimeout(10.0)
    data = bytearray()
    while b"\r\n\r\n" not in data:
        chunk = sock.recv(4096)
        if not chunk:
            return None
        data.extend(chunk)
        if len(data) > 64 * 1024:
            return None
    header_blob, _, _ = data.partition(b"\r\n\r\n")
    lines = header_blob.decode("iso-8859-1").split("\r\n")
    if not lines:
        return None
    method, path, _proto = (lines[0].split(" ", 2) + ["", ""])[:3]
    headers = {}
    for line in lines[1:]:
        if ":" in line:
            k, _, v = line.partition(":")
            headers[k.strip().lower()] = v.strip()
    sock.settimeout(None)
    return {"method": method, "path": path, "headers": headers}


def _do_handshake(sock: socket.socket, req: dict) -> bool:
    h = req["headers"]
    if h.get("upgrade", "").lower() != "websocket":
        sock.sendall(b"HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
        return False
    key = h.get("sec-websocket-key")
    if not key:
        sock.sendall(b"HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
        return False
    accept = _accept_key(key)
    resp = (
        "HTTP/1.1 101 Switching Protocols\r\n"
        "Upgrade: websocket\r\n"
        "Connection: Upgrade\r\n"
        f"Sec-WebSocket-Accept: {accept}\r\n\r\n"
    )
    sock.sendall(resp.encode("ascii"))
    return True


@dataclass
class _Frame:
    fin: bool
    opcode: int
    payload: bytes


def _read_frame(sock: socket.socket) -> Optional[_Frame]:
    hdr = _read_exact(sock, 2)
    if hdr is None:
        return None
    b1, b2 = hdr[0], hdr[1]
    fin = (b1 & 0x80) != 0
    opcode = b1 & 0x0F
    masked = (b2 & 0x80) != 0
    length = b2 & 0x7F
    if length == 126:
        ext = _read_exact(sock, 2)
        if ext is None:
            return None
        length = struct.unpack(">H", ext)[0]
    elif length == 127:
        ext = _read_exact(sock, 8)
        if ext is None:
            return None
        length = struct.unpack(">Q", ext)[0]
    mask_key = b""
    if masked:
        mask_key = _read_exact(sock, 4)
        if mask_key is None:
            return None
    payload = _read_exact(sock, length) if length else b""
    if payload is None and length:
        return None
    if masked and payload:
        payload = bytes(b ^ mask_key[i % 4] for i, b in enumerate(payload))
    return _Frame(fin=fin, opcode=opcode, payload=payload or b"")


def _write_frame(sock: socket.socket, opcode: int, payload: bytes) -> None:
    b1 = 0x80 | (opcode & 0x0F)  # FIN=1, no fragmentation server-side
    n = len(payload)
    if n < 126:
        header = struct.pack(">BB", b1, n)
    elif n < (1 << 16):
        header = struct.pack(">BBH", b1, 126, n)
    else:
        header = struct.pack(">BBQ", b1, 127, n)
    sock.sendall(header + payload)


class WSConn:
    """Public connection handle for a single websocket session."""

    def __init__(self, sock: socket.socket):
        self._sock = sock
        self._send_lock = threading.Lock()
        self._recv_q: "queue.Queue[Optional[str]]" = queue.Queue()
        self._closed = threading.Event()
        self._reader = threading.Thread(target=self._read_loop, daemon=True)
        self._reader.start()

    # --- public ---

    def send_text(self, s: str) -> None:
        if self._closed.is_set():
            return
        data = s.encode("utf-8")
        with self._send_lock:
            try:
                _write_frame(self._sock, _OP_TEXT, data)
            except OSError:
                self._mark_closed()

    def recv_text(self, timeout: Optional[float] = None) -> Optional[str]:
        try:
            return self._recv_q.get(timeout=timeout)
        except queue.Empty:
            return None

    def close(self) -> None:
        if self._closed.is_set():
            return
        with self._send_lock:
            try:
                _write_frame(self._sock, _OP_CLOSE, b"\x03\xe8")  # 1000
            except OSError:
                pass
        self._mark_closed()

    # --- internal ---

    def _mark_closed(self) -> None:
        if self._closed.is_set():
            return
        self._closed.set()
        try:
            self._sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        try:
            self._sock.close()
        except OSError:
            pass
        self._recv_q.put(None)

    def _read_loop(self) -> None:
        buf_op = None
        buf_data = bytearray()
        try:
            while not self._closed.is_set():
                frame = _read_frame(self._sock)
                if frame is None:
                    break
                op = frame.opcode
                if op == _OP_PING:
                    with self._send_lock:
                        _write_frame(self._sock, _OP_PONG, frame.payload)
                    continue
                if op == _OP_PONG:
                    continue
                if op == _OP_CLOSE:
                    with self._send_lock:
                        try:
                            _write_frame(self._sock, _OP_CLOSE, b"")
                        except OSError:
                            pass
                    break
                if op == _OP_CONT:
                    if buf_op is None:
                        break  # protocol error
                    buf_data.extend(frame.payload)
                else:
                    buf_op = op
                    buf_data = bytearray(frame.payload)
                if frame.fin:
                    if buf_op == _OP_TEXT:
                        try:
                            self._recv_q.put(buf_data.decode("utf-8"))
                        except UnicodeDecodeError:
                            pass
                    # binary frames silently dropped
                    buf_op = None
                    buf_data = bytearray()
        finally:
            self._mark_closed()


@dataclass
class ServerHandle:
    host: str
    port: int
    _sock: socket.socket
    _thread: threading.Thread
    _stop: threading.Event

    @property
    def url(self) -> str:
        return f"ws://{self.host}:{self.port}/"

    def stop(self) -> None:
        self._stop.set()
        try:
            self._sock.close()
        except OSError:
            pass
        self._thread.join(timeout=2.0)


def serve(
    host: str,
    port: int,
    on_connect: Callable[[WSConn], None],
    *,
    on_request: Optional[Callable[[dict], bool]] = None,
) -> ServerHandle:
    """Start a server. Returns immediately with a ServerHandle.

    on_request, if given, is called with the parsed request dict before
    the handshake; return False to reject (a 404 is sent).
    """
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((host, port))
    srv.listen(8)
    actual_port = srv.getsockname()[1]
    stop = threading.Event()

    def accept_loop() -> None:
        while not stop.is_set():
            try:
                client, _addr = srv.accept()
            except OSError:
                return
            t = threading.Thread(
                target=_handle_client,
                args=(client, on_connect, on_request),
                daemon=True,
            )
            t.start()

    th = threading.Thread(target=accept_loop, daemon=True)
    th.start()
    return ServerHandle(host=host, port=actual_port, _sock=srv, _thread=th, _stop=stop)


def _handle_client(
    sock: socket.socket,
    on_connect: Callable[[WSConn], None],
    on_request: Optional[Callable[[dict], bool]],
) -> None:
    try:
        req = _read_http_request(sock)
        if req is None:
            sock.close()
            return
        if on_request is not None and not on_request(req):
            sock.sendall(b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n")
            sock.close()
            return
        if not _do_handshake(sock, req):
            sock.close()
            return
        conn = WSConn(sock)
        try:
            on_connect(conn)
        finally:
            conn.close()
    except Exception:
        try:
            sock.close()
        except OSError:
            pass


if __name__ == "__main__":
    # smoke: echo server on a random port
    import sys

    def echo(c: WSConn) -> None:
        while True:
            m = c.recv_text(timeout=30)
            if m is None:
                return
            c.send_text("echo:" + m)

    h = serve("127.0.0.1", 0, echo)
    print(h.url, flush=True)
    try:
        threading.Event().wait()
    except KeyboardInterrupt:
        h.stop()
        sys.exit(0)
