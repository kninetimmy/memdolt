package embedding

import (
	"slices"
	"strings"
	"testing"
)

func TestBERTTruncationKeepsSpecialTokensAndLongestFirstPairBudget(t *testing.T) {
	tok, err := buildBertWordPieceTokenizer([]byte(`{"model":{"vocab":{"[PAD]":0,"[UNK]":100,"[CLS]":101,"[SEP]":102,"[MASK]":103,"alpha":104,"beta":105}}}`))
	if err != nil {
		t.Fatal(err)
	}
	short, err := encodeSingle(tok, "alpha beta")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(short.IDs, []int{101, 104, 105, 102}) {
		t.Fatalf("short input changed: %v", short.IDs)
	}
	for _, lengths := range [][2]int{{700, 0}, {10, 700}, {700, 10}, {700, 700}} {
		first, second := lengths[0], lengths[1]
		var enc encodedInput
		if second == 0 {
			enc, err = encodeSingle(tok, strings.Repeat("alpha ", first))
			first = 510
		} else {
			enc, err = encodePair(tok, strings.Repeat("alpha ", first), strings.Repeat("beta ", second))
			for first+second > 509 {
				if first > second {
					first--
				} else {
					second--
				}
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		want := append([]int{101}, slices.Repeat([]int{104}, first)...)
		want = append(want, 102)
		if second > 0 {
			want = append(want, slices.Repeat([]int{105}, second)...)
			want = append(want, 102)
		}
		if len(enc.IDs) != 512 || !slices.Equal(enc.IDs, want) || len(enc.TypeIDs) != 512 || len(enc.AttentionIDs) != 512 {
			t.Fatalf("%v input truncation: ids=%v types=%v", lengths, enc.IDs, enc.TypeIDs)
		}
		for i := range enc.IDs {
			typeID := 0
			if second > 0 && i > first+1 {
				typeID = 1
			}
			if enc.TypeIDs[i] != typeID || enc.AttentionIDs[i] != 1 {
				t.Fatalf("bad mask/type at %d: %+v", i, enc)
			}
		}
	}
}
