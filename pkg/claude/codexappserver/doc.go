// Package codexappserver implements tclaude's narrow control client for the
// Codex app-server protocol.
//
// The validated compatibility window is codex-cli >=0.147.0,<0.148.0. The
// package uses the stable 0.147 protocol methods needed by the first control
// slice and deliberately initializes without experimentalApi. Its types are
// small, hand-written projections of the generated 0.147.0 schema: required
// control fields are typed and additive fields remain tolerated.
//
// Transport is WebSocket over a local Unix socket. One Client owns one
// connection and is safe for concurrent calls. It does not reconnect. A
// timeout or cancellation after a request is written is explicitly ambiguous;
// callers must reconcile through thread/read before retrying a mutation.
//
// M1 never answers server-initiated approval or user-input requests. If Codex
// unexpectedly routes one to this control connection, the Client publishes the
// decoded request, quarantines the handle, and closes it with
// ErrUnexpectedServerRequest. The TUI remains the sole interaction owner.
package codexappserver
