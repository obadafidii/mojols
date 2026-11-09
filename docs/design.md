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

## M1 findings: memory (scripts/memory_probe.py, 40 stdlib files, 3 close/reopen rounds)

| Setup | 20 open | 40 open | All closed (churn) | All reopened (churn) |
| --- | --- | --- | --- | --- |
| Official server alone | 659 MB | 1,252 MB | 579-679 MB | 1,289-1,342 MB |
| mojols, 20-document cap | 660 MB | 737 MB | 516-545 MB | 740-753 MB |
| mojols, cap + 700 MB recycle | 654 MB | 749 MB | 245 MB after recycle | 664-710 MB |

- ~31 MB per open document; closing returns about half. The cap cuts 40 open files from 1,252 MB to 737 MB.
- Recycle: 184 ms from decision to a ready backend with 20 files reopened; no failed request; not counted as a restart. Exceeding the limit again 20 s later was held by the cooldown.
- With the default 3 GB limit, recycling is a backstop; the cap does the everyday work.

## M1 findings: 8-hour soak (scripts/soak.py --hours 8 --min-tabs 24), passed

| Check | Result |
| --- | --- |
| Requests answered | 20,723 of 20,723 |
| mojols memory | 14.9 MB median first hour, 15.3 MB last hour |
| mojols goroutines | 8 first hour, max 9 last hour |
| Backend memory | peak 1,022 MB (limit 3,072); 1,464 evictions; 0 recycles |
| p95 first to last hour | hover 463 to 621 ms; completion 1,231 to 1,372 ms; definition 446 to 569 ms |

- The official server crashed 32 times (20 segfaults, 14 aborts), ~4 per hour; none file-specific (no quarantine), none mojols hang-kills. mojols contained all of them: 0 unanswered, 32 ContentModified completions.
- 27,384 dependent refreshes (every save re-checks importers, even for comment-only edits): likely the main latency cost, possibly a crash trigger.
- Latency is well over the design targets under this load, with little drift.
- Next: crash stack dumps are now always logged; compare 1-hour soaks with refresh on and off (same seed).

## M1 findings: the crashes are completion, now isolated

1-hour soaks on ordinary code (the 1.1.0 stdlib's tests), same seed:

| Run | Main restarts | Completion-server crashes | p95 hover | p95 completion |
| --- | --- | --- | --- | --- |
| Baseline | 16 | n/a | 244-324 ms | 646-678 ms |
| --settle-requests | 10 | n/a | 225-346 ms | 625-710 ms |
| Completion isolated (default) | 0 | 8, contained | 336-344 ms | 877-1,154 ms |

- Refresh on vs off (stdlib edits): 4 vs 3 crashes per hour, so refreshes are not the cause; they cost 15-25% latency.
- Every captured crash was textDocument/completion. 3 of 6 bundles reproduce 3/3 on a bare mojo-lsp-server from one file and one request: completion mid-word in valid code (e.g. `assert_equal(ele|m[0], 1)`), including the `unknown decl symbol @std::@builtin::@value::@RegisterPassable` abort. Upstream bug; repro folders via scripts/replay_crash.py.
- Completion now runs in a second, sacrificial backend (max 3 docs): a crash costs one empty, incomplete list; the main backend never restarts.
- Completion isolation costs ~115 ms at the median. Three ways of keeping the sidecar's copy current were measured; on-request sync (the simplest) stays.

Completion latency over every request (1-hour soaks, 619-630 completions each, same seed):

| Run | p50 | p90 | p95 | p99 | ContentModified |
| --- | --- | --- | --- | --- | --- |
| Before isolation | 304 ms | 573 ms | 701 ms | 2,295 ms | 6-16 |
| Isolated, synced on request (kept) | 420 ms | 743 ms | 883 ms | 2,198 ms | 28 |
| Isolated, every keystroke mirrored | 495 ms | 731 ms | 946 ms | 1,544 ms | 0 |
| Isolated, mirrored after a 300 ms pause | 490 ms | 726 ms | 1,148 ms | 2,178 ms | 12 (17 retried) |

- Hover and definition are unchanged by isolation (p50 1 ms, p95 ~300-330 ms).
- Mirroring (9,373 edits/h per keystroke, 862/h debounced) keeps the sidecar re-parsing, which competes with the main backend and with completion's own re-parse: +70 ms at the median either way. Only per-keystroke mirroring helped the tail, and it doubles parsing work on every edit. Not worth it.
- ContentModified on completion is now retried once in the sidecar; the remaining ones are an empty, incomplete list the editor re-requests on the next keystroke.
- Even before isolation, completion's median was 304 ms: the official server re-parses at the cursor for every completion. The 250 ms target needs mojols' own completion from its index (M3), not routing.

## M3: mojols' own syntax layer and index (in progress)

`internal/syntax` is a hand-written, error-tolerant Mojo lexer and parser
in Go (no cgo, no tree-sitter, so mojols stays one static binary).
`internal/index` resolves every identifier across the workspace. The proxy
answers definition, references, rename and workspace symbols from it.

Validation against the official server (Mojo 1.1.0, all 579 files of the
1.1 stdlib and its tests):

| Check | Result |
| --- | --- |
| Parser: panics / illegal tokens | 0 / 0 (fuzzed: 3M lexer, 1.76M parser executions) |
| Outline vs documentSymbol | 10,506 of 10,507 symbols (the other is a synthesized closure `__call__`) |
| Parse time, whole corpus (5.9 MB) | ~180 ms on one core |
| Definition, 158,789 occurrences | precision 98.99%, recall 97.09%, 45 disagreements |
| References, 6,456 stdlib symbols | 93.9% exactly the server's set; 24 wrong refs, 1,671 missing (3.5%) of 47,076 |
| References latency via mojols | cold 349 ms (lists and parses the workspace), warm p50 1 ms, p95 2 ms |

- Ground truth: `scripts/oracle_definitions.py` asks the official server's
  definition at every occurrence (the stdlib sources resolve across files
  inside themselves); `scripts/score_resolver.py` and `scripts/refs_gate.py`
  score against it.
- Oracle blind spots, not counted as errors: keyword arguments (the server
  never resolves them), generic trait dispatch (`writer.write` on a
  `Some[Writer]`), `comptime for` loop variables, implicit conversions
  (`if x:` resolving to `__bool__`).
- Upstream quirks found: comptime declaration ranges start at column 0;
  `out` arguments and captures are reported at their def line; `except X:`
  binds X; no definition into binary packages (std from user code).
- Missing references are members reached through generic types
  (`node[].left` through `Pointer[Node]`, `opt.value()` on `Optional[T]`,
  `x.cast[...]` on SIMD expressions). Next: type arguments in the resolver,
  then confirming what remains with the official server in the background.
