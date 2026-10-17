package localapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type record struct {
	hash    [32]byte
	done    bool
	retryAt time.Time
	created time.Time
	status  int
	body    response
}

func (h *handler) post(w http.ResponseWriter, r *http.Request, p []string, id string) {
	fail := func(e *Error) {
		h.write(w, e.Status, response{Error: e, Meta: metadata{RequestID: id, Source: "local"}})
	}
	if r.URL.RawQuery != "" {
		fail(failure(400, "invalid_query", "Writes do not accept query parameters"))
		return
	}
	if r.Header.Get("X-Slk-Instance") == "" {
		fail(failure(400, "instance_required", "X-Slk-Instance is required"))
		return
	}
	if r.Header.Get("X-Slk-Instance") != h.instance {
		fail(failure(409, "instance_mismatch", "Server instance changed; reconcile earlier writes before retrying"))
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 1 || len(key) > 128 || strings.IndexFunc(key, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		fail(failure(400, "invalid_idempotency_key", "Idempotency-Key must contain 1–128 printable ASCII bytes"))
		return
	}
	var input any
	var run func(context.Context) (any, error)
	status := 200
	switch {
	case len(p) == 4 && p[3] == "conversations":
		var req OpenRequest
		if e := decodeBody(w, r, &req); e != nil {
			fail(e)
			return
		}
		if len(req.UserIDs) < 1 || len(req.UserIDs) > 8 {
			fail(failure(400, "invalid_users", "Expected 1–8 distinct user IDs"))
			return
		}
		seen := map[string]bool{}
		for _, id := range req.UserIDs {
			if !validID(id) || seen[id] {
				fail(failure(400, "invalid_users", "Expected distinct user IDs"))
				return
			}
			seen[id] = true
		}
		input = req
		run = func(ctx context.Context) (any, error) { return h.backend.Open(ctx, p[2], req) }
	case len(p) == 6 && p[3] == "conversations" && validID(p[4]) && p[5] == "messages":
		var req SendRequest
		if e := decodeBody(w, r, &req); e != nil {
			fail(e)
			return
		}
		if strings.TrimSpace(req.Text) == "" || len(req.Text) > 32<<10 || req.ThreadTS != "" && !tsPattern.MatchString(req.ThreadTS) || req.Broadcast && req.ThreadTS == "" {
			fail(failure(400, "invalid_message", "Expected 1–32768 bytes of text and a valid optional thread timestamp"))
			return
		}
		input = req
		status = 201
		run = func(ctx context.Context) (any, error) { return h.backend.Send(ctx, p[2], p[4], req) }
	default:
		fail(failure(404, "not_found", "Route not found"))
		return
	}
	data, _ := json.Marshal(input)
	hash := sha256.Sum256(data)
	ledgerKey := r.URL.Path + "\x00" + key
	h.mu.Lock()
	now := time.Now()
	for key, rec := range h.ledger {
		if rec.done && rec.body.Error != nil && rec.body.Error.Code == "outcome_unknown" {
			continue
		}
		if rec.done && now.Sub(rec.created) > retention {
			delete(h.ledger, key)
		}
	}
	if rec := h.ledger[ledgerKey]; rec != nil {
		if rec.hash != hash {
			h.mu.Unlock()
			fail(failure(409, "idempotency_conflict", "Key was already used with a different request"))
			return
		}
		if !rec.done {
			h.mu.Unlock()
			e := failure(409, "operation_in_progress", "Operation is still running")
			e.RetryAfter = time.Second
			e.Retryable = true
			fail(e)
			return
		}
		if rec.retryAt.IsZero() || now.Before(rec.retryAt) {
			status, body := rec.status, rec.body
			h.mu.Unlock()
			h.write(w, status, body)
			return
		}
	} else if len(h.ledger) >= ledgerCapacity {
		h.mu.Unlock()
		fail(failure(503, "idempotency_capacity", "Idempotency ledger is full"))
		return
	}
	rec := &record{hash: hash, created: now}
	h.ledger[ledgerKey] = rec
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(h.ctx, operationTimeout)
	defer cancel()
	var result any
	var err error
	if ctx.Err() != nil {
		err = failure(503, "shutting_down", "API is shutting down")
	} else {
		result, err = run(ctx)
	}
	body := response{Data: result, Meta: metadata{RequestID: id, Source: "slack"}}
	if err != nil {
		e := asError(err)
		if !e.Definitive {
			e = failure(409, "outcome_unknown", "Slack may have accepted this write; check history before issuing a new key")
		}
		status = e.Status
		body.Data = nil
		body.Error = e
	}
	h.mu.Lock()
	rec.status = status
	rec.body = body
	rec.done = true
	if body.Error != nil && body.Error.Code == "slack_rate_limited" {
		rec.retryAt = time.Now().Add(max(time.Second, body.Error.RetryAfter))
	}
	h.mu.Unlock()
	h.write(w, status, body)
}
