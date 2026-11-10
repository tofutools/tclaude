package agentd

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/renametitle"
)

// --- /v1/info ---

// handleInfo returns daemon-wide constants the CLI needs to discover
// at runtime — currently just the popup base URL so `tclaude agent
// dashboard` can open it without hard-coding the random port.
//
// Open to anyone: no identity required, no permission gate. Loopback
// URLs aren't sensitive on their own; the auth-gated endpoints
// (popup approve, dashboard /api) sit behind cookies.
func handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	// "processes" and "triggers" are always true. They once projected the
	// retired features.processes / features.triggers flags; an older CLI
	// still probes "processes" before `process templates ls`, so they stay.
	writeJSON(w, http.StatusOK, map[string]any{
		"popup_base_url": popupBaseURL,
		"idempotency":    "v1",
		"processes":      true,
		"triggers":       true,
		"proxy":          gitProxyRoutesEnabled(r),
	})
}

// --- /v1/whoami ---

type whoamiResp struct {
	FederationPresence *db.AgentFederationPresence `json:"federation_presence,omitempty"`
	Predecessor        *db.FederationMoveLink      `json:"predecessor,omitempty"`
	IsHuman            bool                        `json:"is_human"`
	AgentID            string                      `json:"agent_id,omitempty"` // stable actor key — the canonical identity
	ConvID             string                      `json:"conv_id,omitempty"`  // live generation behind it (rotates)
	Title              string                      `json:"title,omitempty"`
	Groups             []string                    `json:"groups,omitempty"`
	ActiveGroups       []string                    `json:"active_groups,omitempty"` // current memberships eligible for an implicit spawn target
	// Phases lists the advisory process phase (JOH-242) of each group the
	// caller is in that HAS a process — one "<group>: phase <n>/<m>: <name>"
	// line per such group. Omitted when no group the caller is in has a
	// process (absence = feature off, degrade everywhere).
	Phases []string `json:"phases,omitempty"`
	// PendingMorph is a requested morph of the caller waiting for its turn to
	// end (tclaude agent morph).
	PendingMorph *db.AgentPendingMorph `json:"pending_morph,omitempty"`
	// KeptPermissionGroups names the groups whose group permissions the
	// caller kept when the operator moved it out of them.
	KeptPermissionGroups []string `json:"kept_permission_groups,omitempty"`
}

func handleWhoami(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classHuman:
		writeJSON(w, http.StatusOK, whoamiResp{IsHuman: true})
		return
	case classAgent:
		// fall through and report the agent's conv-id
	default:
		// Neither a confirmed agent nor the human (unidentified /
		// unconfirmed / unidentifiable-agent). Report honestly rather
		// than the old fail-open is_human:true.
		writeJSON(w, http.StatusOK, whoamiResp{})
		return
	}
	// Resolve the agent's name through FreshTitle so it picks up the
	// spawn-time pending_name (agents.pending_name) when no custom title has
	// landed yet — the same priority (custom → pending → summary → first
	// prompt) the dashboard and conv-listing surfaces use. A bare
	// DisplayTitle would skip the pending name, which is the bug for a
	// freshly-spawned Codex agent (JOH-219): Codex persists its title
	// out-of-band AFTER the welcome is injected (JOH-216), so at the
	// moment the agent runs `whoami` there is no custom title in
	// conv_index yet and it would read itself as "(unnamed)" — and
	// self-describe that way — despite the welcome having named it.
	// Claude Code is unaffected (its /rename writes the .jsonl before the
	// welcome). UnknownTitle maps back to the "(unnamed)" placeholder
	// whoami has always shown.
	title := agent.FreshTitle(p.ConvID)
	if title == agent.UnknownTitle {
		title = "(unnamed)"
	}
	groups, _ := db.ListGroupsForConv(p.ConvID)
	gs := make([]string, 0, len(groups))
	activeGroups := make([]string, 0, len(groups))
	var phases []string
	for _, g := range groups {
		gs = append(gs, g.Name)
		if !g.IsArchived() {
			activeGroups = append(activeGroups, g.Name)
		}
		// Advisory process (JOH-242): surface the group's current phase when it
		// has a process. Best-effort — a lookup error just omits the line.
		if st, err := db.GetGroupProcessState(g.ID); err == nil && st != nil {
			phases = append(phases, g.Name+": "+phaseLabel(st))
		}
	}
	agentID, _ := db.AgentIDForConv(p.ConvID)
	presence, _ := db.ProjectedAgentFederationPresence(agentID)
	var pendingMorph *db.AgentPendingMorph
	if agentID != "" {
		pendingMorph, _ = db.PendingMorphForAgent(agentID)
	}
	writeJSON(w, http.StatusOK, whoamiResp{FederationPresence: presence, Predecessor: teleportPredecessor(agentID), AgentID: agentID, ConvID: p.ConvID, Title: title, Groups: gs, ActiveGroups: activeGroups, Phases: phases, PendingMorph: pendingMorph, KeptPermissionGroups: keptPermissionGroupNames(agentID)})
}

// --- /v1/lookup ---

func handleLookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	// Open to humans as well as agents — selector resolution is a
	// read-only conv_index lookup with no PII unique to one caller,
	// and the CLI's `agent delete` uses this to preview matches
	// before prompting the human for confirmation.
	selector := r.URL.Query().Get("selector")
	if selector == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing selector")
		return
	}
	res, matches, err := agent.ResolveSelector(selector)
	if errors.Is(err, agent.ErrAmbiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      "selector matches multiple conversations",
			"code":       "ambiguous",
			"candidates": peerEntriesFromResolved(matches),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	agentID, _ := db.AgentIDForConv(res.ConvID)
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": res.ConvID, "agent_id": agentID})
}

// --- /v1/peers ---

type peerEntry struct {
	FederationPresence *db.AgentFederationPresence `json:"federation_presence,omitempty"`
	TeleportPaused     string                      `json:"teleport_paused,omitempty"`
	Predecessor        *db.FederationMoveLink      `json:"predecessor,omitempty"`
	// AgentID is the stable, rotation-immune actor key — the canonical
	// way to reference an agent. The agent CLI leads with it; ConvID is
	// the live generation behind it (which rotates on reincarnate/clone).
	// Empty only for a resolved candidate that isn't an agent. (The
	// dashboard still keys on conv_id — a separate follow-up.)
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	Role    string `json:"role,omitempty"`
	Descr   string `json:"descr,omitempty"`
	// agentLocationView carries `branch` (current branch) plus the
	// startup/current directory split — see agent_location_view.go.
	agentLocationView
	Online bool     `json:"online"`
	Groups []string `json:"groups"`
	// State is the intentionally lean activity/runtime subset agent peers may
	// see. Do not embed agentState here: it also carries context and cost data,
	// whose cross-agent read is separately permission-gated.
	State peerState `json:"state"`
	// PendingMorph is a requested morph waiting for the agent to go idle.
	PendingMorph *db.AgentPendingMorph `json:"pending_morph,omitempty"`
}

// peerState is the runtime summary `tclaude agent ls` needs. Its values come
// from stateForConvInSessions, the same settled-state resolver that feeds the
// dashboard, so sub-agent TTL reconciliation and offline handling cannot drift
// between the two surfaces.
type peerState struct {
	Harness             string `json:"harness,omitempty"`
	Model               string `json:"model,omitempty"`
	EffortLevel         string `json:"effort_level,omitempty"`
	Status              string `json:"status,omitempty"`
	SubagentCount       int    `json:"subagent_count"`
	ExitReason          string `json:"exit_reason,omitempty"`
	RecoveryStatus      string `json:"recovery_status,omitempty"`
	RecoveryDetail      string `json:"recovery_detail,omitempty"`
	RecoveryReason      string `json:"recovery_reason,omitempty"`
	RecoveryCount       int    `json:"recovery_count,omitempty"`
	RecoveryNextAttempt string `json:"recovery_next_attempt_at,omitempty"`
}

func peerStateFromAgentState(state agentState) peerState {
	return peerState{
		Harness:             state.Harness,
		Model:               state.Model,
		EffortLevel:         state.EffortLevel,
		Status:              state.Status,
		SubagentCount:       state.SubagentCount,
		ExitReason:          state.ExitReason,
		RecoveryStatus:      state.RecoveryStatus,
		RecoveryDetail:      state.RecoveryDetail,
		RecoveryReason:      state.RecoveryReason,
		RecoveryCount:       state.RecoveryCount,
		RecoveryNextAttempt: state.RecoveryNextAttempt,
	}
}

// handlePeers returns the conversations the caller can see.
//
// Two passes:
//
//  1. **Group members.** Agent caller → members of every group the
//     caller is in. Human caller, or an agent holding fleet.read → members
//     of every known group (they aren't scoped by group membership and see
//     the full picture; what an agent can reach is still decided at send).
//  2. **Ungrouped agents.** Every active enrolled agent not already
//     surfaced by pass 1, online or offline. Caller (when known) is
//     excluded. Being an agent is an explicit, durable fact now, so
//     `tclaude agent ls` keeps showing an agent after its tmux pane
//     closes instead of dropping it the moment it goes offline.
//     Retired agents are excluded — they are no longer agents.
func handlePeers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	// The human operator sees every group; an agent is scoped to its
	// own; unidentified / unconfirmed callers are refused fail-closed.
	myID, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}

	var groups []*db.AgentGroup
	var err error
	if isHuman || readsWholeFleet(r, myID) {
		groups, err = db.ListAgentGroups()
	} else {
		groups, err = db.ListGroupsForConv(myID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// Optional `--group` filter. Resolve the requested name/ID against ONLY
	// the caller-visible group set computed above, so an agent can never widen
	// its view to a group it has no approved link to — even by naming that
	// group explicitly. A miss (nothing in the visible set matches) is a
	// scoped 404: for a human that means the group truly doesn't exist; for an
	// agent it means "not one of yours", revealing nothing about groups out of
	// its scope.
	groupFilter := strings.TrimSpace(r.URL.Query().Get("group"))
	if groupFilter != "" {
		matched := matchVisibleGroup(groups, groupFilter)
		if matched == nil {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no group %q reachable by you", groupFilter))
			return
		}
		groups = []*db.AgentGroup{matched}
	}
	// One tmux ls for the whole listing — every isConvOnlineIn below
	// is a map lookup against this snapshot, not a per-row subprocess.
	shared, ok := requestStatusSnapshot(w, r, nil)
	if !ok {
		return
	}
	aliveSessions := shared.alive

	byConv := map[string]*peerEntry{}
	// Pass 1: group members.
	for _, g := range groups {
		members, _ := db.ListAgentGroupMembers(g.ID)
		for _, m := range members {
			if m.ConvID == myID {
				continue
			}
			pe, exists := byConv[m.ConvID]
			if !exists {
				// FreshTitle / FreshBranch refresh the conv_index row
				// from the .jsonl first, so a renamed / freshly-spawned
				// member shows its real name and branch instead of stale
				// values.
				pe = &peerEntry{
					AgentID:           peerAgentID(m.ConvID),
					ConvID:            m.ConvID,
					Title:             agent.FreshTitle(m.ConvID),
					Role:              m.Role,
					Descr:             m.Descr,
					agentLocationView: locationView(m.ConvID),
					Online:            isConvOnlineIn(m.ConvID, aliveSessions),
				}
				byConv[m.ConvID] = pe
			}
			pe.Groups = append(pe.Groups, g.Name)
		}
	}
	// Pass 2: active enrolled agents that belong to NO group, online or
	// offline. Switched from "online sessions" to the enrollment roster
	// so an agent that is offline (tmux closed) still shows up —
	// agent-ness is an explicit, durable fact now, not a function of
	// whether a pane happens to be alive.
	//
	// Only UNGROUPED agents are surfaced here: a grouped agent is
	// either already in byConv (a group the caller shares, via pass 1)
	// or in a group the caller cannot see — and in the latter case it
	// must stay hidden, preserving the group-scoping an agent caller
	// relies on. ListActiveAgents excludes retired agents.
	//
	// Skipped entirely under a `--group` filter: the caller asked for the
	// members of one specific group, and an ungrouped agent is by definition
	// in no group, so it can never belong to the requested one.
	if active, err := db.ListActiveAgents(); err == nil && groupFilter == "" {
		for _, e := range active {
			conv := e.CurrentConvID
			if conv == "" || conv == myID {
				continue
			}
			if _, exists := byConv[conv]; exists {
				continue
			}
			if groups, gerr := db.ListGroupsForConv(conv); gerr != nil || len(groups) > 0 {
				continue
			}
			byConv[conv] = &peerEntry{
				AgentID:           e.AgentID,
				ConvID:            conv,
				Title:             agent.FreshTitle(conv),
				agentLocationView: locationView(conv),
				Online:            isConvOnlineIn(conv, aliveSessions),
			}
		}
	}
	// Reuse the same gathered runtime state as dashboard, tools and peers.
	// Group membership and permissions above are fresh projection decisions.
	for conv, pe := range byConv {
		state, _ := shared.stateFor(conv)
		pe.State = peerStateFromAgentState(state)
		pe.FederationPresence, _ = db.ProjectedAgentFederationPresence(pe.AgentID)
		if pe.FederationPresence != nil && pe.FederationPresence.State == "away" {
			pe.Online = false
			pe.State.Status = "away"
		}
		pe.Predecessor = teleportPredecessor(pe.AgentID)
		pe.TeleportPaused = pausedTeleportDescription(pe.AgentID)
		if pe.AgentID != "" {
			pe.PendingMorph, _ = db.PendingMorphForAgent(pe.AgentID)
		}
	}
	out := make([]*peerEntry, 0, len(byConv))
	for _, pe := range byConv {
		out = append(out, pe)
	}
	writeJSON(w, http.StatusOK, out)
}

// matchVisibleGroup resolves a `--group` filter value against the
// caller-visible group set. It accepts either the group name or its numeric
// ID. Name match takes precedence over ID, so a group literally named "5"
// still resolves by name rather than being shadowed by a group whose ID is 5.
//
// It only ever inspects the slice it is handed — the caller has already scoped
// that to what the requester may see — so it cannot resolve a group the caller
// has no approved link to. Returns nil when nothing matches; the caller turns
// that into a scoped 404.
func matchVisibleGroup(groups []*db.AgentGroup, sel string) *db.AgentGroup {
	for _, g := range groups {
		if g.Name == sel {
			return g
		}
	}
	if id, err := strconv.ParseInt(sel, 10, 64); err == nil {
		for _, g := range groups {
			if g.ID == id {
				return g
			}
		}
	}
	return nil
}

func peerEntriesFromResolved(rs []*agent.Resolved) []*peerEntry {
	out := make([]*peerEntry, 0, len(rs))
	for _, r := range rs {
		out = append(out, &peerEntry{
			// agent.ResolveSelector already stamped the stable agent_id on
			// every candidate, so read it off the resolved handle instead of
			// a second db.AgentIDForConv lookup.
			AgentID: r.AgentID,
			ConvID:  r.ConvID,
			// Use the same cache-only custom > pending > summary > prompt
			// precedence that selected the candidate. In particular, an
			// ambiguity response for pending-only Codex agents must not render
			// the very name they matched as blank.
			Title:             agent.TitleFor(r.ConvID),
			agentLocationView: locationView(r.ConvID),
		})
	}
	return out
}

// peerAgentID resolves a conv to its stable agent_id for display, or ""
// when the conv is not (yet) an agent. A miss is non-fatal — the row just
// shows no stable id.
func peerAgentID(conv string) string {
	id, _ := db.AgentIDForConv(conv)
	return id
}

// sameActor reports whether two conv-ids belong to the same stable agent.
// It is the rotation-immune form of `a == b` for actor-equality self-checks
// (JOH-323): each conv resolves to its agent_id and the agents are compared,
// so two generations of one agent (post-reincarnate / post-/clear) count as
// equal — the old conv-literal compare only matched the exact generation.
//
// An empty conv-id is "no actor" and is never the same actor as anything,
// including another empty — so it is fail-closed (returns false) regardless
// of the a == b short-circuit below. Callers already guard their empty cases
// (a human caller is ""; a placeholder member's conv is ""), but failing
// closed here keeps the primitive safe for any future caller.
//
// Semantics are otherwise preserved for the non-agent cases the old compare
// handled: the `a == b` short-circuit keeps the common self case cheap (no DB
// hit) and still matches two identical convs; and a conv with no actor row (a
// plain conversation) resolves to "" and is matched by conv-id alone, so two
// distinct non-agent convs never collide, and a non-agent conv never matches
// an agent. Mirrors the JOH-317 inbox/ownership matcher (callerIsRecipient).
func sameActor(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	aa := peerAgentID(a)
	return aa != "" && aa == peerAgentID(b)
}

// stampCallerAgentID adds the durable `caller_agent_id` companion to a
// response that already carries a `caller_conv` snapshot. The conv-id names
// only one generation, so after the caller reincarnates the snapshot points
// at a dead generation; the agent_id is the stable actor the attribution
// should lead with (JOH-329). No-op when the caller isn't an enrolled agent
// (older / un-enrolled callers keep only the conv snapshot, and the CLI falls
// back to it).
func stampCallerAgentID(resp map[string]any, caller string) {
	if aid := peerAgentID(caller); aid != "" {
		resp["caller_agent_id"] = aid
	}
}

// isValidFollowUp enforces follow-up prompt sanitization.
//
// Unlike rename titles (which need a hard charset gate against
// keystroke-injection across agents), the follow-up is a free-form
// prompt the agent submits to *itself* — there's no privilege
// escalation surface, since the agent already runs in its own pane.
//
// We only reject control characters (newlines, tabs, NUL, etc.)
// because each newline in tmux send-keys would land as a prompt-submit,
// fragmenting the follow-up into multiple turns. Length is capped at
// 4096 bytes to keep tmux invocations reasonable.
func isValidFollowUp(s string) bool {
	if s == "" || len(s) > 4096 {
		return false
	}
	for _, r := range s {
		if r == ' ' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// isValidInitialMessage validates a spawn's initial-context brief.
//
// Unlike isValidFollowUp — which guards text typed into a tmux pane,
// where a raw newline would land as a premature prompt-submit — the
// initial message is delivered to the new agent's inbox as an
// agent_messages row and rendered by `inbox read`. So newlines and
// tabs are allowed (and wanted: a multi-line brief keeps its
// paragraph structure). We still reject other control characters
// (NUL, escape, carriage return, …) that would corrupt a terminal
// render, and cap the length at agent.MaxInitialMessageBytes.
//
// An empty string is valid — it simply means "no initial message".
func isValidInitialMessage(s string) bool {
	if len(s) > agent.MaxInitialMessageBytes {
		return false
	}
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// isValidSpawnName is the daemon's name for the one spawn-name gate,
// agent.IsValidSpawnName; the CLI calls the same function, and the dashboard
// JS mirror is pinned to the same fixture (agentd/jstest/spawn-name-fixture.json).
func isValidSpawnName(name string) bool { return agent.IsValidSpawnName(name) }

// maxSpawnAttachments caps how many attachment paths a single spawn may carry.
// The dashboard upload endpoint enforces its own per-batch caps; this is the
// daemon-side backstop on the spawn request itself so a hand-rolled caller
// can't balloon the briefing.
const maxSpawnAttachments = 50

// sanitizeSpawnAttachments cleans the spawn request's attachment paths for the
// startup briefing: trims each, drops blanks, and rejects any path containing a
// control character (newline included — each path is a single briefing line) or
// exceeding a sane length, since the list is rendered by `inbox read` and may
// ride inline in the launch prompt. The paths are NOT access-checked: the
// daemon never opens the files — the spawned agent reads them itself, as the
// same user — so an arbitrary path only ever tells the new agent "look here".
// Returns the cleaned list, or a non-empty error message describing the first
// rejection. A tclaude-layer spawn later accepts only exact daemon-issued
// staging paths and securely copies them into its private namespace-visible
// root; harness-builtin spawns retain the historical path-only behavior.
func sanitizeSpawnAttachments(paths []string) ([]string, string) {
	if len(paths) > maxSpawnAttachments {
		return nil, fmt.Sprintf("too many attachments: %d (max %d)", len(paths), maxSpawnAttachments)
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) > 4096 {
			return nil, fmt.Sprintf("attachment path too long: %d bytes (max 4096)", len(p))
		}
		for _, r := range p {
			if r < 0x20 || r == 0x7f {
				return nil, "attachment path contains a control character"
			}
		}
		out = append(out, p)
	}
	return out, ""
}

// handleWhoamiContext returns the caller's own context-window state.
// Read-only and self-targeted, so no permission gate — any agent can
// introspect its own session. Returns 0/0 if the row hasn't been
// populated yet (statusbar hook hasn't fired this session).
//
// Note: context_pct is keyed in SQLite by tclaude's session ID (the
// label, not the conv-id) because the statusbar hook only knows
// TCLAUDE_SESSION_ID at write time. So we resolve conv-id → session
// row first, preferring an alive tmux session when multiple historical
// rows share the same conv-id.
func handleWhoamiContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	convID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	writeContextInfo(w, r, convID, "")
}

// handleAgentContext returns ANOTHER agent's context-window state — the
// manager-pattern read reached via /v1/agent/{selector}/context. Unlike
// the directory read (ungated, see handleAgentDir), context usage is
// gated like the other cross-agent verbs: the caller passes with the
// agent.context-info slug, or with groups.members.context-info covering
// every active group containing the target (ownership contributes it, so
// a lead never needs a slug to watch its own team).
//
// An explicit deny override is ALWAYS authoritative and suppresses the
// owner bypass — the universal precedence resolvePermission /
// requireCrossAgentPermission apply to every cross-agent verb. The owner
// bypass only fills the "undecided" gap (no grant, no deny). Read-only,
// so GET only; the X-Tclaude-Ask-Human popup escape hatch still applies.
func handleAgentContext(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentContextInfo, targetConv)
	if !ok {
		return
	}
	writeContextInfo(w, r, targetConv, caller)
}

// writeContextInfo resolves convID to its most-relevant session row and
// writes that row's context snapshot. caller is the requesting agent's
// conv-id on the cross-agent path (echoed for the audit trail) and ""
// for self / human reads. Shared by the self and cross-agent handlers.
func writeContextInfo(w http.ResponseWriter, r *http.Request, convID, caller string) {
	shared, ok := requestStatusSnapshot(w, r, nil)
	if !ok {
		return
	}
	snap, sessionID, _ := shared.contextFor(convID)
	state, _ := shared.stateFor(convID)
	resp := map[string]any{
		"conv_id":               convID,
		"session_id":            sessionID,
		"context_pct":           snap.ContextPct,
		"tokens_input":          snap.TokensInput,
		"tokens_output":         snap.TokensOutput,
		"context_window_size":   snap.ContextWindowSize,
		"context_window_max":    state.ContextWindowMax,
		"context_window_source": state.ContextWindowSource,
		"model":                 snap.Model,
	}
	if caller != "" && caller != convID {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	writeJSON(w, http.StatusOK, resp)
}

// pickWithLiveness returns the session row whose tmux pane is alive
// (per the injected liveness predicate), or — falling back — the row
// that comes first in the list (which FindSessionsByConvID orders by
// updated_at DESC). nil when the list is empty. The predicate is
// injected so callers can supply a pre-fetched alive set (a single tmux
// ls for a whole listing) and unit tests can stub it without reaching
// for tmux on the host.
func pickWithLiveness(candidates []*db.SessionRow, isAlive func(string) bool) *db.SessionRow {
	for _, c := range candidates {
		if isAlive(c.TmuxSession) {
			return c
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return nil
}

// isValidRenameTitle and isValidRenameSink are the daemon's names for the
// shared send-keys title gate (renametitle). The hook callback and the CLI
// call the same package, and renametitletest's table pins all three.
func isValidRenameTitle(t string) bool { return renametitle.Valid(t) }

func isValidRenameSink(t string) bool { return renametitle.ValidSink(t) }
