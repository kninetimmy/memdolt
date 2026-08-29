//! M0 rig-2 (PRD §16) reference-fixture generator.
//!
//! Loads the exact SHA-256-pinned BGE-small-en-v1.5 and ms-marco-MiniLM-L-6-v2
//! model files memhub's `build.rs` declares, drives them through fastembed 5
//! (the same crate and code path memhub ships), and writes reference token
//! ids, embeddings, and rerank scores for `tests/parity/testdata/corpus.json`
//! to `tests/parity/testdata/*.json`. The Go parity harness in
//! `tests/parity` (build tag `parity`) reproduces the same computation in Go
//! and diffs against these files.
//!
//! Never built by `go build`/`go test` — this is a standalone Cargo crate.
//! Run manually:
//!
//!   MEMDOLT_PARITY_MODEL_DIR=<dir with bge-small-en-v1.5/ and
//!     ms-marco-MiniLM-L-6-v2/ subdirs, memhub's build.rs OUT_DIR layout>
//!   cargo run --manifest-path tests/parity/fixturegen/Cargo.toml
//!
//! The staged bytes are verified against production models/manifest.json;
//! commit the regenerated tests/parity/testdata/*.json.

use std::collections::BTreeMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use anyhow::{bail, Context, Result};
use fastembed::{
    InitOptionsUserDefined, OnnxSource, Pooling, RerankInitOptionsUserDefined, TextEmbedding,
    TextRerank, TokenizerFiles, UserDefinedEmbeddingModel, UserDefinedRerankingModel,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

const FASTEMBED_VERSION: &str = "5.13.4";

#[derive(Deserialize)]
struct ModelFile {
    remote_path: String,
    local_name: String,
    sha256: String,
}

#[derive(Deserialize)]
struct ModelBundle {
    name: String,
    base_url: String,
    files: Vec<ModelFile>,
}

#[derive(Deserialize)]
struct ModelPins {
    bundles: Vec<ModelBundle>,
}

#[derive(Deserialize)]
struct CorpusText {
    id: String,
    text: String,
}

#[derive(Deserialize)]
struct Corpus {
    texts: Vec<CorpusText>,
}

#[derive(Deserialize)]
struct RerankPair {
    id: String,
    query: String,
    passage_id: String,
}

#[derive(Deserialize)]
struct RerankPairs {
    pairs: Vec<RerankPair>,
}

#[derive(Serialize)]
struct RerankFixtureEntry {
    id: String,
    query: String,
    passage_id: String,
    score: f32,
}

#[derive(Serialize)]
struct ProvenanceBundleFile {
    local_name: String,
    sha256: String,
}

#[derive(Serialize)]
struct ProvenanceBundle {
    name: String,
    files: Vec<ProvenanceBundleFile>,
}

#[derive(Serialize)]
struct Provenance {
    generated_at_unix_seconds: u64,
    fastembed_version: &'static str,
    generator_source: &'static str,
    model_pins_source: &'static str,
    model_dir: String,
    bundles: Vec<ProvenanceBundle>,
    note: &'static str,
}

fn main() -> Result<()> {
    let worktree_root = Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .and_then(Path::parent)
        .and_then(Path::parent)
        .context("fixturegen must live at tests/parity/fixturegen under the memdolt worktree")?
        .to_path_buf();
    let testdata_dir = worktree_root.join("tests").join("parity").join("testdata");

    let model_dir = std::env::var("MEMDOLT_PARITY_MODEL_DIR").context(
        "MEMDOLT_PARITY_MODEL_DIR is not set.\n\
         Point it at a directory containing bge-small-en-v1.5/ and \
         ms-marco-MiniLM-L-6-v2/ subdirectories, each holding model.onnx, \
         tokenizer.json, config.json, special_tokens_map.json and \
         tokenizer_config.json (memhub build.rs's OUT_DIR staging layout, \
         e.g. memhub's target/debug/build/memhub-*/out/). If those files are \
         not staged locally, fetch them from the pinned Hugging Face URLs in \
         models/manifest.json and verify each SHA-256 \
         before use.",
    )?;
    let model_dir = PathBuf::from(model_dir);

    let pins: ModelPins = serde_json::from_str(
        &fs::read_to_string(worktree_root.join("models").join("manifest.json"))
            .context("reading models/manifest.json")?,
    )?;

    let corpus: Corpus = serde_json::from_str(
        &fs::read_to_string(testdata_dir.join("corpus.json"))
            .context("reading tests/parity/testdata/corpus.json")?,
    )?;

    let rerank_pairs: RerankPairs = serde_json::from_str(
        &fs::read_to_string(testdata_dir.join("rerank_pairs.json"))
            .context("reading tests/parity/testdata/rerank_pairs.json")?,
    )?;

    let mut provenance_bundles = Vec::new();
    let mut staged: BTreeMap<String, BTreeMap<String, Vec<u8>>> = BTreeMap::new();

    for bundle in &pins.bundles {
        let bundle_dir = model_dir.join(&bundle.name);
        let mut files = BTreeMap::new();
        let mut prov_files = Vec::new();
        for file in &bundle.files {
            let path = bundle_dir.join(&file.local_name);
            let bytes = fs::read(&path).with_context(|| {
                format!(
                    "reading {} (expected staged copy of {}{})\n\
                     Fetch it and verify sha256={} before use.",
                    path.display(),
                    bundle.base_url,
                    if file.remote_path.starts_with('/') {
                        file.remote_path.clone()
                    } else {
                        format!("/{}", file.remote_path)
                    },
                    file.sha256
                )
            })?;
            let actual = hex_sha256(&bytes);
            if !actual.eq_ignore_ascii_case(&file.sha256) {
                bail!(
                    "{}: sha256 mismatch: expected {}, got {actual}. Re-fetch from {}/{} and verify before use.",
                    path.display(),
                    file.sha256,
                    bundle.base_url,
                    file.remote_path
                );
            }
            prov_files.push(ProvenanceBundleFile {
                local_name: file.local_name.clone(),
                sha256: actual,
            });
            files.insert(file.local_name.clone(), bytes);
        }
        provenance_bundles.push(ProvenanceBundle {
            name: bundle.name.clone(),
            files: prov_files,
        });
        staged.insert(bundle.name.clone(), files);
    }

    let bge_files = &staged["bge-small-en-v1.5"];
    let reranker_files = &staged["ms-marco-MiniLM-L-6-v2"];

    let bge_tokenizer_files = TokenizerFiles {
        tokenizer_file: bge_files["tokenizer.json"].clone(),
        config_file: bge_files["config.json"].clone(),
        special_tokens_map_file: bge_files["special_tokens_map.json"].clone(),
        tokenizer_config_file: bge_files["tokenizer_config.json"].clone(),
    };
    let user_embedding_model =
        UserDefinedEmbeddingModel::new(bge_files["model.onnx"].clone(), bge_tokenizer_files)
            .with_pooling(Pooling::Cls);
    let mut embedding_model = TextEmbedding::try_new_from_user_defined(
        user_embedding_model,
        InitOptionsUserDefined::new(),
    )
    .context("loading BGE-small-en-v1.5 via fastembed")?;

    let reranker_tokenizer_files = TokenizerFiles {
        tokenizer_file: reranker_files["tokenizer.json"].clone(),
        config_file: reranker_files["config.json"].clone(),
        special_tokens_map_file: reranker_files["special_tokens_map.json"].clone(),
        tokenizer_config_file: reranker_files["tokenizer_config.json"].clone(),
    };
    let user_rerank_model = UserDefinedRerankingModel::new(
        OnnxSource::Memory(reranker_files["model.onnx"].clone()),
        reranker_tokenizer_files,
    );
    let mut reranker = TextRerank::try_new_from_user_defined(
        user_rerank_model,
        RerankInitOptionsUserDefined::default(),
    )
    .context("loading ms-marco-MiniLM-L-6-v2 via fastembed")?;

    // Per-text token ids for both tokenizers, and 384-dim embeddings for
    // every probe text (acceptance criterion 1).
    let mut tokens_bge: BTreeMap<String, Vec<u32>> = BTreeMap::new();
    let mut tokens_reranker: BTreeMap<String, Vec<u32>> = BTreeMap::new();
    let mut embeddings: BTreeMap<String, Vec<f32>> = BTreeMap::new();
    let mut text_by_id: BTreeMap<String, String> = BTreeMap::new();

    for entry in &corpus.texts {
        text_by_id.insert(entry.id.clone(), entry.text.clone());

        let bge_encoding = embedding_model
            .tokenizer
            .encode(entry.text.as_str(), true)
            .map_err(|e| anyhow::anyhow!("{e}"))
            .with_context(|| format!("bge tokenizer encode({})", entry.id))?;
        tokens_bge.insert(entry.id.clone(), bge_encoding.get_ids().to_vec());

        let reranker_encoding = reranker
            .tokenizer
            .encode(entry.text.as_str(), true)
            .map_err(|e| anyhow::anyhow!("{e}"))
            .with_context(|| format!("reranker tokenizer encode({})", entry.id))?;
        tokens_reranker.insert(entry.id.clone(), reranker_encoding.get_ids().to_vec());

        // batch_size=Some(1): every reference embedding is produced from a
        // lone-text forward pass, so the fixture is independent of whatever
        // batch size a caller later chooses.
        let mut vecs = embedding_model
            .embed(&[entry.text.as_str()], Some(1))
            .with_context(|| format!("embed({})", entry.id))?;
        let vec = vecs
            .pop()
            .ok_or_else(|| anyhow::anyhow!("empty embed() output for {}", entry.id))?;
        embeddings.insert(entry.id.clone(), vec);
    }

    // Rerank scores for the probe query/passage pairs (acceptance criterion 1).
    let mut rerank_fixture = Vec::new();
    for pair in &rerank_pairs.pairs {
        let passage = text_by_id
            .get(&pair.passage_id)
            .with_context(|| format!("rerank pair {} references unknown passage_id {}", pair.id, pair.passage_id))?
            .clone();
        let results = reranker
            .rerank(pair.query.as_str(), &[passage.as_str()], false, None)
            .with_context(|| format!("rerank({})", pair.id))?;
        let score = results
            .first()
            .ok_or_else(|| anyhow::anyhow!("empty rerank() output for {}", pair.id))?
            .score;
        rerank_fixture.push(RerankFixtureEntry {
            id: pair.id.clone(),
            query: pair.query.clone(),
            passage_id: pair.passage_id.clone(),
            score,
        });
    }

    write_json(&testdata_dir.join("tokens_bge.json"), &tokens_bge)?;
    write_json(&testdata_dir.join("tokens_reranker.json"), &tokens_reranker)?;
    write_json(&testdata_dir.join("embeddings.json"), &embeddings)?;
    write_json(&testdata_dir.join("rerank.json"), &rerank_fixture)?;

    let generated_at = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();
    let provenance = Provenance {
        generated_at_unix_seconds: generated_at,
        fastembed_version: FASTEMBED_VERSION,
        generator_source: "tests/parity/fixturegen/src/main.rs",
        model_pins_source: "models/manifest.json (production pins; model hashes copied from memhub's build.rs)",
        model_dir: model_dir.display().to_string(),
        bundles: provenance_bundles,
        note: "Re-run with `MEMDOLT_PARITY_MODEL_DIR=<dir> cargo run --manifest-path tests/parity/fixturegen/Cargo.toml` from the memdolt worktree root to regenerate every fixture in this directory.",
    };
    write_json(&testdata_dir.join("provenance.json"), &provenance)?;

    println!(
        "wrote fixtures for {} probe texts and {} rerank pairs to {}",
        corpus.texts.len(),
        rerank_pairs.pairs.len(),
        testdata_dir.display()
    );
    Ok(())
}

fn write_json<T: Serialize>(path: &Path, value: &T) -> Result<()> {
    let json = serde_json::to_string_pretty(value)?;
    fs::write(path, json).with_context(|| format!("writing {}", path.display()))
}

fn hex_sha256(bytes: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(bytes);
    let digest = hasher.finalize();
    let mut out = String::with_capacity(digest.len() * 2);
    for b in digest {
        out.push_str(&format!("{b:02x}"));
    }
    out
}
