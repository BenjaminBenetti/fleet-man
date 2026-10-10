#!/usr/bin/env python3
"""Host end of the network itests: TCP and WebSocket clients that drive
traffic through a fleet port forward or the browser proxy, against the
in-instance servers of integration/fixture-net/itest-net.pl.

Each subcommand checks its own result: it exits 0 and prints one summary line
when the traffic came through intact, or exits 1 with what went wrong.

  upload   <port> <bytes> [--parallel N] [--late S]
                                          send random bytes, half-close, and
                                          expect the sink's "bytes= sha256="
                                          reply to match (--late: and to come
                                          no sooner than S seconds after the
                                          half-close, from a latesink)
  download <port> <bytes> <sha256>        read to EOF and expect that content
  echo     <port> <bytes>                 stream while reading the echo back
  closed   <port>                         expect the forward to close the
                                          connection with no data
  ws       <proxy-port> <host:port> <text>
                                          CONNECT through the proxy, upgrade
                                          to a WebSocket and expect the echo
"""
import argparse
import base64
import hashlib
import os
import socket
import sys
import threading
import time

TIMEOUT = 60.0


def connect(port):
    return socket.create_connection(("127.0.0.1", port), timeout=TIMEOUT)


def read_to_eof(sock):
    chunks = []
    while True:
        data = sock.recv(65536)
        if not data:
            return b"".join(chunks)
        chunks.append(data)


def fail(msg):
    print(msg, file=sys.stderr)
    sys.exit(1)


def upload_once(port, size, late, results, index):
    try:
        data = os.urandom(size)
        want = "bytes=%d sha256=%s" % (size, hashlib.sha256(data).hexdigest())
        with connect(port) as sock:
            sock.sendall(data)
            # The half-close under test: the sink replies only after EOF, and
            # the reply must still reach this (read) side.
            sock.shutdown(socket.SHUT_WR)
            closed_at = time.monotonic()
            reply = read_to_eof(sock).decode(errors="replace").strip()
            waited = time.monotonic() - closed_at
        if reply != want:
            results[index] = "reply %r, want %r (%.1fs after the half-close)" % (reply, want, waited)
        elif waited < late - 0.5:
            # (0.5s of slack: the sink starts counting at EOF, a little after
            # this side half-closed.)
            results[index] = "reply came %.1fs after the half-close, want %gs (a latesink)" % (waited, late)
        else:
            results[index] = None
    except OSError as err:
        results[index] = "connection %d: %s" % (index, err)


def cmd_upload(args):
    results = [None] * args.parallel
    threads = [threading.Thread(target=upload_once, args=(args.port, args.bytes, args.late, results, i))
               for i in range(args.parallel)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    errors = [r for r in results if r]
    if errors:
        fail("upload: %d of %d connections failed: %s" % (len(errors), args.parallel, "; ".join(errors)))
    print("upload ok: %d x %d bytes, each answered %safter half-close" % (
        args.parallel, args.bytes, "%gs " % args.late if args.late else ""))


def cmd_download(args):
    with connect(args.port) as sock:
        data = read_to_eof(sock)
    got = hashlib.sha256(data).hexdigest()
    if len(data) != args.bytes or got != args.sha256:
        fail("download: got %d bytes sha256=%s, want %d bytes sha256=%s" % (len(data), got, args.bytes, args.sha256))
    print("download ok: %d bytes" % len(data))


def cmd_echo(args):
    data = os.urandom(args.bytes)
    errors = []

    def send(sock):
        try:
            view = memoryview(data)
            for off in range(0, len(data), 16384):
                sock.sendall(view[off:off + 16384])
            sock.shutdown(socket.SHUT_WR)
        except OSError as err:
            errors.append("send: %s" % err)

    with connect(args.port) as sock:
        sender = threading.Thread(target=send, args=(sock,))
        sender.start()
        got = read_to_eof(sock)
        sender.join()
    if errors:
        fail("echo: " + "; ".join(errors))
    if got != data:
        fail("echo: got %d bytes back (sha256=%s), sent %d (sha256=%s)" % (
            len(got), hashlib.sha256(got).hexdigest(), len(data), hashlib.sha256(data).hexdigest()))
    print("echo ok: %d bytes" % len(data))


def cmd_closed(args):
    try:
        with socket.create_connection(("127.0.0.1", args.port), timeout=15) as sock:
            data = sock.recv(1)
    except ConnectionResetError:
        data = b""
    except socket.timeout:
        fail("closed: the connection stayed open (no EOF within 15s)")
    if data:
        fail("closed: got data %r from a port nothing listens on" % data)
    print("closed ok: the forward closed the connection")


def read_head(sock):
    head = b""
    while b"\r\n\r\n" not in head:
        data = sock.recv(1)
        if not data:
            break
        head += data
    return head.decode(errors="replace")


def recv_exact(sock, n):
    buf = b""
    while len(buf) < n:
        data = sock.recv(n - len(buf))
        if not data:
            fail("ws: connection closed mid-frame (%d of %d bytes)" % (len(buf), n))
        buf += data
    return buf


def cmd_ws(args):
    if len(args.text) > 100:
        fail("ws: keep the text under 100 bytes (single-byte frame lengths)")
    with connect(args.proxy_port) as sock:
        # How a browser reaches ws:// through an HTTP proxy: a CONNECT tunnel.
        sock.sendall(("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n" % (args.target, args.target)).encode())
        head = read_head(sock)
        status = head.split("\r\n", 1)[0]
        if " 200" not in status:
            fail("ws: CONNECT %s answered %r" % (args.target, status))
        key = base64.b64encode(os.urandom(16)).decode()
        sock.sendall((
            "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
            "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n" % (args.target, key)).encode())
        head = read_head(sock)
        status = head.split("\r\n", 1)[0]
        if " 101" not in status:
            fail("ws: upgrade answered %r" % status)
        want_accept = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
        if ("sec-websocket-accept: %s" % want_accept.lower()) not in head.lower():
            fail("ws: bad Sec-WebSocket-Accept in %r" % head)
        payload = args.text.encode()
        mask = os.urandom(4)
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        sock.sendall(bytes([0x81, 0x80 | len(payload)]) + mask + masked)
        b0, b1 = recv_exact(sock, 2)
        reply = recv_exact(sock, b1 & 0x7F).decode(errors="replace")
        if b0 != 0x81 or reply != "echo:" + args.text:
            fail("ws: got frame 0x%02x %r, want a text frame 'echo:%s'" % (b0, reply, args.text))
    print("ws ok: %s" % reply)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("upload")
    p.add_argument("port", type=int)
    p.add_argument("bytes", type=int)
    p.add_argument("--parallel", type=int, default=1)
    p.add_argument("--late", type=float, default=0)
    p.set_defaults(fn=cmd_upload)
    p = sub.add_parser("download")
    p.add_argument("port", type=int)
    p.add_argument("bytes", type=int)
    p.add_argument("sha256")
    p.set_defaults(fn=cmd_download)
    p = sub.add_parser("echo")
    p.add_argument("port", type=int)
    p.add_argument("bytes", type=int)
    p.set_defaults(fn=cmd_echo)
    p = sub.add_parser("closed")
    p.add_argument("port", type=int)
    p.set_defaults(fn=cmd_closed)
    p = sub.add_parser("ws")
    p.add_argument("proxy_port", type=int)
    p.add_argument("target")
    p.add_argument("text")
    p.set_defaults(fn=cmd_ws)
    args = parser.parse_args()
    try:
        args.fn(args)
    except OSError as err:
        fail("%s: %s" % (args.cmd, err))


if __name__ == "__main__":
    main()
