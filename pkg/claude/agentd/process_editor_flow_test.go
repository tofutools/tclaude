package agentd_test

// Flow tests for the template editor's REST round trip (TCL-296). The editor
// UI is JS (unit-tested in jstest/process-edit-model.test.mjs); these tests
// exercise the exact wire flow that UI performs — GET edit model, mutate it
// the way the editor's model does (nodes map + normalized edges + layout
// pins), POST, and assert through the templates REST surface that the new
// version's canonical YAML carries the edit. Plus the 409 conflict flow that
// feeds the editor's explicit conflict dialog.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	done.Metadata = model.Metadata{"join": "all"}
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
	assert.Equal(t, "all", next.Template.Nodes["done"].Metadata["join"])

	assert.Contains(t, next.Source, "review:", "canonical YAML names the new node")
	assert.Contains(t, next.Source, "fail: review", "canonical YAML carries the new edge as next")
	assert.Contains(t, next.Source, "join: all", "canonical YAML persists the join marker")
	assert.NotEqual(t, edit.SemanticHash, next.SemanticHash, "adding a node is a semantic change")
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
