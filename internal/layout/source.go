package layout

import (
	"errors"
	"fmt"
	"os"
)

// ErrOwnerSource identifies the known repository owner's credential file.
var ErrOwnerSource = errors.New("source reads refuse protected memdolt owner metadata")

// CheckOwnerSource compares an already-open source with this repository's
// owner file before any content read. The metadata directory is already
// confined by the caller. Only identity is read from the protected file.
// Before CLI file bodies, its callers were document ingestion and code-index
// readers. It now also protects the CLI body-file reader, not arbitrary readers.
func CheckOwnerSource(metadata *os.Root, source os.FileInfo) (err error) {
	if metadata == nil {
		return nil // No metadata directory, therefore no known owner file.
	}
	expected, err := metadata.Lstat(PidFileName)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify protected memdolt owner metadata: %w", err)
	}
	if !expected.Mode().IsRegular() {
		return errors.New("cannot verify protected memdolt owner metadata: expected a regular file")
	}
	owner, err := metadata.Open(PidFileName)
	if err != nil {
		return fmt.Errorf("open protected memdolt owner metadata for identity verification: %w", err)
	}
	actual, statErr := owner.Stat()
	if err := errors.Join(statErr, owner.Close()); err != nil {
		return fmt.Errorf("inspect/close protected memdolt owner metadata: %w", err)
	}
	if !actual.Mode().IsRegular() || !os.SameFile(expected, actual) {
		return errors.New("protected memdolt owner metadata identity could not be verified")
	}
	// Both compared identities are obtained from File.Stat on open handles.
	// In particular, a failed later Windows lookup cannot look like inequality.
	if os.SameFile(source, actual) {
		return ErrOwnerSource
	}
	return nil
}
