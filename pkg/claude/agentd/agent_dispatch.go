package agentd

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// handleAgentByConv dispatches POST /v1/agent/{selector}/{verb} to a
// per-verb handler. The {selector} is resolved via agent.ResolveSelector
// (title, full conv-id, or 8+-char prefix); the {verb} routes to one
// of the cross-agent operations (rename, compact, interrupt, lifecycle,
// scheduling, and metadata verbs).
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
	var convID string
	if err == nil {
		convID = res.ConvID
	} else if verb == "delete" && looksLikeConvID(selector) {
		// Orphan-delete fallback: when conv_index is gone but
		// referencing rows (sessions, group_members, …) still exist,
		// the resolver legitimately can't find the conv. Accept the
		// raw UUID for `delete` only so the union purge can still run.
		// Gated on UUID shape so we don't blindly accept arbitrary
		// input — defence-in-depth on top of the dispatcher's
		// permission gating downstream.
		convID = selector
	} else if verb == "retire" && looksLikeConvID(selector) && isDanglingAgentEntry(selector) {
		// Dangling agent entry: an enrollment whose conversation data
		// is gone, so retire can't resolve a conversation to demote.
		// Signal the caller (the CLI prints guidance toward `agent
		// delete`; the dashboard pops a remove-confirm) instead of the
		// dead-end 404 that left the entry stuck. Read-only — the
		// destructive cleanup runs only if the caller follows up with
		// DELETE, which has its own permission gate.
		//
		// Gate the signal behind the SAME permission a normal retire
		// requires, so an unauthorized caller gets the usual 403/404 and
		// can't use the 409 to distinguish "dangling enrollment" from
		// "unknown conv" — a disclosure a bare resolver 404 never gave.
		// requireCrossAgentPermission writes its own failure response.
		if _, ok := requireCrossAgentPermission(w, r, PermAgentRetire, selector); !ok {
			return
		}
		writeDanglingAgentResponse(w, selector)
		return
	} else {
		writeError(w, http.StatusNotFound, "not_found",
			"could not resolve target conv "+selector+": "+err.Error())
		return
	}

	switch verb {
	case "reincarnate":
		handleAgentReincarnate(w, r, convID)
	case "compact":
		handleAgentCompact(w, r, convID)
	case "interrupt":
		handleAgentInterrupt(w, r, convID)
	case "rename":
		handleAgentRename(w, r, convID)
	case "remote-control":
		handleAgentRemoteControl(w, r, convID)
	case "sandbox-impl":
		handleAgentSandboxImplementation(w, r, convID)
	case "clone":
		handleAgentClone(w, r, convID)
	case "stop":
		handleAgentStop(w, r, convID)
	case "resume":
		handleAgentResume(w, r, convID)
	case "delete":
		handleAgentDelete(w, r, convID)
	case "promote":
		handleAgentPromote(w, r, convID)
	case "retire":
		handleAgentRetire(w, r, convID)
	case "reinstate":
		handleAgentReinstate(w, r, convID)
	case "dir":
		handleAgentDir(w, r, convID)
	case "context":
		handleAgentContext(w, r, convID)
	case "codex-app-server":
		handleAgentCodexAppServerStatus(w, r, convID)
	case "task":
		handleAgentTask(w, r, convID)
	case "prs":
		handleAgentPRs(w, r, convID)
	case "tags":
		handleAgentTags(w, r, convID)
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
//
// actx (at most one) carries the scope evaluation context, with the same
// semantics as requirePermissionEx. target_agent is always the target's stable
// agent_id and is derived centrally from targetConv when the caller did not
// already supply it. A failed resolution leaves the dimension empty, so a
// target-scoped grant fails closed.
func requireCrossAgentPermission(w http.ResponseWriter, r *http.Request, perm, targetConv string, actx ...ActionContext) (string, bool) {
	clearAuthorizedPermission(r)
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classUnidentified:
		writeUnidentified(w)
		return "", false
	case classHuman:
		return "", true
	case classAgentUnknown:
		writeAgentUnknown(w)
		return "", false
	case classUnconfirmed:
		writeUnconfirmed(w, r)
		return "", false
	case classAgent:
		// Confirmed agent — fall through to the per-conv evaluation below.
	}
	if hasWriteProofApprovalContinuation(r, p.ConvID, perm, targetConv) ||
		hasHumanApprovalContinuation(r, perm, targetConv) {
		recordAuthorizedPermission(r, perm, 0)
		return p.ConvID, true
	}
	scopeContext := actionContextOf(actx)
	scopeContext.targetConv = targetConv
	if scopeContext.TargetAgent == "" {
		targetAgent, err := db.AgentIDForConv(targetConv)
		if err != nil {
			slog.Warn("permissions: cross-agent target lookup failed (scopes fail closed)",
				"permission", perm, "target_conv", targetConv, "error", err)
		} else {
			scopeContext.TargetAgent = targetAgent
		}
	}
	if allowed, matched := permissionAllowsAction(r, p.ConvID, perm, scopeContext); allowed {
		recordAuditPermissionScope(r, perm, matched)
		recordAuthorizedPermission(r, perm, loadBearingSudoGrantID(r, p.ConvID, perm, scopeContext))
		return p.ConvID, true
	}

	// A global agent.* deny or missing grant does not suppress the distinct
	// groups.members.* capability. The sibling must cover EVERY current active
	// group containing the target; different positive sources may collectively
	// cover that finite set.
	if sibling := GroupSiblingForSlug(perm); sibling != "" {
		groups := scopeContext.affectedGroups
		var err error
		if groups == nil {
			groups, err = activeGroupNamesForConvs(targetConv)
		}
		if err != nil {
			slog.Warn("permissions: target group footprint lookup failed", "target", targetConv, "error", err)
		} else if len(groups) > 0 {
			covered := true
			var sudoGrantID int64
			for _, group := range groups {
				candidate := scopeContext
				candidate.Group = group
				if allowed, _ := permissionAllowsAction(r, p.ConvID, sibling, candidate); !allowed {
					covered = false
					break
				}
				if sudoGrantID == 0 {
					sudoGrantID = loadBearingSudoGrantID(r, p.ConvID, sibling, candidate)
				}
			}
			if covered {
				recordAuditPermissionScope(r, sibling, "group="+strings.Join(groups, ","))
				recordAuthorizedPermission(r, sibling, sudoGrantID)
				return p.ConvID, true
			}
		}
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
			// autoGrantable stays false on the cross-agent path: "always
			// allow" persists the slug on the CALLER, which for a cross-agent
			// capability would grant it against ALL targets — broader than the
			// one-off, one-target approval the human gave. "Always allow" is
			// intentionally scoped to the self / human-surface popup path.
			createdAt: time.Now(),
			timeout:   timeout,
			decision:  make(chan approvalOutcome, 1),
			extend:    make(chan time.Duration, 1),
		}
		if requestHumanApproval(req, popupBaseURL) {
			markWriteProofHumanApproval(r, perm, targetConv)
			recordAuthorizedPermission(r, perm, 0)
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

const (
	authorizedPermissionHeader = "X-Tclaude-Internal-Authorized-Permission"
	authorizedSudoGrantHeader  = "X-Tclaude-Internal-Authorized-Sudo-Grant"
)

func clearAuthorizedPermission(r *http.Request) {
	r.Header.Del(authorizedPermissionHeader)
	r.Header.Del(authorizedSudoGrantHeader)
}

func recordAuthorizedPermission(r *http.Request, slug string, sudoGrantID int64) {
	r.Header.Set(authorizedPermissionHeader, slug)
	if sudoGrantID > 0 {
		r.Header.Set(authorizedSudoGrantHeader, strconv.FormatInt(sudoGrantID, 10))
	} else {
		r.Header.Del(authorizedSudoGrantHeader)
	}
}

func authorizedPermissionForRequest(r *http.Request, fallback string) string {
	if slug := r.Header.Get(authorizedPermissionHeader); slug != "" {
		return slug
	}
	return fallback
}

func authorizedSudoGrantIDForRequest(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.Header.Get(authorizedSudoGrantHeader), 10, 64)
	return id
}

// activeGroupNamesForConvs computes the bounded authorization footprint used
// by group-scoped operations on globally shared agents. It snapshots current
// active memberships exactly once per target, ignores archived/history rows,
// and never recursively chases downstream effects.
func activeGroupNamesForConvs(targets ...string) ([]string, error) {
	seen := map[string]bool{}
	for _, target := range targets {
		if target == "" {
			continue
		}
		groups, err := db.ListGroupsForConv(target)
		if err != nil {
			return nil, err
		}
		for _, group := range groups {
			if group != nil && !group.IsArchived() {
				seen[group.Name] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// requireInboxAccess resolves the effective inbox conv for a read-only
// operation (list, message-fetch). When no X-Tclaude-Target-Conv header
// is set, behaves like requireAgent — returns the caller's own conv.
// When the header IS set:
//
//   - The target is resolved via agent.ResolveSelector (title / prefix /
//     full conv-id), same convention as the manager-pattern verbs.
//   - The caller must hold the agent.inbox-watch slug, or own a group
//     containing the target. Humans (no claude ancestor) bypass.
//   - On grant, returns (target, isOperator=true, ok=true).
//
// 403 with the slug surfaced in the error message on denial. The popup
// escape hatch (X-Tclaude-Ask-Human) is supported the same way as on
// the lifecycle verbs — header-based, capped at 300s.
//
// Same dual-source check as the lifecycle verbs (cfg defaults +
// per-agent SQLite grants), so a slug granted via either mechanism
// works identically.
func requireInboxAccess(w http.ResponseWriter, r *http.Request) (effectiveConv string, isOperator, ok bool) {
	target := strings.TrimSpace(r.Header.Get("X-Tclaude-Target-Conv"))
	if target == "" {
		// Self-targeted: caller IS the target. Same shape as requireAgent
		// — agent identity is required.
		convID, ok := requireAgent(w, r)
		return convID, false, ok
	}
	// Resolve titles / prefixes the same way the lifecycle dispatcher
	// does, so callers can pass `--target some-name`.
	res, _, err := agent.ResolveSelector(target)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found",
			"could not resolve --target "+target+": "+err.Error())
		return "", false, false
	}
	target = res.ConvID

	caller, ok := requireCrossAgentPermission(w, r, PermAgentInboxWatch, target)
	if !ok {
		return "", false, false
	}
	// caller is "" for humans (no agent identity), the agent's conv otherwise.
	// In both cases the EFFECTIVE conv to query is the target.
	//
	// isOperator means "the caller is reading someone ELSE's inbox" — it
	// forces keep-unread so a drive-by read doesn't clobber the recipient's
	// read marker. Compare on the stable actor (JOH-323): an agent that
	// reincarnated / ran /clear and reads its own inbox via --target<self>
	// resolves target to its current head, which differs from a predecessor
	// caller conv as a string — a conv-literal `caller != target` would have
	// mis-flagged that self-read as an operator view. sameActor keeps it a
	// self-read across generations; genuinely different agents still differ.
	return target, caller != "" && !sameActor(caller, target), true
}

// ownerOfGroupContaining returns true if ownerConv owns at least one
// group whose membership includes targetConv.
//
// Membership is matched on the stable agent (JOH-323): db.FindMemberInGroup
// resolves targetConv to its agent_id and looks the member up by that, so a
// target named by any of its generations is recognised — the rotation-immune
// form of the old `m.ConvID == targetConv` scan (which only matched a
// member's current conv). Semantics are unchanged today: members are listed
// from agent-keyed storage, so a non-agent targetConv could never equal a
// member's conv under the old compare either, and FindMemberInGroup likewise
// returns no match for a conv with no actor row.
func ownerOfGroupContaining(ownerConv, targetConv string) bool {
	if !ownerOwnsEveryActiveGroupContaining(ownerConv, targetConv) {
		return false
	}
	ok, err := db.OwnerHasGroupContaining(ownerConv, targetConv)
	return err == nil && ok
}

// ownerOwnsEveryActiveGroupContaining is retained for non-permission
// relationship visibility (currently cron read filtering). Authorization gates
// do not call it; they resolve the dedicated groups.members.* slugs instead.
func ownerOwnsEveryActiveGroupContaining(ownerConv, targetConv string) bool {
	groups, err := db.ListGroupsForConv(targetConv)
	if err != nil {
		return false
	}
	active := 0
	for _, group := range groups {
		if group.IsArchived() {
			continue
		}
		active++
		owns, err := db.IsAgentGroupOwner(group.ID, ownerConv)
		if err != nil || !owns {
			return false
		}
	}
	return active > 0
}
