package agentd_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processview "github.com/tofutools/tclaude/pkg/claude/process/view"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type processRunViewResponse = processview.Envelope

func TestProcessRunViewResolvesLegacyPinnedTemplateAndDoesNotMutateHistory(t *testing.T) {
	f, root := processEngineFlow(t)
	fsStore := createEngineRun(t, root, "viewer-legacy", viewerFlowTemplate("viewer-legacy"), false)
	at := time.Date(2026, 7, 14, 11, 0, 0, 0, time.UTC)
	_, err := fsStore.Append(t.Context(), "viewer-legacy", 0, []evidence.LogEntry{
		{
			SchemaVersion: evidence.LogEntrySchemaVersion, At: at,
			Scope: evidence.Scope{Kind: evidence.ScopeNode, ID: "work"}, Kind: evidence.EntryKindAttempt,
			Event: &state.Event{Type: state.EventNodeAttemptStarted, NodeID: "work", Attempt: 1, Actor: "agent:agt_viewer"},
		},
		{
			SchemaVersion: evidence.LogEntrySchemaVersion, At: at.Add(time.Second),
			Scope: evidence.Scope{Kind: evidence.ScopeNode, ID: "work"}, Kind: evidence.EntryKindAttempt,
			Event: &state.Event{Type: state.EventNodeAttemptSettled, NodeID: "work", Attempt: 1, Outcome: "pass", NodeStatus: state.NodeStatusCompleted, EvidenceRef: "artifact:work"},
		},
	})
	require.NoError(t, err)

	snapshot, err := fsStore.LoadRun(t.Context(), "viewer-legacy")
	require.NoError(t, err)
	snapshot.Run.Template = nil // pre-embedded-template legacy record
	snapshot.Run.Params = map[string]string{"token": "VIEWER_SECRET_PARAM"}
	runData, err := json.MarshalIndent(snapshot.Run, "", "  ")
	require.NoError(t, err)
	runData = append(runData, '\n')
	runPath := filepath.Join(root, "runs", "viewer-legacy", "run.json")
	require.NoError(t, os.WriteFile(runPath, runData, 0o644))

	runDir := filepath.Join(root, "runs", "viewer-legacy")
	oldTime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, filepath.WalkDir(runDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		return os.Chtimes(path, oldTime, oldTime)
	}))
	before := fingerprintTree(t, runDir)

	legacy := processEngineGet(t, f, "/v1/process/runs/viewer-legacy")
	require.Equal(t, http.StatusOK, legacy.Code, legacy.Body.String())
	assert.NotContains(t, legacy.Body.String(), `"report"`, "legacy endpoint contract remains unchanged")
	assert.Contains(t, legacy.Body.String(), `"state"`)

	rec := processEngineGet(t, f, "/v1/process/runs/viewer-legacy/view")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response processRunViewResponse
	testharness.DecodeJSON(t, rec, &response)
	assert.NotContains(t, rec.Body.String(), "VIEWER_SECRET_PARAM")
	assert.NotContains(t, rec.Body.String(), `"params"`)
	require.NotNil(t, response.Graph, "legacy runs must resolve the exact pinned template")
	assert.Equal(t, snapshot.Run.TemplateRef, response.Run.TemplateRef)
	assert.Equal(t, state.RunStatusRunning, response.Verification.EffectiveStatus)
	assert.Equal(t, processview.SchemaVersion, response.Report.SchemaVersion)
	assert.Equal(t, 1, response.Report.Nodes["work"].Summary.AttemptCount)
	require.Equal(t, []processview.TraversedEdge{{
		From: "work", Outcome: "pass", To: "done", Count: 1, LastAt: at.Add(time.Second),
	}}, response.Report.TraversedEdges)

	after := fingerprintTree(t, runDir)
	assert.Equal(t, before, after, "viewer GET must preserve file count, checksums, sizes, and mtimes for run.json, state, manifest, and logs")
}

func TestProcessRunViewPreservesStoreValidLongIdentifiers(t *testing.T) {
	f, root := processEngineFlow(t)
	longTemplateID := "viewer-" + strings.Repeat("t", 170)
	longNodeID := "node-" + strings.Repeat("n", 170)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: longTemplateID, Start: longNodeID,
		Nodes: map[string]model.Node{longNodeID: {Type: model.NodeTypeEnd}},
	}
	createEngineRun(t, root, "viewer-long-identifiers", tmpl, false)
	rec := processEngineGet(t, f, "/v1/process/runs/viewer-long-identifiers/view")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response processRunViewResponse
	testharness.DecodeJSON(t, rec, &response)
	require.NotNil(t, response.Graph)
	assert.Contains(t, response.Run.TemplateRef, longTemplateID+"@sha256:")
	require.Len(t, response.Graph.Nodes, 1)
	assert.Equal(t, longNodeID, response.Graph.Nodes[0].ID)
}

func TestProcessRunViewRejectsUnavailableOrMismatchedPinnedTemplates(t *testing.T) {
	t.Run("pending attributed save is not recovered", func(t *testing.T) {
		f, root := processEngineFlow(t)
		fsStore := createEngineRun(t, root, "viewer-template-pending", viewerFlowTemplate("viewer-template-pending"), false)
		snapshot, err := fsStore.LoadRun(t.Context(), "viewer-template-pending")
		require.NoError(t, err)
		snapshot.Run.Template = nil
		writeRunRecord(t, root, snapshot.Run)
		id, _, ok := strings.Cut(snapshot.Run.TemplateRef, "@sha256:")
		require.True(t, ok)
		intentPath := filepath.Join(root, "templates", id, ".attributed-save-intent.json")
		require.NoError(t, os.WriteFile(intentPath, []byte(`{"secret":"PENDING_INTENT_SECRET"}`), 0o600))
		templateDir := filepath.Join(root, "templates", id)
		runDir := filepath.Join(root, "runs", snapshot.Run.ID)
		beforeTemplate, beforeRun := fingerprintTree(t, templateDir), fingerprintTree(t, runDir)

		rec := processEngineGet(t, f, "/v1/process/runs/viewer-template-pending/view")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var response processRunViewResponse
		testharness.DecodeJSON(t, rec, &response)
		assert.Nil(t, response.Graph)
		assert.True(t, hasProcessDiagnostic(response.Verification, "template_unavailable"))
		assert.NotContains(t, rec.Body.String(), "PENDING_INTENT_SECRET")
		assert.Equal(t, beforeTemplate, fingerprintTree(t, templateDir))
		assert.Equal(t, beforeRun, fingerprintTree(t, runDir))
	})

	t.Run("legacy pinned version unavailable", func(t *testing.T) {
		f, root := processEngineFlow(t)
		fsStore := createEngineRun(t, root, "viewer-template-missing", viewerFlowTemplate("viewer-template-missing"), false)
		snapshot, err := fsStore.LoadRun(t.Context(), "viewer-template-missing")
		require.NoError(t, err)
		snapshot.Run.Template = nil
		writeRunRecord(t, root, snapshot.Run)
		removePinnedTemplateBody(t, root, snapshot.Run.TemplateRef)

		rec := processEngineGet(t, f, "/v1/process/runs/viewer-template-missing/view")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var response processRunViewResponse
		testharness.DecodeJSON(t, rec, &response)
		assert.Nil(t, response.Graph)
		assert.Equal(t, state.RunStatusInconsistent, response.Verification.EffectiveStatus)
		assert.True(t, hasProcessDiagnostic(response.Verification, "template_unavailable"))
		assert.Empty(t, response.Report.TraversedEdges)
	})

	t.Run("embedded template mismatch", func(t *testing.T) {
		f, root := processEngineFlow(t)
		fsStore := createEngineRun(t, root, "viewer-template-mismatch", viewerFlowTemplate("viewer-template-mismatch"), false)
		snapshot, err := fsStore.LoadRun(t.Context(), "viewer-template-mismatch")
		require.NoError(t, err)
		require.NotNil(t, snapshot.Run.Template)
		snapshot.Run.Template.Name = "tampered"
		writeRunRecord(t, root, snapshot.Run)

		rec := processEngineGet(t, f, "/v1/process/runs/viewer-template-mismatch/view")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var response processRunViewResponse
		testharness.DecodeJSON(t, rec, &response)
		assert.Nil(t, response.Graph)
		assert.Equal(t, state.RunStatusInconsistent, response.Verification.EffectiveStatus)
		assert.True(t, hasProcessDiagnostic(response.Verification, "embedded_template_mismatch"))
		assert.Empty(t, response.Report.TraversedEdges)
	})

	t.Run("legacy pinned content mismatch", func(t *testing.T) {
		f, root := processEngineFlow(t)
		fsStore := createEngineRun(t, root, "viewer-pinned-mismatch", viewerFlowTemplate("viewer-pinned-mismatch"), false)
		snapshot, err := fsStore.LoadRun(t.Context(), "viewer-pinned-mismatch")
		require.NoError(t, err)
		require.NotNil(t, snapshot.Run.Template)
		pinned := snapshot.Run.Template
		snapshot.Run.Template = nil
		writeRunRecord(t, root, snapshot.Run)
		pinned.Name = "content no longer matches its address"
		data, err := json.Marshal(pinned)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(pinnedTemplateBodyPath(t, root, snapshot.Run.TemplateRef), data, 0o644))

		rec := processEngineGet(t, f, "/v1/process/runs/viewer-pinned-mismatch/view")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var response processRunViewResponse
		testharness.DecodeJSON(t, rec, &response)
		assert.Nil(t, response.Graph)
		assert.Equal(t, state.RunStatusInconsistent, response.Verification.EffectiveStatus)
		assert.True(t, hasProcessDiagnostic(response.Verification, "pinned_template_mismatch"))
		assert.Empty(t, response.Report.TraversedEdges)
	})

	t.Run("legacy unsafe pinned ref is persisted mismatch", func(t *testing.T) {
		f, root := processEngineFlow(t)
		fsStore := createEngineRun(t, root, "viewer-unsafe-template-ref", viewerFlowTemplate("viewer-unsafe-template-ref"), false)
		snapshot, err := fsStore.LoadRun(t.Context(), "viewer-unsafe-template-ref")
		require.NoError(t, err)
		snapshot.Run.Template = nil
		snapshot.Run.TemplateRef = "../outside@sha256:" + strings.Repeat("a", 64)
		writeRunRecord(t, root, snapshot.Run)

		rec := processEngineGet(t, f, "/v1/process/runs/viewer-unsafe-template-ref/view")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var response processRunViewResponse
		testharness.DecodeJSON(t, rec, &response)
		assert.Nil(t, response.Graph)
		assert.Equal(t, state.RunStatusInconsistent, response.Verification.EffectiveStatus)
		assert.True(t, hasProcessDiagnostic(response.Verification, "pinned_template_mismatch"))
		assert.Empty(t, response.Report.TraversedEdges)
		_, statErr := os.Stat(filepath.Join(root, "outside.lock"))
		assert.ErrorIs(t, statErr, os.ErrNotExist)
	})
}

func TestProcessRunViewDegradesOnlyConfirmedExistingRuns(t *testing.T) {
	t.Run("mismatched run record id is confirmed by directory", func(t *testing.T) {
		f, root := processEngineFlow(t)
		fsStore := createEngineRun(t, root, "viewer-id-mismatch", viewerFlowTemplate("viewer-id-mismatch"), false)
		snapshot, err := fsStore.LoadRun(t.Context(), "viewer-id-mismatch")
		require.NoError(t, err)
		snapshot.Run.ID = "MISMATCHED_RECORD_SECRET"
		data, err := json.Marshal(snapshot.Run)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(root, "runs", "viewer-id-mismatch", "run.json"), data, 0o644))
		rec := processEngineGet(t, f, "/v1/process/runs/viewer-id-mismatch/view")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "snapshot_unreadable")
		assert.NotContains(t, rec.Body.String(), "MISMATCHED_RECORD_SECRET")
	})

	t.Run("corrupt state", func(t *testing.T) {
		f, root := processEngineFlow(t)
		createEngineRun(t, root, "viewer-corrupt", viewerFlowTemplate("viewer-corrupt"), false)
		secret := "SECRET-CORRUPT-BYTES"
		require.NoError(t, os.WriteFile(filepath.Join(root, "runs", "viewer-corrupt", "state.json"), []byte(`{"broken":"`+secret), 0o644))

		rec := processEngineGet(t, f, "/v1/process/runs/viewer-corrupt/view")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var response processRunViewResponse
		testharness.DecodeJSON(t, rec, &response)
		assert.Equal(t, "viewer-corrupt", response.Run.ID)
		assert.Nil(t, response.Graph)
		assert.Equal(t, state.RunStatusInconsistent, response.Verification.EffectiveStatus)
		require.Len(t, response.Verification.Diagnostics, 1)
		assert.Equal(t, "snapshot_unreadable", response.Verification.Diagnostics[0].Code)
		assert.Empty(t, response.Report.Nodes)
		assert.Empty(t, response.Report.TraversedEdges)
		assert.NotContains(t, rec.Body.String(), secret)
		assert.NotContains(t, rec.Body.String(), root)
	})

	t.Run("torn evidence", func(t *testing.T) {
		f, root := processEngineFlow(t)
		fsStore := createEngineRun(t, root, "viewer-torn", viewerFlowTemplate("viewer-torn"), false)
		at := time.Date(2026, 7, 14, 11, 30, 0, 0, time.UTC)
		_, err := fsStore.Append(t.Context(), "viewer-torn", 0, []evidence.LogEntry{{
			SchemaVersion: evidence.LogEntrySchemaVersion, At: at,
			Scope: evidence.Scope{Kind: evidence.ScopeNode, ID: "work"}, Kind: evidence.EntryKindAttempt,
			Event: &state.Event{Type: state.EventNodeAttemptStarted, NodeID: "work", Attempt: 1, Actor: "agent:agt_viewer"},
		}})
		require.NoError(t, err)
		logPath := filepath.Join(root, "runs", "viewer-torn", "nodes", "work", "log.jsonl")
		data, err := os.ReadFile(logPath)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(logPath, []byte(strings.TrimSuffix(string(data), "\n")), 0o644))

		rec := processEngineGet(t, f, "/v1/process/runs/viewer-torn/view")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var response processRunViewResponse
		testharness.DecodeJSON(t, rec, &response)
		require.Len(t, response.Verification.Diagnostics, 1)
		assert.Equal(t, "read_torn_tail", response.Verification.Diagnostics[0].Code)
		assert.NotContains(t, rec.Body.String(), root)
	})

	t.Run("missing run", func(t *testing.T) {
		f, _ := processEngineFlow(t)
		rec := processEngineGet(t, f, "/v1/process/runs/not-present/view")
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("encoded unsafe id has no lock side effect", func(t *testing.T) {
		f, root := processEngineFlow(t)
		rec := processEngineGet(t, f, "/v1/process/runs/%2e%2e%2foutside/view")
		assert.NotEqual(t, http.StatusOK, rec.Code)
		_, err := os.Stat(filepath.Join(root, "outside.lock"))
		assert.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("oversized regular component is sanitized", func(t *testing.T) {
		f, root := processEngineFlow(t)
		createEngineRun(t, root, "viewer-oversized", viewerFlowTemplate("viewer-oversized"), false)
		require.NoError(t, os.Truncate(filepath.Join(root, "runs", "viewer-oversized", "state.json"), (16<<20)+1))
		rec := processEngineGet(t, f, "/v1/process/runs/viewer-oversized/view")
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), root)
	})

	t.Run("store infrastructure failure", func(t *testing.T) {
		f, root := processEngineFlow(t)
		require.NoError(t, os.MkdirAll(root, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, "runs"), []byte("not a directory"), 0o644))
		rec := processEngineGet(t, f, "/v1/process/runs/unknown/view")
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), root)
	})

	t.Run("run component io failure", func(t *testing.T) {
		f, root := processEngineFlow(t)
		createEngineRun(t, root, "viewer-io", viewerFlowTemplate("viewer-io"), false)
		statePath := filepath.Join(root, "runs", "viewer-io", "state.json")
		require.NoError(t, os.Remove(statePath))
		require.NoError(t, os.Mkdir(statePath, 0o755))
		rec := processEngineGet(t, f, "/v1/process/runs/viewer-io/view")
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), root)
	})

	t.Run("run component symlink is rejected", func(t *testing.T) {
		f, root := processEngineFlow(t)
		createEngineRun(t, root, "viewer-component-link", viewerFlowTemplate("viewer-component-link"), false)
		target := filepath.Join(t.TempDir(), "TARGET_PATH_SECRET")
		require.NoError(t, os.WriteFile(target, []byte("TARGET_CONTENT_SECRET"), 0o644))
		statePath := filepath.Join(root, "runs", "viewer-component-link", "state.json")
		require.NoError(t, os.Remove(statePath))
		require.NoError(t, os.Symlink(target, statePath))
		rec := processEngineGet(t, f, "/v1/process/runs/viewer-component-link/view")
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), target)
		assert.NotContains(t, rec.Body.String(), "TARGET_CONTENT_SECRET")
	})

	t.Run("legacy template component symlink is infrastructure failure", func(t *testing.T) {
		f, root := processEngineFlow(t)
		fsStore := createEngineRun(t, root, "viewer-template-link", viewerFlowTemplate("viewer-template-link"), false)
		snapshot, err := fsStore.LoadRun(t.Context(), "viewer-template-link")
		require.NoError(t, err)
		snapshot.Run.Template = nil
		writeRunRecord(t, root, snapshot.Run)
		templatePath := pinnedTemplateBodyPath(t, root, snapshot.Run.TemplateRef)
		target := filepath.Join(t.TempDir(), "TEMPLATE_TARGET_SECRET")
		require.NoError(t, os.WriteFile(target, []byte(`{"secret":"TEMPLATE_CONTENT_SECRET"}`), 0o644))
		require.NoError(t, os.Remove(templatePath))
		require.NoError(t, os.Symlink(target, templatePath))
		rec := processEngineGet(t, f, "/v1/process/runs/viewer-template-link/view")
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), target)
		assert.NotContains(t, rec.Body.String(), "TEMPLATE_CONTENT_SECRET")
	})

	t.Run("run symlink is rejected", func(t *testing.T) {
		f, root := processEngineFlow(t)
		target := filepath.Join(t.TempDir(), "target")
		require.NoError(t, os.MkdirAll(target, 0o755))
		require.NoError(t, os.MkdirAll(filepath.Join(root, "runs"), 0o755))
		require.NoError(t, os.Symlink(target, filepath.Join(root, "runs", "viewer-link")))
		rec := processEngineGet(t, f, "/v1/process/runs/viewer-link/view")
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), root)
		assert.NotContains(t, rec.Body.String(), target)
	})
}

type fileFingerprint struct {
	Size    int64
	Mode    fs.FileMode
	ModTime int64
	SHA256  string
}

func fingerprintTree(t *testing.T, root string) map[string]fileFingerprint {
	t.Helper()
	result := map[string]fileFingerprint{}
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[filepath.ToSlash(rel)] = fileFingerprint{
			Size: info.Size(), Mode: info.Mode(), ModTime: info.ModTime().UnixNano(), SHA256: hex.EncodeToString(sum[:]),
		}
		return nil
	}))
	return result
}

func viewerFlowTemplate(id string) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "work",
		Nodes: map[string]model.Node{
			"work": {
				Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Profile: "dev", Prompt: "do work"},
				Next: model.Next{"pass": "done"},
			},
			"done": {Type: model.NodeTypeEnd},
		},
	}
}

func writeRunRecord(t *testing.T, root string, run store.RunRecord) {
	t.Helper()
	data, err := json.MarshalIndent(run, "", "  ")
	require.NoError(t, err)
	data = append(data, '\n')
	require.NoError(t, os.WriteFile(filepath.Join(root, "runs", run.ID, "run.json"), data, 0o644))
}

func removePinnedTemplateBody(t *testing.T, root, ref string) {
	t.Helper()
	require.NoError(t, os.Remove(pinnedTemplateBodyPath(t, root, ref)))
}

func pinnedTemplateBodyPath(t *testing.T, root, ref string) string {
	t.Helper()
	id, hash, ok := strings.Cut(ref, "@sha256:")
	require.True(t, ok)
	return filepath.Join(root, "templates", id, "sha256-"+hash, "template.json")
}

func hasProcessDiagnostic(report processview.Verification, code string) bool {
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
