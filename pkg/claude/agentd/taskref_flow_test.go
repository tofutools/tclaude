package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// taskRefResp mirrors the daemon's task-endpoint wire shape
// (/v1/whoami/task and /v1/agent/{sel}/task).
type taskRefResp struct {
	ConvID        string `json:"conv_id"`
	TaskURL       string `json:"task_ref_url"`
	TaskLabel     string `json:"task_ref_label"`
	Cleared       bool   `json:"cleared"`
	CallerConv    string `json:"caller_conv"`
	CallerAgentID string `json:"caller_agent_id"`
}

// Scenario: spawning an agent with a task-reference link persists it on the new
// actor (per-agent, not per-membership) and the dashboard snapshot renders it
// with a derived label (Linear → issue id) alongside the raw URL.
func TestTaskRef_SpawnPersistsAndRendersDerivedLabel(t *testing.T) {
	f := newFlow(t)
	// The dashboard snapshot fetch below rides the popup mux, whose CSRF
	// guard needs an Origin — the test handler only injects one when a
	// popup base URL is set.
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f.HaveGroup("alpha")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":         "worker",
		"cwd":          t.TempDir(),
		"task_ref_url": "https://linear.app/acme/issue/JOH-321/wire-task-links",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	require.NotEmpty(t, spawn.ConvID)

	agentID, err := db.AgentIDForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, agentID, "spawn mints an actor")

	// Persisted on the agent row.
	ref, err := db.GetAgentTaskRef(agentID)
	require.NoError(t, err)
	assert.Equal(t, "https://linear.app/acme/issue/JOH-321/wire-task-links", ref.URL)
	assert.Equal(t, "", ref.Label, "no explicit label was supplied")

	// Rendered on the dashboard snapshot with the derived label.
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	m := findDashMember(snap, "alpha", spawn.ConvID)
	require.NotNil(t, m, "spawned worker missing from alpha members")
	assert.Equal(t, "https://linear.app/acme/issue/JOH-321/wire-task-links", m.TaskURL)
	assert.Equal(t, "JOH-321", m.TaskLabel, "Linear issue id is derived server-side")
}

// Scenario: a spawn carrying a non-http(s) task URL is rejected at the spawn
// boundary with a 400 — the same scheme guard the standalone task endpoints
// apply, keeping a javascript:/data: URL out of the dashboard href.
func TestTaskRef_SpawnRejectsNonHTTPURL(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":         "worker",
		"cwd":          t.TempDir(),
		"task_ref_url": "javascript:alert(1)",
	})
	assert.Equal(t, http.StatusBadRequest, spawn.Code, "bad task URL is a 400; body=%s", spawn.Raw)
}

// Scenario: an agent sets, reads, and clears its OWN task link via
// /v1/whoami/task (self.task). The label is derived (GitHub → #number); a clear
// removes both the URL and any label.
func TestTaskRef_SelfSetGetClear(t *testing.T) {
	f := newFlow(t)
	const worker = "wtsk-aaaa-bbbb-cccc-dddd"

	f.HaveGroup("alpha")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-wtsk", "tmux-wtsk", f.TestCwd("wtsk"))
	f.HaveMember("alpha", worker)
	require.NoError(t,
		db.SetAgentPermissionOverride(worker, agentd.PermSelfTask, db.PermEffectGrant, "test"),
		"grant self.task")

	// Set.
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/task",
			map[string]any{"url": "https://github.com/tofutools/tclaude/issues/9"}), worker))
	require.Equalf(t, http.StatusOK, rec.Code, "self set: body=%s", rec.Body.String())
	var set taskRefResp
	testharness.DecodeJSON(t, rec, &set)
	assert.Equal(t, "#9", set.TaskLabel, "GitHub issue number is derived")
	assert.False(t, set.Cleared)
	assert.Empty(t, set.CallerConv, "self write carries no caller_conv")

	// Get.
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/whoami/task", nil), worker))
	require.Equalf(t, http.StatusOK, rec.Code, "self get: body=%s", rec.Body.String())
	var got taskRefResp
	testharness.DecodeJSON(t, rec, &got)
	assert.Equal(t, "https://github.com/tofutools/tclaude/issues/9", got.TaskURL)
	assert.Equal(t, "#9", got.TaskLabel)

	// Clear.
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/task",
			map[string]any{"clear": true}), worker))
	require.Equalf(t, http.StatusOK, rec.Code, "self clear: body=%s", rec.Body.String())
	var cleared taskRefResp
	testharness.DecodeJSON(t, rec, &cleared)
	assert.True(t, cleared.Cleared)
	assert.Empty(t, cleared.TaskURL)

	agentID, err := db.AgentIDForConv(worker)
	require.NoError(t, err)
	ref, err := db.GetAgentTaskRef(agentID)
	require.NoError(t, err)
	assert.Equal(t, db.AgentTaskRef{}, ref, "clear wipes the row")
}

// Scenario: a lead that OWNS the group sets a worker's task link via the
// manager-pattern --target route — no agent.task slug needed; group ownership
// is the structural bypass (mirrors the cross-agent rename/context verbs).
func TestTaskRef_OwnerSetsWorkerWithoutSlug(t *testing.T) {
	f := newFlow(t)
	const lead = "ltsk-aaaa-bbbb-cccc-dddd"
	const worker = "wts2-aaaa-bbbb-cccc-dddd"

	g := f.HaveGroup("squad")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-wts2", "tmux-wts2", f.TestCwd("wts2"))
	f.HaveMember("squad", worker)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, lead, "test"), "seed owner")

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/agent/"+worker+"/task",
			map[string]any{"url": "https://linear.app/acme/issue/AB-2/x"}), lead))
	require.Equalf(t, http.StatusOK, rec.Code, "owner set: body=%s", rec.Body.String())
	var resp taskRefResp
	testharness.DecodeJSON(t, rec, &resp)
	assert.Equal(t, "AB-2", resp.TaskLabel)
	assert.Equal(t, lead, resp.CallerConv, "cross-agent write echoes caller_conv")

	agentID, err := db.AgentIDForConv(worker)
	require.NoError(t, err)
	ref, err := db.GetAgentTaskRef(agentID)
	require.NoError(t, err)
	assert.Equal(t, "https://linear.app/acme/issue/AB-2/x", ref.URL)
}

// Scenario: an agent that neither holds agent.task nor owns a group containing
// the target is refused (403) on a cross-agent set.
func TestTaskRef_CrossAgentDeniedWithoutOwnershipOrSlug(t *testing.T) {
	f := newFlow(t)
	const stranger = "stsk-aaaa-bbbb-cccc-dddd"
	const worker = "wts3-aaaa-bbbb-cccc-dddd"

	f.HaveGroup("squad")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-wts3", "tmux-wts3", f.TestCwd("wts3"))
	f.HaveMember("squad", worker)
	f.HaveEnrolledAgent(stranger) // a known agent, but no ownership / slug

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/agent/"+worker+"/task",
			map[string]any{"url": "https://linear.app/acme/issue/AB-3/x"}), stranger))
	assert.Equal(t, http.StatusForbidden, rec.Code, "no slug + not owner ⇒ 403; body=%s", rec.Body.String())
}
