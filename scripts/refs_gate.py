#!/usr/bin/env python3
"""M3 exit gate: references accuracy and latency on the stdlib.

Accuracy (offline, from the oracle written by oracle_definitions.py): for
every module- or struct-level declaration S in the stdlib sources, compare
  ours(S)    occurrences mojols resolves to S (or an overload of S)
  oracle(S)  occurrences mojo-lsp-server resolves to S (or to a hop on
             mojols' chain for that occurrence, e.g. a re-export)
and count, per symbol:
  missing     in oracle(S), not in ours(S)
  wrong       in ours(S), but the server resolves it to something else
  unverified  in ours(S), the server resolves nothing there (keyword
              arguments, generic trait dispatch, comptime loop variables)
A symbol passes when it has no missing and no wrong references.

Latency (online): starts `mojols` with the real mojo-lsp-server on the
corpus workspace and times textDocument/references for a sample of
symbols: the first (cold: lists and parses the workspace) and the rest.

Usage:
  python3 scripts/refs_gate.py ORACLE.jsonl --mojols bin/mojols --server mojo-lsp-server --workspace DIR [-I DIR] [--sample 300]
"""

import argparse
import collections
import json
import os
import random
import subprocess
import sys
import time
from pathlib import Path
from urllib.parse import quote, unquote

sys.path.insert(0, str(Path(__file__).parent))
from m0_probe import Client  # noqa: E402

DECL_KINDS = {"struct", "trait", "def", "comptime", "field"}


def run_json_lines(argv):
    res = subprocess.run(argv, capture_output=True, text=True)
    if res.returncode != 0:
        sys.exit(res.stderr)
    return [json.loads(l) for l in res.stdout.splitlines()]


def accuracy(args, oracle):
    files = sorted(f for f in oracle if "/std/" in f)
    inc = []
    for d in args.include:
        inc += ["-I", d]
    ours, decls = {}, []
    for i in range(0, len(files), 60):
        for d in run_json_lines([args.mojols, "debug-resolve", *inc, *files[i:i + 60]]):
            ours[d["file"]] = {(r["line"], r["char"]): r for r in d["refs"] or []}
        for d in run_json_lines([args.mojols, "debug-parse", *files[i:i + 60]]):
            for x in d["decls"] or []:
                if x["kind"] in DECL_KINDS and x["scope"] in ("module", "struct"):
                    st = x["range"]["start"]
                    decls.append((d["file"], st["line"], st["character"], x["name"]))

    sym_of = {}  # (file,line,char) of a decl -> canonical symbol key (first overload)
    for r_file, rows in ours.items():
        for r in rows.values():
            if r["chain"] and r["chain"][-1][1] >= 0:
                fin = tuple(r["chain"][-1])
                group = [fin] + [tuple(o) for o in r.get("overloads") or []]
                canon = min(group)
                for g in group:
                    sym_of.setdefault(g, canon)

    names = {}
    for f, line, char, name in decls:
        names[sym_of.get((f, line, char), (f, line, char))] = name
    ours_refs = collections.defaultdict(set)
    oracle_refs = collections.defaultdict(set)
    oracle_at = {}
    for f in files:
        for line, char, name, kind, loc in oracle[f]:
            occ = (f, line, char)
            r = ours[f].get((line, char))
            chain = [tuple(c) for c in (r["chain"] if r and r["chain"] else [])]
            if chain and chain[-1][1] >= 0:
                ours_refs[sym_of.get(chain[-1], chain[-1])].add(occ)
            if not loc:
                oracle_at[occ] = None
                continue
            o = (unquote(loc[0].removeprefix("file://")), loc[1], loc[2])
            oracle_at[occ] = o
            target = None
            if o in sym_of:
                target = sym_of[o]
            elif chain and (o in chain or (loc[3] == loc[1] and any(c[0] == o[0] and c[1] == o[1] for c in chain))):
                target = sym_of.get(chain[-1], chain[-1])  # a hop on our chain
            elif o[1] >= 0:
                # The server's off-by-keyword comptime ranges: same line.
                for c in [k for k in sym_of if k[0] == o[0] and k[1] == o[1]]:
                    target = sym_of[c]
                    break
            # A reference is spelled like what it names (aliases aside);
            # the server also "resolves" implicit conversions such as the
            # __bool__ an `if x:` calls, which are not references.
            tname = names.get(target, name) if target else ""
            implicit = tname.startswith("__") and tname.endswith("__") and name != tname
            if target and not implicit:
                oracle_refs[target].add(occ)

    stats = collections.Counter()
    worst = []
    for f, line, char, name in decls:
        key = sym_of.get((f, line, char), (f, line, char))
        mine, theirs = ours_refs.get(key, set()), oracle_refs.get(key, set())
        missing = theirs - mine
        wrong = {o for o in mine - theirs if oracle_at.get(o) is not None}
        unverified = {o for o in mine - theirs if oracle_at.get(o) is None}
        stats["symbols"] += 1
        stats["refs_oracle"] += len(theirs)
        stats["refs_ours"] += len(mine)
        stats["missing"] += len(missing)
        stats["wrong"] += len(wrong)
        stats["unverified"] += len(unverified)
        if not missing and not wrong:
            stats["exact"] += 1
        else:
            worst.append((len(missing) + len(wrong), name, f, line, len(missing), len(wrong), sorted(missing)[:3], sorted(wrong)[:3]))
    worst.sort(reverse=True)
    print(f"accuracy over {stats['symbols']} stdlib symbols:")
    print(f"  symbols with exactly the server's references: {stats['exact']} ({100*stats['exact']/max(stats['symbols'],1):.2f}%)")
    print(f"  references: server {stats['refs_oracle']}, mojols {stats['refs_ours']}; "
          f"missing {stats['missing']}, wrong {stats['wrong']}, unverified {stats['unverified']}")
    for n, name, f, line, m, w, ms, ws in worst[: args.examples]:
        short = lambda p: p.split("/stdlib/")[-1]
        print(f"  {name} ({short(f)}:{line+1}): missing {m} {[f'{short(x[0])}:{x[1]+1}' for x in ms]} wrong {w} {[f'{short(x[0])}:{x[1]+1}' for x in ws]}")
    return decls


def latency(args, decls):
    rng = random.Random(7)
    sample = rng.sample(decls, min(args.sample, len(decls)))
    argv = [args.mojols, "--backend", args.server, "--log-file", "/dev/null"]
    for d in args.include:
        argv += ["-I", d]
    c = Client(argv)
    root = "file://" + quote(str(Path(args.workspace).resolve()))
    c.call("initialize", {"processId": os.getpid(), "rootUri": root, "capabilities": {}})
    c.notify("initialized", {})
    times, counts = [], []
    for i, (f, line, char, name) in enumerate(sample):
        t0 = time.monotonic()
        resp, _ = c.call("textDocument/references", {"textDocument": {"uri": "file://" + quote(f)},
                                                     "position": {"line": line, "character": char},
                                                     "context": {"includeDeclaration": True}}, timeout=120)
        dt = time.monotonic() - t0
        if resp.get("error"):
            print(f"  error for {name}: {resp['error']}")
            continue
        times.append(dt)
        counts.append(len(resp.get("result") or []))
    c.proc.kill()
    cold, warm = times[0], sorted(times[1:])
    pct = lambda p: warm[min(len(warm) - 1, int(p * len(warm)))] * 1000
    print(f"latency over {len(times)} references requests (through mojols and the real server):")
    print(f"  cold (lists and parses the whole workspace): {cold*1000:.0f} ms")
    print(f"  warm: p50 {pct(0.5):.0f} ms, p95 {pct(0.95):.0f} ms, p99 {pct(0.99):.0f} ms, max {warm[-1]*1000:.0f} ms")
    print(f"  results per request: median {sorted(counts)[len(counts)//2]}, max {max(counts)}")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("oracle")
    ap.add_argument("--mojols", required=True)
    ap.add_argument("--server", required=True)
    ap.add_argument("--workspace", required=True)
    ap.add_argument("-I", dest="include", action="append", default=[])
    ap.add_argument("--sample", type=int, default=300)
    ap.add_argument("--examples", type=int, default=12)
    args = ap.parse_args()
    oracle = {}
    for line in Path(args.oracle).read_text().splitlines():
        d = json.loads(line)
        oracle[d["file"]] = d["defs"]
    decls = accuracy(args, oracle)
    latency(args, decls)


if __name__ == "__main__":
    main()
