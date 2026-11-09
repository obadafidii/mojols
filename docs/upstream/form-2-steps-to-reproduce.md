1. Save this one-line file as `repro.mojo`:

   ```mojo
   from std.iter import zip
   ```

   followed by an empty line (the file is `from std.iter import zip\n\n`).

2. Start `mojo-lsp-server` with no arguments and send, over stdio:
   - `initialize` (any client capabilities), then `initialized`
   - `textDocument/didOpen` for `repro.mojo` (languageId `mojo`, version 1, the text above)
   - wait for `textDocument/publishDiagnostics` (it reports no diagnostics)
   - `textDocument/completion` with `"position": {"line": 1, "character": 0}`

3. The server prints the error above and exits.

Other positions that crash the same way:

- `{"line": 0, "character": 24}` (end of the import line)
- `from std.iter import StopIteration` followed by any second line, e.g. `from std.collections import List`, with completion inside its `from` keyword (`{"line": 1, "character": 2}`)

A self-contained client that runs these steps (Python 3 standard library only; `python3 repro_lsp.py [path/to/mojo-lsp-server]`):

<details><summary>repro_lsp.py</summary>

```python
#!/usr/bin/env python3
"""Reproduce a mojo-lsp-server abort on textDocument/completion.

Usage: python3 repro_lsp.py [path/to/mojo-lsp-server]

Writes `from std.iter import zip` to a temporary file, opens it in a bare
mojo-lsp-server, waits for diagnostics, then requests completion on line 1,
character 0. Prints whether the server survived.
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import time

server = sys.argv[1] if len(sys.argv) > 1 else shutil.which("mojo-lsp-server")
if not server:
    sys.exit("mojo-lsp-server not found; pass its path")

tmp = tempfile.mkdtemp()
path = os.path.join(tmp, "repro.mojo")
text = "from std.iter import zip\n\n"
with open(path, "w") as f:
    f.write(text)
uri = "file://" + path

proc = subprocess.Popen([server], cwd=tmp, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                        stderr=subprocess.PIPE)


def send(msg):
    body = json.dumps(msg).encode()
    proc.stdin.write(b"Content-Length: %d\r\n\r\n" % len(body) + body)
    proc.stdin.flush()


def read():
    """Next message from the server, or None if it exited."""
    length = None
    while True:
        line = proc.stdout.readline()
        if not line:
            return None
        line = line.strip()
        if not line:
            break
        key, _, value = line.decode().partition(":")
        if key.lower() == "content-length":
            length = int(value)
    return json.loads(proc.stdout.read(length))


def until(pred):
    while True:
        msg = read()
        if msg is None or pred(msg):
            return msg


send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
      "params": {"processId": os.getpid(), "rootUri": "file://" + tmp, "capabilities": {}}})
until(lambda m: m.get("id") == 1)
send({"jsonrpc": "2.0", "method": "initialized", "params": {}})
send({"jsonrpc": "2.0", "method": "textDocument/didOpen",
      "params": {"textDocument": {"uri": uri, "languageId": "mojo", "version": 1, "text": text}}})
until(lambda m: m.get("method") == "textDocument/publishDiagnostics")

send({"jsonrpc": "2.0", "id": 2, "method": "textDocument/completion",
      "params": {"textDocument": {"uri": uri}, "position": {"line": 1, "character": 0}}})
reply = until(lambda m: m.get("id") == 2)
time.sleep(1)

if reply is None or proc.poll() is not None:
    proc.wait()
    err = proc.stderr.read().decode(errors="replace")
    lines = [l for l in err.splitlines() if "LLVM ERROR" in l or l.startswith(("0.", "1."))]
    print(f"CRASHED (exit status {proc.returncode})")
    print("\n".join(lines) or err[-2000:])
    sys.exit(1)

items = reply.get("result") or {}
items = items.get("items", items) if isinstance(items, dict) else items
print(f"OK: completion returned {len(items or [])} items")
send({"jsonrpc": "2.0", "id": 3, "method": "shutdown"})
send({"jsonrpc": "2.0", "method": "exit"})
proc.wait(timeout=5)
```

</details>

Server output on 1.1.0 (exit status SIGABRT; 1.2.0 is identical apart from the symbol name):

```
LLVM ERROR: unknown decl symbol: @std::@builtin::@value::@RegisterPassable
Stack dump:
0.	Crash resolving decl body at loc("repro.mojo":1:1)
1.	Crash parsing statement at loc("repro.mojo":1:1)
```

How it was found: this came out of an 8-hour soak test of an editor client against `mojo-lsp-server`, which saw ~4 server crashes an hour, all on `textDocument/completion`. A delta-debugging reduction shrank the original 230-line file (`stdlib/test/iter/test_ref_iteration.mojo` from the `mojo/v1.1.0` tag) to the one line above.

Possibly related, macOS arm64 only: completion requested in the middle of an identifier also crashes the server deterministically on macOS, but not with the same build on Linux x86_64. Example: `stdlib/test/iter/test_zip.mojo` (1.1.0 tag), `textDocument/completion` at `{"line": 234, "character": 20}`, i.e. `assert_equal(ele|m[0], 1)`. The stack dump there:

```
0.	Crash resolving decl body at loc("test_zip.mojo":229:5)
    >> def test_zip3_owned() raises:
1.	Crash parsing statement at loc("test_zip.mojo":235:5)
    >>     assert_equal(elem[0], 1)
```

`stdlib/test/itertools/test_cycle.mojo` at `{"line": 50, "character": 18}` crashes the same way. I can file this separately if that is easier to track.
