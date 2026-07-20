package agentd_test

// Flow tests for the template editor's REST round trip (TCL-296). The editor
// UI is JS (unit-tested in jstest/process-edit-model.test.mjs); these tests
// exercise the exact wire flow that UI performs — GET edit model, mutate it
// the way the editor's model does (nodes map + normalized edges + layout
// pins), POST, and assert through the templates REST surface that the new
// version's canonical YAML carries the edit. Plus the 409 conflict flow that
// feeds the editor's explicit conflict dialog.

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestProcessEditorSaveRoundTripsNodeEdgeJoinAndPin(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	_, err = fs.PutTemplate(t.Context(), processRESTTemplate("editor-roundtrip", "seeded", 40))
	require.NoError(t, err)

	getRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/editor-roundtrip", nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var edit processEditResponse
	testharness.DecodeJSON(t, getRec, &edit)

	// The editor's add-node + draw-edge + join + drop-pin ops, expressed on the
	// wire model: a new node in the nodes map, normalized edges only (never
	// node.next), a join marker on the fan-in target, and a pinned position.
	edit.Template.Nodes["review"] = model.Node{Type: model.NodeTypeTask, Name: "Cold review"}
	done := edit.Template.Nodes["done"]
	done.Join = model.JoinAll
	edit.Template.Nodes["done"] = done
	edit.Edges = append(edit.Edges,
		model.Edge{From: "begin", Outcome: "fail", To: "review"},
		model.Edge{From: "review", Outcome: "pass", To: "done"},
	)
	require.NotNil(t, edit.Layout)
	edit.Layout.Nodes["review"] = model.LayoutNode{X: 420, Y: 260}

	saveRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/editor-roundtrip", edit)
	require.Equal(t, http.StatusCreated, saveRec.Code, saveRec.Body.String())

	// Assert through the REST read path, not store internals: the new head's
	// edit model and canonical YAML both carry the editor's changes.
	reload := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/editor-roundtrip", nil)
	require.Equal(t, http.StatusOK, reload.Code, reload.Body.String())
	var next processEditResponse
	testharness.DecodeJSON(t, reload, &next)
	require.Contains(t, next.Template.Nodes, "review")
	assert.Equal(t, "Cold review", next.Template.Nodes["review"].Name)
	// The server rebuilt next: from the edges array (the editor never writes
	// node.next itself) — the reloaded view shows the rebuilt semantic truth.
	assert.Equal(t, model.Next{"pass": "done"}, next.Template.Nodes["review"].Next)
	assert.Contains(t, next.Edges, model.Edge{From: "begin", Outcome: "fail", To: "review"})
	assert.Contains(t, next.Edges, model.Edge{From: "review", Outcome: "pass", To: "done"})
	assert.Equal(t, model.LayoutNode{X: 420, Y: 260}, next.Layout.Nodes["review"])
	assert.Equal(t, model.JoinAll, next.Template.Nodes["done"].Join)
	assert.NotContains(t, next.Template.Nodes["done"].Metadata, "join")

	assert.Contains(t, next.Source, "review:", "canonical YAML names the new node")
	assert.Contains(t, next.Source, "fail: review", "canonical YAML carries the new edge as next")
	assert.Contains(t, next.Source, "join: all", "canonical YAML persists the join marker")
	assert.NotEqual(t, edit.SemanticHash, next.SemanticHash, "adding a node is a semantic change")
}

func TestProcessEditorRenameDisplayNameCreatesVersionAndUpdatesReadSurfaces(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	original, err := fs.PutTemplate(t.Context(), processRESTTemplate("rename-display", "before rename", 40))
	require.NoError(t, err)

	// Given the versioned edit view an editor opened.
	getRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/rename-display", nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var edit processEditResponse
	testharness.DecodeJSON(t, getRec, &edit)
	opened := edit
	opened.Template = semanticProcessTemplate(edit.Template)

	// When the display name is edited and saved through the normal CAS path.
	edit.Template.Name = "Renamed release train"
	saveRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/rename-display", edit)
	require.Equal(t, http.StatusCreated, saveRec.Code, saveRec.Body.String())
	var saved struct {
		Ref          string `json:"ref"`
		SemanticHash string `json:"semanticHash"`
		SourceHash   string `json:"sourceHash"`
	}
	testharness.DecodeJSON(t, saveRec, &saved)
	assert.NotEqual(t, original.Ref, saved.Ref)
	assert.NotEqual(t, edit.SemanticHash, saved.SemanticHash, "display name is semantic template content")
	assert.NotEqual(t, edit.SourceHash, saved.SourceHash)

	// Then the Templates list and editor-header edit data both expose the new
	// name, and the canonical YAML persists it as the second version.
	listRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates", nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	var list struct {
		Templates []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			VersionCount int    `json:"versionCount"`
		} `json:"templates"`
	}
	testharness.DecodeJSON(t, listRec, &list)
	require.Len(t, list.Templates, 1)
	assert.Equal(t, "rename-display", list.Templates[0].ID)
	assert.Equal(t, "Renamed release train", list.Templates[0].Name)
	assert.Equal(t, 2, list.Templates[0].VersionCount)

	reloadRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/rename-display", nil)
	require.Equal(t, http.StatusOK, reloadRec.Code, reloadRec.Body.String())
	var reloaded processEditResponse
	testharness.DecodeJSON(t, reloadRec, &reloaded)
	assert.Equal(t, "Renamed release train", reloaded.Template.Name)
	assert.Contains(t, reloaded.Source, "name: Renamed release train")

	// The content edit does not bypass CAS: the view opened before the rename
	// is stale and still receives the editor's established 409 contract.
	opened.Template.Name = "Stale rename"
	conflictRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/rename-display", opened)
	assert.Equal(t, http.StatusConflict, conflictRec.Code, conflictRec.Body.String())
	assert.Contains(t, conflictRec.Body.String(), `"code":"process_template_conflict"`)
}

func TestProcessEditorConflictDialogDataAndBothResolutions(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	_, err = fs.PutTemplate(t.Context(), processRESTTemplate("editor-conflict", "opened in editor", 30))
	require.NoError(t, err)

	getRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/editor-conflict", nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var mine processEditResponse
	testharness.DecodeJSON(t, getRec, &mine)

	// Another editor saves first: the head moves under us.
	theirs := processRESTTemplate("editor-conflict", "their concurrent save", 90)
	theirRecord, err := fs.PutTemplateEditorSource(t.Context(), theirs, mine.SourceHash)
	require.NoError(t, err)

	// Our stale save must surface the explicit conflict dialog's data — the
	// error, code, and the current head identifiers — never silently overwrite.
	mine.Template.Description = "my local edit"
	staleRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/editor-conflict", mine)
	require.Equal(t, http.StatusConflict, staleRec.Code, staleRec.Body.String())
	var conflict struct {
		Code              string `json:"code"`
		Error             string `json:"error"`
		CurrentSourceHash string `json:"currentSourceHash"`
		CurrentRef        string `json:"currentRef"`
	}
	testharness.DecodeJSON(t, staleRec, &conflict)
	assert.Equal(t, "process_template_conflict", conflict.Code)
	assert.NotEmpty(t, conflict.Error)
	assert.Equal(t, theirRecord.Ref, conflict.CurrentRef)
	assert.NotEmpty(t, conflict.CurrentSourceHash)
	assert.NotEqual(t, mine.SourceHash, conflict.CurrentSourceHash)

	// Dialog path 1 — "reload theirs": a fresh GET serves their head.
	reload := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/editor-conflict", nil)
	require.Equal(t, http.StatusOK, reload.Code, reload.Body.String())
	var reloaded processEditResponse
	testharness.DecodeJSON(t, reload, &reloaded)
	assert.Equal(t, "their concurrent save", reloaded.Template.Description)
	assert.Equal(t, conflict.CurrentSourceHash, reloaded.SourceHash)

	// Dialog path 2 — "save as new version anyway": rebase the CAS base onto
	// their head and re-POST; our content becomes the new head on top of theirs.
	mine.SourceHash = conflict.CurrentSourceHash
	forceRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/editor-conflict", mine)
	require.Equal(t, http.StatusCreated, forceRec.Code, forceRec.Body.String())
	after := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/editor-conflict", nil)
	require.Equal(t, http.StatusOK, after.Code, after.Body.String())
	var head processEditResponse
	testharness.DecodeJSON(t, after, &head)
	assert.Equal(t, "my local edit", head.Template.Description)
}

// TestProcessEditorBlankFirstSaveUsesEmptyBaseHash covers the editor's blank
// scaffold: the first save of a brand-new id CASes against an empty head, and
// the same save against an id that already exists is the conflict the editor
// presents as "template id already exists".
func TestProcessEditorBlankFirstSaveUsesEmptyBaseHash(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)

	blank := processEditResponse{
		Template: semanticProcessTemplate(processRESTTemplate("brand-new", "from blank scaffold", 10)),
		Edges: []model.Edge{
			{From: "", Outcome: "start", To: "begin"},
			{From: "begin", Outcome: "pass", To: "done"},
		},
		Layout: &model.Layout{Nodes: map[string]model.LayoutNode{"begin": {X: 120, Y: 90}}},
	}
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/brand-new", blank)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	_, err = fs.GetTemplateHead(t.Context(), "brand-new")
	require.NoError(t, err)

	// The same blank-scaffold save against an existing id is a 409, not an
	// overwrite — the editor turns this into its id-already-exists dialog.
	dup := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/brand-new", blank)
	assert.Equal(t, http.StatusConflict, dup.Code, dup.Body.String())
	assert.Contains(t, dup.Body.String(), `"code":"process_template_conflict"`)
}

// The dashboard's list-row rename dialog does not send the editor's full edit
// view. It sends exactly the four decodable keys, reusing the head's edges and
// layout verbatim and changing only the display name. This asserts that shape
// preserves everything the rename is not supposed to touch — the graph, the
// declared params, and above all the editor layout, which is authoring state
// the operator would otherwise silently lose by renaming.
func TestProcessListRenameDialogBodyPreservesLayoutGraphAndParams(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	seed := processRESTTemplate("rename-preserves", "before rename", 40)
	seed.Params = map[string]model.Param{
		"release": {Type: "string", Description: "Release to ship", Required: new(true)},
	}
	_, err = fs.PutTemplate(t.Context(), seed)
	require.NoError(t, err)

	getRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/rename-preserves", nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var head processEditResponse
	testharness.DecodeJSON(t, getRec, &head)
	require.NotNil(t, head.Layout, "the seeded layout must reach the dialog to be preserved")

	// Exactly the body processes-actions.js submitRename builds.
	renamed := *head.Template
	renamed.Name = "Renamed from the list"
	body := map[string]any{
		"template": &renamed, "edges": head.Edges, "layout": head.Layout, "sourceHash": head.SourceHash,
	}
	saveRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/rename-preserves", body)
	require.Equal(t, http.StatusCreated, saveRec.Code, saveRec.Body.String())

	reloadRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/rename-preserves", nil)
	require.Equal(t, http.StatusOK, reloadRec.Code, reloadRec.Body.String())
	var reloaded processEditResponse
	testharness.DecodeJSON(t, reloadRec, &reloaded)

	assert.Equal(t, "Renamed from the list", reloaded.Template.Name)
	assert.Equal(t, "rename-preserves", reloaded.Template.ID, "rename never moves the store key")
	assert.Equal(t, head.Layout, reloaded.Layout, "editor layout survives a rename")
	assert.Equal(t, head.Edges, reloaded.Edges, "the graph survives a rename")
	assert.Equal(t, head.Template.Nodes, reloaded.Template.Nodes)
	assert.Equal(t, head.Template.Params, reloaded.Template.Params, "declared params survive a rename")
	assert.Equal(t, head.Template.Description, reloaded.Template.Description)
	assert.Equal(t, head.Template.Doc, reloaded.Template.Doc)
	assert.Equal(t, head.Template.Start, reloaded.Template.Start)

	// Clearing the name is a real edit, not a no-op, and still keeps the id.
	cleared := *reloaded.Template
	cleared.Name = ""
	clearRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/rename-preserves", map[string]any{
		"template": &cleared, "edges": reloaded.Edges, "layout": reloaded.Layout, "sourceHash": reloaded.SourceHash,
	})
	require.Equal(t, http.StatusCreated, clearRec.Code, clearRec.Body.String())
	listRec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates", nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	var list struct {
		Templates []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"templates"`
	}
	testharness.DecodeJSON(t, listRec, &list)
	require.Len(t, list.Templates, 1)
	assert.Equal(t, "rename-preserves", list.Templates[0].ID)
	assert.Empty(t, list.Templates[0].Name, "a cleared name falls back to the id in the list")
}

// Template ids are generated by the store side, never chosen by the operator:
// an id is a permanent key embedded in every ref, whereas the display name is
// renameable. This asserts the creation contract end to end.
func TestProcessTemplateCreateGeneratesIDAndRejectsCallerSuppliedOnes(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)

	scaffold := processRESTTemplate("placeholder", "created from the dashboard", 40)
	scaffold.ID = ""
	scaffold.Name = "Release Train"
	createRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates", map[string]any{
		"template": scaffold, "layout": scaffold.Layout,
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created struct {
		ID  string `json:"id"`
		Ref string `json:"ref"`
	}
	testharness.DecodeJSON(t, createRec, &created)

	assert.Regexp(t, `^[0-9a-f]{32}$`, created.ID, "ids are compact lowercase hex UUIDs")
	assert.NoError(t, store.ValidateTemplateID(created.ID), "a generated id must satisfy the store grammar")
	assert.True(t, strings.HasPrefix(created.Ref, created.ID+"@sha256:"))

	// The generated id is what the read surfaces key on, and the name the
	// operator chose is preserved.
	stored, err := fs.GetTemplate(t.Context(), created.Ref)
	require.NoError(t, err)
	assert.Equal(t, created.ID, stored.ID)
	assert.Equal(t, "Release Train", stored.Name)

	// Two creations never collide, even with identical content.
	secondRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates", map[string]any{
		"template": scaffold, "layout": scaffold.Layout,
	})
	require.Equal(t, http.StatusCreated, secondRec.Code, secondRec.Body.String())
	var second struct {
		ID string `json:"id"`
	}
	testharness.DecodeJSON(t, secondRec, &second)
	assert.NotEqual(t, created.ID, second.ID,
		"identical content must still produce distinct templates, unlike the old hand-typed default id")

	// A caller-supplied id is refused rather than silently honoured, so the
	// generated-id contract cannot be bypassed from the dashboard path.
	withID := processRESTTemplate("hand-picked", "should be refused", 40)
	rejectRec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates", map[string]any{
		"template": withID, "layout": withID.Layout,
	})
	assert.Equal(t, http.StatusBadRequest, rejectRec.Code, rejectRec.Body.String())
	assert.Contains(t, rejectRec.Body.String(), "generated")
}

func TestDashboardProcessTemplateCreateReplaysGeneratedIDForSameBrowserAttempt(t *testing.T) {
	_, root := processEngineFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dash := agentd.BuildDashboardHandlerForTest()
	scaffold := processRESTTemplate("placeholder", "created from the dashboard", 40)
	scaffold.ID = ""
	scaffold.Name = "Release Train"
	body := map[string]any{"template": scaffold, "layout": scaffold.Layout}
	const path = "/v1/process/templates"
	const attemptID = "11111111-2222-4333-8444-555555555555"
	request := func() *http.Request {
		req := testharness.JSONRequest(t, http.MethodPost, path, body)
		encoded, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		req.Body = io.NopCloser(bytes.NewReader(encoded))
		digest := sha256.Sum256([]byte(http.MethodPost + "\x00" + path + "\x00" + string(encoded)))
		req.Header.Set(agent.IdempotencyKeyHeader, attemptID)
		req.Header.Set(agent.RequestDigestHeader, fmt.Sprintf("%x", digest))
		return req
	}

	first := testharness.Serve(dash, request())
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	second := testharness.Serve(dash, request())
	require.Equal(t, http.StatusCreated, second.Code, second.Body.String())
	assert.Equal(t, first.Body.String(), second.Body.String(), "a retry replays the original generated id")
	assert.Equal(t, "true", second.Header().Get("X-Tclaude-Idempotent-Replay"))

	fs, err := store.NewFS(root)
	require.NoError(t, err)
	records, err := fs.ListTemplates(t.Context())
	require.NoError(t, err)
	assert.Len(t, records, 1, "one logical browser create persists one generated template")
}
