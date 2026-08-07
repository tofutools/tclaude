// Package copilotapi is a JSON-RPC client for the embedded server that GitHub
// Copilot CLI exposes in TUI+server mode (`copilot --ui-server --port N`).
//
// In that mode a single Copilot process runs a fully interactive TUI *and*
// listens on a TCP port, so tclaude can drive a Copilot agent through a typed
// API instead of tmux send-keys while the human still sees and uses the pane.
//
// # Transport
//
// LSP-style `Content-Length:`-framed JSON-RPC 2.0 over TCP. Not HTTP. The
// `--stdio` flag exists but only applies to the headless `--server` mode.
//
// # Scope
//
// This package is a client library only. It deliberately knows nothing about
// agentd, spawn, or the harness descriptor, and must stay free of those
// imports so it can be consumed from either side.
//
// Copilot exposes roughly 250 methods. This package models only the narrow
// surface tclaude needs today — see methods.go. Everything else is reachable
// through [Client.Call] without adding a typed wrapper.
//
// # Why these types are hand-written
//
// The npm package ships `schemas/api.schema.json`, which describes itself as
// input for SDK codegen, so generating the types is superficially attractive.
// It is not sufficient: the schema is missing methods the real SDK actually
// calls. `session.create`, `session.setForeground` and `session.getForeground`
// are all absent from the schema's `session` section, yet all three are
// required to drive a session and all three work against a live server.
// Generating from the schema would therefore produce a client that cannot
// create a session at all. `copilot-sdk/index.js` — the reference client — is
// authoritative for what the wire actually carries, and the narrow surface we
// need is small enough to hand-write against it and verify live.
//
// # Traps
//
// The TUI's own startup session is not drivable over RPC.
// [Client.GetForegroundSession] reports its ID, but every `session.*` call
// against it fails with "Session not found": foreground registration wires up
// get/set-foreground only, not the RPC session registry. The working pattern
// is to create our own session ([Client.CreateSession], caller chooses the
// UUID) and then [Client.SetForegroundSession] it, which drops the pane's
// original blank session to the background.
//
// `sessions.open` looks like the way to adopt an existing session and is the
// one the schema documents, but against an unknown session it returns
// `{"status":"not_found"}` as a *successful* result rather than an error. Code
// that trusts it silently proceeds against a session that does not exist and
// only fails later, on every subsequent `session.*` call. Use `session.create`.
//
// # Versioning
//
// [Client.Connect] records the server's protocol version and CLI version. The
// protocol version is checked against [SupportedProtocolVersion] and a
// mismatch fails loudly, because the alternative — running a client built for
// a different contract and misreading its replies — is exactly the silent
// misbehaviour this package exists to avoid. [Options.AllowProtocolMismatch]
// downgrades that to a recorded warning for deliberate experiments.
//
// # Security
//
// TUI+server mode has no authentication: the server accepts any local client
// and applies no per-connection scoping, so any process that can reach the
// port can drive the agent. That is a known, accepted limitation of the mode
// rather than something this client can fix. [Options.Token] sends a
// connection token for the headless `--server` path, which does honour
// COPILOT_CONNECTION_TOKEN.
package copilotapi
