#!/usr/bin/env python3
"""Score mojols' name resolution against mojo-lsp-server's definitions.

Reads the oracle written by scripts/oracle_definitions.py, runs
`mojols debug-resolve` on the same files, and compares every occurrence:

  agree     both resolve, and the server's answer is on mojols' chain
            (the import, re-export or declaration), or is an overload of it
  none      neither resolves
  missing   the server resolves, mojols does not
  wrong     both resolve, to different places
  extra     mojols resolves, the server does not

`extra` is split by where mojols points: into the stdlib from a file
outside it (the server has no stdlib sources there, so these are expected)
or elsewhere (worth checking).

Usage:
  python3 scripts/score_resolver.py ORACLE.jsonl --mojols bin/mojols [-I DIR] [--examples 20] [--only std|test]
"""

import argparse
import collections
import json
import subprocess
import sys
from pathlib import Path
from urllib.parse import unquote


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("oracle")
    ap.add_argument("--mojols", required=True)
    ap.add_argument("-I", dest="include", action="append", default=[])
    ap.add_argument("--examples", type=int, default=12)
    ap.add_argument("--only", choices=["std", "test"])
    ap.add_argument("--json")
    ap.add_argument("--kind", help="only show examples of this occurrence kind")
    ap.add_argument("--name", help="only show examples with this name")
    args = ap.parse_args()

    oracle = {}
    for line in Path(args.oracle).read_text().splitlines():
        d = json.loads(line)
        if args.only == "std" and "/std/" not in d["file"]:
            continue
        if args.only == "test" and "/test/" not in d["file"]:
            continue
        oracle[d["file"]] = d["defs"]

    ours = {}
    files = sorted(oracle)
    inc = []
    for d in args.include:
        inc += ["-I", d]
    for i in range(0, len(files), 60):
        res = subprocess.run([args.mojols, "debug-resolve", *inc, *files[i:i + 60]], capture_output=True, text=True)
        if res.returncode != 0:
            sys.exit(res.stderr)
        for line in res.stdout.splitlines():
            d = json.loads(line)
            ours[d["file"]] = {(r["line"], r["char"]): r for r in d["refs"] or []}

    counts = collections.Counter()
    by_kind = collections.defaultdict(collections.Counter)
    examples = collections.defaultdict(list)
    for f in files:
        mine = ours.get(f, {})
        in_std = "/std/" in f
        for line, char, name, kind, loc in oracle[f]:
            r = mine.get((line, char))
            chain = [tuple(c) for c in (r["chain"] if r and r["chain"] else [])]
            overl = [tuple(c) for c in (r.get("overloads") or [] if r else [])]
            if loc:
                o = (unquote(loc[0].removeprefix("file://")), loc[1], loc[2])
                if not chain:
                    cat = "missing"
                elif o in chain or o in overl:
                    cat = "agree"
                elif loc[3] == loc[1] and any(c[0] == o[0] and c[1] == o[1] for c in chain + overl):
                    # The server reports a `comptime` name at column 0 of its
                    # line (an upstream off-by-keyword); same line counts.
                    cat = "agree"
                elif o[1] in (r.get("owners") or []) and any(c[0] == o[0] for c in chain):
                    cat = "agree"  # an `out` argument or a capture: the server points at its def
                elif any(c[0] == o[0] and c[1] == -1 for c in chain) or (chain and chain[-1][1] == -1 and o[0] == f):
                    cat = "agree"  # a module: the server points at the import line
                else:
                    cat = "wrong"
            else:
                if not chain:
                    cat = "none"
                elif kind == "kwarg":
                    cat = "kwarg-unverified"  # the server never resolves keyword arguments
                elif not in_std and "/std/" in chain[-1][0]:
                    cat = "extra-into-std"
                elif r.get("final") == "trait-member":
                    cat = "extra-trait-member"  # generic dispatch: the server does not resolve these
                else:
                    cat = "extra"
            counts[cat] += 1
            by_kind[kind][cat] += 1
            if cat in ("missing", "wrong", "extra") and len(examples[cat]) < 2000:
                examples[cat].append((f, line, char, name, kind, loc, chain))

    total = sum(counts.values())
    resolved = counts["agree"] + counts["missing"] + counts["wrong"]
    print(f"{total} occurrences in {len(files)} files")
    for cat in ("agree", "none", "missing", "wrong", "extra", "extra-trait-member", "extra-into-std", "kwarg-unverified"):
        print(f"  {cat:15} {counts[cat]:7}  {100*counts[cat]/max(total,1):5.1f}%")
    print(f"  precision (agree / (agree+wrong+extra)) "
          f"{100*counts['agree']/max(counts['agree']+counts['wrong']+counts['extra'],1):.2f}%")
    print(f"  recall    (agree / server-resolved)      {100*counts['agree']/max(resolved,1):.2f}%")
    for kind, c in sorted(by_kind.items()):
        print(f"  {kind:7} " + " ".join(f"{k}={c[k]}" for k in ("agree", "none", "missing", "wrong", "extra", "extra-trait-member", "extra-into-std", "kwarg-unverified")))

    def short(p):
        return p.split("/stdlib/")[-1] if p else p

    for cat in ("missing", "wrong", "extra"):
        rows = examples[cat]
        if not rows:
            continue
        pattern = collections.Counter((r[4], r[3]) for r in rows)
        print(f"\n{cat}: most common (kind, name): {pattern.most_common(15)}")
        rows = [r for r in rows if (not args.kind or r[4] == args.kind) and (not args.name or r[3] == args.name)]
        for f, line, char, name, kind, loc, chain in rows[: args.examples]:
            src = Path(f).read_text(errors="replace").split("\n")[line]
            o = f"{short(unquote(loc[0][7:]))}:{loc[1]+1}" if loc else "-"
            c = f"{short(chain[-1][0])}:{chain[-1][1]+1}" if chain else "-"
            print(f"  {short(f)}:{line+1}:{char+1} {kind} {name!r} server={o} mojols={c} | {src.strip()[:90]}")
    if args.json:
        Path(args.json).write_text(json.dumps({"counts": counts, "examples": {k: v[:500] for k, v in examples.items()}}, default=list))


if __name__ == "__main__":
    main()
