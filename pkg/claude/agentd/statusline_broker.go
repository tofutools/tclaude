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
//  1. The request's session id only selects a candidate row. The daemon uses
//     the live pane's launch generation and host PID ancestry to prove that
//     candidate owns the Unix-socket caller before applying anything.
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
	case classAgent, classAgentUnknown, classUnconfirmed:
		// classAgentUnknown is accepted for the same reason the hook
		// endpoint accepts it: a freshly spawned agent renders its status
		// line before its first SessionStart hook has established a conv-id.
		// classUnconfirmed is provisional until the generation-bound pane
		// proof below authenticates a version-named sandboxed harness.
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

	// Resolve the ordinary candidate before parsing, but keep it provisional
	// until the request claim can be checked against the live pane. The shared
	// guard bounds that pre-proof work; only the final row is charged below.
	row, _ := hookSessionRowForPID(p.PID)
	preProofKey := brokerPreIdentityKey
	if row != nil {
		preProofKey = brokerPreIdentityKeyForRow(row.ID)
	}
	if checkBrokerRate(endpoint, preProofKey, brokerPreIdentityRatePerSecond).Reject {
		if row == nil {
			brokerRefusals.recordUnplaceable("statusline: caller could not be placed")
		}
		writeError(w, http.StatusTooManyRequests, "rate", "too many requests before identity verification")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, brokerMaxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body", "could not read request body")
		return
	}
	if len(body) > brokerMaxBody {
		resolvedID := ""
		if row != nil {
			resolvedID = row.ID
		}
		logBrokerBodyOverCap(endpoint, resolvedID, len(body))
		writeError(w, http.StatusRequestEntityTooLarge, "body", "statusline payload too large")
		return
	}

	var req statusbar.BrokeredRenderRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "body", "malformed statusline payload")
		return
	}
	claimed := strings.TrimSpace(req.ClaimedSessionID)
	proofKey := ""
	if row != nil {
		proofKey = row.ID
	}
	if checkBrokerProofRate(endpoint, brokerProofKeyForRow(proofKey)).Reject {
		writeError(w, http.StatusTooManyRequests, "rate", "too many identity proof attempts")
		return
	}
	provedRow, _, layerClaim := proveTclaudeLayerCaller(p.PID, claimed)
	switch {
	case layerClaim && provedRow != nil:
		row = provedRow
	case layerClaim:
		if row != nil {
			brokerRefusals.recordClaimMismatch(row.ID,
				"statusline: claimed tclaude-layer session failed live-pane proof")
		} else {
			brokerRefusals.recordUnplaceable("statusline: tclaude-layer caller failed live-pane proof")
		}
		writeError(w, http.StatusForbidden, "auth", "claimed tclaude-layer session does not own this caller")
		return
	case row == nil:
		brokerRefusals.recordUnplaceable("statusline: caller could not be placed")
		writeError(w, http.StatusForbidden, "auth",
			"could not resolve a session row for this caller; refusing to apply its statusline")
		return
	case isTclaudeLayerRow(row):
		brokerRefusals.recordUnplaceable("statusline: tclaude-layer callback omitted its session claim")
		writeError(w, http.StatusForbidden, "auth",
			"tclaude-layer statusline requires a proved session claim")
		return
	case claimed != "" && claimed != row.ID:
		slog.Warn("statusline broker: rejecting render whose claimed session id disagrees with the resolved row",
			"caller_pid", p.PID, "claimed_session", claimed, "resolved_session", row.ID, "module", "hooks")
		brokerRefusals.recordClaimMismatch(row.ID, "statusline: claimed session id disagrees with the resolved row")
		writeError(w, http.StatusForbidden, "auth",
			"claimed session id does not match the session resolved for this caller")
		return
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
