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
	MethodSessionResume      = "session.resume"
	MethodSessionSetFg       = "session.setForeground"
	MethodSessionGetFg       = "session.getForeground"
	MethodSessionSend        = "session.send"
	MethodSessionNameSet     = "session.name.set"
	MethodSessionCompact     = "session.history.compact"
	MethodSessionContextInfo = "session.metadata.contextInfo"
	MethodSessionUsage       = "session.usage.getMetrics"

	// The point-in-time state reads. Between them these answer every question
	// a consumer would otherwise be tempted to reconstruct by accumulating
	// events, which is why they are modelled together — see the "state" section
	// of the package docs.
	MethodSessionIsProcessing = "session.metadata.isProcessing"
	MethodSessionActivity     = "session.metadata.activity"
	MethodSessionPermissions  = "session.permissions.pendingRequests"
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

// ResumeSession reloads an EXISTING session's history and returns a session
// this client can drive.
//
// # Why this is not [Client.CreateSession] with a different name
//
// `session.create` at an id that already has history does not attach to it: it
// starts that id FRESH (measured on 1.0.78 — `alreadyInUse:false`, an empty
// `session.getMessages`, and a model with no memory of the previous turn). So
// the difference between the two methods is not convenience, it is whether the
// conversation survives. A caller that means "resume" and sends create silently
// destroys exactly the history it was trying to keep, and the launch still looks
// entirely healthy.
//
// This method's own shape mirrors CreateSession's — same params, same
// [SessionInfo] back — which is what makes the confusion easy and is the reason
// [ResumeSessionParams] is a separate type rather than an alias.
//
// # Failure is not "then create one"
//
// The server answers a session it cannot find with a plain "Session not found"
// error ([IsSessionNotFound]) rather than creating anything, so the two outcomes
// are distinguishable. They must stay that way at the call site too: recovering
// from a failed resume by creating would turn "I could not reach the history"
// into "I replaced the history", which is the worse of the two by a wide margin.
//
// # The echoed id
//
// The server resolves sessionId as a prefix, so the session it resumes need not
// be the one that was named. The check below refuses a reply that disagrees for
// the same reason CreateSession does: continuing would drive a different
// session than the one the caller recorded.
func (c *Client) ResumeSession(ctx context.Context, params ResumeSessionParams) (SessionInfo, error) {
	if params.SessionID == "" {
		return SessionInfo{}, errors.New("copilotapi: session.resume needs a session id to resume")
	}
	var info SessionInfo
	if err := c.Call(ctx, MethodSessionResume, params, &info); err != nil {
		return SessionInfo{}, err
	}
	if info.SessionID == "" {
		return SessionInfo{}, errors.New("copilotapi: session.resume returned no sessionId")
	}
	if info.SessionID != params.SessionID {
		return SessionInfo{}, fmt.Errorf("copilotapi: session.resume returned session %s but %s was requested",
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
//
// A compaction the server declines IN-BAND — a successful JSON-RPC response
// carrying `success: false` — is converted to an error here, the same way
// [Client.SetForegroundSession] handles its own in-band refusal. Reporting it
// as a result would hand the caller a struct whose zero counts read exactly
// like a compaction that ran and removed nothing.
func (c *Client) Compact(ctx context.Context, params CompactParams) (CompactResult, error) {
	var result CompactResult
	if err := c.Call(ctx, MethodSessionCompact, params, &result); err != nil {
		return CompactResult{}, err
	}
	if !result.Success {
		return result, fmt.Errorf(
			"copilotapi: session.history.compact reported failure for session %s",
			params.SessionID)
	}
	return result, nil
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

// IsProcessing reports whether the session is running a turn or a background
// continuation right now.
//
// This is the authoritative answer to "is the agent busy", and it exists here
// because the obvious alternative is wrong in a way that is hard to see. The
// `session.idle` and `assistant.idle` events are both EPHEMERAL — verified
// against a live 1.0.78 server — so they are absent from the persisted event
// log and cannot be replayed after a reconnect. A consumer that inferred
// idleness from the durable log would have to read it off `assistant.turn_end`,
// which also fires mid-loop: one tool-using turn was measured producing three
// turn_start/turn_end pairs and exactly one `session.idle`. This call answers
// the question directly and at any moment, including on a connection that has
// just been established.
func (c *Client) IsProcessing(ctx context.Context, sessionID string) (bool, error) {
	var result IsProcessingResult
	if err := c.Call(ctx, MethodSessionIsProcessing,
		map[string]string{"sessionId": sessionID}, &result); err != nil {
		return false, err
	}
	return result.Processing, nil
}

// Activity reports the session's current activity flags.
//
// HasActiveWork was measured to track [Client.IsProcessing] exactly across a
// turn; Abortable is the additional fact, and it is the one a caller needs
// before deciding whether an interrupt has anything to interrupt.
func (c *Client) Activity(ctx context.Context, sessionID string) (SessionActivity, error) {
	var activity SessionActivity
	err := c.Call(ctx, MethodSessionActivity, map[string]string{"sessionId": sessionID}, &activity)
	return activity, err
}

// PendingPermissionRequests returns the permission prompts that are waiting for
// a human decision.
//
// The server reconstructs these from the session's event history rather than
// from a client's own bookkeeping, so a client that attached late — or
// reconnected — sees prompts raised before it was listening. That is what makes
// "a human must answer this" answerable without tracking
// `permission.requested`/`permission.completed` pairs across a connection that
// may have dropped between them.
//
// Measured: under `--allow-all-tools` this stays empty and no permission event
// is emitted at all, so a consumer mapping it to a waiting-for-human state
// cannot mislabel an unattended agent. Without that flag, a blocked tool call
// appears here and the session stays processing indefinitely with no `Stop`
// hook, which is precisely the state that is otherwise indistinguishable from
// ordinary work.
func (c *Client) PendingPermissionRequests(
	ctx context.Context, sessionID string,
) ([]PendingPermissionRequest, error) {
	var result PendingPermissionRequestList
	if err := c.Call(ctx, MethodSessionPermissions,
		map[string]string{"sessionId": sessionID}, &result); err != nil {
		return nil, err
	}
	return result.Items, nil
}
