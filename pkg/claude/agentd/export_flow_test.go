package agentd_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
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

// TestExportFlow_HappyPath drives the whole per-agent export round-trip across
// the real surfaces: the dashboard create endpoint, the pane nudge, the agent's
// /v1 show + submit, and the dashboard poll + download.
func TestExportFlow_HappyPath(t *testing.T) {
	f := newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()

	f.HaveGroup("g")
	f.HaveAliveSession("conv-exp", "expagent", "tmux-exp-1", "/tmp/exp")
	f.HaveMember("g", "conv-exp")

	// 1. Human clicks "📋 summary…": dashboard creates the job.
	rec := testharness.Serve(dash, dashReq(t, http.MethodPost,
		"/api/agents/conv-exp/export", map[string]any{
			"title":        "Auth research",
			"instructions": "keep it concise for non-engineers",
			"preset":       "summary",
		}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	testharness.DecodeJSON(t, rec, &created)
	require.Positive(t, created.ID)
	require.Equal(t, "requested", created.Status)

	// 2. The pane got a POINTER nudge — and NOT the raw instructions.
	f.AssertSentContains("tmux-exp-1:0.0",
		fmt.Sprintf("export show %d", created.ID), 2*time.Second)
	for _, sk := range f.World.Tmux.Sent() {
		assert.NotContains(t, sk.Text, "keep it concise for non-engineers",
			"raw instructions must never ride send-keys")
	}

	// 3. Agent fetches the brief — status flips to running.
	showRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet,
			fmt.Sprintf("/v1/export-jobs/%d", created.ID), nil), "conv-exp"))
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
	assert.Contains(t, brief.SubmitHint, fmt.Sprintf("export submit %d", created.ID))

	// 4. Agent uploads the artifact (raw body + filename query param).
	artifact := []byte("# Auth research\n\nKey findings…\n")
	subReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v1/export-jobs/%d/artifact?name=auth-research.md", created.ID),
		bytes.NewReader(artifact))
	subReq.Header.Set("Content-Type", "text/markdown")
	subRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(subReq, "conv-exp"))
	require.Equal(t, http.StatusOK, subRec.Code, subRec.Body.String())

	// 4b. A duplicate submit is refused — the delivered artifact is not clobbered.
	dupReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v1/export-jobs/%d/artifact?name=other.md", created.ID),
		bytes.NewReader([]byte("different")))
	dupReq.Header.Set("Content-Type", "text/markdown")
	dupRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(dupReq, "conv-exp"))
	assert.Equal(t, http.StatusConflict, dupRec.Code, dupRec.Body.String())

	// 5. Dashboard poll sees it ready.
	pollRec := testharness.Serve(dash, dashReq(t, http.MethodGet,
		fmt.Sprintf("/api/export-jobs/%d", created.ID), nil))
	require.Equal(t, http.StatusOK, pollRec.Code)
	var view struct {
		Status       string `json:"status"`
		Ready        bool   `json:"ready"`
		ArtifactName string `json:"artifact_name"`
		ArtifactSize int64  `json:"artifact_size"`
	}
	testharness.DecodeJSON(t, pollRec, &view)
	assert.Equal(t, "ready", view.Status)
	assert.True(t, view.Ready)
	assert.Equal(t, "auth-research.md", view.ArtifactName)
	assert.Equal(t, int64(len(artifact)), view.ArtifactSize)

	// 6. Download returns the bytes with an attachment disposition.
	dlRec := testharness.Serve(dash, dashReq(t, http.MethodGet,
		fmt.Sprintf("/api/export-jobs/%d/artifact", created.ID), nil))
	require.Equal(t, http.StatusOK, dlRec.Code)
	assert.Equal(t, artifact, dlRec.Body.Bytes())
	assert.Contains(t, dlRec.Header().Get("Content-Disposition"), "auth-research.md")
}

// TestExportFlow_History drives the "Previous exports" panel surfaces: list,
// per-entry delete, and clear-all, across two completed exports for one agent.
func TestExportFlow_History(t *testing.T) {
	f := newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()

	f.HaveGroup("g")
	f.HaveAliveSession("conv-h", "hagent", "tmux-h-1", "/tmp/h")
	f.HaveMember("g", "conv-h")

	mkExport := func(name string) int64 {
		rec := testharness.Serve(dash, dashReq(t, http.MethodPost,
			"/api/agents/conv-h/export", map[string]any{"preset": "summary"}))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var created struct {
			ID int64 `json:"id"`
		}
		testharness.DecodeJSON(t, rec, &created)
		subReq := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/v1/export-jobs/%d/artifact?name=%s", created.ID, name),
			bytes.NewReader([]byte("data:"+name)))
		subReq.Header.Set("Content-Type", "text/markdown")
		require.Equal(t, http.StatusOK,
			testharness.Serve(f.Mux, agentd.AsAgentPeer(subReq, "conv-h")).Code)
		return created.ID
	}
	id1 := mkExport("one.md")
	id2 := mkExport("two.md")

	// History lists both, newest first.
	listExports := func() []map[string]any {
		rec := testharness.Serve(dash, dashReq(t, http.MethodGet, "/api/agents/conv-h/exports", nil))
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
	clrRec := testharness.Serve(dash, dashReq(t, http.MethodDelete, "/api/agents/conv-h/exports", nil))
	require.Equal(t, http.StatusOK, clrRec.Code, clrRec.Body.String())
	assert.Empty(t, listExports())
}

// TestExportFlow_OfflineAgentFastFails proves an export of an agent with no live
// session is refused up front (409) rather than queuing a job nobody services.
func TestExportFlow_OfflineAgentFastFails(t *testing.T) {
	f := newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()

	f.HaveGroup("g")
	f.HaveAliveSession("conv-off", "offagent", "tmux-off-1", "/tmp/off")
	f.HaveMember("g", "conv-off")
	f.MarkOffline("tmux-off-1")

	rec := testharness.Serve(dash, dashReq(t, http.MethodPost,
		"/api/agents/conv-off/export", map[string]any{"preset": "summary"}))
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, strings.ToLower(rec.Body.String()), "no running session")
}

// TestExportFlow_CrossAgentSubmitDenied proves one agent cannot upload into
// another agent's export job — the self-scoped ownership gate.
func TestExportFlow_CrossAgentSubmitDenied(t *testing.T) {
	f := newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()

	f.HaveGroup("g")
	f.HaveAliveSession("conv-owner", "owner", "tmux-own-1", "/tmp/own")
	f.HaveMember("g", "conv-owner")

	rec := testharness.Serve(dash, dashReq(t, http.MethodPost,
		"/api/agents/conv-owner/export", map[string]any{"preset": "summary"}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created struct {
		ID int64 `json:"id"`
	}
	testharness.DecodeJSON(t, rec, &created)

	// A different agent tries to submit into the owner's job.
	subReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v1/export-jobs/%d/artifact?name=x.md", created.ID),
		bytes.NewReader([]byte("nope")))
	subReq.Header.Set("Content-Type", "text/markdown")
	subRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(subReq, "conv-intruder"))
	assert.Equal(t, http.StatusForbidden, subRec.Code, subRec.Body.String())
}
