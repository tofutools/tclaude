package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// permMutate POSTs to a /v1/permissions/{grant,deny,revoke} endpoint as
// the human (who bypasses the permissions.* slug gate) and asserts 200.
func permMutate(t *testing.T, f *testharness.Flow, verb, target, slug string) {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/permissions/"+verb, map[string]any{"target": target, "slug": slug}))
	rec := testharness.Serve(f.Mux, r)
	require.Equalf(t, http.StatusOK, rec.Code,
		"POST /v1/permissions/%s target=%s slug=%s body=%s", verb, target, slug, rec.Body.String())
}

// agentCreatesGroup hits POST /v1/groups as convID — an endpoint gated
// on the groups.create slug — and returns the HTTP status. It is the
// real requirePermission surface this feature must move: 200 means the
// caller resolved to permAllow, 403 means deny / no source.
func agentCreatesGroup(t *testing.T, f *testharness.Flow, convID, groupName string) int {
	t.Helper()
	r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/groups", map[string]any{"name": groupName}), convID)
	return testharness.Serve(f.Mux, r).Code
}

// Scenario: a slug lives in the global default-permissions list, so
// every agent holds it. The human writes a per-conv DENY override for
// one agent through /v1/permissions/deny. That agent must now be
// refused at requirePermission (the deny overrides the default), while
// a sibling agent with no override still passes. Clearing the override
// via /v1/permissions/revoke returns the denied agent to the default.
//
// Pins the core promise of the permanent-permission editor: deny is a
// real subtractive override, authoritative over config defaults.
func TestPermEditorFlow_DenyOverridesDefault(t *testing.T) {
	f := newFlow(t)

	const convA = "perm-aaaa-bbbb-cccc-0001"
	const convB = "perm-bbbb-cccc-dddd-0002"
	f.HaveConvWithTitle(convA, "agent-a")
	f.HaveConvWithTitle(convB, "agent-b")

	// groups.create becomes a global default — both agents inherit it.
	permMutate(t, f, "grant", "default", "groups.create")
	assert.Equal(t, http.StatusCreated, agentCreatesGroup(t, f, convA, "grp-a1"),
		"agent A holds groups.create via the default")

	// Deny it for A only.
	permMutate(t, f, "deny", convA, "groups.create")
	assert.Equal(t, http.StatusForbidden, agentCreatesGroup(t, f, convA, "grp-a2"),
		"agent A's deny override must beat the default")
	assert.Equal(t, http.StatusCreated, agentCreatesGroup(t, f, convB, "grp-b1"),
		"agent B has no override — still holds the default")

	// Clearing the override returns A to the inherited default.
	permMutate(t, f, "revoke", convA, "groups.create")
	assert.Equal(t, http.StatusCreated, agentCreatesGroup(t, f, convA, "grp-a3"),
		"after revoke, agent A is back to the default grant")
}

// Scenario: no global default for the slug. The human writes a per-conv
// GRANT override; the agent gains the slug. Revoking it removes the
// slug again. The grant/revoke half of the editor, end to end through
// requirePermission.
func TestPermEditorFlow_GrantOverrideThenRevoke(t *testing.T) {
	f := newFlow(t)

	const convC = "perm-cccc-dddd-eeee-0003"
	f.HaveConvWithTitle(convC, "agent-c")

	assert.Equal(t, http.StatusForbidden, agentCreatesGroup(t, f, convC, "grp-c1"),
		"no default, no override — agent C is refused")

	permMutate(t, f, "grant", convC, "groups.create")
	assert.Equal(t, http.StatusCreated, agentCreatesGroup(t, f, convC, "grp-c2"),
		"per-conv grant override lets agent C through")

	permMutate(t, f, "revoke", convC, "groups.create")
	assert.Equal(t, http.StatusForbidden, agentCreatesGroup(t, f, convC, "grp-c3"),
		"after revoke, agent C loses the slug again")

	// deny refuses the "default" sentinel — it is a per-conv override.
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/permissions/deny", map[string]any{"target": "default", "slug": "groups.create"}))
	rec := testharness.Serve(f.Mux, r)
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"deny on the default sentinel must be rejected; body=%s", rec.Body.String())
}

// Scenario: the dashboard's permanent-permission editor POSTs a batch
// of tri-state overrides to /api/permissions in one round-trip. The
// next snapshot must reflect them — both the raw permissions.overrides
// map the modal pre-populates from, and the deny-aware effective list.
// Setting every slug back to "default" clears the rows.
//
// Pins the dashboard read/write loop: cookie-auth POST → DB → snapshot.
func TestPermEditorFlow_DashboardBatchEndpoint(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)
	const convD = "perm-dddd-eeee-ffff-0004"
	f.HaveConvWithTitle(convD, "agent-d")
	f.HaveEnrolledAgent(convD)

	mux := agentd.BuildDashboardHandlerForTest()

	postPerms := func(overrides map[string]string) map[string]any {
		body, err := json.Marshal(map[string]any{"conv": convD, "overrides": overrides})
		require.NoError(t, err, "marshal body")
		r, err := http.NewRequest(http.MethodPost, "/api/permissions", strings.NewReader(string(body)))
		require.NoError(t, err, "build request")
		r.Header.Set("Content-Type", "application/json")
		rec := testharness.Serve(mux, r)
		require.Equalf(t, http.StatusOK, rec.Code, "POST /api/permissions body=%s", rec.Body.String())
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode response")
		return resp
	}

	// Grant one slug, deny another, in one batch.
	resp := postPerms(map[string]string{"groups.spawn": "grant", "self.rename": "deny"})
	assert.EqualValues(t, 2, resp["changed"], "two overrides written")

	snap := fetchPermSnapshot(t, mux)
	assert.Equal(t, map[string]string{"groups.spawn": "grant", "self.rename": "deny"},
		snap.Permissions.Overrides[convD], "snapshot must carry the raw tri-state overrides")
	eff := effectiveFor(snap, convD)
	require.NotNil(t, eff, "agent D missing from snapshot agents[]")
	assert.Contains(t, eff, "groups.spawn", "granted slug must appear in effective")
	assert.NotContains(t, eff, "self.rename", "denied slug must not appear in effective")

	// Re-running with default clears both rows.
	resp = postPerms(map[string]string{"groups.spawn": "default", "self.rename": "default"})
	assert.EqualValues(t, 2, resp["changed"], "two overrides cleared")

	snap = fetchPermSnapshot(t, mux)
	assert.Empty(t, snap.Permissions.Overrides[convD],
		"cleared overrides must be gone from the snapshot")
}

// permSnapshot mirrors the slice of /api/snapshot the permanent-
// permission editor reads.
type permSnapshot struct {
	Agents []struct {
		ConvID    string   `json:"conv_id"`
		Effective []string `json:"effective"`
	} `json:"agents"`
	Permissions struct {
		Defaults  []string                     `json:"defaults"`
		Overrides map[string]map[string]string `json:"overrides"`
	} `json:"permissions"`
}

func fetchPermSnapshot(t *testing.T, mux http.Handler) permSnapshot {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, rec.Code, "/api/snapshot body=%s", rec.Body.String())
	var snap permSnapshot
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap), "decode snapshot")
	return snap
}

func effectiveFor(snap permSnapshot, convID string) []string {
	for _, a := range snap.Agents {
		if a.ConvID == convID {
			return a.Effective
		}
	}
	return nil
}
