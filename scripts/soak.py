#!/usr/bin/env python3
"""M1 exit gate: a long scripted editing session through mojols.

Simulates a person working in the stdlib for hours: typing comment lines
keystroke by keystroke (and deleting them), hovering, completing, jumping
to definitions, listing symbols, saving, and opening and closing tabs. It
works on a private copy of the corpus in .probe/soak/ws, never the corpus.

Every 30 s it samples mojols' and the backend's memory and mojols' own
goroutines and heap; every 10 minutes it prints a progress line. At the
end it checks the exit gate:

  * every request was answered (none hit the 60 s client deadline)
  * mojols' own memory and goroutines stayed flat
  * the backend stayed under mojols' memory limit (recycling allowed)
  * p95 latency in the last hour is no worse than 2x the first hour

Run it with the machine kept awake:

  caffeinate -i python3 scripts/soak.py --hours 8

Results: .probe/soak/samples.csv, .probe/soak/latency.csv,
.probe/soak/summary.txt, logs in .probe/soak/mojols.log.
"""

import argparse
import csv
import os
import random
import re
import shutil
import statistics
import subprocess
import sys
import time
from collections import defaultdict
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from m0_probe import Client  # noqa: E402

REPO = Path(__file__).resolve().parent.parent
OUT = REPO / ".probe" / "soak"  # replaced by --out
IDENT = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")
CLIENT_DEADLINE = 60.0


def rss_mb(pid):
    if not pid:
        return 0.0
    try:
        out = subprocess.run(["ps", "-o", "rss=", "-p", str(pid)], capture_output=True, text=True, timeout=5).stdout
        return int(out.strip()) / 1024
    except (ValueError, subprocess.SubprocessError):
        return 0.0


def utf16_len(s):
    return len(s.encode("utf-16-le")) // 2


class Doc:
    """The editor's view of one open file, kept in sync with what we send."""

    def __init__(self, path):
        self.path = path
        self.uri = path.as_uri()
        self.lines = path.read_text().split("\n")
        self.version = 1
        self.inserted = None  # line index of a comment line we typed, to delete later

    def text(self):
        return "\n".join(self.lines)

    def pos(self, line, col_chars):
        return {"line": line, "character": utf16_len(self.lines[line][:col_chars])}


class Soak:
    def __init__(self, args):
        self.args = args
        self.rng = random.Random(args.seed)
        self.lat = defaultdict(list)  # method -> [(t_since_start, seconds)]
        self.errors = defaultdict(int)  # "method code" -> count
        self.unanswered = 0
        self.actions = defaultdict(int)
        self.samples = []
        self.open = {}  # uri -> Doc
        self.focus = None
        self.start = time.monotonic()
        self.sync_checks = 0
        self.sync_mismatches = []

    # ---- setup ---------------------------------------------------------

    def prepare(self):
        corpus = Path(self.args.corpus).resolve()
        ws = OUT / "ws"
        if ws.exists():
            # Refresh the copy in place (deleting may be unavailable here).
            shutil.copytree(corpus, ws / corpus.name, dirs_exist_ok=True)
        else:
            shutil.copytree(corpus, ws / corpus.name)
        root = ws / corpus.name
        files = sorted(p for p in root.rglob("*.mojo") if p.name != "__init__.mojo")
        self.rng.shuffle(files)
        self.pool = files[: self.args.files]
        self.root = ws
        print(f"workspace {ws} ({len(files)} files; working set {len(self.pool)})", flush=True)

    def launch(self):
        argv = [self.args.mojols, "serve", "--log-level", "info", "--log-file", str(OUT / "mojols.log")]
        argv += self.args.mojols_arg
        if self.args.backend:
            argv += ["--backend", self.args.backend]
        self.c = Client(argv)
        self.mojols_pid = self.c.proc.pid
        init = self.call("initialize", {"processId": os.getpid(), "rootUri": self.root.as_uri(), "capabilities": {
            "textDocument": {"hover": {"contentFormat": ["markdown", "plaintext"]},
                             "documentSymbol": {"hierarchicalDocumentSymbolSupport": True},
                             "completion": {"completionItem": {"snippetSupport": True}}}}})
        if not init or "error" in init:
            sys.exit(f"initialize failed: {init}")
        self.c.notify("initialized", {})

    # ---- protocol helpers ----------------------------------------------

    def call(self, method, params):
        t0 = time.monotonic()
        try:
            resp, took = self.c.call(method, params, timeout=CLIENT_DEADLINE)
        except TimeoutError:
            self.unanswered += 1
            self.errors[f"{method} UNANSWERED"] += 1
            return None
        if method != "mojols/status":
            self.lat[method].append((t0 - self.start, took))
            if "error" in resp:
                self.errors[f"{method} {resp['error'].get('code')}"] += 1
        return resp

    def status(self):
        r = self.call("mojols/status", None)
        return (r or {}).get("result", {})

    def change(self, d, rng, text):
        d.version += 1
        self.c.notify("textDocument/didChange", {
            "textDocument": {"uri": d.uri, "version": d.version},
            "contentChanges": [{"range": {"start": rng[0], "end": rng[1]}, "text": text}],
        })

    # ---- actions -------------------------------------------------------

    def open_tab(self):
        closed = [p for p in self.pool if p.as_uri() not in self.open]
        if not closed:
            return self.switch_focus()
        d = Doc(self.rng.choice(closed))
        self.open[d.uri] = d
        self.c.notify("textDocument/didOpen", {"textDocument": {"uri": d.uri, "languageId": "mojo", "version": d.version, "text": d.text()}})
        self.focus = d

    def close_tab(self):
        if len(self.open) <= self.args.min_tabs:
            return self.open_tab()
        d = self.rng.choice(list(self.open.values()))
        if d.inserted is not None:
            self.untype(d)
        self.c.notify("textDocument/didClose", {"textDocument": {"uri": d.uri}})
        del self.open[d.uri]
        if self.focus is d:
            self.focus = self.rng.choice(list(self.open.values()))

    def switch_focus(self):
        self.focus = self.rng.choice(list(self.open.values()))

    def type_line(self):
        d = self.focus
        if d.inserted is not None:
            return self.untype(d)
        # A new comment line after a random line, typed one key at a time.
        line = self.rng.randrange(len(d.lines))
        indent = re.match(r"\s*", d.lines[line]).group(0)
        end = d.pos(line, len(d.lines[line]))
        self.change(d, (end, end), "\n" + indent)
        d.lines.insert(line + 1, indent)
        d.inserted = line + 1
        for ch in f"# soak edit {self.rng.randrange(10**6)}":
            col = len(d.lines[d.inserted])
            p = d.pos(d.inserted, col)
            self.change(d, (p, p), ch)
            d.lines[d.inserted] += ch
            time.sleep(self.rng.uniform(0.03, 0.09))  # human typing speed

    def untype(self, d):
        i = d.inserted
        start = d.pos(i - 1, len(d.lines[i - 1]))
        end = d.pos(i, len(d.lines[i]))
        self.change(d, (start, end), "")
        del d.lines[i]
        d.inserted = None

    def random_ident(self, d):
        for _ in range(20):
            line = self.rng.randrange(len(d.lines))
            words = list(IDENT.finditer(d.lines[line]))
            if words and not d.lines[line].lstrip().startswith("#"):
                w = self.rng.choice(words)
                return d.pos(line, w.start() + self.rng.randrange(len(w.group(0))))
        return {"line": 0, "character": 0}

    def query(self, method):
        d = self.focus
        params = {"textDocument": {"uri": d.uri}}
        if method != "textDocument/documentSymbol":
            params["position"] = self.random_ident(d)
        self.call(method, params)

    def save(self):
        d = self.focus
        d.path.write_text(d.text())
        self.c.notify("textDocument/didSave", {"textDocument": {"uri": d.uri}})

    def act(self):
        if not self.open or self.focus is None:
            return self.open_tab()
        r = self.rng.random()
        for weight, name, fn in (
            (0.30, "type", self.type_line),
            (0.22, "hover", lambda: self.query("textDocument/hover")),
            (0.12, "completion", lambda: self.query("textDocument/completion")),
            (0.12, "definition", lambda: self.query("textDocument/definition")),
            (0.05, "symbols", lambda: self.query("textDocument/documentSymbol")),
            (0.05, "save", self.save),
            (0.06, "switch", self.switch_focus),
            (0.04, "open", self.open_tab),
            (0.04, "close", self.close_tab),
        ):
            if r < weight:
                self.actions[name] += 1
                return fn()
            r -= weight

    # ---- sampling and reporting ---------------------------------------

    def sample(self):
        st = self.status()
        row = {
            "t_min": round((time.monotonic() - self.start) / 60, 2),
            "mojols_rss_mb": round(rss_mb(self.mojols_pid), 1),
            "backend_rss_mb": round(rss_mb(st.get("backendPid")), 1),
            "goroutines": st.get("goroutines", 0),
            "heap_mb": round(st.get("heapMB", 0), 2),
            "open_tabs": len(self.open),
            "resident": st.get("residentDocuments", 0),
            "restarts": st.get("restarts", 0),
            "recycles": st.get("recycles", 0),
            "evictions": st.get("evictions", 0),
            "refreshes": st.get("dependentRefreshes", 0),
            "quarantined": len(st.get("quarantined") or []),
            "completion_crashes": (st.get("completionBackend") or {}).get("crashes", 0),
            "completion_memo_hits": (st.get("completionBackend") or {}).get("memoHits", 0),
            "completion_retries": (st.get("completionBackend") or {}).get("retries", 0),
            "completion_mirrored": (st.get("completionBackend") or {}).get("mirroredEdits", 0),
            "timeouts": st.get("timeouts", 0),
            "pending": st.get("pendingRequests", 0),
            "unanswered": self.unanswered,
        }
        if self.args.check_sync:
            self.check_sync()
        self.samples.append(row)
        with self.c.lock:
            self.c.notifications.clear()  # keep the probe's own memory flat
        return row

    def check_sync(self):
        """With the fake backend only: every resident document's text in the
        backend must equal the editor's, after any number of edits."""
        r = self.call("fake/state", None)
        docs = ((r or {}).get("result") or {}).get("docs") or {}
        for uri, d in self.open.items():
            if uri in docs:
                self.sync_checks += 1
                if docs[uri]["text"] != d.text():
                    self.sync_mismatches.append(uri)

    def window_p95(self, method, lo, hi):
        xs = sorted(s for t, s in self.lat[method] if lo <= t < hi)
        if len(xs) < 20:
            return None
        return xs[int(len(xs) * 0.95) - 1]

    def progress(self, row):
        now = time.monotonic() - self.start
        p95s = []
        for m in ("textDocument/hover", "textDocument/completion", "textDocument/definition"):
            v = self.window_p95(m, now - 600, now + 1)
            p95s.append(f"{m.split('/')[1]} {v*1000:.0f}" if v is not None else f"{m.split('/')[1]} -")
        print(f"[{row['t_min']:7.1f} min] mojols {row['mojols_rss_mb']:5.1f} MB, {row['goroutines']} goroutines | "
              f"backend {row['backend_rss_mb']:6.0f} MB, {row['resident']} resident | "
              f"restarts {row['restarts']} recycles {row['recycles']} completion crashes {row['completion_crashes']} unanswered {row['unanswered']} | "
              f"p95 ms (10 min): {', '.join(p95s)}", flush=True)

    def write_files(self):
        with open(OUT / "samples.csv", "w", newline="") as f:
            w = csv.DictWriter(f, fieldnames=list(self.samples[0].keys()))
            w.writeheader()
            w.writerows(self.samples)
        with open(OUT / "latency.csv", "w", newline="") as f:
            w = csv.writer(f)
            w.writerow(["t_s", "method", "ms"])
            for m, xs in self.lat.items():
                for t, s in xs:
                    w.writerow([round(t, 1), m, round(s * 1000, 1)])

    def verdict(self, memory_limit_mb):
        lines, ok = [], True

        def check(name, passed, detail):
            nonlocal ok
            ok = ok and passed
            lines.append(f"{'PASS' if passed else 'FAIL'}  {name}: {detail}")

        dur = time.monotonic() - self.start
        hour = min(3600.0, dur / 4)  # "first/last hour", scaled down for short runs
        first = [r for r in self.samples if r["t_min"] * 60 <= hour]
        last = [r for r in self.samples if r["t_min"] * 60 >= dur - hour]

        if self.args.check_sync:
            check("backend text matches the editor", self.sync_checks > 0 and not self.sync_mismatches,
                  f"{self.sync_checks} document checks, {len(self.sync_mismatches)} mismatches")
        check("every request answered", self.unanswered == 0, f"{self.unanswered} hit the {CLIENT_DEADLINE:.0f} s client deadline")

        f_rss = statistics.median(r["mojols_rss_mb"] for r in first)
        l_rss = statistics.median(r["mojols_rss_mb"] for r in last)
        check("mojols memory flat", l_rss <= f_rss * 1.5 + 10, f"median {f_rss:.1f} MB at the start, {l_rss:.1f} MB at the end")

        f_g = max(r["goroutines"] for r in first)
        l_g = max(r["goroutines"] for r in last)
        check("mojols goroutines flat", l_g <= f_g + 10, f"max {f_g} at the start, {l_g} at the end")

        peak = max(r["backend_rss_mb"] for r in self.samples)
        check("backend under the memory limit", peak <= memory_limit_mb * 1.1,
              f"peak {peak:.0f} MB, limit {memory_limit_mb} MB, {self.samples[-1]['recycles']} recycles")

        for m in ("textDocument/hover", "textDocument/completion", "textDocument/definition"):
            a, b = self.window_p95(m, 0, hour), self.window_p95(m, dur - hour, dur + 1)
            if a is None or b is None:
                lines.append(f"SKIP  {m} latency drift: not enough samples")
                continue
            check(f"{m} latency drift", b <= max(a * 2, a + 0.05), f"p95 {a*1000:.0f} ms at the start, {b*1000:.0f} ms at the end")

        total = sum(len(v) for v in self.lat.values())
        lines.append("")
        lines.append(f"duration {dur/3600:.2f} h, {total} requests, actions {dict(self.actions)}")
        lines.append(f"completion backend crashes {self.samples[-1]['completion_crashes']} (contained), "
                     f"memo hits {self.samples[-1]['completion_memo_hits']}, retries {self.samples[-1]['completion_retries']}, "
                     f"mirrored edits {self.samples[-1]['completion_mirrored']}")
        lines.append(f"main backend restarts {self.samples[-1]['restarts']}, recycles {self.samples[-1]['recycles']}, "
                     f"quarantined {self.samples[-1]['quarantined']}, evictions {self.samples[-1]['evictions']}, "
                     f"dependent refreshes {self.samples[-1]['refreshes']}")
        if self.errors:
            lines.append("request errors: " + ", ".join(f"{k}: {v}" for k, v in sorted(self.errors.items())))
            lines.append("(-32801 ContentModified is normal while typing: the backend drops answers for stale text)")
        lines.insert(0, f"M1 soak: {'PASS' if ok else 'FAIL'}")
        return "\n".join(lines)

    def crashes(self):
        """Group the backend crash reports mojols logged by crash site."""
        import ast
        log = OUT / "mojols.log"
        if not log.exists():
            return []
        reports = []
        for line in log.read_text(errors="replace").splitlines():
            if 'msg="backend crash output"' not in line or line < self.log_start:
                continue
            m = re.search(r'output=("(?:[^"\\]|\\.)*")', line)
            if not m:
                continue
            try:
                reports.append(ast.literal_eval(m.group(1)))
            except (ValueError, SyntaxError):
                reports.append(m.group(1))
        sites = defaultdict(int)
        for r in reports:
            site = next((l for l in r.splitlines() if re.match(r"\s*0\.\s", l)), None) \
                or next((l for l in r.splitlines() if "Assertion" in l or "LLVM ERROR" in l), None) \
                or (r.splitlines() or ["(empty)"])[0]
            sites[re.sub(r"0x[0-9a-f]+", "0x…", site.strip())[:160]] += 1
        (OUT / "crashes.txt").write_text("\n\n".join(reports) + "\n")
        return sorted(sites.items(), key=lambda kv: -kv[1])

    def run(self):
        OUT.mkdir(parents=True, exist_ok=True)
        self.log_start = time.strftime("time=%Y-%m-%dT%H:%M:%S")
        self.prepare()
        self.launch()
        for _ in range(self.args.min_tabs):
            self.open_tab()
        end = self.start + self.args.hours * 3600
        next_sample = next_report = time.monotonic()
        try:
            while time.monotonic() < end:
                self.act()
                time.sleep(self.rng.uniform(0.5, 1.5) * self.args.pause)
                now = time.monotonic()
                if now >= next_sample:
                    row = self.sample()
                    next_sample = now + self.args.sample_every
                    if now >= next_report:
                        self.progress(row)
                        next_report = now + self.args.report_every
        except KeyboardInterrupt:
            print("\ninterrupted: reporting on what ran", flush=True)
        self.sample()
        self.write_files()
        limit = self.args.memory_limit_mb
        summary = self.verdict(limit)
        sites = self.crashes()
        if sites:
            summary += "\n\nbackend crash sites (full reports in crashes.txt):\n" + "\n".join(f"  {n:3}x  {site}" for site, n in sites)
        (OUT / "summary.txt").write_text(summary + "\n")
        print("\n" + summary, flush=True)
        self.call("shutdown", None)
        self.c.notify("exit", None)
        self.c.proc.wait(timeout=15)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--hours", type=float, default=8)
    ap.add_argument("--mojols", default=str(REPO / "bin" / "mojols"))
    ap.add_argument("--backend", help="explicit mojo-lsp-server path")
    ap.add_argument("--corpus", default=str(REPO / ".corpus" / "mojo-v1.1.0" / "Mojo" / "stdlib" / "std"))
    ap.add_argument("--files", type=int, default=40, help="working set of files the session touches")
    ap.add_argument("--min-tabs", type=int, default=8)
    ap.add_argument("--pause", type=float, default=0.4, help="mean seconds between actions")
    ap.add_argument("--sample-every", type=float, default=30)
    ap.add_argument("--report-every", type=float, default=600)
    ap.add_argument("--memory-limit-mb", type=int, default=3072, help="the limit mojols runs with (its default)")
    ap.add_argument("--seed", type=int, default=1)
    ap.add_argument("--check-sync", action="store_true", help="with bin/fakelsp as --backend: compare its text with the editor's")
    ap.add_argument("--out", default="soak", help="results go to .probe/<out>")
    ap.add_argument("--mojols-arg", action="append", default=[], help="extra mojols flag, e.g. --mojols-arg=--refresh-dependents=false")
    run = ap.parse_args()
    global OUT
    OUT = REPO / ".probe" / run.out
    Soak(run).run()


if __name__ == "__main__":
    main()
