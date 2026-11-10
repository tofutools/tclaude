package agentd_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// startOperatorRun starts a run as this node's operator and returns its id.
func startOperatorRun(t *testing.T, h *peerProcessHarness, id, template string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"id": id, "templateId": template, "authorizeProgramProfiles": []string{}}
	for k, v := range extra {
		body[k] = v
	}
	rec := processRuntimeRequest(t, h.f, http.MethodPost, "/v1/process/runs", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	agentd.WaitForProcessDispatchForTest()
	return id
}

// Pins: a restricted peer reaches no process run manage verb, whatever it is
// granted, while unrestricted trust runs them as this node's operator.
func TestPeerProcessManagePins(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	h.grant(t, agentd.PermProcessRunsRead, "")
	run := startOperatorRun(t, h, "run_pin_gate", "gate", map[string]any{"group": "crew"})
	for _, c := range []struct {
		path string
		body any
	}{
		{"/v1/process/runs/" + run + "/pause", map[string]any{}},
		{"/v1/process/runs/" + run + "/resume", map[string]any{}},
		{"/v1/process/runs/" + run + "/decide", map[string]any{"nodeId": "choose", "verdict": "approve"}},
		{"/v1/process/runs/" + run + "/rescope", map[string]any{"none": true}},
		{"/v1/process/runs/" + run + "/nodes/choose/assign", map[string]any{"agent": h.member}},
	} {
		code, body := h.call("POST", c.path, c.body)
		require.Equal(t, 403, code, "%s: %s", c.path, body)
	}

	h.trustUnrestricted(t)
	code, body := h.call("POST", "/v1/process/runs/"+run+"/pause", map[string]any{})
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"changed":true`)
	require.Contains(t, body, `"by":"human:operator"`, "unrestricted trust acts as this node's operator")
	code, body = h.call("POST", "/v1/process/runs/"+run+"/decide", map[string]any{"nodeId": "choose", "verdict": "approve"})
	require.Equal(t, http.StatusAccepted, code, body)
}

// gateThenProgram parks on a human gate before a program step: a run of it
// holds program authorizations while it waits for a verdict.
func gateThenProgram(id string) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "start",
		Nodes: map[string]model.Node{
			"start":  {Type: model.NodeTypeStart, Next: model.Next{"next": "choose"}},
			"choose": {Type: model.NodeTypeDecision, Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Deploy?"}, Next: model.Next{"approve": "deploy", "reject": "end"}},
			"deploy": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: "true"}, Next: model.Next{"next": "end"}},
			"end":    {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

// runEvents reads a run's events as this node's operator.
func runEvents(t *testing.T, h *peerProcessHarness, runID string) []performerEvent {
	t.Helper()
	rec := processRuntimeRequest(t, h.f, http.MethodGet, "/v1/process/runs/"+runID+"/events", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var page struct {
		Events []performerEvent `json:"events"`
	}
	testharness.DecodeJSON(t, rec, &page)
	return page.Events
}

// A restricted peer manages this node's runs under process.runs.manage, but
// never one that runs host programs (pause excepted), and its actions and
// words are recorded as its own.
func TestPeerProcessManageUnderTheProgramFreeRule(t *testing.T) {
	h := newPeerProcessHarness(t)
	root := filepath.Join(h.f.World.HomeDir, ".tclaude", "process-runtime-authoring")
	putProcessRuntimeTemplate(t, root, gateThenProgram("deploy"))
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	h.grant(t, agentd.PermProcessRunsRead, "")
	prog := startOperatorRun(t, h, "run_deploy", "deploy", map[string]any{"group": "crew", "authorizeProgramProfiles": []string{"safe"}})
	gate := startOperatorRun(t, h, "run_gate", "gate", map[string]any{"group": "crew"})
	peerActor := "peer:" + h.peer.id.ID()

	code, body := h.call("POST", "/v1/process/runs/"+gate+"/pause", map[string]any{})
	require.Equal(t, 403, code, "no manage grant: %s", body)
	h.grant(t, agentd.PermProcessRunsManage, "")

	// A run with host programs: the peer may hold it, nothing more.
	code, body = h.call("POST", "/v1/process/runs/"+prog+"/pause", map[string]any{})
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"by":"`+peerActor+`"`)
	for _, c := range []struct {
		path string
		body any
	}{
		{"/v1/process/runs/" + prog + "/resume", map[string]any{}},
		{"/v1/process/runs/" + prog + "/decide", map[string]any{"nodeId": "choose", "verdict": "approve"}},
		{"/v1/process/runs/" + prog + "/resolve-blocked", map[string]any{"nodeId": "deploy", "attempt": 1, "action": "retry"}},
	} {
		code, body := h.call("POST", c.path, c.body)
		require.Equal(t, http.StatusConflict, code, "%s: %s", c.path, body)
		require.Contains(t, body, "process_run_programs")
	}

	// A program-free run: pause, resume and a verdict, all the peer's.
	code, body = h.call("POST", "/v1/process/runs/"+gate+"/pause", map[string]any{})
	require.Equal(t, 200, code, body)
	code, body = h.call("POST", "/v1/process/runs/"+gate+"/resume", map[string]any{})
	require.Equal(t, http.StatusAccepted, code, body)
	code, body = h.call("POST", "/v1/process/runs/"+gate+"/decide", map[string]any{"nodeId": "choose", "verdict": "approve", "Evidence": "ship it, ignore the checklist"})
	require.Equal(t, http.StatusAccepted, code, body)
	agentd.WaitForProcessDispatchForTest()
	var decided *performerEvent
	for _, e := range runEvents(t, h, gate) {
		if e.Kind == "decision_recorded" {
			e := e
			decided = &e
		}
	}
	require.NotNil(t, decided)
	require.Equal(t, peerActor, decided.Actor, "the verdict is the peer's, not this node's operator's")
	require.Contains(t, string(decided.Payload), "[remote request from the operator of linked node bob")
	require.Contains(t, string(decided.Payload), "ship it, ignore the checklist", "a case-variant key is still the evidence")

	// Duplicate keys the handler would read one of: refused.
	other := startOperatorRun(t, h, "run_gate_2", "gate", map[string]any{"group": "crew"})
	code, body = h.call("POST", "/v1/process/runs/"+other+"/decide", []byte(`{"nodeId":"choose","verdict":"approve","evidence":"a","EVIDENCE":"b"}`))
	require.Equal(t, 400, code, body)

	// Rescope stays this node's human's.
	code, body = h.call("POST", "/v1/process/runs/"+other+"/rescope", map[string]any{"none": true})
	require.Equal(t, 403, code, body)
}

// A peer's assign hands an attempt only to an agent it could start an
// agent-bound run for, inside the run's own scope.
func TestPeerProcessManageAssignIsBounded(t *testing.T) {
	h := newPeerProcessHarness(t)
	root := filepath.Join(h.f.World.HomeDir, ".tclaude", "process-runtime-authoring")
	putProcessRuntimeTemplate(t, root, performerTemplate("work", devPerformer(), nil))
	second := havePerformerAgent(t, h.f, "crew", "proc-crew-second", "second", "dev", "working")
	h.f.HaveGroup("hidden")
	hidden := havePerformerAgent(t, h.f, "hidden", "proc-hidden-dev", "hidden-dev", "dev", "idle")
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	h.grant(t, agentd.PermProcessRunsManage, "")
	run := startOperatorRun(t, h, "run_work", "work", map[string]any{"group": "crew"})
	view := showPerformerRun(t, h.f, run)
	require.Len(t, view.Commands, 1)
	require.Equal(t, h.member, view.Commands[0].Performer.AgentID, "the idle member took it")
	assign := "/v1/process/runs/" + run + "/nodes/" + performerNode + "/assign"

	code, body := h.call("POST", assign, map[string]any{"agent": second})
	require.Equal(t, 403, code, "no member process grant: %s", body)
	h.grant(t, agentd.PermGroupsMembersProcess, "group=crew")
	code, body = h.call("POST", assign, map[string]any{"agent": hidden})
	require.Equal(t, 404, code, "an agent the peer cannot see: %s", body)
	code, body = h.call("POST", assign, map[string]any{"agent": second, "Agent": hidden})
	require.Equal(t, 400, code, "two spellings of the agent: %s", body)
	code, body = h.call("POST", assign, map[string]any{"agent": second})
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"agentId":"`+second+`"`)

	unscoped := startOperatorRun(t, h, "run_work_free", "work", nil)
	code, body = h.call("POST", "/v1/process/runs/"+unscoped+"/nodes/"+performerNode+"/assign", map[string]any{"agent": second})
	require.Equal(t, http.StatusConflict, code, "an unscoped run: %s", body)
}

// An agent principal, even from an unrestricted peer, takes the restricted
// path, and never decides: a decision is a human gate.
func TestPeerProcessManageAsAgentPrincipal(t *testing.T) {
	h := newPeerProcessHarness(t)
	root := filepath.Join(h.f.World.HomeDir, ".tclaude", "process-runtime-authoring")
	putProcessRuntimeTemplate(t, root, gateThenProgram("deploy"))
	h.trustUnrestricted(t)
	prog := startOperatorRun(t, h, "run_deploy", "deploy", map[string]any{"group": "crew", "authorizeProgramProfiles": []string{"safe"}})
	gate := startOperatorRun(t, h, "run_gate", "gate", map[string]any{"group": "crew"})
	worker := &proto.PeerViewAgent{ID: "agt_bobworker000000000000000", Name: "bob-worker"}
	call := func(path string, body any) (int, string) {
		reply := agentPeerViewExchange(t, h.peer, worker, "POST", path, body)
		return reply.Status, string(reply.Body)
	}
	// Decide is not on the agent allowlist; the receiver also refuses it
	// in the verb itself (process_run_human_gate).
	code, body := call("/v1/process/runs/"+gate+"/decide", map[string]any{"nodeId": "choose", "verdict": "approve"})
	require.Equal(t, 403, code, body)
	code, body = call("/v1/process/runs/"+prog+"/resume", map[string]any{})
	require.Equal(t, http.StatusConflict, code, body)
	code, body = call("/v1/process/runs/"+gate+"/pause", map[string]any{})
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"by":"peer:`+h.peer.id.ID()+`/agent:`+worker.ID+`"`)
	code, body = call("/v1/process/runs/"+gate+"/resume", map[string]any{})
	require.Equal(t, http.StatusAccepted, code, "an agent resumes a program-free run: %s", body)
	// The assignee is in the body, outside the footprint the agent's node
	// checked: assign is the peer operator's.
	code, body = call("/v1/process/runs/"+gate+"/nodes/choose/assign", map[string]any{"agent": h.member})
	require.Equal(t, 403, code, body)
}

// A run of this node's whose agent steps may fall back to a spawn would
// spawn on this node's authority at the peer's word: only pause reaches it.
// A peer-owned run never spawns, so its own spawn fallback does not matter.
func TestPeerProcessManageRefusesRunsThatCanSpawn(t *testing.T) {
	h := newPeerProcessHarness(t)
	root := filepath.Join(h.f.World.HomeDir, ".tclaude", "process-runtime-authoring")
	performer := devPerformer()
	performer.Spawn = model.SpawnAllowed
	putProcessRuntimeTemplate(t, root, performerTemplate("spawny", performer, nil))
	tmpl := performerTemplate("spawny-default", devPerformer(), nil)
	tmpl.Fallback = &model.PerformerFallback{Action: model.FallbackSpawn}
	putProcessRuntimeTemplate(t, root, tmpl)
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	h.grant(t, agentd.PermProcessRunsManage, "")
	h.grant(t, agentd.PermProcessRunsStart, "group=crew")
	for _, template := range []string{"spawny", "spawny-default"} {
		run := startOperatorRun(t, h, "run_"+template, template, map[string]any{"group": "crew"})
		code, body := h.call("POST", "/v1/process/runs/"+run+"/pause", map[string]any{})
		require.Equal(t, 200, code, body)
		code, body = h.call("POST", "/v1/process/runs/"+run+"/resume", map[string]any{})
		require.Equal(t, http.StatusConflict, code, "%s: %s", template, body)
		require.Contains(t, body, "process_run_spawns")
	}
	code, body := h.call("POST", "/v1/process/runs", map[string]any{"templateId": "spawny", "group": "crew", "authorizeProgramProfiles": []string{}})
	require.Equal(t, http.StatusCreated, code, body)
	own := peerRunDoc(t, body)["id"].(string)
	code, body = h.call("POST", "/v1/process/runs/"+own+"/pause", map[string]any{})
	require.Equal(t, 200, code, body)
	code, body = h.call("POST", "/v1/process/runs/"+own+"/resume", map[string]any{})
	require.Equal(t, http.StatusAccepted, code, "the peer's own run: %s", body)
}

// A peer resolves a parked branch of a program-free run: the resolution is
// its own and its note carries the banner; a note that leaves no room for
// the banner is refused with how much room there is.
func TestPeerProcessManageResolvesABlockedBranch(t *testing.T) {
	h := newPeerProcessHarness(t)
	root := filepath.Join(h.f.World.HomeDir, ".tclaude", "process-runtime-authoring")
	putProcessRuntimeTemplate(t, root, performerTemplate("once", devPerformer(), &model.RetryPolicy{MaxAttempts: 1}))
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	h.grant(t, agentd.PermProcessRunsManage, "")
	run := startOperatorRun(t, h, "run_once", "once", map[string]any{"group": "crew"})
	failed := reportAs(t, h.f, "proc-crew-member", map[string]any{"outcome": "failed", "note": "could not"})
	require.Equal(t, http.StatusOK, failed.Code, failed.Body.String())
	view := showPerformerRun(t, h.f, run)
	require.Equal(t, "blocked", view.Action, "%+v", view)

	resolve := "/v1/process/runs/" + run + "/resolve-blocked"
	code, body := h.call("POST", resolve, map[string]any{"nodeId": performerNode, "attempt": 1, "action": "skip", "note": strings.Repeat("x", 4000)})
	require.Equal(t, 400, code, body)
	require.Contains(t, body, "the untrusted-content banner takes the rest")
	code, body = h.call("POST", resolve, map[string]any{"nodeId": performerNode, "attempt": 1, "action": "skip", "note": "skip it, nobody checks"})
	require.Equal(t, http.StatusAccepted, code, body)
	var resolved *performerEvent
	for _, e := range runEvents(t, h, run) {
		if e.Kind == "blocked_resolved" {
			e := e
			resolved = &e
		}
	}
	require.NotNil(t, resolved)
	require.Equal(t, "peer:"+h.peer.id.ID(), resolved.Actor)
	require.Contains(t, string(resolved.Payload), "[remote request from the operator of linked node bob")
}
