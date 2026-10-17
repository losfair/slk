package slackclient

import (
	"context"
	"fmt"

	"github.com/slack-go/slack"
)

// ConversationsPage retrieves one page of this account's joined conversations.
func (c *Client) ConversationsPage(ctx context.Context, limit int, cursor string) ([]slack.Channel, string, error) {
	channels, next, err := c.api.GetConversationsForUserContext(ctx, &slack.GetConversationsForUserParameters{
		TeamID: c.teamID, Types: []string{"public_channel", "private_channel", "mpim", "im"},
		Limit: limit, Cursor: cursor, ExcludeArchived: true,
	})
	if err != nil {
		return nil, "", fmt.Errorf("getting conversation page: %w", err)
	}
	return channels, next, nil
}

// HistoryPage preserves Slack's pagination and completeness metadata.
func (c *Client) HistoryPage(ctx context.Context, channelID string, limit int, cursor, oldest, latest string) (*slack.GetConversationHistoryResponse, error) {
	page, err := c.api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
		ChannelID: channelID, Limit: limit, Cursor: cursor, Oldest: oldest, Latest: latest,
	})
	if err != nil {
		return nil, fmt.Errorf("getting history page: %w", err)
	}
	return page, nil
}

// RepliesPage returns a single page, including the parent if Slack supplies it.
func (c *Client) RepliesPage(ctx context.Context, channelID, threadTS string, limit int, cursor string) ([]slack.Message, bool, string, error) {
	msgs, more, next, err := c.api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channelID, Timestamp: threadTS, Limit: limit, Cursor: cursor,
	})
	if err != nil {
		return nil, false, "", fmt.Errorf("getting replies page: %w", err)
	}
	return msgs, more, next, nil
}

func (c *Client) SearchMessagesPage(ctx context.Context, query string, count, page int) (*slack.SearchMessages, error) {
	params := slack.NewSearchParameters()
	params.Count = count
	params.Page = page
	params.TeamID = c.teamID
	result, err := c.api.SearchMessagesContext(ctx, query, params)
	if err != nil {
		return nil, fmt.Errorf("searching messages: %w", err)
	}
	return result, nil
}
