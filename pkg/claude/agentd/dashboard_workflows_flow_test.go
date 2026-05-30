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

// Scenario: node start/attach are Step 4 — they return 501 for now.
func TestDashboardWorkflows_StartAttachStubbed(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_ = newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	id := wfCreate(t, mux, "example:implement-microservice", "", map[string]any{"service_name": "x"})
	for _, sub := range []string{"start", "attach"} {
		rec := wfReq(t, mux, http.MethodPost,
			"/api/workflows/"+strconv.FormatInt(id, 10)+"/nodes/plan/"+sub, nil)
		assert.Equal(t, http.StatusNotImplemented, rec.Code, "%s should be 501; body=%s", sub, rec.Body.String())
	}
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
			ID    int64  `json:"id"`
			Title string `json:"title"`
			Status string `json:"status"`
			Total int    `json:"total"`
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
