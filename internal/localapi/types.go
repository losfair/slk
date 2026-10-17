// Package localapi serves slk's opt-in local HTTP API over a private Unix socket.
// It owns the wire contract; cmd/slk supplies workspace-scoped operations.
package localapi

import (
	"context"
	"time"
)

const (
	operationTimeout = 30 * time.Second
	retention        = 24 * time.Hour
	ledgerCapacity   = 10_000
)

type Workspace struct {
	ID    string `json:"team_id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type Conversation struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsIM      bool   `json:"is_im"`
	IsMPIM    bool   `json:"is_mpim"`
	IsPrivate bool   `json:"is_private"`
	IsMember  bool   `json:"is_member"`
}

type File struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Permalink string `json:"permalink,omitempty"`
}

type Message struct {
	TeamID         string  `json:"team_id"`
	ChannelID      string  `json:"channel_id"`
	TS             string  `json:"ts"`
	ThreadTS       *string `json:"thread_ts"`
	UserID         *string `json:"user_id"`
	BotID          *string `json:"bot_id"`
	Text           string  `json:"text"`
	TextFormat     string  `json:"text_format"`
	Subtype        string  `json:"subtype"`
	ReplyCount     int     `json:"reply_count"`
	Files          []File  `json:"files"`
	HasBlocks      bool    `json:"has_blocks"`
	HasAttachments bool    `json:"has_attachments"`
}

type User struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	RealName    string `json:"real_name"`
	IsBot       bool   `json:"is_bot"`
	Deleted     bool   `json:"deleted"`
}

type OpenRequest struct {
	UserIDs []string `json:"user_ids"`
}
type SendRequest struct {
	Text      string `json:"text"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	Broadcast bool   `json:"broadcast,omitempty"`
}
type OpenResult struct {
	ChannelID   string `json:"channel_id"`
	AlreadyOpen bool   `json:"already_open"`
}

// PageRequest contains a decoded upstream position, never a public cursor.
type PageRequest struct {
	Limit  int
	Cursor string
	Oldest string
	Latest string
	Query  string
}
type Result struct {
	Data      any
	Cursor    string
	Parent    *Message
	Truncated bool
}

// Backend implementations must not select a workspace from UI focus or mutate
// UI-owned state. All returned values belong to the caller.
type Backend interface {
	Workspaces() []Workspace
	Conversations(context.Context, string, PageRequest) (Result, error)
	Conversation(context.Context, string, string) (Conversation, error)
	History(context.Context, string, string, PageRequest) (Result, error)
	Replies(context.Context, string, string, string, PageRequest) (Result, error)
	Search(context.Context, string, PageRequest) (Result, error)
	User(context.Context, string, string) (User, error)
	Open(context.Context, string, OpenRequest) (OpenResult, error)
	Send(context.Context, string, string, SendRequest) (Message, error)
}

// Error is a sanitized failure. Definitive means an upstream write is known to
// have been refused (or was never dispatched), rather than possibly accepted.
type Error struct {
	Status     int           `json:"-"`
	Code       string        `json:"code"`
	Message    string        `json:"message"`
	Retryable  bool          `json:"retryable"`
	RetryAfter time.Duration `json:"-"`
	Definitive bool          `json:"-"`
}

func (e *Error) Error() string { return e.Code }
