#!/usr/bin/env python3
"""Reproduce a mojo-lsp-server abort on textDocument/completion.

Usage: python3 repro_lsp.py [path/to/mojo-lsp-server]

Writes `from std.iter import zip` to a temporary file, opens it in a bare
mojo-lsp-server, waits for diagnostics, then requests completion on line 1,
character 0. Prints whether the server survived.
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import time

server = sys.argv[1] if len(sys.argv) > 1 else shutil.which("mojo-lsp-server")
if not server:
    sys.exit("mojo-lsp-server not found; pass its path")

tmp = tempfile.mkdtemp()
path = os.path.join(tmp, "repro.mojo")
text = "from std.iter import zip\n\n"
with open(path, "w") as f:
    f.write(text)
uri = "file://" + path

proc = subprocess.Popen([server], cwd=tmp, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                        stderr=subprocess.PIPE)


def send(msg):
    body = json.dumps(msg).encode()
    proc.stdin.write(b"Content-Length: %d\r\n\r\n" % len(body) + body)
    proc.stdin.flush()


def read():
    """Next message from the server, or None if it exited."""
    length = None
    while True:
        line = proc.stdout.readline()
        if not line:
            return None
        line = line.strip()
        if not line:
            break
        key, _, value = line.decode().partition(":")
        if key.lower() == "content-length":
            length = int(value)
    return json.loads(proc.stdout.read(length))


def until(pred):
    while True:
        msg = read()
        if msg is None or pred(msg):
            return msg


send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
      "params": {"processId": os.getpid(), "rootUri": "file://" + tmp, "capabilities": {}}})
until(lambda m: m.get("id") == 1)
send({"jsonrpc": "2.0", "method": "initialized", "params": {}})
send({"jsonrpc": "2.0", "method": "textDocument/didOpen",
      "params": {"textDocument": {"uri": uri, "languageId": "mojo", "version": 1, "text": text}}})
until(lambda m: m.get("method") == "textDocument/publishDiagnostics")

send({"jsonrpc": "2.0", "id": 2, "method": "textDocument/completion",
      "params": {"textDocument": {"uri": uri}, "position": {"line": 1, "character": 0}}})
reply = until(lambda m: m.get("id") == 2)
time.sleep(1)

if reply is None or proc.poll() is not None:
    proc.wait()
    err = proc.stderr.read().decode(errors="replace")
    lines = [l for l in err.splitlines() if "LLVM ERROR" in l or l.startswith(("0.", "1."))]
    print(f"CRASHED (exit status {proc.returncode})")
    print("\n".join(lines) or err[-2000:])
    sys.exit(1)

items = reply.get("result") or {}
items = items.get("items", items) if isinstance(items, dict) else items
print(f"OK: completion returned {len(items or [])} items")
send({"jsonrpc": "2.0", "id": 3, "method": "shutdown"})
send({"jsonrpc": "2.0", "method": "exit"})
proc.wait(timeout=5)
