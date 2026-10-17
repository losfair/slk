package localapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type metadata struct {
	RequestID  string   `json:"request_id"`
	Source     string   `json:"source"`
	NextCursor any      `json:"next_cursor,omitempty"`
	Parent     *Message `json:"parent,omitempty"`
	Truncated  bool     `json:"truncated,omitempty"`
}
type response struct {
	Data  any      `json:"data,omitempty"`
	Error *Error   `json:"error,omitempty"`
	Meta  metadata `json:"meta"`
}

type handler struct {
	backend  Backend
	instance string
	key      []byte
	ctx      context.Context
	mu       sync.Mutex
	active   int
	teams    map[string]int
	ledger   map[string]*record
}

func newHandler(ctx context.Context, backend Backend, instance string) *handler {
	return &handler{backend: backend, instance: instance, key: []byte(randomID()), ctx: ctx,
		teams: make(map[string]int), ledger: make(map[string]*record)}
}
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("localapi: random source unavailable")
	}
	return hex.EncodeToString(b[:])
}
func failure(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message, Definitive: true}
}
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	id := randomID()
	respond := func(status int, data any, err *Error, source string) {
		h.write(w, status, response{Data: data, Error: err, Meta: metadata{RequestID: id, Source: source}})
	}
	if r.Host != "slk.local" || len(r.Header.Values("Origin")) != 0 {
		respond(403, nil, failure(403, "request_forbidden", "Host or Origin is not allowed"), "local")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		respond(405, nil, failure(405, "method_not_allowed", "Method is not supported"), "local")
		return
	}
	if r.URL.Path == "/v1/status" && r.Method == http.MethodGet {
		state := "unavailable"
		for _, workspace := range h.backend.Workspaces() {
			if workspace.State == "connecting" {
				state = "starting"
				break
			}
			if workspace.State == "ready" {
				state = "ready"
			}
		}
		respond(200, map[string]any{"version": "v1", "state": state, "instance_id": h.instance, "api_versions": []string{"v1"},
			"capabilities": []string{"read", "write"}, "idempotency_retention_seconds": int(retention.Seconds())}, nil, "local")
		return
	}
	if r.URL.Path == "/v1/workspaces" && r.Method == http.MethodGet {
		data := h.backend.Workspaces()
		if data == nil {
			data = []Workspace{}
		}
		respond(200, data, nil, "local")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "v1" || parts[1] != "workspaces" || !validID(parts[2]) {
		respond(404, nil, failure(404, "not_found", "Route not found"), "local")
		return
	}
	team := parts[2]
	if !h.admit(team) {
		e := failure(429, "local_busy", "Local API is busy")
		e.Retryable = true
		e.RetryAfter = time.Second
		respond(429, nil, e, "local")
		return
	}
	defer h.release(team)
	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	if r.Method == http.MethodPost {
		h.post(w, r, parts, id)
		return
	}
	result, err := h.get(ctx, r, parts)
	if err != nil {
		e := asError(err)
		respond(e.Status, nil, e, "local")
		return
	}
	meta := metadata{RequestID: id, Source: "slack", Parent: result.Parent, Truncated: result.Truncated}
	if isCollection(parts) {
		cursor := ""
		if result.Cursor != "" {
			cursor = h.encodeCursor(r, result.Cursor)
		}
		meta.NextCursor = json.RawMessage("null")
		if cursor != "" {
			meta.NextCursor = cursor
		}
	}
	h.write(w, 200, response{Data: result.Data, Meta: meta})
}
func isCollection(p []string) bool {
	return len(p) == 4 && (p[3] == "conversations" || p[3] == "search") ||
		len(p) == 6 && p[3] == "conversations" && p[5] == "messages" ||
		len(p) == 8 && p[3] == "conversations" && p[5] == "threads" && p[7] == "replies"
}
func (h *handler) admit(team string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.active >= 8 || h.teams[team] >= 2 {
		return false
	}
	h.active++
	h.teams[team]++
	return true
}
func (h *handler) release(team string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active--
	h.teams[team]--
	if h.teams[team] == 0 {
		delete(h.teams, team)
	}
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var tsPattern = regexp.MustCompile(`^[0-9]{1,20}\.[0-9]{1,9}$`)

func validID(s string) bool { return idPattern.MatchString(s) }
func (h *handler) get(ctx context.Context, r *http.Request, p []string) (Result, error) {
	page, err := h.page(r)
	if err != nil {
		return Result{}, err
	}
	switch {
	case len(p) == 4 && p[3] == "conversations":
		return h.backend.Conversations(ctx, p[2], page)
	case len(p) == 4 && p[3] == "search":
		if strings.TrimSpace(page.Query) == "" || len(page.Query) > 4096 || !utf8.ValidString(page.Query) {
			return Result{}, failure(400, "invalid_query", "q must contain 1–4096 UTF-8 bytes")
		}
		return h.backend.Search(ctx, p[2], page)
	case len(p) == 5 && p[3] == "users" && validID(p[4]):
		user, err := h.backend.User(ctx, p[2], p[4])
		return Result{Data: user}, err
	case len(p) == 5 && p[3] == "conversations" && validID(p[4]):
		c, err := h.backend.Conversation(ctx, p[2], p[4])
		return Result{Data: c}, err
	case len(p) == 6 && p[3] == "conversations" && validID(p[4]) && p[5] == "messages":
		return h.backend.History(ctx, p[2], p[4], page)
	case len(p) == 8 && p[3] == "conversations" && validID(p[4]) && p[5] == "threads" && tsPattern.MatchString(p[6]) && p[7] == "replies":
		return h.backend.Replies(ctx, p[2], p[4], p[6], page)
	default:
		return Result{}, failure(404, "not_found", "Route not found")
	}
}
func (h *handler) page(r *http.Request) (PageRequest, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return PageRequest{}, failure(400, "invalid_query", "Malformed query string")
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	allowed := map[string]bool{}
	if isCollection(parts) {
		allowed["limit"] = true
		allowed["cursor"] = true
	}
	if len(parts) == 4 && parts[3] == "search" {
		allowed["q"] = true
	}
	if len(parts) == 6 && parts[5] == "messages" {
		allowed["oldest"] = true
		allowed["latest"] = true
	}
	for key, values := range q {
		if len(values) != 1 || !allowed[key] {
			return PageRequest{}, failure(400, "invalid_query", "Unknown or repeated query parameter")
		}
	}
	page := PageRequest{Limit: 50, Oldest: q.Get("oldest"), Latest: q.Get("latest"), Query: q.Get("q")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return PageRequest{}, failure(400, "invalid_limit", "limit must be between 1 and 100")
		}
		page.Limit = n
	}
	for _, ts := range []string{page.Oldest, page.Latest} {
		if ts != "" && !tsPattern.MatchString(ts) {
			return PageRequest{}, failure(400, "invalid_timestamp", "Expected a decimal Slack timestamp")
		}
	}
	if page.Oldest != "" && page.Latest != "" && compareTS(page.Oldest, page.Latest) >= 0 {
		return PageRequest{}, failure(400, "invalid_range", "oldest must precede latest")
	}
	if q.Get("cursor") != "" {
		page.Cursor, err = h.decodeCursor(r, q.Get("cursor"))
	}
	return page, err
}
func compareTS(a, b string) int {
	aParts, bParts := strings.SplitN(a, ".", 2), strings.SplitN(b, ".", 2)
	as, bs := strings.TrimLeft(aParts[0], "0"), strings.TrimLeft(bParts[0], "0")
	if len(as) < len(bs) {
		return -1
	}
	if len(as) > len(bs) {
		return 1
	}
	if n := strings.Compare(as, bs); n != 0 {
		return n
	}
	return strings.Compare(aParts[1]+strings.Repeat("0", 9-len(aParts[1])), bParts[1]+strings.Repeat("0", 9-len(bParts[1])))
}
func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &Error{Status: 504, Code: "deadline_exceeded", Message: "Operation deadline exceeded", Retryable: true}
	}
	return &Error{Status: 502, Code: "upstream_failure", Message: "Slack request failed", Retryable: true}
}
func (h *handler) write(w http.ResponseWriter, status int, body response) {
	if body.Error != nil && body.Error.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(body.Error.RetryAfter.Seconds()))))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
