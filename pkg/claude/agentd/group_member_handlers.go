package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type memberJSON struct {
	// AgentID is the member's stable actor key — the canonical ID the
	// agent CLI leads with; ConvID is the live generation behind it. (The
	// dashboard still keys on conv_id — a separate follow-up.)
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	// CreatedAt is the earliest known existence timestamp: actor birth
	// (agents.created_at) or, for a late-enrolled legacy actor, the older
	// conv_index.Created — see agent.MemberCreated. Emitted as UTC RFC3339Nano,
	// empty when unknown. The dashboard renders it as a relative
	// "Age", and it is the default sort key (newest first).
	CreatedAt string `json:"created_at,omitempty"`
	Role      string `json:"role,omitempty"`
	Descr     string `json:"descr,omitempty"`
	// agentLocationView carries `branch` (current branch) plus the
	// startup/current directory split — see agent_location_view.go.
	agentLocationView
	Online bool `json:"online"`
	Owner  bool `json:"owner,omitempty"`
}

// sortMembersByAge orders a group-member listing newest-first by the Age
// timestamp (earliest known actor/conversation existence — see
// agent.MemberCreated).
// Values are parsed as RFC3339 instants before comparison; their textual
// precision or zone never controls ordering. It is the default ordering for
// every group listing, shared by the CLI (`tclaude agent groups members`, which
// renders the JSON below) and the browser dashboard. Blank or unparseable times
// sort last; conv_id breaks equal-time ties so the order is deterministic — the
// previous joined_at order left owner-only rows, appended from a map, in random
// iteration order.
func sortMembersByAge[T any](items []T, created func(T) string, convID func(T) string) {
	sort.SliceStable(items, func(i, j int) bool {
		ti, errI := time.Parse(time.RFC3339Nano, created(items[i]))
		tj, errJ := time.Parse(time.RFC3339Nano, created(items[j]))
		knownI, knownJ := errI == nil, errJ == nil
		if knownI != knownJ {
			return knownI // known creation times before unknown
		}
		if knownI && !ti.Equal(tj) {
			return ti.After(tj) // newest first
		}
		return convID(items[i]) < convID(items[j])
	})
}

func handleGroupMembersList(w http.ResponseWriter, _ *http.Request, g *db.AgentGroup) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// One tmux ls for the listing — per-member online checks below are
	// map lookups against this snapshot.
	aliveSessions, _ := cachedLiveTmuxSessions()
	// Pre-load the owner set so we can tag any members who are also
	// owners. Distinct-from-members owners are emitted as their own
	// rows below so the list stays comprehensive.
	ownerSet := map[string]bool{}
	if owners, err := db.ListAgentGroupOwners(g.ID); err == nil {
		for _, o := range owners {
			ownerSet[o.ConvID] = true
		}
	}
	memberSet := map[string]bool{}
	out := make([]memberJSON, 0, len(members))
	for _, m := range members {
		memberSet[m.ConvID] = true
		out = append(out, memberJSON{
			AgentID:           peerAgentID(m.ConvID),
			ConvID:            m.ConvID,
			Title:             agent.FreshTitle(m.ConvID),
			CreatedAt:         agent.MemberCreated(m.ConvID),
			Role:              m.Role,
			Descr:             m.Descr,
			agentLocationView: locationView(m.ConvID),
			Online:            isConvOnlineIn(m.ConvID, aliveSessions),
			Owner:             ownerSet[m.ConvID],
		})
	}
	// Surface owners who aren't members so the list is comprehensive.
	// They get an "owner" role tag and no descr (that is a
	// member-scoped field).
	for ownerConv := range ownerSet {
		if memberSet[ownerConv] {
			continue
		}
		out = append(out, memberJSON{
			AgentID:           peerAgentID(ownerConv),
			ConvID:            ownerConv,
			Title:             agent.FreshTitle(ownerConv),
			CreatedAt:         agent.MemberCreated(ownerConv),
			Role:              "owner",
			agentLocationView: locationView(ownerConv),
			Online:            isConvOnlineIn(ownerConv, aliveSessions),
			Owner:             true,
		})
	}
	sortMembersByAge(out,
		func(m memberJSON) string { return m.CreatedAt },
		func(m memberJSON) string { return m.ConvID })
	writeJSON(w, http.StatusOK, out)
}

// groupContextEntry is one member's context-window state in the
// group-wide listing (GET /v1/groups/{name}/context). HasSnapshot is
// false for a member whose statusline hook has never fired (a
// freshly-spawned agent pre-first-response, or one that never ran under
// tclaude) — the caller renders that as "—" rather than a misleading 0%.
// Model can be non-empty even when HasSnapshot is false: it's written on
// every statusline render (UpdateSessionModel), including the empty
// pre-first-response ones that carry no context figures.
type groupContextEntry struct {
	// AgentID is the member's stable actor key — the canonical ID the CLI
	// leads with; ConvID is the live generation behind it (kept as the
	// snapshot/hover). "" when the conv is not a known agent.
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	Role    string `json:"role,omitempty"`
	Online  bool   `json:"online"`
	// Status is the member's settled hook status (idle / working /
	// awaiting_* / exited …) — the SAME value stateForConvIn feeds the
	// dashboard, so a caller classifying force liveness (JOH-346's
	// task-force status) agrees with the dashboard force block's rollup:
	// offline → dead, online+idle → idle, anything else in flight →
	// working. Empty when no session row exists for the conv.
	Status              string  `json:"status,omitempty"`
	HasSnapshot         bool    `json:"has_snapshot"`
	ContextPct          float64 `json:"context_pct"`
	TokensInput         int64   `json:"tokens_input"`
	TokensOutput        int64   `json:"tokens_output"`
	ContextWindowSize   int64   `json:"context_window_size"`
	ContextWindowMax    int64   `json:"context_window_max,omitempty"`
	ContextWindowSource string  `json:"context_window_source,omitempty"`
	Model               string  `json:"model,omitempty"`
}

// handleGroupContext returns the context-window state of every member of
// a group in one read — the lead-watching-workers view, so a manager can
// spot anyone approaching their context limit at a glance. Read-only
// (GET only). Gated like the per-target read: the human always passes,
// an agent passes with the agent.context-info slug or by owning this
// group (owner bypass — the lead is normally the owner). All members'
// snapshots ride on the same already-persisted sessions rows the
// dashboard reads; no new data source.
func handleGroupContext(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if _, ok := requireGroupContextAccess(w, r, g, members); !ok {
		return
	}
	// One tmux ls for the whole listing; the per-member snapshot read
	// resolves liveness against this set, not a per-row subprocess.
	shared, ok := requestStatusSnapshot(w, r, nil)
	if !ok {
		return
	}
	aliveSessions := shared.alive
	out := make([]groupContextEntry, 0, len(members))
	for _, m := range members {
		// The shared gather uses the SAME canonical reader as the
		// dashboard snapshot uses, so this endpoint's context figures AND its
		// settled status (idle / working / awaiting_* / exited, incl. the
		// subagent-idle settle) agree with the dashboard force block that
		// classifies liveness on them (JOH-346). A populated snapshot implies
		// a session row, so has_snapshot collapses to "any real context figure"
		// — the separate hasSession gate was redundant.
		st := shared.states[m.ConvID]
		out = append(out, groupContextEntry{
			AgentID:             peerAgentID(m.ConvID),
			ConvID:              m.ConvID,
			Title:               agent.FreshTitle(m.ConvID),
			Role:                m.Role,
			Online:              isConvOnlineIn(m.ConvID, aliveSessions),
			Status:              st.Status,
			HasSnapshot:         agentStateHasSnapshot(st),
			ContextPct:          st.ContextPct,
			TokensInput:         st.TokensInput,
			TokensOutput:        st.TokensOutput,
			ContextWindowSize:   st.ContextWindowSize,
			ContextWindowMax:    st.ContextWindowMax,
			ContextWindowSource: st.ContextWindowSource,
			Model:               st.Model,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// snapshotPopulated reports whether a context snapshot carries any real
// figure — i.e. the statusline hook has fired at least once. An all-zero
// snapshot is the "not reported yet" sentinel (the same shape a missing
// row produces); distinguishing it lets the caller mark "hook never
// fired" separately from a genuine 0%.
func snapshotPopulated(s db.ContextSnapshot) bool {
	return s.ContextPct != 0 || s.TokensInput != 0 || s.TokensOutput != 0 || s.ContextWindowSize != 0
}

// agentStateHasSnapshot applies snapshotPopulated to the context figures an
// stateForConvIn read carries — true once the statusline hook has reported at
// least one real figure. A populated snapshot implies a live-or-past session
// row, so this needs no separate hasSession gate. Delegates to snapshotPopulated
// so the "is this real data" rule lives in exactly one place.
func agentStateHasSnapshot(s agentState) bool {
	return snapshotPopulated(db.ContextSnapshot{
		ContextPct:        s.ContextPct,
		TokensInput:       s.TokensInput,
		TokensOutput:      s.TokensOutput,
		ContextWindowSize: s.ContextWindowSize,
	})
}

// requireGroupContextAccess gates the group-wide context read through the
// global agent.context-info slug OR the dedicated groups.members.context-info
// sibling. The sibling must cover every active group containing every returned
// member; ownership contributes it only for owned groups.
func requireGroupContextAccess(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, members []*db.AgentGroupMember) (string, bool) {
	affected := make([]string, 0, len(members))
	caller := ""
	if p := peerFromContext(r.Context()); classify(p) == classAgent {
		caller = p.ConvID
	}
	for _, member := range members {
		if sameActor(caller, member.ConvID) {
			continue
		}
		affected = append(affected, member.ConvID)
	}
	ctx := ActionContext{
		Group:                   g.Name,
		structuralGroup:         g.Name,
		affectedConvs:           affected,
		bulkGroupMemberCoverage: true,
		alternatePermission:     PermGroupsMembersContextInfo,
	}
	return requirePermission(w, r, PermAgentContextInfo, ctx)
}

type ownerJSON struct {
	// AgentID is the owner's stable actor key — the canonical ID the CLI
	// leads with; ConvID is the live generation behind it.
	AgentID   string `json:"agent_id,omitempty"`
	ConvID    string `json:"conv_id"`
	Title     string `json:"title"`
	Online    bool   `json:"online"`
	GrantedAt string `json:"granted_at,omitempty"`
	GrantedBy string `json:"granted_by,omitempty"`
}

// handleGroupOwnersList returns the owner set for the group. Owners
// can message members (and multicast) without being members of the
// group themselves.
func handleGroupOwnersList(w http.ResponseWriter, _ *http.Request, g *db.AgentGroup) {
	owners, err := db.ListAgentGroupOwners(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// One tmux ls for the listing — per-owner online checks below are
	// map lookups against this snapshot.
	aliveSessions, _ := cachedLiveTmuxSessions()
	out := make([]ownerJSON, 0, len(owners))
	for _, o := range owners {
		entry := ownerJSON{
			AgentID: o.AgentID,
			ConvID:  o.ConvID,
			Title:   agent.FreshTitle(o.ConvID),
			Online:  isConvOnlineIn(o.ConvID, aliveSessions),
		}
		if !o.GrantedAt.IsZero() {
			entry.GrantedAt = o.GrantedAt.Format(time.RFC3339)
		}
		entry.GrantedBy = o.GrantedBy
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGroupOwnersAdd grants ownership of g to a conv. Permission-
// gated on groups.owners.manage (default human-only). The granted_by column
// records "" for human-issued grants and the agent's conv-id for
// agent-issued ones.
func handleGroupOwnersAdd(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	grantedBy, ok := requireGroupPermission(w, r, PermGroupsOwnersManage, g)
	if !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	convID, fail := addGroupOwner(r, g, auditedCaller(grantedBy, PermGroupsOwnersManage))
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group":    g.Name,
		"agent_id": peerAgentID(convID),
		"conv_id":  convID,
	})
}

// handleGroupOwnersRemove revokes ownership. 404 when convID wasn't
// an owner — distinct from "no such group" (which the dispatcher
// catches). Permission-gated on groups.owners.manage.
func handleGroupOwnersRemove(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, convSelector string) {
	if _, ok := requireGroupPermission(w, r, PermGroupsOwnersManage, g); !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	if fail := removeGroupOwner(g, convSelector); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleGroupMembersAdd(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if _, ok := requireGroupPermission(w, r, PermGroupsMembersAdd, g); !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	if !routeMembershipMutationAllowed(w, g) {
		return
	}
	convID, fail := addGroupMember(r, g)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": convID})
}

// handleGroupMembersUpdate patches role/descr on an existing member.
// Only fields explicitly present in the request body are touched — pass
// `null` (or omit) to leave a field unchanged. Gated on groups.members.update.
func handleGroupMembersUpdate(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, convSelector string) {
	if _, ok := requireGroupPermission(w, r, PermGroupsMembersUpdate, g); !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	convID, fail := updateGroupMember(r, g, convSelector)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": convID})
}

func handleGroupMembersRemove(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, convSelector string) {
	if _, ok := requireGroupPermission(w, r, PermGroupsMembersRemove, g); !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	if !routeMembershipMutationAllowed(w, g) {
		return
	}
	if fail := removeGroupMember(g, convSelector); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGroupMembersMove moves a member from g to another group in one step,
// carrying its role and description. Body: {to, keep_sandbox?,
// keep_environment?}. By default the agent's group-sourced sandbox profile
// and environment follow it to the target group from its next relaunch; the
// keep_* flags leave either on the source group instead (see
// db.MoveAgentGroupMember). Gated like the remove-then-add it replaces:
// groups.members.remove on the source and groups.members.add on the target.
func handleGroupMembersMove(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, convSelector string) {
	if _, ok := requireGroupPermission(w, r, PermGroupsMembersRemove, g); !ok {
		return
	}
	if !requireGroupActive(w, g) || !routeMembershipMutationAllowed(w, g) {
		return
	}
	var body struct {
		To              string `json:"to"`
		KeepSandbox     bool   `json:"keep_sandbox,omitempty"`
		KeepEnvironment bool   `json:"keep_environment,omitempty"`
		KeepPermissions bool   `json:"keep_permissions,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	// Kept permissions outlive the membership that showed them, so only the
	// operator can choose them; an agent manager moves with the defaults.
	if body.KeepPermissions && classify(peerFromContext(r.Context())) != classHuman {
		writeError(w, http.StatusForbidden, "permission_denied", "keep_permissions is operator-only")
		return
	}
	if body.To == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "to is required")
		return
	}
	target, err := db.GetAgentGroupByName(body.To)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no such group %q", body.To))
		return
	}
	if target.ID == g.ID {
		writeError(w, http.StatusBadRequest, "invalid_arg", "source and target group are the same")
		return
	}
	if _, ok := requireGroupPermission(w, r, PermGroupsMembersAdd, target); !ok {
		return
	}
	if !requireGroupActive(w, target) || !routeMembershipMutationAllowed(w, target) {
		return
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	moved, err := db.MoveAgentGroupMember(res.ConvID, g.ID, target.ID, db.MoveOptions{
		KeepSandbox: body.KeepSandbox, KeepEnvironment: body.KeepEnvironment,
		KeepPermissions: body.KeepPermissions, By: "operator",
	})
	if errors.Is(err, db.ErrNotGroupMember) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("conv %s is not a member of %q", res.ConvID, g.Name))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	sandboxSource, environmentSource := groupMoveSources(moved, g, target)
	notifyGroupMove(res.ConvID, g, target, sandboxSource, environmentSource, body.KeepPermissions)
	writeJSON(w, http.StatusOK, map[string]any{
		"conv_id": res.ConvID, "from": g.Name, "to": target.Name,
		"sandbox": sandboxSource, "environment": environmentSource,
		"kept_permissions": body.KeepPermissions,
	})
}

// groupMoveSources describes where a moved agent's sandbox profile and group
// environment now come from, as the move actually left them (a keep that did
// not apply, such as a setting that never came from the source group, is not
// reported as kept).
func groupMoveSources(moved db.MoveResult, from, to *db.AgentGroup) (sandbox, environment string) {
	if !moved.Tracked {
		const follows = "follows your group memberships at your next launch"
		return follows, follows
	}
	describe := func(groupID int64) string {
		switch groupID {
		case 0:
			return "none (no group)"
		case to.ID:
			return fmt.Sprintf("now comes from %q", to.Name)
		case from.ID:
			return fmt.Sprintf("stays on %q", from.Name)
		}
		if g, err := db.GetAgentGroupByID(groupID); err == nil && g != nil {
			return fmt.Sprintf("comes from %q", g.Name)
		}
		return fmt.Sprintf("comes from group #%d", groupID)
	}
	return describe(moved.SandboxGroupID), describe(moved.EnvironmentGroupID)
}

// keptPermissionGroupNames names the groups whose permissions agentID kept
// after being moved out of them (db.MoveOptions.KeepPermissions).
func keptPermissionGroupNames(agentID string) []string {
	if agentID == "" {
		return nil
	}
	ids, err := db.ListRetainedPermissionGroups(agentID)
	if err != nil {
		slog.Warn("whoami: kept permission groups unreadable", "agent", agentID, "error", err)
		return nil
	}
	var names []string
	for _, id := range ids {
		if g, err := db.GetAgentGroupByID(id); err == nil && g != nil {
			names = append(names, g.Name)
		}
	}
	return names
}

// handleGroupKeptPermissionsList lists the agents that kept g's permissions
// after leaving it, so whoever edits g's grants can see who else receives
// them. Open, like the member listing.
func handleGroupKeptPermissionsList(w http.ResponseWriter, _ *http.Request, g *db.AgentGroup) {
	convs, err := db.ListRetainedPermissionHolders(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	type holder struct {
		AgentID string `json:"agent_id,omitempty"`
		ConvID  string `json:"conv_id"`
		Title   string `json:"title"`
	}
	out := make([]holder, 0, len(convs))
	for _, conv := range convs {
		out = append(out, holder{AgentID: peerAgentID(conv), ConvID: conv, Title: agent.FreshTitle(conv)})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGroupKeptPermissionsDrop ends the permissions an agent kept from g
// when it was moved out of it. Dropping only narrows access, so it is gated
// like removing a member of g.
func handleGroupKeptPermissionsDrop(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, convSelector string) {
	if _, ok := requireGroupPermission(w, r, PermGroupsMembersRemove, g); !ok {
		return
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	agentID, err := db.AgentIDForConv(res.ConvID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	dropped := false
	if agentID != "" {
		if dropped, err = db.DropRetainedGroupPermissions(agentID, g.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
	}
	if !dropped {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("conv %s kept no permissions from %q", res.ConvID, g.Name))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": res.ConvID, "group": g.Name})
}

// notifyGroupMove tells a moved agent what changed, so the new group's
// permissions and, after its next relaunch, its sandbox are not a surprise.
func notifyGroupMove(convID string, from, to *db.AgentGroup, sandboxSource, environmentSource string, keptPermissions bool) {
	permissions := fmt.Sprintf("%q's apply now; %q's no longer do", to.Name, from.Name)
	if keptPermissions {
		permissions = fmt.Sprintf("%q's apply now, and the operator kept %q's for you too", to.Name, from.Name)
	}
	body := fmt.Sprintf("You were moved from group %q to group %q.\n\n"+
		"- Group permissions: %s.\n"+
		"- Sandbox profile: %s.\n"+
		"- Group environment variables: %s.\n\n"+
		"Permission changes apply immediately. Sandbox and environment changes take effect when you are next resumed or restarted; this process keeps its "+
		"current sandbox until then. Your own per-agent permissions and overrides are unchanged. "+
		"Run `tclaude agent whoami` to see your groups.",
		from.Name, to.Name, permissions, sandboxSource, environmentSource)
	if _, err := queueAgentMessage(&db.AgentMessage{
		GroupID: to.ID, ToConv: convID, Subject: "Moved to group " + to.Name, Body: body,
	}); err != nil {
		slog.Warn("group move: notify agent failed", "conv", short8(convID), "error", err)
	}
}
