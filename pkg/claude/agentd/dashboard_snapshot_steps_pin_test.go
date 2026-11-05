package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// handleDashboardSnapshot is being broken into named steps (load, collect,
// assemble). Its perf phases are the request's observable shape: each step
// owns a span, and the Debug tab reads them by name and order. The top-level
// order and the payload/groups/codex children are pinned by
// TestDashboardPerf_RecordsPollTimings; these pin the two child lists that
// the step extraction rearranges — every preload read, and every collector —
// so a step that reorders, drops or renames one fails here.
func TestDashboardSnapshot_PreloadAndCollectorPhasesKeepTheirOrder(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	agentd.ResetPerfForTest()
	t.Cleanup(agentd.ResetPerfForTest)

	const convID = "pin-1111-2222-3333-4444"
	f.HaveConvWithTitle(convID, "pin-worker")
	f.HaveAliveSession(convID, "spwn-pin", "tmux-pin", f.TestCwd("pin"))
	f.HaveGroup("pingrp")
	f.HaveMember("pingrp", convID)

	dash := agentd.BuildDashboardHandlerForTest()
	fetchSnapshotOnly(t, dash)
	snap := perfEndpointNamed(t, fetchPerf(t, dash, "/api/perf"), "/api/snapshot")
	require.Len(t, snap.Samples, 1)

	childNames := func(phase string) []string {
		var names []string
		for _, p := range snap.Samples[0].Phases {
			if p.Name != phase {
				continue
			}
			for _, c := range p.Children {
				names = append(names, c.Name)
			}
			return names
		}
		t.Fatalf("snapshot sample has no %q phase", phase)
		return nil
	}

	assert.Equal(t, []string{
		// First fan-out: the independent registry, permission and config reads.
		"groups", "sandbox_profiles", "global_sandbox", "permission_overrides",
		"permission_scopes", "config", "notify_prefs", "agent_roster", "sudo_grants",
		// Per-group rows, keyed by the group ids the first fan-out found.
		"group_members", "group_owners", "routes", "route_leases", "darwin_route_launches",
		// The conv set, then the row cache's batch loads over it.
		"conv_set", "conv_index", "sessions", "workdirs", "workspaces", "agents",
		"locations", "git_cache",
		// Agent-keyed reads over the cache's visible agents, then the PR cache.
		"task_refs", "presented_prs", "tags", "authored_open_prs", "tracked_pr_authors",
		"pr_state_cache",
		// The row cache's un-batched per-row side reads, reported last.
		"bg_reconcile", "context_snapshot",
	}, childNames("preload"))

	assert.Equal(t, []string{
		"pending", "cron", "export_jobs", "links", "usage", "opencode_usage_activity",
		"templates", "profiles", "default_profile", "roles", "saved_prompts",
		"queued_spawns", "capacity_queue", "messages", "plugins",
	}, childNames("collectors"))
}

// The only refusals /api/snapshot writes happen before the first step: an
// uncookied request and a non-GET. Every later failure is logged and the
// snapshot renders without that part, so these are the response codes the
// step extraction must leave alone.
func TestDashboardSnapshot_EarlyRefusals(t *testing.T) {
	newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	bare := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(bare)
	unauth := httptest.NewRecorder()
	bare.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/snapshot", nil))
	assert.Equal(t, http.StatusForbidden, unauth.Code, "body=%s", unauth.Body.String())

	dash := agentd.BuildDashboardHandlerForTest()
	post := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost, "/api/snapshot", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, post.Code)
	assert.Equal(t, "GET only\n", post.Body.String())
}
