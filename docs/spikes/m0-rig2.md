# M0 rig 2 — ONNX-in-Go parity harness

PRD §16's second M0 rig: *"ONNX-in-Go: embeddings + rerank scores match
memhub within tolerance on a probe corpus (tokenizer ids byte-identical)."*
It also carries the job of resolving four **[verify]** markers: PRD §8.3's
Go tokenizer library candidate and its Linux-AMD64 onnxruntime bundling
question, and their mirrors in §14's tech-stack table.

## 1. Verdict

**PASS — with one load-bearing caveat that changes what "PASS" means.**

| Gate condition (PRD §16) | Measured |
|---|---|
| Tokenizer ids byte-identical (both models, whole probe corpus) | **31/31 texts, both tokenizers** — but only with a one-line caller-side compensation for a bug in the candidate library (§4) |
| Embeddings match within tolerance | max \|deviation\| **1.79×10⁻⁷** across 31 texts (tolerance 1×10⁻³) |
| Rerank scores match within tolerance | max \|deviation\| **0** (bit-identical) across 14 query/passage pairs (tolerance 1×10⁻²) |

Embedding and rerank parity are not close calls — the measured deviations
are four to five orders of magnitude inside their tolerances, and they are
that tight despite the Rust and Go sides linking two different onnxruntime
minor releases (1.24.2 vs. 1.26.0; §2, §7) over the same model bytes —
evidence of an equivalent computation reached two ways, not a coincidence
of identical binaries.
Tokenizer parity is a closer call: it is only byte-identical because this
harness NFD-normalizes text before handing it to
`github.com/sugarme/tokenizer`, compensating for that library's own
`BertNormalizer.StripAccents` never doing so itself (§4). Un-compensated,
2 of the 31 probe texts (the two carrying precomposed accented Latin
characters) tokenize differently from memhub's fastembed reference. That is
a real defect in the PRD's named candidate, not a probe-corpus artifact —
§4 reproduces it in three lines against the library directly, independent
of this harness's ONNX plumbing.

### What this PASS does and does not license

It says: BGE-small-en-v1.5 and ms-marco-MiniLM-L-6-v2, run through
`yalue/onnxruntime_go` from Go, reproduce memhub's fastembed-driven
embeddings and rerank scores closely enough that a Go retrieval pipeline
built on this stack should reproduce memhub's ranking behavior. It also
says `sugarme/tokenizer` is usable for M1, but *only* with the NFD
compensation this harness applies — shipping it unmodified would
silently mis-tokenize any fact, decision, or doc chunk containing accented
Latin script (French, German, Spanish, Portuguese, and more — not an edge
case for a coding-agent memory tool used on real-world codebases and
prose).

It does not cover CJK- or other non-Latin-script text quality (the CJK/emoji
probe text tokenizes identically to the reference specifically *because*
neither the reference nor the harness's vocabulary can represent it — see
§3), the reranker's int8 quantization path under adversarial inputs beyond
what 14 pairs exercise, or long-input truncation behavior (§5).

## 2. What the numbers were measured on

| | |
|---|---|
| Date | 2026-07-30 |
| Worktree base | `d931c24` on `main`, measured on the then-uncommitted working tree of `orch/issue-13-build-the-m0-rig-2-onnx-in-go-parity-har` before this rig's own work was committed on top of it |
| memhub commit measured against | `github.com/kninetimmy/memhub` v0.2.0, local checkout at `C:\Users\Kninetimmy\memhub` |
| OS | Windows 11 Pro, build 10.0.26200, amd64 |
| Go | go1.26.5 windows/amd64 |
| C toolchain | MinGW-W64 x86_64-ucrt-posix-seh (winlibs r3) GCC 16.1.0 |
| Go build settings | `CGO_ENABLED=1`, `-tags parity,gms_pure_go` |
| `github.com/sugarme/tokenizer` | v0.3.0 |
| `github.com/yalue/onnxruntime_go` | v1.31.0 |
| `golang.org/x/text` (NFD compensation) | v0.35.0 |
| onnxruntime shared library (Go side) | `onnxruntime.dll` bundled as test data inside the `yalue/onnxruntime_go@v1.31.0` module itself, **onnxruntime 1.26.0** per that module's README |
| Rust toolchain (fixture generator) | cargo/rustc 1.95.0 |
| `fastembed` (Rust side, memhub's exact pin) | 5.13.4, pinning `ort = "=2.0.0-rc.12"` |
| onnxruntime shared library (Rust side) | **onnxruntime 1.24.2** — `ort-sys-2.0.0-rc.12/build/download/dist.txt` maps `x86_64-pc-windows-msvc` (no accelerator) to `pyke:ort-rs/ms@1.24.2`, digest `b685bfc8d336e0ba95c066a7a982c03aa6dedd528a492eb99ca4ccb7f3af9e7a`, matching the cached download at `C:\Users\Kninetimmy\AppData\Local\ort.pyke.io\dfbin\x86_64-pc-windows-msvc\b685bfc8d336e0ba95c066a7a982c03aa6dedd528a492eb99ca4ccb7f3af9e7a\` |
| BGE-small-en-v1.5 model files | staged copy at memhub's `target/debug/build/memhub-*/out/bge-small-en-v1.5/`, SHA-256-verified against `tests/parity/testdata/model_pins.json` before use |
| ms-marco-MiniLM-L-6-v2 model files | staged copy at memhub's `target/debug/build/memhub-*/out/ms-marco-MiniLM-L-6-v2/`, SHA-256-verified the same way |

**The Rust and Go sides link two different onnxruntime minor releases —
1.24.2 and 1.26.0 — not the same build.** This was an open question in an
earlier draft of this document (§7); §5 folds in what it means for the
measured deviations.

**One platform only**, same caveat as rig 1: this is a Windows/amd64
measurement. §6 covers what that means for the Linux-AMD64 bundling
question specifically — the answer there does not depend on this platform,
but it is not independently re-verified on one.

## 3. The probe corpus

`tests/parity/testdata/corpus.json`: 31 synthetic, memdolt-shaped texts —
not real project data — spanning the shapes `recall` actually serves:

| Category | Count | Examples |
|---|---|---|
| `fact_key` | 6 | `embedding_model`, `fusion_formula`, `max_rerank_pool` |
| `fact_value` | 6 | `"6"`, `"relevance = 0.5*norm(lexical) + 0.5*cosine"`, model provenance strings |
| `decision` | 4 | title + rationale prose (int8 reranker adoption, ULID PKs, …) |
| `task` | 5 | short imperative task titles |
| `doc_chunk` | 5 | multi-line, LF-separated passages (retrieval pipeline, single-owner/IPC, rig-1 summary, …) |
| `unusual` | 5 | a Go code snippet, a URL+Windows-path snippet, German (umlauts + ß), Chinese + emoji, French (accents + guillemets) |

`tests/parity/testdata/rerank_pairs.json`: 14 (query, passage) pairs across
4 queries, `passage_id` resolving into the corpus above.

The CJK/emoji probe text (`non_ascii_cjk_emoji`) is a genuine parity check
even though both the Go and Rust paths reduce most of its characters to
`[UNK]` (id 100): BGE and ms-marco's shared vocabulary is a 30,522-token
*English* BERT WordPiece vocabulary with no Chinese characters or emoji in
it, so heavy `[UNK]` fallback is the *correct* behavior on both sides —
`tokenize_chinese_chars: true` still isolates each CJK codepoint as its own
pretokenized "word" (confirmed in `tokenizer.json` for both models, §4), so
the question this text answers is "does the Go pretokenizer's Chinese-char
boundary logic agree with the reference's," not "can this vocabulary
represent Chinese." It does agree: token ids matched byte-for-byte before
any compensation was needed.

## 4. Tokenizer ids: the bug, reproduced, and its fix

**Root cause.** `github.com/sugarme/tokenizer`'s `BertNormalizer` strip-accents
step (`normalizer.RemoveAccents`) is:

```go
func (n *NormalizedString) RemoveAccents() (retVal *NormalizedString) {
	return n.Filter(func(r rune) bool {
		return !unicode.Is(unicode.Mn, r)
	})
}
```

It filters runes in Unicode category Mn (non-spacing mark) — correct *only*
if the string has already been decomposed into base characters plus
combining marks (Unicode Normalization Form D). Precomposed Latin-1/Latin
Extended characters like U+00F6 ("ö") are a single codepoint in category
`Ll` (lowercase letter), not `Mn` — the accent is only exposed as a
separately filterable `Mn` rune *after* NFD decomposition. HF's reference
implementations do this decomposition as part of the same step (Python
`transformers`' `BasicTokenizer._run_strip_accents` calls
`unicodedata.normalize("NFD", text)` first; the Rust `tokenizers` crate's
`BertNormalizer` uses `unicode-normalization`'s `.nfd()` the same way).
`sugarme/tokenizer` v0.3.0 does not, anywhere in its `BertNormalizer`
pipeline. Reproduced directly against the library, independent of this
harness's ONNX code:

```go
n := normalizer.NewNormalizedFrom("Größe")
bn := normalizer.NewBertNormalizer(true, true, true, true) // clean, lowercase, handle-chinese, strip-accents
out, _ := bn.Normalize(n)
fmt.Printf("%q\n", out.GetNormalized()) // "größe" — unchanged apart from lowercasing
```

Un-compensated, this produced non-byte-identical token ids for the two
probe texts carrying precomposed accented Latin characters
(`non_ascii_german`, `non_ascii_french`) — everything else in the 31-text
corpus, including the punctuation-heavy code/URL snippets and the CJK/emoji
text, matched exactly on the first attempt:

```
non_ascii_german: BGE token ids differ
  got:  [101 3280 100 4078 100 100 20405 19317 19204 2015 1010 6151 1096 15536 4103 27969 11039 10047 5017 25520 1000 7020 1000 3671 17417 8743 1012 102]
  want: [101 3280 24665 2080 17499 4078 24185 19418 25987 2015 6655 29181 2102 20405 19317 19204 2015 1010 6151 1096 15536 4103 27969 11039 10047 5017 25520 1000 7020 1000 3671 17417 8743 1012 102]
```

**The fix.** `tests/inference/tokenizer.go`'s `nfdCompensate` NFD-normalizes
text (`golang.org/x/text/unicode/norm`, already an indirect dependency of
this module via dolt/vitess — no new third-party dependency) before it ever
reaches the tokenizer, restoring the decomposition `sugarme`'s own
`StripAccents` assumes has already happened:

```go
n := normalizer.NewNormalizedFrom(norm.NFD.String("Größe"))
// ... "große" — accent correctly stripped
```

With that one call in place, all 31×2 = 62 tokenizations (31 probe texts ×
2 tokenizers) matched byte-for-byte, including both accented-text probes.

`tests/parity/testdata/tokens_bge.json` and `tokens_reranker.json` are
byte-identical files: BGE-small-en-v1.5 and ms-marco-MiniLM-L-6-v2 ship the
same `tokenizer.json` (`model_pins.json`'s two `sha256` entries for it
match), so "both tokenizers" is one shared BERT WordPiece tokenizer built
twice from identical bytes and measured once, not two distinct
vocabularies. What single-sequence encoding alone cannot exercise is
*pair* encoding — the `[CLS] query [SEP] passage [SEP]` template with
`token_type_ids` the reranker actually runs on (§5) — and that path is
covered by the rerank scores' bit-identical result rather than by a
separate pair-encoding token-id fixture: a wrong offset or type-id in the
pair template would show up as a wrong logit, and none did across all 14
pairs.

**What this means for the [verify] marker.** `sugarme/tokenizer` is usable
for M1 — but *only* alongside this compensation, which is not something the
library does for its callers. Any M1 code that adopts it must NFD-normalize
input before encoding, or it will silently mis-tokenize accented Latin
script (and, by the same mechanism, any other script whose normal form
relies on combining marks). That is a real, load-bearing implementation
note for whoever picks this library up next, not a footnote — it belongs in
the M1 code that wires up `internal/embedding`'s tokenizer, not just in this
document.

## 5. Embeddings and rerank: full measured run

Per-text embedding deviations (max absolute difference across all 384
dimensions, BGE-small-en-v1.5, CLS-pooled + L2-normalized on both sides):

```
fact_key_embedding_model:          2.98e-08
fact_value_embedding_model:        1.19e-07
fact_key_rerank_model:             2.98e-08
fact_value_rerank_model:           8.94e-08
fact_key_single_owner:             2.98e-08
fact_value_single_owner:           1.49e-08
fact_key_recall_limit:             7.45e-09
fact_value_recall_limit:           5.96e-08
fact_key_fusion_formula:           2.98e-08
fact_value_fusion_formula:         1.49e-08
fact_key_max_rerank_pool:          8.94e-08
fact_value_max_rerank_pool:        8.94e-08
decision_int8_reranker:            2.98e-08
decision_reject_dolt_vector_index: 8.94e-08
decision_embeddings_out_of_repo:   8.94e-08
decision_ulid_pks:                 5.96e-08
task_wire_rig2:                    2.98e-08
task_resolve_tokenizer_verify:     5.96e-08
task_stage_model_files:            1.49e-08
task_write_spike_doc:              5.96e-08
task_ci_lint_gate:                 2.98e-08
doc_chunk_retrieval_pipeline:      2.98e-08
doc_chunk_single_owner_ipc:        1.79e-07  <- measured max
doc_chunk_embedding_side_store:    1.49e-08
doc_chunk_m0_gate:                 8.94e-08
doc_chunk_rig1_soak_summary:       8.94e-08
code_snippet_commit_method:        1.49e-08
url_and_path_snippet:              5.96e-08
non_ascii_german:                  2.98e-08
non_ascii_cjk_emoji:               1.19e-07
non_ascii_french:                  2.98e-08
```

Every value is within one to two orders of magnitude of `float32` epsilon
(~1.19×10⁻⁷) — the signature of independent-but-equivalent floating-point
summation order across two onnxruntime bindings, not a behavioral
difference. **Measured max: 1.79×10⁻⁷, tolerance 1×10⁻³** (~5,600× margin).

Rerank scores (raw cross-encoder logit, no sigmoid, matching memhub's
`rerank.rs` exactly — column 0 of `logits`, both sides):

```
q1_single_owner_vs_single_owner_fact:  -11.073666  vs -11.073666  |Δ| 0
q1_single_owner_vs_ipc_doc:            -11.047071  vs -11.047071  |Δ| 0
q1_single_owner_vs_ulid_decision:      -11.056389  vs -11.056389  |Δ| 0
q1_single_owner_vs_gate_doc:           -11.046994  vs -11.046994  |Δ| 0
q2_reranker_vs_reranker_fact:           -3.973858  vs  -3.973858  |Δ| 0
q2_reranker_vs_int8_decision:           -2.320503  vs  -2.320503  |Δ| 0
q2_reranker_vs_embedding_fact:         -10.980812  vs -10.980812  |Δ| 0
q2_reranker_vs_code_snippet:           -11.081811  vs -11.081811  |Δ| 0
q3_fusion_vs_fusion_fact:              -10.010750  vs -10.010750  |Δ| 0
q3_fusion_vs_pipeline_doc:              -7.311951  vs  -7.311951  |Δ| 0
q3_fusion_vs_cjk_text:                 -10.706572  vs -10.706572  |Δ| 0
q4_german_vs_german_text:                6.073356  vs   6.073356  |Δ| 0
q4_german_vs_french_text:              -10.989250  vs -10.989250  |Δ| 0
q4_german_vs_side_store_doc:           -11.066606  vs -11.066606  |Δ| 0
```

**Measured max: 0 (bit-identical) across all 14 pairs, tolerance 1×10⁻².**
`q4_german_vs_german_text`'s +6.07 against the other pairs' ~-11 to -2 range
is also a useful sanity check independent of the parity question: the
cross-encoder clearly separates the one genuinely on-topic pair from the
rest, on both language stacks identically.

**Why these tolerances.** Both are chosen as defensible ceilings against
version and platform drift the actual measurement here doesn't exercise —
not as thresholds tuned to make today's numbers pass. That drift is not
hypothetical: the Rust and Go sides measured here link two *different*
onnxruntime minor releases (§2) — 1.24.2 (Rust, via `ort`'s pyke-hosted
download) and 1.26.0 (Go, via `yalue/onnxruntime_go`'s bundled test
binary) — over byte-identical model files, and still landed at 1.79×10⁻⁷
max embedding deviation and bit-identical rerank scores. That makes the
parity result *stronger* than "the same binary reached two ways" would
have shown: two onnxruntime minor versions apart agree to `float32`
noise-floor precision on both models, on this probe set. This run is also
still single-platform (§2), so the tolerances stay a deliberate margin
above what was actually measured rather than a fit to it. 1×10⁻³
for embeddings is far above `float32` accumulation noise but well below
anything that would change a cosine-similarity ranking at the precision
`recall`'s fusion formula (PRD §8.1) actually uses; 1×10⁻² for rerank
scores is small relative to the ~1-15-point spread separating relevant from
irrelevant pairs above, so a deviation at the tolerance boundary still
couldn't flip a ranking decision in this probe set. Both are an order of
magnitude looser than what was actually measured, deliberately: this run
had a small corpus (31 texts, 14 pairs) and one platform, and a tolerance
tuned to *today's* noise floor would be re-litigated by the next person who
runs this on Linux or macOS.

## 6. PRD §8.3 / §14's onnxruntime-bundling `[verify]` marker, resolved

The marker: *"Runtime: `yalue/onnxruntime_go` **[V]** (bundled shared libs
for Windows AMD64, Linux ARM64, macOS ARM64; …). **[verify]** Linux AMD64
bundling; supply `ONNXRUNTIME_SHARED_LIBRARY_PATH` path if absent."*

**Resolved: there is no bundling gap specific to Linux AMD64 — because
there is no bundling at all, for any platform, in the sense the marker's
phrasing implies.**

`yalue/onnxruntime_go` does not embed, statically link, or auto-fetch an
onnxruntime shared library for its consumers on *any* platform. Its own
README says so explicitly: *"you'll also need a copy of the correct version
of the onnxruntime shared library or DLL for your operating system and
architecture. Prior to initializing `onnxruntime_go`, you need to provide a
path to this shared library"* via `SetSharedLibraryPath`. What the PRD's
"[bundled … Windows AMD64, Linux ARM64, macOS ARM64]" parenthetical is
accurately pointing at is that module's own `test_data/` directory — used
only by its own test suite — which as of v1.31.0 contains exactly:

```
test_data/onnxruntime.dll             (Windows AMD64)
test_data/onnxruntime_arm64.so        (Linux ARM64)
test_data/onnxruntime_arm64.dylib     (macOS ARM64)
```

confirming the three-platform list verbatim, and confirming the absence of
a Linux AMD64 file the same way. This harness used exactly that Windows
AMD64 file (§2) via `ONNXRUNTIME_SHARED_LIBRARY_PATH`, fetched into the Go
module cache as an ordinary side effect of `go get`-ing the module — not
because the library exposes any supported API for consuming it, but because
Go module downloads include a repository's whole tree including its test
fixtures.

**What this means for Linux AMD64 specifically.** It needs the same
treatment as every other platform, not a special case: Microsoft's own
onnxruntime releases publish an official `onnxruntime-linux-x64-<version>.tgz`
release asset for every release including 1.26.0 (confirmed against the
GitHub Releases API for `microsoft/onnxruntime` tag `v1.26.0`, alongside
the `linux-aarch64`, `win-x64`, and `osx-arm64` assets the other three
platforms would use). There is no missing upstream artifact. memdolt's own
"first-run fetch into `~/.memdolt/models/`, every file SHA-256-pinned"
mechanism (PRD §8.3) is the right shape for staging this too — model files
and the onnxruntime shared library are the same kind of problem, and this
harness's `StagedModelFiles`/SHA-256-verification code
(`tests/inference/inference.go`, factored out for reuse by M0 rig 3 —
docs/spikes/m0-rig3.md) is a small working example of exactly that
pattern, generalizable from "model files" to "the runtime library" with no
new mechanism.

## 7. What could not be measured, and what was assumed

**One platform.** Windows/amd64 only (§2), same limitation as rig 1. The
Linux-AMD64 answer in §6 is a documented-artifact-exists claim, not an
independently re-run parity measurement on that platform — nothing here
ran an onnxruntime Linux AMD64 shared library end to end.

**Resolved: the Rust and Go sides do not link the same onnxruntime
version, and it doesn't matter at the precision measured here.** fastembed
5.13.4 pins `ort = "=2.0.0-rc.12"`; that crate's own
`ort-sys-2.0.0-rc.12/build/download/dist.txt` maps the no-accelerator
`x86_64-pc-windows-msvc` target to `pyke:ort-rs/ms@1.24.2` (digest
`b685bfc8d336e0ba95c066a7a982c03aa6dedd528a492eb99ca4ccb7f3af9e7a`,
matching the copy already cached locally at
`%LOCALAPPDATA%\ort.pyke.io\dfbin\x86_64-pc-windows-msvc\` from building
memhub) — **onnxruntime 1.24.2**. The Go side used the `onnxruntime.dll`
bundled in `yalue/onnxruntime_go@v1.31.0`'s test data, coupled to
**onnxruntime 1.26.0** per that module's README. Two different minor
releases, not a hedge — and §5's measured deviations (1.79×10⁻⁷ max on
embeddings, bit-identical on rerank) are the result *with* that gap in
place, which makes the parity claim stronger than "same onnxruntime build,
two bindings" would have been.

**Truncation and very long inputs are untested.** Every probe text in this
corpus is short enough that `max_length=512` truncation (fastembed's
default, matching both models' `tokenizer_config.json`) never triggers on
either side — deliberately, to keep this harness's job "does tokenization
and inference agree" rather than also re-deriving `tokenizers`' truncation
edge-case behavior in Go. Whatever code eventually chunks long documents
before embedding them (PRD §9's chunker) inherits this gap, not this rig.

**Only int8 quantization's happy path.** The reranker is int8-quantized
(PRD §8.3); this run exercised it on 14 ordinary-language pairs. Nothing
here says anything about quantization-sensitive adversarial inputs.

## 8. What this resolves in the PRD

- **§8.3, tokenizer-lib marker** — resolved: `sugarme/tokenizer` v0.3.0,
  *with* the caller-side NFD-normalization compensation in §4. Marker text
  updated in place to record the measurement and point here.
- **§14, tech-stack table's tokenizer-lib marker** — same resolution,
  updated in place with a pointer here.
- **§8.3, Linux-AMD64 onnxruntime-bundling marker** — resolved per §6:
  no special-case gap; Linux AMD64 needs the same SHA-256-pinned
  fetch-and-stage treatment as every other platform, via an official
  `onnxruntime-linux-x64-<version>.tgz` release asset and
  `ONNXRUNTIME_SHARED_LIBRARY_PATH`. Marker text updated in place.

## 9. Reproducing this

```sh
export CGO_ENABLED=1
export GOFLAGS=-tags=gms_pure_go

# 1. Stage BGE-small-en-v1.5 and ms-marco-MiniLM-L-6-v2's model files
#    (model.onnx, tokenizer.json, config.json, special_tokens_map.json,
#    tokenizer_config.json) under one directory, each pair in its own
#    bge-small-en-v1.5/ and ms-marco-MiniLM-L-6-v2/ subdirectory. A memhub
#    checkout already has them staged after any local `cargo build` at
#    target/debug/build/memhub-*/out/. Otherwise fetch from the pinned URLs
#    in tests/parity/testdata/model_pins.json and verify each SHA-256.

# 2. (Optional — only if regenerating fixtures) regenerate the Rust
#    reference outputs:
MEMDOLT_PARITY_MODEL_DIR=<staged model dir> \
  cargo run --manifest-path tests/parity/fixturegen/Cargo.toml
# commit the regenerated tests/parity/testdata/*.json if anything changed.

# 3. Run the Go parity harness:
export MEMDOLT_PARITY_MODEL_DIR=<staged model dir>
export ONNXRUNTIME_SHARED_LIBRARY_PATH=<path to an onnxruntime 1.26.0 shared library>
# On Windows AMD64 with no other copy handy, the yalue/onnxruntime_go
# module's own test fixture works (see §6):
#   $(go env GOMODCACHE)/github.com/yalue/onnxruntime_go@v1.31.0/test_data/onnxruntime.dll
go test -tags parity,gms_pure_go ./tests/parity/... -v -timeout 30m
```

Omitting either environment variable, or pointing `MEMDOLT_PARITY_MODEL_DIR`
at a directory with a mismatched or missing file, fails the test loudly
with the fetch/stage instructions above rather than skipping (verified: see
`tests/inference/inference.go`'s `RequireEnv`/`StagedModelFiles`).

That paragraph records the M0 rig as it ran; M2 deliberately removed that
operational behavior. Before M2, both variables were required and missing
artifacts only produced manual staging instructions. After M2, the same rigs
call `internal/embedding.Open`: overrides are optional, missing artifacts are
fetched from `models/manifest.json` unless offline mode is requested, and all
pre-positioned or fetched bytes are verified by the production path before
ONNX initialization. The old `tests/inference` package and
`tests/parity/testdata/model_pins.json` no longer exist as second copies.

Plain `go test ./...` never runs any of this — same convention as rig 1's
`soak` tag (docs/spikes/m0-rig1.md §14): a command-line `-tags` replaces
`GOFLAGS`'s tag list rather than adding to it, so `gms_pure_go` has to be
repeated on any `-tags` invocation or the build fails inside
`go-icu-regex`.

## 10. M2 production handoff blast radius

| Structural element touched | Before M2 | After M2; does prior behavior still hold? |
|---|---|---|
| Model/runtime pin authority | `tests/parity/testdata/model_pins.json` pinned model files only. | `models/manifest.json` is the sole authority and additionally pins immutable model revisions plus each official runtime archive and extracted library. Model identities and hashes still hold; the test-only authority is removed. |
| Artifact cache and fetch | Rigs only verified manually staged model files; callers supplied the runtime path without a committed runtime checksum. | `internal/embedding` verifies existing cache files on every open, fetches only missing files, verifies downloads before atomic cache installation, and extracts a runtime only after its archive verifies. The manual verified staging path still holds through `Options.Offline`; unverified runtime acceptance does not. |
| Supported runtime structure | Windows AMD64 was measured; three other official assets were documented but not wired. | One manifest-selected provisioner covers Windows AMD64, Linux AMD64, Linux ARM64, and macOS ARM64. The M0 claim remains limited to measured parity on Windows AMD64; this change does not rewrite it as four-platform parity measurement. |
| Tokenizer | `tests/inference.nfdCompensate` repaired sugarme's precomposed-accent bug for both rigs. | The same logic moved unchanged to `internal/embedding`; every `Engine` token, embed, and rerank method routes through it, so byte-identical token behavior and the NFD requirement still hold. This restriction binds those `Engine` methods, not every possible `sugarme/tokenizer` value in the process. |
| Embedding and rerank sessions | `tests/inference` owned the BGE CLS pooling/L2 normalization and raw ms-marco logit logic. | The logic moved to production `internal/embedding` without changing dimensions, pooling, epsilon, tensor names, logits, or tolerances. `Engine` serializes its own calls; that restriction is per `Engine`, not a process-wide ban on parallel engines. |
| ONNX environment ownership | Each rig's `TestMain` initialized and destroyed yalue's process-global environment directly. | `embedding.Open` initializes only after provisioning succeeds; `Engine.Close` destroys it after the final engine closes. `Open` refuses an environment initialized outside this package because its runtime verification cannot be established. That refusal belongs to this production entry point, not to yalue's symbols generally. |
| Parity and golden rigs | Both imported build-tagged `tests/inference`; parity separately loaded test pins. | Both import `internal/embedding` and call `Engine` methods. Their build tags, fixtures, recorded tolerances, NFD token probe, retrieval pipeline, and golden gates still hold; the second tokenizer/session implementation does not. |
| Rust fixture generator and provenance | The independent fastembed reference read the test-only model pin JSON. | It reads the model portion of production `models/manifest.json`; reference generation, staged-byte verification, and fixture formats still hold. It remains an independent reference implementation, not production inference. |
| Default Go tests and dependencies | Full inference rigs stayed out of `go test ./...`; sugarme, yalue, and x/text were already direct dependencies. | The tagged rigs still stay out, while provisioning unit tests use tiny local TLS fixtures. No dependency changed. |
| Documentation | This report and the PRD described `tests/inference`, required rig environment variables, and first-run fetch as future design. | The historical M0 statements remain above with this before/after. PRD §8.3 and MD10 now record the production boundary and implemented distribution behavior instead of deleting the old evidence. |
