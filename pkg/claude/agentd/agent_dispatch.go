package agentd

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// handleAgentByConv dispatches POST /v1/agent/{selector}/{verb} to a
// per-verb handler. The {selector} is resolved via agent.ResolveSelector
// (alias, full conv-id, or 8+-char prefix); the {verb} routes to one
// of the cross-agent operations (today: reincarnate; clone, compact,
// rename are future work).
//
// Self-targeted variants (e.g. /v1/whoami/reincarnate) keep their
// existing self.<verb> auth and are NOT routed here.
func handleAgentByConv(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/agent/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		writeError(w, http.StatusNotFound, "not_found",
			"expected /v1/agent/{selector}/{verb}")
		return
	}
	selector, verb := parts[0], parts[1]
	if u, err := url.PathUnescape(selector); err == nil {
		selector = u
	}
	res, _, err := agent.ResolveSelector(selector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found",
			"could not resolve target conv "+selector+": "+err.Error())
		return
	}

	switch verb {
	case "reincarnate":
		handleAgentReincarnate(w, r, res.ConvID)
	case "compact":
		handleAgentCompact(w, r, res.ConvID)
	case "rename":
		handleAgentRename(w, r, res.ConvID)
	case "clone":
		handleAgentClone(w, r, res.ConvID)
	case "stop":
		handleAgentStop(w, r, res.ConvID)
	case "resume":
		handleAgentResume(w, r, res.ConvID)
	case "delete":
		handleAgentDelete(w, r, res.ConvID)
	default:
		writeError(w, http.StatusNotFound, "not_found",
			"unknown verb "+verb+" for /v1/agent/{selector}/...")
	}
}

// requireCrossAgentPermission gates a cross-agent endpoint. The caller
// passes if ANY of:
//
//   - they hold the slug `perm` (granted via config defaults or
//     per-conv SQLite grants — same dual-source check as
//     requirePermission)
//   - they own at least one group that contains targetConv (mirrors
//     the owner-implicit-power semantics already used for messaging
//     in db.CanSenderReachTarget)
//   - they sent X-Tclaude-Ask-Human: <duration> AND the human
//     approves the cross-agent action via the loopback popup before
//     the timeout expires (same escape hatch the self-targeted
//     endpoints honor)
//
// Humans (no claude ancestor) always pass — same convention as
// requirePermission. Returns (callerConvID, ok); callerConvID is ""
// for humans, the agent's conv-id otherwise. On failure the error
// response is already written.
//
// The popup is the manager-pattern escape hatch: a manager that
// doesn't normally manage a particular peer can ask the human for
// one-off escalation rather than forcing the human to issue a
// permanent slug grant. The popup surfaces who's calling, what the
// target is, and which perm slug is being requested so the human
// can make an informed decision.
func requireCrossAgentPermission(w http.ResponseWriter, r *http.Request, perm, targetConv string) (string, bool) {
	p := peerFromContext(r.Context())
	if p.PID == 0 {
		writeError(w, http.StatusUnauthorized, "auth",
			"could not determine peer PID; refusing to evaluate permission")
		return "", false
	}
	if !p.HasClaudeAncestor {
		return "", true
	}
	if p.ConvID == "" {
		writeError(w, http.StatusForbidden, "auth",
			"caller has a Claude Code ancestor but no resolvable conv-id; cannot evaluate permission")
		return "", false
	}
	cfg, _ := config.Load()
	if cfg.HasDefaultPermission(perm) {
		return p.ConvID, true
	}
	if ok, err := db.HasAgentPermissionRow(p.ConvID, perm); err == nil && ok {
		return p.ConvID, true
	}
	if ownerOfGroupContaining(p.ConvID, targetConv) {
		return p.ConvID, true
	}

	// Last chance: human-approval popup. Same shape as the
	// self-targeted path in requirePermission, with the cross-agent
	// target surfaced so the popup can render
	// "<caller> wants to <verb> <target>". Timeout = deny.
	if timeout := parseAskHumanHeader(r); timeout > 0 && popupBaseURL != "" {
		bodyPreview := snapshotRequestBody(r)
		callerTitle := ""
		if row := agent.FreshConvRowResolved(p.ConvID); row != nil {
			callerTitle = agent.DisplayTitle(row)
		}
		targetTitle := ""
		if row := agent.FreshConvRowResolved(targetConv); row != nil {
			targetTitle = agent.DisplayTitle(row)
		}
		req := &approvalRequest{
			id:              newApprovalID(),
			perm:            perm,
			convID:          p.ConvID,
			convTitle:       callerTitle,
			method:          r.Method,
			path:            r.URL.Path,
			rawQuery:        r.URL.RawQuery,
			bodyPreview:     bodyPreview,
			targetConvID:    targetConv,
			targetConvTitle: targetTitle,
			createdAt:       time.Now(),
			timeout:         timeout,
			decision:        make(chan bool, 1),
			extend:          make(chan time.Duration, 1),
		}
		if requestHumanApproval(req, popupBaseURL) {
			return p.ConvID, true
		}
		writeError(w, http.StatusForbidden, "permission",
			fmt.Sprintf("human declined or timed out after %s on cross-agent permission %q for target %s",
				timeout, perm, short8(targetConv)))
		return "", false
	}

	writeError(w, http.StatusForbidden, "permission",
		fmt.Sprintf("caller is not granted %q for target %s, and is not an owner of any group containing it (grant via `tclaude agent permissions grant %s %s`, add caller as owner of a shared group, or call again with X-Tclaude-Ask-Human: <duration> to ask the human via popup)",
			perm, short8(targetConv), perm, short8(p.ConvID)))
	return "", false
}

// ownerOfGroupContaining returns true if ownerConv owns at least one
// group whose membership includes targetConv. Linear scan over owned
// groups; expected to be cheap (most agents own a handful of groups
// at most).
func ownerOfGroupContaining(ownerConv, targetConv string) bool {
	owned, err := db.ListGroupsOwnedBy(ownerConv)
	if err != nil || len(owned) == 0 {
		return false
	}
	for _, gID := range owned {
		members, err := db.ListAgentGroupMembers(gID)
		if err != nil {
			continue
		}
		for _, m := range members {
			if m.ConvID == targetConv {
				return true
			}
		}
	}
	return false
}
