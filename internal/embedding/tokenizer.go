package embedding

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
	return encodedInput{
		IDs:          enc.GetIds(),
		AttentionIDs: enc.GetAttentionMask(),
		TypeIDs:      enc.GetTypeIds(),
	}, nil
}

func encodePair(tok *sgtokenizer.Tokenizer, query, passage string) (encodedInput, error) {
	enc, err := tok.EncodePair(nfdCompensate(query), nfdCompensate(passage), true)
	if err != nil {
		return encodedInput{}, fmt.Errorf("encode pair: %w", err)
	}
	return encodedInput{
		IDs:          enc.GetIds(),
		AttentionIDs: enc.GetAttentionMask(),
		TypeIDs:      enc.GetTypeIds(),
	}, nil
}
