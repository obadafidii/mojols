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

## M0 findings (Mojo 1.1.0, macOS arm64, Nov 8 2026)

| File | Lines | Cold parse (ms) | Warm parse (ms) |
| --- | --- | --- | --- |
| hello.mojo | 7 | 304 | 273 |
| pkgproj/app/main.mojo | 6 | 262 | 280 |
| std/io/io.mojo (1.1.0) | 493 | 533 | 482 |
| std/builtin/string_literal.mojo (1.1.0) | 911 | 405 | 403 |
| std/collections/list.mojo (1.1.0) | 1,746 | 481 | 476 |
| std/collections/dict.mojo (1.1.0) | 2,403 | 545 | 548 |

- Parse cost is mostly fixed (stdlib re-parsed per document) and reopening is no faster. After the parse, symbols take 1 to 7 ms, and hover and definition under 1 ms.
- Definition works on files only mojols opened, including across packages.
- Projects need their package roots as -I; without them imports fail. mojols should infer them (M1).
- The corpus must match the toolchain: modular main (1.3.0.dev) segfaults the 1.1.0 server on io.mojo; the mojo/v1.1.0 tag parses cleanly. Use `.corpus/mojo-v<installed>` (git archive of the matching tag).
- Quarantine handles the real crash: 2 crashes, 7.0 s, one file blocked, everything else works.

## M1 findings: cross-file invalidation (scripts/invalidation_probe.py)

| Importer | Refresh on: break | Refresh on: fix | Refresh off |
| --- | --- | --- | --- |
| direct.mojo (`from mathlib.ops import double`) | error in 413 ms | cleared in 153 ms | stale |
| main.mojo (`from mathlib import double`, via `__init__`) | refreshed, no error shown | refreshed in 175 ms | stale |

- The official server never re-checks importers itself; mojols' refresh meets the 1 s target.
- The official server does not report a break inside an indirectly imported module (it skips imported bodies and reports only main-file errors). Candidate feature: open the broken module behind the scenes and warn on the importer's import line.
