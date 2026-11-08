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
    ap.add_argument("--root", help="workspace root, as an editor would open it (default: the files' common directory)")
    ap.add_argument("--no-infer-roots", action="store_true", help="do not let mojols infer package roots")
    ap.add_argument("files", nargs="+")
    args = ap.parse_args()

    files = [Path(f).resolve() for f in args.files]
    root = Path(args.root).resolve() if args.root else Path(os.path.commonpath([str(f.parent) for f in files]))
    out_dir = Path(__file__).resolve().parent.parent / ".probe"
    out_dir.mkdir(exist_ok=True)
    log_file, trace_file = out_dir / "mojols.log", out_dir / "trace.jsonl"
    argv = [args.mojols, "serve", "--log-level", "debug", "--log-file", str(log_file), "--trace-file", str(trace_file)]
    if args.backend:
        argv += ["--backend", args.backend]
    for inc in args.includes:
        argv += ["-I", inc]
    if args.no_infer_roots:
        argv += ["--infer-roots=false"]
    print(f"logs: {log_file}\ntrace: {trace_file}\nworkspace root: {root}\n", flush=True)

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
    print("capabilities:", ", ".join(caps), flush=True)
    c.notify("initialized", {})

    def show(label, ms, detail):
        print(f"  {label:<12} {ms:>8} {detail}", flush=True)

    def outcome(resp):
        if "error" in resp:
            return f"ERROR {resp['error'].get('code')}: {resp['error'].get('message')}"
        r = resp.get("result")
        if not r:
            return "empty"
        if isinstance(r, list):
            return f"{len(r)} result(s)"
        return "ok"

    for f in files:
        text = f.read_text()
        uri = f.as_uri()
        print(f"\n{f.name} ({len(text.splitlines())} lines)", flush=True)
        for attempt in ("cold", "warm"):
            print(f" {attempt}:", flush=True)
            if attempt == "warm":
                t_close = time.monotonic()
                c.notify("textDocument/didClose", {"textDocument": {"uri": uri}})
                # The server clears a closed file's diagnostics; don't mistake
                # that message for the reopened file's parse.
                c.wait_diagnostics(uri, t_close, timeout=2)
            t0 = time.monotonic()
            c.notify("textDocument/didOpen", {"textDocument": {"uri": uri, "languageId": "mojo", "version": 1, "text": text}})
            print("  waiting for diagnostics (up to 60 s)...", flush=True)
            parse, diags = c.wait_diagnostics(uri, t0)
            if parse is None:
                show("diagnostics", "timeout", "no publishDiagnostics within 60 s")
            elif any(d.get("source") == "mojols" for d in diags):
                show("diagnostics", f"{parse*1000:.0f} ms", "QUARANTINED by mojols: the backend crashes on this file")
            else:
                errs = [d for d in diags if d.get("severity") == 1]
                show("diagnostics", f"{parse*1000:.0f} ms", f"{len(errs)} error(s), {len(diags)} total")
                for d in errs[:3]:
                    show("", "", f"line {d['range']['start']['line']+1}: {d['message'][:100]}")
            sym, t_sym = c.call("textDocument/documentSymbol", {"textDocument": {"uri": uri}})
            show("symbols", f"{t_sym*1000:.0f} ms", outcome(sym))
            pos = first_symbol_position(text)
            hov, t_hov = c.call("textDocument/hover", {"textDocument": {"uri": uri}, "position": pos})
            show("hover", f"{t_hov*1000:.0f} ms", f"{outcome(hov)} at {pos['line']+1}:{pos['character']+1}")
            call = first_call_position(text)
            if call:
                d, t_def = c.call("textDocument/definition", {"textDocument": {"uri": uri}, "position": call})
                show("definition", f"{t_def*1000:.0f} ms", f"{outcome(d)} at {call['line']+1}:{call['character']+1}")

    status, _ = c.call("mojols/status", None)
    st = status.get("result", {})
    print(f"\nbackend pid {st.get('backendPid')}, restarts {st.get('restarts')}, timeouts {st.get('timeouts')}, "
          f"last backend error: {st.get('lastBackendError') or 'none'}")
    for q in st.get("quarantined") or []:
        print(f"quarantined: {q}")
    print(f"degraded: {st.get('degraded')}")
    print("Errors on files that compile usually mean missing -I include dirs (open question 3).")
    c.call("shutdown", None)
    c.notify("exit", None)
    c.proc.wait(timeout=10)


if __name__ == "__main__":
    main()
