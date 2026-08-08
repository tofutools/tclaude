package copilotapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Method names for the surface this package models. Copilot exposes roughly
// 250; anything not listed here is still reachable through [Client.Call].
const (
	MethodConnect            = "connect"
	MethodPing               = "ping"
	MethodSessionCreate      = "session.create"
	MethodSessionSetFg       = "session.setForeground"
	MethodSessionGetFg       = "session.getForeground"
	MethodSessionSend        = "session.send"
	MethodSessionNameSet     = "session.name.set"
	MethodSessionCompact     = "session.history.compact"
	MethodSessionContextInfo = "session.metadata.contextInfo"
	MethodSessionUsage       = "session.usage.getMetrics"
)

// NewSessionID returns a fresh session identifier. Copilot lets the client
// choose the UUID and echoes it back from `session.create`.
func NewSessionID() string { return uuid.NewString() }

// Ping checks that the server is responsive. The reply echoes message,
// prefixed with "pong: ".
func (c *Client) Ping(ctx context.Context, message string) (PingResult, error) {
	var result PingResult
	err := c.Call(ctx, MethodPing, PingParams{Message: message}, &result)
	return result, err
}

// CreateSession creates a session that this client can drive.
//
// This is the only way to get a drivable session, and the alternatives fail
// in ways that look like success:
//
//   - The pane's own startup session is visible through
//     [Client.GetForegroundSession] but rejects every `session.*` call.
//   - `sessions.open` with `{kind: "create"}` — the create path the schema
//     documents — really does create a session and answers
//     `{"status":"created"}`, but that session is never registered with the
//     RPC session registry, so it cannot be named, foregrounded or driven.
//   - `sessions.open` with `{kind: "attach"}` answers `{"status":"resumed"}`
//     for a session that stays undrivable, and `{"status":"not_found"}` — a
//     successful JSON-RPC result — for one that does not exist.
//
// See the package docs for the full account. In each case the error surfaces
// later, on an unrelated call, so preferring this method is what keeps the
// failure where it belongs.
//
// params.SessionID may be left empty, in which case one is generated.
func (c *Client) CreateSession(ctx context.Context, params CreateSessionParams) (SessionInfo, error) {
	if params.SessionID == "" {
		params.SessionID = NewSessionID()
	}
	var info SessionInfo
	if err := c.Call(ctx, MethodSessionCreate, params, &info); err != nil {
		return SessionInfo{}, err
	}
	if info.SessionID == "" {
		return SessionInfo{}, errors.New("copilotapi: session.create returned no sessionId")
	}
	if info.SessionID != params.SessionID {
		// The SDK treats this as fatal too. Continuing would leave us driving
		// a different session than the one we recorded.
		return SessionInfo{}, fmt.Errorf("copilotapi: session.create returned session %s but %s was requested",
			info.SessionID, params.SessionID)
	}
	return info, nil
}

// SetForegroundSession asks the TUI to display sessionID, backgrounding
// whatever it was showing.
//
// The server reports refusal in the result rather than as a JSON-RPC error,
// which this converts into one.
func (c *Client) SetForegroundSession(ctx context.Context, sessionID string) error {
	var result SetForegroundResult
	if err := c.Call(ctx, MethodSessionSetFg, map[string]string{"sessionId": sessionID}, &result); err != nil {
		return err
	}
	if !result.Success {
		detail := result.Error
		if detail == "" {
			detail = "server reported failure without a reason"
		}
		return fmt.Errorf("copilotapi: set foreground session %s: %s", sessionID, detail)
	}
	return nil
}

// GetForegroundSession reports the session the TUI is currently displaying.
//
// Before we have created and foregrounded our own session this is the pane's
// startup session, which is not drivable: every `session.*` call against it
// fails with an error [IsSessionNotFound] recognises.
//
// When nothing is foregrounded the server answers with an empty object rather
// than an error, which is returned here as [ErrNoForegroundSession]. Reporting
// it as success would hand back a blank session ID that only fails later, as a
// confusing "Session not found" from whatever call used it.
func (c *Client) GetForegroundSession(ctx context.Context) (SessionInfo, error) {
	var info SessionInfo
	if err := c.Call(ctx, MethodSessionGetFg, struct{}{}, &info); err != nil {
		return SessionInfo{}, err
	}
	if info.SessionID == "" {
		return SessionInfo{}, ErrNoForegroundSession
	}
	return info, nil
}

// Send delivers a user message to a session and returns its message ID. It
// does not wait for the agent to answer; watch the event stream for that.
func (c *Client) Send(ctx context.Context, params SendParams) (string, error) {
	var result SendResult
	if err := c.Call(ctx, MethodSessionSend, params, &result); err != nil {
		return "", err
	}
	return result.MessageID, nil
}

// SetSessionName sets the session's friendly name, which the TUI shows as its
// title. The server requires 1–100 characters after trimming.
func (c *Client) SetSessionName(ctx context.Context, sessionID, name string) error {
	return c.Call(ctx, MethodSessionNameSet, SetNameParams{SessionID: sessionID, Name: name}, nil)
}

// Compact summarizes the session's history to reclaim context window, which is
// what Copilot's own `/compact` command does.
//
// It is SYNCHRONOUS: the call runs a summarization turn on the model and only
// returns once the new history is in place, so it takes as long as a short turn
// rather than milliseconds. Callers on a request path must give it its own
// budget and must not hold a client connection open waiting for it.
//
// A session with nothing worth summarizing is an ERROR, not an empty success —
// the server answers "Nothing to compact" as a generic InternalError. That is
// an ordinary outcome for an agent that has barely started, so callers should
// separate it from a real failure with [IsNothingToCompact] rather than
// reporting it as a broken channel.
func (c *Client) Compact(ctx context.Context, params CompactParams) (CompactResult, error) {
	var result CompactResult
	err := c.Call(ctx, MethodSessionCompact, params, &result)
	return result, err
}

// ContextInfo reports the session's context-window token breakdown.
//
// It returns a nil ContextInfo without an error when the session has not
// cached a system prompt and tool metadata yet, which is the normal state
// between `session.create` and the first turn.
func (c *Client) ContextInfo(ctx context.Context, params ContextInfoParams) (*ContextInfo, error) {
	var result contextInfoResult
	if err := c.Call(ctx, MethodSessionContextInfo, params, &result); err != nil {
		return nil, err
	}
	return result.ContextInfo, nil
}

// UsageMetrics reports the session's accumulated token, cost and code-change
// totals.
func (c *Client) UsageMetrics(ctx context.Context, sessionID string) (UsageMetrics, error) {
	var metrics UsageMetrics
	err := c.Call(ctx, MethodSessionUsage, map[string]string{"sessionId": sessionID}, &metrics)
	return metrics, err
}
