package agentd_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
		{http.MethodGet, "/v1/process/template-heads"},
		{http.MethodGet, "/v1/process/templates/example"},
		{http.MethodPost, "/v1/process/templates/example"},
		{http.MethodPost, "/v1/process/validate"},
	} {
		rec := processTemplateRequest(t, f, test.method, test.path, map[string]any{})
		assert.Equalf(t, http.StatusNotFound, rec.Code, "%s %s: %s", test.method, test.path, rec.Body.String())
	}
}

func TestProcessTemplatePermissionSlugsAreRegistered(t *testing.T) {
	assert.True(t, agentd.IsKnownPermSlug(agentd.PermProcessTemplatesRead))
	assert.True(t, agentd.IsKnownPermSlug(agentd.PermProcessTemplatesManage))
}

func TestProcessTemplateRESTListGetSaveAndConflict(t *testing.T) {
	f, root := processAuthoringFlow(t)
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

	headsRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/template-heads", nil)
	require.Equal(t, http.StatusOK, headsRec.Code, headsRec.Body.String())
	var heads struct {
		Heads []store.TemplateHead `json:"heads"`
	}
	testharness.DecodeJSON(t, headsRec, &heads)
	require.Len(t, heads.Heads, 1)
	assert.Equal(t, latestRecord.Ref, heads.Heads[0].Ref)
	assert.Equal(t, "release", heads.Heads[0].ID)
	assert.Equal(t, list.Templates[0].LatestVersion.SourceHash, heads.Heads[0].SourceHash)
	assert.NotContains(t, headsRec.Body.String(), `"actor"`, "legacy/unattributed heads must not invent an identity")

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
	var conflict struct {
		Code              string `json:"code"`
		Error             string `json:"error"`
		CurrentSourceHash string `json:"currentSourceHash"`
		CurrentRef        string `json:"currentRef"`
	}
	testharness.DecodeJSON(t, conflictRec, &conflict)
	assert.Equal(t, "process_template_conflict", conflict.Code)
	assert.Equal(t, "template head changed since it was opened", conflict.Error)
	assert.Equal(t, edit.SourceHash, conflict.CurrentSourceHash)
	assert.Equal(t, latestRecord.Ref, conflict.CurrentRef)
	assert.NotContains(t, conflictRec.Body.String(), `"message"`)

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

// The Templates list edits a description by round-tripping the head edit view
// through the ordinary save route, changing exactly one top-level field. This
// pins that the existing route expresses that safely: a description-only edit
// keeps every other field, still cuts a normal version, can clear the value,
// and still loses the CAS race against a head that moved -- so the list needs
// no in-place metadata mutation that would bypass versioning or hashing.
func TestProcessTemplateSaveDescriptionOnlyEditPreservesTemplateAndCAS(t *testing.T) {
	f, root := processAuthoringFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	original := processRESTTemplate("described", "first description", 40)
	originalRecord, err := fs.PutTemplate(t.Context(), original)
	require.NoError(t, err)

	getRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/described", nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var head processEditResponse
	testharness.DecodeJSON(t, getRec, &head)
	require.NotNil(t, head.Template)
	require.NotEmpty(t, head.Edges, "the edit view carries the graph edges the list must round-trip")
	require.NotNil(t, head.Layout)
	baseHash := head.SourceHash

	// Exactly the wire shape the list commits: the head's editable model with a
	// single top-level field replaced, saved against the hash observed when the
	// edit session began.
	describe := func(sourceHash, description string) *httptest.ResponseRecorder {
		edited := *head.Template
		edited.Description = description
		return processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/described", map[string]any{
			"template": &edited, "edges": head.Edges, "layout": head.Layout, "sourceHash": sourceHash,
		})
	}

	saveRec := describe(baseHash, "corrected description")
	require.Equal(t, http.StatusCreated, saveRec.Code, saveRec.Body.String())
	var saved struct {
		Ref        string `json:"ref"`
		SourceHash string `json:"sourceHash"`
	}
	testharness.DecodeJSON(t, saveRec, &saved)
	assert.NotEqual(t, originalRecord.Ref, saved.Ref, "a description is semantic, so editing it cuts a new version")
	assert.NotEqual(t, baseHash, saved.SourceHash)

	reopenRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/described", nil)
	require.Equal(t, http.StatusOK, reopenRec.Code, reopenRec.Body.String())
	var reopened processEditResponse
	testharness.DecodeJSON(t, reopenRec, &reopened)
	require.NotNil(t, reopened.Template)
	assert.Equal(t, "corrected description", reopened.Template.Description)
	// Everything else must survive value-equivalently: comparing the whole
	// template with the description normalised back catches any field the save
	// path might quietly drop, not just the ones spelled out below.
	restored := *reopened.Template
	restored.Description = head.Template.Description
	assert.Equal(t, *head.Template, restored, "a description edit changes nothing else in the template")
	assert.Equal(t, head.Edges, reopened.Edges, "edges round-trip untouched")
	assert.Equal(t, head.Layout, reopened.Layout, "editor layout round-trips untouched")

	listRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates", nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	var list struct {
		Templates []struct {
			ID           string `json:"id"`
			Description  string `json:"description"`
			VersionCount int    `json:"versionCount"`
		} `json:"templates"`
	}
	testharness.DecodeJSON(t, listRec, &list)
	require.Len(t, list.Templates, 1)
	assert.Equal(t, "corrected description", list.Templates[0].Description, "the list shows the edited description")
	assert.Equal(t, 2, list.Templates[0].VersionCount, "the edit is a normal new version, not an in-place mutation")

	// A stale baseline must still lose: the operator's edit session started on a
	// head that has since moved.
	staleRec := describe(baseHash, "written over a concurrent edit")
	require.Equal(t, http.StatusConflict, staleRec.Code, staleRec.Body.String())
	var conflict struct {
		Code              string `json:"code"`
		CurrentSourceHash string `json:"currentSourceHash"`
	}
	testharness.DecodeJSON(t, staleRec, &conflict)
	assert.Equal(t, "process_template_conflict", conflict.Code)
	assert.Equal(t, saved.SourceHash, conflict.CurrentSourceHash)

	clearRec := describe(saved.SourceHash, "")
	require.Equal(t, http.StatusCreated, clearRec.Code, clearRec.Body.String())
	clearedRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/described", nil)
	require.Equal(t, http.StatusOK, clearedRec.Code, clearedRec.Body.String())
	var cleared processEditResponse
	testharness.DecodeJSON(t, clearedRec, &cleared)
	require.NotNil(t, cleared.Template)
	assert.Empty(t, cleared.Template.Description, "an emptied description clears rather than being ignored")
	assert.Equal(t, head.Template.Name, cleared.Template.Name, "clearing the description keeps the display name")
	assert.Equal(t, head.Template.Doc, cleared.Template.Doc, "clearing the description keeps the documentation")
	assert.Equal(t, head.Edges, cleared.Edges)
	assert.Equal(t, head.Layout, cleared.Layout)
}

func TestProcessTemplateGetRejectsLegacyOverBudgetSourceWithoutPanic(t *testing.T) {
	f, root := processAuthoringFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, err := fs.PutTemplate(t.Context(), processRESTTemplate("legacy-large", "legacy source", 10))
	require.NoError(t, err)

	// Simulate a version written before normalized graph cardinality was an
	// explicit store invariant. The immutable semantic record still selects the
	// version, while the authoring source contains compact alias amplification.
	source := processAliasedNextSource(64, 64)
	sourcePath := filepath.Join(root, "templates", record.ID, "sha256-"+record.SemanticHash, "template.yaml")
	require.NoError(t, os.WriteFile(sourcePath, source, 0o644))

	rec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/legacy-large", nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	var response struct {
		Code        string            `json:"code"`
		Diagnostics []processEditDiag `json:"diagnostics"`
	}
	testharness.DecodeJSON(t, rec, &response)
	assert.Equal(t, "process_template_invalid", response.Code)
	require.Len(t, response.Diagnostics, 1)
	assert.Equal(t, model.DiagnosticCodeNormalizedEdgeLimit, response.Diagnostics[0].Code)
	assert.Equal(t, "template", response.Diagnostics[0].Scope)
}

func TestProcessTemplateGetRejectsLegacyMalformedGraphKeyWithTypedDiagnostic(t *testing.T) {
	f, root := processAuthoringFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, err := fs.PutTemplate(t.Context(), processRESTTemplate("legacy-malformed", "legacy source", 10))
	require.NoError(t, err)

	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: legacy-malformed
nodes:
  ? [malformed]
  : {type: end}
`)
	sourcePath := filepath.Join(root, "templates", record.ID, "sha256-"+record.SemanticHash, "template.yaml")
	require.NoError(t, os.WriteFile(sourcePath, source, 0o644))

	rec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/legacy-malformed", nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	var response struct {
		Code        string            `json:"code"`
		Diagnostics []processEditDiag `json:"diagnostics"`
	}
	testharness.DecodeJSON(t, rec, &response)
	assert.Equal(t, "process_template_invalid", response.Code)
	require.Len(t, response.Diagnostics, 1)
	assert.Equal(t, model.DiagnosticCodeInvalidGraphKey, response.Diagnostics[0].Code)
	assert.Equal(t, "template", response.Diagnostics[0].Scope)
	assert.Empty(t, response.Diagnostics[0].TargetID)
	assert.Less(t, rec.Body.Len(), 1024)
}

func TestProcessTemplateSaveStoreFailureIsInternalError(t *testing.T) {
	f, root := processAuthoringFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	_, err = fs.PutTemplate(t.Context(), processRESTTemplate("broken-head", "before corruption", 10))
	require.NoError(t, err)
	getRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/broken-head", nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var edit processEditResponse
	testharness.DecodeJSON(t, getRec, &edit)
	require.NoError(t, os.WriteFile(filepath.Join(root, "templates", "broken-head", "head"), []byte("invalid-ref\n"), 0o644))

	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/broken-head", edit)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"process_template_store"`)
	assert.NotContains(t, rec.Body.String(), "process_template_unserializable")
}

func TestProcessTemplateSaveRejectsUnsafeIdentityAsClientError(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	tmpl := processRESTTemplate("Bad", "unsafe identity", 10)
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/Bad", map[string]any{
		"template": tmpl,
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"process_template_invalid_id"`)
	assert.NotContains(t, rec.Body.String(), "process_template_store")
}

func TestProcessTemplateSaveHonorsNestedLayoutWhenTopLevelOmitted(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	tmpl := processRESTTemplate("nested-layout", "complete template client", 321)
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/nested-layout", map[string]any{
		"template": tmpl,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	reopenRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/nested-layout", nil)
	require.Equal(t, http.StatusOK, reopenRec.Code, reopenRec.Body.String())
	var reopened processEditResponse
	testharness.DecodeJSON(t, reopenRec, &reopened)
	require.NotNil(t, reopened.Layout)
	assert.Equal(t, float64(321), reopened.Layout.Nodes["begin"].X)
}

func TestProcessTemplateSavePersistsLayoutOnlyEditAtSameRef(t *testing.T) {
	f, root := processAuthoringFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, err := fs.PutTemplate(t.Context(), processRESTTemplate("layout-only", "move me", 10))
	require.NoError(t, err)

	getRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/layout-only", nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var edit processEditResponse
	testharness.DecodeJSON(t, getRec, &edit)
	baseHash := edit.SourceHash
	beforeHeadsRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/template-heads", nil)
	require.Equal(t, http.StatusOK, beforeHeadsRec.Code, beforeHeadsRec.Body.String())
	var beforeHeads struct {
		Heads []store.TemplateHead `json:"heads"`
	}
	testharness.DecodeJSON(t, beforeHeadsRec, &beforeHeads)
	require.Len(t, beforeHeads.Heads, 1)
	assert.Equal(t, baseHash, beforeHeads.Heads[0].SourceHash)
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
	afterHeadsRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/template-heads", nil)
	require.Equal(t, http.StatusOK, afterHeadsRec.Code, afterHeadsRec.Body.String())
	var afterHeads struct {
		Heads []store.TemplateHead `json:"heads"`
	}
	testharness.DecodeJSON(t, afterHeadsRec, &afterHeads)
	require.Len(t, afterHeads.Heads, 1)
	assert.Equal(t, record.Ref, afterHeads.Heads[0].Ref)
	assert.Equal(t, saved.SourceHash, afterHeads.Heads[0].SourceHash)

	reopenRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/layout-only", nil)
	require.Equal(t, http.StatusOK, reopenRec.Code, reopenRec.Body.String())
	var reopened processEditResponse
	testharness.DecodeJSON(t, reopenRec, &reopened)
	assert.Equal(t, float64(444), reopened.Layout.Nodes["begin"].X)
	assert.Equal(t, saved.SourceHash, reopened.SourceHash)
}

func TestProcessTemplateSaveCanRevertHeadToExistingSemanticVersion(t *testing.T) {
	f, root := processAuthoringFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	first := processRESTTemplate("revert", "first", 10)
	firstRecord, err := fs.PutTemplate(t.Context(), first)
	require.NoError(t, err)
	second := processRESTTemplate("revert", "second", 20)
	_, err = fs.PutTemplate(t.Context(), second)
	require.NoError(t, err)

	getRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/revert", nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var edit processEditResponse
	testharness.DecodeJSON(t, getRec, &edit)
	assert.Equal(t, "second", edit.Template.Description)
	edit.Template = semanticProcessTemplate(first)
	edit.Edges = model.NormalizeEdges(first)
	edit.Layout = first.Layout

	saveRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/revert", edit)
	require.Equal(t, http.StatusCreated, saveRec.Code, saveRec.Body.String())
	assert.Contains(t, saveRec.Body.String(), firstRecord.Ref)
	reopenRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/revert", nil)
	require.Equal(t, http.StatusOK, reopenRec.Code, reopenRec.Body.String())
	var reopened processEditResponse
	testharness.DecodeJSON(t, reopenRec, &reopened)
	assert.Equal(t, "first", reopened.Template.Description)
	assert.Equal(t, float64(10), reopened.Layout.Nodes["begin"].X)
}

func TestProcessTemplateSaveRequiresProcessTemplatesManageForAgent(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	const intruder = "proc-intruder-aaaa-bbbb"
	tmpl := processRESTTemplate("agent-owned", "agent draft", 10)
	body := processEditResponse{
		Template: semanticProcessTemplate(tmpl), Edges: model.NormalizeEdges(tmpl), Layout: tmpl.Layout,
	}
	rec := agentReq(t, f, intruder, http.MethodPost, "/v1/process/templates/agent-owned", body)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), agentd.PermProcessTemplatesManage)
	require.NoError(t, db.GrantAgentPermission(intruder, agentd.PermProcessTemplatesManage, "test"))
	rec = agentReq(t, f, intruder, http.MethodPost, "/v1/process/templates/agent-owned", body)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestProcessTemplateAgentSourceWorkflowPermissionsCASAndAttribution(t *testing.T) {
	f, root := processAuthoringFlow(t)
	const scribe = "proc-scribe-aaaa-bbbb"

	tmpl := processRESTTemplate("agent-source", "created conversationally", 10)
	tmpl.Layout = nil // new templates omit editor-owned layout
	source, err := model.CanonicalYAML(tmpl)
	require.NoError(t, err)

	// Read and manage are deliberately independent. Holding manage lets the
	// scribe save, but does not silently confer discovery/validation access.
	require.NoError(t, db.GrantAgentPermission(scribe, agentd.PermProcessTemplatesManage, "test"))
	readDenied := agentReq(t, f, scribe, http.MethodGet, "/v1/process/templates", nil)
	assert.Equal(t, http.StatusForbidden, readDenied.Code, readDenied.Body.String())
	assert.Contains(t, readDenied.Body.String(), agentd.PermProcessTemplatesRead)

	created := agentReq(t, f, scribe, http.MethodPost, "/v1/process/templates/agent-source", map[string]any{
		"source": string(source),
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var saved struct {
		Ref        string    `json:"ref"`
		SourceHash string    `json:"sourceHash"`
		Actor      string    `json:"actor"`
		AuthoredAt time.Time `json:"authoredAt"`
	}
	testharness.DecodeJSON(t, created, &saved)
	assert.NotEmpty(t, saved.Ref)
	assert.NotEmpty(t, saved.SourceHash)
	assert.Regexp(t, `^agent:agt_`, saved.Actor)
	assert.False(t, saved.AuthoredAt.IsZero())

	// The dashboard/human REST view sees the exact same store record, including
	// append-preserving actor attribution; there is no agent-only template store.
	listRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates", nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	assert.Contains(t, listRec.Body.String(), `"id":"agent-source"`)
	assert.Contains(t, listRec.Body.String(), saved.Actor)
	getRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/agent-source", nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var shown struct {
		CurrentRef string                     `json:"currentRef"`
		SourceHash string                     `json:"sourceHash"`
		Authorship []store.TemplateAuthorship `json:"authorship"`
	}
	testharness.DecodeJSON(t, getRec, &shown)
	assert.Equal(t, saved.Ref, shown.CurrentRef)
	assert.Equal(t, saved.SourceHash, shown.SourceHash)
	require.Len(t, shown.Authorship, 1)
	assert.Equal(t, saved.Actor, string(shown.Authorship[0].Actor))
	headsRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/template-heads", nil)
	require.Equal(t, http.StatusOK, headsRec.Code, headsRec.Body.String())
	var heads struct {
		Heads []struct {
			store.TemplateHead
			Actor      string    `json:"actor"`
			AuthoredAt time.Time `json:"authoredAt"`
		} `json:"heads"`
	}
	testharness.DecodeJSON(t, headsRec, &heads)
	require.Len(t, heads.Heads, 1)
	assert.Equal(t, saved.Ref, heads.Heads[0].Ref)
	assert.Equal(t, saved.SourceHash, heads.Heads[0].SourceHash)
	assert.Equal(t, saved.Actor, heads.Heads[0].Actor)
	assert.Equal(t, saved.AuthoredAt, heads.Heads[0].AuthoredAt)

	// A complete scribe grant includes read for show/validate, but stale CAS is
	// still refused with the documented 409 shape.
	require.NoError(t, db.GrantAgentPermission(scribe, agentd.PermProcessTemplatesRead, "test"))
	validateRec := agentReq(t, f, scribe, http.MethodPost, "/v1/process/validate", map[string]any{
		"source": string(source),
	})
	require.Equal(t, http.StatusOK, validateRec.Code, validateRec.Body.String())
	stale := agentReq(t, f, scribe, http.MethodPost, "/v1/process/templates/agent-source", map[string]any{
		"source": string(source), "sourceHash": "stale-source-hash",
	})
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	assert.Contains(t, stale.Body.String(), `"code":"process_template_conflict"`)
	assert.Contains(t, stale.Body.String(), `"currentSourceHash":"`+saved.SourceHash+`"`)
	assert.Contains(t, stale.Body.String(), `"currentRef":"`+saved.Ref+`"`)

	// A later source/layout-only save retains the semantic ref but advances the
	// committed generation. The bounded poll must attribute the exact new
	// ref+sourceHash pair, not fall back to the first event on this ref.
	const secondScribe = "proc-scribe-cccc-dddd"
	require.NoError(t, db.GrantAgentPermission(secondScribe, agentd.PermProcessTemplatesManage, "test"))
	tmpl.Layout = &model.Layout{Nodes: map[string]model.LayoutNode{
		"begin": {X: 120, Y: 80},
	}}
	layoutSource, err := model.CanonicalYAML(tmpl)
	require.NoError(t, err)
	secondSave := agentReq(t, f, secondScribe, http.MethodPost, "/v1/process/templates/agent-source", map[string]any{
		"source": string(layoutSource), "sourceHash": saved.SourceHash,
	})
	require.Equal(t, http.StatusCreated, secondSave.Code, secondSave.Body.String())
	var second struct {
		Ref        string    `json:"ref"`
		SourceHash string    `json:"sourceHash"`
		Actor      string    `json:"actor"`
		AuthoredAt time.Time `json:"authoredAt"`
	}
	testharness.DecodeJSON(t, secondSave, &second)
	assert.Equal(t, saved.Ref, second.Ref, "layout-only authoring keeps the content-addressed semantic ref")
	assert.NotEqual(t, saved.SourceHash, second.SourceHash)
	assert.NotEqual(t, saved.Actor, second.Actor)

	headsRec = processTemplateRequest(t, f, http.MethodGet, "/v1/process/template-heads", nil)
	require.Equal(t, http.StatusOK, headsRec.Code, headsRec.Body.String())
	testharness.DecodeJSON(t, headsRec, &heads)
	require.Len(t, heads.Heads, 1)
	assert.Equal(t, second.Ref, heads.Heads[0].Ref)
	assert.Equal(t, second.SourceHash, heads.Heads[0].SourceHash)
	assert.Equal(t, second.Actor, heads.Heads[0].Actor)

	// Polling attribution is served from the exact bounded head pointer. Even a
	// multi-megabyte corrupt append-only history is outside this read path: it
	// cannot block/fail the heads response and must not trigger actor inference.
	semanticHash := strings.TrimPrefix(second.Ref, "agent-source@sha256:")
	authorshipPath := filepath.Join(root, "templates", "agent-source", "sha256-"+semanticHash, "authorship.jsonl")
	require.NoError(t, os.WriteFile(authorshipPath, []byte(strings.Repeat("corrupt-history\n", 400_000)), 0o644))
	corruptGet := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/agent-source", nil)
	assert.Equal(t, http.StatusInternalServerError, corruptGet.Code, "fixture must be genuinely corrupt: %s", corruptGet.Body.String())
	boundedReview := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/agent-source?authorship=omit", nil)
	require.Equal(t, http.StatusOK, boundedReview.Code, boundedReview.Body.String())
	assert.NotContains(t, boundedReview.Body.String(), `"authorship"`, "dashboard review omits append-only history")
	var bounded struct {
		CurrentRef string         `json:"currentRef"`
		SourceHash string         `json:"sourceHash"`
		Actor      string         `json:"actor"`
		AuthoredAt time.Time      `json:"authoredAt"`
		Template   model.Template `json:"template"`
		Layout     model.Layout   `json:"layout"`
	}
	testharness.DecodeJSON(t, boundedReview, &bounded)
	assert.Equal(t, second.Ref, bounded.CurrentRef)
	assert.Equal(t, second.SourceHash, bounded.SourceHash)
	assert.Equal(t, second.Actor, bounded.Actor)
	assert.Equal(t, second.AuthoredAt, bounded.AuthoredAt)
	assert.Equal(t, tmpl.Description, bounded.Template.Description, "exact external semantics remain visible")
	assert.Equal(t, float64(120), bounded.Layout.Nodes["begin"].X)
	headsRec = processTemplateRequest(t, f, http.MethodGet, "/v1/process/template-heads", nil)
	require.Equal(t, http.StatusOK, headsRec.Code, headsRec.Body.String())
	testharness.DecodeJSON(t, headsRec, &heads)
	require.Len(t, heads.Heads, 1)
	assert.Equal(t, second.Ref, heads.Heads[0].Ref)
	assert.Equal(t, second.SourceHash, heads.Heads[0].SourceHash)
	assert.Equal(t, second.Actor, heads.Heads[0].Actor)
}

func TestProcessTemplateRawSourceValidationPreservesYAMLDiagnostics(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	source := "apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: raw\nunknown: true\nstart: done\nnodes:\n  done:\n    type: end\n"
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/validate", map[string]any{
		"source": source,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"unknown_field"`)

	// The dashboard edit model still allows advisory draft saves, but the raw
	// YAML scribe path refuses validation errors so a skipped validate cannot
	// silently canonicalize away unknown fields.
	save := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/raw", map[string]any{
		"source": source,
	})
	require.Equal(t, http.StatusUnprocessableEntity, save.Code, save.Body.String())
	assert.Contains(t, save.Body.String(), `"code":"process_template_invalid"`)
	list := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates", nil)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	assert.NotContains(t, list.Body.String(), `"id":"raw"`)
}

func TestProcessTemplateRawSchemaAliasBudgetIsStableAndRejectsSave(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	source := processSchemaAliasSource(model.MaxNormalizedNodes)
	validate := processTemplateRequest(t, f, http.MethodPost, "/v1/process/validate", map[string]any{"source": string(source)})
	require.Equal(t, http.StatusOK, validate.Code, validate.Body.String())
	assert.Contains(t, validate.Body.String(), `"code":"template_diagnostic_budget"`)
	assert.LessOrEqual(t, validate.Body.Len(), model.MaxProcessTemplateSourceBytes, "encoded editor diagnostics stay inside the public wire scale")
	assert.Equal(t, 1, strings.Count(validate.Body.String(), `"code":"template_diagnostic_budget"`))

	save := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/schema-budget", map[string]any{"source": string(source)})
	require.Equal(t, http.StatusUnprocessableEntity, save.Code, save.Body.String())
	assert.Contains(t, save.Body.String(), `"code":"process_template_invalid"`)
	assert.Contains(t, save.Body.String(), `"code":"template_diagnostic_budget"`)
}

func TestProcessTemplateDuplicateFloodResponseIsBounded(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	source := processDuplicateMetadataSource(100_000)
	require.Less(t, len(source), model.MaxProcessTemplateSourceBytes)
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/validate", map[string]any{"source": string(source)})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"duplicate_key"`)
	assert.Equal(t, 1, strings.Count(rec.Body.String(), `"code":"template_diagnostic_budget"`))
	assert.LessOrEqual(t, rec.Body.Len(), model.MaxProcessTemplateSourceBytes)
}

func TestProcessTemplateSemanticDiagnosticFloodIsBoundedAndRejectsSave(t *testing.T) {
	f, root := processAuthoringFlow(t)
	tmpl := semanticDiagnosticFloodProcessTemplate()

	validate := processTemplateRequest(t, f, http.MethodPost, "/v1/process/validate", map[string]any{"template": tmpl})
	require.Equal(t, http.StatusOK, validate.Code, validate.Body.String())
	assert.Contains(t, validate.Body.String(), `"code":"undeclared_param_ref"`)
	assert.Equal(t, 1, strings.Count(validate.Body.String(), `"code":"template_diagnostic_budget"`))
	assert.LessOrEqual(t, validate.Body.Len(), model.MaxProcessTemplateSourceBytes)

	save := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/semantic-budget", map[string]any{"template": tmpl})
	require.Equal(t, http.StatusUnprocessableEntity, save.Code, save.Body.String())
	assert.Contains(t, save.Body.String(), `"code":"process_template_invalid"`)
	assert.Equal(t, 1, strings.Count(save.Body.String(), `"code":"template_diagnostic_budget"`))
	assert.LessOrEqual(t, save.Body.Len(), model.MaxProcessTemplateSourceBytes)

	copyTemplate := semanticDiagnosticFloodProcessTemplate()
	copyTemplate.ID = "" // The editor's copy flow lets the destination path supply the new id.
	copyResponse := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/semantic-budget-copy", map[string]any{"template": copyTemplate})
	require.Equal(t, http.StatusUnprocessableEntity, copyResponse.Code, copyResponse.Body.String())
	assert.Equal(t, 1, strings.Count(copyResponse.Body.String(), `"code":"template_diagnostic_budget"`))
	assert.LessOrEqual(t, copyResponse.Body.Len(), model.MaxProcessTemplateSourceBytes)

	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, err := fs.PutTemplate(t.Context(), processRESTTemplate("legacy-semantic-budget", "legacy source", 10))
	require.NoError(t, err)
	tmpl.ID = "legacy-semantic-budget"
	source, err := model.CanonicalYAML(tmpl)
	require.NoError(t, err)
	sourcePath := filepath.Join(root, "templates", record.ID, "sha256-"+record.SemanticHash, "template.yaml")
	require.NoError(t, os.WriteFile(sourcePath, source, 0o644))

	legacy := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/legacy-semantic-budget", nil)
	require.Equal(t, http.StatusUnprocessableEntity, legacy.Code, legacy.Body.String())
	assert.Equal(t, 1, strings.Count(legacy.Body.String(), `"code":"template_diagnostic_budget"`))
	assert.LessOrEqual(t, legacy.Body.Len(), model.MaxProcessTemplateSourceBytes)
}

func TestProcessValidateReturnsEditorScopedAdvisoryDiagnostics(t *testing.T) {
	f, _ := processAuthoringFlow(t)
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

func TestProcessValidateReturnsStableCardinalityDiagnosticsAndSaveRejects(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	tmpl := overBudgetProcessTemplate("cardinality")
	body := map[string]any{"template": tmpl}

	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/validate", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response struct {
		SemanticHash string            `json:"semanticHash"`
		Diagnostics  []processEditDiag `json:"diagnostics"`
	}
	testharness.DecodeJSON(t, rec, &response)
	assert.Empty(t, response.SemanticHash, "over-budget validation must not hash the graph")
	require.Len(t, response.Diagnostics, 2)
	assert.Equal(t, model.DiagnosticCodeNormalizedNodeLimit, response.Diagnostics[0].Code)
	assert.Equal(t, model.DiagnosticCodeNormalizedEdgeLimit, response.Diagnostics[1].Code)
	for _, diagnostic := range response.Diagnostics {
		assert.Equal(t, "template", diagnostic.Scope)
		assert.Empty(t, diagnostic.TargetID)
		assert.Less(t, len(diagnostic.Message), 160, "resource diagnostics stay bounded")
	}

	save := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/cardinality", body)
	require.Equal(t, http.StatusUnprocessableEntity, save.Code, save.Body.String())
	assert.Contains(t, save.Body.String(), `"code":"process_template_invalid"`)
	assert.Contains(t, save.Body.String(), `"code":"normalized_node_limit"`)
	assert.Contains(t, save.Body.String(), `"code":"normalized_edge_limit"`)
	list := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates", nil)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	assert.NotContains(t, list.Body.String(), `"id":"cardinality"`)
}

func TestProcessValidateRejectsHostileStructuredEdgeWireBeforeCanonicalization(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "edge-wire", Start: "target",
		Nodes: map[string]model.Node{"target": {Type: model.NodeTypeEnd}},
	}
	edges := []model.Edge{{From: "", Outcome: "start", To: "target"}}
	for sourceIndex := 0; len(edges) < model.MaxNormalizedEdges+1; sourceIndex++ {
		sourceID := fmt.Sprintf("source-%d", sourceIndex)
		tmpl.Nodes[sourceID] = model.Node{Type: model.NodeTypeDecision}
		for outcome := 0; outcome < model.MaxNormalizedDegree && len(edges) < model.MaxNormalizedEdges+1; outcome++ {
			edges = append(edges, model.Edge{From: sourceID, Outcome: fmt.Sprintf("outcome-%04d", outcome), To: "target"})
		}
	}
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/validate", processEditResponse{
		Template: tmpl, Edges: edges,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response struct {
		Diagnostics []processEditDiag `json:"diagnostics"`
	}
	testharness.DecodeJSON(t, rec, &response)
	require.Len(t, response.Diagnostics, 1)
	assert.Equal(t, model.DiagnosticCodeNormalizedEdgeLimit, response.Diagnostics[0].Code)
}

func overBudgetProcessTemplate(id string) *model.Template {
	nodes := make(map[string]model.Node, model.MaxNormalizedNodes+1)
	for index := 0; index < model.MaxNormalizedNodes+1; index++ {
		nodes[fmt.Sprintf("node-%04d", index)] = model.Node{Type: model.NodeTypeEnd}
	}
	first := nodes["node-0000"]
	first.Next = make(model.Next, model.MaxNormalizedEdges)
	for index := 0; index < model.MaxNormalizedEdges; index++ {
		first.Next[fmt.Sprintf("edge-%04d", index)] = "node-0000"
	}
	nodes["node-0000"] = first
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "node-0000", Nodes: nodes,
	}
}

func semanticDiagnosticFloodProcessTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "semantic-budget", Start: "source",
		Nodes: map[string]model.Node{
			"source": {
				Type: model.NodeTypeTask,
				Performer: &model.Performer{Kind: model.PerformerAgent,
					Prompt: strings.Repeat("{{ params.missing }}", 100_000)},
				Next: model.Next{"pass": "target"},
			},
			"target": {Type: model.NodeTypeEnd},
		},
	}
}

// TestProcessValidateSurfacesSection8aDiagnostics posts a deliberately broken
// edit model and pins one endpoint-level diagnostic per §8a class the live
// editor badges: unreachable nodes, missing/ambiguous outcome edges,
// undeclared param refs, and budget-less retry loops. The task node under
// test has a DOTTED id, so it also pins the longest-prefix targetId
// anchoring (a naive first-dot split would emit "work").
func TestProcessValidateSurfacesSection8aDiagnostics(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	tmpl := &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "broken-fixture",
		Start:      "begin",
		Nodes: map[string]model.Node{
			"begin": {Type: model.NodeTypeStart, Next: model.Next{"pass": "work.impl"}},
			"work.impl": {
				Type:      model.NodeTypeTask,
				Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "Fix {{ params.issue }}"},
				Next:      model.Next{"pass": "gone", "done": "implement"},
			},
			"island": {
				Type:      model.NodeTypeTask,
				Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "Never reached"},
				Next:      model.Next{"pass": "done"},
			},
			"implement": {
				Type:      model.NodeTypeTask,
				Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "Implement it"},
				Checks:    []model.Step{{ID: "tests", Performer: model.Performer{Kind: model.PerformerProgram, Run: "go test ./..."}}},
				Next:      model.Next{"pass": "done", "fail": "escalate"},
			},
			"escalate": {
				Type:      model.NodeTypeDecision,
				Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Retries exhausted. Continue?"},
				Next:      model.Next{"retry": "implement", "cancel": "canceled"},
			},
			"done":     {Type: model.NodeTypeEnd, Result: "success"},
			"canceled": {Type: model.NodeTypeEnd, Result: "canceled"},
		},
	}
	body := processEditResponse{Template: tmpl, Edges: model.NormalizeEdges(tmpl)}
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/validate", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response struct {
		Diagnostics []processEditDiag `json:"diagnostics"`
	}
	testharness.DecodeJSON(t, rec, &response)

	requireDiag := func(want processEditDiag) {
		t.Helper()
		for _, diag := range response.Diagnostics {
			if diag.Code == want.Code && diag.Scope == want.Scope && diag.TargetID == want.TargetID && diag.Severity == want.Severity {
				return
			}
		}
		t.Errorf("missing diagnostic %+v in %+v", want, response.Diagnostics)
	}
	// §8a class 1: unreachable node.
	requireDiag(processEditDiag{Scope: "node", TargetID: "island", Severity: model.SeverityError, Code: "unreachable_node"})
	// §8a class 2: missing/ambiguous outcome edges, anchored on the dotted id.
	requireDiag(processEditDiag{Scope: "edge", TargetID: "work.impl:pass", Severity: model.SeverityError, Code: "unknown_target"})
	requireDiag(processEditDiag{Scope: "edge", TargetID: "work.impl:done", Severity: model.SeverityWarning, Code: "ambiguous_pass_edge"})
	// §8a class 3: undeclared param reference, node-scoped for badge anchoring.
	requireDiag(processEditDiag{Scope: "node", TargetID: "work.impl", Severity: model.SeverityError, Code: "undeclared_param_ref"})
	// §8a class 4: budget-less sanctioned retry loop.
	requireDiag(processEditDiag{Scope: "node", TargetID: "implement", Severity: model.SeverityWarning, Code: "retry_loop_without_budget"})
}

func TestProcessValidateRejectsDuplicateNormalizedEdges(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	tmpl := processRESTTemplate("duplicate-edge", "ambiguous graph", 10)
	edges := model.NormalizeEdges(tmpl)
	require.NotEmpty(t, edges)
	edges = append(edges, edges[0])
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/validate", processEditResponse{
		Template: semanticProcessTemplate(tmpl), Edges: edges, Layout: tmpl.Layout,
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"process_template_edit_model"`)
	assert.Contains(t, rec.Body.String(), "duplicate edge")
}

func TestDashboardProcessRESTRequiresDashboardAuth(t *testing.T) {
	processAuthoringFlow(t)
	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux)
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/v1/process/templates", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/v1/process/template-heads", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestProcessRuntimeHTTPRoutesAreAbsentWithoutEngine(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/process/runs"},
		{http.MethodPost, "/v1/process/runs"},
		{http.MethodGet, "/v1/process/runs/legacy"},
		{http.MethodPost, "/v1/process/runs/legacy/nodes/work/report"},
		{http.MethodGet, "/v1/process/worklist"},
	} {
		rec := processTemplateRequest(t, f, test.method, test.path, map[string]any{})
		assert.Equalf(t, http.StatusNotFound, rec.Code, "%s %s: %s", test.method, test.path, rec.Body.String())
	}
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

func processAliasedNextSource(nodeCount, outcomes int) []byte {
	var source strings.Builder
	source.WriteString("apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: legacy-large\nstart: n000\nnodes:\n")
	for nodeIndex := range nodeCount {
		fmt.Fprintf(&source, "  n%03d:\n    type: end\n    next: ", nodeIndex)
		if nodeIndex == 0 {
			source.WriteString("&shared\n")
			for outcome := range outcomes {
				fmt.Fprintf(&source, "      outcome-%03d: n000\n", outcome)
			}
		} else {
			source.WriteString("*shared\n")
		}
	}
	return []byte(source.String())
}

func processSchemaAliasSource(nodeCount int) []byte {
	var source strings.Builder
	source.WriteString("apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: schema-budget\nstart: n000\nnodes:\n")
	for nodeIndex := range nodeCount {
		fmt.Fprintf(&source, "  n%03d: ", nodeIndex)
		if nodeIndex > 0 {
			source.WriteString("*shared\n")
			continue
		}
		source.WriteString("&shared\n    type: task\n    performer: {kind: agent, prompt: work}\n    checks:\n")
		for check := range 4 {
			fmt.Fprintf(&source, "      - id: check-%d\n        performer: {kind: program, run: echo}\n        unknown<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<: value\n", check)
		}
	}
	return []byte(source.String())
}

func processDuplicateMetadataSource(duplicates int) []byte {
	var source strings.Builder
	source.WriteString("apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: duplicate-budget\nstart: n000\nnodes:\n  n000:\n    type: task\n    performer: {kind: agent, prompt: work}\n    metadata:\n")
	for range duplicates {
		source.WriteString("      repeated: value\n")
	}
	return []byte(source.String())
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
