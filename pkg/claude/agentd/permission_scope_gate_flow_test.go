package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// grantScoped issues a scoped per-agent grant through the production
// permissions endpoint, as the human operator would.
func grantScoped(t *testing.T, f *testharness.Flow, target, slug string, scope map[string]any) {
	t.Helper()
	body := map[string]any{"target": target, "slug": slug}
	if scope != nil {
		body["scope"] = scope
	}
	res := postPermissionScope(t, f, "grant", body)
	require.Equal(t, http.StatusOK, res.Code, res.Body)
}

// haveSpawnCapableMember enrolls convID in group as a plain (non-owner)
// member with the maximally-authorized launch posture the spawn path
// requires, and NO permission grant — the grant is what each scenario varies.
func haveSpawnCapableMember(t *testing.T, f *testharness.Flow, group, convID string) {
	t.Helper()
	f.HaveMember(group, convID)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "sess-" + convID, TmuxSession: "tmux-" + convID, ConvID: convID,
		Cwd: f.World.HomeDir, Status: "running", Harness: harness.DefaultName,
		HarnessBuiltinMode: harness.ClaudeSandboxOff, ApprovalPolicy: "bypassPermissions",
	}))
}

func spawnAttempt(t *testing.T, f *testharness.Flow, caller, group, name string) *httpResult {
	t.Helper()
	rec := agentReq(t, f, caller, http.MethodPost, "/v1/groups/"+group+"/spawn",
		map[string]any{"name": name})
	return &httpResult{Code: rec.Code, Body: rec.Body.String()}
}

// Scenario: an operator narrows a lead's spawn capability to one of the two
// groups it belongs to. The lead spawns into the granted group and is refused
// in the other — the whole point of scoped grants, at the wire surface.
//
// The same scenario pins the display-can't-drift invariant: `permissions ls`
// renders the scope the gate enforced, so a reader is never told the lead
// holds groups.spawn without being told where.
func TestPermissionScope_GroupScopedGrantGatesSpawnPerGroup(t *testing.T) {
	f := newFlow(t)
	// The Audit tab's handler requires a dashboard origin; assert on the
	// audit row through the same endpoint the tab reads.
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f.HaveGroup("alpha")
	f.HaveGroup("beta")

	// A plain member of both groups: no ownership, so nothing but the grant
	// can admit it (the owner bypass would mask what we are testing).
	const lead = "scopegate-lead-aaaa-bbbb-cccc-000000000001"
	haveSpawnCapableMember(t, f, "alpha", lead)
	f.HaveMember("beta", lead)
	grantScoped(t, f, lead, agentd.PermGroupsSpawn, map[string]any{"group": []string{"alpha"}})

	allowed := spawnAttempt(t, f, lead, "alpha", "in-scope-worker")
	require.Equal(t, http.StatusOK, allowed.Code, allowed.Body)

	refused := spawnAttempt(t, f, lead, "beta", "out-of-scope-worker")
	assert.Equal(t, http.StatusForbidden, refused.Code, refused.Body)
	assert.Contains(t, refused.Body, agentd.PermGroupsSpawn,
		"refusal must be the ordinary not-granted path, naming the slug")

	// Nothing was spawned into beta.
	for _, m := range f.ListGroupMembers("beta") {
		assert.Equal(t, lead, m.ConvID, "a refused spawn must not enroll anyone")
	}

	// Listing == gate: the same resolver feeds both, so the effective view
	// carries the winning tier's scope.
	view := getPermissionsTarget(t, f, lead, lead)
	require.Equal(t, http.StatusOK, view.Code, view.Body)
	var effective struct {
		Provenance map[string]string `json:"provenance"`
	}
	require.NoError(t, json.Unmarshal([]byte(view.Body), &effective))
	assert.Equal(t, "override [group=alpha]", effective.Provenance[agentd.PermGroupsSpawn])

	// The audit row for the allowed spawn names the scope that admitted it.
	entries := fetchAudit(t, agentd.BuildDashboardHandlerForTest(), "").Entries
	var spawnDetail string
	for _, e := range entries {
		if e.Verb == "spawn" && e.Status == http.StatusOK {
			spawnDetail = e.Detail
		}
	}
	assert.Contains(t, spawnDetail, "scope: "+agentd.PermGroupsSpawn+" [group=alpha]",
		"a scoped authorization must be recorded on the audit row")
}

// A dimension the call site does not describe fails CLOSED. groups.spawn also
// declares spawn_profile, but no production handler passes it until Phase 4 —
// so a profile-scoped grant authorizes nothing yet rather than everything.
func TestPermissionScope_UndescribedDimensionFailsClosed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "scopegate-lead-aaaa-bbbb-cccc-000000000002"
	haveSpawnCapableMember(t, f, "alpha", lead)
	grantScoped(t, f, lead, agentd.PermGroupsSpawn, map[string]any{"spawn_profile": []string{"locked"}})

	refused := spawnAttempt(t, f, lead, "alpha", "profile-scoped-worker")
	assert.Equal(t, http.StatusForbidden, refused.Code, refused.Body)
}

// Regression guard for the ~129 gate sites that pass no ActionContext: an
// UNSCOPED grant must still decide exactly as it did before scopes existed.
func TestPermissionScope_UnscopedGrantIsUnaffected(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f.HaveGroup("alpha")
	const lead = "scopegate-lead-aaaa-bbbb-cccc-000000000003"
	haveSpawnCapableMember(t, f, "alpha", lead)
	grantScoped(t, f, lead, agentd.PermGroupsSpawn, nil)

	allowed := spawnAttempt(t, f, lead, "alpha", "unscoped-worker")
	require.Equal(t, http.StatusOK, allowed.Code, allowed.Body)

	// And no scope noise on the audit row for an unscoped decision.
	for _, e := range fetchAudit(t, agentd.BuildDashboardHandlerForTest(), "").Entries {
		if e.Verb == "spawn" {
			assert.NotContains(t, e.Detail, "scope: ")
		}
	}
}

// An @selector is parseable and persistable since Phase 1, but the spawn
// lineage it ranges over arrives in Phase 5. Until then it must authorize
// nothing — the fail-closed direction, since the alternative turns a
// deliberately narrow grant into a wildcard.
func TestPermissionScope_SelectorFailsClosedUntilLineage(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const caller = "scopegate-caller-aaaa-bbbb-cccc-00000000004"
	const target = "scopegate-target-aaaa-bbbb-cccc-00000000004"
	f.HaveMember("alpha", caller)
	f.HaveMember("alpha", target)

	grantScoped(t, f, caller, agentd.PermAgentRetire,
		map[string]any{"target_agent": []string{"@descendants"}})
	rec := agentReq(t, f, caller, http.MethodPost,
		"/v1/agent/"+target+"/retire?shutdown=0&delete_worktree=0", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	state, err := db.AgentState(target)
	require.NoError(t, err)
	assert.NotEqual(t, db.AgentStateRetired, state, "a refused retire must not demote the target")

	// Control: the same grant without a scope retires the peer, proving the
	// refusal above came from the selector and not from the setup.
	grantScoped(t, f, caller, agentd.PermAgentRetire, nil)
	rec = agentReq(t, f, caller, http.MethodPost,
		"/v1/agent/"+target+"/retire?shutdown=0&delete_worktree=0", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
