package agentd

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// --- /v1/whoami/hook ---
//
// The agentd side of TCL-754's hook broker. A `tclaude-layer` agent runs
// behind a mount namespace that hides ~/.tclaude/data, so its hook
// callbacks cannot write the conversation database the way every other
// launch does. Instead they POST the parsed hook event here and the
// daemon — which is on the host, holds the real database, the real hook
// lock, the operator's notification config and the tmux socket — applies
// it on their behalf.
//
// Three properties make this safe to leave ungated by a permission slug:
//
//  1. The event is applied to the session row the DAEMON resolved from the
//     caller's process ancestry. Nothing the caller sends selects a target.
//  2. The effect is exactly what the same caller would have achieved by
//     writing the database directly, which is what every harness-builtin
//     agent does unmediated today. Brokering removes a capability (reaching
//     the database) rather than adding one.
//  3. An agent applying its own hook events is infrastructure, in the same
//     class as /v1/whoami itself.
//
// What it does add is a path from inside a sandbox to the daemon's pane
// injection machinery, since ApplyHook can inject `/rename` after a
// /clear. That is handled explicitly below.

// hookBrokerMaxBody bounds a brokered hook payload. Hook inputs carry the
// user's prompt and the assistant's last message, so they are not tiny,
// but they are nowhere near this. Anything larger is malformed or hostile.
const hookBrokerMaxBody = 1 << 20 // 1 MiB

// handleWhoamiHook applies one hook event on behalf of the calling agent.
func handleWhoamiHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classAgent, classAgentUnknown:
		// Both are fine here, and classAgentUnknown deliberately so: a
		// brokered SessionStart is often the event that first establishes
		// the conv-id, so demanding a resolved one would lock out the first
		// hook of every agent. The session row below is resolved from
		// recorded host pids either way — the conv-id is not what
		// identifies the caller.
	case classUnidentified:
		writeUnidentified(w)
		return
	default:
		writeUnconfirmed(w, r)
		return
	}
	if p.PID == 0 {
		writeUnidentified(w)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, hookBrokerMaxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body", "could not read request body")
		return
	}
	if len(body) > hookBrokerMaxBody {
		writeError(w, http.StatusRequestEntityTooLarge, "body", "hook payload too large")
		return
	}
	var req session.BrokeredHookRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "body", "malformed hook payload")
		return
	}

	// Identity: the row comes from the caller's recorded host pids, never
	// from the request. A caller-supplied TCLAUDE_SESSION_ID is accepted
	// only as a cross-check.
	row, harnessPID := hookSessionRowForPID(p.PID)
	if row == nil {
		writeError(w, http.StatusForbidden, "auth",
			"could not resolve a session row for this caller; refusing to apply its hook")
		return
	}
	if claimed := strings.TrimSpace(req.ClaimedSessionID); claimed != "" && claimed != row.ID {
		slog.Warn("hook broker: rejecting event whose claimed session id disagrees with the resolved row",
			"caller_pid", p.PID, "claimed_session", claimed, "resolved_session", row.ID,
			"event", req.Input.HookEventName, "module", "hooks")
		writeError(w, http.StatusForbidden, "auth",
			"claimed session id does not match the session resolved for this caller")
		return
	}

	sanitizeBrokeredHookInput(&req.Input, row.ConvID)

	amb := session.BrokeredHookAmbient(session.BrokeredHookContext{
		RowTmuxSession:    row.TmuxSession,
		RowCwd:            row.Cwd,
		HarnessPID:        harnessPID,
		ExitGeneration:    req.ExitGeneration,
		AutoCompactWindow: req.AutoCompactWindow,
	})

	var stdout bytes.Buffer
	if err := session.DispatchHookEvent(req.Input, row.ID, amb, &stdout); err != nil {
		slog.Warn("hook broker: applying brokered hook event failed",
			"session", row.ID, "event", req.Input.HookEventName, "error", err, "module", "hooks")
		writeError(w, http.StatusInternalServerError, "hook", "failed to apply hook event")
		return
	}
	writeJSON(w, http.StatusOK, session.BrokeredHookResponse{Stdout: stdout.String()})
}

// sanitizeBrokeredHookInput clamps the one payload field that becomes a
// host-side capability when the daemon acts on it instead of the agent.
//
// TranscriptPath is a caller-supplied filesystem path that the Codex
// telemetry projection opens and scans, and whose directory is stored as
// the conversation's project dir. On the direct path the reader is the
// agent's own hook process, so naming a file it could not already open
// buys it nothing. Brokered, the reader is the daemon on the HOST, and the
// same string becomes two distinct capabilities: read any host file whose
// basename looks like a rollout, and read a PEER AGENT's Codex transcript
// — the second being a cross-agent leak the direct path never had.
//
// ownConvID is the conv-id of the session the daemon resolved for this
// caller, so the path is accepted only when it is that session's own
// rollout, really resolving inside the Codex sessions tree. See
// harness.IsCodexRolloutPathForConv for the three checks.
//
// Cost of the ownership check: a session whose row has not learned its
// conv-id yet (its very first event) drops the path and loses that one
// event's rollout projection. It self-heals on the next event, and the
// projection's by-id fallback covers the same ground, so this is a
// rounding error against the leak it closes.
//
// No size cap is imposed here: the projection scans the rollout line by
// line (tail-first for live .jsonl), never slurping it into memory, so
// file size bounds nothing worth bounding.
//
// Deliberately NOT clamped: the free-text fields (Prompt,
// LastAssistantMessage, Message, ErrorMessage, ToolInput, ToolResponse).
// They are stored and rendered, never executed or resolved, and the direct
// path already carries the same values into the same code. Diverging the
// two paths here would buy nothing and cost the shared-implementation
// property the broker is built on.
//
// The pane-injection sink is NOT handled here, because it is already
// handled at the only place it can be handled correctly — see
// hook_broker_injection_test.go for the standing proof.
func sanitizeBrokeredHookInput(in *session.HookCallbackInput, ownConvID string) {
	if in.TranscriptPath == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil || !harness.IsCodexRolloutPathForConv(home, ownConvID, in.TranscriptPath) {
		slog.Debug("hook broker: dropping a transcript path that is not this session's own rollout",
			"path", in.TranscriptPath, "conv_id", ownConvID, "module", "hooks")
		in.TranscriptPath = ""
	}
}
