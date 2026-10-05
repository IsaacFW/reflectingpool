"""Probe the real server on loopback using only a disposable empty pool."""
import os
import select
import socket
import subprocess
import tempfile
import time

with tempfile.TemporaryDirectory(prefix="rp-http-audit-") as fixture:
    root = os.path.join(fixture, "pool")
    data = os.path.join(fixture, "data")
    os.mkdir(root)
    with socket.socket() as reservation:
        reservation.bind(("127.0.0.1", 0))
        port = reservation.getsockname()[1]
    env = dict(os.environ, RP_ROOTS=root, RP_DATA=data,
               RP_LISTEN=f"127.0.0.1:{port}", RP_INSECURE_HTTP="1")
    binary = os.environ.get("RP_AUDIT_BINARY", "/tmp/reflectingpool-audit")
    proc = subprocess.Popen([binary, "serve"],
                            env=env, stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL)
    try:
        deadline = time.monotonic() + 10
        while True:
            try:
                conn = socket.create_connection(("127.0.0.1", port), timeout=1)
                break
            except OSError:
                if proc.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError("fixture server did not start")
                time.sleep(0.05)
        with conn:
            conn.sendall((f"POST /api/login HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\n"
                          "Content-Type: application/json\r\nContent-Length: 1024\r\n"
                          "Connection: close\r\n\r\n{").encode())
            readable, _, _ = select.select([conn], [], [], 12)
            if readable:
                raise RuntimeError(f"unexpected response or connection close: {conn.recv(4096)!r}")
            print("CONFIRMED: unauthenticated incomplete login body remains open after 12 seconds,")
            print("beyond production ReadHeaderTimeout=10s; no body-read deadline terminates it.")
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=15)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
