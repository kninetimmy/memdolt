package embedding

import (
	"encoding/json"
	"fmt"
	"slices"

	sgtokenizer "github.com/sugarme/tokenizer"
	sgmodel "github.com/sugarme/tokenizer/model"
	"github.com/sugarme/tokenizer/model/wordpiece"
	"github.com/sugarme/tokenizer/normalizer"
	"github.com/sugarme/tokenizer/pretokenizer"
	"github.com/sugarme/tokenizer/processor"
	"golang.org/x/text/unicode/norm"
)

// tokenizerJSONFile is the slice of a Hugging Face tokenizer.json this
// package needs: the WordPiece vocabulary. Both pinned models use uncased
// BERT WordPiece with the standard [CLS]/[SEP] template.
type tokenizerJSONFile struct {
	Model struct {
		Vocab map[string]int `json:"vocab"`
	} `json:"model"`
}

func buildBertWordPieceTokenizer(tokenizerJSON []byte) (*sgtokenizer.Tokenizer, error) {
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

	for _, token := range []string{"[PAD]", "[UNK]", "[CLS]", "[SEP]", "[MASK]"} {
		tok.AddSpecialTokens([]sgtokenizer.AddedToken{sgtokenizer.NewAddedToken(token, true)})
	}
	return tok, nil
}

type encodedInput struct {
	IDs          []int
	AttentionIDs []int
	TypeIDs      []int
}

// nfdCompensate works around sugarme/tokenizer v0.3.0's precomposed-accent
// bug. Its BertNormalizer removes Mn runes without first performing the NFD
// decomposition that Hugging Face and Rust tokenizers perform. This
// compensation is required on every production encoding path.
func nfdCompensate(text string) string {
	return norm.NFD.String(text)
}

func encodeSingle(tok *sgtokenizer.Tokenizer, text string) (encodedInput, error) {
	enc, err := tok.EncodeSingle(nfdCompensate(text), true)
	if err != nil {
		return encodedInput{}, fmt.Errorf("encode single: %w", err)
	}
	return truncateBERT(encodedInput{
		IDs:          enc.GetIds(),
		AttentionIDs: enc.GetAttentionMask(),
		TypeIDs:      enc.GetTypeIds(),
	}, false), nil
}

func encodePair(tok *sgtokenizer.Tokenizer, query, passage string) (encodedInput, error) {
	enc, err := tok.EncodePair(nfdCompensate(query), nfdCompensate(passage), true)
	if err != nil {
		return encodedInput{}, fmt.Errorf("encode pair: %w", err)
	}
	return truncateBERT(encodedInput{
		IDs:          enc.GetIds(),
		AttentionIDs: enc.GetAttentionMask(),
		TypeIDs:      enc.GetTypeIds(),
	}, true), nil
}

// Both pinned models use fastembed's 512-token, right-truncated LongestFirst
// policy, including BERT's two/three special tokens. Before code indexing,
// long inputs could exceed the model's positions and fail native inference.
// sugarme v0.3.0's TruncateEncodings decrements both pair lengths in its
// LongestFirst loop; trim the already-encoded sequences here instead.
func truncateBERT(enc encodedInput, pair bool) encodedInput {
	const maxTokens = 512
	if len(enc.IDs) <= maxTokens {
		return enc
	}
	boundary := len(enc.IDs)
	if pair {
		boundary = slices.Index(enc.TypeIDs, 1)
	}
	first, second, special := boundary-2, 0, 2
	if pair {
		second, special = len(enc.IDs)-boundary-1, 3
	}
	for first+second > maxTokens-special {
		if first > second {
			first--
		} else {
			second--
		}
	}
	clip := func(values []int) []int {
		out := slices.Clone(values[:first+1]) // CLS and retained first sequence.
		out = append(out, values[boundary-1]) // First SEP.
		if pair {
			out = append(out, values[boundary:boundary+second]...)
			out = append(out, values[len(values)-1]) // Final SEP.
		}
		return out
	}
	return encodedInput{IDs: clip(enc.IDs), AttentionIDs: clip(enc.AttentionIDs), TypeIDs: clip(enc.TypeIDs)}
}
