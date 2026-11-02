// Package mcpserve is `tclaude mcp serve`: an MCP stdio server that lets an
// operator's own assistant act as an external steward. It is a thin client of
// the daemon socket with no authorization of its own. The daemon knows the caller
// by its credential, and each route's own gate and audit apply.
package mcpserve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tofutools/tclaude/pkg/claude/common/stewardtools"
)

const (
	defaultInboxLimit = 20
	maxInboxLimit     = 100
)

type noInput struct{}

type listAgentsInput struct {
	Group string `json:"group,omitempty" jsonschema:"Only this group, by name or ID"`
}

type inboxInput struct {
	UnreadOnly bool `json:"unread_only,omitempty" jsonschema:"Only messages not read yet"`
	Limit      int  `json:"limit,omitempty" jsonschema:"How many messages, newest first, at most 100. Default 20"`
}

type readMessageInput struct {
	ID int64 `json:"id" jsonschema:"The message ID"`
}

type sendMessageInput struct {
	To      string `json:"to" jsonschema:"An agent's name or ID, group:<name> for a group, or member@peer on a linked instance"`
	Subject string `json:"subject,omitempty" jsonschema:"One-line subject"`
	Body    string `json:"body" jsonschema:"The message"`
}

type replyInput struct {
	ID      int64  `json:"id" jsonschema:"The ID of the message being answered"`
	Subject string `json:"subject,omitempty" jsonschema:"One-line subject"`
	Body    string `json:"body" jsonschema:"The reply"`
}

type notifyHumanInput struct {
	Subject string `json:"subject,omitempty" jsonschema:"One-line subject"`
	Body    string `json:"body" jsonschema:"The message for the human"`
}

// NewServer returns an MCP server offering exactly stewardtools.List.
func NewServer(client *Client, version string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "tclaude", Version: version}, nil)
	for _, tool := range stewardtools.List() {
		register(server, client, tool)
	}
	return server
}

func register(server *mcp.Server, c *Client, tool stewardtools.Tool) {
	def := &mcp.Tool{
		Name:        tool.Name,
		Description: tool.Description,
		// Reading a message marks it read, so it does not qualify as read-only.
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: !tool.Writes && tool.Name != stewardtools.ReadMessage},
	}
	switch tool.Name {
	case stewardtools.Whoami:
		mcp.AddTool(server, def, get[noInput](c, func(noInput) string { return "/v1/whoami" }))
	case stewardtools.FleetBrief:
		mcp.AddTool(server, def, get[noInput](c, func(noInput) string { return "/v1/fleet" }))
	case stewardtools.ListAgents:
		mcp.AddTool(server, def, get[listAgentsInput](c, peersPath))
	case stewardtools.Inbox:
		mcp.AddTool(server, def, get[inboxInput](c, inboxPath))
	case stewardtools.ReadMessage:
		mcp.AddTool(server, def, get[readMessageInput](c, func(in readMessageInput) string {
			return "/v1/messages/" + strconv.FormatInt(in.ID, 10)
		}))
	case stewardtools.SendMessage:
		mcp.AddTool(server, def, post(c, func(in sendMessageInput) (string, any) {
			return "/v1/messages", map[string]any{"to": in.To, "subject": in.Subject, "body": in.Body}
		}))
	case stewardtools.Reply:
		mcp.AddTool(server, def, post(c, func(in replyInput) (string, any) {
			return fmt.Sprintf("/v1/messages/%d/reply", in.ID), map[string]any{"subject": in.Subject, "body": in.Body}
		}))
	case stewardtools.NotifyHuman:
		mcp.AddTool(server, def, post(c, func(in notifyHumanInput) (string, any) {
			return "/v1/notify-human", map[string]any{"subject": in.Subject, "body": in.Body}
		}))
	case stewardtools.WaitForActivity:
		mcp.AddTool(server, def, c.waitForActivity)
	default:
		panic("stewardtools lists a tool with no handler: " + tool.Name)
	}
}

func peersPath(in listAgentsInput) string {
	if in.Group == "" {
		return "/v1/peers"
	}
	return "/v1/peers?group=" + url.QueryEscape(in.Group)
}

func inboxPath(in inboxInput) string {
	return inboxQuery(in.UnreadOnly, in.Limit)
}

func inboxQuery(unreadOnly bool, limit int) string {
	if limit <= 0 {
		limit = defaultInboxLimit
	}
	query := url.Values{"limit": {strconv.Itoa(min(limit, maxInboxLimit))}}
	if unreadOnly {
		query.Set("unread", "1")
	}
	return "/v1/inbox?" + query.Encode()
}

func get[In any](c *Client, path func(In) string) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		raw, err := c.call(ctx, http.MethodGet, path(in), nil)
		if err != nil {
			return nil, nil, err
		}
		return textResult(raw), nil, nil
	}
}

func post[In any](c *Client, request func(In) (string, any)) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		path, body := request(in)
		raw, err := c.call(ctx, http.MethodPost, path, body)
		if err != nil {
			return nil, nil, err
		}
		return textResult(raw), nil, nil
	}
}

func textResult(raw []byte) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return textResult(raw), nil
}
