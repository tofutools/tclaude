package agentd_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// dashReq builds a dashboard (/api) request. The dashTestHandler injects the
// session cookie; checkDashboardAuth also needs an Origin (any value passes
// while the test popupBaseURL is empty — every string has the "" prefix).
func dashReq(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	r := testharness.JSONRequest(t, method, path, body)
	r.Header.Set("Origin", "http://localhost")
	return r
}

// requestExport POSTs the dashboard create endpoint and returns the new job id.
// The job comes back in the leading 'cloning' phase (the daemon is spawning the
// isolated summary-writer clone in the background).
func requestExport(t *testing.T, dash http.Handler, conv string, body map[string]any) int64 {
	t.Helper()
	rec := testharness.Serve(dash, dashReq(t, http.MethodPost,
		"/api/agents/"+conv+"/export", body))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created struct {
		ID     int64  `json:"id"`
		ConvID string `json:"conv_id"`
		Status string `json:"status"`
	}
	testharness.DecodeJSON(t, rec, &created)
	require.Positive(t, created.ID)
	require.Equal(t, conv, created.ConvID, "job stays keyed on the ORIGINAL conv")
	require.Equal(t, "cloning", created.Status, "export starts in the cloning phase")
	return created.ID
}

// awaitExportClone polls until the export job's worker clone has been spawned
// and its pane registered, returning the clone's conv-id and tmux pane target.
// The clone — not the original — is who gets nudged and who submits.
func awaitExportClone(t *testing.T, jobID int64, timeout time.Duration) (workerConv, tmuxTarget string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if job, err := db.GetExportJob(jobID); err == nil && job.WorkerConvID != "" {
			if s, _ := db.FindSessionByConvID(job.WorkerConvID); s != nil && s.TmuxSession != "" {
				return job.WorkerConvID, s.TmuxSession + ":0.0"
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("export clone never materialised for job %d within %s", jobID, timeout)
	return "", ""
}

// submitAsWorker uploads an artifact as the worker clone — the self-scoped /v1
// caller the daemon nudged. Returns the recorder so the caller can assert.
func submitAsWorker(t *testing.T, mux http.Handler, jobID int64, workerConv, name string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	subReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v1/export-jobs/%d/artifact?name=%s", jobID, name),
		bytes.NewReader(data))
	subReq.Header.Set("Content-Type", "text/markdown")
	return testharness.Serve(mux, agentd.AsAgentPeer(subReq, workerConv))
}

// TestExportFlow_HappyPath drives the whole clone-based export round-trip across
// the real surfaces: the dashboard create endpoint, the CLONE spawn + nudge (the
// original is never touched), the clone's /v1 show + submit, the dashboard poll +
// download, and the clone's auto-deletion once the artifact lands.
func TestExportFlow_HappyPath(t *testing.T) {
	f := newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()

	f.HaveGroup("g")
	f.HaveConvWithTitle("exp00000-0000-4000-8000-000000000001", "research-agent")
	f.HaveAliveSession("exp00000-0000-4000-8000-000000000001", "expagent", "tmux-exp-1", "/tmp/exp")
	f.HaveMember("g", "exp00000-0000-4000-8000-000000000001")

	// 1. Human clicks "📋 summary…": the daemon creates a cloning job.
	jobID := requestExport(t, dash, "exp00000-0000-4000-8000-000000000001", map[string]any{
		"title":        "Auth research",
		"instructions": "keep it concise for non-engineers",
		"preset":       "summary",
	})

	// 2. A clone is spawned and ITS pane (not the original's) is nudged with a
	// fixed-format POINTER — never the raw instructions.
	workerConv, cloneTmux := awaitExportClone(t, jobID, 5*time.Second)
	require.NotEqual(t, "exp00000-0000-4000-8000-000000000001", workerConv, "the worker is a clone, not the original")
	f.AssertSentContains(cloneTmux, fmt.Sprintf("export show %d", jobID), 3*time.Second)
	for _, sk := range f.World.Tmux.Sent() {
		assert.NotContains(t, sk.Text, "keep it concise for non-engineers",
			"raw instructions must never ride send-keys")
		if sk.Target == "tmux-exp-1:0.0" {
			assert.NotContains(t, sk.Text, "export show",
				"the LIVE original must never be nudged — the whole point of cloning")
		}
	}

	// 3. The clone fetches the brief — status flips to running.
	showRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet,
			fmt.Sprintf("/v1/export-jobs/%d", jobID), nil), workerConv))
	require.Equal(t, http.StatusOK, showRec.Code, showRec.Body.String())
	var brief struct {
		Status       string `json:"status"`
		Title        string `json:"title"`
		Instructions string `json:"instructions"`
		Preset       string `json:"preset"`
		SubmitHint   string `json:"submit_hint"`
	}
	testharness.DecodeJSON(t, showRec, &brief)
	assert.Equal(t, "running", brief.Status)
	assert.Equal(t, "Auth research", brief.Title)
	assert.Equal(t, "keep it concise for non-engineers", brief.Instructions)
	assert.Equal(t, "summary", brief.Preset)
	assert.Contains(t, brief.SubmitHint, fmt.Sprintf("export submit %d", jobID))

	// 4. The clone uploads the artifact (raw body + filename query param).
	artifact := []byte("# Auth research\n\nKey findings…\n")
	subRec := submitAsWorker(t, f.Mux, jobID, workerConv, "auth-research.md", artifact)
	require.Equal(t, http.StatusOK, subRec.Code, subRec.Body.String())

	// 5. Dashboard poll (keyed on the ORIGINAL job id) sees it ready.
	pollRec := testharness.Serve(dash, dashReq(t, http.MethodGet,
		fmt.Sprintf("/api/export-jobs/%d", jobID), nil))
	require.Equal(t, http.StatusOK, pollRec.Code)
	var view struct {
		ConvID       string `json:"conv_id"`
		Status       string `json:"status"`
		Ready        bool   `json:"ready"`
		ArtifactName string `json:"artifact_name"`
		ArtifactSize int64  `json:"artifact_size"`
	}
	testharness.DecodeJSON(t, pollRec, &view)
	assert.Equal(t, "exp00000-0000-4000-8000-000000000001", view.ConvID, "the export stays attached to the original")
	assert.Equal(t, "ready", view.Status)
	assert.True(t, view.Ready)
	assert.Equal(t, "auth-research.md", view.ArtifactName)
	assert.Equal(t, int64(len(artifact)), view.ArtifactSize)

	// 6. Download returns the bytes with an attachment disposition.
	dlRec := testharness.Serve(dash, dashReq(t, http.MethodGet,
		fmt.Sprintf("/api/export-jobs/%d/artifact", jobID), nil))
	require.Equal(t, http.StatusOK, dlRec.Code)
	assert.Equal(t, artifact, dlRec.Body.Bytes())
	assert.Contains(t, dlRec.Header().Get("Content-Disposition"), "auth-research.md")

	// 7. The throwaway clone is auto-deleted once the export lands.
	assertCloneDeleted(t, workerConv, 3*time.Second)
}

// assertCloneDeleted polls until the worker clone's session rows are gone — the
// auto-delete (kill session + purge conv) runs in the background after submit.
func assertCloneDeleted(t *testing.T, workerConv string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rows, _ := db.FindSessionsByConvID(workerConv); len(rows) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows, _ := db.FindSessionsByConvID(workerConv)
	t.Fatalf("export clone %s was not deleted within %s; %d session rows remain",
		workerConv, timeout, len(rows))
}

// TestExportFlow_OfflineOriginalStillExports proves the clone-based export works
// even when the original has NO live session — a clone resumes from the .jsonl,
// so the old "agent must be online" fast-fail is gone. The export succeeds and
// nudges the clone, not the (offline) original.
func TestExportFlow_OfflineOriginalStillExports(t *testing.T) {
	f := newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()

	f.HaveGroup("g")
	f.HaveAliveSession("off00000-0000-4000-8000-000000000002", "offagent", "tmux-off-1", "/tmp/off")
	f.HaveMember("g", "off00000-0000-4000-8000-000000000002")
	// Take the original offline — its .jsonl + recorded cwd still exist, so the
	// conversation is cloneable.
	f.MarkOffline("tmux-off-1")

	jobID := requestExport(t, dash, "off00000-0000-4000-8000-000000000002", map[string]any{"preset": "summary"})

	workerConv, cloneTmux := awaitExportClone(t, jobID, 5*time.Second)
	require.NotEqual(t, "off00000-0000-4000-8000-000000000002", workerConv)
	f.AssertSentContains(cloneTmux, fmt.Sprintf("export show %d", jobID), 3*time.Second)

	// And the export completes end-to-end on the clone.
	subRec := submitAsWorker(t, f.Mux, jobID, workerConv, "summary.md", []byte("# summary\n"))
	require.Equal(t, http.StatusOK, subRec.Code, subRec.Body.String())
	assertCloneDeleted(t, workerConv, 3*time.Second)
}

// TestExportFlow_StandaloneCloneIsIsolated proves the DEFAULT clone is standalone
// — it does NOT join the original's group, so peers / cron / multicast can't
// touch the throwaway.
func TestExportFlow_StandaloneCloneIsIsolated(t *testing.T) {
	f := newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()

	f.HaveGroup("g")
	f.HaveAliveSession("iso00000-0000-4000-8000-000000000003", "isoagent", "tmux-iso-1", "/tmp/iso")
	f.HaveMember("g", "iso00000-0000-4000-8000-000000000003")

	jobID := requestExport(t, dash, "iso00000-0000-4000-8000-000000000003", map[string]any{"preset": "summary"})
	workerConv, _ := awaitExportClone(t, jobID, 5*time.Second)

	// The clone never joins the group (standalone is the default).
	f.AssertNotGroupMember("g", workerConv)
	groups, err := db.ListGroupsForConv(workerConv)
	require.NoError(t, err)
	assert.Empty(t, groups, "standalone export clone is in no group")
}

// TestExportFlow_SameGroupCloneJoinsGroup proves the "Clone into the same group"
// opt-in (same_group=true) puts the summary writer in the original's group, so it
// can ping peers.
func TestExportFlow_SameGroupCloneJoinsGroup(t *testing.T) {
	f := newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()

	const orig = "sg000000-0000-4000-8000-000000000004"
	g := f.HaveGroup("g")
	f.HaveAliveSession(orig, "sgagent", "tmux-sg-1", "/tmp/sg")
	f.HaveMember("g", orig)
	// Make the ORIGINAL a group owner, so we can prove the clone inherits
	// membership but NOT ownership (a throwaway summary writer must never get
	// administrative control over the group).
	require.NoError(t, db.AddAgentGroupOwner(g.ID, orig, "test"))

	jobID := requestExport(t, dash, orig, map[string]any{
		"preset":     "summary",
		"same_group": true,
	})
	workerConv, _ := awaitExportClone(t, jobID, 5*time.Second)

	// The clone joins the original's group (poll — identity copy is async).
	deadline := time.Now().Add(3 * time.Second)
	joined := false
	for time.Now().Before(deadline) && !joined {
		for _, m := range f.ListGroupMembers("g") {
			if m.ConvID == workerConv {
				joined = true
				break
			}
		}
		if !joined {
			time.Sleep(20 * time.Millisecond)
		}
	}
	require.True(t, joined, "same_group clone %s should join group g", workerConv)

	// …but it is NEVER a group owner — ownership is deliberately not inherited.
	owned, err := db.ListGroupsOwnedBy(workerConv)
	require.NoError(t, err)
	assert.Empty(t, owned, "same_group clone must not inherit group ownership")
}

// TestExportFlow_History drives the "Previous exports" panel surfaces: list,
// per-entry delete, and clear-all, across two completed exports for one agent.
// Each export runs (and auto-deletes) its own clone; the history stays keyed on
// the ORIGINAL conv.
func TestExportFlow_History(t *testing.T) {
	f := newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()

	f.HaveGroup("g")
	f.HaveAliveSession("h0000000-0000-4000-8000-000000000005", "hagent", "tmux-h-1", "/tmp/h")
	f.HaveMember("g", "h0000000-0000-4000-8000-000000000005")

	mkExport := func(name string) int64 {
		jobID := requestExport(t, dash, "h0000000-0000-4000-8000-000000000005", map[string]any{"preset": "summary"})
		workerConv, _ := awaitExportClone(t, jobID, 5*time.Second)
		require.Equal(t, http.StatusOK,
			submitAsWorker(t, f.Mux, jobID, workerConv, name, []byte("data:"+name)).Code)
		return jobID
	}
	id1 := mkExport("one.md")
	id2 := mkExport("two.md")

	// History lists both, newest first.
	listExports := func() []map[string]any {
		rec := testharness.Serve(dash, dashReq(t, http.MethodGet, "/api/agents/h0000000-0000-4000-8000-000000000005/exports", nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out struct {
			Exports []map[string]any `json:"exports"`
		}
		testharness.DecodeJSON(t, rec, &out)
		return out.Exports
	}
	jobs := listExports()
	require.Len(t, jobs, 2)
	assert.Equal(t, float64(id2), jobs[0]["id"], "newest first")
	assert.Equal(t, "ready", jobs[0]["status"])

	// Delete one entry.
	delRec := testharness.Serve(dash, dashReq(t, http.MethodDelete,
		fmt.Sprintf("/api/export-jobs/%d", id1), nil))
	require.Equal(t, http.StatusOK, delRec.Code, delRec.Body.String())
	require.Len(t, listExports(), 1)

	// Clear all wipes the rest.
	clrRec := testharness.Serve(dash, dashReq(t, http.MethodDelete, "/api/agents/h0000000-0000-4000-8000-000000000005/exports", nil))
	require.Equal(t, http.StatusOK, clrRec.Code, clrRec.Body.String())
	assert.Empty(t, listExports())
}

// TestExportFlow_NonCloneableFastFails proves an export of a conversation with no
// locatable metadata (no session, no index, no harness-native conv) is refused up
// front (409) rather than queuing a job whose clone can never be spawned.
func TestExportFlow_NonCloneableFastFails(t *testing.T) {
	f := newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()

	// A conv that RESOLVES (it has a conv_index row, so the dashboard finds it)
	// but has no session and no recorded working directory — nowhere to spawn a
	// clone into. createExportJob must reject it up front rather than queue a job
	// whose clone can never come up.
	const ghost = "ghost000-0000-4000-8000-000000000099"
	f.HaveConvWithTitle(ghost, "ghost-agent")

	rec := testharness.Serve(dash, dashReq(t, http.MethodPost,
		"/api/agents/"+ghost+"/export", map[string]any{"preset": "summary"}))
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "clone")
}

// TestExportFlow_CrossAgentSubmitDenied proves a third agent — neither the
// original nor the worker clone — cannot upload into an export job. The
// self-scoped ownership gate accepts only conv_id or worker_conv_id.
func TestExportFlow_CrossAgentSubmitDenied(t *testing.T) {
	f := newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()

	f.HaveGroup("g")
	f.HaveAliveSession("own00000-0000-4000-8000-000000000006", "owner", "tmux-own-1", "/tmp/own")
	f.HaveMember("g", "own00000-0000-4000-8000-000000000006")

	jobID := requestExport(t, dash, "own00000-0000-4000-8000-000000000006", map[string]any{"preset": "summary"})

	// Wait until the worker clone is recorded, so the gate has BOTH a real
	// conv_id and a real worker_conv_id to reject against — otherwise the test
	// could pass trivially while worker_conv_id is still empty.
	workerConv, _ := awaitExportClone(t, jobID, 5*time.Second)
	require.NotEqual(t, "intruder0-0000-4000-8000-000000000007", workerConv)

	// A third agent — neither the original nor the worker clone — tries to submit.
	subRec := submitAsWorker(t, f.Mux, jobID, "intruder0-0000-4000-8000-000000000007", "x.md", []byte("nope"))
	assert.Equal(t, http.StatusForbidden, subRec.Code, subRec.Body.String())
}
