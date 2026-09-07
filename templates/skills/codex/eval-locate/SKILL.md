---
name: eval-locate
description: Evaluate the production locator against a matching golden corpus.
compatibility: codex
---

Run `memdolt eval locate --golden <file> --dir <repository> --json` using the
user's golden file and its matching corpus. Preserve every query, matcher and
outcome. This invokes production locate with lazy refresh, writes only the
local code index, and changes no durable memory or memory-retrieval metrics.

Report Recall@1, Recall@3, match counts and empty-probe failures. Default fusion
has no floor, so nonsense probes can leak. For a separate safety measurement,
run with `--rerank --min-rerank-score 0`; that floor applies only in the harness
and can lose true matches. Report those losses instead of changing the queries
or runtime filtering. Never treat a passing safety measurement as full recall.

The bundled Rust and polyglot JSON require their frozen public reference
corpora, documented in `tests/golden/testdata/locate/NOTICE.md`; they are not a
golden set for an arbitrary working tree. Run the fixed-corpus gate with
`CGO_ENABLED=1` and
`go test -tags golden,gms_pure_go ./tests/golden/... -run TestLocateGolden -count=1 -timeout 30m`.
Models and native runtime are checksum-verified by the production pipeline.
