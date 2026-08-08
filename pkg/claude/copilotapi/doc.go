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
//
// The gap is not that a generated client would obviously lack a way to create
// a session. It would have one — `sessions.open` with `kind: "create"` is
// fully described in the schema and genuinely succeeds. It would just be the
// wrong one, and the failure would arrive later and elsewhere. See the traps
// below. `copilot-sdk/index.js` — the reference client — is authoritative for
// what the wire actually carries, and the narrow surface we need is small
// enough to hand-write against it and verify live.
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
// `sessions.open` is the session-opening method the schema documents, and it
// is a trap in three separate ways. All three were verified against a live
// 1.0.78 server.
//
//   - `{kind: "create"}` really does create a session and reports
//     `{"status":"created", "sessionId":…}`, writing session state to disk.
//     But the session it creates is not registered in the RPC session
//     registry, so it is undrivable: `session.name.set` on it fails with
//     "Session not found", and `session.setForeground` returns
//     `{"success":false}`. The danger here is not a missing create path — it
//     is a create path that exists, reports success, and hands back something
//     that cannot be driven.
//   - `{kind: "attach"}` against the pane's own startup session returns
//     `{"status":"resumed"}`, which reads as success, while the session
//     remains just as undrivable.
//   - An unknown session yields `{"status":"not_found"}` as a *successful*
//     JSON-RPC result. This one is documented — `not_found` is a value of the
//     SessionsOpenStatus enum — so the hazard is not that the server stays
//     silent, but that a caller checking only for transport and JSON-RPC
//     errors sails straight past a status field that was telling it the
//     truth.
//
// In every case the symptom surfaces later, on some unrelated `session.*`
// call, far from the `sessions.open` that caused it. Use `session.create`,
// which is the only path to a session this client can actually drive.
//
// `session.create` is a trap of its own once a caller has a session id that
// already means something. It does not attach to an existing id — it starts
// that id FRESH, and the loss is total and silent: `alreadyInUse:false`, an
// empty `session.getMessages`, the pane's timeline emptied, and a model with no
// memory of its own previous turn. There is no error and no field in the reply
// that distinguishes "created the session you named" from "replaced the
// conversation you named". [Client.ResumeSession] is the call for an id with
// history behind it, and the choice between the two belongs to whoever knows
// whether this launch was fresh or a resume — which is never this package.
//
// `session.resume` is absent from `schemas/api.schema.json` for the same reason
// `session.create` is: both live in the SDK's own method table (the server's
// `SESSION_CREATE` / `SESSION_RESUME` seam handlers) rather than in the
// documented `session` section. It takes the same argument set as
// `session.create`, answers with the same session-info shape, and reports a
// session it cannot find as a plain "Session not found" error rather than
// creating one — so the two outcomes are distinguishable, which is what lets a
// caller refuse to recover by creating.
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
// # State: read it, do not accumulate it
//
// The event stream is the right way to learn that something HAPPENED and the
// wrong way to learn what is TRUE. Every question a consumer of this package
// is likely to ask has a point-in-time read that answers it outright:
//
//   - "is the agent busy" — [Client.IsProcessing], [Client.Activity]
//   - "is a human being waited on" — [Client.PendingPermissionRequests]
//   - "how full is the context window" — [Client.ContextInfo]
//   - "what has this session spent" — [Client.UsageMetrics]
//
// So the shape that works is: an event marks the session dirty, and a read
// establishes what is true. Nothing displayed is ever derived from an event.
//
// That is not a stylistic preference, because the alternative does not survive
// a reconnect. `session.idle` and `assistant.idle` are both EPHEMERAL, so they
// never reach the persisted log and `session.eventLog.read` cannot replay them;
// a consumer that had accumulated state from the stream would resume with a gap
// it has no way to close. A consumer that reads instead has nothing to catch up
// on: its first act on any connection, new or reconnected, is the same read it
// always does.
//
// `session.eventLog.tail` and `session.eventLog.read` do exist, and are not
// what their names suggest: `tail` returns ONLY a cursor and no events, while
// `read` pages from a cursor with `direction`, `includeEphemeral` and
// `agentScope` filters (`agentScope: "primary"` being a server-side answer to
// the sub-agent question). They are the right tools for replaying a
// TRANSCRIPT. They are the wrong tool for answering "what is true now", and
// this package deliberately models neither.
//
// Reading usage back has two traps worth repeating from the type docs.
// [UsageMetrics.ModelMetrics] nests token counts under
// [ModelMetric.Usage] and request counts under [ModelMetric.Requests]; a
// flattened struct decodes the same payload without error and reports zero
// for everything. And [Client.ContextInfo] legitimately returns nil before a
// session's first turn completes, which is a normal state rather than a
// failure.
//
// Two more that live on [ContextInfo] itself. Its ModelName is NOT the model
// the turn ran on: under auto mode it was measured reporting
// `claude-sonnet-4.5` across two consecutive turns that both ran on
// `gpt-5-mini`, while [UsageMetrics.CurrentModel] and the
// `session.auto_mode_resolved` event's `chosenModel` both named the real one.
// Read the model from usage; usage misattributed to a plausible model looks
// entirely healthy. And its parts are not all additive —
// [ContextInfo.MCPToolsTokens] is a SUBSET of
// [ContextInfo.ToolDefinitionTokens] — so the identity that holds is
// system + conversation + toolDefinitions = total, verified against a live
// payload with a non-zero MCP figure (6911 + 144 + 9320 = 16375, with
// mcpToolsTokens 1238 already inside the 9320).
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
// [Dial] performs the handshake and records the server's protocol version and
// CLI version, readable via [Client.ProtocolVersion] and
// [Client.ServerVersion]. The
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
