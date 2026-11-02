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

type readBoardInput struct {
	ID    string `json:"id" jsonschema:"The board's ID"`
	Since *int64 `json:"since,omitempty" jsonschema:"Only elements changed after this board version, tombstones included. Without it, the live elements"`
}

type createBoardInput struct {
	Title string `json:"title" jsonschema:"The board's title"`
	Group string `json:"group,omitempty" jsonschema:"The group it belongs to, if any"`
}

type updateBoardInput struct {
	ID       string           `json:"id" jsonschema:"The board's ID"`
	Elements []map[string]any `json:"elements" jsonschema:"Excalidraw-style elements: id, type, x, y, width, height, text, version and so on"`
}

type dashboardCueInput struct {
	Tab    string `json:"tab,omitempty" jsonschema:"The dashboard tab to open, such as groups or processes"`
	Path   string `json:"path,omitempty" jsonschema:"A dashboard path to open, such as /processes/templates"`
	Target string `json:"target,omitempty" jsonschema:"The visible text of the control to highlight"`
	Note   string `json:"note,omitempty" jsonschema:"A short note shown beside it"`
}

type setTaskLinkInput struct {
	Agent string `json:"agent" jsonschema:"The agent's name or ID"`
	URL   string `json:"url,omitempty" jsonschema:"The http(s) link. Leave it out to clear the link"`
	Label string `json:"label,omitempty" jsonschema:"Short text to show in place of the link"`
}

type spawnAgentInput struct {
	Group          string `json:"group" jsonschema:"The group to start it in, one the human chose"`
	Name           string `json:"name,omitempty" jsonschema:"A name for the worker"`
	Profile        string `json:"profile,omitempty" jsonschema:"The spawn profile. Leave it out for the one the human chose"`
	InitialMessage string `json:"initial_message,omitempty" jsonschema:"What the worker should do"`
	TaskURL        string `json:"task_url,omitempty" jsonschema:"An http(s) link to the work"`
	TaskLabel      string `json:"task_label,omitempty" jsonschema:"Short text to show in place of the link"`
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
	case stewardtools.ListBoards:
		mcp.AddTool(server, def, get[noInput](c, func(noInput) string { return "/v1/boards" }))
	case stewardtools.ReadBoard:
		mcp.AddTool(server, def, get[readBoardInput](c, boardPath))
	case stewardtools.CreateBoard:
		mcp.AddTool(server, def, post(c, func(in createBoardInput) (string, any) {
			return "/v1/boards", map[string]any{"title": in.Title, "group": in.Group}
		}))
	case stewardtools.UpdateBoard:
		mcp.AddTool(server, def, send(c, http.MethodPatch, func(in updateBoardInput) (string, any) {
			return "/v1/boards/" + url.PathEscape(in.ID), map[string]any{"elements": in.Elements}
		}))
	case stewardtools.DashboardView:
		mcp.AddTool(server, def, get[noInput](c, func(noInput) string { return "/v1/dashboard/view" }))
	case stewardtools.DashboardCue:
		mcp.AddTool(server, def, post(c, func(in dashboardCueInput) (string, any) {
			return "/v1/dashboard/cue", in
		}))
	case stewardtools.DashboardCapture:
		mcp.AddTool(server, def, c.dashboardCapture)
	case stewardtools.SpawnAgent:
		mcp.AddTool(server, def, post(c, func(in spawnAgentInput) (string, any) {
			// A key that's present counts as chosen, even empty, and would wipe the
			// profile's own name and first message.
			body := map[string]any{}
			for key, value := range map[string]string{
				"name": in.Name, "profile": in.Profile, "initial_message": in.InitialMessage,
				"task_ref_url": in.TaskURL, "task_ref_label": in.TaskLabel,
			} {
				if value != "" {
					body[key] = value
				}
			}
			return "/v1/groups/" + url.PathEscape(in.Group) + "/spawn", body
		}))
	case stewardtools.SetTaskLink:
		mcp.AddTool(server, def, post(c, func(in setTaskLinkInput) (string, any) {
			return "/v1/agent/" + url.PathEscape(in.Agent) + "/task", map[string]any{"url": in.URL, "label": in.Label}
		}))
	default:
		panic("stewardtools lists a tool with no handler: " + tool.Name)
	}
}

func boardPath(in readBoardInput) string {
	path := "/v1/boards/" + url.PathEscape(in.ID)
	if in.Since != nil {
		path += "?since=" + strconv.FormatInt(*in.Since, 10)
	}
	return path
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
	return send(c, http.MethodPost, request)
}

func send[In any](c *Client, method string, request func(In) (string, any)) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		path, body := request(in)
		raw, err := c.call(ctx, method, path, body)
		if err != nil {
			return nil, nil, err
		}
		return textResult(raw), nil, nil
	}
}

// dashboardCapture hands the screen over as an image, the form an assistant can
// look at. A form's text sent apart from the image is a separate grant, and this
// tool never asks for it. The image still shows what is typed on screen.
func (c *Client) dashboardCapture(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
	raw, err := c.call(ctx, http.MethodPost, "/v1/dashboard/capture", map[string]any{})
	if err != nil {
		return nil, nil, err
	}
	var capture struct {
		PNG     []byte          `json:"png"`
		Context json.RawMessage `json:"context"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		return nil, nil, fmt.Errorf("read the capture: %w", err)
	}
	content := []mcp.Content{&mcp.ImageContent{Data: capture.PNG, MIMEType: "image/png"}}
	if len(capture.Context) > 0 {
		content = append(content, &mcp.TextContent{Text: string(capture.Context)})
	}
	return &mcp.CallToolResult{Content: content}, nil, nil
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
