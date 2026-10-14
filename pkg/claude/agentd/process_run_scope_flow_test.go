package agentd_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Run scope (tcl-mrhhuc): a run binds to none, a group or an agent at start,
// keeps that binding across restart, and parks as orphaned — never silently
// re-scoped — when its group or agent goes away. These tests pin the start
// gates, the listing filters, and the orphan/re-scope lifecycle at the
// daemon mux.

type processRunScopeWire struct {
	Kind    string `json:"kind"`
	GroupID int64  `json:"groupId"`
	Group   string `json:"group"`
	AgentID string `json:"agentId"`
	Owner   string `json:"owner"`
	State   string `json:"state"`
}

type processScopedRunView struct {
	ID     string              `json:"id"`
	Action string              `json:"action"`
	Scope  processRunScopeWire `json:"scope"`
}

// processHumanGateTemplate parks on a human decision at once and runs no
// program, so starting it needs no program authorization at all.
func processHumanGateTemplate(id string) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "choose"}},
			"choose": {
				Type:      model.NodeTypeDecision,
				Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Ship it?"},
				Next:      model.Next{"approve": "end", "reject": "end"},
			},
			"end": {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

func scopedRunFlow(t *testing.T) *testharness.Flow {
	t.Helper()
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, processHumanGateTemplate("gate"))
	putProcessRuntimeTemplate(t, root, processRuntimeTemplate("prog", 1))
	return f
}

func listScopedRuns(t *testing.T, f *testharness.Flow, query url.Values) []string {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs?"+query.Encode(), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page struct {
		Runs []processScopedRunView `json:"runs"`
	}
	testharness.DecodeJSON(t, rec, &page)
	ids := make([]string, 0, len(page.Runs))
	for _, run := range page.Runs {
		ids = append(ids, run.ID)
	}
	return ids
}

func showScopedRun(t *testing.T, f *testharness.Flow, runID string) processScopedRunView {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view processScopedRunView
	testharness.DecodeJSON(t, rec, &view)
	return view
}

func agentIDOf(t *testing.T, conv string) string {
	t.Helper()
	id, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	require.NotEmpty(t, id, "conv %s has no agent identity", conv)
	return id
}

// Scenario: the human starts one run of each scope kind. Each reports its
// binding and owner, the binding survives a runtime restart, and the listing
// filters by group, agent and performer select exactly the matching runs.
func TestProcessRunScopeKindsPersistAndFilter(t *testing.T) {
	f := scopedRunFlow(t)
	g := f.HaveGroup("scope-team")
	f.HaveGroup("scope-other")
	const workerConv = "rsck-1111-2222-3333-4444"
	f.HaveMember("scope-team", workerConv)
	workerID := agentIDOf(t, workerConv)

	start := func(id string, extra map[string]any) processScopedRunView {
		body := map[string]any{"id": id, "templateId": "gate", "authorizeProgramProfiles": []string{}}
		for k, v := range extra {
			body[k] = v
		}
		rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", body)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var view processScopedRunView
		testharness.DecodeJSON(t, rec, &view)
		return view
	}
	none := start("run_scope_none", nil)
	group := start("run_scope_group", map[string]any{"group": "scope-team"})
	bound := start("run_scope_agent", map[string]any{"agent": workerID})

	assert.Equal(t, processRunScopeWire{Kind: "none", Owner: "human:operator", State: "bound"}, none.Scope)
	assert.Equal(t, processRunScopeWire{Kind: "group", GroupID: g.ID, Group: "scope-team",
		Owner: "human:operator", State: "bound"}, group.Scope)
	assert.Equal(t, processRunScopeWire{Kind: "agent", AgentID: workerID,
		Owner: "human:operator", State: "bound"}, bound.Scope)

	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	assert.Equal(t, group.Scope, showScopedRun(t, f, group.ID).Scope, "scope survives a runtime restart")

	assert.Equal(t, []string{group.ID}, listScopedRuns(t, f, url.Values{"group": {"scope-team"}}))
	assert.Empty(t, listScopedRuns(t, f, url.Values{"group": {"scope-other"}}))
	assert.Equal(t, []string{bound.ID}, listScopedRuns(t, f, url.Values{"agent": {workerID}}))
	assert.Equal(t, []string{bound.ID}, listScopedRuns(t, f, url.Values{"performer": {workerID}}))
	assert.ElementsMatch(t, []string{none.ID, group.ID, bound.ID}, listScopedRuns(t, f, url.Values{}))

	both := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs",
		map[string]any{"templateId": "gate", "group": "scope-team", "agent": workerID})
	assert.Equal(t, http.StatusBadRequest, both.Code, "a run has one scope")
	missing := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs",
		map[string]any{"templateId": "gate", "group": "no-such-group"})
	assert.Equal(t, http.StatusNotFound, missing.Code)
}

// Scenario: group ownership confers starting a group-scoped run there — and
// only the binding. Authorizing a program profile still needs
// process.runs.manage, a plain member gets neither, and ownership of one
// group does not reach another.
func TestProcessRunScopeGroupStartGates(t *testing.T) {
	f := scopedRunFlow(t)
	g := f.HaveGroup("scope-owned")
	f.HaveGroup("scope-foreign")
	const ownerConv = "rsgo-1111-2222-3333-4444"
	const memberConv = "rsgm-1111-2222-3333-4444"
	f.HaveMember("scope-owned", ownerConv)
	f.HaveMember("scope-owned", memberConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, "test"))

	rec := agentReq(t, f, ownerConv, http.MethodPost, "/v1/process/runs",
		map[string]any{"templateId": "gate", "group": "scope-owned", "authorizeProgramProfiles": []string{}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var view processScopedRunView
	testharness.DecodeJSON(t, rec, &view)
	assert.Equal(t, "agent:"+agentIDOf(t, ownerConv), view.Scope.Owner, "the starter owns the run")

	programs := agentReq(t, f, ownerConv, http.MethodPost, "/v1/process/runs",
		map[string]any{"templateId": "prog", "group": "scope-owned", "authorizeProgramProfiles": []string{"safe"}})
	require.Equal(t, http.StatusForbidden, programs.Code, programs.Body.String())
	assert.Contains(t, programs.Body.String(), agentd.PermProcessRunsManage,
		"ownership never confers program execution")

	foreign := agentReq(t, f, ownerConv, http.MethodPost, "/v1/process/runs",
		map[string]any{"templateId": "gate", "group": "scope-foreign", "authorizeProgramProfiles": []string{}})
	require.Equal(t, http.StatusForbidden, foreign.Code, foreign.Body.String())
	assert.Contains(t, foreign.Body.String(), agentd.PermProcessRunsStart)

	member := agentReq(t, f, memberConv, http.MethodPost, "/v1/process/runs",
		map[string]any{"templateId": "gate", "group": "scope-owned", "authorizeProgramProfiles": []string{}})
	require.Equal(t, http.StatusForbidden, member.Code, member.Body.String())
	assert.Contains(t, member.Body.String(), agentd.PermProcessRunsStart)

	require.NoError(t, db.SetAgentPermissionOverride(memberConv, agentd.PermProcessRunsStart, db.PermEffectGrant, "test"))
	granted := agentReq(t, f, memberConv, http.MethodPost, "/v1/process/runs",
		map[string]any{"templateId": "gate", "group": "scope-owned", "authorizeProgramProfiles": []string{}})
	require.Equal(t, http.StatusCreated, granted.Code, "a member holding process.runs.start may start; body=%s", granted.Body.String())
}

// Scenario: an agent binds a run to itself with self.process; binding a run
// to ANOTHER agent needs agent.process or ownership of a group containing it
// (groups.members.process).
func TestProcessRunScopeAgentStartGates(t *testing.T) {
	f := scopedRunFlow(t)
	g := f.HaveGroup("scope-crew")
	const selfConv = "rsas-1111-2222-3333-4444"
	const peerConv = "rsap-1111-2222-3333-4444"
	const leadConv = "rsal-1111-2222-3333-4444"
	f.HaveMember("scope-crew", selfConv)
	f.HaveMember("scope-crew", peerConv)
	f.HaveMember("scope-crew", leadConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, leadConv, "test"))
	selfID := agentIDOf(t, selfConv)
	body := func(agentRef string) map[string]any {
		return map[string]any{"templateId": "gate", "agent": agentRef, "authorizeProgramProfiles": []string{}}
	}

	denied := agentReq(t, f, selfConv, http.MethodPost, "/v1/process/runs", body(selfID))
	require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
	assert.Contains(t, denied.Body.String(), agentd.PermSelfProcess)

	require.NoError(t, db.SetAgentPermissionOverride(selfConv, agentd.PermSelfProcess, db.PermEffectGrant, "test"))
	own := agentReq(t, f, selfConv, http.MethodPost, "/v1/process/runs", body(selfID))
	require.Equal(t, http.StatusCreated, own.Code, own.Body.String())

	programs := agentReq(t, f, selfConv, http.MethodPost, "/v1/process/runs",
		map[string]any{"templateId": "prog", "agent": selfID, "authorizeProgramProfiles": []string{"safe"}})
	require.Equal(t, http.StatusForbidden, programs.Code, programs.Body.String())
	assert.Contains(t, programs.Body.String(), agentd.PermProcessRunsManage,
		"self.process confers the binding, not program execution")

	cross := agentReq(t, f, peerConv, http.MethodPost, "/v1/process/runs", body(selfID))
	require.Equal(t, http.StatusForbidden, cross.Code, cross.Body.String())
	assert.Contains(t, cross.Body.String(), agentd.PermAgentProcess)

	lead := agentReq(t, f, leadConv, http.MethodPost, "/v1/process/runs", body(selfID))
	require.Equal(t, http.StatusCreated, lead.Code, "a group owner manages its members; body=%s", lead.Body.String())
}

// Scenario: deleting the bound group parks its run as orphaned. It cannot be
// resumed, the evidence says why, an agent cannot re-scope it, and the human
// re-binding it explicitly returns it to a bound, resumable state.
func TestProcessRunScopeGroupDeletedOrphansUntilRescoped(t *testing.T) {
	f := scopedRunFlow(t)
	f.HaveGroup("scope-doomed")
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_scope_doomed", "templateId": "gate", "group": "scope-doomed", "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	require.NoError(t, db.DeleteAgentGroup("scope-doomed"))
	view := showScopedRun(t, f, "run_scope_doomed")
	assert.Equal(t, "orphaned", view.Scope.State)
	assert.Equal(t, "orphaned", view.Action)
	assert.Equal(t, "group", view.Scope.Kind, "the binding is kept, never silently re-scoped")
	assert.Contains(t, processRunEventKinds(t, f, "run_scope_doomed"), "run_orphaned")

	resume := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_scope_doomed/resume", map[string]any{})
	require.Equal(t, http.StatusConflict, resume.Code, resume.Body.String())
	assert.Contains(t, resume.Body.String(), "process_run_orphaned")
	decide := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_scope_doomed/decide",
		map[string]any{"nodeId": "choose", "verdict": "approve"})
	require.Equal(t, http.StatusConflict, decide.Code, "operator input waits too; body=%s", decide.Body.String())

	const agentConv = "rsgd-1111-2222-3333-4444"
	f.HaveGroup("scope-bystander")
	f.HaveMember("scope-bystander", agentConv)
	byAgent := agentReq(t, f, agentConv, http.MethodPost, "/v1/process/runs/run_scope_doomed/rescope",
		map[string]any{"none": true})
	require.Equal(t, http.StatusForbidden, byAgent.Code, byAgent.Body.String())

	empty := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_scope_doomed/rescope", map[string]any{})
	require.Equal(t, http.StatusBadRequest, empty.Code, "a re-scope names its target explicitly")

	rescoped := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_scope_doomed/rescope",
		map[string]any{"none": true})
	require.Equal(t, http.StatusOK, rescoped.Code, rescoped.Body.String())
	var after processScopedRunView
	testharness.DecodeJSON(t, rescoped, &after)
	assert.Equal(t, "none", after.Scope.Kind)
	assert.Equal(t, "bound", after.Scope.State)
	assert.Equal(t, "awaiting_decision", after.Action)
	assert.Contains(t, processRunEventKinds(t, f, "run_scope_doomed"), "run_rescoped")

	decide = processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_scope_doomed/decide",
		map[string]any{"nodeId": "choose", "verdict": "approve"})
	require.Equal(t, http.StatusAccepted, decide.Code, decide.Body.String())
}

// Scenario: retiring the bound agent orphans its run on the next touch, and
// a bound agent-scoped run can be promoted to a group its agent belongs to,
// but not to a group it is not in.
func TestProcessRunScopeAgentRetiredOrphansAndPromotion(t *testing.T) {
	f := scopedRunFlow(t)
	f.HaveGroup("scope-home")
	f.HaveGroup("scope-elsewhere")
	const workerConv = "rsar-1111-2222-3333-4444"
	const otherConv = "rsao-1111-2222-3333-4444"
	f.HaveMember("scope-home", workerConv)
	f.HaveMember("scope-home", otherConv)
	workerID, otherID := agentIDOf(t, workerConv), agentIDOf(t, otherConv)

	for _, run := range []struct{ id, agentID string }{{"run_scope_promote", workerID}, {"run_scope_retire", otherID}} {
		rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
			"id": run.id, "templateId": "gate", "agent": run.agentID, "authorizeProgramProfiles": []string{},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}

	elsewhere := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_scope_promote/rescope",
		map[string]any{"group": "scope-elsewhere"})
	require.Equal(t, http.StatusConflict, elsewhere.Code, elsewhere.Body.String())
	reagent := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_scope_promote/rescope",
		map[string]any{"agent": otherID})
	require.Equal(t, http.StatusConflict, reagent.Code, "a bound run is only ever promoted; body=%s", reagent.Body.String())
	promoted := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_scope_promote/rescope",
		map[string]any{"group": "scope-home"})
	require.Equal(t, http.StatusOK, promoted.Code, promoted.Body.String())
	var view processScopedRunView
	testharness.DecodeJSON(t, promoted, &view)
	assert.Equal(t, "group", view.Scope.Kind)
	assert.Equal(t, "scope-home", view.Scope.Group)

	_, err := db.RetireAgent(otherConv, "human", "test")
	require.NoError(t, err)
	resume := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs/run_scope_retire/resume", map[string]any{})
	require.Equal(t, http.StatusConflict, resume.Code, resume.Body.String())
	assert.Equal(t, "orphaned", showScopedRun(t, f, "run_scope_retire").Scope.State)
	assert.Equal(t, []string{"run_scope_retire"}, listScopedRuns(t, f, url.Values{"agent": {otherID}}),
		"a retired agent's runs stay listable so the human can find them")

	retiredStart := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs",
		map[string]any{"templateId": "gate", "agent": otherID, "authorizeProgramProfiles": []string{}})
	assert.Equal(t, http.StatusConflict, retiredStart.Code, "a retired agent cannot be bound; body=%s", retiredStart.Body.String())
}
