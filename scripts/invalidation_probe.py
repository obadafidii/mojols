#!/usr/bin/env python3
"""Measure cross-file invalidation on the real mojo-lsp-server.

Copies testdata/probe/pkgproj into .probe/inval and opens two importers:
app/main.mojo (imports mathlib, whose __init__ re-exports ops.double: a
transitive dependency) and app/direct.mojo (imports mathlib.ops itself).
Then:

  1. renames `double` to `triple` in mathlib/ops.mojo and saves it:
     both importers should now show an error
  2. renames it back and saves: the errors should clear

It runs twice: with mojols refreshing dependents (the default) and with
--refresh-dependents=false as the control, and reports how long main.mojo
took to show the right diagnostics, or STALE if it never did.

Usage: python3 scripts/invalidation_probe.py [--mojols bin/mojols]
"""

import argparse
import os
import shutil
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from m0_probe import Client  # noqa: E402

REPO = Path(__file__).resolve().parent.parent


def wait_diags(c, uri, since, want_errors, timeout=10):
    """Seconds until uri gets diagnostics after `since` whose error count
    matches want_errors (True: at least one, False: none); None if never."""
    end = time.monotonic() + timeout
    with c.lock:
        while True:
            for t, m in c.notifications:
                if t < since or m["method"] != "textDocument/publishDiagnostics" or m["params"]["uri"] != uri:
                    continue
                errs = [d for d in m["params"]["diagnostics"] if d.get("severity") == 1]
                if bool(errs) == want_errors:
                    return t - since, errs
            left = end - time.monotonic()
            if left <= 0 or not c.lock.wait(timeout=left):
                return None, None


def run(mojols, refresh):
    work = REPO / ".probe" / "inval"
    if work.exists():
        for f in work.rglob("*.mojo"):
            f.write_text("")  # overwritten below; deletion may be unavailable
    shutil.copytree(REPO / "testdata" / "probe" / "pkgproj", work, dirs_exist_ok=True)
    main, ops = work / "app" / "main.mojo", work / "src" / "mathlib" / "ops.mojo"
    direct = work / "app" / "direct.mojo"
    direct.write_text("from mathlib.ops import double\n\n\ndef main():\n    print(double(2))\n")
    importers = (("main.mojo (transitive)", main), ("direct.mojo", direct))
    original = (REPO / "testdata/probe/pkgproj/src/mathlib/ops.mojo").read_text()

    argv = [mojols, "serve", "--log-level", "debug", "--log-file", str(REPO / ".probe" / "inval.log")]
    if not refresh:
        argv.append("--refresh-dependents=false")
    c = Client(argv)
    c.call("initialize", {"processId": os.getpid(), "rootUri": work.as_uri(), "capabilities": {}})
    c.notify("initialized", {})
    for name, f in importers:
        t0 = time.monotonic()
        c.notify("textDocument/didOpen", {"textDocument": {"uri": f.as_uri(), "languageId": "mojo", "version": 1, "text": f.read_text()}})
        took, _ = wait_diags(c, f.as_uri(), t0, want_errors=False, timeout=60)
        if took is None:
            sys.exit(f"{name} never parsed cleanly; is package-root inference finding src/? See .probe/inval.log")
    c.notify("textDocument/didOpen", {"textDocument": {"uri": ops.as_uri(), "languageId": "mojo", "version": 1, "text": original}})

    results = []
    for step, text, want_errors in (("break (double -> triple)", original.replace("double", "triple"), True),
                                     ("fix (triple -> double)", original, False)):
        version = len(results) // 2 + 2
        c.notify("textDocument/didChange", {"textDocument": {"uri": ops.as_uri(), "version": version}, "contentChanges": [{"text": text}]})
        ops.write_text(text)
        t_save = time.monotonic()
        c.notify("textDocument/didSave", {"textDocument": {"uri": ops.as_uri()}})
        for name, f in importers:
            took, errs = wait_diags(c, f.as_uri(), t_save, want_errors, timeout=5)
            detail = (errs[0]["message"][:70] if errs else "no errors") if took is not None else ""
            results.append((step, name, took, detail))

    status, _ = c.call("mojols/status", None)
    c.call("shutdown", None)
    c.notify("exit", None)
    c.proc.wait(timeout=10)
    return results, status.get("result", {})


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--mojols", default=str(REPO / "bin" / "mojols"))
    args = ap.parse_args()
    for refresh in (True, False):
        label = "mojols refresh ON " if refresh else "control: refresh OFF"
        results, st = run(args.mojols, refresh)
        print(f"\n{label}  (dependent refreshes: {st.get('dependentRefreshes')}, restarts: {st.get('restarts')})")
        for step, name, took, detail in results:
            shown = f"{took*1000:6.0f} ms" if took is not None else "   STALE"
            print(f"  {step:26} {name:24} {shown}  {detail}")
    print("\nTarget from the design doc: under 1000 ms.")


if __name__ == "__main__":
    main()
