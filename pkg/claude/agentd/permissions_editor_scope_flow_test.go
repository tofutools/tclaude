package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// scopeSnapshotView reads the parts of the snapshot the editor's scope
// controls are driven by: what each grant is narrowed to, and what the pickers
// may offer.
type scopeSnapshotView struct {
	Permissions struct {
		Overrides  map[string]map[string]string              `json:"overrides"`
		Scopes     map[string]map[string]map[string][]string `json:"scopes"`
		Unreadable map[string][]string                       `json:"unreadable_scopes"`
		DimOpts    map[string]struct {
			Values    []string `json:"values"`
			Selectors []string `json:"selectors"`
		} `json:"scope_dim_options"`
	} `json:"permissions"`
}

func postScopedPerms(t *testing.T, mux http.Handler, body map[string]any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	r, err := http.NewRequest(http.MethodPost, "/api/permissions", strings.NewReader(string(raw)))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	return rec.Code, rec.Body.String()
}

func fetchScopeView(t *testing.T, mux http.Handler) scopeSnapshotView {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, rec.Code, "/api/snapshot body=%s", rec.Body.String())
	var view scopeSnapshotView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	return view
}

// Scenario: the operator narrows a grant in the permission editor and the
// narrowing survives the round trip the editor actually makes — POST the
// batch, read it back off the next snapshot poll. Then they clear the scope,
// and the grant goes back to unconditional.
//
// This is the read/write loop the chips render from; the dimension pickers are
// covered at the component level (jstest/permission-scope-editor.test.mjs).
func TestPermEditorScope_RoundTripsThroughSnapshot(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("alpha")
	const conv = "permscope-dddd-eeee-ffff-0001"
	f.HaveConvWithTitle(conv, "agent-scope")
	f.HaveEnrolledAgent(conv)
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "locked-sandbox"})
	require.NoError(t, err)
	writeLinearConfig(t, []string{"TCL", "JOH"})

	mux := agentd.BuildDashboardHandlerForTest()

	code, body := postScopedPerms(t, mux, map[string]any{
		"conv":      conv,
		"overrides": map[string]string{agentd.PermGroupsMembersSpawn: "grant"},
		"scopes":    map[string]any{agentd.PermGroupsMembersSpawn: map[string][]string{"group": {"alpha"}}},
	})
	require.Equal(t, http.StatusOK, code, body)

	view := fetchScopeView(t, mux)
	assert.Equal(t, "grant", view.Permissions.Overrides[conv][agentd.PermGroupsMembersSpawn])
	assert.Equal(t, map[string][]string{"group": {"alpha"}},
		view.Permissions.Scopes[conv][agentd.PermGroupsMembersSpawn],
		"the editor must be able to render what the grant was narrowed to")
	assert.Contains(t, view.Permissions.DimOpts["group"].Values, "alpha",
		"the group picker offers the live groups")
	assert.Contains(t, view.Permissions.DimOpts["sandbox_profile"].Values, "locked-sandbox",
		"the sandbox-profile picker offers operator-authored profiles")
	assert.Contains(t, view.Permissions.DimOpts["target_agent"].Selectors, "@descendants",
		"a dimension's relational selectors are advertised, not hardcoded in the frontend")
	// linear_team's catalogue is the operator's own agent.linear_proxy.allowed_teams:
	// a team they have not allow-listed cannot be reached by any grant, so
	// offering it would only invite a scope that authorizes nothing. Rendered
	// upper-case, the way Linear spells a key and an operator reads one.
	assert.Equal(t, []string{"JOH", "TCL"}, view.Permissions.DimOpts["linear_team"].Values,
		"the Linear team picker offers the operator's allow-listed teams")

	// Clearing the scope is the same POST with an EXPLICIT empty scope — the
	// editor's "remove the last chip" path — and must widen the grant back, not
	// leave a stale narrowing behind. (Omitting the slug means "keep whatever is
	// stored", so that an unrelated save cannot strip a narrowing; the case
	// below pins that.)
	code, body = postScopedPerms(t, mux, map[string]any{
		"conv":      conv,
		"overrides": map[string]string{agentd.PermGroupsMembersSpawn: "grant"},
		"scopes":    map[string]any{agentd.PermGroupsMembersSpawn: map[string][]string{}},
	})
	require.Equal(t, http.StatusOK, code, body)
	view = fetchScopeView(t, mux)
	assert.Empty(t, view.Permissions.Scopes[conv][agentd.PermGroupsMembersSpawn],
		"an unscoped grant carries no scope at all")
	assert.Equal(t, "grant", view.Permissions.Overrides[conv][agentd.PermGroupsMembersSpawn])

	// And the mirror: a save that does not mention the slug in scopes{} leaves
	// the stored narrowing alone. The editor posts every displayed slug at its
	// current effect, so the other reading would strip a scope off every save.
	code, body = postScopedPerms(t, mux, map[string]any{
		"conv":      conv,
		"overrides": map[string]string{agentd.PermGroupsMembersSpawn: "grant"},
		"scopes":    map[string]any{agentd.PermGroupsMembersSpawn: map[string][]string{"group": {"alpha"}}},
	})
	require.Equal(t, http.StatusOK, code, body)
	code, body = postScopedPerms(t, mux, map[string]any{
		"conv":      conv,
		"overrides": map[string]string{agentd.PermGroupsMembersSpawn: "grant"},
	})
	require.Equal(t, http.StatusOK, code, body)
	view = fetchScopeView(t, mux)
	assert.Equal(t, map[string][]string{"group": {"alpha"}},
		view.Permissions.Scopes[conv][agentd.PermGroupsMembersSpawn],
		"a slug the batch does not mention keeps its stored scope")
}

// Scenario: an agent holds a grant whose stored scope this build cannot
// decode — the shape a downgrade or a parallel-phase daemon leaves behind.
// Such a row authorizes NOTHING at the gate, so the editor must never rewrite
// it into a blanket grant: the snapshot reports it as unreadable rather than
// as unscoped, and a save that would overwrite it is refused.
func TestPermEditorScope_RefusesToWidenAnUnreadableScope(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	const conv = "permscope-dddd-eeee-ffff-0003"
	f.HaveConvWithTitle(conv, "agent-scope")
	f.HaveEnrolledAgent(conv)
	// Written past the HTTP validators, exactly as a newer daemon would have.
	require.NoError(t, db.SetAgentPermissionOverrideWithScope(
		conv, agentd.PermGroupsMembersSpawn, db.PermEffectGrant, `{"dimension_from_the_future":["x"]}`, "test"))

	mux := agentd.BuildDashboardHandlerForTest()
	view := fetchScopeView(t, mux)
	assert.Empty(t, view.Permissions.Scopes[conv][agentd.PermGroupsMembersSpawn],
		"an undecodable scope must not be reported as a readable one")
	assert.Contains(t, view.Permissions.Unreadable[conv], agentd.PermGroupsMembersSpawn,
		"it must be reported as unreadable, so the editor cannot render it as unscoped")

	code, body := postScopedPerms(t, mux, map[string]any{
		"conv":      conv,
		"overrides": map[string]string{agentd.PermGroupsMembersSpawn: "grant"},
	})
	assert.Equal(t, http.StatusConflict, code, body)

	rows, err := db.ListAgentPermissionOverrideRowsForConv(conv)
	require.NoError(t, err)
	for _, row := range rows {
		if row.Slug == agentd.PermGroupsMembersSpawn {
			assert.Equal(t, `{"dimension_from_the_future":["x"]}`, row.ScopeJSON,
				"the refused save must leave the narrow (if unreadable) grant exactly as it was")
		}
	}

	// Removing the row entirely is still allowed: it only ever takes authority
	// away, so the operator is never stuck with a grant they cannot clear.
	code, body = postScopedPerms(t, mux, map[string]any{
		"conv":      conv,
		"overrides": map[string]string{agentd.PermGroupsMembersSpawn: "default"},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Empty(t, fetchScopeView(t, mux).Permissions.Overrides[conv][agentd.PermGroupsMembersSpawn])
}

// Scenario: the write path refuses the two ways a scope could mean something
// the gate would not honour — a dimension the slug does not declare, and a
// scope attached to something that is not a grant. Both are 400s rather than
// silent drops, and neither writes anything.
func TestPermEditorScope_RefusesScopesTheGateCouldNotHonour(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	const conv = "permscope-dddd-eeee-ffff-0002"
	f.HaveConvWithTitle(conv, "agent-scope")
	f.HaveEnrolledAgent(conv)
	mux := agentd.BuildDashboardHandlerForTest()

	code, body := postScopedPerms(t, mux, map[string]any{
		"conv":      conv,
		"overrides": map[string]string{agentd.PermGroupsMembersSpawn: "grant"},
		"scopes":    map[string]any{agentd.PermGroupsMembersSpawn: map[string][]string{"process_template": {"t1"}}},
	})
	assert.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, body, "scope dimension")

	code, body = postScopedPerms(t, mux, map[string]any{
		"conv":      conv,
		"overrides": map[string]string{agentd.PermGroupsMembersSpawn: "deny"},
		"scopes":    map[string]any{agentd.PermGroupsMembersSpawn: map[string][]string{"group": {"alpha"}}},
	})
	assert.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, body, "only a grant can be scoped")

	view := fetchScopeView(t, mux)
	assert.Empty(t, view.Permissions.Overrides[conv],
		"a rejected batch must not have written any part of itself")
}
