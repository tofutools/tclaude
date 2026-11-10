package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type scopeValueItem struct {
	Kind   string   `json:"kind"`
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Groups []string `json:"groups"`
}

// Scenario: the operator opens the resource= picker in the permission editor.
// The editor loads the resource catalogue once, from its own endpoint rather
// than the 2s snapshot poll: every drawing board and process template, with its
// title and the groups it belongs to, in the kind:id shape a resource= scope
// names.
func TestPermissionScopeValues_ResourceCatalogue(t *testing.T) {
	e := newUmbrellaEnv(t)
	rec := e.operator(http.MethodGet, "/api/permissions/scope-values?dim=resource", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Items []scopeValueItem `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	got := map[string]scopeValueItem{}
	for _, item := range out.Items {
		got[item.Kind+":"+item.ID] = item
	}
	assert.Equal(t, scopeValueItem{Kind: "drawing_board", ID: "plan", Title: "plan", Groups: []string{"crew"}}, got["drawing_board:plan"])
	assert.Equal(t, scopeValueItem{Kind: "drawing_board", ID: "loose", Title: "loose"}, got["drawing_board:loose"])
	assert.Equal(t, []string{"hidden"}, got["drawing_board:secret"].Groups)
	assert.Equal(t, scopeValueItem{Kind: "process_template", ID: "shared", Title: "shared", Groups: []string{"crew", "hidden"}},
		got["process_template:shared"])
	assert.Equal(t, scopeValueItem{Kind: "process_template", ID: "solo", Title: "solo"}, got["process_template:solo"])
	assert.Len(t, out.Items, 6)
}

// The catalogue names every resource and the groups it is linked to, which is
// the operator's to read: an agent cannot reach it, and an unauthenticated
// dashboard request is refused.
func TestPermissionScopeValues_IsTheOperatorsOnly(t *testing.T) {
	e := newUmbrellaEnv(t)
	rec := e.agent(http.MethodGet, "/api/permissions/scope-values?dim=resource", nil)
	assert.NotEqual(t, http.StatusOK, rec.Code, "the agent API does not serve the catalogue")
	unauthenticated := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
		testharness.JSONRequest(t, http.MethodGet, "/api/permissions/scope-values?dim=resource", nil))
	assert.NotEqual(t, http.StatusOK, unauthenticated.Code, "a request without the dashboard's origin is refused")

	unknown := e.operator(http.MethodGet, "/api/permissions/scope-values?dim=group", nil)
	assert.Equal(t, http.StatusBadRequest, unknown.Code, "only a dimension whose catalogue is off the snapshot is served")
}
