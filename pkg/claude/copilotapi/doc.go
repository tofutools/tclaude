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
// # Driving a session
//
// The bootstrap sequence, verified end to end:
//
//	client, _ := copilotapi.DialRetry(ctx, "127.0.0.1:4599", nil)
//	events := client.Subscribe()                    // before creating, so nothing is missed
//	info, _ := client.CreateSession(ctx, copilotapi.CreateSessionParams{
//		WorkingDirectory: dir, ClientName: "tclaude", Streaming: true,
//	})
//	client.SetForegroundSession(ctx, info.SessionID) // the TUI switches to it
//	client.Send(ctx, copilotapi.SendParams{SessionID: info.SessionID, Prompt: "..."})
//
// [Client.Send] is fire-and-forget: it returns a message ID as soon as the
// message is queued, not when the agent has answered. Turn completion is
// observable only on the event stream, as a `session.idle` event.
//
// A single turn produces on the order of thirty events. The order observed
// live, which a consumer can rely on for shape but should not treat as an
// exhaustive list, is: `user.message`, `assistant.turn_start`,
// `model.call_start`, `assistant.message_start`, repeated
// `assistant.streaming_delta` and `assistant.message_delta`,
// `assistant.usage`, `assistant.message`, `assistant.reasoning`,
// `assistant.turn_end`, `session.usage_checkpoint`, `assistant.idle`, and
// finally `session.idle`. Session setup additionally emits `session.start`,
// `session.model_change`, `session.skills_loaded`, `session.tools_updated`,
// `session.mcp_server_status_changed` and `session.title_changed`.
//
// [SessionEvent.Data] is deliberately left raw. The event vocabulary is open
// and Copilot extends it freely, so decoding only the types a consumer
// handles keeps unknown ones from becoming errors.
//
// Reading usage back has two traps worth repeating from the type docs.
// [UsageMetrics.ModelMetrics] nests token counts under
// [ModelMetric.Usage] and request counts under [ModelMetric.Requests]; a
// flattened struct decodes the same payload without error and reports zero
// for everything. And [Client.ContextInfo] legitimately returns nil before a
// session's first turn completes, which is a normal state rather than a
// failure.
//
// # Running a server to talk to
//
//	copilot --ui-server --port 4599 --allow-all-tools
//
// The process needs a real terminal. Without a PTY the CLI takes a different
// startup branch, never mounts the TUI, and therefore never starts the
// embedded server — it exits having logged nothing about a listener, which
// looks like a crash rather than a missing terminal. Anything launching this
// programmatically must allocate a PTY; tmux panes already have one.
//
// The port is not advertised anywhere machine-readable. Omitting `--port`
// binds an OS-assigned one recorded only in a log line, so callers should
// choose the port themselves. It binds a few seconds after exec, behind auth
// and workspace initialisation, which is what [DialRetry] absorbs.
//
// COPILOT_HOME relocates config and state and `--log-dir` relocates logs,
// which keeps test runs out of the operator's real profile.
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
