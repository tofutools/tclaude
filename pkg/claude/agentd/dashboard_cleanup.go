package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/conv"
)

// Bulk-cleanup endpoints for the dashboard's "🧹 cleanup" affordances.
// Two sub-paths, both POST:
//
//	/api/cleanup/group   — remove confirmed-offline members from ONE
//	                       group (the per-group cleanup button).
//	/api/cleanup/agents  — operate on a human-picked set of
//	                       conversations across the whole system —
//	                       active agents, retired agents and plain
//	                       (never-enrolled) conversations alike. One of
//	                       four tiers per request: unjoin, retire,
//	                       delete or reinstate (see dashboardCleanupAgents).
//	                       Powers the Agents-tab cleanup button and the
//	                       Groups-tab "clean up all groups" one.
//
// Cleanup is human-only by construction: these routes live on the
// loopback dashboard server and are gated by the dashboard cookie +
// Origin pin, exactly like every other /api mutation. Agents talk to
// /v1 and have no path here.
//
// Safety model. The browser sends an explicit, human-edited list of
// conv-ids — the daemon never trusts the "offline" label the snapshot
// rendered. Every conv-id is re-checked against live tmux
// (isConvOnline) at execute time; a conv that turns out still-alive is
// skipped, not touched — unless the request opts in with
// include_online, which lets the tier act on running sessions (delete
// force-stops them first). The non-destructive reinstate tier ignores
// liveness entirely. A few races remain unavoidable (an agent could
// spawn a pane in the window between the check and the DB write); the
// session reaper resolves those on its next sweep. The endpoints are
// idempotent — re-running a cleanup over already-cleaned conv-ids just
// reports them as skipped.

// cleanupOutcome is the per-conv-id result of one cleanup pass. The
// dashboard renders these back into the modal so the human sees
// exactly what happened, including every skip and its reason.
type cleanupOutcome struct {
	// AgentID is the cleaned-up actor's stable key — the canonical ID the
	// dashboard/CLI leads with; ConvID is the live generation behind it
	// (kept as the snapshot/hover). "" when the conv is not a known agent
	// (e.g. a plain conversation being deleted).
	AgentID string   `json:"agent_id,omitempty"`
	ConvID  string   `json:"conv_id"`
	Title   string   `json:"title,omitempty"`
	Result  string   `json:"result"`           // removed | retired | deleted | reinstated | skipped | failed
	Detail  string   `json:"detail,omitempty"` // human-readable reason
	Groups  []string `json:"groups,omitempty"` // groups touched (agents mode)
}

// cleanupResponse is the wire shape returned by both cleanup
// sub-paths. Outcomes is always non-nil so the dashboard can .map()
// over it unconditionally.
type cleanupResponse struct {
	Mode       string           `json:"mode"`
	Outcomes   []cleanupOutcome `json:"outcomes"`
	Removed    int              `json:"removed"`
	Retired    int              `json:"retired"`
	Deleted    int              `json:"deleted"`
	Reinstated int              `json:"reinstated"`
	Skipped    int              `json:"skipped"`
	Failed     int              `json:"failed"`
	Warnings   []string         `json:"warnings,omitempty"`
}

// handleDashboardCleanup dispatches the /api/cleanup/{group,agents}
// sub-paths. Registered from registerDashboardEditRoutes.
func handleDashboardCleanup(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	switch strings.TrimPrefix(r.URL.Path, "/api/cleanup/") {
	case "group":
		dashboardCleanupGroup(w, r)
	case "agents":
		dashboardCleanupAgents(w, r)
	default:
		http.Error(w, "expected POST /api/cleanup/group or /api/cleanup/agents", http.StatusNotFound)
	}
}

// dashboardCleanupGroup removes the listed confirmed-offline members
// from a single group. Body:
//
//	{
//	  "group":          "<group name>",
//	  "members":        ["<conv-id>", ...],   // the human-edited list
//	  "include_owners": false                 // strip owner rows too?
//	}
//
// An offline member that is also a group owner is skipped unless
// include_owners is set — matching the modal's default, where owner
// rows stay unchecked until the human opts in. When include_owners is
// set, the owner row is dropped alongside the membership row.
func dashboardCleanupGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Group         string   `json:"group"`
		Members       []string `json:"members"`
		IncludeOwners bool     `json:"include_owners"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.Group = strings.TrimSpace(body.Group)
	if body.Group == "" {
		http.Error(w, "missing group", http.StatusBadRequest)
		return
	}
	g, err := db.GetAgentGroupByName(body.Group)
	if err != nil {
		http.Error(w, "group lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if g == nil {
		http.Error(w, "no such group "+body.Group, http.StatusNotFound)
		return
	}

	// Snapshot the owner roster once so each conv-id is an O(1) lookup
	// and the ownerless-group warning can compare before/after.
	owners, err := db.ListAgentGroupOwners(g.ID)
	if err != nil {
		http.Error(w, "owner lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ownerSet := make(map[string]bool, len(owners))
	for _, o := range owners {
		ownerSet[o.ConvID] = true
	}
	ownerCountBefore := len(owners)

	resp := cleanupResponse{Mode: "group", Outcomes: []cleanupOutcome{}}
	ownersRemoved := 0
	for _, raw := range body.Members {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		convID, ok := resolveCleanupConv(raw)
		out := cleanupOutcome{AgentID: peerAgentID(convID), ConvID: convID, Title: cleanupTitle(convID)}
		switch {
		case !ok:
			out.Result, out.Detail = "skipped", "could not resolve conv-id"
			resp.Skipped++
		case isConvOnline(convID):
			// The tmux double-check: snapshot said offline, reality
			// says otherwise — leave it alone.
			out.Result, out.Detail = "skipped", "still online — tmux session is alive"
			resp.Skipped++
		default:
			isOwner := ownerSet[convID]
			member, ferr := db.FindMemberInGroup(g.ID, convID)
			switch {
			case ferr != nil:
				out.Result, out.Detail = "failed", "membership lookup: "+ferr.Error()
				resp.Failed++
			case member == nil && !isOwner:
				out.Result, out.Detail = "skipped", "not in group "+g.Name
				resp.Skipped++
			case isOwner && !body.IncludeOwners:
				out.Result, out.Detail = "skipped", "group owner — enable \"include offline owners\" to remove"
				resp.Skipped++
			default:
				if rerr := removeConvFromGroup(g.ID, convID, isOwner, member != nil); rerr != nil {
					out.Result, out.Detail = "failed", rerr.Error()
					resp.Failed++
				} else {
					out.Result = "removed"
					resp.Removed++
					if isOwner {
						ownersRemoved++
						out.Detail = "owner status also removed"
					}
				}
			}
		}
		resp.Outcomes = append(resp.Outcomes, out)
	}

	if ownerCountBefore > 0 && ownersRemoved >= ownerCountBefore {
		resp.Warnings = append(resp.Warnings,
			fmt.Sprintf("group %q now has no owners — grant one so the manager pattern still works", g.Name))
	}
	writeJSON(w, http.StatusOK, resp)
}

// dashboardCleanupAgents operates on a human-picked set of
// conversations across the whole system — active agents, retired
// agents and plain (never-enrolled) conversations alike. Body:
//
//	{
//	  "agents":           ["<conv-id>", ...], // the human-edited list
//	  "mode":             "unjoin",           // unjoin | retire | delete | reinstate
//	  "include_owners":   false,              // (unjoin) strip owner rows?
//	  "include_online":   false,              // act on still-running sessions too?
//	  "delete_worktrees": false,              // (delete) also remove git worktrees?
//	  "shutdown":         true                // (retire) also soft-stop the session?
//	}
//
// Four tiers:
//
//   - unjoin — the agent is removed from every group it belongs to;
//     it stays an agent (its enrollment is untouched) and its
//     conversation history stays on disk.
//   - retire — the agent is demoted to a plain conversation:
//     retireAgentConv unjoins all groups, revokes every permission and
//     sudo grant, and flips the enrollment bit. The .jsonl is left
//     intact and the agent can be reinstated. The non-destructive
//     soft-delete. Unless shutdown is false, a retired agent whose
//     tmux session is still alive is also soft-exited (force=false).
//   - delete — a full purge via conv.DeleteAgentAllGenerations (the
//     per-agent delete button's path), which unlinks the .jsonl and cascades
//     every group/owner/perm row, and for an agent's head generation sweeps
//     its predecessor generations too (JOH-26 PR3d). Irreversible. Works on
//     any conversation, agent or not.
//   - reinstate — the inverse of retire: db.ReinstateAgent clears the
//     retired flag on a retired enrollment, returning it to the active
//     roster. Groups and grants are NOT restored (retire stripped
//     them). A no-op skip for anything that isn't a retired agent.
//
// A target whose tier doesn't apply to it is reported skipped, never
// failed — so a mixed-category selection degrades gracefully:
// retire/unjoin skip non-agents, reinstate skips non-retired convs.
//
// include_online lets a tier act on a conv whose tmux session is still
// alive instead of skipping it; delete force-stops the session first.
// reinstate ignores liveness regardless. The legacy boolean `delete`
// is still honoured when `mode` is absent (delete=true → "delete",
// else → "unjoin") so an older dashboard build keeps working.
// delete_worktrees (delete mode only) also removes each purged agent's
// git worktree — skipping the repo's main worktree and any worktree a
// surviving agent still works in. shutdown (retire mode only) is a
// *bool defaulting to true: an omitted field keeps the shutdown-ON
// default, so an older dashboard build that never sends it still
// soft-stops retired agents' sessions.
func dashboardCleanupAgents(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agents          []string `json:"agents"`
		Mode            string   `json:"mode"`
		Delete          bool     `json:"delete"` // legacy — superseded by Mode
		IncludeOwners   bool     `json:"include_owners"`
		IncludeOnline   bool     `json:"include_online"`
		DeleteWorktrees bool     `json:"delete_worktrees"`
		Shutdown        *bool    `json:"shutdown"` // (retire) nil → default ON
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Resolve the tier: explicit mode wins; fall back to the legacy
	// boolean for older dashboard builds.
	mode := strings.TrimSpace(body.Mode)
	if mode == "" {
		if body.Delete {
			mode = "delete"
		} else {
			mode = "unjoin"
		}
	}
	switch mode {
	case "unjoin", "retire", "delete", "reinstate":
	default:
		http.Error(w, "invalid mode "+mode+" (expected unjoin, retire, delete or reinstate)", http.StatusBadRequest)
		return
	}

	// Pre-pass: resolve every target and decide which will actually be
	// deleted. The worktree "shared with a survivor" check needs the
	// exact set going away — an online target that gets skipped still
	// exists, so its worktree must still count as in-use.
	type cleanupTarget struct {
		convID string
		ok     bool
		online bool
	}
	targets := make([]cleanupTarget, 0, len(body.Agents))
	willDelete := map[string]bool{}
	for _, raw := range body.Agents {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		convID, ok := resolveCleanupConv(raw)
		online := ok && isConvOnline(convID)
		targets = append(targets, cleanupTarget{convID: convID, ok: ok, online: online})
		// An online target is deleted only when include_online lifts the
		// skip — otherwise it survives, so its worktree still counts as
		// in-use for the survivor check.
		if mode == "delete" && ok && (!online || body.IncludeOnline) {
			willDelete[convID] = true
		}
	}
	// Worktree roots still claimed by agents that survive this cleanup
	// — resolved once, only when worktree removal was actually asked
	// for, so the no-worktree path pays nothing.
	var survivorRoots map[string]bool
	if mode == "delete" && body.DeleteWorktrees {
		survivorRoots = otherAgentWorktreeRoots(willDelete)
	}

	// retireShutdown (retire tier only): whether a successfully-retired
	// agent's still-live session should also be soft-stopped. nil → ON,
	// the documented default — an older dashboard build omits the field.
	retireShutdown := body.Shutdown == nil || *body.Shutdown

	resp := cleanupResponse{Mode: "agents", Outcomes: []cleanupOutcome{}}
	// Group IDs whose owner roster this pass may have emptied — swept
	// once at the end for the ownerless-group warning.
	ownerless := map[int64]bool{}

	for _, tg := range targets {
		// Resolve the title up-front: the delete path wipes the
		// conv_index row, so a post-delete lookup would come back empty.
		out := cleanupOutcome{AgentID: peerAgentID(tg.convID), ConvID: tg.convID, Title: cleanupTitle(tg.convID)}
		switch {
		case !tg.ok:
			out.Result, out.Detail = "skipped", "could not resolve conv-id"
			resp.Skipped++
		case tg.online && !body.IncludeOnline && mode != "reinstate":
			// The tmux double-check: snapshot said offline (or the human
			// ticked it anyway), reality says alive — leave it untouched
			// unless the request opted in. Reinstate is non-destructive,
			// so it never blocks on liveness.
			out.Result, out.Detail = "skipped", "still online — enable \"include online sessions\" to act on it"
			resp.Skipped++
		case mode == "reinstate":
			// The inverse of retire: clear the retired flag, returning a
			// demoted agent to the active roster. Groups and grants stay
			// gone — retire stripped them.
			did, rerr := db.ReinstateAgent(tg.convID)
			switch {
			case rerr != nil:
				out.Result, out.Detail = "failed", "reinstate: "+rerr.Error()
				resp.Failed++
			case !did:
				out.Result, out.Detail = "skipped", "not a retired agent — nothing to reinstate"
				resp.Skipped++
			default:
				out.Result = "reinstated"
				out.Detail = "returned to the active roster — groups and permissions were not restored"
				resp.Reinstated++
			}
		case mode == "retire":
			// Soft-delete: demote to a plain conversation. The .jsonl
			// and conv_index row are left intact and the agent can be
			// reinstated later.
			outcome, ownerGroups, rerr := retireAgentConv(tg.convID, "human", "")
			switch {
			case rerr != nil:
				out.Result, out.Detail = "failed", "retire: "+rerr.Error()
				resp.Failed++
			case !outcome.Retired:
				out.Result, out.Detail = "skipped", "not an active agent — nothing to retire"
				resp.Skipped++
			default:
				out.Result = "retired"
				out.Groups = outcome.GroupsLeft
				out.Detail = fmt.Sprintf("demoted to a plain conversation · left %d group(s), revoked %d perm + %d sudo grant(s)",
					len(outcome.GroupsLeft), outcome.PermsRevoked, outcome.SudoRevoked)
				resp.Retired++
				for _, gid := range ownerGroups {
					ownerless[gid] = true
				}
				// Optionally soft-stop the now-retired agent's session.
				// stopOneConv is idempotent — an already-dead session
				// (the common case for an offline target) is a no-op, so
				// the detail note only appears when a pane was running.
				if retireShutdown {
					switch st := stopOneConv(tg.convID, false /* soft exit */); st.Action {
					case "soft_stopped":
						out.Detail += " · session soft-stopped"
					case "error":
						out.Detail += " · session shutdown failed: " + st.Detail
					}
				}
			}
		case mode == "delete":
			// Resolve the worktree BEFORE the purge — DeleteConvByID
			// wipes the session rows the resolution reads from.
			wt := inspectAgentWorktree(tg.convID)
			wt.Shared = wt.Path != "" && survivorRoots[wt.Path]
			// Capture owned groups before the purge so the warning
			// sweep can tell which ones lose their last owner.
			ownedBefore, _ := db.ListGroupsOwnedBy(tg.convID)
			// Best-effort stop. The conv is confirmed offline, so this
			// is normally a no-op; kept for parity with the per-agent
			// delete button, which force-kills any lingering pane.
			stopOneConv(tg.convID, true)
			// Actor-aware (JOH-26 PR3d): an agent's head-generation delete
			// also sweeps its predecessor generations' rows + .jsonl.
			counts, _, derr := conv.DeleteAgentAllGenerations(tg.convID)
			if derr != nil {
				out.Result, out.Detail = "failed", "delete: "+derr.Error()
				resp.Failed++
			} else {
				out.Result = "deleted"
				out.Detail = fmt.Sprintf("purged · dropped %d group + %d owner row(s)",
					counts.GroupMembers, counts.GroupOwners)
				if note := applyWorktreeCleanup(wt, body.DeleteWorktrees); note != "" {
					out.Detail += " · " + note
				}
				resp.Deleted++
				for _, gid := range ownedBefore {
					ownerless[gid] = true
				}
			}
		default:
			removed, kept, ownerGroups, rerr := unjoinConvFromAllGroups(tg.convID, body.IncludeOwners)
			switch {
			case rerr != nil:
				out.Result, out.Detail = "failed", rerr.Error()
				resp.Failed++
			case len(removed) == 0 && len(kept) == 0:
				out.Result, out.Detail = "skipped", "not a member of any group"
				resp.Skipped++
			case len(removed) == 0:
				out.Result = "skipped"
				out.Detail = "only owns group(s) — enable \"include offline owners\" to remove"
				out.Groups = kept
				resp.Skipped++
			default:
				out.Result = "removed"
				out.Groups = removed
				out.Detail = "removed from " + strings.Join(removed, ", ")
				if len(kept) > 0 {
					out.Detail += " · kept in " + strings.Join(kept, ", ") + " (owner)"
				}
				resp.Removed++
				for _, gid := range ownerGroups {
					ownerless[gid] = true
				}
			}
		}
		resp.Outcomes = append(resp.Outcomes, out)
	}

	resp.Warnings = append(resp.Warnings, warnOwnerlessGroups(ownerless)...)
	writeJSON(w, http.StatusOK, resp)
}

// removeConvFromGroup drops convID's membership and/or owner row from
// one group. isOwner / isMember tell it which rows actually exist so
// it doesn't issue no-op deletes. The owner row goes first so a mid-op
// failure can't leave an owner with no membership.
func removeConvFromGroup(groupID int64, convID string, isOwner, isMember bool) error {
	if isOwner {
		if _, err := db.RemoveAgentGroupOwner(groupID, convID); err != nil {
			return fmt.Errorf("remove owner: %w", err)
		}
	}
	if isMember {
		if err := db.RemoveAgentGroupMember(groupID, convID); err != nil {
			return fmt.Errorf("remove member: %w", err)
		}
	}
	return nil
}

// unjoinConvFromAllGroups removes convID from every group it belongs
// to. A group convID OWNS is left untouched unless includeOwners is
// set, in which case the owner row is dropped alongside the
// membership. Returns the group names it was removed from, the names
// it was kept in (owner, includeOwners=false), and the IDs of groups
// whose owner roster was touched (for the ownerless-group warning).
func unjoinConvFromAllGroups(convID string, includeOwners bool) (removed, kept []string, ownerGroups []int64, err error) {
	groups, err := db.ListGroupsForConv(convID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list groups: %w", err)
	}
	for _, g := range groups {
		owner, oerr := db.IsAgentGroupOwner(g.ID, convID)
		if oerr != nil {
			return removed, kept, ownerGroups, fmt.Errorf("owner check for %s: %w", g.Name, oerr)
		}
		if owner && !includeOwners {
			kept = append(kept, g.Name)
			continue
		}
		if rerr := removeConvFromGroup(g.ID, convID, owner, true); rerr != nil {
			return removed, kept, ownerGroups, fmt.Errorf("%s: %w", g.Name, rerr)
		}
		removed = append(removed, g.Name)
		if owner {
			ownerGroups = append(ownerGroups, g.ID)
		}
	}
	sort.Strings(removed)
	sort.Strings(kept)
	return removed, kept, ownerGroups, nil
}

// warnOwnerlessGroups returns a warning string for every group in the
// set that has no owners left. Best-effort: a lookup error for one
// group is silently skipped rather than failing the whole cleanup.
func warnOwnerlessGroups(groupIDs map[int64]bool) []string {
	var warnings []string
	for gid := range groupIDs {
		owners, err := db.ListAgentGroupOwners(gid)
		if err != nil || len(owners) > 0 {
			continue
		}
		name := fmt.Sprintf("#%d", gid)
		if g, gerr := db.GetAgentGroupByID(gid); gerr == nil && g != nil {
			name = fmt.Sprintf("%q", g.Name)
		}
		warnings = append(warnings,
			fmt.Sprintf("group %s now has no owners — grant one so the manager pattern still works", name))
	}
	sort.Strings(warnings)
	return warnings
}

// resolveCleanupConv normalises a cleanup target into a canonical
// conv-id. The dashboard sends snapshot conv-ids, which normally
// resolve cleanly through agent.ResolveSelector; an orphan whose
// conv_index row is already gone falls back to the UUID-shape check
// — the same defence-in-depth handleDashboardAgentsAPI uses for its
// raw-conv-id delete path. ok=false means the input matched nothing
// and is not even UUID-shaped, so the caller skips it.
func resolveCleanupConv(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	if res, _, err := agent.ResolveSelector(s); err == nil {
		return res.ConvID, true
	}
	if looksLikeConvID(s) {
		return s, true
	}
	return s, false
}

// cleanupTitle resolves a conv-id to its display title for the
// per-item result rows. Best-effort — an orphan whose conv_index row
// is already gone comes back as "" and the dashboard falls back to the
// short conv-id.
func cleanupTitle(convID string) string {
	if row := agent.FreshConvRowResolved(convID); row != nil {
		return agent.DisplayTitle(row)
	}
	return ""
}
