package localapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"
)

// uniqueJSON checks object keys recursively before typed decoding, which the
// standard Decoder otherwise accepts with last-key-wins semantics.
func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting exceeds 64 levels")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			token, err = d.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || seen[key] {
				return errors.New("duplicate JSON key")
			}
			seen[key] = true
			if err = uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err = uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}
func decodeBody(w http.ResponseWriter, r *http.Request, target any) *Error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return failure(415, "unsupported_media_type", "Expected application/json")
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return failure(413, "body_too_large", "Request body exceeds 256 KiB")
		}
		return failure(400, "invalid_json", "Could not read request body")
	}
	trimmed := strings.TrimSpace(string(data))
	if !utf8.Valid(data) || len(trimmed) == 0 || trimmed[0] != '{' {
		return failure(400, "invalid_json", "Expected a UTF-8 JSON object")
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	if uniqueJSON(d, 0) != nil {
		return failure(400, "invalid_json", "Malformed JSON or duplicate object keys")
	}
	if _, err = d.Token(); !errors.Is(err, io.EOF) {
		return failure(400, "invalid_json", "Expected one JSON value")
	}
	d = json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil {
		return failure(400, "invalid_json", "Malformed JSON or unknown fields")
	}
	return nil
}
