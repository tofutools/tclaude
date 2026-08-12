package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// askHumanSpawnAttempt is spawnAttempt with the popup escape hatch armed, so
// the request reaches the human-approval path instead of a bare 403.
func askHumanSpawnAttempt(t *testing.T, f *testharness.Flow, caller, group, name string) *httptest.ResponseRecorder {
	t.Helper()
	r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/groups/"+group+"/spawn", map[string]any{"name": name}), caller)
	r.Header.Set("X-Tclaude-Ask-Human", "30s")
	return testharness.Serve(f.Mux, r)
}

// Scenario (the headline of the scoped always-allow): a lead belongs to two
// groups and holds no spawn grant. It spawns into "alpha" with --ask-human,
// and the human picks the NARROW button — "Always allow — group=alpha"
// — rather than the blanket one.
//
// What must then be true, and is asserted at the wire surface:
//   - the one-off spawn goes through;
//   - the persisted override carries EXACTLY the group the gate evaluated,
//     and nothing else — not the slug's other declared dimension, not the
//     second group, not an unscoped grant;
//   - the next spawn into alpha passes with no popup available at all;
//   - a spawn into beta is still refused, which is the entire difference
//     between this button and the blanket one.
func TestScopedAlwaysAllow_PersistsOnlyTheApprovedScope(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.SetAutoGrantableForTest(agentd.PermGroupsMembersSpawn))
	t.Cleanup(agentd.StubScopedAlwaysAllowApprovalForTest())

	f := newFlow(t)
	f.HaveGroup("alpha")
	f.HaveGroup("beta")

	// A plain member of both groups: no ownership, so only a grant can admit
	// it (an owner bypass would mask exactly what this test is about).
	const lead = "scopealw-aaaa-bbbb-cccc-000000000001"
	haveSpawnCapableMember(t, f, "alpha", lead)
	f.HaveMember("beta", lead)

	approved := askHumanSpawnAttempt(t, f, lead, "alpha", "in-scope-worker")
	require.Equal(t, http.StatusOK, approved.Code,
		"the approved spawn itself must go through; body=%s", approved.Body.String())

	rows, err := db.ListAgentPermissionOverrideRowsForConv(lead)
	require.NoError(t, err)
	var stored *db.AgentPermission
	for i := range rows {
		if rows[i].Slug == agentd.PermGroupsMembersSpawn {
			stored = &rows[i]
		}
	}
	require.NotNil(t, stored, "the scoped always-allow must persist an override")
	assert.Equal(t, db.PermEffectGrant, stored.Effect)
	// The exact-value guard: groups.members.spawn also declares spawn_profile, and the
	// gate described no profile. A scope naming one — or naming no dimension
	// at all — would hand over more than the human approved.
	assert.JSONEq(t, `{"group":["alpha"]}`, stored.ScopeJSON,
		"the persisted scope must be exactly the evaluated ActionContext, never a superset")
	assert.Equal(t, "human:popup-always-scoped", stored.GrantedBy,
		"provenance must distinguish the narrowed click from the blanket one")

	// No --ask-human header from here on: nothing but the persisted grant can
	// decide these, so the popup cannot rescue (or falsify) either result.
	again := spawnAttempt(t, f, lead, "alpha", "second-in-scope-worker")
	assert.Equal(t, http.StatusOK, again.Code,
		"the persisted scoped grant must stop the popup for the approved scope; body=%s", again.Body)

	other := spawnAttempt(t, f, lead, "beta", "out-of-scope-worker")
	assert.Equal(t, http.StatusForbidden, other.Code,
		"a scoped always-allow must NOT widen to another group; body=%s", other.Body)
}

// Scenario: the human narrows twice. Approving group beta after group alpha
// must ADD to the grant, not replace it — an "always allow" click that
// silently revoked an earlier allowance would be the opposite of what the
// button says.
func TestScopedAlwaysAllow_SecondApprovalUnionsWithTheFirst(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.SetAutoGrantableForTest(agentd.PermGroupsMembersSpawn))
	t.Cleanup(agentd.StubScopedAlwaysAllowApprovalForTest())

	f := newFlow(t)
	f.HaveGroup("alpha")
	f.HaveGroup("beta")

	const lead = "scopealw-aaaa-bbbb-cccc-000000000002"
	haveSpawnCapableMember(t, f, "alpha", lead)
	haveSpawnCapableMember(t, f, "beta", lead)

	require.Equal(t, http.StatusOK,
		askHumanSpawnAttempt(t, f, lead, "alpha", "alpha-worker").Code)
	require.Equal(t, http.StatusOK,
		askHumanSpawnAttempt(t, f, lead, "beta", "beta-worker").Code)

	rows, err := db.ListAgentPermissionOverrideRowsForConv(lead)
	require.NoError(t, err)
	for _, row := range rows {
		if row.Slug != agentd.PermGroupsMembersSpawn {
			continue
		}
		assert.JSONEq(t, `{"group":["alpha","beta"]}`, row.ScopeJSON,
			"the second approval must union with the first, not replace it")
	}

	// Both approved groups now pass without a popup; a third does not exist
	// to widen into, so the union is bounded by what the human actually saw.
	assert.Equal(t, http.StatusOK, spawnAttempt(t, f, lead, "alpha", "alpha-again").Code)
	assert.Equal(t, http.StatusOK, spawnAttempt(t, f, lead, "beta", "beta-again").Code)
}

// Scenario: an agent whose gate site describes nothing (the ~129 that pass no
// ActionContext) gets the blanket button and nothing else. The dashboard's
// decision endpoint is the surface a hand-crafted POST would attack, so the
// refusal is asserted there rather than on the hidden button.
func TestScopedAlwaysAllow_RefusedWhenTheGateDescribedNothing(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	// A slug with no scope dimensions at all: there is nothing this request
	// could be narrowed to, so the scoped persist has nothing to write.
	const conv = "scopealw-aaaa-bbbb-cccc-000000000003"
	const id = "alw-scoped-none-0001"
	f.HaveConvWithTitle(conv, "worker")
	_, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	t.Cleanup(agentd.SeedApprovalForTest(id, agentd.PermHumanClipboard, true))

	dash := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+id+"/decision", map[string]any{"decision": "always_scoped"}))
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"a request with no action scope cannot be scope-granted; body=%s", rec.Body.String())

	_, ok, err := db.AgentPermissionOverride(conv, agentd.PermHumanClipboard)
	require.NoError(t, err)
	assert.False(t, ok, "the refused decision must write no override at all")
}
