#!/usr/bin/env python3
"""M0 probe: drive mojols against the real mojo-lsp-server and measure it.

Answers the M0 open questions with numbers instead of guesses:
  1. Does `definition` work on a file that only mojols opened (never shown in an editor)?
  2. How long does the first parse of a file take, cold and warm?
  3. Which -I include dirs does a project need for imports to resolve?

Usage (from the repo root, after `make build`):
  python3 scripts/m0_probe.py [--mojols bin/mojols] [-I DIR ...] FILE.mojo [FILE.mojo ...]

Example against Modular's stdlib checkout:
  python3 scripts/m0_probe.py ~/Documents/personal/modular/mojo/stdlib/std/collections/list.mojo

Standard library only; no dependencies.
"""

import argparse
import json
import os
import re
import subprocess
import sys
import threading
import time
from pathlib import Path


class Client:
    def __init__(self, argv):
        self.proc = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.next_id = 0
        self.lock = threading.Condition()
        self.responses = {}
        self.notifications = []
        threading.Thread(target=self._read, daemon=True).start()
        threading.Thread(target=self._drain_stderr, daemon=True).start()

    def _drain_stderr(self):
        for line in self.proc.stderr:
            if os.environ.get("PROBE_VERBOSE"):
                sys.stderr.write(line.decode(errors="replace"))

    def _read(self):
        out = self.proc.stdout
        while True:
            length = None
            while True:
                line = out.readline()
                if not line:
                    return
                line = line.strip()
                if not line:
                    break
                k, _, v = line.decode().partition(":")
                if k.lower() == "content-length":
                    length = int(v)
            msg = json.loads(out.read(length))
            with self.lock:
                if "method" in msg and "id" in msg:
                    # Server -> client request: answer with null so nothing blocks.
                    self._send({"jsonrpc": "2.0", "id": msg["id"], "result": None})
                elif "method" in msg:
                    self.notifications.append((time.monotonic(), msg))
                else:
                    self.responses[msg["id"]] = msg
                self.lock.notify_all()

    def _send(self, msg):
        body = json.dumps(msg).encode()
        self.proc.stdin.write(b"Content-Length: %d\r\n\r\n" % len(body) + body)
        self.proc.stdin.flush()

    def notify(self, method, params):
        with self.lock:
            self._send({"jsonrpc": "2.0", "method": method, "params": params})

    def call(self, method, params, timeout=120):
        with self.lock:
            self.next_id += 1
            rid = self.next_id
            self._send({"jsonrpc": "2.0", "id": rid, "method": method, "params": params})
            start = time.monotonic()
            while rid not in self.responses:
                if not self.lock.wait(timeout=timeout - (time.monotonic() - start)):
                    raise TimeoutError(method)
            return self.responses.pop(rid), time.monotonic() - start

    def wait_diagnostics(self, uri, since, timeout=60):
        with self.lock:
            end = time.monotonic() + timeout
            while True:
                for t, m in self.notifications:
                    if t >= since and m["method"] == "textDocument/publishDiagnostics" and m["params"]["uri"] == uri:
                        return t - since, m["params"]["diagnostics"]
                if not self.lock.wait(timeout=max(0, end - time.monotonic())):
                    return None, None


def first_symbol_position(text):
    """Position of the first name defined with def/fn/struct/trait."""
    for i, line in enumerate(text.splitlines()):
        m = re.match(r"\s*(def|fn|struct|trait)\s+([A-Za-z_]\w*)", line)
        if m:
            return {"line": i, "character": m.start(2)}
    return {"line": 0, "character": 0}


def first_call_position(text):
    """Position of the first identifier followed by '(' inside a body: a likely reference."""
    for i, line in enumerate(text.splitlines()):
        if not line.startswith((" ", "\t")):
            continue
        m = re.search(r"\b([A-Za-z_]\w*)\(", line)
        if m and m.group(1) not in {"if", "for", "while", "return", "print"}:
            return {"line": i, "character": m.start(1)}
    return None


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--mojols", default=str(Path(__file__).resolve().parent.parent / "bin" / "mojols"))
    ap.add_argument("--backend", help="explicit mojo-lsp-server path")
    ap.add_argument("-I", dest="includes", action="append", default=[])
    ap.add_argument("files", nargs="+")
    args = ap.parse_args()

    files = [Path(f).resolve() for f in args.files]
    root = Path(os.path.commonpath([str(f.parent) for f in files]))
    argv = [args.mojols, "serve", "--log-level", "debug"]
    if args.backend:
        argv += ["--backend", args.backend]
    for inc in args.includes:
        argv += ["-I", inc]

    c = Client(argv)
    init, t = c.call("initialize", {"processId": os.getpid(), "rootUri": root.as_uri(), "capabilities": {
        "textDocument": {"hover": {"contentFormat": ["markdown", "plaintext"]},
                         "documentSymbol": {"hierarchicalDocumentSymbolSupport": True}},
        "general": {"positionEncodings": ["utf-16"]}}})
    if "error" in init:
        sys.exit(f"initialize failed: {init['error']}")
    info = init["result"].get("serverInfo", {})
    print(f"{info.get('name')} {info.get('version')}  (initialize {t*1000:.0f} ms)")
    caps = sorted(k for k, v in init["result"]["capabilities"].items() if v)
    print("capabilities:", ", ".join(caps))
    c.notify("initialized", {})

    rows = []
    for f in files:
        text = f.read_text()
        uri = f.as_uri()
        for attempt in ("cold", "warm"):
            if attempt == "warm":
                c.notify("textDocument/didClose", {"textDocument": {"uri": uri}})
            t0 = time.monotonic()
            c.notify("textDocument/didOpen", {"textDocument": {"uri": uri, "languageId": "mojo", "version": 1, "text": text}})
            parse, diags = c.wait_diagnostics(uri, t0)
            sym, t_sym = c.call("textDocument/documentSymbol", {"textDocument": {"uri": uri}})
            pos = first_symbol_position(text)
            hov, t_hov = c.call("textDocument/hover", {"textDocument": {"uri": uri}, "position": pos})
            call = first_call_position(text)
            d_ok, t_def = "-", 0.0
            if call:
                d, t_def = c.call("textDocument/definition", {"textDocument": {"uri": uri}, "position": call})
                res = d.get("result")
                d_ok = "error" if "error" in d else ("yes" if res else "empty")
            errors = sum(1 for x in (diags or []) if x.get("severity") == 1)
            rows.append((f.name, attempt, parse, errors, len(sym.get("result") or []), t_sym, t_hov,
                         "yes" if hov.get("result") else "empty", t_def, d_ok))

    print()
    print(f"{'file':28} {'run':5} {'parse':>8} {'errs':>5} {'syms':>5} {'symbols':>8} {'hover':>8} {'':6} {'def':>8} {'':6}")
    for name, run, parse, errs, nsym, t_sym, t_hov, hov_ok, t_def, def_ok in rows:
        p = f"{parse*1000:.0f}ms" if parse is not None else "timeout"
        print(f"{name[:28]:28} {run:5} {p:>8} {errs:>5} {nsym:>5} {t_sym*1000:>6.0f}ms {t_hov*1000:>6.0f}ms {hov_ok:6} {t_def*1000:>6.0f}ms {def_ok:6}")

    status, _ = c.call("mojols/status", None)
    st = status.get("result", {})
    print(f"\nbackend pid {st.get('backendPid')}, restarts {st.get('restarts')}, timeouts {st.get('timeouts')}")
    print("Errors > 0 on files that compile usually means missing -I include dirs (open question 3).")
    c.call("shutdown", None)
    c.notify("exit", None)
    c.proc.wait(timeout=10)


if __name__ == "__main__":
    main()
