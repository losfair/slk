package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gammons/slk/internal/localapi"
	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/slack-go/slack"
)

func TestLocalAPIWorkspaceRoutingAndWireContract(t *testing.T) {
	fake := newFakeSlack(t, map[string]string{
		"/api/auth.test":             `{"ok":true,"team_id":"T1","team":"one","user_id":"Uself","url":"https://one.slack.com/"}`,
		"/api/users.conversations":   `{"ok":true,"channels":[{"id":"C1","name":"general","is_member":true}],"response_metadata":{"next_cursor":"conversations-next"}}`,
		"/api/conversations.info":    `{"ok":true,"channel":{"id":"C1","name":"general","is_member":true}}`,
		"/api/conversations.history": `{"ok":true,"messages":[{"ts":"2.000001","text":"hello","user":"U1","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"body"}}],"files":[{"id":"F1","title":"report","url_private":"secret","permalink":"https://one.slack.com/files/F1"}]}],"has_more":true,"response_metadata":{"next_cursor":"history-next"}}`,
		"/api/conversations.replies": `{"ok":true,"messages":[{"ts":"1.000001","text":"parent"},{"ts":"2.000001","thread_ts":"1.000001","text":"reply"}],"has_more":true,"response_metadata":{"next_cursor":"replies-next"}}`,
		"/api/search.messages":       `{"ok":true,"messages":{"matches":[{"ts":"2.000001","text":"match","user":"U1","channel":{"id":"C1"},"permalink":"https://one.slack.com/archives/C1/p2000001?thread_ts=1.000001"}],"paging":{"page":2,"pages":3},"total":50}}`,
		"/api/users.info":            `{"ok":true,"user":{"id":"U1","real_name":"Alice","profile":{"display_name":"alice","email":"private@example.com"}}}`,
		"/api/conversations.open":    `{"ok":true,"channel":{"id":"D1"},"already_open":true}`,
		"/api/chat.postMessage":      `{"ok":true,"ts":"3.000001","channel":"C1"}`,
	})
	client := newTestClient(t, fake.Server)
	if err := client.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	router := newWorkspaceRouter()
	workspace := &WorkspaceContext{TeamID: "T1", TeamName: "one", Client: client}
	router.Add(workspace)
	// Every API operation targets T1 even while the UI points at another workspace.
	router.Set(&WorkspaceContext{TeamID: "T2"})
	b := newLocalAPIBackend(router, []slackclient.Token{{TeamID: "T1", TeamName: "one"}})
	b.setWorkspace("T1", "one", "ready")
	page := localapi.PageRequest{Limit: 20, Cursor: "upstream", Oldest: "1.000001", Latest: "9.000001"}
	conversations, err := b.Conversations(context.Background(), "T1", page)
	if err != nil || conversations.Cursor != "conversations-next" {
		t.Fatalf("%+v %v", conversations, err)
	}
	form := fake.requestTo(t, "/api/users.conversations").form
	if form.Get("team_id") != "T1" || form.Get("cursor") != "upstream" || form.Get("limit") != "20" {
		t.Fatal(form)
	}
	c, err := b.Conversation(context.Background(), "T1", "C1")
	if err != nil || c.ID != "C1" {
		t.Fatalf("%+v %v", c, err)
	}
	history, err := b.History(context.Background(), "T1", "C1", page)
	if err != nil || history.Cursor != "history-next" {
		t.Fatalf("%+v %v", history, err)
	}
	msgs := history.Data.([]localapi.Message)
	if len(msgs) != 1 || !msgs[0].HasBlocks || len(msgs[0].Files) != 1 || msgs[0].Files[0].ID != "F1" {
		t.Fatal(msgs)
	}
	form = fake.requestTo(t, "/api/conversations.history").form
	if form.Get("oldest") != "1.000001" || form.Get("latest") != "9.000001" || form.Get("inclusive") == "true" {
		t.Fatal(form)
	}
	replies, err := b.Replies(context.Background(), "T1", "C1", "1.000001", page)
	if err != nil || replies.Parent == nil || replies.Parent.TS != "1.000001" || len(replies.Data.([]localapi.Message)) != 1 || replies.Cursor != "replies-next" {
		t.Fatalf("%+v %v", replies, err)
	}
	search, err := b.Search(context.Background(), "T1", localapi.PageRequest{Limit: 20, Cursor: "2", Query: "in:general hello"})
	if err != nil || search.Cursor != "3" {
		t.Fatalf("%+v %v", search, err)
	}
	form = fake.requestTo(t, "/api/search.messages").form
	if form.Get("page") != "2" || form.Get("query") != "in:general hello" || form.Get("team_id") != "T1" {
		t.Fatal(form)
	}
	user, err := b.User(context.Background(), "T1", "U1")
	if err != nil || user.DisplayName != "alice" {
		t.Fatalf("%+v %v", user, err)
	}
	opened, err := b.Open(context.Background(), "T1", localapi.OpenRequest{UserIDs: []string{"U1"}})
	if err != nil || opened.ChannelID != "D1" || !opened.AlreadyOpen {
		t.Fatalf("%+v %v", opened, err)
	}
	sent, err := b.Send(context.Background(), "T1", "C1", localapi.SendRequest{Text: "**hello**", ThreadTS: "1.000001", Broadcast: true})
	if err != nil || sent.TS != "3.000001" || sent.Text != "*hello*" || sent.UserID == nil || *sent.UserID != "Uself" {
		t.Fatalf("%+v %v", sent, err)
	}
	form = fake.requestTo(t, "/api/chat.postMessage").form
	if form.Get("thread_ts") != "1.000001" || form.Get("reply_broadcast") != "true" {
		t.Fatal(form)
	}
	if router.Active().TeamID != "T2" {
		t.Fatal("API switched UI workspace")
	}
}
func TestLocalAPIRegistryConcurrentOwnership(t *testing.T) {
	router := newWorkspaceRouter()
	b := newLocalAPIBackend(router, []slackclient.Token{{TeamID: "T1", TeamName: "one"}})
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			b.setWorkspace("T1", "one", "ready")
		}
	})
	wg.Go(func() {
		for range 100 {
			values := b.Workspaces()
			values[0].Name = "caller owns this copy"
			router.Set(&WorkspaceContext{TeamID: "T2"})
		}
	})
	wg.Wait()
	if got := b.Workspaces(); got[0].Name != "one" {
		t.Fatal(got)
	}
	_, err := b.client("T1")
	var e *localapi.Error
	if !errors.As(err, &e) || e.Code != "workspace_unavailable" || !e.Definitive {
		t.Fatal(err)
	}
	_, err = b.client("unknown")
	if !errors.As(err, &e) || e.Status != 404 {
		t.Fatal(err)
	}
}
func TestLocalAPIErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		err    error
		code   string
		status int
	}{
		{&slack.RateLimitedError{RetryAfter: 3 * time.Second}, "slack_rate_limited", 429},
		{slack.SlackErrorResponse{Err: "invalid_auth"}, "workspace_auth_required", 503},
		{slack.SlackErrorResponse{Err: "channel_not_found"}, "not_found", 404},
		{slack.SlackErrorResponse{Err: "missing_scope"}, "slack_access_refused", 403},
		{slack.SlackErrorResponse{Err: "invalid_arguments"}, "slack_rejected", 502},
	} {
		err := localAPIError(fmt.Errorf("wrapped: %w", tc.err))
		var e *localapi.Error
		if !errors.As(err, &e) || e.Status != tc.status || e.Code != tc.code || !e.Definitive {
			t.Fatalf("%v -> %+v", tc.err, e)
		}
	}
	transport := errors.New("private transport details")
	if localAPIError(transport) != transport {
		t.Fatal("transport failure lost uncertainty")
	}
}
