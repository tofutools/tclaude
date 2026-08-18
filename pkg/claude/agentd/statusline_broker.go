package agentd

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/statusbar"
)

// --- /v1/whoami/statusline ---
//
// The statusline half of TCL-754's broker, and the sibling of
// /v1/whoami/hook. A `tclaude-layer` agent's mount namespace hides
// ~/.tclaude/data, so its status line can neither record what it shows
// (context usage, model, effort, cost, where the agent is working) nor
// read the two facts it renders from the database. It POSTs its payload
// here instead and the daemon does both on its behalf.
//
// The same three properties that make the hook endpoint safe without a
// permission slug hold here, for the same reasons:
//
//  1. The row comes from the caller's process ancestry. Nothing in the
//     request selects a target — the conversation it names is checked
//     against the resolved row before anything is keyed by it.
//  2. The effect is what the caller would have achieved by writing the
//     database directly, which every unsandboxed agent does unmediated.
//  3. Brokering removes a capability rather than adding one.
//
// Where it differs from the hook endpoint is cadence. Hooks arrive when
// the agent does something; statuslines re-render several times a second
// forever. The client therefore only calls when its payload actually
// changed or its cached reads went stale (see the statusbar package), and
// this endpoint is behind the shared per-agent limiter either way.

// handleWhoamiStatusline applies one statusline render's writes on behalf
// of the calling agent and returns the facts its bar needs.
func handleWhoamiStatusline(w http.ResponseWriter, r *http.Request) {
	const endpoint = "/v1/whoami/statusline"

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classAgent, classAgentUnknown:
		// classAgentUnknown is accepted for the same reason the hook
		// endpoint accepts it: a freshly spawned agent renders its status
		// line before its first SessionStart hook has established a
		// conv-id, and the row below is resolved from recorded host pids
		// regardless.
	case classUnidentified:
		if checkBrokerRate(endpoint, brokerPreIdentityKey, brokerPreIdentityRatePerSecond).Reject {
			writeError(w, http.StatusTooManyRequests, "rate", "too many unplaceable requests")
			return
		}
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

	// Identity first, so the rate limit can be keyed on the agent rather
	// than on anything the caller controls.
	row, _ := hookSessionRowForPID(p.PID)
	if row == nil {
		// Before the rate check, for the reason spelled out at the same
		// point in hook_broker.go: a throttled request is still a refused
		// one, and this limiter's bucket is shared across every
		// unplaceable caller.
		brokerRefusals.recordUnplaceable("statusline: caller could not be placed")
		if checkBrokerRate(endpoint, brokerPreIdentityKey, brokerPreIdentityRatePerSecond).Reject {
			writeError(w, http.StatusTooManyRequests, "rate", "too many unplaceable requests")
			return
		}
		writeError(w, http.StatusForbidden, "auth",
			"could not resolve a session row for this caller; refusing to apply its statusline")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, brokerMaxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body", "could not read request body")
		return
	}
	if len(body) > brokerMaxBody {
		logBrokerBodyOverCap(endpoint, row.ID, len(body))
		writeError(w, http.StatusRequestEntityTooLarge, "body", "statusline payload too large")
		return
	}

	var req statusbar.BrokeredRenderRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "body", "malformed statusline payload")
		return
	}
	if claimed := strings.TrimSpace(req.ClaimedSessionID); claimed != "" && claimed != row.ID {
		if startupRow, _ := claimedLivePaneSessionRow(p.PID, claimed); startupRow != nil {
			row = startupRow
		} else {
			slog.Warn("statusline broker: rejecting render whose claimed session id disagrees with the resolved row",
				"caller_pid", p.PID, "claimed_session", claimed, "resolved_session", row.ID, "module", "hooks")
			brokerRefusals.recordClaimMismatch(row.ID, "statusline: claimed session id disagrees with the resolved row")
			writeError(w, http.StatusForbidden, "auth",
				"claimed session id does not match the session resolved for this caller")
			return
		}
	}
	if checkBrokerRate(endpoint, row.ID, brokerRatePerSecond).Reject {
		writeError(w, http.StatusTooManyRequests, "rate", "too many statusline renders")
		return
	}
	if !safeBrokeredConvID(req.RenderConvID) {
		// Same reasoning as the hook endpoint: a conv-id is not merely
		// stored, and a caller-controlled one that is not a single
		// path-safe segment has no legitimate use.
		writeError(w, http.StatusBadRequest, "body",
			"conversation id must be a single path-safe segment")
		return
	}

	resp, err := statusbar.ApplyBrokeredRender(req, row.ID, row.ConvID)
	if err != nil {
		slog.Debug("statusline broker: applying brokered render failed",
			"session", row.ID, "error", err, "module", "hooks")
		writeError(w, http.StatusBadRequest, "body", "could not apply statusline render")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
