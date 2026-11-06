package agentd_test

import (
	"encoding/json"
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

// grantOwnerScopesSlugs grants what a group's owner-scope change needs.
func grantOwnerScopesSlugs(t *testing.T, conv string) {
	t.Helper()
	for _, slug := range []string{agentd.PermGroupsSettingsOwnerScopes, agentd.PermPermissionsGrant, agentd.PermPermissionsRevoke} {
		require.NoError(t, db.GrantAgentPermission(conv, slug, "test"))
	}
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

// crewOwnerScopes is the template's owner scopes, "" when it has none or is gone.
func crewOwnerScopes(t *testing.T, f *testharness.Flow) string {
	t.Helper()
	rec := humanReq(t, f, http.MethodGet, "/v1/templates/crew", nil)
	if rec.Code == http.StatusNotFound {
		return ""
	}
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		OwnerScopes json.RawMessage `json:"owner_scopes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	if string(body.OwnerScopes) == "" || string(body.OwnerScopes) == "null" {
		return ""
	}
	return string(body.OwnerScopes)
}

func assertCrewOwnerScopes(t *testing.T, f *testharness.Flow, want, msg string) {
	t.Helper()
	got := crewOwnerScopes(t, f)
	if want == "" {
		assert.Empty(t, got, msg)
		return
	}
	assert.JSONEq(t, want, got, msg)
}

// A user-visible change (tcl-yjjbfh): changing or removing an existing
// template's owner scopes needs what changing a live group's does
// (groups.settings.owner-scopes, permissions.grant, permissions.revoke).
// templates.manage alone is refused, and the template is left as it was.
func TestTemplateWritesThatChangeOwnerScopesNeedTheGroupOwnerScopeSlugs(t *testing.T) {
	for _, tc := range ownerScopesWrites() {
		t.Run(tc.name+" is refused with templates.manage alone", func(t *testing.T) {
			f := newTemplateScribeFlow(t)
			assert.Equal(t, http.StatusForbidden, tc.write(t, f, templateScribe))
			assertCrewOwnerScopes(t, f, roundTripScopes, "a refused write leaves the owner scopes as they were")
		})
		t.Run(tc.name+" is refused with groups.settings.owner-scopes alone", func(t *testing.T) {
			f := newTemplateScribeFlow(t)
			require.NoError(t, db.GrantAgentPermission(templateScribe, agentd.PermGroupsSettingsOwnerScopes, "test"))
			assert.Equal(t, http.StatusForbidden, tc.write(t, f, templateScribe))
			assertCrewOwnerScopes(t, f, roundTripScopes, "a refused write leaves the owner scopes as they were")
		})
		t.Run(tc.name+" passes with the group's owner-scope slugs", func(t *testing.T) {
			f := newTemplateScribeFlow(t)
			grantOwnerScopesSlugs(t, templateScribe)
			assert.Equal(t, tc.ok, tc.write(t, f, templateScribe))
			assertCrewOwnerScopes(t, f, tc.after, "the write applied")
		})
	}
}

// A config bundle that replaces a template with other owner scopes is judged
// the same way, before any of its items apply.
func TestConfigBundleReplacingTemplateOwnerScopesNeedsTheGroupOwnerScopeSlugs(t *testing.T) {
	f := newTemplateScribeFlow(t)
	importBundle := unscopedCrewBundle(t, f)
	refused := importBundle()
	assert.NotEqual(t, http.StatusOK, refused.Code)
	assert.Contains(t, refused.Body.String(), agentd.PermGroupsSettingsOwnerScopes, "the refusal names the slug")
	assertCrewOwnerScopes(t, f, roundTripScopes, "a refused bundle leaves the owner scopes as they were")

	grantOwnerScopesSlugs(t, templateScribe)
	rec := importBundle()
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assertCrewOwnerScopes(t, f, "", "the bundle replaced the template")
}

// unscopedCrewBundle returns an import, by the scribe holding config.import,
// of a bundle holding crew without its owner scopes.
func unscopedCrewBundle(t *testing.T, f *testharness.Flow) func() *httptest.ResponseRecorder {
	t.Helper()
	require.NoError(t, db.GrantAgentPermission(templateScribe, agentd.PermConfigImport, "test"))
	patch := humanReq(t, f, http.MethodPatch, "/v1/templates/crew", crewWithScopes(t, f, ""))
	require.Equal(t, http.StatusOK, patch.Code, patch.Body.String())
	rec := humanReq(t, f, http.MethodGet, "/v1/config-bundle/export?only=templates/crew", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var bundle map[string]any
	testharness.DecodeJSON(t, rec, &bundle)
	patch = humanReq(t, f, http.MethodPatch, "/v1/templates/crew", crewWithScopes(t, f, roundTripScopes))
	require.Equal(t, http.StatusOK, patch.Code, patch.Body.String())
	return func() *httptest.ResponseRecorder {
		return agentReq(t, f, templateScribe, http.MethodPost, "/v1/config-bundle/import",
			map[string]any{"bundle": bundle, "apply": true, "replace": true})
	}
}

// A use-limited sudo grant of groups.settings.owner-scopes is spent by an edit,
// one use per edit. A bundle cannot spend it, so a bundle refuses it rather than
// let it authorize without counting.
func TestLimitedOwnerScopesSudoIsSpentByAnEditAndRefusedByABundle(t *testing.T) {
	f := newTemplateScribeFlow(t)
	importBundle := unscopedCrewBundle(t, f)
	for _, slug := range []string{agentd.PermPermissionsGrant, agentd.PermPermissionsRevoke} {
		require.NoError(t, db.GrantAgentPermission(templateScribe, slug, "test"))
	}
	now := time.Now()
	id, err := db.InsertSudoGrant(&db.SudoGrant{ConvID: templateScribe, Slug: agentd.PermGroupsSettingsOwnerScopes,
		GrantedAt: now, ExpiresAt: now.Add(time.Hour), GrantedBy: "test", UsesLimit: 1, UsesLeft: 1})
	require.NoError(t, err)

	refused := importBundle()
	assert.NotEqual(t, http.StatusOK, refused.Code, refused.Body.String())
	assert.Contains(t, refused.Body.String(), "use-limited")
	assertCrewOwnerScopes(t, f, roundTripScopes, "a refused bundle leaves the owner scopes as they were")
	assert.Equal(t, int64(1), sudoUsesLeft(t, id), "a refused bundle spends nothing")

	rec := agentReq(t, f, templateScribe, http.MethodPatch, "/v1/templates/crew", crewWithScopes(t, f, otherScopes))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, int64(0), sudoUsesLeft(t, id), "the edit spent the use")
}

// A rename keeps the owner scopes verbatim, even a stored map this build cannot
// decode, so templates.manage alone cannot drop them by renaming.
func TestTemplateRenameKeepsOwnerScopesVerbatim(t *testing.T) {
	f := newTemplateScribeFlow(t)
	const undecodable = `{"no.such.slug":{"group":["g"]}}`
	_, err := db.CreateGroupTemplate(&db.GroupTemplate{Name: "legacy", OwnerScopesJSON: undecodable,
		Agents: []db.GroupTemplateAgent{{Name: "lead"}}})
	require.NoError(t, err)
	for from, to := range map[string]string{"crew": "crew2", "legacy": "legacy2"} {
		before, err := db.GetGroupTemplate(from)
		require.NoError(t, err)
		rec := agentReq(t, f, templateScribe, http.MethodPost, "/v1/library/rename",
			map[string]any{"kind": "templates", "name": from, "to": to})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		after, err := db.GetGroupTemplate(to)
		require.NoError(t, err)
		require.NotNil(t, after)
		assert.Equal(t, before.OwnerScopesJSON, after.OwnerScopesJSON, "renaming %s keeps its owner scopes", from)
	}
}
