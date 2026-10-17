package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBackend struct {
	Backend
	history func(context.Context, string, string, PageRequest) (Result, error)
	send    func(context.Context, string, string, SendRequest) (Message, error)
}

func (*fakeBackend) Workspaces() []Workspace {
	return []Workspace{{ID: "T1", Name: "test", State: "ready"}}
}
func (*fakeBackend) Conversations(context.Context, string, PageRequest) (Result, error) {
	return Result{Data: []Conversation{{ID: "C1"}}}, nil
}
func (*fakeBackend) Conversation(context.Context, string, string) (Conversation, error) {
	return Conversation{ID: "C1"}, nil
}
func (b *fakeBackend) History(ctx context.Context, team, channel string, p PageRequest) (Result, error) {
	if b.history != nil {
		return b.history(ctx, team, channel, p)
	}
	return Result{Data: []Message{}}, nil
}
func (*fakeBackend) Replies(context.Context, string, string, string, PageRequest) (Result, error) {
	return Result{Data: []Message{}, Parent: &Message{TS: "1.000001"}}, nil
}
func (*fakeBackend) Search(context.Context, string, PageRequest) (Result, error) {
	return Result{Data: []Message{}}, nil
}
func (*fakeBackend) User(context.Context, string, string) (User, error) { return User{ID: "U1"}, nil }
func (*fakeBackend) Open(context.Context, string, OpenRequest) (OpenResult, error) {
	return OpenResult{ChannelID: "D1"}, nil
}
func (b *fakeBackend) Send(ctx context.Context, team, channel string, r SendRequest) (Message, error) {
	if b.send != nil {
		return b.send(ctx, team, channel, r)
	}
	return Message{TS: "1.000001", Text: r.Text}, nil
}
func apiRequest(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://slk.local"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Slk-Instance", "instance")
	r.Header.Set("Idempotency-Key", "test-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func responseCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body response
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error == nil {
		return ""
	}
	return body.Error.Code
}
func TestReadsWithoutAuthentication(t *testing.T) {
	h := newHandler(context.Background(), &fakeBackend{}, "instance")
	for _, path := range []string{"/v1/status", "/v1/workspaces", "/v1/workspaces/T1/conversations", "/v1/workspaces/T1/conversations/C1", "/v1/workspaces/T1/conversations/C1/messages", "/v1/workspaces/T1/conversations/C1/threads/1.000001/replies", "/v1/workspaces/T1/search?q=hello", "/v1/workspaces/T1/users/U1"} {
		t.Run(path, func(t *testing.T) {
			w := apiRequest(h, "GET", path, "")
			if w.Code != 200 {
				t.Fatalf("%d: %s", w.Code, w.Body)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing no-store")
			}
		})
	}
	w := apiRequest(h, "GET", "/v1/workspaces/T1/conversations/C1/messages", "")
	if !strings.Contains(w.Body.String(), `"next_cursor":null`) || !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Fatal(w.Body)
	}
}
func TestCursorScopeAndWorkspaceRouting(t *testing.T) {
	calls := 0
	h := newHandler(context.Background(), &fakeBackend{history: func(_ context.Context, team, channel string, p PageRequest) (Result, error) {
		calls++
		if team != "T1" || channel != "C1" || p.Limit != 20 {
			t.Fatalf("wrong target: %s %s %+v", team, channel, p)
		}
		if calls == 2 && p.Cursor != "upstream-cursor" {
			t.Fatalf("cursor: %q", p.Cursor)
		}
		return Result{Data: []Message{}, Cursor: "upstream-cursor"}, nil
	}}, "instance")
	path := "/v1/workspaces/T1/conversations/C1/messages?limit=20"
	w := apiRequest(h, "GET", path, "")
	var body struct {
		Meta struct {
			Next string `json:"next_cursor"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || body.Meta.Next == "" {
		t.Fatal(w.Body)
	}
	w = apiRequest(h, "GET", path+"&cursor="+body.Meta.Next, "")
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	for _, path := range []string{
		"/v1/workspaces/T2/conversations/C1/messages?limit=20&cursor=" + body.Meta.Next,
		"/v1/workspaces/T1/conversations/C1/messages?limit=21&cursor=" + body.Meta.Next,
		"/v1/workspaces/T1/conversations/C1/messages?limit=20&cursor=x" + body.Meta.Next,
	} {
		w = apiRequest(h, "GET", path, "")
		if responseCode(t, w) != "invalid_cursor" {
			t.Fatal(w.Body)
		}
	}
	restarted := newHandler(context.Background(), &fakeBackend{}, "next")
	if code := responseCode(t, apiRequest(restarted, "GET", path+"&cursor="+body.Meta.Next, "")); code != "invalid_cursor" {
		t.Fatal(code)
	}
	if calls != 2 {
		t.Fatalf("invalid requests reached backend: %d", calls)
	}
}
func TestWriteValidation(t *testing.T) {
	h := newHandler(context.Background(), &fakeBackend{}, "instance")
	for _, tc := range []struct{ name, body, code string }{
		{"empty", "", "invalid_json"}, {"whitespace", "  ", "invalid_json"}, {"null", "null", "invalid_json"},
		{"unknown", `{"text":"hello","extra":true}`, "invalid_json"},
		{"duplicate", `{"text":"first","text":"second"}`, "invalid_json"},
		{"trailing", `{"text":"hello"} {}`, "invalid_json"},
		{"blank", `{"text":" "}`, "invalid_message"},
		{"broadcast", `{"text":"hello","broadcast":true}`, "invalid_message"},
		{"bad ts", `{"text":"hello","thread_ts":"123"}`, "invalid_message"},
		{"too long", `{"text":"` + strings.Repeat("a", 32769) + `"}`, "invalid_message"},
		{"body limit", `{"text":"` + strings.Repeat("a", 256<<10) + `"}`, "body_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := responseCode(t, apiRequest(h, "POST", "/v1/workspaces/T1/conversations/C1/messages", tc.body)); got != tc.code {
				t.Fatalf("got %q, want %q", got, tc.code)
			}
		})
	}
	for _, header := range []string{"X-Slk-Instance", "Idempotency-Key", "Content-Type"} {
		r := httptest.NewRequest("POST", "http://slk.local/v1/workspaces/T1/conversations/C1/messages", strings.NewReader(`{"text":"hello"}`))
		r.Header.Set("X-Slk-Instance", "instance")
		r.Header.Set("Idempotency-Key", "key")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Del(header)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatalf("accepted missing %s", header)
		}
	}
}
func TestWritesWithoutAuthenticationAndDuplicateSuppression(t *testing.T) {
	var calls atomic.Int32
	h := newHandler(context.Background(), &fakeBackend{send: func(_ context.Context, team, channel string, r SendRequest) (Message, error) {
		calls.Add(1)
		if team != "T1" || channel != "C1" || r.ThreadTS != "1.000001" || !r.Broadcast {
			t.Fatal("lost target or thread")
		}
		return Message{TS: "2.000001", Text: r.Text}, nil
	}}, "instance")
	path := "/v1/workspaces/T1/conversations/C1/messages"
	first := apiRequest(h, "POST", path, `{"text":"hello","thread_ts":"1.000001","broadcast":true}`)
	replay := apiRequest(h, "POST", path, `{"broadcast":true,"thread_ts":"1.000001","text":"hello"}`)
	if first.Code != 201 || replay.Code != 201 || first.Body.String() != replay.Body.String() || calls.Load() != 1 {
		t.Fatalf("first %s replay %s calls %d", first.Body, replay.Body, calls.Load())
	}
	if got := responseCode(t, apiRequest(h, "POST", path, `{"text":"changed"}`)); got != "idempotency_conflict" {
		t.Fatal(got)
	}
	if w := apiRequest(h, "POST", "/v1/workspaces/T1/conversations", `{"user_ids":["U1"]}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
}
func TestConcurrentWriteAndClientCancellation(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	h := newHandler(context.Background(), &fakeBackend{send: func(ctx context.Context, _, _ string, _ SendRequest) (Message, error) {
		calls.Add(1)
		close(started)
		<-finish
		if err := ctx.Err(); err != nil {
			return Message{}, err
		}
		return Message{TS: "2.000001"}, nil
	}}, "instance")
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "http://slk.local/v1/workspaces/T1/conversations/C1/messages", strings.NewReader(`{"text":"hi"}`)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Slk-Instance", "instance")
	r.Header.Set("Idempotency-Key", "test-key")
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(first, r); close(done) }()
	<-started
	cancel()
	replay := apiRequest(h, "POST", r.URL.Path, `{"text":"hi"}`)
	if responseCode(t, replay) != "operation_in_progress" {
		t.Fatal(replay.Body)
	}
	close(finish)
	<-done
	if first.Code != 201 || calls.Load() != 1 {
		t.Fatalf("%d %s calls %d", first.Code, first.Body, calls.Load())
	}
}
func TestUnknownWriteNeverAutomaticallyRetried(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, errors.New("private credential must not be exposed")} {
		calls := 0
		h := newHandler(context.Background(), &fakeBackend{send: func(context.Context, string, string, SendRequest) (Message, error) { calls++; return Message{}, err }}, "instance")
		for range 2 {
			w := apiRequest(h, "POST", "/v1/workspaces/T1/conversations/C1/messages", `{"text":"hello"}`)
			if responseCode(t, w) != "outcome_unknown" || strings.Contains(w.Body.String(), "private credential") {
				t.Fatal(w.Body)
			}
		}
		if calls != 1 {
			t.Fatalf("dispatched %d times", calls)
		}
	}
}
func TestRateLimitReadDeadlineAndAdmission(t *testing.T) {
	h := newHandler(context.Background(), &fakeBackend{history: func(context.Context, string, string, PageRequest) (Result, error) {
		return Result{}, context.DeadlineExceeded
	}}, "instance")
	w := apiRequest(h, "GET", "/v1/workspaces/T1/conversations/C1/messages", "")
	if w.Code != 504 {
		t.Fatal(w.Body)
	}
	for range 2 {
		if !h.admit("T1") {
			t.Fatal("admission failed")
		}
	}
	w = apiRequest(h, "GET", "/v1/workspaces/T1/conversations/C1/messages", "")
	if responseCode(t, w) != "local_busy" || w.Header().Get("Retry-After") == "" {
		t.Fatal(w.Body)
	}
	h.release("T1")
	h.release("T1")
	calls := 0
	h.backend = &fakeBackend{send: func(context.Context, string, string, SendRequest) (Message, error) {
		calls++
		if calls == 1 {
			return Message{}, &Error{Status: 429, Code: "slack_rate_limited", RetryAfter: time.Second, Definitive: true}
		}
		return Message{TS: "1.000001"}, nil
	}}
	path := "/v1/workspaces/T1/conversations/C1/messages"
	w = apiRequest(h, "POST", path, `{"text":"hi"}`)
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatal(w.Body)
	}
	_ = apiRequest(h, "POST", path, `{"text":"hi"}`)
	if calls != 1 {
		t.Fatal("retried too soon")
	}
	h.ledger[path+"\x00test-key"].retryAt = time.Now().Add(-time.Second)
	if w = apiRequest(h, "POST", path, `{"text":"hi"}`); w.Code != 201 || calls != 2 {
		t.Fatal(w.Body)
	}
}
func TestInstanceAndBrowserGuards(t *testing.T) {
	h := newHandler(context.Background(), &fakeBackend{}, "instance")
	for _, tc := range []struct{ header, value string }{{"Host", "localhost"}, {"Origin", "http://slk.local"}, {"Origin", ""}, {"X-Slk-Instance", "old-instance"}} {
		r := httptest.NewRequest("POST", "http://slk.local/v1/workspaces/T1/conversations", strings.NewReader(`{"user_ids":["U1"]}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Slk-Instance", "instance")
		r.Header.Set("Idempotency-Key", "key")
		if tc.header == "Host" {
			r.Host = tc.value
		} else {
			r.Header.Set(tc.header, tc.value)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 && w.Code != 409 {
			t.Fatalf("%s: %d %s", tc.header, w.Code, w.Body)
		}
	}
}
func TestLedgerCapacityRejectsBeforeDispatch(t *testing.T) {
	h := newHandler(context.Background(), &fakeBackend{}, "instance")
	for i := range ledgerCapacity {
		h.ledger[string(rune(i))] = &record{created: time.Now()}
	}
	w := apiRequest(h, "POST", "/v1/workspaces/T1/conversations/C1/messages", `{"text":"hi"}`)
	if responseCode(t, w) != "idempotency_capacity" {
		t.Fatal(w.Body)
	}
}

func TestLedgerExpirationRetainsUnknownOutcomes(t *testing.T) {
	calls := 0
	h := newHandler(context.Background(), &fakeBackend{send: func(context.Context, string, string, SendRequest) (Message, error) {
		calls++
		return Message{TS: "1.000001"}, nil
	}}, "instance")
	path := "/v1/workspaces/T1/conversations/C1/messages"
	body := `{"text":"hi"}`
	if w := apiRequest(h, "POST", path, body); w.Code != 201 {
		t.Fatal(w.Body)
	}
	h.ledger[path+"\x00test-key"].created = time.Now().Add(-retention - time.Second)
	if w := apiRequest(h, "POST", path, body); w.Code != 201 || calls != 2 {
		t.Fatalf("expired outcome was retained: %s; calls %d", w.Body, calls)
	}
	h.backend = &fakeBackend{send: func(context.Context, string, string, SendRequest) (Message, error) {
		calls++
		return Message{}, context.DeadlineExceeded
	}}
	h.ledger = make(map[string]*record)
	if w := apiRequest(h, "POST", path, body); responseCode(t, w) != "outcome_unknown" {
		t.Fatal(w.Body)
	}
	h.ledger[path+"\x00test-key"].created = time.Now().Add(-retention - time.Second)
	if w := apiRequest(h, "POST", path, body); responseCode(t, w) != "outcome_unknown" || calls != 3 {
		t.Fatalf("unknown outcome was re-dispatched: %s; calls %d", w.Body, calls)
	}
}

func TestQueryValidation(t *testing.T) {
	h := newHandler(context.Background(), &fakeBackend{}, "instance")
	for _, path := range []string{
		"/v1/workspaces/T1/users/U1?cursor=ignored",
		"/v1/workspaces/T1/conversations?oldest=1.000001",
		"/v1/workspaces/T1/search?q=%20%20",
		"/v1/workspaces/T1/search?q=a&q=b",
		"/v1/workspaces/T1/conversations/C1/messages?limit=0",
		"/v1/workspaces/T1/conversations/C1/messages?limit=101",
		"/v1/workspaces/T1/conversations/C1/messages?oldest=2.000001&latest=1.000001",
		"/v1/workspaces/T1/conversations/C1/messages?oldest=1.000001&latest=01.000001",
		"/v1/workspaces/T1/conversations/C1/messages?oldest=broken",
	} {
		if w := apiRequest(h, "GET", path, ""); w.Code != 400 {
			t.Fatalf("accepted %s: %d %s", path, w.Code, w.Body)
		}
	}
	// Compare fractions without parsing through float64 or losing precision.
	if w := apiRequest(h, "GET", "/v1/workspaces/T1/conversations/C1/messages?oldest=10000000000.000001&latest=10000000000.000002", ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
}
