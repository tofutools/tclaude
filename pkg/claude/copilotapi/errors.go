package copilotapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// JSON-RPC 2.0 error codes we distinguish. Copilot folds nearly every handler
// failure into InternalError, so the code alone rarely identifies a cause.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Error is a JSON-RPC error object returned by the server.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("copilot rpc error %d: %s", e.Code, e.Message)
}

// ErrClosed reports use of a connection that has been closed, either by
// [Client.Close] or because the server went away.
var ErrClosed = errors.New("copilotapi: connection closed")

// ErrProtocolVersion reports a server whose protocol version differs from
// [SupportedProtocolVersion].
var ErrProtocolVersion = errors.New("copilotapi: unsupported protocol version")

// ErrNoForegroundSession reports that the TUI is not displaying any session.
// It is an expected state rather than a fault, but it is returned as an error
// so it cannot be mistaken for a session with a blank ID.
var ErrNoForegroundSession = errors.New("copilotapi: no foreground session")

// ErrSubscriptionOverrun reports a subscription whose buffer filled because
// the consumer fell behind. The subscription is closed and its channel drained
// no further: a consumer that silently skipped events would misreport agent
// state, so the loss is surfaced instead. Recover by re-subscribing and
// re-reading authoritative state (context info, usage metrics).
var ErrSubscriptionOverrun = errors.New("copilotapi: subscription overrun, events dropped")

// IsSessionNotFound reports whether err is the server's "Session not found"
// failure.
//
// This matches on message text, which is unpleasant but unavoidable: the
// server reports it as a generic InternalError with the detail only in the
// message, so there is no code or data field to key off. It is worth
// distinguishing because it is the expected outcome of the two traps described
// in the package docs — driving the TUI's own startup session, or trusting
// `sessions.open` — rather than a transport problem.
func IsSessionNotFound(err error) bool {
	var rpcErr *Error
	if !errors.As(err, &rpcErr) {
		return false
	}
	return strings.Contains(rpcErr.Message, "Session not found")
}
