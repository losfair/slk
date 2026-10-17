package main

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/gammons/slk/internal/localapi"
	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/gammons/slk/internal/slackurl"
	"github.com/slack-go/slack"
)

// localAPIBackend never reads UI state or mutable workspace conversation slices.
// Registry metadata is copied at the connection lifecycle boundaries.
type localAPIBackend struct {
	router     *workspaceRouter
	mu         sync.RWMutex
	order      []string
	workspaces map[string]localapi.Workspace
}

func newLocalAPIBackend(router *workspaceRouter, tokens []slackclient.Token) *localAPIBackend {
	b := &localAPIBackend{router: router, workspaces: make(map[string]localapi.Workspace)}
	for _, tok := range tokens {
		if _, exists := b.workspaces[tok.TeamID]; !exists {
			b.order = append(b.order, tok.TeamID)
		}
		b.workspaces[tok.TeamID] = localapi.Workspace{ID: tok.TeamID, Name: tok.TeamName, State: "connecting"}
	}
	return b
}
func (b *localAPIBackend) setWorkspace(id, name, state string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.workspaces[id]; !exists {
		b.order = append(b.order, id)
	}
	b.workspaces[id] = localapi.Workspace{ID: id, Name: name, State: state}
}
func (b *localAPIBackend) Workspaces() []localapi.Workspace {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]localapi.Workspace, 0, len(b.order))
	for _, id := range b.order {
		result = append(result, b.workspaces[id])
	}
	return result
}
func (b *localAPIBackend) client(team string) (*slackclient.Client, error) {
	w := b.router.ByID(team)
	if w != nil && w.Client != nil {
		return w.Client, nil
	}
	b.mu.RLock()
	_, known := b.workspaces[team]
	b.mu.RUnlock()
	if !known {
		return nil, &localapi.Error{Status: 404, Code: "not_found", Message: "Workspace not found", Definitive: true}
	}
	return nil, &localapi.Error{Status: 503, Code: "workspace_unavailable", Message: "Workspace is not ready", Retryable: true, Definitive: true}
}
func localAPIError(err error) error {
	if err == nil {
		return nil
	}
	var rl *slack.RateLimitedError
	if errors.As(err, &rl) {
		return &localapi.Error{Status: 429, Code: "slack_rate_limited", Message: "Slack rate limit exceeded", Retryable: true, RetryAfter: max(time.Second, rl.RetryAfter), Definitive: true}
	}
	var upstream slack.SlackErrorResponse
	if errors.As(err, &upstream) {
		e := &localapi.Error{Status: 502, Code: "slack_rejected", Message: "Slack refused the request", Definitive: true}
		switch upstream.Err {
		case "invalid_auth", "not_authed", "token_revoked", "account_inactive", "token_expired":
			e.Status = 503
			e.Code = "workspace_auth_required"
			e.Message = "Workspace sign-in is required"
		case "channel_not_found", "message_not_found", "thread_not_found", "user_not_found":
			e.Status = 404
			e.Code = "not_found"
			e.Message = "Resource not found or inaccessible"
		case "not_in_channel", "missing_scope", "no_permission", "restricted_action", "is_archived", "enterprise_is_restricted":
			e.Status = 403
			e.Code = "slack_access_refused"
			e.Message = "Slack access restrictions prevent this operation"
		}
		return e
	}
	return err // The HTTP layer sanitizes transport failures; writes become unknown.
}
func apiConversation(c slack.Channel) localapi.Conversation {
	return localapi.Conversation{ID: c.ID, Name: c.Name, IsIM: c.IsIM, IsMPIM: c.IsMpIM, IsPrivate: c.IsPrivate, IsMember: c.IsMember}
}
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func apiMessage(team, channel string, m slack.Message) localapi.Message {
	files := make([]localapi.File, 0, len(m.Files))
	for _, f := range m.Files {
		files = append(files, localapi.File{ID: f.ID, Title: f.Title, Permalink: f.Permalink})
	}
	return localapi.Message{TeamID: team, ChannelID: channel, TS: m.Timestamp, ThreadTS: nullable(m.ThreadTimestamp), UserID: nullable(m.User), BotID: nullable(m.BotID),
		Text: m.Text, TextFormat: "slack_mrkdwn", Subtype: m.SubType, ReplyCount: m.ReplyCount, Files: files, HasBlocks: len(m.Blocks.BlockSet) > 0, HasAttachments: len(m.Attachments) > 0}
}
func apiMessages(team, channel string, msgs []slack.Message) []localapi.Message {
	result := make([]localapi.Message, 0, len(msgs))
	for _, msg := range msgs {
		result = append(result, apiMessage(team, channel, msg))
	}
	return result
}
func (b *localAPIBackend) Conversations(ctx context.Context, team string, p localapi.PageRequest) (localapi.Result, error) {
	client, err := b.client(team)
	if err != nil {
		return localapi.Result{}, err
	}
	channels, next, err := client.ConversationsPage(ctx, p.Limit, p.Cursor)
	if err != nil {
		return localapi.Result{}, localAPIError(err)
	}
	result := make([]localapi.Conversation, 0, len(channels))
	for _, c := range channels {
		result = append(result, apiConversation(c))
	}
	return localapi.Result{Data: result, Cursor: next}, nil
}
func (b *localAPIBackend) Conversation(ctx context.Context, team, channel string) (localapi.Conversation, error) {
	client, err := b.client(team)
	if err != nil {
		return localapi.Conversation{}, err
	}
	c, err := client.GetConversationInfo(ctx, channel)
	if err != nil {
		return localapi.Conversation{}, localAPIError(err)
	}
	return apiConversation(*c), nil
}
func (b *localAPIBackend) History(ctx context.Context, team, channel string, p localapi.PageRequest) (localapi.Result, error) {
	client, err := b.client(team)
	if err != nil {
		return localapi.Result{}, err
	}
	page, err := client.HistoryPage(ctx, channel, p.Limit, p.Cursor, p.Oldest, p.Latest)
	if err != nil {
		return localapi.Result{}, localAPIError(err)
	}
	next := page.ResponseMetaData.NextCursor
	return localapi.Result{Data: apiMessages(team, channel, page.Messages), Cursor: next, Truncated: page.HasMore && next == ""}, nil
}
func (b *localAPIBackend) Replies(ctx context.Context, team, channel, thread string, p localapi.PageRequest) (localapi.Result, error) {
	client, err := b.client(team)
	if err != nil {
		return localapi.Result{}, err
	}
	msgs, more, next, err := client.RepliesPage(ctx, channel, thread, p.Limit, p.Cursor)
	if err != nil {
		return localapi.Result{}, localAPIError(err)
	}
	var parent *localapi.Message
	replies := make([]slack.Message, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Timestamp == thread {
			item := apiMessage(team, channel, msg)
			parent = &item
		} else {
			replies = append(replies, msg)
		}
	}
	if parent == nil {
		parents, _, _, err := client.RepliesPage(ctx, channel, thread, 1, "")
		if err != nil {
			return localapi.Result{}, localAPIError(err)
		}
		for _, msg := range parents {
			if msg.Timestamp == thread {
				item := apiMessage(team, channel, msg)
				parent = &item
				break
			}
		}
	}
	return localapi.Result{Data: apiMessages(team, channel, replies), Cursor: next, Parent: parent, Truncated: more && next == ""}, nil
}
func (b *localAPIBackend) Search(ctx context.Context, team string, p localapi.PageRequest) (localapi.Result, error) {
	client, err := b.client(team)
	if err != nil {
		return localapi.Result{}, err
	}
	page := 1
	if p.Cursor != "" {
		page, err = strconv.Atoi(p.Cursor)
		if err != nil || page < 1 || page > 100 {
			return localapi.Result{}, &localapi.Error{Status: 400, Code: "invalid_cursor", Message: "Invalid search position", Definitive: true}
		}
	}
	result, err := client.SearchMessagesPage(ctx, p.Query, p.Limit, page)
	if err != nil {
		return localapi.Result{}, localAPIError(err)
	}
	messages := make([]localapi.Message, 0, len(result.Matches))
	for _, match := range result.Matches {
		item := apiMessage(team, match.Channel.ID, slack.Message{Msg: slack.Msg{Timestamp: match.Timestamp, User: match.User, Text: match.Text, Blocks: match.Blocks, Attachments: match.Attachments}})
		if link, ok := slackurl.Parse(match.Permalink); ok {
			item.ThreadTS = nullable(string(link.ThreadTS))
		}
		messages = append(messages, item)
	}
	pages := max(result.Pages, result.PageCount)
	// Some responses expose total but not page count.
	if pages == 0 && result.Total > 0 {
		pages = (result.Total + p.Limit - 1) / p.Limit
	}
	next := ""
	if page < pages && page < 100 {
		next = strconv.Itoa(page + 1)
	}
	return localapi.Result{Data: messages, Cursor: next, Truncated: pages > 100 && page >= 100}, nil
}
func (b *localAPIBackend) User(ctx context.Context, team, user string) (localapi.User, error) {
	client, err := b.client(team)
	if err != nil {
		return localapi.User{}, err
	}
	u, err := client.GetUserProfileContext(ctx, user)
	if err != nil {
		return localapi.User{}, localAPIError(err)
	}
	return localapi.User{ID: u.ID, DisplayName: u.Profile.DisplayName, RealName: u.RealName, IsBot: u.IsBot, Deleted: u.Deleted}, nil
}
func (b *localAPIBackend) Open(ctx context.Context, team string, req localapi.OpenRequest) (localapi.OpenResult, error) {
	client, err := b.client(team)
	if err != nil {
		return localapi.OpenResult{}, err
	}
	channel, already, err := client.OpenConversation(ctx, req.UserIDs)
	if err != nil {
		return localapi.OpenResult{}, localAPIError(err)
	}
	if channel == "" {
		return localapi.OpenResult{}, errors.New("missing Slack acknowledgement")
	}
	return localapi.OpenResult{ChannelID: channel, AlreadyOpen: already}, nil
}
func (b *localAPIBackend) Send(ctx context.Context, team, channel string, req localapi.SendRequest) (localapi.Message, error) {
	client, err := b.client(team)
	if err != nil {
		return localapi.Message{}, err
	}
	var ts, text string
	if req.ThreadTS != "" {
		ts, text, err = client.SendReply(ctx, channel, req.ThreadTS, req.Text, req.Broadcast)
	} else {
		ts, text, err = client.SendMessage(ctx, channel, req.Text)
	}
	if err != nil {
		return localapi.Message{}, localAPIError(err)
	}
	if ts == "" {
		return localapi.Message{}, errors.New("missing Slack acknowledgement")
	}
	return apiMessage(team, channel, slack.Message{Msg: slack.Msg{Timestamp: ts, Text: text, User: client.UserID(), ThreadTimestamp: req.ThreadTS}}), nil
}
