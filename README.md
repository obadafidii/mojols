# mojols

A robust language server for Mojo, for Zed and CLion.

mojols sits between your editor and Modular's official `mojo-lsp-server`. The official server stays the source of truth for types and semantics. mojols owns everything around it: keeping it alive, keeping it in sync, and (from M2 on) a workspace-wide index that adds cross-file features the official server does not have.

The rule mojols enforces: **the backend is allowed to fail; mojols is not.**

Design doc: [Mojo LSP: Design Doc](https://claude.ai/artifact/9rWQUQFcebHzUmnJzXF4qK) (summary in [docs/design.md](docs/design.md)).

## Status: M0 (pass-through spike)

| Works today | How it is tested |
| --- | --- |
| Transparent LSP proxy over stdio, ids remapped in both directions | `TestPassThrough`, `TestServerRequestRoundTrip` |
| Backend crash: in-flight requests fail with `ContentModified`, restart with backoff, editor state replayed (initialize, initialized, every open document) | `TestCrashRestartsAndReplays` |
| Crash loop (5 in 2 min): degraded mode, editor told once, retry every 2 min; `initialize` is always answered | `TestCrashLoopEntersDegradedMode`, `TestMissingBackendDegradesWithoutHanging` |
| Hang: per-method deadlines, `$/cancelRequest` to the backend, kill after 3 consecutive timeouts | `TestHangIsDetectedAndKilled` |
| Garbage from the backend is dropped and counted, never forwarded | `TestGarbageReplyIsContained` |
| Cancellation forwarded; queued requests cancelled locally | `TestCancelReachesBackend` |
| Editor hangs up: backend never orphaned | `TestEditorHangupKillsBackend` |
| A file that crashes the backend is found and quarantined: suspects are reopened one at a time, the guilty file gets an in-file warning and fast errors, every other file keeps full features, saving different content retries it | `TestPoisonedFileIsQuarantined`, `TestOnlyTheGuiltySuspectIsQuarantined`, `TestRequestCrashQuarantinesNothing` |
| UTF-16/UTF-8/UTF-32 positions, incremental edits | table tests + `FuzzOffset` against a reference implementation |
| Strict JSON-RPC framing and decoding | table tests + `FuzzReader` |
| `mojols doctor`, `mojols/status`, `--trace-file` session recording | manual, `scripts/m0_probe.py` |

First real-toolchain finding: `mojo-lsp-server` segfaults on `std/io/io.mojo` from a modular checkout (crash in `string_literal.mojo:69`, likely a source/toolchain version mismatch). Before quarantine that put every file in degraded mode after 5 crashes; now it costs that one file, after 2.

That crash turned out to be a version mismatch: modular `main` is Mojo 1.3.0.dev, the installed server is 1.1.0. The same file from the `mojo/v1.1.0` tag parses cleanly. M0 is done; results are in [docs/design.md](docs/design.md#m0-findings-mojo-110-macos-arm64-nov-8-2026).

Next (M1): infer package roots for `-I`, cross-file invalidation, memory caps.

## Quick start

Needs Go 1.24+ and a Mojo toolchain (`mojo-lsp-server` on PATH, or in the project's `.pixi` env).

```sh
make build                 # bin/mojols, bin/fakelsp
bin/mojols doctor .        # which mojo-lsp-server will be used, and why
make test                  # unit + chaos tests, race detector on
```

Measure the real server (M0 open questions):

```sh
make probe FILES="path/to/some.mojo path/to/other.mojo"
```

### Zed

1. Put `mojols` on your PATH (`cp bin/mojols ~/go/bin/` or `go install ./server/cmd/mojols` from `server/`).
2. Uninstall any other Mojo extension (two extensions cannot both define the Mojo language).
3. In Zed: `zed: extensions` → **Install Dev Extension** → choose `editors/zed`.

Options go in Zed settings:

```json
{
  "lsp": {
    "mojols": {
      "binary": {
        "path": "~/go/bin/mojols",
        "arguments": ["--log-level", "debug", "--trace-file", "/tmp/mojols-trace.jsonl", "-I", "src"]
      }
    }
  }
}
```

### CLion

Planned for M4 on JetBrains' LSP API. See [editors/clion](editors/clion/README.md).

## Command line

```
mojols [serve] [flags] [-- extra mojo-lsp-server args]
  --backend PATH      mojo-lsp-server to run (default: discover)
  -I DIR              include dir passed to mojo-lsp-server (repeatable)
  --log-file PATH     logs (default: stderr; stdout carries the protocol)
  --log-level LEVEL   debug | info | warn | error
  --trace-file PATH   record every message as JSON lines
mojols doctor [dir]   explain backend discovery for a workspace
mojols version
```

Discovery order: `--backend`, `$MOJOLS_BACKEND`, the workspace's `.pixi/envs/default/bin`, `.magic/envs/default/bin`, `.venv/bin`, `PATH`, then `~/.pixi/bin`, `~/.mojo/bin`, `~/.modular/bin`, `~/.local/bin`, `/opt/homebrew/bin`, `/usr/local/bin`. It runs again before every restart, so installing Mojo while the editor is open recovers on the next retry.

## Layout

```
server/                  Go module (stdlib only)
  cmd/mojols             the server
  cmd/fakelsp            scriptable fake backend for manual chaos testing
  internal/jsonrpc       framing + strict message decoding
  internal/text          position encodings, incremental edits, document store
  internal/proxy         supervisor, router, restart + replay, timeouts, degraded mode
  internal/discover      finding mojo-lsp-server
  internal/fakelsp       the fake backend used by the chaos tests
  internal/trace         JSON-lines session recording
editors/zed              Zed extension (Rust → WebAssembly)
editors/clion            CLion plugin (M4)
scripts/m0_probe.py      measures the real server through mojols
scripts/release.sh       dist/mojols_<os>_<arch>.tar.gz (the names the Zed extension downloads)
```

## License

Apache 2.0. The Zed extension's tree-sitter queries and snippets come from [vadim-su/zed_mojo](https://github.com/vadim-su/zed_mojo) under MIT; see `editors/zed/LICENSE-queries-MIT`.
