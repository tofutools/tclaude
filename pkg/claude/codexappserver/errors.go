package codexappserver

import (
	"encoding/json"
	"errors"
	"fmt"
)

var (
	ErrClosed                  = errors.New("codexappserver: connection closed")
	ErrProtocol                = errors.New("codexappserver: invalid protocol message")
	ErrUnsupportedVersion      = errors.New("codexappserver: unsupported Codex version")
	ErrNotificationOverrun     = errors.New("codexappserver: notification queue overrun")
	ErrUnexpectedServerRequest = errors.New("codexappserver: unexpected server request; M1 must leave approvals and user input to the TUI")
)

// RPCError is an error response from app-server.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("codex app-server error %d: %s", e.Code, e.Message)
}

// CallError records whether a failed call may already have taken effect.
// Ambiguous is true for cancellation, timeout, or connection loss after the
// complete request was written.
type CallError struct {
	Method    string
	Cause     error
	Ambiguous bool
}

func (e *CallError) Error() string {
	suffix := ""
	if e.Ambiguous {
		suffix = "; outcome is ambiguous and must be reconciled before retry"
	}
	return fmt.Sprintf("codexappserver: call %s: %v%s", e.Method, e.Cause, suffix)
}

func (e *CallError) Unwrap() error { return e.Cause }

// UnexpectedServerRequestError is the terminal error retained by a client
// that received a request it is forbidden to answer.
type UnexpectedServerRequestError struct {
	Request ServerRequest
}

func (e *UnexpectedServerRequestError) Error() string {
	return fmt.Sprintf("%v: %s (id %s); keep the TUI subscribed and reconnect a fresh control handle",
		ErrUnexpectedServerRequest, e.Request.Method, string(e.Request.ID))
}

func (e *UnexpectedServerRequestError) Unwrap() error { return ErrUnexpectedServerRequest }
