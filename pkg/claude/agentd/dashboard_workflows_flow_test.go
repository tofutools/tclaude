package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// wfReq drives one dashboard workflow request through the cookie-injecting test
// mux. body is JSON-marshalled (nil → empty body).
func wfReq(t *testing.T, mux http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload := ""
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err, "marshal body")
		payload = string(b)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	return testharness.Serve(mux, r)
}

// wfPatchResult is the PATCH /nodes/{id} response shape.
type wfPatchResult struct {
	OK             bool     `json:"ok"`
	NodeID         string   `json:"node_id"`
	Status         string   `json:"status"`
	InstanceStatus string   `json:"instance_status"`
	Ready          []string `json:"ready"`
	Skipped        []string `json:"skipped"`
}

// wfDetail is the subset of GET /api/workflows/{id} the tests assert on.
type wfDetail struct {
	Instance struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	} `json:"instance"`
	Mermaid string `json:"mermaid"`
	Nodes   []struct {
		NodeID  string `json:"node_id"`
		Status  string `json:"status"`
		Outcome string `json:"outcome"`
		Output  string `json:"output"`
	} `json:"nodes"`
	Events []struct {
		Kind string `json:"kind"`
	} `json:"events"`
}

func wfNodeStatuses(d wfDetail) map[string]string {
	m := map[string]string{}
	for _, n := range d.Nodes {
		m[n.NodeID] = n.Status
	}
	return m
}

func wfCreate(t *testing.T, mux http.Handler, ref, title string, params map[string]any) int64 {
	t.Helper()
	rec := wfReq(t, mux, http.MethodPost, "/api/workflows", map[string]any{
		"template_ref": ref, "title": title, "params": params,
	})
	require.Equal(t, http.StatusOK, rec.Code, "POST /api/workflows body=%s", rec.Body.String())
	var resp struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode create resp")
	require.NotZero(t, resp.ID, "instance id")
	return resp.ID
}

func wfGet(t *testing.T, mux http.Handler, id int64) wfDetail {
	t.Helper()
	rec := wfReq(t, mux, http.MethodGet, "/api/workflows/"+strconv.FormatInt(id, 10), nil)
	require.Equal(t, http.StatusOK, rec.Code, "GET detail body=%s", rec.Body.String())
	var d wfDetail
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &d), "decode detail")
	return d
}

func wfPatch(t *testing.T, mux http.Handler, id int64, nodeID string, body map[string]any) wfPatchResult {
	t.Helper()
	rec := wfReq(t, mux, http.MethodPatch,
		"/api/workflows/"+strconv.FormatInt(id, 10)+"/nodes/"+nodeID, body)
	require.Equal(t, http.StatusOK, rec.Code, "PATCH %s body=%s", nodeID, rec.Body.String())
	var res wfPatchResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res), "decode patch resp")
	return res
}

// Scenario: instantiate the embedded example template, then manually drive each
// node done in order — the happy path — and watch the instance walk to
// completed with successors readying as their predecessors settle.
func TestDashboardWorkflows_InstantiateAndWalkToCompleted(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreate(t, mux, "example:implement-microservice", "svc build",
		map[string]any{"service_name": "billing"})

	// Initial layout: plan (entry) ready, everything else pending.
	d := wfGet(t, mux, id)
	require.Equal(t, "running", d.Instance.Status)
	st := wfNodeStatuses(d)
	assert.Equal(t, "ready", st["plan"], "entry node plan should be ready")
	for _, n := range []string{"implement", "test", "review", "deploy", "done"} {
		assert.Equal(t, "pending", st[n], "%s should start pending", n)
	}
	assert.NotEmpty(t, d.Mermaid, "snapshot mermaid present")

	// Walk it. Each settle should ready the next node.
	assert.Contains(t, wfPatch(t, mux, id, "plan", map[string]any{"status": "done"}).Ready, "implement")
	assert.Contains(t, wfPatch(t, mux, id, "implement", map[string]any{"status": "done"}).Ready, "test")
	assert.Contains(t, wfPatch(t, mux, id, "test", map[string]any{"status": "done", "outcome": "pass"}).Ready, "review")
	assert.Contains(t, wfPatch(t, mux, id, "review", map[string]any{"status": "done", "outcome": "approved"}).Ready, "deploy")
	assert.Contains(t, wfPatch(t, mux, id, "deploy", map[string]any{"status": "done"}).Ready, "done")

	final := wfPatch(t, mux, id, "done", map[string]any{"status": "done"})
	assert.Equal(t, "completed", final.InstanceStatus, "instance should be completed after the last node")

	d = wfGet(t, mux, id)
	assert.Equal(t, "completed", d.Instance.Status)
	for n, s := range wfNodeStatuses(d) {
		assert.Equal(t, "done", s, "node %s should be done", n)
	}
}

// Scenario: an enum branch readies the taken successor and skips the sibling
// branch; a downstream JoinAll node fires once the live arm settles (its other
// predecessor having been skipped). The classic diamond.
func TestDashboardWorkflows_EnumBranchSkipAndJoin(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)

	root := t.TempDir()
	writeDiamondTemplate(t, root)
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:diamond", "", nil)

	// Layout: start (entry) ready; a/b/j/done pending.
	st := wfNodeStatuses(wfGet(t, mux, id))
	assert.Equal(t, "ready", st["start"])
	for _, n := range []string{"a", "b", "j", "done"} {
		assert.Equal(t, "pending", st[n], "%s pending", n)
	}

	// Take the left branch: a readies, b is skipped, j stays pending (still
	// waits for a), done stays pending.
	res := wfPatch(t, mux, id, "start", map[string]any{"status": "done", "outcome": "left"})
	assert.Equal(t, []string{"a"}, res.Ready, "left branch readies a")
	assert.Equal(t, []string{"b"}, res.Skipped, "right branch b is skipped")

	st = wfNodeStatuses(wfGet(t, mux, id))
	assert.Equal(t, "done", st["start"])
	assert.Equal(t, "ready", st["a"])
	assert.Equal(t, "skipped", st["b"])
	assert.Equal(t, "pending", st["j"], "join waits for the live arm")
	assert.Equal(t, "pending", st["done"])

	// Settle a → join j fires (its other predecessor b is settled/skipped).
	aRes := wfPatch(t, mux, id, "a", map[string]any{"status": "done"})
	assert.Contains(t, aRes.Ready, "j", "join should fire once the live arm settles")

	// Settle j → done readies; settle done → instance completed.
	assert.Contains(t, wfPatch(t, mux, id, "j", map[string]any{"status": "done"}).Ready, "done")
	final := wfPatch(t, mux, id, "done", map[string]any{"status": "done"})
	assert.Equal(t, "completed", final.InstanceStatus)
}

// Scenario: a done PATCH on an enum-verified node without an outcome is a 400 —
// the engine can't pick a branch without it.
func TestDashboardWorkflows_EnumNodeRequiresOutcome(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	root := t.TempDir()
	writeDiamondTemplate(t, root)
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))

	mux := agentd.BuildDashboardHandlerForTest()
	id := wfCreate(t, mux, "project:diamond", "", nil)

	rec := wfReq(t, mux, http.MethodPatch,
		"/api/workflows/"+strconv.FormatInt(id, 10)+"/nodes/start",
		map[string]any{"status": "done"})
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"enum node without outcome should 400; body=%s", rec.Body.String())
}

// Scenario: a missing required param is rejected at instantiation.
func TestDashboardWorkflows_MissingRequiredParam(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	rec := wfReq(t, mux, http.MethodPost, "/api/workflows", map[string]any{
		"template_ref": "example:implement-microservice",
		// service_name omitted
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"missing required param should 400; body=%s", rec.Body.String())
}

// Scenario: cancel marks the instance cancelled and skips the still-active
// nodes; delete removes it (404 afterward).
func TestDashboardWorkflows_CancelAndDelete(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreate(t, mux, "example:implement-microservice", "", map[string]any{"service_name": "x"})

	rec := wfReq(t, mux, http.MethodPost, "/api/workflows/"+strconv.FormatInt(id, 10)+"/cancel", nil)
	require.Equal(t, http.StatusOK, rec.Code, "cancel body=%s", rec.Body.String())

	d := wfGet(t, mux, id)
	assert.Equal(t, "cancelled", d.Instance.Status)
	for _, n := range d.Nodes {
		assert.Contains(t, []string{"done", "skipped", "failed"}, n.Status,
			"node %s should be terminal after cancel, got %s", n.NodeID, n.Status)
	}

	del := wfReq(t, mux, http.MethodDelete, "/api/workflows/"+strconv.FormatInt(id, 10), nil)
	require.Equal(t, http.StatusNoContent, del.Code, "delete body=%s", del.Body.String())

	gone := wfReq(t, mux, http.MethodGet, "/api/workflows/"+strconv.FormatInt(id, 10), nil)
	assert.Equal(t, http.StatusNotFound, gone.Code, "deleted instance should 404")
}

// Scenario: the per-node audit endpoint returns that node's timeline.
func TestDashboardWorkflows_NodeAudit(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreate(t, mux, "example:implement-microservice", "", map[string]any{"service_name": "x"})
	wfPatch(t, mux, id, "plan", map[string]any{"status": "done"})

	rec := wfReq(t, mux, http.MethodGet,
		"/api/workflows/"+strconv.FormatInt(id, 10)+"/nodes/plan/audit", nil)
	require.Equal(t, http.StatusOK, rec.Code, "audit body=%s", rec.Body.String())
	var resp struct {
		Events []struct {
			NodeID string `json:"node_id"`
			Kind   string `json:"kind"`
		} `json:"events"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Events, "plan should have at least a node_done event")
	for _, e := range resp.Events {
		assert.Equal(t, "plan", e.NodeID, "audit must be scoped to the node")
	}
}

// ----- Step 4: group binding, start/attach, approve gate ----------------

// wfNodePost drives POST /api/workflows/{id}/nodes/{node}/{sub} (start/attach/approve).
func wfNodePost(t *testing.T, mux http.Handler, id int64, node, sub string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return wfReq(t, mux, http.MethodPost,
		"/api/workflows/"+strconv.FormatInt(id, 10)+"/nodes/"+node+"/"+sub, body)
}

// Scenario: an instance binds to an existing group by name at create — the
// snapshot then carries the group_id + resolved group_name. An unknown group
// name is rejected (no auto-create); omitting it leaves the instance unbound.
func TestDashboardWorkflows_GroupBindingOnCreate(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	g := f.HaveGroup("squad")
	mux := agentd.BuildDashboardHandlerForTest()

	rec := wfReq(t, mux, http.MethodPost, "/api/workflows", map[string]any{
		"template_ref": "example:implement-microservice", "title": "bound",
		"params": map[string]any{"service_name": "b"}, "group": "squad",
	})
	require.Equal(t, http.StatusOK, rec.Code, "bound create body=%s", rec.Body.String())
	var created struct {
		ID      int64 `json:"id"`
		GroupID int64 `json:"group_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, g.ID, created.GroupID, "create response echoes the bound group id")

	// The snapshot resolves the group name off the bound id.
	snapRec := wfReq(t, mux, http.MethodGet, "/api/snapshot", nil)
	require.Equal(t, http.StatusOK, snapRec.Code)
	var snap struct {
		Workflows []struct {
			ID        int64  `json:"id"`
			GroupID   int64  `json:"group_id"`
			GroupName string `json:"group_name"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal(snapRec.Body.Bytes(), &snap))
	var found bool
	for _, wfi := range snap.Workflows {
		if wfi.ID == created.ID {
			found = true
			assert.Equal(t, g.ID, wfi.GroupID)
			assert.Equal(t, "squad", wfi.GroupName, "snapshot resolves group_name from group_id")
		}
	}
	assert.True(t, found, "bound instance should appear in snapshot")

	// Unknown group → 400, no auto-create.
	bad := wfReq(t, mux, http.MethodPost, "/api/workflows", map[string]any{
		"template_ref": "example:implement-microservice",
		"params":       map[string]any{"service_name": "b"}, "group": "ghost",
	})
	assert.Equal(t, http.StatusBadRequest, bad.Code, "unknown group should 400; body=%s", bad.Body.String())

	// Omitted group → unbound (group_id 0).
	unbound := wfReq(t, mux, http.MethodPost, "/api/workflows", map[string]any{
		"template_ref": "example:implement-microservice", "params": map[string]any{"service_name": "b"},
	})
	require.Equal(t, http.StatusOK, unbound.Code, "unbound create body=%s", unbound.Body.String())
	var ub struct {
		GroupID int64 `json:"group_id"`
	}
	require.NoError(t, json.Unmarshal(unbound.Body.Bytes(), &ub))
	assert.Zero(t, ub.GroupID, "omitted group leaves the instance unbound")
}

// Scenario: starting a ready ai node spawns a fresh agent into the instance's
// bound group (via the shared executeSpawn core), marks the node running, and
// records the spawned conv-id as the assignee. The new agent joins the group.
func TestDashboardWorkflows_StartSpawnsAgentIntoGroup(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreateInGroup(t, mux, "example:implement-microservice", "build", map[string]any{"service_name": "b"}, "squad")

	rec := wfNodePost(t, mux, id, "plan", "start", nil)
	require.Equal(t, http.StatusOK, rec.Code, "start plan body=%s", rec.Body.String())
	var out struct {
		Status   string `json:"status"`
		Assignee string `json:"assignee"`
		ConvID   string `json:"conv_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "running", out.Status)
	assert.NotEmpty(t, out.Assignee, "spawned conv-id recorded as assignee")
	assert.Equal(t, out.Assignee, out.ConvID)

	// Node is running in the detail view.
	assert.Equal(t, "running", wfNodeStatuses(wfGet(t, mux, id))["plan"])

	// The spawned agent is now a member of the bound group.
	members := f.ListGroupMembers("squad")
	var sawSpawned bool
	for _, m := range members {
		if m.ConvID == out.ConvID {
			sawSpawned = true
		}
	}
	assert.True(t, sawSpawned, "spawned agent %q should be a member of squad", out.ConvID)
}

// Scenario: start's preconditions — unbound instance (400), non-ai node (400),
// a node that isn't ready (409), and a double-start (409).
func TestDashboardWorkflows_StartGuards(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	mux := agentd.BuildDashboardHandlerForTest()

	// Unbound instance: plan is ai+ready, but there's no group → 400.
	unbound := wfCreate(t, mux, "example:implement-microservice", "", map[string]any{"service_name": "b"})
	assert.Equal(t, http.StatusBadRequest, wfNodePost(t, mux, unbound, "plan", "start", nil).Code,
		"start on an unbound instance should 400")

	// Non-ai node: the diamond's entry node is a human executor → 400.
	root := t.TempDir()
	writeDiamondTemplate(t, root)
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))
	dia := wfCreate(t, mux, "project:diamond", "", nil)
	assert.Equal(t, http.StatusBadRequest, wfNodePost(t, mux, dia, "start", "start", nil).Code,
		"start on a non-ai node should 400")

	// Bound instance for the ready/double-start checks.
	id := wfCreateInGroup(t, mux, "example:implement-microservice", "", map[string]any{"service_name": "b"}, "squad")
	// implement is pending (not on the frontier) → 409.
	assert.Equal(t, http.StatusConflict, wfNodePost(t, mux, id, "implement", "start", nil).Code,
		"start on a pending node should 409")
	// First start of plan succeeds; the second 409s (no longer ready).
	require.Equal(t, http.StatusOK, wfNodePost(t, mux, id, "plan", "start", nil).Code)
	assert.Equal(t, http.StatusConflict, wfNodePost(t, mux, id, "plan", "start", nil).Code,
		"double start should 409")
}

// Scenario: attach assigns an existing group member to a ready node and delivers
// the node's task to that member's inbox — no new agent is spawned. A non-member
// conv-id and an empty conv-id are both rejected.
func TestDashboardWorkflows_AttachExistingMember(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	_ = f.HaveGroup("squad")
	f.HaveMember("squad", "worker-1")
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreateInGroup(t, mux, "example:implement-microservice", "", map[string]any{"service_name": "b"}, "squad")

	// Empty conv_id → 400.
	assert.Equal(t, http.StatusBadRequest, wfNodePost(t, mux, id, "plan", "attach", map[string]any{}).Code,
		"attach without conv_id should 400")

	// Non-member conv → 400.
	assert.Equal(t, http.StatusBadRequest,
		wfNodePost(t, mux, id, "plan", "attach", map[string]any{"conv_id": "stranger"}).Code,
		"attach of a non-member should 400")

	// Member conv → 200, node running + assigned, task delivered to inbox.
	rec := wfNodePost(t, mux, id, "plan", "attach", map[string]any{"conv_id": "worker-1"})
	require.Equal(t, http.StatusOK, rec.Code, "attach member body=%s", rec.Body.String())
	var out struct {
		Status   string `json:"status"`
		Assignee string `json:"assignee"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "running", out.Status)
	assert.Equal(t, "worker-1", out.Assignee)
	assert.Equal(t, "running", wfNodeStatuses(wfGet(t, mux, id))["plan"])

	msgs, err := db.ListAgentMessagesForConv("worker-1", 10)
	require.NoError(t, err)
	require.NotEmpty(t, msgs, "attach should deliver the node task to the member's inbox")
	assert.Contains(t, msgs[0].Subject, "Workflow task", "inbox message names the workflow task")
}

// Scenario: the human-verify gate. A node whose verify.kind is human goes
// running, then approve settles it done and advances the frontier (its unlabeled
// successor readies) — using the same Advance the manual settle uses.
func TestDashboardWorkflows_HumanApproveAdvancesFrontier(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreate(t, mux, "example:implement-microservice", "", map[string]any{"service_name": "b"})

	// plan (verify: human) → running, then approve.
	wfPatch(t, mux, id, "plan", map[string]any{"status": "running"})
	rec := wfNodePost(t, mux, id, "plan", "approve", map[string]any{"decision": "approve", "note": "looks good"})
	require.Equal(t, http.StatusOK, rec.Code, "approve plan body=%s", rec.Body.String())
	var out struct {
		Status         string   `json:"status"`
		InstanceStatus string   `json:"instance_status"`
		Ready          []string `json:"ready"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "done", out.Status)
	assert.Contains(t, out.Ready, "implement", "approving plan readies its successor")

	st := wfNodeStatuses(wfGet(t, mux, id))
	assert.Equal(t, "done", st["plan"])
	assert.Equal(t, "ready", st["implement"])
}

// Scenario: reject records the rejection (audit event) and does NOT advance —
// the node stays running so it can be re-worked, and successors stay pending.
func TestDashboardWorkflows_HumanRejectRecordsNoAdvance(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreate(t, mux, "example:implement-microservice", "", map[string]any{"service_name": "b"})
	wfPatch(t, mux, id, "plan", map[string]any{"status": "running"})

	rec := wfNodePost(t, mux, id, "plan", "approve", map[string]any{"decision": "reject", "note": "needs work"})
	require.Equal(t, http.StatusOK, rec.Code, "reject plan body=%s", rec.Body.String())

	st := wfNodeStatuses(wfGet(t, mux, id))
	assert.Equal(t, "running", st["plan"], "reject leaves the node running for re-work")
	assert.Equal(t, "pending", st["implement"], "reject does not advance the frontier")

	// The rejection is on the node's audit timeline.
	audit := wfReq(t, mux, http.MethodGet,
		"/api/workflows/"+strconv.FormatInt(id, 10)+"/nodes/plan/audit", nil)
	require.Equal(t, http.StatusOK, audit.Code)
	assert.Contains(t, audit.Body.String(), "node_rejected", "audit records the rejection")
}

// Scenario: approve guards — the gate only applies to human-verify nodes (400
// otherwise), and the node must have run first (409 on a not-yet-started node).
func TestDashboardWorkflows_ApproveGuards(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreate(t, mux, "example:implement-microservice", "", map[string]any{"service_name": "b"})

	// plan is human-verify but still ready (not started) → 409.
	assert.Equal(t, http.StatusConflict,
		wfNodePost(t, mux, id, "plan", "approve", map[string]any{"decision": "approve"}).Code,
		"approving a not-yet-running node should 409")

	// implement has tool verify, not human → 400 once it is running.
	wfPatch(t, mux, id, "plan", map[string]any{"status": "done"}) // readies implement
	wfPatch(t, mux, id, "implement", map[string]any{"status": "running"})
	assert.Equal(t, http.StatusBadRequest,
		wfNodePost(t, mux, id, "implement", "approve", map[string]any{"decision": "approve"}).Code,
		"approving a non-human-verify node should 400")

	// Bad decision value → 400.
	assert.Equal(t, http.StatusBadRequest,
		wfNodePost(t, mux, id, "plan", "approve", map[string]any{"decision": "maybe"}).Code,
		"an unknown decision should 400")
}

// Scenario: manual PATCH may only drive running/done/failed; a direct hop to
// skipped (which would strand the sub-tree) or pending is rejected. (Cold-review
// #230 hardening.)
func TestDashboardWorkflows_ManualSkipRejected(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreate(t, mux, "example:implement-microservice", "", map[string]any{"service_name": "b"})
	for _, bad := range []string{"skipped", "pending", "ready", "awaiting_verify"} {
		rec := wfReq(t, mux, http.MethodPatch,
			"/api/workflows/"+strconv.FormatInt(id, 10)+"/nodes/plan",
			map[string]any{"status": bad})
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"manual PATCH to %q should 400; body=%s", bad, rec.Body.String())
	}
}

// Scenario (L3): a manual PATCH must NOT be able to stamp the engine-owner
// sentinel as a node's assignee — that marker is what the startup reaper trusts
// to tell an engine corpse from a human-driven node, so a client setting it
// could trick the reaper into resetting (and the engine into re-running) the
// node. The assignee write is rejected with 400.
func TestDashboardWorkflows_RejectsEngineSentinelAssignee(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreate(t, mux, "example:implement-microservice", "", map[string]any{"service_name": "b"})
	rec := wfReq(t, mux, http.MethodPatch,
		"/api/workflows/"+strconv.FormatInt(id, 10)+"/nodes/plan",
		map[string]any{"assignee": "<workflow-engine>"})
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"PATCH setting the engine sentinel assignee should 400; body=%s", rec.Body.String())
}

// Scenario: the node JSON surfaces the template's executor.Agent hint and
// verify.kind (Step 5's intended-agent overlay + approve affordance), and the
// detail payload carries a (possibly empty) warnings array.
func TestDashboardWorkflows_NodeJSONAgentVerifyAndWarnings(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreate(t, mux, "example:implement-microservice", "", map[string]any{"service_name": "b"})
	rec := wfReq(t, mux, http.MethodGet, "/api/workflows/"+strconv.FormatInt(id, 10), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var detail struct {
		Nodes []struct {
			NodeID     string `json:"node_id"`
			Agent      string `json:"agent"`
			VerifyKind string `json:"verify_kind"`
		} `json:"nodes"`
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
	byID := map[string]struct{ agent, verify string }{}
	for _, n := range detail.Nodes {
		byID[n.NodeID] = struct{ agent, verify string }{n.Agent, n.VerifyKind}
	}
	assert.Equal(t, "planner", byID["plan"].agent, "plan surfaces its executor.agent hint")
	assert.Equal(t, "human", byID["plan"].verify, "plan surfaces verify.kind human")
	assert.Equal(t, "implementor", byID["implement"].agent)
	assert.NotNil(t, detail.Warnings, "warnings is present (empty array when clean)")
}

// Scenario: a template with an enum value that has no outgoing edge is a
// topology smell; the warning rides through on both the templates snapshot and
// the instance detail payload.
func TestDashboardWorkflows_Warnings(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	root := t.TempDir()
	writeLeakyEnumTemplate(t, root)
	t.Cleanup(agentd.SetWorkflowProjectDirsForTest(root))
	mux := agentd.BuildDashboardHandlerForTest()

	// Templates snapshot carries the warning.
	snapRec := wfReq(t, mux, http.MethodGet, "/api/snapshot", nil)
	require.Equal(t, http.StatusOK, snapRec.Code)
	var snap struct {
		WorkflowTemplates []struct {
			Ref      string   `json:"ref"`
			Warnings []string `json:"warnings"`
		} `json:"workflow_templates"`
	}
	require.NoError(t, json.Unmarshal(snapRec.Body.Bytes(), &snap))
	var tplWarned bool
	for _, tpl := range snap.WorkflowTemplates {
		if tpl.Ref == "project:leaky" {
			tplWarned = len(tpl.Warnings) > 0
		}
	}
	assert.True(t, tplWarned, "leaky template should carry a topology warning in the snapshot")

	// Instance detail re-derives the same warning off the snapshotted chart.
	id := wfCreate(t, mux, "project:leaky", "", nil)
	detRec := wfReq(t, mux, http.MethodGet, "/api/workflows/"+strconv.FormatInt(id, 10), nil)
	require.Equal(t, http.StatusOK, detRec.Code)
	var detail struct {
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(detRec.Body.Bytes(), &detail))
	assert.NotEmpty(t, detail.Warnings, "instance detail surfaces the topology warning")
}

// wfCreateInGroup instantiates a template bound to an existing agent group.
func wfCreateInGroup(t *testing.T, mux http.Handler, ref, title string, params map[string]any, group string) int64 {
	t.Helper()
	rec := wfReq(t, mux, http.MethodPost, "/api/workflows", map[string]any{
		"template_ref": ref, "title": title, "params": params, "group": group,
	})
	require.Equal(t, http.StatusOK, rec.Code, "POST /api/workflows (group=%s) body=%s", group, rec.Body.String())
	var resp struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotZero(t, resp.ID)
	return resp.ID
}

// writeLeakyEnumTemplate lays down a template whose enum node declares a value
// ("hold") with no matching outgoing edge — the Step-2b enum-coverage smell.
func writeLeakyEnumTemplate(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "leaky")
	nodes := filepath.Join(dir, "nodes")
	require.NoError(t, os.MkdirAll(nodes, 0o755))
	write := func(rel, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644))
	}
	write("workflow.yaml", "name: leaky\ndescription: enum with an uncovered value\nentry: gate\n")
	write("flow.mmd", "flowchart TD\n"+
		"  gate{Gate} -->|go| done\n")
	write("nodes/gate.yaml", "label: Gate\nexecutor:\n  kind: human\nverify:\n  kind: enum\n  values: [go, hold]\n")
	write("nodes/done.yaml", "label: Done\nexecutor:\n  kind: human\n")
}

// Scenario: the snapshot carries workflow instances (with node counts) and the
// discoverable templates — what the Workflows tab renders off the 2s poll.
func TestDashboardWorkflows_Snapshot(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreate(t, mux, "example:implement-microservice", "snap", map[string]any{"service_name": "x"})

	rec := wfReq(t, mux, http.MethodGet, "/api/snapshot", nil)
	require.Equal(t, http.StatusOK, rec.Code, "snapshot body=%s", rec.Body.String())
	var snap struct {
		Workflows []struct {
			ID     int64  `json:"id"`
			Title  string `json:"title"`
			Status string `json:"status"`
			Total  int    `json:"total"`
		} `json:"workflows"`
		WorkflowTemplates []struct {
			Ref    string `json:"ref"`
			Source string `json:"source"`
		} `json:"workflow_templates"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap), "decode snapshot")

	var found bool
	for _, wfi := range snap.Workflows {
		if wfi.ID == id {
			found = true
			assert.Equal(t, "running", wfi.Status)
			assert.Equal(t, 6, wfi.Total, "example has 6 nodes")
		}
	}
	assert.True(t, found, "instance %d should appear in snapshot.workflows", id)

	var sawExample bool
	for _, tpl := range snap.WorkflowTemplates {
		if tpl.Ref == "example:implement-microservice" {
			sawExample = true
			assert.Equal(t, "example", tpl.Source)
		}
	}
	assert.True(t, sawExample, "example template should appear in snapshot.workflow_templates")
}

// Scenario: the workflow routes are behind the dashboard cookie gate — an
// uncookied create is refused.
func TestDashboardWorkflows_AuthRequired(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)

	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux) // no cookie injection
	r := httptest.NewRequest(http.MethodPost, "/api/workflows",
		strings.NewReader(`{"template_ref":"example:implement-microservice","params":{"service_name":"x"}}`))
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	assert.NotEqual(t, http.StatusOK, rec.Code,
		"uncookied POST /api/workflows should be refused; body=%s", rec.Body.String())
}

// Scenario: a non-settling PATCH (output only) on a cancelled instance must NOT
// resurrect it. After cancel every node is skipped, so the status recompute
// would read "all terminal → completed" and overwrite "cancelled" — the
// instance-running guard freezes the recompute instead. (Uses an output-only
// patch so it bypasses the re-settle 409 guard and reaches the recompute path.)
func TestDashboardWorkflows_TerminalInstanceNotResurrected(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreate(t, mux, "example:implement-microservice", "", map[string]any{"service_name": "x"})

	rec := wfReq(t, mux, http.MethodPost, "/api/workflows/"+strconv.FormatInt(id, 10)+"/cancel", nil)
	require.Equal(t, http.StatusOK, rec.Code, "cancel body=%s", rec.Body.String())

	res := wfPatch(t, mux, id, "plan", map[string]any{"output": "late note"})
	assert.Equal(t, "cancelled", res.InstanceStatus, "cancelled instance must stay cancelled")
	assert.Equal(t, "cancelled", wfGet(t, mux, id).Instance.Status, "still cancelled in detail")
}

// Scenario: re-settling an already-done node is rejected (409) — it would
// duplicate audit events and re-run advance over stale state.
func TestDashboardWorkflows_ReSettleRejected(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreate(t, mux, "example:implement-microservice", "", map[string]any{"service_name": "x"})
	wfPatch(t, mux, id, "plan", map[string]any{"status": "done"})

	rec := wfReq(t, mux, http.MethodPatch,
		"/api/workflows/"+strconv.FormatInt(id, 10)+"/nodes/plan",
		map[string]any{"status": "done"})
	assert.Equal(t, http.StatusConflict, rec.Code,
		"re-settling a done node should 409; body=%s", rec.Body.String())
}

// writeDiamondTemplate lays down a minimal branch+join template under
// <root>/diamond so it resolves as "project:diamond":
//
//	start{enum left|right} --|left|--> a --> j --> done
//	                       --|right|-> b --> j
func writeDiamondTemplate(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "diamond")
	nodes := filepath.Join(dir, "nodes")
	require.NoError(t, os.MkdirAll(nodes, 0o755))

	write := func(rel, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644))
	}
	write("workflow.yaml", "name: diamond\ndescription: branch and join\nentry: start\n")
	write("flow.mmd", "flowchart TD\n"+
		"  start{Pick} -->|left| a\n"+
		"  start -->|right| b\n"+
		"  a --> j\n"+
		"  b --> j\n"+
		"  j --> done\n")
	write("nodes/start.yaml", "label: Pick\nexecutor:\n  kind: human\nverify:\n  kind: enum\n  values: [left, right]\n")
	write("nodes/a.yaml", "label: A\nexecutor:\n  kind: human\n")
	write("nodes/b.yaml", "label: B\nexecutor:\n  kind: human\n")
	write("nodes/j.yaml", "label: Join\nexecutor:\n  kind: human\n")
	write("nodes/done.yaml", "label: Done\nexecutor:\n  kind: human\n")
}
