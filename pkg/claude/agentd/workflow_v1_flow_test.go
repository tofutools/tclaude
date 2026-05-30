package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// v1WfTemplate lays down a minimal single-node ai-template under a temp project
// dir so /v1 create can resolve "project:<name>". Returns the project root.
func v1WfTemplate(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nodes"), 0o755))
	write := func(rel, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644))
	}
	write("workflow.yaml", "name: "+name+"\nentry: work\n")
	write("flow.mmd", "flowchart TD\n work --> done\n")
	write("nodes/work.yaml", "label: Work\nexecutor:\n  kind: ai\n  agent: worker\n  prompt: do the thing\n")
	write("nodes/done.yaml", "label: Done\nexecutor:\n  kind: human\n")
	return root
}

// v1Create instantiates via the /v1 socket POST as the human peer; returns the id.
func v1Create(t *testing.T, f *testharness.Flow, ref, group string) int64 {
	t.Helper()
	body := map[string]any{"template_ref": ref}
	if group != "" {
		body["group"] = group
	}
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/workflows", body))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "POST /v1/workflows body=%s", rec.Body.String())
	var resp struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotZero(t, resp.ID)
	return resp.ID
}

// Scenario: the /v1 read surface — list, detail, events — all reachable by any
// authed caller, returning the same shapes the dashboard emits.
func TestWorkflowV1_ReadSurface(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(v1WfTemplate(t, "v1read")))

	id := v1Create(t, f, "project:v1read", "")

	// list
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/workflows", nil))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "list body=%s", rec.Body.String())
	var list struct {
		Instances []struct {
			ID     int64  `json:"id"`
			Total  int    `json:"total"`
			Status string `json:"status"`
		} `json:"instances"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	var found bool
	for _, inst := range list.Instances {
		if inst.ID == id {
			found = true
			assert.Equal(t, 2, inst.Total, "two nodes")
			assert.Equal(t, "running", inst.Status)
		}
	}
	assert.True(t, found, "instance should appear in /v1/workflows list")

	// detail
	r = agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/workflows/"+strconv.FormatInt(id, 10), nil))
	rec = testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "detail body=%s", rec.Body.String())
	var detail struct {
		Instance struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"instance"`
		Mermaid string `json:"mermaid"`
		Nodes   []any  `json:"nodes"`
		Events  []any  `json:"events"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
	assert.Equal(t, id, detail.Instance.ID)
	assert.NotEmpty(t, detail.Mermaid)
	assert.Len(t, detail.Nodes, 2)

	// events (whole instance)
	r = agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/workflows/"+strconv.FormatInt(id, 10)+"/events", nil))
	rec = testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "events body=%s", rec.Body.String())
	var ev struct {
		Events []struct {
			Kind string `json:"kind"`
		} `json:"events"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ev))
	assert.NotEmpty(t, ev.Events, "instance_created + node_ready events present")
}

// Scenario: missing instance → 404; bad id → 400.
func TestWorkflowV1_NotFoundAndBadID(t *testing.T) {
	f := newFlow(t)
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/v1/workflows/9999", http.StatusNotFound},
		{"/v1/workflows/notanint", http.StatusBadRequest},
	} {
		r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, tc.path, nil))
		rec := testharness.Serve(f.Mux, r)
		assert.Equal(t, tc.code, rec.Code, "GET %s", tc.path)
	}
}

// Scenario: cancel + delete over /v1 as the human; delete then 404s.
func TestWorkflowV1_CancelAndDelete(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(v1WfTemplate(t, "v1cd")))
	id := v1Create(t, f, "project:v1cd", "")

	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/workflows/"+strconv.FormatInt(id, 10)+"/cancel", nil))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "cancel body=%s", rec.Body.String())
	inst, _ := db.GetWorkflowInstance(id)
	require.NotNil(t, inst)
	assert.Equal(t, "cancelled", inst.Status)

	del := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodDelete, "/v1/workflows/"+strconv.FormatInt(id, 10), nil))
	delRec := testharness.Serve(f.Mux, del)
	require.Equal(t, http.StatusNoContent, delRec.Code, "delete body=%s", delRec.Body.String())
	gone, _ := db.GetWorkflowInstance(id)
	assert.Nil(t, gone, "instance deleted")
}

// Scenario: node-PATCH authz — the node's assignee agent may settle its node;
// an unrelated agent is 403; the human always may.
func TestWorkflowV1_NodePatchAuthz(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(v1WfTemplate(t, "v1authz")))

	const assignee = "asgn-aaaa-bbbb-cccc-1111"
	const other = "othr-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(assignee, "worker")
	f.HaveConvWithTitle(other, "bystander")

	id := v1Create(t, f, "project:v1authz", "")
	// Assign the entry node "work" to `assignee` (the engine/dashboard would
	// normally do this on start; set it directly for the authz test).
	asg := assignee
	_, err := db.UpdateWorkflowNode(id, "work", db.WorkflowNodePatch{Assignee: &asg})
	require.NoError(t, err)

	nodePath := "/v1/workflows/" + strconv.FormatInt(id, 10) + "/nodes/work"

	// Unrelated agent → 403.
	r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPatch, nodePath,
		map[string]any{"status": "done"}), other)
	rec := testharness.Serve(f.Mux, r)
	assert.Equal(t, http.StatusForbidden, rec.Code, "unrelated agent should 403; body=%s", rec.Body.String())

	// The assignee agent → allowed, settles the node done.
	r = agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPatch, nodePath,
		map[string]any{"status": "done"}), assignee)
	rec = testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "assignee should settle its node; body=%s", rec.Body.String())
	got, _ := db.GetWorkflowNode(id, "work")
	assert.Equal(t, "done", got.Status, "node settled done by its assignee")
}

// Scenario: the ai-verify authz contract (JOH-35). After a node is parked in
// awaiting_verify and the engine reassigns it to a JUDGE, only the judge — the
// node's current responsible actor — may settle the verdict; the original worker
// (no longer the assignee) is refused, so it cannot self-approve. The judge's
// `done` settles from awaiting_verify (the park interception only fires from
// `running`, so there is no re-park).
func TestWorkflowV1_AIVerifyJudgeAuthz(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(v1WfTemplate(t, "v1verify")))

	const worker = "wrkr-aaaa-bbbb-cccc-7777"
	const judge = "judg-aaaa-bbbb-cccc-8888"
	f.HaveConvWithTitle(worker, "worker")
	f.HaveConvWithTitle(judge, "judge")

	id := v1Create(t, f, "project:v1verify", "")
	// Model the post-park, judge-assigned state: the worker did the work, the node
	// parked in awaiting_verify (assignee cleared), and the engine reassigned it to
	// the judge.
	awaiting := db.WorkflowNodeStatusAwaitingVerify
	asgJudge := judge
	_, err := db.UpdateWorkflowNode(id, "work", db.WorkflowNodePatch{Status: &awaiting, Assignee: &asgJudge})
	require.NoError(t, err)

	nodePath := "/v1/workflows/" + strconv.FormatInt(id, 10) + "/nodes/work"

	// The original worker — no longer the assignee — cannot settle the verdict.
	r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPatch, nodePath,
		map[string]any{"status": "done"}), worker)
	assert.Equal(t, http.StatusForbidden, testharness.Serve(f.Mux, r).Code,
		"the worker can't self-approve once the judge owns the node")

	// The judge (current assignee) settles its verdict from awaiting_verify.
	r = agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPatch, nodePath,
		map[string]any{"status": "done"}), judge)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "judge settles its verdict; body=%s", rec.Body.String())
	got, _ := db.GetWorkflowNode(id, "work")
	assert.Equal(t, "done", got.Status, "judge's done verdict settles the node (no re-park from awaiting_verify)")
}

// Scenario: an agent that is the node's assignee marks it running then done —
// the start/done flow the CLI `workflow node` verb drives.
func TestWorkflowV1_AssigneeRunningThenDone(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(v1WfTemplate(t, "v1run")))
	const assignee = "runr-aaaa-bbbb-cccc-3333"
	f.HaveConvWithTitle(assignee, "worker")

	id := v1Create(t, f, "project:v1run", "")
	asg := assignee
	_, err := db.UpdateWorkflowNode(id, "work", db.WorkflowNodePatch{Assignee: &asg})
	require.NoError(t, err)

	nodePath := "/v1/workflows/" + strconv.FormatInt(id, 10) + "/nodes/work"
	// running
	r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPatch, nodePath,
		map[string]any{"status": "running"}), assignee)
	require.Equal(t, http.StatusOK, testharness.Serve(f.Mux, r).Code)
	// done
	r = agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPatch, nodePath,
		map[string]any{"status": "done"}), assignee)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "done body=%s", rec.Body.String())
	got, _ := db.GetWorkflowNode(id, "work")
	assert.Equal(t, "done", got.Status)
}

// Scenario: /v1/workflows/where is first-person — an agent sees ONLY its own
// assigned node; a human caller gets an empty assignments list.
func TestWorkflowV1_Where(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(v1WfTemplate(t, "v1where")))
	const mine = "mine-aaaa-bbbb-cccc-4444"
	const yours = "your-aaaa-bbbb-cccc-5555"
	f.HaveConvWithTitle(mine, "me")
	f.HaveConvWithTitle(yours, "you")

	id := v1Create(t, f, "project:v1where", "")
	asgMine := mine
	_, err := db.UpdateWorkflowNode(id, "work", db.WorkflowNodePatch{Assignee: &asgMine})
	require.NoError(t, err)

	// Agent `mine` sees its assignment.
	r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/workflows/where", nil), mine)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "where body=%s", rec.Body.String())
	var resp struct {
		Caller      string `json:"caller"`
		Assignments []struct {
			Node struct {
				NodeID string `json:"node_id"`
			} `json:"node"`
			Instance struct {
				ID int64 `json:"id"`
			} `json:"instance"`
		} `json:"assignments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, mine, resp.Caller)
	require.Len(t, resp.Assignments, 1, "mine should see exactly its one assignment")
	assert.Equal(t, "work", resp.Assignments[0].Node.NodeID)
	assert.Equal(t, id, resp.Assignments[0].Instance.ID)

	// Agent `yours` sees nothing (not assigned).
	r = agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/workflows/where", nil), yours)
	rec = testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code)
	var yoursResp struct {
		Assignments []any `json:"assignments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &yoursResp))
	assert.Empty(t, yoursResp.Assignments, "an unassigned agent sees no assignments")

	// Human caller → empty (humans aren't node assignees).
	r = agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/workflows/where", nil))
	rec = testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code)
	var humanResp struct {
		Caller      string `json:"caller"`
		Assignments []any  `json:"assignments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &humanResp))
	assert.Equal(t, "", humanResp.Caller, "human caller has no conv-id")
	assert.Empty(t, humanResp.Assignments)
}

// Scenario: an unidentified caller (no peer identity) is refused on the read
// surface — authedCaller fails closed.
func TestWorkflowV1_UnidentifiedRefused(t *testing.T) {
	f := newFlow(t)
	r := agentd.AsUnconfirmedPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/workflows", nil))
	rec := testharness.Serve(f.Mux, r)
	assert.NotEqual(t, http.StatusOK, rec.Code, "unidentified caller should be refused")
}
