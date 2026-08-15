package memory

import (
	"bytes"
	"crypto/rand"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func TestNewIDProducesTimeSortableULIDs(t *testing.T) {
	fixed := time.UnixMilli(1_786_750_400_123)
	first := newIDAt(fixed)
	second := newIDAt(fixed)
	for _, id := range []string{first, second} {
		if _, err := ulid.ParseStrict(id); err != nil {
			t.Fatalf("newID() = %q, not a strict ULID: %v", id, err)
		}
	}
	if first >= second {
		t.Fatalf("IDs minted in the same millisecond are not monotonic: %q then %q", first, second)
	}
}

func TestNewIDReadsCryptographicEntropy(t *testing.T) {
	original := rand.Reader
	originalEntropy := idEntropy
	entropy := bytes.NewBuffer(make([]byte, 10))
	rand.Reader = entropy
	idEntropy = &ulid.LockedMonotonicReader{MonotonicReader: ulid.Monotonic(rand.Reader, 0)}
	t.Cleanup(func() {
		rand.Reader = original
		idEntropy = originalEntropy
	})

	if _, err := ulid.ParseStrict(newID()); err != nil {
		t.Fatalf("newID with controlled crypto/rand input: %v", err)
	}
	if entropy.Len() != 0 {
		t.Fatalf("newID consumed %d of 10 crypto/rand bytes", 10-entropy.Len())
	}
}
