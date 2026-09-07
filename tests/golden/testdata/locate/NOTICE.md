# Frozen locator references

These are public memhub sources, copyright (c) 2026 kninetimmy, copied under
the MIT license preserved in `LICENSE-memhub`. The upstream
`THIRD-PARTY-NOTICES.md` is retained as `THIRD-PARTY-NOTICES-memhub.md`.
No source here is executed as application code. JSON envelopes preserve each
original path, original UTF-8 content and SHA-256; the harness writes only a
throwaway Git working tree. JSON envelopes keep reference code out of the
memdolt working tree's own code index and Go package discovery.

Upstream: https://github.com/kninetimmy/memhub

| Artifact | Original source |
| --- | --- |
| `rust-benchmark.json` | Every indexable tracked source file at `4606934a8e696da6bc7e316a239dd039202ca29c` (100 files), the module-doc/test-penalty change underlying the recorded 18/18 benchmark |
| `rust.json` | Every indexable tracked source file at v0.2.0, `43fdbb1269dab0ad42c5514f77a758b4fbe626a8` (117 files), retained for the separate tagged-corpus diagnostic |
| `polyglot.json` | Exact six string constants and path mapping in v0.2.0 `tests/retrieval/locate_polyglot.rs` |
| `../../code_locate_golden.json` | Unchanged v0.2.0 `tests/code_locate_golden.json`; `git diff 4606934 v0.2.0 -- tests/code_locate_golden.json` is empty |
| `../../code_locate_golden_polyglot.json` | Unchanged v0.2.0 `tests/code_locate_golden_polyglot.json` |

Source selection uses the tagged seven-language extension set and excludes
filenames containing `.min.`. All selected tracked files are retained, including
tests and examples; no wanted-file subset, query rewrite, source relocation or
synthetic easy replacement was used. The six polyglot constants are decoded
from their original Rust raw-string literals without modifying their content.
The golden description's shorter `tests/locate_polyglot.rs` spelling is a
historical documentation typo; `git ls-tree v0.2.0` establishes the actual
`tests/retrieval/locate_polyglot.rs` path above.

The corpus distinction is load-bearing. Commit
`4606934a8e696da6bc7e316a239dd039202ca29c` introduced the recorded full Rust
Recall@3 improvement. Later commit
`8f255682f5ac88339c930fe830848b4670ed12b4` moved common helpers from
`src/code_index/locate.rs` into `src/retrieval/util.rs` without updating the
golden's cosine path/symbol matcher. That move is in v0.2.0. The original
golden against the complete tagged corpus measures **17/18**, with the real
cosine implementation correctly returned in `src/retrieval/util.rs`. This is
a stale expected path, not a model failure. The complete corresponding
benchmark corpus measures **18/18** with the same unchanged golden and the
production locator's tagged parsing/scoring. Pairing those original artifacts
preserves the full gate; the historical corpus is not described as v0.2.0.

Measured on Windows AMD64, 2026-09-07, Go 1.26.5, verified production models:

| Corpus/mode | Recall@1 | Recall@3 | Empty probes rejected |
| --- | --- | --- | --- |
| Corresponding Rust benchmark, fusion | 11/18 | 18/18 | 0/2 |
| Corresponding Rust benchmark, rerank + harness floor 0 | 13/18 | 15/18 | 2/2 |
| Tagged polyglot, fusion | 14/17 | 17/17 | 0/2 |
| Tagged polyglot, rerank + harness floor 0 | 9/17 | 11/17 | 2/2 |
| Complete v0.2.0 Rust, fusion diagnostic | 11/18 | 17/18 | 0/2 |

The protected Ubuntu CI gate uses verified real BGE/ms-marco models, checks
the full 18/18 and 17/17 fusion match bars, and separately checks that rerank
floor 0 rejects both probes on each corpus. Default fusion retains its no-floor
behavior. Reranking losses and fusion nonsense leakage are reported honestly.
The existing manifest-keyed model cache is reused only after verification.

```sh
CGO_ENABLED=1 go test -tags golden,gms_pure_go ./tests/golden/... -run TestLocateGolden -count=1 -timeout 30m
CGO_ENABLED=1 go test -tags golden,gms_pure_go ./tests/golden/... -run TestLocateTaggedCorpusDiagnostic -count=1 -timeout 30m -v
```

The golden harness accepts `MEMDOLT_PARITY_MODEL_DIR` for an existing verified
cache and `MEMDOLT_INFERENCE_OFFLINE=1` to forbid downloads. Leaving
`ONNXRUNTIME_SHARED_LIBRARY_PATH` unset selects the manifest's verified native
library from that cache; an explicit library is still checksum-verified.
Neither setting bypasses verification. No binary or model is copied here.

The harness pins normalized outer-JSON SHA-256 values in `locate_test.go` and
verifies every decoded source hash before writing it. Original source strings
retain their exact line endings; only outer JSON checkout line endings can
vary across hosts. The two golden JSON SHA-256 values are:

- Rust: `843f48f743e967c17332d476751341fec53e566dd7a61f6c9921bc3e78ebd866`
- Polyglot: `6aac2e5dafe63bd75ffa233257796200a4ed8fb8734919b95d11a57deb6b2a40`
