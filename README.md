# mojols

A robust Mojo language server for Zed, with CLion support on the way.

mojols sits between your editor and Modular's official `mojo-lsp-server`. The official server stays the source of truth for types, diagnostics and completion. mojols keeps it alive, routes around its crashes, and adds a workspace-wide index of its own: cross-file navigation, references, rename, hierarchies and organize imports, which keep working even while the official server is down.

The rule mojols enforces: **the backend is allowed to fail; mojols is not.**

## mojols and the `mojo` Zed extension

They work at different layers, and you install both.

| | `mojo` extension ([vadim-su/zed_mojo](https://github.com/vadim-su/zed_mojo)) | `mojols` extension (this repo) |
| --- | --- | --- |
| What it is | The Mojo language for Zed | A language server |
| Provides | Grammar, syntax highlighting, outline queries, snippets | Everything that needs to understand code: hover, navigation, references, rename, completion, diagnostics |
| Language server | Starts `mojo-lsp-server` directly and passes requests through unchanged | Runs `mojo-lsp-server` under supervision and answers many requests itself |
| When the official server crashes | Every feature stops until Zed restarts it ("server shut down") | The server is restarted, the file that crashed it is set aside, and hover and outline keep working from mojols' index |

So `mojo` makes Mojo files look right, and mojols makes them work right. mojols attaches to the Mojo language that `mojo` defines, and replaces the bare server it would otherwise start (see [Zed setup](#zed)).

## Features

### Reliability around the official server

- **Crash recovery.** If `mojo-lsp-server` dies, in-flight requests fail cleanly, it restarts with backoff, and the editor's state (every open document) is replayed into the new process.
- **Crash quarantine.** A file that crashes the server is found automatically and kept away from it. Every other file keeps full features; saving the file with different content retries it.
- **Answers from source while the server is down.** Hover (signature, enclosing type, docstring) and the document outline come from mojols' own parser for quarantined files and while the server is in degraded mode. Other requests get empty results instead of errors.
- **Completion isolation.** Completion and signature help, which caused every crash observed so far, run in a second, sacrificial server. A completion crash costs one empty completion list, never hover or diagnostics.
- **Hangs and crash loops.** Per-request deadlines, cancellation forwarded to the server, a kill after 3 consecutive timeouts. After 5 crashes in 2 minutes mojols enters degraded mode, tells you once, and retries every 2 minutes.
- **Memory caps.** At most 20 documents are open in the server at once (about 31 MB each); others reopen transparently when touched. Above 3 GB the server is recycled without failing a request.
- **Cross-file freshness.** Saving a file, or changing it on disk, re-checks every open file that imports it, transitively.

### Code intelligence from mojols' own index

mojols parses Mojo itself (a hand-written, error-tolerant parser in Go) and resolves every identifier across the workspace, including generic types carried through calls, subscripts, loops and literals.

- **Go to definition** across files, into packages, and into the stdlib's sources (see below).
- **Find references**, with anything the index can't resolve on its own confirmed by a separate `mojo-lsp-server`.
- **Rename** across the workspace. It refuses rather than guesses if any possible use is unconfirmed, and never edits library files.
- **Workspace symbols.**
- **Type hierarchy**: supertypes (the traits a type lists) and subtypes (every struct or trait that lists it).
- **Call hierarchy**: incoming and outgoing calls, with constructions counted as calls to `__init__`.
- **Organize imports** as a code action: merges, sorts and groups the leading import block, wraps long lines as `mojo format` does, and removes imports the file doesn't use. Package `__init__.mojo` files keep every import, since they re-export.
- **Go to definition into the stdlib.** The installed Mojo ships only compiled `std`, so mojols downloads the matching sources from GitHub once into your user cache, or uses a directory you point it at.

Measured against the official server on the Mojo 1.1 stdlib and its tests (579 files):

| Check | Result |
| --- | --- |
| Go to definition, 158,789 occurrences | 98.99% precision, 98.00% recall |
| References, 6,456 stdlib symbols | 96.8% exactly the compiler's set |
| References latency | p50 1 ms, p95 2 ms warm |
| Members resolved in code that uses the stdlib | 4.4% before stdlib sources, 86.2% after |
| Organize imports on all 579 files | 347 changed, a second pass changes none, no file stops compiling |

Details and methodology: [docs/design.md](docs/design.md).

### Toolchain and project setup

- **Finds the right `mojo-lsp-server` per project**: the project's pixi, magic or venv environment first, then PATH and the usual install locations. It looks again before every restart, so installing Mojo while the editor is open recovers on its own.
- **Package roots.** Top-level packages and `.mojopkg` files in the workspace are passed to the server as `-I` include paths, so imports between your own packages resolve.
- **`mojols doctor`** explains which server it picked and why, the inferred package roots, and the stdlib sources status.
- **Bug reports.** `--trace-file` records a session as JSON lines, and `--crash-dir` saves a replayable bundle for every server crash.

### Passed through from the official server

Diagnostics, completion, signature help, semantic highlighting, inlay hints, the server's own quick fixes, and hover (when the server is healthy) come from `mojo-lsp-server` unchanged.

## In progress and planned

| Feature | Status |
| --- | --- |
| Zed extension in Zed's extension list | In progress: first release, then the registry submission |
| CLion plugin, on JetBrains' built-in LSP client | Planned next ([editors/clion](editors/clion/README.md)) |
| Inspections: unused imports and variables, unresolved names | Planned |
| Quick fixes: add a missing import (from the workspace and stdlib), remove an unused one | Planned |
| Refactorings: extract variable, extract function, inline variable | Planned |
| Run and test: "run file" and "run test" in Zed, run configurations in CLion | Planned |
| Debugging through the toolchain's LLDB debug adapter | Planned |
| Discovery: the nearest `.pixi` above each file (for monorepos like modular), conda environments | Planned |
| Use a stdlib checkout in the workspace as the stdlib sources automatically | Planned |
| On-disk index cache for faster cold starts | Planned |

## Install

### Zed

1. Install the **mojo** extension from Zed's extension list. It provides the Mojo language itself.
2. Install mojols:
   - **Release build**: the mojols extension downloads the latest [release](https://github.com/obadafidii/mojols/releases) by itself if `mojols` isn't on your PATH.
   - **From source**: `make build` and copy `bin/mojols` onto your PATH, or `go install github.com/obadafidii/mojols/server/cmd/mojols@latest`.
3. Install the **mojols** extension. Until it's in Zed's list: `zed: install dev extension` and pick `editors/zed`.
4. Turn off the `mojo` extension's direct server, since mojols already runs and supervises it:

   ```json
   "languages": {
     "Mojo": { "language_servers": ["mojols", "!mojo-lsp-server", "..."] }
   }
   ```

5. Open a Mojo project, and run `mojols doctor` in it if anything looks off.

### CLion

Not available yet; see [In progress and planned](#in-progress-and-planned).

## Configuration

In Zed, mojols' flags go in your settings:

```json
"lsp": {
  "mojols": {
    "binary": {
      "path": "/path/to/mojols",
      "arguments": ["--log-file", "/tmp/mojols.log"]
    }
  }
}
```

```
mojols [serve] [flags] [-- extra mojo-lsp-server args]
  --backend PATH               mojo-lsp-server to run (default: discover)
  -I DIR                       include dir for mojo-lsp-server (repeatable; wins over inferred ones)
  --infer-roots=false          don't scan the workspace for package roots
  --refresh-dependents=false   don't re-check importers when a file is saved
  --index=false                don't answer from mojols' own index
  --verify=false               don't confirm references with a second mojo-lsp-server
  --stdlib-sources auto|off|DIR  stdlib sources for go to definition into std (default auto: download once)
  --isolate-completion=false   run completion in the main mojo-lsp-server
  --max-resident-docs N        documents open in mojo-lsp-server at once (default 20; -1 = no cap)
  --memory-limit-mb N          recycle mojo-lsp-server above this resident memory (default 3072; -1 = never)
  --settle-requests            hold a request until the server has parsed the latest edit (experimental)
  --crash-dir DIR              save a replayable bundle for every mojo-lsp-server crash
  --log-file PATH              logs (default: stderr; stdout carries the protocol)
  --log-level LEVEL            debug | info | warn | error
  --trace-file PATH            record every message as JSON lines
mojols doctor [dir]            explain server discovery, package roots and stdlib sources
mojols stdlib [dir]            download the stdlib sources for the installed Mojo now
mojols version
```

Server discovery order: `--backend`, `$MOJOLS_BACKEND`, the workspace's `.pixi/envs/default/bin`, `.magic/envs/default/bin`, `.venv/bin`, `PATH`, then `~/.pixi/bin`, `~/.mojo/bin`, `~/.modular/bin`, `~/.local/bin`, `/opt/homebrew/bin`, `/usr/local/bin`.

## How it works

```
editor ──LSP── mojols ──LSP── mojo-lsp-server            (main: diagnostics, hover, ...)
                 │    └─LSP── mojo-lsp-server            (completion, sacrificial)
                 │    └─LSP── mojo-lsp-server            (reference checks, on demand)
                 └── index: syntax layer + resolver over the workspace and stdlib sources
```

mojols is one static Go binary with no dependencies outside the Go standard library.

```
server/
  cmd/mojols             the server and its commands
  internal/proxy         supervisor, routing, restart and replay, quarantine, fallbacks
  internal/syntax        Mojo lexer and parser
  internal/index         workspace index, resolver, type engine, hierarchies
  internal/verify        reference checks with a second mojo-lsp-server
  internal/organize      organize imports
  internal/stdlib        fetching the stdlib sources for the installed Mojo
  internal/discover      finding mojo-lsp-server
  internal/roots         inferring package roots
  internal/imports       the import graph for cross-file refresh
  internal/jsonrpc       framing and strict message decoding
  internal/text          position encodings, incremental edits
  internal/fakelsp       a scriptable fake server for the chaos tests
editors/zed              Zed extension (Rust, compiled to WebAssembly)
editors/clion            CLion plugin (planned)
scripts/                 probes, soak test, and the scoring scripts behind the numbers above
docs/design.md           design notes and measurements
```

## Development

Needs Go 1.24+ and, for real-server checks, a Mojo toolchain.

```sh
make build        # bin/mojols and bin/fakelsp
make test         # unit and chaos tests with the race detector
make fuzz         # 30 s per fuzz target
make zed-check    # type-check the Zed extension
make release      # cross-compile dist/mojols_<os>_<arch>.tar.gz
```

Releases are published by pushing a `v*` tag; the workflow builds the archives the Zed extension downloads.

## Milestones

| | Scope | Status |
| --- | --- | --- |
| M0 | Pass-through proxy, fake server, chaos tests, Zed dev extension | Done |
| M1 | Crash quarantine, completion isolation, memory caps, cross-file refresh, 8-hour soak | Done |
| M2 | mojols' own parser and workspace index | Done (on-disk cache still to come) |
| M3 | Cross-file definition, references, rename, hierarchies, organize imports, stdlib sources | Done |
| M4 | Zed extension listed, CLion plugin on the Marketplace | In progress |

Findings and measurements for each milestone are in [docs/design.md](docs/design.md).

## License

Apache 2.0.
