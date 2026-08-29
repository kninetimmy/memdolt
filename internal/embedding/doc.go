// Package embedding provides memdolt's production tokenizer, inference engine,
// and derived SQLite embedding side-store. Open provisions every model and
// runtime artifact through the committed SHA-256 manifest before initializing
// ONNX Runtime; vectors remain outside Dolt history (PRD §8.2).
package embedding
