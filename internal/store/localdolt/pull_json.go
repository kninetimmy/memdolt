package localdolt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// DecodePullResolution and DecodePullChoice share strict operator-input parsing
// across the terminal and MCP forms. Unknown, duplicate, or trailing fields are
// refused before a store operation can be submitted.
func DecodePullResolution(r io.Reader) (*PullResolution, error) {
	var result PullResolution
	if err := decodePullJSON(r, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func DecodePullChoice(r io.Reader) (PullChoice, error) {
	var result PullChoice
	err := decodePullJSON(r, &result)
	return result, err
}

func decodePullJSON(r io.Reader, target any) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := uniquePullJSON(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("resolution must contain exactly one JSON value")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func uniquePullJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	keys := map[string]bool{}
	for decoder.More() {
		if delimiter == '{' {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			// encoding/json matches struct field names without case sensitivity.
			// Reject aliases too, rather than letting the last spelling win.
			key = strings.ToLower(key)
			if !ok || keys[key] {
				return errors.New("resolution contains a duplicate or invalid JSON object key")
			}
			keys[key] = true
		}
		if err := uniquePullJSON(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}
