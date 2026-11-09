#!/usr/bin/env python3
"""Replay mojols crash bundles against a bare mojo-lsp-server (no mojols).

mojols --crash-dir DIR saves one JSON bundle per backend crash: the requests
in flight, the exact text of their documents and of every document open in
the backend, the initialize parameters and the backend command. This script
starts mojo-lsp-server directly, opens the documents, sends the in-flight
requests, and reports whether it crashes, several times per bundle.

It first tries the minimal case (only the documents the requests are about).
If that does not crash, it retries with every document that was open. A
bundle that crashes every time is a deterministic reproduction; for those it
writes a folder under <crash-dir>/repro/ with the document and a README, ready
to attach to a bug report for Modular.

Usage:
  python3 scripts/replay_crash.py .probe/<run>/crashes [--times 3] [--backend PATH]
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


class Bare:
    """A minimal LSP client for one mojo-lsp-server process."""

    def __init__(self, argv, cwd, env):
        self.proc = subprocess.Popen(argv, cwd=cwd or None, env=env, stdin=subprocess.PIPE,
                                     stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.lock = threading.Condition()
        self.responses, self.diagnosed, self.stderr = {}, set(), []
        self.next_id = 0
        threading.Thread(target=self._read, daemon=True).start()
        threading.Thread(target=self._err, daemon=True).start()

    def _err(self):
        for line in self.proc.stderr:
            self.stderr.append(line.decode(errors="replace").rstrip("\n"))

    def _read(self):
        out = self.proc.stdout
        while True:
            length = None
            while True:
                line = out.readline()
                if not line:
                    with self.lock:
                        self.lock.notify_all()
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
                    self._send({"jsonrpc": "2.0", "id": msg["id"], "result": None})
                elif msg.get("method") == "textDocument/publishDiagnostics":
                    self.diagnosed.add(msg["params"]["uri"])
                elif "id" in msg:
                    self.responses[msg["id"]] = msg
                self.lock.notify_all()

    def _send(self, msg):
        body = json.dumps(msg).encode()
        try:
            self.proc.stdin.write(b"Content-Length: %d\r\n\r\n" % len(body) + body)
            self.proc.stdin.flush()
        except (BrokenPipeError, OSError):
            pass

    def notify(self, method, params):
        with self.lock:
            self._send({"jsonrpc": "2.0", "method": method, "params": params})

    def request(self, method, params):
        with self.lock:
            self.next_id += 1
            self._send({"jsonrpc": "2.0", "id": self.next_id, "method": method, "params": params})
            return self.next_id

    def wait(self, done, timeout):
        """Wait until done() or the process dies. Returns 'ok', 'crashed' or 'timeout'."""
        end = time.monotonic() + timeout
        with self.lock:
            while True:
                if done():
                    return "ok"
                if self.proc.poll() is not None:
                    return "crashed"
                left = end - time.monotonic()
                if left <= 0:
                    return "timeout"
                self.lock.wait(timeout=min(left, 0.1))

    def crash_site(self):
        for l in self.stderr:
            if re.match(r"\s*0\.\s", l) or "LLVM ERROR" in l or "Assertion" in l:
                return l.strip()
        return f"exit status {self.proc.returncode}"

    def close(self):
        if self.proc.poll() is None:
            self.notify("exit", None)
            try:
                self.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.proc.kill()


def replay(bundle, docs, backend, env, parse_timeout=90, request_timeout=60):
    cmd = [backend or bundle["backend"]["path"]] + (bundle["backend"].get("args") or [])
    c = Bare(cmd, bundle["backend"].get("dir"), env)
    try:
        init = dict(json.loads(json.dumps(bundle["initParams"])) or {})
        init["processId"] = os.getpid()
        rid = c.request("initialize", init)
        if c.wait(lambda: rid in c.responses, 30) != "ok":
            return "crashed" if c.proc.poll() is not None else "no-init", c.crash_site()
        c.notify("initialized", {})
        for d in docs:
            c.notify("textDocument/didOpen", {"textDocument": {"uri": d["uri"], "languageId": d.get("languageId") or "mojo",
                                                               "version": d["version"], "text": d["text"]}})
        uris = {d["uri"] for d in docs}
        st = c.wait(lambda: uris <= c.diagnosed, parse_timeout)
        if st == "crashed":
            return "crashed while parsing", c.crash_site()
        ids = [c.request(f["method"], f["params"]) for f in bundle["inFlight"]]
        st = c.wait(lambda: all(i in c.responses for i in ids), request_timeout)
        if st == "crashed":
            return "crashed", c.crash_site()
        if st == "ok":
            time.sleep(1.0)  # a crash can follow the reply
            if c.proc.poll() is not None:
                return "crashed", c.crash_site()
        return ("survived" if st == "ok" else "hung"), ""
    finally:
        c.close()


def describe(bundle):
    out = []
    for f in bundle["inFlight"]:
        p = f["params"]
        where = Path(p.get("textDocument", {}).get("uri", "")).name
        if "position" in p:
            where += f":{p['position']['line'] + 1}:{p['position']['character'] + 1}"
        out.append(f"{f['method']} {where}")
    return "; ".join(out) or "(no request in flight)"


def write_repro(root, name, bundle, targets, site):
    d = root / "repro" / name
    d.mkdir(parents=True, exist_ok=True)
    lines = [f"# mojo-lsp-server crash: {site}", "",
             "Deterministic: replaying this through a bare mojo-lsp-server (no other client) crashes it every time.", "",
             "Steps: initialize, open the file(s) below, wait for diagnostics, then send:", ""]
    for f in bundle["inFlight"]:
        lines += [f"- `{f['method']}` with params:", "", "```json", json.dumps(f["params"], indent=2), "```", ""]
    for t in targets:
        fname = Path(t["uri"]).name
        (d / fname).write_text(t["text"])
        lines.append(f"File `{fname}` (as open in the editor, version {t['version']}) is saved next to this README.")
    lines += ["", "Server output at the crash:", "", "```", bundle.get("crashOutput", "")[:4000], "```"]
    (d / "README.md").write_text("\n".join(lines) + "\n")
    return d


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("crash_dir")
    ap.add_argument("--times", type=int, default=3)
    ap.add_argument("--backend", help="mojo-lsp-server to use (default: the one in each bundle)")
    args = ap.parse_args()
    root = Path(args.crash_dir)
    bundles = sorted(root.glob("crash-*.json"))
    if not bundles:
        sys.exit(f"no crash-*.json in {root}")
    env = dict(os.environ)
    deterministic = 0
    for path in bundles:
        b = json.loads(path.read_text())
        targets = [d for d in b["documents"] if d.get("target")]
        print(f"\n{path.name}\n  in flight: {describe(b)}", flush=True)
        if not b["inFlight"]:
            print("  nothing was in flight: not a request crash, skipped")
            continue
        verdict = None
        for label, docs in (("target documents only", targets), ("every open document", b["documents"])):
            if not docs or (label == "every open document" and len(docs) == len(targets)):
                continue
            results = [replay(b, docs, args.backend, env) for _ in range(args.times)]
            crashed = [r for r in results if r[0].startswith("crashed")]
            sites = sorted({s for _, s in crashed})
            print(f"  {label:22} ({len(docs)} docs): crashed {len(crashed)}/{len(results)}"
                  + (f"  [{results[0][0]}]" if not crashed else "") + "".join(f"\n      {s}" for s in sites), flush=True)
            if len(crashed) == len(results):
                verdict = (label, docs, sites[0] if sites else "")
                break
        if verdict:
            deterministic += 1
            d = write_repro(root, path.stem, b, [x for x in verdict[1] if x.get("target")] or verdict[1], verdict[2])
            print(f"  DETERMINISTIC with {verdict[0]}: reproduction in {d}")
    print(f"\n{deterministic} of {len(bundles)} bundles reproduce every time.")


if __name__ == "__main__":
    main()
