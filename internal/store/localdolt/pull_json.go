package localdolt

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
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
	if err := ValidateJSONUnicode(data); err != nil {
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

// ValidateJSONUnicode rejects text encoding that encoding/json would replace.
// JSON grammar and duplicate/unknown fields remain the caller's decoder's job.
func ValidateJSONUnicode(data []byte) error {
	_, err := io.Copy(io.Discard, JSONUnicodeReader(bytes.NewReader(data)))
	return err
}

// JSONUnicodeReader passes bytes and framing unchanged, checking each UTF-8
// rune / escaped UTF-16 pair before exposing it. Buffering is bounded to the
// standard bufio reader plus one 12-byte surrogate pair, independent of message
// size. It neither closes the source nor replaces the SDK's protocol connection.
func JSONUnicodeReader(source io.Reader) io.Reader {
	return &jsonUnicodeReader{source: bufio.NewReader(source)}
}

type jsonUnicodeReader struct {
	source  *bufio.Reader
	unit    [12]byte
	pending []byte
	err     error
}

func (r *jsonUnicodeReader) Read(p []byte) (n int, err error) {
	for n < len(p) {
		if len(r.pending) == 0 {
			if r.err != nil {
				return n, r.err
			}
			r.pending, r.err = r.nextUnit()
			if r.err != nil {
				return n, r.err
			}
		}
		copied := copy(p[n:], r.pending)
		r.pending = r.pending[copied:]
		n += copied
		// Do not wait for a later message merely to fill the caller's buffer.
		if len(r.pending) == 0 && r.source.Buffered() == 0 {
			break
		}
	}
	return n, nil
}

func (r *jsonUnicodeReader) nextUnit() ([]byte, error) {
	first, err := r.source.ReadByte()
	if err != nil {
		return nil, err
	}
	r.unit[0] = first
	n := 1
	if first >= utf8.RuneSelf {
		for !utf8.FullRune(r.unit[:n]) {
			next, err := r.source.ReadByte()
			if err != nil {
				return nil, errors.New("JSON text must be valid UTF-8")
			}
			r.unit[n] = next
			n++
		}
		if !utf8.Valid(r.unit[:n]) {
			return nil, errors.New("JSON text must be valid UTF-8")
		}
	} else if first == '\\' {
		next, err := r.source.ReadByte()
		if err != nil {
			return nil, io.ErrUnexpectedEOF
		}
		r.unit[1], n = next, 2
		if next >= utf8.RuneSelf {
			return nil, errors.New("invalid JSON escape")
		}
		if next == 'u' {
			high, err := r.readHex(2)
			if err != nil {
				return nil, err
			}
			n = 6
			if utf16.IsSurrogate(high) {
				if high >= 0xdc00 {
					return nil, errors.New("JSON Unicode surrogate escapes must be paired")
				}
				for i, want := range []byte{'\\', 'u'} {
					got, err := r.source.ReadByte()
					if err != nil || got != want {
						return nil, errors.New("JSON Unicode surrogate escapes must be paired")
					}
					r.unit[6+i] = got
				}
				low, err := r.readHex(8)
				if err != nil {
					return nil, err
				}
				if utf16.DecodeRune(high, low) == utf8.RuneError {
					return nil, errors.New("JSON Unicode surrogate escapes must be paired")
				}
				n = 12
			}
		}
	}
	return r.unit[:n], nil
}

func (r *jsonUnicodeReader) readHex(offset int) (rune, error) {
	for i := offset; i < offset+4; i++ {
		b, err := r.source.ReadByte()
		if err != nil || !strings.ContainsRune("0123456789abcdefABCDEF", rune(b)) {
			return 0, errors.New("invalid JSON Unicode escape")
		}
		r.unit[i] = b
	}
	value, err := strconv.ParseUint(string(r.unit[offset:offset+4]), 16, 16)
	return rune(value), err
}

// ValidateText runs before direct SQL arguments or OwnerStore's JSON marshal.
// Checking after marshal cannot recover bytes encoding/json already replaced.
func (o TransferOptions) ValidateText() error {
	text := []string{o.Remote, o.User, o.Author.Name, o.Author.Email}
	if o.Resolution != nil {
		text = append(text, o.Resolution.LocalCommit, o.Resolution.RemoteCommit)
		for _, choice := range o.Resolution.Choices {
			text = append(text, choice.Conflict, choice.Take, choice.Winner)
			for column, value := range choice.Row {
				text = append(text, column)
				if value != nil {
					text = append(text, *value)
				}
			}
		}
	}
	for _, value := range text {
		if !utf8.ValidString(value) {
			return errors.New("transfer text must be valid UTF-8")
		}
	}
	return nil
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
