//go:build parity || golden

package inference

import (
	"encoding/json"
	"fmt"

	sgtokenizer "github.com/sugarme/tokenizer"
	sgmodel "github.com/sugarme/tokenizer/model"
	"github.com/sugarme/tokenizer/model/wordpiece"
	"github.com/sugarme/tokenizer/normalizer"
	"github.com/sugarme/tokenizer/pretokenizer"
	"github.com/sugarme/tokenizer/processor"
	"golang.org/x/text/unicode/norm"
)

// tokenizerJSONFile is the slice of a Hugging Face tokenizer.json this
// package needs: the WordPiece vocabulary. BGE-small-en-v1.5 and
// ms-marco-MiniLM-L-6-v2 both bundle a standard uncased BERT tokenizer
// (normalizer: BertNormalizer{clean_text, handle_chinese_chars: true,
// strip_accents: null (→ true, tied to lowercase), lowercase: true},
// pre_tokenizer: BertPreTokenizer, post_processor: TemplateProcessing
// equivalent to BertProcessing's "[CLS] A [SEP]" / "[CLS] A [SEP] B [SEP]"),
// confirmed by reading both models' tokenizer_config.json and tokenizer.json
// directly (docs/spikes/m0-rig2.md records the exact fields). This package
// hand-wires those same components from github.com/sugarme/tokenizer rather
// than parsing the rest of tokenizer.json, because that package has no
// from-file loader for the full HF tokenizer.json config (its
// NewTokenizerFromFile is an unimplemented stub as of v0.3.0) — see
// pretrained/bert.go in that module for the upstream-endorsed pattern this
// mirrors.
type tokenizerJSONFile struct {
	Model struct {
		Vocab map[string]int `json:"vocab"`
	} `json:"model"`
}

// BuildBertWordPieceTokenizer constructs a Go WordPiece tokenizer from the
// vocabulary embedded in a Hugging Face tokenizer.json's bytes, wired up to
// match BAAI/bge-small-en-v1.5 and Xenova/ms-marco-MiniLM-L-6-v2's shared
// tokenizer config (both uncased BERT WordPiece with the standard
// [CLS]/[SEP] template).
func BuildBertWordPieceTokenizer(tokenizerJSON []byte) (*sgtokenizer.Tokenizer, error) {
	var tj tokenizerJSONFile
	if err := json.Unmarshal(tokenizerJSON, &tj); err != nil {
		return nil, fmt.Errorf("parsing tokenizer.json: %w", err)
	}
	if len(tj.Model.Vocab) == 0 {
		return nil, fmt.Errorf("tokenizer.json has an empty model.vocab")
	}

	vocab := sgmodel.Vocab(tj.Model.Vocab)
	wp := wordpiece.NewWordPieceBuilder().
		Vocab(&vocab).
		UnkToken("[UNK]").
		Build()

	tok := sgtokenizer.NewTokenizer(wp)
	// clean_text=true, lowercase=true, handle_chinese_chars=true,
	// strip_accents=true (tokenizer.json's strip_accents is null, which HF's
	// BertTokenizer resolves to do_lower_case's value — true here).
	tok.WithNormalizer(normalizer.NewBertNormalizer(true, true, true, true))
	tok.WithPreTokenizer(pretokenizer.NewBertPreTokenizer())

	sepID, ok := tok.TokenToId("[SEP]")
	if !ok {
		return nil, fmt.Errorf("vocab has no [SEP] token")
	}
	clsID, ok := tok.TokenToId("[CLS]")
	if !ok {
		return nil, fmt.Errorf("vocab has no [CLS] token")
	}
	tok.WithPostProcessor(processor.NewBertProcessing(
		processor.PostToken{Value: "[SEP]", Id: sepID},
		processor.PostToken{Value: "[CLS]", Id: clsID},
	))

	// Registering the special tokens as "added" tokens matches
	// pretrained/bert.go's convention: it keeps them from being split by
	// the pre-tokenizer/WordPiece model if they ever appear literally in
	// input text.
	tok.AddSpecialTokens([]sgtokenizer.AddedToken{sgtokenizer.NewAddedToken("[PAD]", true)})
	tok.AddSpecialTokens([]sgtokenizer.AddedToken{sgtokenizer.NewAddedToken("[UNK]", true)})
	tok.AddSpecialTokens([]sgtokenizer.AddedToken{sgtokenizer.NewAddedToken("[CLS]", true)})
	tok.AddSpecialTokens([]sgtokenizer.AddedToken{sgtokenizer.NewAddedToken("[SEP]", true)})
	tok.AddSpecialTokens([]sgtokenizer.AddedToken{sgtokenizer.NewAddedToken("[MASK]", true)})

	return tok, nil
}

// EncodedInput carries the model-ready encoding of one sequence (or
// sequence pair): token ids, attention mask, and BERT segment (token_type)
// ids, all the same length.
type EncodedInput struct {
	IDs          []int
	AttentionIDs []int
	TypeIDs      []int
}

// nfdCompensate works around a bug in sugarme/tokenizer v0.3.0:
// normalizer.RemoveAccents (BertNormalizer's strip_accents step) filters
// Unicode category Mn (non-spacing marks) directly on its input, but never
// Unicode-NFD-decomposes it first. HF's reference implementation does
// (transformers' BasicTokenizer._run_strip_accents calls
// unicodedata.normalize("NFD", text) before the same filter; the Rust
// `tokenizers` crate's BertNormalizer does the equivalent via
// unicode-normalization's .nfd()). Precomposed accented Latin characters
// (e.g. U+00F6 "ö") are therefore never in category Mn themselves — the
// accent only becomes a separate, filterable Mn rune after NFD decomposition
// — so sugarme's StripAccents is a near no-op on ordinary accented input:
// confirmed directly (docs/spikes/m0-rig2.md) by normalizing "Größe"
// through sugarme's BertNormalizer, which returns "größe" unchanged apart
// from lowercasing.
//
// NFD-decomposing here before handing text to the tokenizer restores the
// same behavior sugarme's own StripAccents is documented to provide,
// without touching sugarme's code. golang.org/x/text is already an indirect
// dependency of this module (pulled in by dolt/vitess), so this adds no new
// third-party dependency.
func nfdCompensate(text string) string {
	return norm.NFD.String(text)
}

func EncodeSingle(tok *sgtokenizer.Tokenizer, text string) (EncodedInput, error) {
	enc, err := tok.EncodeSingle(nfdCompensate(text), true)
	if err != nil {
		return EncodedInput{}, fmt.Errorf("encode single: %w", err)
	}
	return EncodedInput{
		IDs:          enc.GetIds(),
		AttentionIDs: enc.GetAttentionMask(),
		TypeIDs:      enc.GetTypeIds(),
	}, nil
}

func EncodePair(tok *sgtokenizer.Tokenizer, query, passage string) (EncodedInput, error) {
	enc, err := tok.EncodePair(nfdCompensate(query), nfdCompensate(passage), true)
	if err != nil {
		return EncodedInput{}, fmt.Errorf("encode pair: %w", err)
	}
	return EncodedInput{
		IDs:          enc.GetIds(),
		AttentionIDs: enc.GetAttentionMask(),
		TypeIDs:      enc.GetTypeIds(),
	}, nil
}
