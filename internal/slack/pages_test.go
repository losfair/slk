package slackclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func TestPageAndSendContextsReachTransport(t *testing.T) {
	calls := []struct {
		name string
		call func(context.Context, *Client) error
	}{
		{"conversations", func(ctx context.Context, c *Client) error { _, _, err := c.ConversationsPage(ctx, 20, ""); return err }},
		{"history", func(ctx context.Context, c *Client) error {
			_, err := c.HistoryPage(ctx, "C1", 20, "", "", "")
			return err
		}},
		{"replies", func(ctx context.Context, c *Client) error {
			_, _, _, err := c.RepliesPage(ctx, "C1", "1.000001", 20, "")
			return err
		}},
		{"search", func(ctx context.Context, c *Client) error {
			_, err := c.SearchMessagesPage(ctx, "test", 20, 1)
			return err
		}},
		{"send", func(ctx context.Context, c *Client) error { _, _, err := c.SendMessage(ctx, "C1", "hello"); return err }},
		{"reply", func(ctx context.Context, c *Client) error {
			_, _, err := c.SendReply(ctx, "C1", "1.000001", "hello", false)
			return err
		}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			released := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-r.Context().Done():
				case <-released:
				}
			}))
			defer server.Close()
			defer close(released)
			c := &Client{api: slack.New("test", slack.OptionAPIURL(server.URL+"/"))}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- tc.call(ctx, c) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not start")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("got %v, want cancellation", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("context was ignored")
			}
		})
	}
}
