package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestProcessTemplateRoutes404WhenFeatureOff(t *testing.T) {
	f := newFlow(t)
	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/process/templates"},
		{http.MethodGet, "/v1/process/templates/example"},
		{http.MethodPost, "/v1/process/templates/example"},
		{http.MethodPost, "/v1/process/validate"},
	} {
		rec := processTemplateRequest(t, f, test.method, test.path, map[string]any{})
		assert.Equalf(t, http.StatusNotFound, rec.Code, "%s %s: %s", test.method, test.path, rec.Body.String())
	}
}

func TestProcessTemplateRESTListGetSaveAndConflict(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	restore := fs.SetNowForTest(func() time.Time { return now })
	t.Cleanup(restore)

	old := processRESTTemplate("release", "first version", 20)
	oldRecord, err := fs.PutTemplate(t.Context(), old)
	require.NoError(t, err)
	now = now.Add(time.Minute)
	latest := processRESTTemplate("release", "latest version", 80)
	latest.Name = "Release train"
	latest.Nodes["finish"] = model.Node{Type: model.NodeTypeEnd, Name: "Finished", Description: "All done", Doc: "Completion documentation", Result: "success"}
	start := latest.Nodes["begin"]
	start.Next = model.Next{"pass": "finish"}
	latest.Nodes["begin"] = start
	latestRecord, err := fs.PutTemplate(t.Context(), latest)
	require.NoError(t, err)
	require.NotEqual(t, oldRecord.Ref, latestRecord.Ref)

	listRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates", nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	var list struct {
		Templates []struct {
			ID            string `json:"id"`
			Description   string `json:"description"`
			VersionCount  int    `json:"versionCount"`
			LatestVersion struct {
				Ref          string `json:"ref"`
				SemanticHash string `json:"semanticHash"`
				SourceHash   string `json:"sourceHash"`
			} `json:"latestVersion"`
			Versions []any `json:"versions"`
		} `json:"templates"`
	}
	testharness.DecodeJSON(t, listRec, &list)
	require.Len(t, list.Templates, 1)
	assert.Equal(t, "release", list.Templates[0].ID)
	assert.Equal(t, "latest version", list.Templates[0].Description)
	assert.Equal(t, 2, list.Templates[0].VersionCount)
	assert.Len(t, list.Templates[0].Versions, 2)
	assert.Equal(t, latestRecord.Ref, list.Templates[0].LatestVersion.Ref)
	assert.NotEmpty(t, list.Templates[0].LatestVersion.SourceHash)

	getRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/release", nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var edit processEditResponse
	testharness.DecodeJSON(t, getRec, &edit)
	require.NotNil(t, edit.Template)
	assert.Equal(t, "Release train", edit.Template.Name)
	assert.Nil(t, edit.Template.Layout, "layout is a separate edit-model section")
	require.NotNil(t, edit.Layout)
	assert.Equal(t, float64(80), edit.Layout.Nodes["begin"].X)
	assert.Equal(t, latestRecord.SemanticHash, edit.SemanticHash)
	assert.Contains(t, edit.Source, "layout:")
	assert.NotEmpty(t, edit.SourceHash)

	oldRec := processTemplateRequest(t, f, http.MethodGet,
		"/v1/process/templates/release?version="+url.QueryEscape(oldRecord.SemanticHash), nil)
	require.Equal(t, http.StatusOK, oldRec.Code, oldRec.Body.String())
	var oldEdit processEditResponse
	testharness.DecodeJSON(t, oldRec, &oldEdit)
	assert.Equal(t, "first version", oldEdit.Template.Description)

	conflictBody := edit
	conflictBody.SourceHash = "stale-source-hash"
	conflictRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/release", conflictBody)
	assert.Equal(t, http.StatusConflict, conflictRec.Code, conflictRec.Body.String())
	assert.Contains(t, conflictRec.Body.String(), "currentSourceHash")

	edit.Template.Description = "saved from editor"
	saveRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/release", edit)
	require.Equal(t, http.StatusCreated, saveRec.Code, saveRec.Body.String())
	var saved struct {
		Ref          string            `json:"ref"`
		SemanticHash string            `json:"semanticHash"`
		SourceHash   string            `json:"sourceHash"`
		Diagnostics  []processEditDiag `json:"diagnostics"`
	}
	testharness.DecodeJSON(t, saveRec, &saved)
	assert.NotEqual(t, latestRecord.Ref, saved.Ref)
	assert.NotEqual(t, edit.SourceHash, saved.SourceHash)
	assert.NotEmpty(t, saved.SemanticHash)
}

func TestProcessTemplateSavePersistsLayoutOnlyEditAtSameRef(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, err := fs.PutTemplate(t.Context(), processRESTTemplate("layout-only", "move me", 10))
	require.NoError(t, err)

	getRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/layout-only", nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var edit processEditResponse
	testharness.DecodeJSON(t, getRec, &edit)
	baseHash := edit.SourceHash
	edit.Layout.Nodes["begin"] = model.LayoutNode{X: 444, Y: 222}

	saveRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/layout-only", edit)
	require.Equal(t, http.StatusCreated, saveRec.Code, saveRec.Body.String())
	var saved struct {
		Ref        string `json:"ref"`
		SourceHash string `json:"sourceHash"`
	}
	testharness.DecodeJSON(t, saveRec, &saved)
	assert.Equal(t, record.Ref, saved.Ref, "layout does not alter semantic identity")
	assert.NotEqual(t, baseHash, saved.SourceHash, "source hash includes layout")

	reopenRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/layout-only", nil)
	require.Equal(t, http.StatusOK, reopenRec.Code, reopenRec.Body.String())
	var reopened processEditResponse
	testharness.DecodeJSON(t, reopenRec, &reopened)
	assert.Equal(t, float64(444), reopened.Layout.Nodes["begin"].X)
	assert.Equal(t, saved.SourceHash, reopened.SourceHash)
}

func TestProcessValidateReturnsEditorScopedAdvisoryDiagnostics(t *testing.T) {
	f, _ := processEngineFlow(t)
	tmpl := processRESTTemplate("validate-me", "invalid edge", 10)
	edges := model.NormalizeEdges(tmpl)
	for i := range edges {
		if edges[i].From == "begin" {
			edges[i].To = "missing"
		}
	}
	body := processEditResponse{Template: semanticProcessTemplate(tmpl), Edges: edges, Layout: tmpl.Layout}
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/validate", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response struct {
		Diagnostics []processEditDiag `json:"diagnostics"`
	}
	testharness.DecodeJSON(t, rec, &response)
	require.NotEmpty(t, response.Diagnostics)
	assert.Contains(t, response.Diagnostics, processEditDiag{
		Scope: "edge", TargetID: "begin:pass", Severity: model.SeverityError,
		Code: "unknown_target", Message: `target node "missing" is not declared`,
	})

	// Editor saves intentionally keep structurally serializable drafts even
	// when validation reports errors, so multi-step graph edits are not lost.
	saveRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/validate-me", body)
	require.Equal(t, http.StatusCreated, saveRec.Code, saveRec.Body.String())
	assert.Contains(t, saveRec.Body.String(), "unknown_target")
}

func TestDashboardProcessRESTRequiresDashboardAuth(t *testing.T) {
	processEngineFlow(t)
	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux)
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/v1/process/templates", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestDashboardSnapshotDynamicallyGatesProcessesTab(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.False(t, snap.ProcessesEnabled)
	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Processes: true}}))
	snap = fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.True(t, snap.ProcessesEnabled)
}

type processEditResponse struct {
	Template     *model.Template `json:"template"`
	Edges        []model.Edge    `json:"edges"`
	Layout       *model.Layout   `json:"layout"`
	SourceHash   string          `json:"sourceHash"`
	SemanticHash string          `json:"semanticHash"`
	Source       string          `json:"source"`
}

type processEditDiag struct {
	Scope    string         `json:"scope"`
	TargetID string         `json:"targetId"`
	Severity model.Severity `json:"severity"`
	Code     string         `json:"code"`
	Message  string         `json:"message"`
}

func processRESTTemplate(id, description string, x float64) *model.Template {
	return &model.Template{
		APIVersion:  model.APIVersion,
		Kind:        model.Kind,
		ID:          id,
		Name:        "Process " + id,
		Description: description,
		Doc:         "Template documentation",
		Start:       "begin",
		Nodes: map[string]model.Node{
			"begin": {
				Type: model.NodeTypeStart, Name: "Begin", Description: "Start here", Doc: "Start documentation",
				Next: model.Next{"pass": "done"},
			},
			"done": {Type: model.NodeTypeEnd, Name: "Done", Description: "Finished", Doc: "End documentation", Result: "success"},
		},
		Layout: &model.Layout{Nodes: map[string]model.LayoutNode{
			"begin": {X: x, Y: 30}, "done": {X: x + 200, Y: 30},
		}},
	}
}

func semanticProcessTemplate(tmpl *model.Template) *model.Template {
	clone := *tmpl
	clone.Layout = nil
	return &clone
}

func processTemplateRequest(t *testing.T, f *testharness.Flow, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := testharness.JSONRequest(t, method, path, body)
	return testharness.Serve(f.Mux, agentd.AsHumanPeer(req))
}
