package agentd_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// pwfApply previews a file and applies exactly that review.
func pwfApply(t *testing.T, f *testharness.Flow, file []byte, extra string) *httptest.ResponseRecorder {
	t.Helper()
	prev := pwfRaw(t, f, http.MethodPost, "/v1/process/import", file)
	require.Equal(t, http.StatusOK, prev.Code, prev.Body.String())
	var view pwfImportResponse
	testharness.DecodeJSON(t, prev, &view)
	return pwfRaw(t, f, http.MethodPost, "/v1/process/import?apply=1&review="+view.ReviewDigest+extra, file)
}

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
	Applied      bool
	ReviewDigest string
	Templates    []struct {
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

	applied := pwfApply(t, f, file, "")
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
	conflict := pwfApply(t, f, file, "")
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
	assert.Contains(t, conflict.Body.String(), "replace")
	replaced := pwfApply(t, f, file, "&replace=1")
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

// A draft that fails validation, and points at a subprocess template nobody
// has written yet, still shares: the check names its problems, the export
// carries it as saved, and the importer's review lists the same problems
// before storing it.
func TestProcessWorkflowExportImportFailingValidation(t *testing.T) {
	f, root := processAuthoringFlow(t)
	draft := "apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: pwf-draft\nstart: s\n" +
		"nodes:\n  s:\n    type: start\n    next:\n      pass: work\n" +
		"  work:\n    type: task\n    next:\n      pass: later\n" +
		"  later:\n    type: subprocess\n    subprocess:\n      templateRef: pwf-unwritten\n    next:\n      pass: pinned\n" +
		"  pinned:\n    type: subprocess\n    subprocess:\n      templateRef: pwf-half-typed\n      version: v1\n    next:\n      pass: done\n" +
		"  done:\n    type: end\n"
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/pwf-draft", map[string]any{"source": draft})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	check := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-draft/export?check=1", nil)
	require.Equal(t, http.StatusOK, check.Code, check.Body.String())
	var checked struct{ Problems []string }
	testharness.DecodeJSON(t, check, &checked)
	require.NotEmpty(t, checked.Problems)
	assert.Contains(t, checked.Problems, `template "pwf-draft" references "pwf-unwritten", which the file does not carry`)
	assert.Contains(t, checked.Problems, `template "pwf-draft" references "pwf-half-typed@v1", which the file does not carry`,
		"a malformed pin is a problem, not an export failure")

	exp := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-draft/export", nil)
	require.Equal(t, http.StatusOK, exp.Code, exp.Body.String())
	file := exp.Body.Bytes()
	archive, err := pwf.Open(bytes.NewReader(file), int64(len(file)), paf.Limits{})
	require.NoError(t, err)
	assert.Equal(t, checked.Problems, archive.Problems, "the check reports what every reader of the file sees")

	require.NoError(t, os.RemoveAll(root))
	prev := pwfRaw(t, f, http.MethodPost, "/v1/process/import", file)
	require.Equal(t, http.StatusOK, prev.Code, prev.Body.String())
	var view struct{ Problems []string }
	testharness.DecodeJSON(t, prev, &view)
	assert.Equal(t, checked.Problems, view.Problems)
	applied := pwfApply(t, f, file, "")
	require.Equal(t, http.StatusOK, applied.Code, applied.Body.String())
	got := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/pwf-draft", nil)
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
}

// Export says which paths come from this machine before the file is shared,
// the file still exports, and the importer's review shows the same list.
func TestProcessWorkflowExportScansForMachinePaths(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	source := strings.Replace(pwfRootSource, "run: gh", "run: /home/alice/bin/gh", 1)
	put := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/pwf-ship", map[string]any{"source": source})
	require.Equal(t, http.StatusCreated, put.Code, put.Body.String())

	check := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-ship/export?check=1", nil)
	require.Equal(t, http.StatusOK, check.Code, check.Body.String())
	var checked struct{ Paths []pwf.PathFinding }
	testharness.DecodeJSON(t, check, &checked)
	want := []pwf.PathFinding{{Where: "pwf-ship: nodes.merge.performer.run", Path: "/home/alice/bin/gh", Kind: pwf.PathHome}}
	assert.Equal(t, want, checked.Paths)

	exported := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-ship/export", nil)
	require.Equal(t, http.StatusOK, exported.Code, exported.Body.String())
	prev := pwfRaw(t, f, http.MethodPost, "/v1/process/import", exported.Body.Bytes())
	require.Equal(t, http.StatusOK, prev.Code, prev.Body.String())
	var review struct{ Paths []pwf.PathFinding }
	testharness.DecodeJSON(t, prev, &review)
	assert.Equal(t, want, review.Paths)
}

func TestProcessWorkflowExportUnknownTemplate(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	rec := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/nope/export", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// A file carrying a pinned older version and the head of the same template
// imports cleanly onto a node that has neither, with the head current.
func TestProcessWorkflowImportPinnedAndHeadOnAFreshNode(t *testing.T) {
	f, root := processAuthoringFlow(t)
	save := func(id, source, sourceHash string) string {
		rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/"+id, map[string]any{"source": source, "sourceHash": sourceHash})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var out struct{ Ref, SemanticHash, SourceHash string }
		testharness.DecodeJSON(t, rec, &out)
		return out.SemanticHash + " " + out.SourceHash
	}
	v1 := save("pwf-review", pwfChildSource, "")
	var v1Hash, v1Source string
	_, _ = fmt.Sscan(v1, &v1Hash, &v1Source)
	save("pwf-review", pwfChildSource+"description: second\n", v1Source)
	pinned := "apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: pwf-both\nstart: s\n" +
		"nodes:\n  s:\n    type: start\n    next:\n      pass: a\n" +
		"  a:\n    type: subprocess\n    subprocess:\n      templateRef: pwf-review\n      version: sha256:" + v1Hash + "\n      inputs:\n        target: x\n    next:\n      pass: b\n" +
		"  b:\n    type: subprocess\n    subprocess:\n      templateRef: pwf-review\n      inputs:\n        target: y\n    next:\n      pass: done\n" +
		"  done:\n    type: end\n"
	save("pwf-both", pinned, "")
	exp := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-both/export", nil)
	require.Equal(t, http.StatusOK, exp.Code, exp.Body.String())
	file := exp.Body.Bytes()
	headBefore := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/pwf-review", nil)
	var want struct{ CurrentRef string }
	testharness.DecodeJSON(t, headBefore, &want)

	require.NoError(t, os.RemoveAll(root))
	applied := pwfApply(t, f, file, "")
	require.Equal(t, http.StatusOK, applied.Code, applied.Body.String())
	headAfter := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/pwf-review", nil)
	var got struct{ CurrentRef string }
	testharness.DecodeJSON(t, headAfter, &got)
	assert.Equal(t, want.CurrentRef, got.CurrentRef, "the imported head is current, not the pinned older version")
}

// An apply acts only on the review it names: a local save after the review
// makes it stale, so nothing unreviewed is replaced.
func TestProcessWorkflowImportRefusesAStaleReview(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/pwf-review", map[string]any{"source": pwfChildSource})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	exp := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-review/export", nil)
	require.Equal(t, http.StatusOK, exp.Code, exp.Body.String())
	file := exp.Body.Bytes()

	prev := pwfRaw(t, f, http.MethodPost, "/v1/process/import", file)
	var view pwfImportResponse
	testharness.DecodeJSON(t, prev, &view)
	cur := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/pwf-review", nil)
	var head struct{ SourceHash string }
	testharness.DecodeJSON(t, cur, &head)
	rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/pwf-review",
		map[string]any{"source": pwfChildSource + "description: edited after the review\n", "sourceHash": head.SourceHash})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	stale := pwfRaw(t, f, http.MethodPost, "/v1/process/import?apply=1&replace=1&review="+view.ReviewDigest, file)
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	assert.Contains(t, stale.Body.String(), "process_import_review_stale")
	missing := pwfRaw(t, f, http.MethodPost, "/v1/process/import?apply=1&replace=1", file)
	assert.Equal(t, http.StatusConflict, missing.Code, "an apply without a review is refused")
}
