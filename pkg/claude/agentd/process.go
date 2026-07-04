package agentd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Advisory process runtime (JOH-242). A template carries a declarative,
// ORDERED process spec (phases of {name, roles, criteria}); a group
// instantiated from it snapshots that spec into advisory runtime state — a
// current phase plus an append-only transition log. It is EXPLICITLY advisory:
// no gates, no phase-scoped permissions, no auto-advancement. The runtime only
// records the phase, surfaces it, and nudges the entering roles on advance.
//
// Wire surface (group-scoped, under the existing /v1/groups/{name} family):
//
//	GET  /v1/groups/{name}/process          → current phase + phase map + log
//	POST /v1/groups/{name}/process/advance  → move to the next (or a named) phase
//
// The GET is open (read-only introspection, like the other group reads);
// advance is gated: the human always, group owners of the group (the
// owner-pass every group-lifecycle verb uses), and otherwise the
// process.advance slug.

// roleLabelMatches reports whether two role labels are the same, the
// case-insensitive trimmed-equality rule the work-pattern --role routing uses
// (handlers.go). Extracted so the process phase-role matcher and the message
// router share ONE rule rather than inventing a second.
func roleLabelMatches(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// phaseRoleAll is the reserved phase-role token that marks every member active
// in a phase — the same broadcast sense "all" carries as a work-pattern
// send_to target and the reason "all" is a reserved role name.
const phaseRoleAll = "all"

// phaseActiveForRole reports whether a member with role memberRole is active in
// a phase whose active roles are phaseRoles. A phase listing the reserved
// "all" makes EVERY member active (regardless of role, even an unrolled one);
// otherwise a member is active when its role matches one of the phase's roles
// case-insensitively.
func phaseActiveForRole(phaseRoles []string, memberRole string) bool {
	for _, r := range phaseRoles {
		if roleLabelMatches(r, phaseRoleAll) {
			return true
		}
		if memberRole != "" && roleLabelMatches(r, memberRole) {
			return true
		}
	}
	return false
}

// activePhaseNames returns, in order, the names of the phases a member with
// role memberRole is active in — used both for the ## Process per-agent callout
// and for reasoning about who a phase-entry nudge reaches.
func activePhaseNames(phases []db.ProcessPhase, memberRole string) []string {
	out := []string{}
	for _, ph := range phases {
		if phaseActiveForRole(ph.Roles, memberRole) {
			out = append(out, ph.Name)
		}
	}
	return out
}

// appendProcessBlock folds a template's process spec into an agent's startup
// context as a trailing "## Process" section (JOH-242), rendered per agent so
// the block can call out which phases THIS agent's role is active in. A no-op
// (returns the context unchanged) when the process is empty. memberRole is the
// agent's role label; a phase's roles match it case-insensitively, and a phase
// listing "all" is active for everyone.
func appendProcessBlock(groupContext string, phases []db.ProcessPhase, memberRole string) string {
	if len(phases) == 0 {
		return groupContext
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Process\n\n")
	fmt.Fprintf(&b, "This group follows an advisory %d-phase process (its quest plan). "+
		"It is tracked and surfaced but NOT enforced — coordinate among yourselves; advancing a phase is a "+
		"deliberate act (`tclaude agent process advance`). The phases, in order:\n\n", len(phases))
	for i, ph := range phases {
		roles := "(any)"
		if len(ph.Roles) > 0 {
			roles = strings.Join(ph.Roles, ", ")
		}
		fmt.Fprintf(&b, "%d. **%s** — active roles: %s\n", i+1, ph.Name, roles)
		if ph.Criteria != "" {
			for _, line := range strings.Split(ph.Criteria, "\n") {
				fmt.Fprintf(&b, "   %s\n", line)
			}
		}
	}
	active := activePhaseNames(phases, memberRole)
	b.WriteString("\n")
	roleLabel := strings.TrimSpace(memberRole)
	switch {
	case len(active) == 0 && roleLabel == "":
		b.WriteString("You have no role label, so no phase calls you out as specifically active.")
	case len(active) == 0:
		fmt.Fprintf(&b, "Your role (%q) is not specifically called out as active in any phase.", roleLabel)
	case roleLabel == "":
		fmt.Fprintf(&b, "You are active in phase(s): %s.", strings.Join(active, ", "))
	default:
		fmt.Fprintf(&b, "Your role (%q) is active in phase(s): %s.", roleLabel, strings.Join(active, ", "))
	}
	section := b.String()
	if strings.TrimSpace(groupContext) == "" {
		return section
	}
	return groupContext + "\n\n" + section
}

// phaseLabel renders a group's current phase as a compact "phase <n>/<m>:
// <name>" chip (1-based index), the shared format the dashboard chip and
// whoami both show. Falls back to just the phase name when the current phase
// has drifted out of the snapshot (index unknown).
func phaseLabel(st *db.GroupProcessState) string {
	idx := st.PhaseIndex()
	if idx < 0 {
		return "phase: " + st.CurrentPhase
	}
	return fmt.Sprintf("phase %d/%d: %s", idx+1, len(st.Process), st.CurrentPhase)
}

// --- wire shapes ---

type processPhaseView struct {
	Name     string   `json:"name"`
	Roles    []string `json:"roles"`
	Criteria string   `json:"criteria,omitempty"`
	// Current marks the phase the group is presently in.
	Current bool `json:"current,omitempty"`
}

type processTransitionView struct {
	From  string `json:"from"`
	To    string `json:"to"`
	At    string `json:"at,omitempty"`
	Actor string `json:"actor,omitempty"`
}

// processStateJSON is the wire shape of a group's advisory process state: the
// current phase (name + 0-based index + count for a "phase 2/5" chip), the
// full ordered phase map, and the transition log. Slices are always non-nil so
// a JS .map() never trips.
type processStateJSON struct {
	CurrentPhase   string                  `json:"current_phase"`
	PhaseIndex     int                     `json:"phase_index"`
	PhaseCount     int                     `json:"phase_count"`
	PhaseStartedAt string                  `json:"phase_started_at,omitempty"`
	Phases         []processPhaseView      `json:"phases"`
	Transitions    []processTransitionView `json:"transitions"`
}

// processStateToJSON projects a db.GroupProcessState + its transition log onto
// the wire shape.
func processStateToJSON(st *db.GroupProcessState, transitions []db.GroupProcessTransition) processStateJSON {
	out := processStateJSON{
		CurrentPhase: st.CurrentPhase,
		PhaseIndex:   st.PhaseIndex(),
		PhaseCount:   len(st.Process),
		Phases:       []processPhaseView{},
		Transitions:  []processTransitionView{},
	}
	if !st.PhaseStartedAt.IsZero() {
		out.PhaseStartedAt = st.PhaseStartedAt.Format(time.RFC3339)
	}
	for _, ph := range st.Process {
		roles := ph.Roles
		if roles == nil {
			roles = []string{}
		}
		out.Phases = append(out.Phases, processPhaseView{
			Name:     ph.Name,
			Roles:    roles,
			Criteria: ph.Criteria,
			Current:  ph.Name == st.CurrentPhase,
		})
	}
	for _, tr := range transitions {
		v := processTransitionView{From: tr.FromPhase, To: tr.ToPhase, Actor: tr.Actor}
		if !tr.At.IsZero() {
			v.At = tr.At.Format(time.RFC3339)
		}
		out.Transitions = append(out.Transitions, v)
	}
	return out
}

// loadGroupProcess is the shared "fetch state + transitions" read behind the
// GET handler, the advance response, and the dashboard snapshot. Returns
// (nil, nil, nil) when the group has no process.
func loadGroupProcess(groupID int64) (*db.GroupProcessState, []db.GroupProcessTransition, error) {
	st, err := db.GetGroupProcessState(groupID)
	if err != nil {
		return nil, nil, err
	}
	if st == nil {
		return nil, nil, nil
	}
	trs, err := db.ListGroupProcessTransitions(groupID)
	if err != nil {
		return nil, nil, err
	}
	return st, trs, nil
}

// handleGroupProcessGet serves GET /v1/groups/{name}/process: the group's
// advisory process state, or 404 when the group has no process. Open +
// read-only (introspection), like the other group reads.
func handleGroupProcessGet(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET")
		return
	}
	st, trs, err := loadGroupProcess(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if st == nil {
		writeError(w, http.StatusNotFound, "no_process", "this group has no process")
		return
	}
	writeJSON(w, http.StatusOK, processStateToJSON(st, trs))
}

// handleGroupProcessAdvance serves POST /v1/groups/{name}/process/advance: move
// the group to the NEXT phase, or — for correction — to an explicitly named
// phase (body {"to": "<phase>"}). Still advisory: it records the transition
// with the acting identity and nudges the newly-entering roles, but enforces
// nothing.
//
// Gated with requireGroupPermission(process.advance): the human always passes,
// a group owner passes structurally (the owner-bypass every group-lifecycle
// verb uses), and any other agent needs the process.advance slug.
func handleGroupProcessAdvance(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST")
		return
	}
	caller, ok := requireGroupPermission(w, r, PermProcessAdvance, g)
	if !ok {
		return
	}

	var body struct {
		To string `json:"to,omitempty"`
	}
	// An empty body is fine (advance to the next phase); only reject malformed
	// JSON when a body is present.
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
	}

	st, err := db.GetGroupProcessState(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if st == nil {
		writeError(w, http.StatusNotFound, "no_process", "this group has no process to advance")
		return
	}

	target, fail := resolveAdvanceTarget(st, strings.TrimSpace(body.To))
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}

	actor := granterLabel(caller)
	// AdvanceGroupProcess returns the phase actually moved FROM, read inside its
	// own transaction — so the response reports the true recorded transition
	// even if a concurrent advance interleaved after resolveAdvanceTarget's
	// pre-read.
	fromPhase, err := db.AdvanceGroupProcess(g.ID, target.Name, actor)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "no_process", "this group has no process to advance")
			return
		}
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}

	// Nudge the members whose role is active in the NEWLY-ENTERED phase (the
	// "entering" roles), reusing the existing agent_messages pipeline — no
	// direct send-keys, no new delivery mechanism. Best-effort: a delivery
	// failure is logged, never fails the advance (the transition is recorded).
	notified := notifyEnteringRoles(g, fromPhase, target, caller)

	freshState, trs, err := loadGroupProcess(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	resp := map[string]any{
		"group":    g.Name,
		"from":     fromPhase,
		"to":       target.Name,
		"notified": notified,
	}
	if freshState != nil {
		resp["state"] = processStateToJSON(freshState, trs)
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveAdvanceTarget picks the phase to move to. With an explicit `to` it
// validates the name is a real phase in the snapshot (case-insensitive, the
// same match everything else uses) and returns it. With no `to` it advances to
// the NEXT phase after the current one; a 409 when the current phase is already
// the last (nothing to advance to) or when the current phase name has drifted
// out of the snapshot.
func resolveAdvanceTarget(st *db.GroupProcessState, to string) (db.ProcessPhase, *spawnFailure) {
	if to != "" {
		for _, ph := range st.Process {
			if roleLabelMatches(ph.Name, to) {
				return ph, nil
			}
		}
		names := make([]string, 0, len(st.Process))
		for _, ph := range st.Process {
			names = append(names, ph.Name)
		}
		return db.ProcessPhase{}, &spawnFailure{http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("no phase named %q in this group's process. Phases: %s", to, strings.Join(names, ", "))}
	}
	idx := st.PhaseIndex()
	if idx < 0 {
		return db.ProcessPhase{}, &spawnFailure{http.StatusConflict, "phase_drift",
			fmt.Sprintf("current phase %q is not in the process snapshot — pass an explicit --to <phase>", st.CurrentPhase)}
	}
	if idx+1 >= len(st.Process) {
		return db.ProcessPhase{}, &spawnFailure{http.StatusConflict, "at_last_phase",
			fmt.Sprintf("already at the last phase (%q) — pass an explicit --to <phase> to move to a named phase", st.CurrentPhase)}
	}
	return st.Process[idx+1], nil
}

// notifyEnteringRoles fires an agent_messages nudge to every live member whose
// role is active in the phase just entered (JOH-242) — the "entering" roles.
// The message names old → new and carries the new phase's criteria prose. It
// reuses the existing message pipeline (which nudges live panes); a phase
// listing "all" reaches every member. Returns the number of members notified.
func notifyEnteringRoles(g *db.AgentGroup, fromPhase string, entered db.ProcessPhase, fromConv string) int {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		slog.Warn("process advance: list members failed", "group", g.Name, "error", err)
		return 0
	}
	targets := []string{}
	for _, m := range members {
		if m.ConvID == fromConv {
			continue // don't nudge the actor about its own advance
		}
		if phaseActiveForRole(entered.Roles, m.Role) {
			targets = append(targets, m.ConvID)
		}
	}
	if len(targets) == 0 {
		return 0
	}
	var body strings.Builder
	fmt.Fprintf(&body, "The group %q advanced its process", g.Name)
	if fromPhase != "" {
		fmt.Fprintf(&body, " from %q", fromPhase)
	}
	fmt.Fprintf(&body, " to %q — your role is active in this phase.", entered.Name)
	if entered.Criteria != "" {
		fmt.Fprintf(&body, "\n\nCriteria for %q:\n%s", entered.Name, entered.Criteria)
	}
	subject := "[process] phase: " + entered.Name
	notified := 0
	for _, conv := range targets {
		if _, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:      g.ID,
			FromConv:     fromConv,
			ToConv:       conv,
			Subject:      subject,
			Body:         body.String(),
			ToRecipients: targets,
		}); err != nil {
			slog.Warn("process advance: nudge insert failed", "group", g.Name, "conv", conv, "error", err)
			continue
		}
		notified++
		enqueueDeliveryForConv(conv)
	}
	return notified
}
