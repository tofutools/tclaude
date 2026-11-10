package mcpserve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultWait = 30 * time.Second
	maxWait     = 50 * time.Second
)

// activityPollInterval spaces out the polls of one wait. Each fleet read builds a
// status snapshot, so this is not tighter. Swapped by tests.
var activityPollInterval = 3 * time.Second

type waitInput struct {
	Cursor         string `json:"cursor,omitempty" jsonschema:"The cursor from the last call. Leave it out to get the current state at once"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"How long to wait, at most 50. Default 30"`
}

type inboxEntry struct {
	ID        int64  `json:"id"`
	From      string `json:"from_short,omitempty"`
	Group     string `json:"group,omitempty"`
	Subject   string `json:"subject,omitempty"`
	Preview   string `json:"preview,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

type attentionEntry struct {
	Kind    string `json:"kind"`
	AgentID string `json:"agent_id,omitempty"`
	Title   string `json:"title,omitempty"`
	Detail  string `json:"detail,omitempty"`
	URL     string `json:"url,omitempty"`
}

type waitResult struct {
	Changed bool `json:"changed"`
	// Cursor goes back in on the next call.
	Cursor string `json:"cursor"`
	// Messages are unread and newer than the cursor.
	Messages []inboxEntry `json:"messages"`
	// AttentionChanged says Attention is the whole current list, which can be empty.
	AttentionChanged bool             `json:"attention_changed"`
	Attention        []attentionEntry `json:"attention,omitempty"`
	// AttentionUnavailable says the fleet could not be read, so only mail is being
	// watched.
	AttentionUnavailable string `json:"attention_unavailable,omitempty"`
}

type activityCursor struct {
	lastMessage int64
	attention   string
}

func (c activityCursor) String() string {
	return strconv.FormatInt(c.lastMessage, 10) + "." + c.attention
}

func parseActivityCursor(raw string) (activityCursor, error) {
	id, attention, ok := strings.Cut(raw, ".")
	last, err := strconv.ParseInt(id, 10, 64)
	if !ok || err != nil {
		return activityCursor{}, errors.New("that cursor did not come from wait_for_activity")
	}
	return activityCursor{lastMessage: last, attention: attention}, nil
}

// The server keeps no state: the cursor holds the newest message seen and a digest
// of what needed attention, so an assistant restart loses nothing.
func (c *Client) waitForActivity(ctx context.Context, _ *mcp.CallToolRequest, in waitInput) (*mcp.CallToolResult, any, error) {
	var since activityCursor
	if in.Cursor != "" {
		var err error
		if since, err = parseActivityCursor(in.Cursor); err != nil {
			return nil, nil, err
		}
	}
	wait := defaultWait
	if in.TimeoutSeconds > 0 {
		wait = min(time.Duration(in.TimeoutSeconds)*time.Second, maxWait)
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	unavailable := ""
	for {
		state, err := c.activitySince(ctx, since)
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return unchanged(since, unavailable)
		case err != nil:
			return nil, nil, err
		case state.Changed || in.Cursor == "":
			state.Changed = state.Changed || in.Cursor == ""
			result, err := jsonResult(state)
			return result, nil, err
		}
		unavailable = state.AttentionUnavailable
		select {
		case <-ctx.Done():
			if parent := context.Cause(ctx); errors.Is(parent, context.DeadlineExceeded) {
				return unchanged(since, unavailable)
			}
			return nil, nil, ctx.Err()
		case <-time.After(activityPollInterval):
		}
	}
}

func unchanged(since activityCursor, unavailable string) (*mcp.CallToolResult, any, error) {
	result, err := jsonResult(waitResult{Cursor: since.String(), Messages: []inboxEntry{}, AttentionUnavailable: unavailable})
	return result, nil, err
}

func (c *Client) activitySince(ctx context.Context, since activityCursor) (*waitResult, error) {
	raw, err := c.call(ctx, http.MethodGet, inboxQuery(true, maxInboxLimit), nil)
	if err != nil {
		return nil, err
	}
	var inbox []inboxEntry
	if err := json.Unmarshal(raw, &inbox); err != nil {
		return nil, fmt.Errorf("read inbox response: %w", err)
	}
	result := &waitResult{Messages: []inboxEntry{}}
	next := since
	for _, message := range inbox {
		if message.ID > since.lastMessage {
			result.Messages = append(result.Messages, message)
			next.lastMessage = max(next.lastMessage, message.ID)
		}
	}
	attention, err := c.fleetAttention(ctx)
	var refusal *DaemonError
	switch {
	case errors.As(err, &refusal) && refusal.Status == http.StatusForbidden:
		result.AttentionUnavailable = refusal.Error()
	case err != nil:
		return nil, err
	default:
		next.attention = attentionDigest(attention)
		if next.attention != since.attention {
			result.AttentionChanged = true
			result.Attention = attention
		}
	}
	result.Changed = len(result.Messages) > 0 || next.attention != since.attention
	result.Cursor = next.String()
	return result, nil
}

func (c *Client) fleetAttention(ctx context.Context) ([]attentionEntry, error) {
	raw, err := c.call(ctx, http.MethodGet, "/v1/fleet", nil)
	if err != nil {
		return nil, err
	}
	var fleet struct {
		Attention []attentionEntry `json:"attention"`
	}
	if err := json.Unmarshal(raw, &fleet); err != nil {
		return nil, fmt.Errorf("read fleet response: %w", err)
	}
	return fleet.Attention, nil
}

// IdleAttentionKind is the one item whose detail runs on its own, "has been idle
// for 3m". Reading it would report a change on every poll.
const IdleAttentionKind = "idle"

// attentionDigest covers each item's detail, because that is where a second
// approval from one agent or a rising unread count shows.
func attentionDigest(items []attentionEntry) string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		detail := item.Detail
		if item.Kind == IdleAttentionKind {
			detail = ""
		}
		keys = append(keys, strings.Join([]string{item.Kind, item.AgentID, item.Title, item.URL, detail}, "\x00"))
	}
	slices.Sort(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\x01")))
	return hex.EncodeToString(sum[:8])
}
