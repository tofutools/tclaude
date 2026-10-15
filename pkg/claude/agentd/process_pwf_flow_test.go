package agentd_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/pwf"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const pwfChildSource = "apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: pwf-review\nname: Review\nstart: s\n" +
	"params:\n  target:\n    type: string\n" +
	"nodes:\n  s:\n    type: start\n    next:\n      pass: review\n" +
	"  review:\n    type: task\n    performer:\n      kind: agent\n      profile: reviewer\n      prompt: Review it\n    next:\n      pass: done\n" +
	"  done:\n    type: end\n"

const pwfRootSource = "apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: pwf-ship\nname: Ship\nstart: begin\n" +
	"params:\n  issue:\n    type: string\n    required: true\n" +
	"nodes:\n  begin:\n    type: start\n    next:\n      pass: build\n" +
	"  build:\n    type: task\n    performer:\n      kind: agent\n      profile: dev\n      prompt: Build it\n    next:\n      pass: review\n" +
	"  review:\n    type: subprocess\n    subprocess:\n      templateRef: pwf-review\n      inputs:\n        target: \"{{ params.issue }}\"\n    next:\n      pass: merge\n" +
	"  merge:\n    type: task\n    performer:\n      kind: program\n      profile: git\n      run: gh\n      args: [pr, merge]\n    next:\n      pass: done\n" +
	"  done:\n    type: end\n"

func pwfRaw(t *testing.T, f *testharness.Flow, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", pwf.MediaType)
	return testharness.Serve(f.Mux, agentd.AsHumanPeer(req))
}

type pwfImportResponse struct {
	Title     string
	Root      string
	Roles     []pwf.Use
	Programs  []pwf.Program
	Updates   int
	Applied   bool
	Templates []struct {
		Ref, ID, Status, CurrentRef string
		Head                        bool
	}
}

// A workflow exported from one store carries its subprocess template, and
// importing it previews what changes before anything is written. A file
// edited after export is refused, and a template that already exists with
// different content only moves with replace.
func TestProcessWorkflowExportImportRoundTrip(t *testing.T) {
	f, root := processAuthoringFlow(t)
	for id, source := range map[string]string{"pwf-review": pwfChildSource, "pwf-ship": pwfRootSource} {
		rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/"+id, map[string]any{"source": source})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}

	exp := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-ship/export?title=Ship%20a%20feature", nil)
	require.Equal(t, http.StatusOK, exp.Code, exp.Body.String())
	assert.Equal(t, pwf.MediaType, exp.Header().Get("Content-Type"))
	file := exp.Body.Bytes()
	archive, err := pwf.Open(bytes.NewReader(file), int64(len(file)), paf.Limits{})
	require.NoError(t, err)
	assert.Equal(t, "Ship a feature", archive.Workflow.Title)
	require.Len(t, archive.Order, 2, "the referenced subprocess template travels with the root")
	assert.NotNil(t, archive.Head("pwf-review"))

	// Importing into the store it came from changes nothing.
	prev := pwfRaw(t, f, http.MethodPost, "/v1/process/import", file)
	require.Equal(t, http.StatusOK, prev.Code, prev.Body.String())
	var view pwfImportResponse
	testharness.DecodeJSON(t, prev, &view)
	assert.False(t, view.Applied)
	assert.Equal(t, []pwf.Program{{Step: "pwf-ship:merge", Profile: "git", Run: "gh", Args: []string{"pr", "merge"}}}, view.Programs,
		"the review lists every command the workflow would run")
	assert.Equal(t, []string{"dev", "reviewer"}, []string{view.Roles[0].Name, view.Roles[1].Name})
	for _, row := range view.Templates {
		assert.Equal(t, "identical", row.Status, row.ID)
	}

	// A fresh node: both templates are new, and preview writes nothing.
	require.NoError(t, os.RemoveAll(root))
	prev = pwfRaw(t, f, http.MethodPost, "/v1/process/import", file)
	require.Equal(t, http.StatusOK, prev.Code, prev.Body.String())
	testharness.DecodeJSON(t, prev, &view)
	for _, row := range view.Templates {
		assert.Equal(t, "new", row.Status, row.ID)
	}
	gone := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/pwf-ship", nil)
	assert.Equal(t, http.StatusNotFound, gone.Code, "a preview never stores anything")

	applied := pwfRaw(t, f, http.MethodPost, "/v1/process/import?apply=1", file)
	require.Equal(t, http.StatusOK, applied.Code, applied.Body.String())
	testharness.DecodeJSON(t, applied, &view)
	assert.True(t, view.Applied)
	got := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/pwf-ship", nil)
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	runs := processTemplateRequest(t, f, http.MethodGet, "/v1/process/runs", nil)
	require.Equal(t, http.StatusOK, runs.Code, runs.Body.String())
	assert.NotContains(t, runs.Body.String(), "pwf-ship", "importing never starts a run")

	// The local child diverges; the import now needs replace to move it back.
	edited := pwfChildSource + "description: local edit\n"
	cur := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/pwf-review", nil)
	var head struct{ SourceHash string }
	testharness.DecodeJSON(t, cur, &head)
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/pwf-review", map[string]any{"source": edited, "sourceHash": head.SourceHash})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	conflict := pwfRaw(t, f, http.MethodPost, "/v1/process/import?apply=1", file)
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
	assert.Contains(t, conflict.Body.String(), "replace")
	replaced := pwfRaw(t, f, http.MethodPost, "/v1/process/import?apply=1&replace=1", file)
	require.Equal(t, http.StatusOK, replaced.Code, replaced.Body.String())
	testharness.DecodeJSON(t, replaced, &view)
	for _, row := range view.Templates {
		if row.ID == "pwf-review" {
			assert.Equal(t, "update", row.Status)
			assert.NotEmpty(t, row.CurrentRef)
		}
	}
	again := pwfRaw(t, f, http.MethodPost, "/v1/process/import", file)
	testharness.DecodeJSON(t, again, &view)
	assert.Zero(t, view.Updates, "after replace the imported versions are current")

	// A file changed after export is refused before anything is read into the store.
	tampered := bytes.Replace(file, []byte("pwf-ship"), []byte("pwf-shiq"), 1)
	bad := pwfRaw(t, f, http.MethodPost, "/v1/process/import", tampered)
	assert.Equal(t, http.StatusUnprocessableEntity, bad.Code, bad.Body.String())
}

func TestProcessWorkflowExportUnknownTemplate(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	rec := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/nope/export", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}
