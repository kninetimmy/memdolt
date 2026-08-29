// Package models exposes the committed inference artifact manifest without
// embedding the model or runtime payloads in the memdolt binary.
package models

import _ "embed"

//go:embed manifest.json
var manifestJSON []byte

// ManifestJSON returns an isolated copy of the committed artifact manifest.
func ManifestJSON() []byte {
	return append([]byte(nil), manifestJSON...)
}
