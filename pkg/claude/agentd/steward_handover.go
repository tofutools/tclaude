package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Handing the Steward role to a running agent. The role is its levels, the
// operator's defaults and what is stored for future Stewards, as for a summon;
// the agent keeps its conversation, harness, working directory, groups and
// whatever it already held. Exactly one Steward holds the seat that the address,
// the Config name and the companion follow. Others stand by on the roster,
// without Steward access, a click away from coming back.
//
// Every route here is the human's alone (requireHuman), with no permission slug
// and so no sudo or --ask-human path: an agent cannot make itself or another
// agent the Steward, nor move the seat. The message an agent might inject can at
// most ask the operator to.

const (
	// stewardRoleGranter audits a grant that came with the role, and marks it
	// as one a demotion takes back.
	stewardRoleGranter = "system:steward-role"

	stewardThenRoster = "roster"
	stewardThenKeep   = "keep"
	stewardThenRetire = "retire"
)

// isStewardRoleGrant reports whether a row exists only because of the role. The
// operator's own grant of the same slug carries their label, and stays. Every
// birth grant of a summoned Steward came with the role; a promoted agent's birth
// grants, if it was summoned as another scribe, did not.
func isStewardRoleGrant(row db.AgentPermission, offered map[string]bool) bool {
	return row.GrantedBy == stewardRoleGranter || row.GrantedBy == stewardReconcileGranter ||
		(!offered[db.StewardRecordPromoted] && strings.Contains(row.GrantedBy, ":scribe-summon:"))
}

// stewardAccessPlan is what the role would change on one agent.
type stewardAccessPlan struct {
	// Grant are the rows to write, Gains the slugs it does not reach today.
	Grant map[string]db.PermissionOverride
	Gains []string
	// Denied stay denied: an override on a slug stands, as in a reconcile.
	Denied []string
	// Withheld were taken off it in an earlier time as Steward, and stay off.
	Withheld []string
}

func planStewardAccess(cfg *config.Config, conv, agentID string, state permissionsState) (*stewardAccessPlan, []string, error) {
	wanted := map[string]db.PermissionOverride{}
	for _, slug := range stewardLevelSlugs(cfg) {
		wanted[slug] = db.Grant()
	}
	mergeStewardCarried(wanted)
	overrides, err := agentPermissionOverrides(conv)
	if err != nil {
		return nil, nil, err
	}
	offered, err := db.ListStewardOfferedPermissions(agentID)
	if err != nil {
		return nil, nil, err
	}
	effective, _, _, _ := effectivePermsFor(state, conv, ownerImpliedSlugsFor(conv))
	plan := &stewardAccessPlan{Grant: map[string]db.PermissionOverride{}}
	for _, slug := range slices.Sorted(maps.Keys(wanted)) {
		held, overridden := overrides[slug]
		switch {
		case overridden && held.Effect == db.PermEffectDeny:
			plan.Denied = append(plan.Denied, slug)
		case overridden:
		case offered[slug]:
			plan.Withheld = append(plan.Withheld, slug)
		default:
			plan.Grant[slug] = wanted[slug]
			if wanted[slug].Effect == db.PermEffectGrant && !slices.Contains(effective, slug) {
				plan.Gains = append(plan.Gains, slug)
			}
		}
	}
	return plan, slices.Sorted(maps.Keys(wanted)), nil
}

// stewardDemotion is what leaving the role takes and leaves.
type stewardDemotion struct {
	Loses []string `json:"loses"`
	Keeps []string `json:"keeps"`
	// rows are the overrides to delete, held all it still has a row for: only a
	// slug with no row left is one the operator took off.
	rows, held []string
}

func readStewardDemotion(conv, agentID string) (*stewardDemotion, error) {
	rows, err := db.ListAgentPermissionOverrideRowsForConv(conv)
	if err != nil {
		return nil, err
	}
	offered, err := db.ListStewardOfferedPermissions(agentID)
	if err != nil {
		return nil, err
	}
	sudo, err := db.ListActiveSudoGrants(conv)
	if err != nil {
		return nil, err
	}
	out := &stewardDemotion{Loses: []string{}, Keeps: []string{}}
	for _, row := range rows {
		out.held = append(out.held, row.Slug)
		switch {
		case isStewardRoleGrant(row, offered):
			out.rows = append(out.rows, row.Slug)
			if row.Effect == db.PermEffectGrant {
				out.Loses = append(out.Loses, row.Slug)
			}
		case row.Effect == db.PermEffectGrant:
			out.Keeps = append(out.Keeps, row.Slug)
		}
	}
	for _, grant := range sudo {
		if grant.GrantedBy == stewardCarriedGranter {
			out.Loses = append(out.Loses, grant.Slug+" (sudo)")
		} else {
			out.Keeps = append(out.Keeps, grant.Slug+" (sudo)")
		}
	}
	return out, nil
}

// demoteSteward takes a Steward out of the role. Only what the role gave goes,
// so what the operator granted it separately stays. Closing the record leaves
// the slugs the operator took off in it, so a return does not bring them back,
// and means that no member description can make it a Steward again.
func demoteSteward(g *db.AgentGroup, conv, agentID string) (*stewardDemotion, error) {
	demotion, err := readStewardDemotion(conv, agentID)
	if err != nil {
		return nil, err
	}
	for _, slug := range demotion.rows {
		if _, err := db.RevokeAgentPermission(conv, slug); err != nil {
			return nil, err
		}
	}
	if _, err := db.EndInheritedStewardSudo(agentID, stewardCarriedGranter); err != nil {
		return nil, err
	}
	if err := db.CloseStewardRecord(agentID, demotion.held...); err != nil {
		return nil, err
	}
	if err := db.RemoveAgentGroupMember(g.ID, conv); err != nil {
		return nil, err
	}
	return demotion, nil
}

// adoptSummonedSteward gives the operator's new Steward the seat. An offline
// Steward it replaces stands by on the roster without its access, ready to come
// back. It runs only once the new one exists, so a failed summon changes nothing.
func adoptSummonedSteward(conv string) {
	scribeSummonMu.Lock()
	defer scribeSummonMu.Unlock()
	g, fail := ensureScribeGroup(stewardGroupName)
	if fail != nil {
		slog.Warn("steward: read group failed", "error", fail.Msg)
		return
	}
	for _, offline := range stewardConvs() {
		if offline == conv || pickAliveSession(offline) != nil {
			continue
		}
		agentID, err := db.AgentIDForConv(offline)
		if err != nil || agentID == "" {
			continue
		}
		demotion, err := demoteSteward(g, offline, agentID)
		if err != nil {
			slog.Warn("steward: demote offline Steward failed", "conv", short8(offline), "error", err)
			continue
		}
		if err := db.SetStewardStandby(agentID, granterLabel("")); err != nil {
			slog.Warn("steward: put offline Steward on the roster failed", "conv", short8(offline), "error", err)
		}
		tellStoodDown(offline, agent.FreshTitle(conv), demotion, true, pauseStandbyJobs(agentID))
	}
	pruneDeadScribes(g, conv)
	agentID, err := db.AgentIDForConv(conv)
	if err != nil || agentID == "" {
		slog.Warn("steward: resolve agent for the seat failed", "conv", short8(conv), "error", err)
		return
	}
	resumeStandbyJobs(agentID)
	if err := db.SetActiveSteward(agentID, granterLabel("")); err != nil {
		slog.Warn("steward: seat failed", "conv", short8(conv), "error", err)
	}
}

// forgetRetiredSteward ends an agent's place on the roster with its retirement.
func forgetRetiredSteward(conv string) {
	agentID, err := db.AgentIDForConv(conv)
	if err != nil || agentID == "" {
		return
	}
	if err := db.ForgetSteward(agentID); err != nil {
		slog.Warn("steward: forget retired Steward failed", "conv", short8(conv), "error", err)
	}
}

// stewardRefusal says why an agent cannot take the role, "" when it can. A
// visiting agent answers to its home, and a plain shell has nothing to read
// the brief with. Other harnesses work through the capability gates each Steward
// surface already has; stewardHarnessNotes says what they lack.
func stewardRefusal(conv, agentID string, h *harness.Harness) string {
	if state, err := db.AgentState(conv); err != nil || state != db.AgentStateActive {
		return "only an active agent can be the Steward"
	}
	presence, err := db.GetAgentFederationPresence(agentID)
	if err != nil {
		return "could not read where this agent is"
	}
	if presence != nil && (presence.State != "here" || presence.HomeInstance != presence.CurrentInstance) {
		return "only an agent at home on this node can be its Steward"
	}
	if h != nil && h.Name == harness.ShellName {
		return "a plain shell cannot read a Steward's brief"
	}
	if db.IsExternalStewardConv(conv) {
		return "an external steward keeps its own permissions and cannot take the Steward role"
	}
	return ""
}

// stewardHarnessNotes are the Steward surfaces this harness does without.
func stewardHarnessNotes(h *harness.Harness) []string {
	if h != nil && h.Name == harness.OpenCodeName {
		return []string{"Voice stays off: it reads the agent's transcript, which OpenCode does not have."}
	}
	return nil
}

type stewardRosterEntry struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	// Seat is active, standby, or alongside for a Steward running without the
	// seat, as one summoned before the operator chose.
	Seat    string `json:"seat"`
	Online  bool   `json:"online"`
	Harness string `json:"harness,omitempty"`
	// LastRan is when an offline one was last seen running.
	LastRan string `json:"last_ran,omitempty"`
	// Keeps is what an offline one would keep if a new Steward replaced it:
	// what the operator gave it separately.
	Keeps []string `json:"keeps,omitempty"`
}

// stewardRosterView lists the active Steward, then the others, in seat order.
func stewardRosterView() []stewardRosterEntry {
	active := activeStewardConv()
	stored, err := db.ListStewardRoster()
	if err != nil {
		slog.Warn("steward: read roster failed", "error", err)
	}
	entry := func(conv, seat string) (stewardRosterEntry, bool) {
		agentID, err := db.AgentIDForConv(conv)
		if err != nil || agentID == "" {
			return stewardRosterEntry{}, false
		}
		out := stewardRosterEntry{AgentID: agentID, Name: agent.FreshTitle(conv), Seat: seat, Online: pickAliveSession(conv) != nil}
		if h := harnessForConv(conv); h != nil {
			out.Harness = h.Name
		}
		if !out.Online {
			if sess, err := db.FindSessionByConvID(conv); err == nil && sess != nil {
				out.LastRan = sess.UpdatedAt.UTC().Format(time.RFC3339)
			}
			if seat != db.StewardSeatStandby {
				if demotion, err := readStewardDemotion(conv, agentID); err == nil {
					out.Keeps = demotion.Keeps
				}
			}
		}
		return out, true
	}
	out := []stewardRosterEntry{}
	if active != "" {
		if e, ok := entry(active, db.StewardSeatActive); ok {
			out = append(out, e)
		}
	}
	for _, conv := range stewardConvs() {
		if conv == active {
			continue
		}
		if e, ok := entry(conv, "alongside"); ok {
			out = append(out, e)
		}
	}
	for _, row := range stored {
		if row.Seat != db.StewardSeatStandby {
			continue
		}
		conv, err := db.CurrentConvForAgent(row.AgentID)
		if err != nil || conv == "" {
			continue
		}
		if state, err := db.AgentState(conv); err != nil || state != db.AgentStateActive {
			continue
		}
		if e, ok := entry(conv, db.StewardSeatStandby); ok {
			out = append(out, e)
		}
	}
	return out
}

// stewardPrevious is the agent the seat would leave.
type stewardPrevious struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	Online  bool   `json:"online"`
	stewardDemotion
}

// stewardHandoverPreview is what the confirmation shows.
type stewardHandoverPreview struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	Harness string `json:"harness,omitempty"`
	Online  bool   `json:"online"`
	// Refused says why this agent cannot take the role.
	Refused string `json:"refused,omitempty"`
	// AlreadyActive is true when it holds the seat.
	AlreadyActive bool     `json:"already_active,omitempty"`
	Gains         []string `json:"gains"`
	// Extra is what it already holds beyond a Steward's defaults, which becomes
	// part of what the Steward can do.
	Extra       stewardElevation `json:"extra"`
	ExtraSudo   []string         `json:"extra_sudo,omitempty"`
	Denied      []string         `json:"denied,omitempty"`
	Withheld    []string         `json:"withheld,omitempty"`
	Notes       []string         `json:"notes,omitempty"`
	Previous    *stewardPrevious `json:"previous,omitempty"`
	GrantsPower []string         `json:"grants_power,omitempty"`
}

func resolveStewardTarget(w http.ResponseWriter, selector string) (*agent.Resolved, bool) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		writeError(w, http.StatusBadRequest, "agent", "agent is required")
		return nil, false
	}
	res, candidates, err := agent.ResolveSelector(selector)
	if err != nil || res == nil {
		if len(candidates) > 1 {
			writeError(w, http.StatusConflict, "ambiguous", "that matches more than one agent; use its id")
			return nil, false
		}
		writeError(w, http.StatusNotFound, "not_found", "no such agent")
		return nil, false
	}
	if res.AgentID == "" {
		writeError(w, http.StatusConflict, "conflict", "only an agent can be the Steward")
		return nil, false
	}
	return res, true
}

func stewardPreview(cfg *config.Config, res *agent.Resolved) (*stewardHandoverPreview, error) {
	conv, agentID := res.ConvID, res.AgentID
	h := harnessForConv(conv)
	state, err := snapshotPermissions()
	if err != nil {
		return nil, err
	}
	seat, err := db.ActiveStewardAgent()
	if err != nil {
		return nil, err
	}
	plan, _, err := planStewardAccess(cfg, conv, agentID, state)
	if err != nil {
		return nil, err
	}
	out := &stewardHandoverPreview{
		AgentID: agentID, Name: agent.FreshTitle(conv), Online: pickAliveSession(conv) != nil,
		Refused: stewardRefusal(conv, agentID, h), AlreadyActive: seat == agentID && slices.Contains(stewardConvs(), conv),
		Gains: append([]string{}, plan.Gains...), Denied: plan.Denied, Withheld: plan.Withheld,
		Notes: stewardHarnessNotes(h), Extra: stewardElevationOf(cfg, state, conv),
	}
	if h != nil {
		out.Harness = h.Name
	}
	sudo, err := db.ListActiveSudoGrants(conv)
	if err != nil {
		return nil, err
	}
	for _, grant := range sudo {
		out.ExtraSudo = append(out.ExtraSudo, grant.Slug)
	}
	for _, slug := range append(slices.Clone(out.Extra.Standing), append(out.Extra.Inherited, out.ExtraSudo...)...) {
		if slices.Contains(sudoDefaultBlocklist, slug) {
			out.GrantsPower = append(out.GrantsPower, slug)
		}
	}
	if prev := activeStewardConv(); prev != "" && prev != conv {
		prevAgent, err := db.AgentIDForConv(prev)
		if err != nil {
			return nil, err
		}
		demotion, err := readStewardDemotion(prev, prevAgent)
		if err != nil {
			return nil, err
		}
		out.Previous = &stewardPrevious{AgentID: prevAgent, Name: stewardDisplayName(cfg, prev), Online: pickAliveSession(prev) != nil, stewardDemotion: *demotion}
	}
	return out, nil
}

func stewardDisplayName(cfg *config.Config, conv string) string {
	if name := cfg.StewardName(); name != "" {
		return name
	}
	return agent.FreshTitle(conv)
}

// grantStewardRole gives an agent the role: its access, the group everything that
// recognises a Steward looks in, and an open record, in that order, so a failure
// leaves it either with nothing new or with what demoting takes back.
func grantStewardRole(cfg *config.Config, g *db.AgentGroup, conv, agentID string) error {
	state, err := snapshotPermissions()
	if err != nil {
		return err
	}
	wasSteward := slices.Contains(stewardConvs(), conv)
	plan, offered, err := planStewardAccess(cfg, conv, agentID, state)
	if err != nil {
		return err
	}
	if len(plan.Grant) > 0 {
		if err := db.ApplyAgentPermissionOverrides(conv, plan.Grant, stewardRoleGranter, false, true); err != nil {
			return err
		}
	}
	record := append(offered, db.StewardRecordOpened)
	if !wasSteward {
		record = append(record, db.StewardRecordPromoted)
	}
	if err := db.OfferStewardPermissions(agentID, record...); err != nil {
		return err
	}
	if !wasSteward {
		if err := db.AddAgentGroupMember(&db.AgentGroupMember{
			GroupID: g.ID, ConvID: conv, Role: scribeScopeRole(stewardScopeKind + "/"), Descr: scribeScopeDescr(stewardScopeKind + "/"),
		}); err != nil {
			if _, undo := demoteSteward(g, conv, agentID); undo != nil {
				slog.Warn("steward: undo the role failed", "conv", short8(conv), "error", undo)
			}
			return err
		}
		inheritStewardCarriedSudo(conv)
	}
	return nil
}

// stewardHandoverOutcome reports a finished handover.
type stewardHandoverOutcome struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	// Previous and Then say what became of the agent that left the seat.
	Previous string `json:"previous,omitempty"`
	Then     string `json:"then,omitempty"`
	// Warning is a step that failed after the seat moved.
	Warning string `json:"warning,omitempty"`
}

// handOverSteward moves the seat to an agent. then says what becomes of the agent
// leaving it. Its retirement waits for the session to stop, so it runs last and
// outside the lock.
func handOverSteward(cfg *config.Config, res *agent.Resolved, then, by, auditEventID string) (*stewardHandoverOutcome, *spawnFailure) {
	scribeSummonMu.Lock()
	out, retiring, fail := seatSteward(cfg, res, then, by)
	scribeSummonMu.Unlock()
	if fail != nil {
		return nil, fail
	}
	if retiring != "" {
		_, ownerGroups, err := retireAgentConv(retiring, by, "the operator replaced this Steward")
		if err != nil {
			out.Warning = "could not retire the old Steward: " + err.Error()
			return out, nil
		}
		owned := map[int64]bool{}
		for _, id := range ownerGroups {
			owned[id] = true
		}
		out.Warning = strings.Join(warnOwnerlessGroups(owned), " ")
		finishRetiredConv(retiring, true, false, agentWorktreeView{}, auditEventID)
	}
	return out, nil
}

func seatSteward(cfg *config.Config, res *agent.Resolved, then, by string) (*stewardHandoverOutcome, string, *spawnFailure) {
	conv, agentID := res.ConvID, res.AgentID
	if refusal := stewardRefusal(conv, agentID, harnessForConv(conv)); refusal != "" {
		return nil, "", &spawnFailure{http.StatusConflict, "conflict", refusal}
	}
	seat, err := db.ActiveStewardAgent()
	if err != nil {
		return nil, "", &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	if seat == agentID && slices.Contains(stewardConvs(), conv) {
		return nil, "", &spawnFailure{http.StatusConflict, "conflict", "this agent is already the active Steward"}
	}
	g, fail := ensureScribeGroup(stewardGroupName)
	if fail != nil {
		return nil, "", fail
	}
	previous := activeStewardConv()
	wasSteward := slices.Contains(stewardConvs(), conv)
	if err := grantStewardRole(cfg, g, conv, agentID); err != nil {
		return nil, "", &spawnFailure{http.StatusInternalServerError, "io", "give the Steward role: " + err.Error()}
	}
	if err := db.SetActiveSteward(agentID, by); err != nil {
		if !wasSteward {
			if _, undo := demoteSteward(g, conv, agentID); undo != nil {
				slog.Warn("steward: undo the role failed", "conv", short8(conv), "error", undo)
			}
		}
		return nil, "", &spawnFailure{http.StatusInternalServerError, "io", "take the seat: " + err.Error()}
	}
	resumed := resumeStandbyJobs(agentID)
	name := agent.FreshTitle(conv)
	out := &stewardHandoverOutcome{AgentID: agentID, Name: name}
	note := "The operator made you their Steward. You keep this conversation, your working directory, your groups and what you already held. " +
		"Finish or park what you were doing as they prefer. From now on this is your role:\n\n"
	if resumed > 0 {
		note = fmt.Sprintf("Your %d paused scheduled job(s) are running again. ", resumed) + note
	}
	if _, err := queueAgentMessage(&db.AgentMessage{GroupID: g.ID, ToConv: conv, Subject: "You are now the Steward",
		Body: stewardBrief(note+stewardRoleBrief(), cfg), ToRecipients: []string{conv}}); err != nil {
		out.Warning = "could not send the Steward's brief: " + err.Error()
	}
	autoFetchKnowledgeSource(cfg)
	if previous == "" || previous == conv {
		return out, "", nil
	}
	retiring, err := leaveStewardSeat(g, previous, name, then, by)
	out.Previous, out.Then = agent.FreshTitle(previous), then
	if err != nil {
		out.Warning = "the old Steward kept its access: " + err.Error()
		return out, "", nil
	}
	return out, retiring, nil
}

// leaveStewardSeat demotes a Steward and carries out the operator's choice for
// it, returning the conversation still to retire.
func leaveStewardSeat(g *db.AgentGroup, conv, successor, then, by string) (string, error) {
	agentID, err := db.AgentIDForConv(conv)
	if err != nil {
		return "", err
	}
	if agentID == "" {
		return "", errors.New("no agent for that conversation")
	}
	demotion, err := demoteSteward(g, conv, agentID)
	if err != nil {
		return "", err
	}
	switch then {
	case stewardThenRetire:
		return conv, nil
	case stewardThenRoster:
		if err := db.SetStewardStandby(agentID, by); err != nil {
			return "", err
		}
	default:
		if err := db.RemoveFromStewardRoster(agentID); err != nil {
			return "", err
		}
	}
	paused := 0
	if then == stewardThenRoster {
		paused = pauseStandbyJobs(agentID)
	}
	tellStoodDown(conv, successor, demotion, then == stewardThenRoster, paused)
	return "", nil
}

// pauseStandbyJobs stops what a Steward on the roster scheduled from coming from
// an agent no longer in the role. It logs a failure: the demotion stands either way.
func pauseStandbyJobs(agentID string) int {
	paused, err := db.PauseStewardStandbyJobs(agentID)
	if err != nil {
		slog.Warn("steward: pause a standby's scheduled jobs failed", "agent", agentID, "error", err)
	}
	if paused > 0 {
		reconcileOwnedStandingOrderHooks(agentID)
	}
	return paused
}

// resumeStandbyJobs runs what going on the roster paused, except what aims at a
// group that emptied meanwhile. It says how many, for the agent's message.
func resumeStandbyJobs(agentID string) int {
	groups, err := db.StewardStandbyTargetGroups(agentID)
	if err != nil {
		slog.Warn("steward: read a standby's scheduled jobs failed", "agent", agentID, "error", err)
		return 0
	}
	var empty []int64
	for _, group := range groups {
		// A failed check counts as empty: the job waits for its group.
		if live, err := groupHasLiveMembers(group); err != nil || !live {
			empty = append(empty, group)
		}
	}
	resumed, err := db.ResumeStewardStandbyJobs(agentID, empty)
	if err != nil {
		slog.Warn("steward: resume scheduled jobs failed", "agent", agentID, "error", err)
	}
	if resumed > 0 {
		reconcileOwnedStandingOrderHooks(agentID)
	}
	return resumed
}

// reconcileOwnedStandingOrderHooks redeclares the native hooks after the agent's
// orders changed state in bulk: they are declared from enabled orders only, so
// an order that comes back to a pruned hook would otherwise never fire.
func reconcileOwnedStandingOrderHooks(agentID string) {
	orders, err := db.ListStandingOrders()
	if err != nil {
		slog.Warn("steward: read standing orders for hooks failed", "agent", agentID, "error", err)
		return
	}
	harnesses := map[string]bool{}
	for _, order := range orders {
		if order.OwnerAgent != agentID {
			continue
		}
		for _, selector := range order.HookSelectors {
			harnesses[selector.Harness] = true
		}
	}
	if warning := reconcileStandingOrderHookHarnesses(harnesses); warning != "" {
		slog.Warn("steward: standing-order hook reconciliation failed", "agent", agentID, "warning", warning)
	}
}

// tellStoodDown tells a former Steward what happened. One that is not running
// reads it when it resumes, on a conversation that still says it is the Steward.
func tellStoodDown(conv, successor string, demotion *stewardDemotion, onRoster bool, paused int) {
	body := "The operator stood you down as Steward"
	if successor != "" {
		body = "The operator made " + successor + " the Steward in your place"
	}
	body += ". Stop acting as the Steward; the Steward's access has been removed: " + strings.Join(demotion.Loses, ", ") + "."
	if len(demotion.Keeps) > 0 {
		body += " You keep what the operator gave you separately: " + strings.Join(demotion.Keeps, ", ") + "."
	}
	if onRoster {
		body += " You are on the roster, so they can bring you back."
	}
	if paused > 0 {
		body += fmt.Sprintf(" Your %d scheduled job(s) are paused until you take the seat again.", paused)
	}
	if _, err := queueAgentMessage(&db.AgentMessage{ToConv: conv, Subject: "You are no longer the Steward", Body: body}); err != nil {
		slog.Warn("steward: stand-down message failed", "conv", short8(conv), "error", err)
	}
}

// routes

// asDashboardHuman is the cookie-auth twin of a /v1 steward route.
func asDashboardHuman(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		next(w, asDashboardHumanPeer(r))
	}
}

func stewardActor(r *http.Request) string {
	if p := peerFromContext(r.Context()); p != nil && p.DashboardHuman {
		return dashboardGranter
	}
	return granterLabel("")
}

func stewardRequestConfig(w http.ResponseWriter) (*config.Config, bool) {
	cfg, err := config.LoadShared()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config", err.Error())
		return nil, false
	}
	return cfg, true
}

// GET /v1/steward: the seat and the roster.
func handleStewardRoster(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read the Steward roster") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"roster": stewardRosterView()})
}

// GET /v1/steward/handover?agent=: what making this agent the Steward changes.
func handleStewardHandoverPreview(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "make an agent the Steward") {
		return
	}
	res, ok := resolveStewardTarget(w, r.URL.Query().Get("agent"))
	if !ok {
		return
	}
	cfg, ok := stewardRequestConfig(w)
	if !ok {
		return
	}
	preview, err := stewardPreview(cfg, res)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

// POST /v1/steward/handover: move the seat.
func handleStewardHandover(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "make an agent the Steward") {
		return
	}
	var body struct {
		Agent string `json:"agent"`
		// Then is what becomes of the Steward leaving the seat.
		Then string `json:"then"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	then := body.Then
	if then == "" {
		then = stewardThenRoster
	}
	if then != stewardThenRoster && then != stewardThenKeep && then != stewardThenRetire {
		writeError(w, http.StatusBadRequest, "then", fmt.Sprintf("then must be %s, %s or %s", stewardThenRoster, stewardThenKeep, stewardThenRetire))
		return
	}
	res, ok := resolveStewardTarget(w, body.Agent)
	if !ok {
		return
	}
	cfg, ok := stewardRequestConfig(w)
	if !ok {
		return
	}
	out, fail := handOverSteward(cfg, res, then, stewardActor(r), auditRequestEventID(r))
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /v1/steward/step-down: a Steward leaves the role with nobody seated in
// its place. The agent defaults to the active Steward.
func handleStewardStepDown(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "stand a Steward down") {
		return
	}
	var body struct {
		Agent string `json:"agent"`
		Then  string `json:"then"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	if body.Then != "" && body.Then != stewardThenRoster && body.Then != stewardThenKeep {
		writeError(w, http.StatusBadRequest, "then", fmt.Sprintf("then must be %s or %s; retire a Steward to end it for good", stewardThenRoster, stewardThenKeep))
		return
	}
	scribeSummonMu.Lock()
	defer scribeSummonMu.Unlock()
	conv := activeStewardConv()
	if body.Agent != "" {
		res, ok := resolveStewardTarget(w, body.Agent)
		if !ok {
			return
		}
		conv = res.ConvID
	}
	if conv == "" || !slices.Contains(stewardConvs(), conv) {
		writeError(w, http.StatusConflict, "conflict", "that agent is not a Steward")
		return
	}
	g, fail := ensureScribeGroup(stewardGroupName)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	then := body.Then
	if then == "" {
		then = stewardThenRoster
	}
	if _, err := leaveStewardSeat(g, conv, "", then, stewardActor(r)); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": agent.FreshTitle(conv), "then": then})
}

// POST /v1/steward/roster/remove: take a standby off the roster. It stays a
// normal agent.
func handleStewardRosterRemove(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "change the Steward roster") {
		return
	}
	var body struct {
		Agent string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	res, ok := resolveStewardTarget(w, body.Agent)
	if !ok {
		return
	}
	if slices.Contains(stewardConvs(), res.ConvID) {
		writeError(w, http.StatusConflict, "conflict", "that agent is a Steward; stand it down first")
		return
	}
	if err := db.RemoveFromStewardRoster(res.AgentID); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent_id": res.AgentID})
}
