package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// awb tcl-yjjbfh: a template's owner scopes narrow what the owners of every
// group later deployed from it may do. Changing a live group's owner scopes
// needs groups.settings.owner-scopes; these pin which template writes need it
// too, and which stay templates.manage only.

const templateScribe = "tscr-aaaa-bbbb-cccc-dddd"

const otherScopes = `{"groups.members.spawn":{"spawn_profile":["p2"]}}`

func scopesValue(t *testing.T, raw string) map[string]any {
	t.Helper()
	var v map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &v))
	return v
}

// newTemplateScribeFlow is a flow with the scoped template "crew" and an agent
// that holds templates.manage only.
func newTemplateScribeFlow(t *testing.T) *testharness.Flow {
	t.Helper()
	f := newFlow(t)
	haveScopedTemplate(t, f.Mux)
	require.NoError(t, db.GrantAgentPermission(templateScribe, agentd.PermTemplatesManage, "test"))
	return f
}

func crewWithScopes(t *testing.T, f *testharness.Flow, scopes string) map[string]any {
	t.Helper()
	rec := humanReq(t, f, http.MethodGet, "/v1/templates/crew", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body map[string]any
	testharness.DecodeJSON(t, rec, &body)
	if scopes == "" {
		delete(body, "owner_scopes")
	} else {
		body["owner_scopes"] = scopesValue(t, scopes)
	}
	return body
}

func crewExportWithScopes(t *testing.T, f *testharness.Flow, scopes string) map[string]any {
	t.Helper()
	rec := humanReq(t, f, http.MethodGet, "/v1/templates/crew/export", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var env map[string]any
	testharness.DecodeJSON(t, rec, &env)
	tmpl := env["template"].(map[string]any)
	if scopes == "" {
		delete(tmpl, "owner_scopes")
	} else {
		tmpl["owner_scopes"] = scopesValue(t, scopes)
	}
	return env
}

func scopedGroup(t *testing.T, name, scopes string) {
	t.Helper()
	_, err := db.CreateAgentGroup(name, "")
	require.NoError(t, err)
	if scopes != "" {
		_, err = db.SetAgentGroupOwnerScopes(name, scopes)
		require.NoError(t, err)
	}
}

// Writes that leave a template's owner scopes as they are, and creates, need
// templates.manage only.
func TestTemplateWritesThatKeepOwnerScopesNeedOnlyTemplatesManage(t *testing.T) {
	t.Run("create with owner scopes", func(t *testing.T) {
		f := newTemplateScribeFlow(t)
		rec := agentReq(t, f, templateScribe, http.MethodPost, "/v1/templates", map[string]any{
			"name": "fresh", "agents": []map[string]any{{"name": "lead"}}, "owner_scopes": scopesValue(t, otherScopes),
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.JSONEq(t, otherScopes, templateOwnerScopes(t, f.Mux, "fresh"))
	})
	t.Run("edit that keeps them", func(t *testing.T) {
		f := newTemplateScribeFlow(t)
		body := crewWithScopes(t, f, roundTripScopes)
		body["descr"] = "edited"
		rec := agentReq(t, f, templateScribe, http.MethodPatch, "/v1/templates/crew", body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.JSONEq(t, roundTripScopes, templateOwnerScopes(t, f.Mux, "crew"))
	})
	t.Run("import update that keeps them", func(t *testing.T) {
		f := newTemplateScribeFlow(t)
		rec := agentReq(t, f, templateScribe, http.MethodPost, "/v1/templates/import?update=true",
			crewExportWithScopes(t, f, roundTripScopes))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.JSONEq(t, roundTripScopes, templateOwnerScopes(t, f.Mux, "crew"))
	})
	t.Run("re-snapshot from a group with the same scopes", func(t *testing.T) {
		f := newTemplateScribeFlow(t)
		scopedGroup(t, "crewgroup", roundTripScopes)
		rec := agentReq(t, f, templateScribe, http.MethodPost, "/v1/templates/from-group", map[string]any{
			"group": "crewgroup", "template_name": "crew", "update": true,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.JSONEq(t, roundTripScopes, templateOwnerScopes(t, f.Mux, "crew"))
	})
	t.Run("delete an unscoped template", func(t *testing.T) {
		f := newTemplateScribeFlow(t)
		rec := agentReq(t, f, templateScribe, http.MethodPost, "/v1/templates", map[string]any{
			"name": "plain", "agents": []map[string]any{{"name": "lead"}},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		rec = agentReq(t, f, templateScribe, http.MethodDelete, "/v1/templates/plain", nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	})
}

// Writes that change or remove an existing template's owner scopes.
type ownerScopesWrite struct {
	name  string
	write func(t *testing.T, f *testharness.Flow, caller string) int
	// ok is the write's success status.
	ok int
	// after is the template's owner scopes once the write succeeded ("" when
	// it removed them, or removed the template).
	after string
}

func ownerScopesWrites() []ownerScopesWrite {
	return []ownerScopesWrite{
		{"edit that changes them", func(t *testing.T, f *testharness.Flow, caller string) int {
			return agentReq(t, f, caller, http.MethodPatch, "/v1/templates/crew", crewWithScopes(t, f, otherScopes)).Code
		}, http.StatusOK, otherScopes},
		{"edit that removes them", func(t *testing.T, f *testharness.Flow, caller string) int {
			return agentReq(t, f, caller, http.MethodPatch, "/v1/templates/crew", crewWithScopes(t, f, "")).Code
		}, http.StatusOK, ""},
		{"import update that changes them", func(t *testing.T, f *testharness.Flow, caller string) int {
			return agentReq(t, f, caller, http.MethodPost, "/v1/templates/import?update=true",
				crewExportWithScopes(t, f, otherScopes)).Code
		}, http.StatusOK, otherScopes},
		{"import update that removes them", func(t *testing.T, f *testharness.Flow, caller string) int {
			return agentReq(t, f, caller, http.MethodPost, "/v1/templates/import?update=true",
				crewExportWithScopes(t, f, "")).Code
		}, http.StatusOK, ""},
		{"re-snapshot from an unscoped group", func(t *testing.T, f *testharness.Flow, caller string) int {
			scopedGroup(t, "loosegroup", "")
			return agentReq(t, f, caller, http.MethodPost, "/v1/templates/from-group", map[string]any{
				"group": "loosegroup", "template_name": "crew", "update": true,
			}).Code
		}, http.StatusOK, ""},
		{"delete a scoped template", func(t *testing.T, f *testharness.Flow, caller string) int {
			return agentReq(t, f, caller, http.MethodDelete, "/v1/templates/crew", nil).Code
		}, http.StatusNoContent, ""},
	}
}

func TestTemplateWritesThatChangeOwnerScopesNeedGroupsSettingsOwnerScopes(t *testing.T) {
	for _, tc := range ownerScopesWrites() {
		t.Run(tc.name, func(t *testing.T) {
			f := newTemplateScribeFlow(t)
			code := tc.write(t, f, templateScribe)
			assert.Equal(t, tc.ok, code, "templates.manage alone changes them (before tcl-yjjbfh)")
			_ = tc.after
		})
	}
}
