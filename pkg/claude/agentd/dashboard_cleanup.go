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
//	/api/cleanup/agents  — operate on confirmed-offline agents across
//	                       the whole system: strip them from every
//	                       group, or (delete=true) permanently delete
//	                       them. Powers the Agents-tab cleanup button
//	                       and the Groups-tab "clean up all groups" one.
//
// Cleanup is human-only by construction: these routes live on the
// loopback dashboard server and are gated by the dashboard cookie +
// Origin pin, exactly like every other /api mutation. Agents talk to
// /v1 and have no path here.
//
// Safety model. The browser sends an explicit, human-edited list of
// conv-ids — the daemon never trusts the "offline" label the snapshot
// rendered. Every conv-id is re-checked against live tmux
// (isConvOnline) at execute time and any that turns out still-alive is
// skipped, not touched. A few races remain unavoidable (an agent could
// spawn a pane in the window between the check and the DB write); the
// session reaper resolves those on its next sweep. The endpoints are
// idempotent — re-running a cleanup over already-cleaned conv-ids just
// reports them as skipped.

// cleanupOutcome is the per-conv-id result of one cleanup pass. The
// dashboard renders these back into the modal so the human sees
// exactly what happened, including every skip and its reason.
type cleanupOutcome struct {
	ConvID string   `json:"conv_id"`
	Title  string   `json:"title,omitempty"`
	Result string   `json:"result"`           // removed | deleted | skipped | failed
	Detail string   `json:"detail,omitempty"` // human-readable reason
	Groups []string `json:"groups,omitempty"` // groups touched (agents mode)
}

// cleanupResponse is the wire shape returned by both cleanup
// sub-paths. Outcomes is always non-nil so the dashboard can .map()
// over it unconditionally.
type cleanupResponse struct {
	Mode     string           `json:"mode"`
	Outcomes []cleanupOutcome `json:"outcomes"`
	Removed  int              `json:"removed"`
	Deleted  int              `json:"deleted"`
	Skipped  int              `json:"skipped"`
	Failed   int              `json:"failed"`
	Warnings []string         `json:"warnings,omitempty"`
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
		out := cleanupOutcome{ConvID: convID, Title: cleanupTitle(convID)}
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

// dashboardCleanupAgents operates on confirmed-offline agents across
// the whole system. Body:
//
//	{
//	  "agents":         ["<conv-id>", ...],  // the human-edited list
//	  "delete":         false,               // false: unjoin all groups
//	                                         // true:  permanently delete
//	  "include_owners": false                // (delete=false) strip owner rows?
//	}
//
// delete=false is the Groups-tab "clean up all groups" default: the
// agent is removed from every group it belongs to but its
// conversation history is left on disk. delete=true is the Agents-tab
// behaviour: a full purge via conv.DeleteConvByID (the same path the
// per-agent delete button uses), which cascades group/owner/perm rows.
func dashboardCleanupAgents(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agents        []string `json:"agents"`
		Delete        bool     `json:"delete"`
		IncludeOwners bool     `json:"include_owners"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	resp := cleanupResponse{Mode: "agents", Outcomes: []cleanupOutcome{}}
	// Group IDs whose owner roster this pass may have emptied — swept
	// once at the end for the ownerless-group warning.
	ownerless := map[int64]bool{}

	for _, raw := range body.Agents {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		convID, ok := resolveCleanupConv(raw)
		// Resolve the title up-front: the delete path wipes the
		// conv_index row, so a post-delete lookup would come back empty.
		out := cleanupOutcome{ConvID: convID, Title: cleanupTitle(convID)}
		switch {
		case !ok:
			out.Result, out.Detail = "skipped", "could not resolve conv-id"
			resp.Skipped++
		case isConvOnline(convID):
			out.Result, out.Detail = "skipped", "still online — tmux session is alive"
			resp.Skipped++
		case body.Delete:
			// Capture owned groups before the purge so the warning
			// sweep can tell which ones lose their last owner.
			ownedBefore, _ := db.ListGroupsOwnedBy(convID)
			// Best-effort stop. The conv is confirmed offline, so this
			// is normally a no-op; kept for parity with the per-agent
			// delete button, which force-kills any lingering pane.
			stopOneConv(convID, true)
			counts, derr := conv.DeleteConvByID(convID)
			if derr != nil {
				out.Result, out.Detail = "failed", "delete: "+derr.Error()
				resp.Failed++
			} else {
				out.Result = "deleted"
				out.Detail = fmt.Sprintf("purged · dropped %d group + %d owner row(s)",
					counts.GroupMembers, counts.GroupOwners)
				resp.Deleted++
				for _, gid := range ownedBefore {
					ownerless[gid] = true
				}
			}
		default:
			removed, kept, ownerGroups, rerr := unjoinConvFromAllGroups(convID, body.IncludeOwners)
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
