package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/renametitle"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
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

func groupByID(id int64) (*db.AgentGroup, error) {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.ID == id {
			return g, nil
		}
	}
	return nil, nil
}

// --- /v1/groups (GET = anyone, POST = human only) ---

type groupSummary struct {
	Name                    string `json:"name"`
	Descr                   string `json:"descr,omitempty"`
	AttachmentURL           string `json:"attachment_url,omitempty"`
	AttachmentLabel         string `json:"attachment_label,omitempty"`
	AttachmentLabelOverride string `json:"attachment_label_override,omitempty"`
	Members                 int    `json:"members"`
	Online                  int    `json:"online"`
	// MaxMembers is the group's hard member cap (agent_groups.max_members);
	// 0 = unlimited. A spawn that would exceed it is refused.
	MaxMembers int `json:"max_members,omitempty"`
	// DefaultProfile is the name of the spawn profile whose launch fields fill
	// blank spawn fields for this group's agents (JOH-210); "" = none. It is
	// the spawn default's single source — the vestigial per-group
	// default_model was dropped (JOH-220).
	DefaultProfile string `json:"default_profile,omitempty"`
	// DefaultPrompts names the saved prompts composed into a spawn's initial
	// message when neither the spawn nor its named profile names any.
	DefaultPrompts []string `json:"default_prompts,omitempty"`
	// DefaultCwd is the working directory blank spawns into this group fall
	// back to (agent_groups.default_cwd); "" = none. It is a path on the
	// operator's own filesystem and this listing is readable by every local
	// caller, so it is served only to a human one — see handleGroups.
	DefaultCwd  string                           `json:"default_cwd,omitempty"`
	Environment []sandboxpolicy.EnvironmentEntry `json:"environment,omitempty"`
	// DefaultSpawnGroup is the operator-selected tie-breaker used only when
	// directory auto-join matches more than one active group.
	DefaultSpawnGroup bool     `json:"default_spawn_group,omitempty"`
	Permissions       []string `json:"permissions,omitempty"`
	Archived          bool     `json:"archived,omitempty"`
	// NotifyMuted flags a group whose OS notifications are switched
	// off (agent_groups.notify_enabled = false). omitempty: only the
	// exceptional muted state is serialized.
	NotifyMuted bool `json:"notify_muted,omitempty"`
	// RemoteControlPolicy is the group's remote-control policy (JOH-262):
	// "inherit" (defer to the spawn profile), "optin" (force Remote Access on)
	// or "deny" (force it off). Always serialized (the canonical token) so a
	// consumer never has to guess between "absent" and "inherit".
	RemoteControlPolicy string `json:"remote_control_policy"`
	// ReinjectAfterCompact is what tclaude re-injects into member agents after
	// a compaction or /clear: "contexts" (the default), "identity" or "off".
	// Always serialized as the effective token.
	ReinjectAfterCompact string `json:"reinject_after_compact"`
	// Mission and SourceTemplate are the deploy provenance (JOH-245) carried
	// on the group row. They are what tells a plain group apart from a deployed
	// task force: `tclaude agent task-force ls` (JOH-346) filters on a
	// non-empty SourceTemplate and shows the truncated mission. omitempty — a
	// hand-built group carries neither, and plain `groups ls` ignores them.
	Mission        string `json:"mission,omitempty"`
	SourceTemplate string `json:"source_template,omitempty"`
	// OwnerScopes narrows the group's STRUCTURAL owner-implied permission
	// bypass (TCL-1071): slug → the scope an owner's bypass is confined to
	// here. omitempty — the overwhelming majority of groups impose no
	// narrowing, and an absent field reads as the historical unrestricted
	// bypass exactly as an older daemon's response did.
	OwnerScopes map[string]PermissionScope `json:"owner_scopes,omitempty"`
}

// isConvOnline reports whether any tmux session registered for this conv-id
// is currently alive. Same alive-check the queue delivery path uses.
//
// Single-target callers (delivery, lifecycle, reaper) use this — one
// conv at a time, one `tmux has-session` subprocess at most. For
// snapshot-shaped callers iterating many convs, use isConvOnlineIn
// with a pre-fetched alive set: O(N) subprocesses collapses to one.
func isConvOnline(convID string) bool {
	candidates, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return false
	}
	for _, c := range candidates {
		if c.TmuxSession != "" && session.IsTmuxSessionAlive(c.TmuxSession) {
			return true
		}
	}
	return false
}

// isConvOnlineIn is the snapshot-shaped variant of isConvOnline. It
// takes a pre-fetched alive set (e.g. from clcommon.Default.ListSessions
// at the top of an HTTP handler) and does the per-row check via map
// lookup instead of per-row subprocess. Callers MUST fetch the set
// once per request and pass the SAME map to every call — passing nil
// (or fetching afresh per call) defeats the purpose and is the worst
// of both worlds.
func isConvOnlineIn(convID string, alive map[string]struct{}) bool {
	candidates, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return false
	}
	return isConvOnlineInSessions(candidates, alive)
}

// isConvOnlineInSessions is isConvOnlineIn over an already-fetched session
// slice (most-recent-first, as FindSessionsByConvID returns). The dashboard
// snapshot's per-request batch loader (TCL-368) resolves liveness through it so
// a conv's rows are read once for the whole poll instead of per surface.
func isConvOnlineInSessions(candidates []*db.SessionRow, alive map[string]struct{}) bool {
	for _, c := range candidates {
		if c.TmuxSession == "" {
			continue
		}
		if _, ok := alive[c.TmuxSession]; ok {
			return true
		}
	}
	return false
}

func handleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Anyone (token or not) can list groups. By default archived
		// groups are filtered out — they're soft-deleted and rarely
		// belong in a default listing. Pass `?archived=1` (any non-empty
		// truthy value) to include them; the CLI surfaces this via
		// `groups ls --archived`.
		groups, err := db.ListAgentGroups()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		showArchived := isTruthy(r.URL.Query().Get("archived"))
		// A group's default_cwd is a directory on the human's own filesystem,
		// outside any agent's sandbox, and this listing is the one /v1 read
		// that asks nothing of its caller. Serving the path to an agent would
		// hand it filesystem layout it has no way to guess (the terminal
		// console gates its Tab-completion for exactly this reason), so it
		// goes to a human caller only — who configured it and reads it back
		// in the dashboard and `groups ls` anyway. The directory also goes to a
		// fleet reader, whose setup help starts from it; the environment, which
		// can carry private values, stays with the human.
		caller := peerFromContext(r.Context())
		human := classify(caller) == classHuman
		showDirs := human || (caller != nil && readsWholeFleet(r, caller.ConvID))
		// One tmux ls for the listing — per-member online checks below
		// are map lookups against this snapshot.
		aliveSessions, _ := cachedLiveTmuxSessions()
		out := make([]groupSummary, 0, len(groups))
		for _, g := range groups {
			if !showArchived && g.IsArchived() {
				continue
			}
			members, _ := db.ListAgentGroupMembers(g.ID)
			groupPermissions, _ := db.ListAgentGroupPermissions(g.ID)
			online := 0
			for _, m := range members {
				if isConvOnlineIn(m.ConvID, aliveSessions) {
					online++
				}
			}
			attachment := groupAttachmentViewFor(g)
			defaultCwd := ""
			var environment []sandboxpolicy.EnvironmentEntry
			if showDirs {
				defaultCwd = g.DefaultCwd
			}
			if human {
				environment = append([]sandboxpolicy.EnvironmentEntry(nil), g.Environment...)
			}
			out = append(out, groupSummary{
				Name:                    g.Name,
				Descr:                   g.Descr,
				AttachmentURL:           attachment.URL,
				AttachmentLabel:         attachment.Label,
				AttachmentLabelOverride: attachment.LabelOverride,
				Members:                 len(members),
				Online:                  online,
				MaxMembers:              g.MaxMembers,
				DefaultProfile:          g.DefaultProfile,
				DefaultPrompts:          g.DefaultPrompts,
				DefaultCwd:              defaultCwd,
				Environment:             environment,
				DefaultSpawnGroup:       g.DefaultSpawnGroup,
				Permissions:             groupPermissions,
				Archived:                g.IsArchived(),
				NotifyMuted:             !g.NotifyEnabled,
				RemoteControlPolicy:     remoteControlPolicyToWire(g.RemoteControl),
				ReinjectAfterCompact:    g.EffectiveReinjectAfterCompact(),
				OwnerScopes:             ownerScopesWireOmitEmpty(g.OwnerScopesJSON),
				Mission:                 g.Mission,
				SourceTemplate:          g.SourceTemplate,
			})
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		creator, ok := requirePermission(w, r, PermGroupsCreate)
		if !ok {
			return
		}
		var body struct {
			Name            string                            `json:"name"`
			Parent          string                            `json:"parent,omitempty"`
			Descr           string                            `json:"descr,omitempty"`
			DefaultCwd      string                            `json:"default_cwd,omitempty"`
			DefaultContext  string                            `json:"default_context,omitempty"`
			Environment     *[]sandboxpolicy.EnvironmentEntry `json:"environment,omitempty"`
			AttachmentURL   string                            `json:"attachment_url,omitempty"`
			AttachmentLabel string                            `json:"attachment_label,omitempty"`
			// DefaultProfile names the spawn profile (JOH-210) whose launch
			// fields fill blank spawn fields for this group's agents. "" = none.
			DefaultProfile string `json:"default_profile,omitempty"`
			// MaxMembers is the group's hard member cap; 0 = unlimited.
			// A negative value is clamped to 0 by db.SetAgentGroupMaxMembers.
			MaxMembers int `json:"max_members,omitempty"`
			// RemoteControlPolicy is the group's remote-control policy that
			// overrides a spawn profile's remote-control default (JOH-262):
			// "" / "inherit" (defer to the profile), "optin" (force on), "deny"
			// (force off).
			RemoteControlPolicy string `json:"remote_control_policy,omitempty"`
			// RepositoryClone is a dashboard-human-only convenience: clone a
			// GitHub repository before creating the group, use that checkout as
			// default_cwd, and optionally expose its web URL as the group attachment.
			RepositoryClone *groupRepositoryClone `json:"repository_clone,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		// validateGroupName is the same guard rename and clone already
		// apply — it rejects empty names, embedded slashes (which would
		// break the /v1/groups/{name}/... and /api/groups/{name}/...
		// route dispatch), control characters, and stray whitespace.
		// Create historically skipped it, which let a slash-named group
		// through and made every later sub-route on that group
		// unroutable.
		if err := validateGroupName(body.Name); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		if body.RepositoryClone != nil && creator != "" {
			writeError(w, http.StatusForbidden, "human_required", "repository cloning during group creation is available only to the dashboard human")
			return
		}
		repositoryPlan, err := prepareGroupRepositoryClone(body.RepositoryClone)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_repository", err.Error())
			return
		}
		// Validate the optional default cwd + startup context up front,
		// before any DB write, so a bad value fails cleanly without
		// leaving a half-configured group behind.
		groupCwd, err := resolveGroupDefaultCwd(body.DefaultCwd)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
			return
		}
		if repositoryPlan != nil {
			if groupCwd != "" && groupCwd != repositoryPlan.Destination {
				writeError(w, http.StatusBadRequest, "invalid_cwd", "default_cwd must match the repository clone destination")
				return
			}
			groupCwd = repositoryPlan.Destination
		}
		// A rename could give a group made this way a pinned group's name.
		if db.IsExternalStewardConv(creator) && (groupCwd != "" || body.Environment != nil) {
			writeError(w, http.StatusForbidden, externalStewardCeilingCode,
				"an external steward can't set a group's directory or environment; those decide how its workers start")
			return
		}
		groupContext, err := normalizeGroupContext(body.DefaultContext)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		var groupEnvironment []sandboxpolicy.EnvironmentEntry
		if body.Environment != nil {
			groupEnvironment, err = sandboxpolicy.NormalizeEnvironment(*body.Environment)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_environment", err.Error())
				return
			}
		}
		attachmentURL, attachmentLabel, err := normalizeGroupAttachment(body.AttachmentURL, body.AttachmentLabel)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_attachment", err.Error())
			return
		}
		// The default profile (JOH-210) replaces the Claude-only default_model
		// gate: validate only that the referenced profile exists — its launch
		// fields were already validated against their own harness at save, so a
		// group can default its team onto a Codex profile without the
		// Claude-only model validator rejecting it (#343). "" = no default.
		groupProfile, err := resolveGroupDefaultProfileName(body.DefaultProfile)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_profile", err.Error())
			return
		}
		if fail := defaultProfileConferralFailure(r, creator, body.Name, groupProfile); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		// Validate the optional remote-control policy up front (JOH-262); a bad
		// token fails cleanly before any DB write. "" / "inherit" → nil (no
		// override, the column default).
		groupRemoteControl, err := parseRemoteControlPolicy(body.RemoteControlPolicy)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_remote_control_policy", err.Error())
			return
		}
		// Fold newlines out of the description on the create path too,
		// not just update — the one-line header invariant must hold
		// however the descr first arrives (see normalizeGroupDescr).
		groupDescr := normalizeGroupDescr(body.Descr)
		if existing, _ := db.GetAgentGroupByName(body.Name); existing != nil {
			writeError(w, http.StatusConflict, "exists", "group already exists")
			return
		}
		id, err := db.CreateAgentGroupWithParentAndAttachment(
			body.Name, groupDescr, body.Parent, attachmentURL, attachmentLabel)
		if errors.Is(err, db.ErrGroupParentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		if errors.Is(err, db.ErrGroupParentCycle) {
			writeError(w, http.StatusConflict, "cycle", err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		// The group row reserves its unique name and validates its parent before
		// host-side cloning begins. It is still empty here, so a clone failure can
		// safely roll it back and leave the dialog retryable without an orphaned
		// group or a successful checkout detached from a failed DB insert.
		if err := cloneGroupRepository(repositoryPlan); err != nil {
			if deleteErr := db.DeleteAgentGroup(body.Name); deleteErr != nil {
				slog.Error("groups create: clone failed and empty group rollback failed",
					"group", body.Name, "error", deleteErr)
			}
			writeError(w, http.StatusBadGateway, "clone_failed", err.Error())
			return
		}
		// Apply the default cwd + startup context as post-create updates
		// — keeps CreateAgentGroup's signature untouched (it's shared with
		// the clone path and flow-test helpers). A failure here is logged
		// but doesn't unwind the create; the human can set it later via
		// `groups set-context` or the dashboard.
		if groupCwd != "" {
			if _, err := db.SetAgentGroupDefaultCwd(body.Name, groupCwd); err != nil {
				slog.Warn("groups create: failed to set default cwd",
					"group", body.Name, "error", err)
			}
		}
		if repositoryPlan != nil && body.RepositoryClone.Attach {
			if _, err := db.SetAgentGroupAttachment(body.Name, repositoryPlan.WebURL, repositoryPlan.Label); err != nil {
				slog.Warn("groups create: failed to set repository attachment",
					"group", body.Name, "error", err)
			}
		}
		if groupContext != "" {
			if _, err := db.SetAgentGroupDefaultContext(body.Name, groupContext); err != nil {
				slog.Warn("groups create: failed to set default context",
					"group", body.Name, "error", err)
			}
		}
		if body.Environment != nil {
			if _, err := db.SetAgentGroupEnvironment(body.Name, groupEnvironment); err != nil {
				slog.Warn("groups create: failed to set environment",
					"group", body.Name, "error", err)
			}
		}
		if groupProfile != "" {
			if _, err := db.SetAgentGroupDefaultProfile(body.Name, groupProfile); err != nil {
				slog.Warn("groups create: failed to set default profile",
					"group", body.Name, "error", err)
			}
		}
		if body.MaxMembers != 0 {
			if _, err := db.SetAgentGroupMaxMembers(body.Name, body.MaxMembers); err != nil {
				slog.Warn("groups create: failed to set max members",
					"group", body.Name, "error", err)
			}
		}
		// Only write a non-inherit policy; "inherit" (nil) is the column default,
		// so a create that didn't ask for a policy leaves the group deferring to
		// the profile (JOH-262).
		if groupRemoteControl != nil {
			if _, err := db.SetAgentGroupRemoteControl(body.Name, groupRemoteControl); err != nil {
				slog.Warn("groups create: failed to set remote-control policy",
					"group", body.Name, "error", err)
			}
		}
		// Auto-grant ownership to the creator. Skipped for the human
		// path (creator == "") since humans don't have a conv-id; the
		// human is implicitly above the permission system anyway.
		// Failure here is logged but doesn't unwind the create — the
		// human can grant ownership manually if needed.
		if creator != "" {
			if err := db.AddAgentGroupOwner(id, creator, auditedCaller(creator, PermGroupsCreate)); err != nil {
				slog.Warn("groups create: auto-grant owner failed",
					"group", body.Name, "creator", creator, "error", err)
			}
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": id, "name": body.Name, "parent": strings.TrimSpace(body.Parent),
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// --- /v1/groups/{name}* routes ---

// registerV1GroupRoutes wires the SO_PEERCRED-authed /v1/groups/{name}
// endpoints onto the daemon mux as Go 1.22 method+pattern routes:
//
//	POST   /v1/groups/{name}/stop            → stop every member
//	POST   /v1/groups/{name}/resume          → resume every member
//	POST   /v1/groups/{name}/retire          → retire every other member
//	POST   /v1/groups/{name}/spawn           → spawn a session into the group
//	POST   /v1/groups/{name}/archive         → soft-delete (archive)
//	POST   /v1/groups/{name}/unarchive       → restore an archived group
//	PUT    /v1/groups/{name}/parent          → nest under another group (body: {parent}); "" clears
//	POST   /v1/groups/{name}/rename          → rename (body: {new_name})
//	POST   /v1/groups/{name}/clone           → clone the group
//	GET    /v1/groups/{name}/owners          → list owners
//	POST   /v1/groups/{name}/owners          → grant owner
//	DELETE /v1/groups/{name}/owners/{conv}   → revoke owner
//	GET    /v1/groups/{name}/context         → list members' context-window state
//	GET    /v1/groups/{name}/members         → list members
//	POST   /v1/groups/{name}/members         → add member
//	PATCH  /v1/groups/{name}/members/{conv}  → update role/descr
//	DELETE /v1/groups/{name}/members/{conv}  → remove member
//	POST   /v1/groups/{name}/members/{conv}/move → move member to another group
//	       /v1/groups/{name}/links[/{id}]    → link CRUD (own method dispatch)
//	GET/POST /v1/groups/{name}/attachment    → persistent group reference
//	PATCH  /v1/groups/{name}                 → update settings
//	DELETE /v1/groups/{name}                 → delete group
//
// The {name} / {conv} / {id} wildcards are matched and percent-decoded
// by the mux (read via r.PathValue), replacing the old hand-rolled
// TrimPrefix + SplitN dispatch. That manual parse split on r.URL.Path
// — already percent-decoded — so a group name with an embedded slash
// was re-split into bogus path segments and the route was lost. A
// {name} wildcard matches one segment of the *escaped* path, so the
// slash survives intact.
func registerV1GroupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/groups/{name}/stop", v1GroupRoute(handleGroupStop))
	mux.HandleFunc("POST /v1/groups/{name}/resume", v1GroupRoute(handleGroupResume))
	mux.HandleFunc("POST /v1/groups/{name}/retire", v1GroupRoute(handleGroupRetire))
	mux.HandleFunc("POST /v1/groups/{name}/spawn", v1GroupRoute(handleGroupSpawnAPI))
	mux.HandleFunc("GET /v1/spawn-queue", handleSpawnQueueList)
	mux.HandleFunc("DELETE /v1/spawn-queue/{id}", handleSpawnQueueCancel)
	mux.HandleFunc("POST /v1/groups/{name}/archive", v1GroupRoute(handleGroupArchive))
	mux.HandleFunc("POST /v1/groups/{name}/unarchive", v1GroupRoute(handleGroupUnarchive))
	mux.HandleFunc("PUT /v1/groups/{name}/parent", v1GroupRoute(handleGroupParent))
	mux.HandleFunc("POST /v1/groups/{name}/rename", v1GroupRoute(handleGroupRename))
	mux.HandleFunc("POST /v1/groups/{name}/clone", v1GroupRoute(handleGroupClone))
	mux.HandleFunc("GET /v1/groups/{name}/export", v1GroupRoute(handleGroupExport))

	mux.HandleFunc("GET /v1/groups/{name}/owners", v1GroupRoute(handleGroupOwnersList))
	mux.HandleFunc("POST /v1/groups/{name}/owners", v1GroupRoute(handleGroupOwnersAdd))
	mux.HandleFunc("DELETE /v1/groups/{name}/owners/{conv}", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupOwnersRemove(w, r, g, r.PathValue("conv"))
	}))

	mux.HandleFunc("GET /v1/groups/{name}/context", v1GroupRoute(handleGroupContext))

	// Advisory process runtime (JOH-242): read the current phase (open),
	// advance to the next / a named phase (gated on process.advance, owner-pass).
	mux.HandleFunc("GET /v1/groups/{name}/process", v1GroupRoute(handleGroupProcessGet))
	mux.HandleFunc("POST /v1/groups/{name}/process/advance", v1GroupRoute(handleGroupProcessAdvance))

	// Staged-spawn choreography status (JOH-244): read the group's pending
	// waves (open, read-only). 404 when the deploy has no pending waves.
	mux.HandleFunc("GET /v1/groups/{name}/waves", v1GroupRoute(handleGroupWavesGet))

	// Re-brief a deployed force (JOH-247): re-deliver the source template's
	// work pattern to the live roster with the group's mission interpolated.
	// Gated on templates.instantiate + owner-pass (see handleGroupRebrief).
	mux.HandleFunc("POST /v1/groups/{name}/rebrief", v1GroupRoute(handleGroupRebrief))

	// Stand down a task force (JOH-345): retire the roster + sweep the
	// deploy-seeded rhythms and pending waves, keeping the group as a dormant
	// record. The mirror of `task-force deploy`. Gated on groups.members.retire +
	// owner-pass (see handleGroupStandDown).
	mux.HandleFunc("POST /v1/groups/{name}/stand-down", v1GroupRoute(handleGroupStandDown))

	mux.HandleFunc("GET /v1/groups/{name}/members", v1GroupRoute(handleGroupMembersList))
	mux.HandleFunc("POST /v1/groups/{name}/members", v1GroupRoute(handleGroupMembersAdd))
	mux.HandleFunc("PATCH /v1/groups/{name}/members/{conv}", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupMembersUpdate(w, r, g, r.PathValue("conv"))
	}))
	mux.HandleFunc("DELETE /v1/groups/{name}/members/{conv}", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupMembersRemove(w, r, g, r.PathValue("conv"))
	}))
	mux.HandleFunc("POST /v1/groups/{name}/members/{conv}/move", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupMembersMove(w, r, g, r.PathValue("conv"))
	}))
	mux.HandleFunc("GET /v1/groups/{name}/kept-permissions", v1GroupRoute(handleGroupKeptPermissionsList))
	mux.HandleFunc("DELETE /v1/groups/{name}/kept-permissions/{conv}", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupKeptPermissionsDrop(w, r, g, r.PathValue("conv"))
	}))

	// handleGroupLinks dispatches its own methods, so the two link
	// patterns are registered without a method in the pattern.
	mux.HandleFunc("/v1/groups/{name}/links", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupLinks(w, r, g, nil)
	}))
	mux.HandleFunc("/v1/groups/{name}/links/{id}", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupLinks(w, r, g, []string{r.PathValue("id")})
	}))
	mux.HandleFunc("/v1/groups/{name}/attachment", v1GroupRoute(handleGroupAttachment))

	mux.HandleFunc("PATCH /v1/groups/{name}", v1GroupRoute(handleGroupUpdate))
	mux.HandleFunc("DELETE /v1/groups/{name}", v1GroupRoute(handleGroupDelete))
}

// v1GroupRoute adapts a group-scoped /v1 handler into an
// http.HandlerFunc. It resolves the {name} path wildcard to a group row
// and replies with a 404 error envelope when the group is missing; the
// per-handler permission gate (SO_PEERCRED identity → requirePermission)
// still runs inside fn, exactly as before.
func v1GroupRoute(fn func(http.ResponseWriter, *http.Request, *db.AgentGroup)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "missing group name")
			return
		}
		g, err := db.GetAgentGroupByName(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if g == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		fn(w, r, g)
	}
}

// handleGroupDelete deletes a group. Permission slug: groups.delete
// (default human-only). db.DeleteAgentGroup fails with a constraint
// error if the group still has blocking references.
func handleGroupDelete(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if _, ok := requirePermission(w, r, PermGroupsDelete); !ok {
		return
	}
	hookHarnesses := standingOrderHookHarnessesForGroupBestEffort(g.ID)
	if err := db.DeleteAgentGroup(g.Name); err != nil {
		writeError(w, http.StatusConflict, "constraint", err.Error())
		return
	}
	closeGroupPages(g.ID)
	if warning := reconcileStandingOrderHookHarnesses(hookHarnesses); warning != "" {
		w.Header().Set("X-Tclaude-Hook-Warning", warning)
		slog.Warn("group delete: standing-order hook reconciliation failed",
			"group", g.Name, "warning", warning)
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxGroupContextBytes caps a group's startup context. The context is
// folded into the startup briefing delivered to each spawned agent's
// inbox; 16 KiB is comfortably more than any reasonable block of
// startup guidance while keeping the briefing message bounded.
const maxGroupContextBytes = 16 * 1024

// normalizeGroupContext prepares a group startup context for storage:
// CRLF / lone-CR line endings are folded to LF so the briefing
// renders consistently regardless of where the human typed it, and
// the result is rejected when it exceeds maxGroupContextBytes.
func normalizeGroupContext(s string) (string, error) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if len(s) > maxGroupContextBytes {
		return "", fmt.Errorf("group context too long: %d bytes (max %d)", len(s), maxGroupContextBytes)
	}
	return s, nil
}

// resolveGroupDefaultProfileName validates a requested group default-profile
// reference (JOH-210): it trims the name and, when non-empty, requires a spawn
// profile by that name to exist — there is no DB-level foreign key, so the
// referential check lives here. Empty is valid and clears the group's default.
// Returns the trimmed name to store.
func resolveGroupDefaultProfileName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", nil
	}
	p, err := db.ResolveSpawnProfile(name)
	if err != nil {
		return "", err
	}
	if p == nil {
		return "", fmt.Errorf("no spawn profile named %q", name)
	}
	return p.Name, nil
}

// normalizeGroupDescr prepares a group description for storage. The
// descr is a one-line label rendered inline in the dashboard's group
// header, so any embedded CR / LF is folded to a single space and the
// result is trimmed — a raw API caller can no longer wedge a newline
// into a header that has nowhere to put it. Empty stays empty.
//
// Applied on every write path — create, update, and clone — so the
// one-line invariant holds regardless of how the descr was set, not
// just when it was last edited.
func normalizeGroupDescr(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// handleGroupUpdate patches mutable group-level settings:
//
//   - descr — the group's own one-line description, shown next to the
//     group name on the dashboard. Distinct from a per-member descr.
//   - default_cwd — the working directory pre-filled into the spawn
//     form (and substituted server-side by handleGroupSpawn when a
//     spawn request leaves cwd blank).
//   - default_context — a block of shared startup guidance delivered
//     to the inbox of agents spawned into the group (see
//     handleGroupSpawn).
//   - default_profile — the spawn profile (JOH-210) whose launch fields
//     fill blank spawn fields server-side (see executeSpawn /
//     applyDefaultProfile), so a group can default its whole team onto a
//     given harness+model+… without the Claude-only default_model gate.
//     Validated only for existence; the profile carries its own validated
//     launch fields. Replaces the retired default_model setting (#343).
//   - max_members — the group's hard member cap (0 = unlimited); a
//     spawn that would exceed it is refused by the spawn-guardrail
//     layer. See checkSpawnGuardrails.
//   - permissions — live additive grants held by every current member. These
//     are membership policy, not spawn defaults, and are never copied into
//     agent_permissions.
//   - owner_scopes — the narrowing applied to this group's STRUCTURAL
//     owner-implied permission bypass (TCL-1071), as a slug → scope map.
//     {} / null clears it, restoring the unrestricted bypass. It never
//     touches an explicit grant an owner holds.
//
// Partial-update contract, matching handleGroupMembersUpdate: only
// fields present (non-nil) in the body are touched. descr / default_cwd
// / default_context / default_profile are *string so a caller can clear
// any of them by sending "" — distinct from omitting it; max_members is
// *int and clears to "unlimited" with 0. An empty body (no field) is a
// 400.
//
// Each independently meaningful setting has its own permission. This keeps a
// default-directory grant from also authorizing a rename, context injection,
// or a member-cap change. groups.admin is accepted centrally as an umbrella.
//
// permissions AND owner_scopes are permission administration and additionally
// require permissions.grant + permissions.revoke — the same pair, because both
// fields can widen (clearing a narrowing) and narrow (adding one) in a single
// write.
func handleGroupUpdate(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	var body struct {
		Descr             *string                           `json:"descr,omitempty"`
		DefaultCwd        *string                           `json:"default_cwd,omitempty"`
		DefaultContext    *string                           `json:"default_context,omitempty"`
		Environment       *[]sandboxpolicy.EnvironmentEntry `json:"environment,omitempty"`
		DefaultSpawnGroup *bool                             `json:"default_spawn_group,omitempty"`
		// DefaultProfile names the spawn profile (JOH-210) whose launch fields
		// fill blank spawn fields for this group's agents; "" clears it. *string
		// so a caller can clear it by sending "" — distinct from omitting it.
		DefaultProfile *string `json:"default_profile,omitempty"`
		// DefaultPrompts is a complete replacement of the group's saved-prompt
		// set; [] clears it.
		DefaultPrompts *[]string `json:"default_prompts,omitempty"`
		// MaxMembers is the group's hard member cap; 0 = unlimited. A
		// negative value is clamped to 0 by db.SetAgentGroupMaxMembers.
		MaxMembers *int `json:"max_members,omitempty"`
		// NotifyEnabled is the group's OS-notification switch; false
		// mutes state-transition notifications for every member agent
		// (a per-agent 'on' pref still overrides).
		NotifyEnabled *bool `json:"notify_enabled,omitempty"`
		// RemoteControlPolicy is the group's remote-control policy that overrides
		// a spawn profile's remote-control default (JOH-262): "inherit" (defer to
		// the profile), "optin" (force on) or "deny" (force off). *string so a
		// caller can change it without touching the other fields; omitting it
		// leaves the policy unchanged.
		RemoteControlPolicy *string `json:"remote_control_policy,omitempty"`
		// ReinjectAfterCompact sets what tclaude re-injects into member agents
		// after a compaction or /clear: "contexts", "identity" or "off".
		ReinjectAfterCompact *string `json:"reinject_after_compact,omitempty"`
		// Permissions is a complete replacement allowlist. Pointer distinguishes
		// omitted (unchanged) from [] (clear every group grant).
		Permissions *[]db.PermissionGrant `json:"permissions,omitempty"`
		// OwnerScopes is a complete replacement owner-scope map for this group
		// (TCL-1071): slug → the scope that slug's owner-implied bypass is
		// confined to here. Raw so {} and null both reach the parser and both
		// mean "no narrowing"; the pointer distinguishes omitted (unchanged)
		// from an explicit clear.
		OwnerScopes *json.RawMessage `json:"owner_scopes,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	if body.Descr == nil && body.DefaultCwd == nil && body.DefaultContext == nil && body.Environment == nil && body.DefaultSpawnGroup == nil && body.DefaultProfile == nil && body.DefaultPrompts == nil && body.MaxMembers == nil && body.NotifyEnabled == nil && body.RemoteControlPolicy == nil && body.ReinjectAfterCompact == nil && body.Permissions == nil && body.OwnerScopes == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"nothing to update (expected descr, default_cwd, default_context, environment, default_spawn_group, default_profile, default_prompts, max_members, notify_enabled, remote_control_policy, reinject_after_compact, permissions and/or owner_scopes)")
		return
	}
	required := make([]string, 0, 10)
	if body.Descr != nil {
		required = append(required, PermGroupsSettingsDescription)
	}
	if body.DefaultCwd != nil {
		required = append(required, PermGroupsSettingsDefaultDir)
	}
	// The re-injection mode governs what happens to the startup context after
	// a compaction, so it shares the startup-context slug, as do the default
	// prompts: both are text every fresh member receives at spawn.
	if body.DefaultContext != nil || body.ReinjectAfterCompact != nil || body.DefaultPrompts != nil {
		required = append(required, PermGroupsSettingsDefaultContext)
	}
	if body.Environment != nil {
		required = append(required, PermGroupsSettingsEnvironment)
	}
	if body.DefaultSpawnGroup != nil {
		required = append(required, PermGroupsSettingsDefaultSpawnTarget)
	}
	if body.DefaultProfile != nil {
		required = append(required, PermGroupsSettingsDefaultProfile)
	}
	if body.MaxMembers != nil {
		required = append(required, PermGroupsSettingsMaxMembers)
	}
	if body.NotifyEnabled != nil {
		required = append(required, PermGroupsSettingsNotifications)
	}
	if body.RemoteControlPolicy != nil {
		required = append(required, PermGroupsSettingsRemoteControlPolicy)
	}
	if body.Permissions != nil {
		required = append(required, PermGroupsSettingsMemberPermissions)
	}
	if body.OwnerScopes != nil {
		required = append(required, PermGroupsSettingsOwnerScopes)
	}
	caller := ""
	for _, perm := range required {
		resolved, ok := requireGroupPermission(w, r, perm, g, ActionContext{Group: g.Name})
		if !ok {
			return
		}
		caller = resolved
	}
	if !requireGroupActive(w, g) {
		return
	}
	if a := peerGrantFromRequest(r); a != nil && body.DefaultProfile != nil {
		name, err := resolveGroupDefaultProfileName(*body.DefaultProfile)
		if err == nil {
			err = a.allowsDefaultProfile(name)
		}
		if err != nil {
			writeError(w, http.StatusForbidden, "profile_not_allowed", "a peer may set only a profile it may pick for a spawn into this group")
			return
		}
	}
	if a := peerGrantFromRequest(r); a != nil && body.DefaultPrompts != nil && !a.allowsDefaultPrompts() {
		writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("this node does not grant you %s for group:%s", PermGroupsMembersSpawnPrompts, g.Name))
		return
	}
	var defaultPrompts []string
	if body.DefaultPrompts != nil {
		// Validate before any field is written so a bad name cannot leave a
		// partial settings update behind.
		var fail *spawnFailure
		if defaultPrompts, fail = resolveSavedPromptRefs(*body.DefaultPrompts); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
	}
	if body.Permissions != nil || body.OwnerScopes != nil {
		// Authorize the higher-impact fields before applying ANY part of a mixed
		// PATCH, so a caller cannot get a partial settings update followed by a
		// permission-administration 403.
		if _, ok := requirePermission(w, r, PermPermissionsGrant); !ok {
			return
		}
		if _, ok := requirePermission(w, r, PermPermissionsRevoke); !ok {
			return
		}
	}
	// Validate + canonicalize the owner-scope map before any write lands, for
	// the same reason: a rejected map must not follow a committed descr.
	ownerScopes := ""
	if body.OwnerScopes != nil {
		canonical, err := canonicalOwnerScopes(string(*body.OwnerScopes))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_owner_scopes", err.Error())
			return
		}
		ownerScopes = canonical
	}
	var normalizedEnvironment []sandboxpolicy.EnvironmentEntry
	if body.Environment != nil {
		var err error
		normalizedEnvironment, err = sandboxpolicy.NormalizeEnvironment(*body.Environment)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_environment", err.Error())
			return
		}
	}
	reinjectMode := ""
	if body.ReinjectAfterCompact != nil {
		reinjectMode = strings.TrimSpace(*body.ReinjectAfterCompact)
		if reinjectMode == "" || !db.ValidReinjectAfterCompact(reinjectMode) {
			writeError(w, http.StatusBadRequest, "invalid_reinject_after_compact",
				fmt.Sprintf("invalid reinject_after_compact %q (want contexts, identity, or off)", reinjectMode))
			return
		}
	}
	var normalizedPermissions []db.PermissionGrant
	if body.Permissions != nil {
		var failure *spawnFailure
		normalizedPermissions, failure = normalizeBlueprintGrants(*body.Permissions)
		if failure != nil {
			writeError(w, failure.Status, failure.Kind, failure.Msg)
			return
		}
		sort.Slice(normalizedPermissions, func(i, j int) bool {
			return normalizedPermissions[i].Slug < normalizedPermissions[j].Slug
		})
		// A group grant is conferred on every current AND future member, so it is
		// a minting surface. An agent caller must clear the same attenuation bar
		// as the spawn boundary and /v1/permissions/grant. Without this an agent
		// whose own hold on a slug is scoped could add that slug to a group it
		// belongs to and, on the very next request, resolve unscoped: the group
		// tier unions its rows, and one unscoped row absorbs the tier.
		conferred := make([]conferredGrant, 0, len(normalizedPermissions))
		for _, grant := range normalizedPermissions {
			conferred = append(conferred, conferredGrant{Slug: grant.Slug, Scope: grant.Scope})
		}
		if err := checkGrantAttenuation(caller, grantConferee{}, conferred); err != nil {
			writeError(w, http.StatusForbidden, "scope_not_attenuated", err.Error())
			return
		}
	}
	// Every field is validated before the first write, so a rejected save leaves
	// nothing behind.
	var cwd, ctx, profile string
	if body.DefaultCwd != nil {
		// Normalise + validate: expand "~", require an absolute path (a
		// relative default would resolve against the daemon's cwd at
		// spawn time, which is meaningless). Empty stays empty — clears it.
		var err error
		if cwd, err = resolveGroupDefaultCwd(*body.DefaultCwd); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
			return
		}
	}
	if body.DefaultContext != nil {
		// Fold CRLF → LF and enforce the size cap. Empty stays empty —
		// that clears the group context.
		var err error
		if ctx, err = normalizeGroupContext(*body.DefaultContext); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
	}
	if body.DefaultProfile != nil {
		// Validate only that the referenced profile exists (its launch fields
		// were validated against their own harness at save) — the harness-correct
		// replacement for the retired Claude-only default_model gate (#343).
		// Empty stays empty — that clears the group default.
		var err error
		if profile, err = resolveGroupDefaultProfileName(*body.DefaultProfile); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_profile", err.Error())
			return
		}
		if fail := defaultProfileConferralFailure(r, caller, g.Name, profile); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
	}
	var policy *bool
	if body.RemoteControlPolicy != nil {
		var err error
		if policy, err = parseRemoteControlPolicy(*body.RemoteControlPolicy); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_remote_control_policy", err.Error())
			return
		}
	}
	resp := map[string]any{"group": g.Name}
	// The writes are not one transaction, so a failure part way through has
	// changed something, and a sudo use spent on it stays spent.
	defer func() {
		if len(resp) > 1 {
			keepSudoUses(r)
		}
	}()

	if body.Descr != nil {
		// The group descr is a one-line label. Fold any embedded
		// newline (an API caller could send one — the CLI positional
		// and the dashboard's <input type=text> cannot) to a space so
		// it never breaks the single-line dashboard header. Empty
		// stays empty — that clears the description.
		descr := normalizeGroupDescr(*body.Descr)
		n, err := db.SetAgentGroupDescr(g.Name, descr)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		// Zero rows: the group was renamed or deleted between the
		// dispatcher's lookup and this update. Report not_found rather
		// than a misleading 200.
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["descr"] = descr
	}

	if body.DefaultCwd != nil {
		n, err := db.SetAgentGroupDefaultCwd(g.Name, cwd)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		// Zero rows: the group was renamed or deleted between the
		// dispatcher's lookup and this update. Report not_found rather
		// than a misleading 200.
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["default_cwd"] = cwd
	}

	if body.DefaultSpawnGroup != nil {
		n, err := db.SetAgentGroupDefaultSpawn(g.Name, *body.DefaultSpawnGroup)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_default_spawn_group", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["default_spawn_group"] = *body.DefaultSpawnGroup
	}

	if body.DefaultContext != nil {
		n, err := db.SetAgentGroupDefaultContext(g.Name, ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["default_context"] = ctx
	}

	if body.Environment != nil {
		n, err := db.SetAgentGroupEnvironment(g.Name, normalizedEnvironment)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["environment"] = normalizedEnvironment
	}

	if body.DefaultProfile != nil {
		n, err := db.SetAgentGroupDefaultProfile(g.Name, profile)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["default_profile"] = profile
	}

	if body.DefaultPrompts != nil {
		n, err := db.SetAgentGroupDefaultPrompts(g.Name, defaultPrompts)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["default_prompts"] = defaultPrompts
	}

	if body.MaxMembers != nil {
		// db.SetAgentGroupMaxMembers clamps a negative value to 0
		// (unlimited) rather than rejecting it, so a careless caller
		// can never wedge a group with an impossible cap.
		n, err := db.SetAgentGroupMaxMembers(g.Name, *body.MaxMembers)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		stored := *body.MaxMembers
		if stored < 0 {
			stored = 0
		}
		resp["max_members"] = stored
	}

	if body.NotifyEnabled != nil {
		n, err := db.SetAgentGroupNotifyEnabled(g.Name, *body.NotifyEnabled)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["notify_enabled"] = *body.NotifyEnabled
	}

	if body.RemoteControlPolicy != nil {
		n, err := db.SetAgentGroupRemoteControl(g.Name, policy)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["remote_control_policy"] = remoteControlPolicyToWire(policy)
	}

	if body.ReinjectAfterCompact != nil {
		// Validated above, before any write landed.
		n, err := db.SetAgentGroupReinjectAfterCompact(g.Name, reinjectMode)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["reinject_after_compact"] = reinjectMode
	}

	if body.Permissions != nil {
		// Changing a group allowlist is permission administration, not ordinary
		// cosmetic group editing. Both meta-slugs were checked before mutations
		// because a replacement can widen and narrow the set in one request.
		grantedBy := caller
		if grantedBy == "" {
			grantedBy = "<human>"
		}
		if err := db.ReplaceAgentGroupPermissionGrants(g.ID, normalizedPermissions, grantedBy); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		resp["permissions"] = normalizedPermissions
	}

	if body.OwnerScopes != nil {
		// Same class as the allowlist above, and gated on the same pair. The
		// canonical form was computed before any write landed, so this cannot
		// fail on validation here.
		n, err := db.SetAgentGroupOwnerScopes(g.Name, ownerScopes)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		resp["owner_scopes"] = ownerScopesWire(ownerScopes)
	}

	writeJSON(w, http.StatusOK, resp)
}

// ownerScopesWireOmitEmpty is ownerScopesWire for a field tagged omitempty: it
// answers nil rather than an empty map so an unnarrowed group serializes with
// no owner_scopes key at all.
func ownerScopesWireOmitEmpty(raw string) map[string]PermissionScope {
	out := ownerScopesWire(raw)
	if len(out) == 0 {
		return nil
	}
	return out
}

// ownerScopesWire renders a stored owner-scope map for a JSON response: the
// decoded object, or an empty object when the group imposes no narrowing. A
// raw string would force every client to double-decode, and json.RawMessage
// of "" is not valid JSON.
//
// A stored map this build cannot decode also renders empty — there is nothing
// typed to show — so it is LOGGED here rather than passing silently. The
// display is lossy in that case, but it cannot destroy the stored value: an
// editor sends owner_scopes only when its box was edited, and an omitted field
// leaves the row untouched.
func ownerScopesWire(raw string) map[string]PermissionScope {
	out := map[string]PermissionScope{}
	scopes, err := ownerScopesForEval(raw)
	if err != nil {
		slog.Warn("permissions: stored owner scopes do not decode on this build; "+
			"rendering them as absent (the gate still fails their bypass closed)",
			"error", err)
		return out
	}
	for slug, scope := range scopes {
		out[slug] = scope
	}
	return out
}

// parseRemoteControlPolicy maps a group remote-control policy wire token to the
// tri-state *bool stored on the group (JOH-262): "" / "inherit" → nil (defer to
// the spawn profile's default), "optin" / "on" → true (force Remote Access on),
// "deny" / "off" → false (force it off). Any other token is an error so a typo
// can't silently land as "inherit". Tolerant of a few synonyms so the CLI and
// dashboard can speak the operator's words.
func parseRemoteControlPolicy(s string) (*bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "inherit", "unset", "default":
		return nil, nil
	case "optin", "opt-in", "on", "enable", "enabled", "true":
		v := true
		return &v, nil
	case "deny", "off", "disable", "disabled", "false":
		v := false
		return &v, nil
	default:
		return nil, fmt.Errorf("invalid remote_control_policy %q (want inherit, optin, or deny)", s)
	}
}

// remoteControlPolicyToWire is the inverse of parseRemoteControlPolicy: it maps
// the stored tri-state *bool back to its canonical wire token for read surfaces
// (group GET, dashboard payload). nil → "inherit", true → "optin", false → "deny".
func remoteControlPolicyToWire(p *bool) string {
	switch {
	case p == nil:
		return "inherit"
	case *p:
		return "optin"
	default:
		return "deny"
	}
}

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
