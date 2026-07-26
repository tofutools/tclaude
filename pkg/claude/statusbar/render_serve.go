package statusbar

import (
	"encoding/json"
	"fmt"
)

// ApplyBrokeredRender performs, on behalf of a sandboxed agent, the
// host-touching half of one statusline render.
//
// It is the daemon's entry point into exactly the code the pane would
// have run: the same attribution gate, the same derivation, the same
// write set. Living in this package rather than in agentd is what makes
// that true by construction — there is no second implementation to keep
// in step.
//
// The two identity arguments are the whole security model:
//
//   - sessionID is the session row agentd resolved from the CALLER'S
//     PROCESS ANCESTRY. It replaces TCLAUDE_SESSION_ID, which on the
//     direct path is an inherited environment variable the caller could
//     set to anything. Substituting a trusted value into the same gate
//     makes the brokered path strictly stronger than the direct one.
//   - rowConvID is that row's own conversation, used only when the
//     payload named none.
//
// Everything else in the request is the agent's own telemetry about
// itself. It cannot select a target: the request has no field that names
// a session, and the conversation it names is checked against the row
// before anything is keyed by it.
func ApplyBrokeredRender(req BrokeredRenderRequest, sessionID, rowConvID string) (BrokeredRenderResponse, error) {
	var resp BrokeredRenderResponse

	var input StatusLineInput
	if len(req.Payload) > 0 {
		if err := json.Unmarshal(req.Payload, &input); err != nil {
			return resp, fmt.Errorf("malformed statusline payload: %w", err)
		}
	}

	// The attribution gate, run against the RESOLVED session rather than
	// a claimed one.
	owned := ownedSessionID(sessionID, req.RenderConvID)
	resp.Owned = owned != ""

	// Which conversation this render may key conv-scoped work by.
	//
	// The direct path lets an unowned render write its OWN workspace row,
	// because there the writer is the foreign process itself and the row
	// it touches is its own. Brokered, the writer is the daemon and the
	// conversation is a caller-supplied string, so the same permissive
	// rule would let any wrapped agent overwrite any peer's location,
	// branch and PR cells on the dashboard. It is therefore refused here
	// instead: a render that does not belong to the resolved row keys
	// nothing.
	workspaceConv := ""
	if owned != "" {
		workspaceConv = req.RenderConvID
		if workspaceConv == "" {
			workspaceConv = rowConvID
		}
	}

	observed := observedPinnedWindow(req.EnvPinnedWindow)
	resolved := observed
	if resolved == 0 && owned != "" {
		// Same precedence and same fail-soft direction as
		// resolvePinnedWindow: the row is consulted only when the pane's
		// own environment said nothing, and only for a render that owns
		// the row.
		_, resolved = resolvePinnedWindow(req.EnvPinnedWindow, owned)
	}
	resp.PinnedWindow = resolved
	resp.SandboxOff = temporarySandboxOff(workspaceConv)

	derived := deriveRender(input, observed, resolved)
	hasLimits := hasSubscriptionLimits(input)
	if req.ApplyWrites {
		// Written before it is read, as on the direct path — see
		// updateUsageCacheFromRender.
		updateUsageCacheFromRender(input)
	}
	if !hasLimits && req.WantUsage {
		usage, stale := cachedUsage()
		resp.Usage, resp.UsageStale, resp.UsagePresent = usage, stale, true
		hasLimits = usageHasLimits(usage)
	}

	if req.ApplyWrites {
		applyRenderWrites(renderWrites{
			Input:         input,
			Payload:       req.Payload,
			Git:           req.Git,
			Derived:       derived,
			Owned:         owned,
			WorkspaceConv: workspaceConv,
			HasLimits:     hasLimits,
		})
	}
	return resp, nil
}
