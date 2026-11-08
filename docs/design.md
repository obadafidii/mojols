# mojols design (summary)

The full design doc, with diagrams, lives at
https://claude.ai/artifact/9rWQUQFcebHzUmnJzXF4qK. This file keeps the parts
the code depends on next to the code.

## Core bet

The official `mojo-lsp-server` is a thin C++ layer over the real Mojo parser
(`Mojo/tools/mojo-lsp-server` in modular/modular). Its answers are correct,
but every open file gets its own `MLIRContext`, parser context and
`SymbolIndex`, so references and rename never leave the current file, and the
stdlib is re-parsed per open document. mojols keeps its semantics and fixes
its reach and its resilience.

## Robustness targets

- A backend crash costs at most the requests in flight.
- Resident memory stays under a cap (default 3 GB) however many files are open. (M1)
- Editing `a.mojo` refreshes diagnostics in every open file that imports it within 1 s. (M1)
- p95 on the stdlib: hover < 100 ms, completion < 250 ms, workspace references < 1.5 s. (M3)

## Failure handling (implemented in internal/proxy)

| Failure | Response |
| --- | --- |
| Crash | In-flight requests fail with `ContentModified`; restart after 250 ms, doubling to 30 s; replay initialize, initialized and all open documents |
| Hang | Per-method deadline (hover 5 s, completion 10 s, references 20 s, default 15 s); `$/cancelRequest` sent; kill after 3 consecutive timeouts |
| Crash loop | 5 failures in 2 min: degraded mode, one `showMessage`, retry every 2 min |
| Garbage reply | Dropped and counted; the request times out normally |
| No toolchain | `initialize` still answered with minimal capabilities; degraded mode |
| Editor hangs up | Backend gets `exit`, then is killed after 2 s |
| A file crashes the backend | Docs not yet diagnosed at the crash are suspects; after restart they are reopened one at a time; a crash during a probe quarantines that file (in-file warning, requests fail fast, crash not counted toward degraded mode); saving different content retries it |

Deadlines are looser than the latency targets on purpose: the official server
parses a file on first use, and a cold parse must not look like a hang.

## Milestones

| | Scope | Exit gate |
| --- | --- | --- |
| M0 | Pass-through proxy, fake backend, Zed dev extension, probe | Protocol harness passes; open questions answered with numbers |
| M1 | Memory caps, quarantine, cross-file invalidation via import graph | Every failure row has a chaos test; 8-hour soak flat |
| M2 | tree-sitter syntax layer, workspace index, on-disk cache | Cold and warm stdlib index timed |
| M3 | Cross-file references and rename, hierarchies, organize imports | Zero missing or extra refs on stdlib; p95 refs < 1.5 s |
| M4 | Zed extension published, CLion plugin released | Merged into zed-industries/extensions; live on Marketplace |

## M0 open questions

Run `make probe FILES=...` on a real toolchain to answer:

1. Does `definition` work on a file only mojols opened?
2. How long does the first parse take, cold and warm?
3. Which `-I` include dirs does a pixi project need?
