#!/usr/bin/env python3
"""Measure mojo-lsp-server memory as documents are opened, through mojols.

Opens stdlib files from a corpus that matches the installed toolchain in
growing batches (1, 5, 10, 20, 40 open documents), waits for each batch to
be parsed, and samples the backend's resident memory. Then it closes and
reopens everything a few times to see whether memory comes back (leaks).
These numbers decide mojols' memory cap: how many documents the backend
may hold open, and when to recycle it.

Usage:
  python3 scripts/memory_probe.py [--corpus .corpus/mojo-v1.1.0/Mojo/stdlib/std] [--max 40] [--cycles 3]
"""

import argparse
import os
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from m0_probe import Client  # noqa: E402

REPO = Path(__file__).resolve().parent.parent


def rss_mb(pid):
    """Resident set size of pid in MiB, or None."""
    if not pid:
        return None
    try:
        out = subprocess.run(["ps", "-o", "rss=", "-p", str(pid)], capture_output=True, text=True, timeout=5).stdout
        return int(out.strip()) / 1024
    except (ValueError, subprocess.SubprocessError):
        return None


def peak_rss(pid, seconds=1.5, samples=4):
    best = 0.0
    for _ in range(samples):
        v = rss_mb(pid)
        if v is not None and (best is None or v > best):
            best = v
        time.sleep(seconds / samples)
    return best


def wait_parsed(c, uris, since, timeout=120):
    """Wait until every uri has published diagnostics after `since`."""
    pending = set(uris)
    end = time.monotonic() + timeout
    with c.lock:
        while pending:
            for t, m in c.notifications:
                if t >= since and m["method"] == "textDocument/publishDiagnostics":
                    pending.discard(m["params"]["uri"])
            if not pending:
                break
            left = end - time.monotonic()
            if left <= 0 or not c.lock.wait(timeout=left):
                break
    return len(uris) - len(pending)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--mojols", default=str(REPO / "bin" / "mojols"))
    ap.add_argument("--corpus", default=str(REPO / ".corpus" / "mojo-v1.1.0" / "Mojo" / "stdlib" / "std"))
    ap.add_argument("--max", type=int, default=40)
    ap.add_argument("--cycles", type=int, default=3)
    ap.add_argument("--backend", help="explicit mojo-lsp-server path")
    ap.add_argument("--max-resident-docs", type=int, default=20, help="passed to mojols (-1: no cap, the raw backend)")
    ap.add_argument("--memory-limit-mb", type=int, default=3072, help="passed to mojols (-1: never recycle)")
    args = ap.parse_args()

    corpus = Path(args.corpus).resolve()
    files = sorted(p for p in corpus.rglob("*.mojo") if p.name != "__init__.mojo")
    if len(files) < args.max:
        sys.exit(f"only {len(files)} files under {corpus}")
    # Spread the picks across the tree rather than taking one directory.
    step = len(files) / args.max
    files = [files[int(i * step)] for i in range(args.max)]
    texts = {f.as_uri(): f.read_text() for f in files}

    argv = [args.mojols, "serve", "--log-level", "info", "--log-file", str(REPO / ".probe" / "memory.log")]
    if args.backend:
        argv += ["--backend", args.backend]
    argv += [f"--max-resident-docs={args.max_resident_docs}", f"--memory-limit-mb={args.memory_limit_mb}"]
    cap = args.max_resident_docs if args.max_resident_docs > 0 else 10**9

    def resident_tail(uris, opened_so_far):
        # mojols evicts least recently used first, so the most recently opened
        # `cap` documents are the ones the backend parses to completion.
        keep = set(opened_so_far[-cap:])
        return [u for u in uris if u in keep]
    c = Client(argv)
    c.call("initialize", {"processId": os.getpid(), "rootUri": corpus.as_uri(), "capabilities": {}})
    c.notify("initialized", {})
    st = c.call("mojols/status", None)[0]["result"]
    pid = st.get("backendPid")
    if not pid:
        sys.exit(f"no backend running ({st.get('lastBackendError') or 'degraded'}); see .probe/memory.log")
    print(f"corpus {corpus} ({len(texts)} files picked)\nbackend pid {pid}\n")

    def backend_pid():
        # The backend's pid changes when mojols recycles it.
        return c.call("mojols/status", None)[0]["result"].get("backendPid") or pid

    base = peak_rss(backend_pid())
    print(f"{'open docs':>9} {'backend RSS':>12} {'per doc':>9} {'batch parse':>12}")
    print(f"{0:>9} {base:>9.0f} MB {'':>9} {'':>12}")

    opened = []
    rows = []
    for target in [n for n in (1, 5, 10, 20, 40, 80) if n <= args.max]:
        batch = [u for u in list(texts)[len(opened):target]]
        t0 = time.monotonic()
        for u in batch:
            c.notify("textDocument/didOpen", {"textDocument": {"uri": u, "languageId": "mojo", "version": 1, "text": texts[u]}})
        waited = resident_tail(batch, opened + batch)
        done = wait_parsed(c, waited, t0) + (len(batch) - len(waited))
        took = time.monotonic() - t0
        opened += batch
        mem = peak_rss(backend_pid())
        per = (mem - base) / len(opened) if mem and base else 0
        rows.append((len(opened), mem))
        flag = "" if done == len(batch) else f"  ({len(batch) - done} not parsed in time)"
        print(f"{len(opened):>9} {mem:>9.0f} MB {per:>6.1f} MB {took*1000:>9.0f} ms{flag}", flush=True)

    print(f"\nchurn: close all {len(opened)}, reopen all, {args.cycles} times")
    for cycle in range(1, args.cycles + 1):
        for u in opened:
            c.notify("textDocument/didClose", {"textDocument": {"uri": u}})
        time.sleep(2)
        closed = peak_rss(backend_pid())
        t0 = time.monotonic()
        for u in opened:
            c.notify("textDocument/didOpen", {"textDocument": {"uri": u, "languageId": "mojo", "version": 1, "text": texts[u]}})
        wait_parsed(c, resident_tail(opened, opened), t0)
        reopened = peak_rss(backend_pid())
        print(f"  cycle {cycle}: all closed {closed:>6.0f} MB   all reopened {reopened:>6.0f} MB", flush=True)

    st = c.call("mojols/status", None)[0]["result"]
    print(f"\nmojols itself: {rss_mb(c.proc.pid):.0f} MB   restarts {st.get('restarts')}   quarantined {st.get('quarantined') or 'none'}")
    print(f"resident in backend: {st.get('residentDocuments')}   evictions: {st.get('evictions')}   recycles: {st.get('recycles')}")
    c.call("shutdown", None)
    c.notify("exit", None)
    c.proc.wait(timeout=15)


if __name__ == "__main__":
    main()
