package localapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
)

func cursorScope(r *http.Request) string {
	q := r.URL.Query()
	q.Del("cursor")
	if q.Get("limit") == "" {
		q.Set("limit", "50")
	}
	return r.URL.Path + "?" + q.Encode()
}
func (h *handler) encodeCursor(r *http.Request, upstream string) string {
	data, _ := json.Marshal([]string{cursorScope(r), upstream})
	mac := hmac.New(sha256.New, h.key)
	mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(append(mac.Sum(nil), data...))
}
func (h *handler) decodeCursor(r *http.Request, encoded string) (string, error) {
	invalid := failure(400, "invalid_cursor", "Cursor is invalid for this request or instance")
	if len(encoded) > 16384 {
		return "", invalid
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(data) < sha256.Size {
		return "", invalid
	}
	mac := hmac.New(sha256.New, h.key)
	mac.Write(data[sha256.Size:])
	if !hmac.Equal(data[:sha256.Size], mac.Sum(nil)) {
		return "", invalid
	}
	var pair []string
	if json.Unmarshal(data[sha256.Size:], &pair) != nil || len(pair) != 2 || pair[0] != cursorScope(r) {
		return "", invalid
	}
	return pair[1], nil
}
