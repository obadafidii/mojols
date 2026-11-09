#!/usr/bin/env python3
"""Record mojo-lsp-server's go-to-definition answer at every identifier.

For each .mojo file under CORPUS, `mojols debug-parse` lists the identifier
occurrences (plain names, members and keyword arguments). This opens the
file in a bare mojo-lsp-server and asks textDocument/definition at the start
of each one, then writes one JSON line per file:

  {"file": ..., "sha": ..., "defs": [[line, char, name, kind, [uri, l0, c0, l1, c1] | null], ...]}

The result is the ground truth mojols' resolver is scored against
(scripts/score_resolver.py). Files already in OUT with the same content hash
are skipped, so reruns only redo what changed.

Usage:
  python3 scripts/oracle_definitions.py CORPUS --mojols bin/mojols --server mojo-lsp-server --out oracle.jsonl [-I DIR] [--jobs 2]
"""

import argparse
import hashlib
import json
import os
import subprocess
import sys
import threading
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
from m0_probe import Client  # noqa: E402

KINDS = {"name", "member", "kwarg"}


def start(argv):
    c = Client(argv)
    c.call("initialize", {"processId": os.getpid(), "rootUri": None, "capabilities": {}})
    c.notify("initialized", {})
    return c


def one_file(c, path, refs):
    uri = "file://" + str(path)
    text = path.read_text(errors="replace")
    t0 = time.monotonic()
    c.notify("textDocument/didOpen", {"textDocument": {"uri": uri, "languageId": "mojo", "version": 1, "text": text}})
    c.wait_diagnostics(uri, t0, timeout=120)
    out = []
    for r in refs:
        st = r["range"]["start"]
        resp, _ = c.call("textDocument/definition", {"textDocument": {"uri": uri}, "position": st}, timeout=60)
        res = resp.get("result")
        loc = None
        if isinstance(res, list) and res:
            res = res[0]
        if isinstance(res, dict) and "uri" in res:
            rg = res["range"]
            loc = [res["uri"], rg["start"]["line"], rg["start"]["character"], rg["end"]["line"], rg["end"]["character"]]
        out.append([st["line"], st["character"], r["name"], r["kind"], loc])
    c.notify("textDocument/didClose", {"textDocument": {"uri": uri}})
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("corpus")
    ap.add_argument("--mojols", required=True)
    ap.add_argument("--server", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("-I", dest="include", action="append", default=[])
    ap.add_argument("--jobs", type=int, default=2)
    args = ap.parse_args()

    files = sorted(Path(args.corpus).rglob("*.mojo"))
    done = {}
    if Path(args.out).exists():
        for line in Path(args.out).read_text().splitlines():
            d = json.loads(line)
            done[d["file"]] = d["sha"]
    todo = []
    for f in files:
        sha = hashlib.sha256(f.read_bytes()).hexdigest()[:16]
        if done.get(str(f)) != sha:
            todo.append((f, sha))
    print(f"{len(files)} files, {len(todo)} to query", flush=True)
    if not todo:
        return

    parsed = {}
    for i in range(0, len(todo), 100):
        chunk = [str(f) for f, _ in todo[i:i + 100]]
        res = subprocess.run([args.mojols, "debug-parse", *chunk], capture_output=True, text=True, check=True)
        for line in res.stdout.splitlines():
            d = json.loads(line)
            parsed[d["file"]] = [r for r in d["refs"] or [] if r["kind"] in KINDS]

    argv = [args.server]
    for d in args.include:
        argv += ["-I", d]
    lock = threading.Lock()
    queue = list(todo)
    stats = {"files": 0, "refs": 0, "resolved": 0, "failed": 0}
    out = open(args.out, "a")
    t0 = time.monotonic()

    def worker():
        c = start(argv)
        while True:
            with lock:
                if not queue:
                    break
                f, sha = queue.pop(0)
            try:
                defs = one_file(c, f, parsed.get(str(f), []))
            except Exception as e:
                with lock:
                    stats["failed"] += 1
                    print(f"  failed {f}: {e!r}", flush=True)
                if c.proc.poll() is not None:
                    c = start(argv)
                continue
            with lock:
                out.write(json.dumps({"file": str(f), "sha": sha, "defs": defs}) + "\n")
                out.flush()
                stats["files"] += 1
                stats["refs"] += len(defs)
                stats["resolved"] += sum(1 for d in defs if d[4])
                if stats["files"] % 50 == 0:
                    print(f"{stats['files']}/{len(todo)} files, {stats['refs']} refs, {time.monotonic()-t0:.0f}s", flush=True)
        c.proc.kill()

    threads = [threading.Thread(target=worker) for _ in range(args.jobs)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    print(f"done: {stats['files']} files, {stats['refs']} refs, {stats['resolved']} resolved by the server, "
          f"{stats['failed']} files failed, {time.monotonic()-t0:.0f}s", flush=True)


if __name__ == "__main__":
    main()
