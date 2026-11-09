#!/usr/bin/env python3
"""Compare mojols' parser with mojo-lsp-server's document symbols.

For every .mojo file under CORPUS, opens it in a bare mojo-lsp-server,
requests textDocument/documentSymbol, and compares the declarations it lists
(structs, traits, defs, comptime members, fields) with what
`mojols debug-parse` finds, by name and position.

Members the server synthesizes (reported at their struct's name, e.g. an
implicit __deinit__) are not in the source and are skipped.

Usage:
  python3 scripts/validate_outline.py CORPUS --mojols bin/mojols --server mojo-lsp-server [-I DIR] [--out report.json]
"""

import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
from m0_probe import Client  # noqa: E402

OURS = {"struct", "trait", "def", "comptime", "field"}


def official(c, path):
    uri = "file://" + str(path)
    t0 = time.monotonic()
    c.notify("textDocument/didOpen", {"textDocument": {"uri": uri, "languageId": "mojo", "version": 1,
                                                       "text": path.read_text(errors="replace")}})
    c.wait_diagnostics(uri, t0, timeout=120)
    r, _ = c.call("textDocument/documentSymbol", {"textDocument": {"uri": uri}}, timeout=120)
    c.notify("textDocument/didClose", {"textDocument": {"uri": uri}})
    out = set()

    def walk(syms, parent_start):
        for s in syms or []:
            st = s["selectionRange"]["start"]
            key = (s["name"], st["line"], st["character"])
            if (st["line"], st["character"]) != parent_start:  # synthesized members
                out.add(key)
            walk(s.get("children"), (st["line"], st["character"]))

    walk(r.get("result"), None)
    return out


def ours(mojols, paths):
    res = subprocess.run([mojols, "debug-parse", *map(str, paths)], capture_output=True, text=True, check=False)
    by_file, trait_comptimes = {}, {}
    for line in res.stdout.splitlines():
        d = json.loads(line)
        top, anywhere = set(), set()
        traits = {x["name"] for x in d["decls"] or [] if x["kind"] == "trait"}
        tc = trait_comptimes.setdefault(d["file"], set())
        for x in d["decls"] or []:
            if x["kind"] == "comptime" and x["scope"] == "struct" and x.get("parent") in traits:
                st = x["range"]["start"]
                tc.add((x["name"], st["line"], st["character"]))
            if x["kind"] in OURS:
                st = x["range"]["start"]
                key = (x["name"], st["line"], st["character"])
                anywhere.add(key)
                if x["scope"] in ("module", "struct"):
                    top.add(key)
        by_file[d["file"]] = (top, anywhere, d.get("panic"))
    return by_file, trait_comptimes


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("corpus")
    ap.add_argument("--mojols", required=True)
    ap.add_argument("--server", required=True)
    ap.add_argument("-I", dest="include", action="append", default=[])
    ap.add_argument("--out")
    ap.add_argument("--limit", type=int, default=0)
    args = ap.parse_args()

    files = sorted(Path(args.corpus).rglob("*.mojo"))
    if args.limit:
        files = files[: args.limit]
    mine, trait_comptimes = ours(args.mojols, files)
    argv = [args.server]
    for d in args.include:
        argv += ["-I", d]
    c = Client(argv)
    c.call("initialize", {"processId": os.getpid(), "rootUri": None,
                          "capabilities": {"textDocument": {"documentSymbol": {"hierarchicalDocumentSymbolSupport": True}}}})
    c.notify("initialized", {})

    report = {"files": 0, "official": 0, "matched": 0, "trait_comptime": 0, "missing": [], "extra": [], "panics": [], "errors": []}
    t0 = time.monotonic()
    for i, path in enumerate(files):
        try:
            theirs = official(c, path)
        except Exception as e:  # a crash or timeout in the official server
            report["errors"].append([str(path), repr(e)])
            if c.proc.poll() is not None:
                c = Client(argv)
                c.call("initialize", {"processId": os.getpid(), "rootUri": None,
                                      "capabilities": {"textDocument": {"documentSymbol": {"hierarchicalDocumentSymbolSupport": True}}}})
                c.notify("initialized", {})
            continue
        # The server lists some nested defs (those with a decorator), so a
        # match may be at any depth; an extra only counts at module or
        # struct level.
        us, anywhere, panic = mine.get(str(path), (set(), set(), None))
        if panic:
            report["panics"].append([str(path), panic])
        report["files"] += 1
        report["official"] += len(theirs)
        report["matched"] += len(theirs & anywhere)
        for k in sorted(theirs - anywhere, key=lambda k: (k[1], k[2])):
            report["missing"].append([str(path), *k])
        for k in sorted(us - theirs, key=lambda k: (k[1], k[2])):
            if k in trait_comptimes.get(str(path), set()):
                # Associated comptime members of traits (`comptime Element:
                # Movable`) are real declarations the server does not list.
                report["trait_comptime"] += 1
                continue
            report["extra"].append([str(path), *k])
        if (i + 1) % 50 == 0:
            print(f"{i+1}/{len(files)} files, {time.monotonic()-t0:.0f}s, missing {len(report['missing'])}, extra {len(report['extra'])}", flush=True)

    print(f"files {report['files']}, official symbols {report['official']}, matched {report['matched']}, "
          f"missing {len(report['missing'])}, extra {len(report['extra'])}, trait comptime members {report['trait_comptime']}, parser panics {len(report['panics'])}, "
          f"server errors {len(report['errors'])}")
    for kind in ("missing", "extra"):
        for row in report[kind][:15]:
            print(f"  {kind}: {row[0].split('/Mojo/')[-1]}:{row[2]+1}:{row[3]+1} {row[1]}")
    if args.out:
        Path(args.out).write_text(json.dumps(report, indent=1))


if __name__ == "__main__":
    main()
