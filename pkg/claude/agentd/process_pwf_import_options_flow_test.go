package agentd_test

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-sb56fp: an importer maps the workflow's roles onto local ones, imports
// a template whose id is taken here under a new id, and pins the imported
// root to a group. The choices ride on the preview and the apply alike, and
// the reviewDigest covers them.

type pwfImportOptionsView struct {
	pwfImportResponse
	RootID  string
	RoleMap []struct {
		Name   string
		Steps  []string
		Target string
		Mapped bool
		Known  bool
	}
	LocalRoles []string
	Group      string
	Pinned     bool
	Templates  []struct {
		Ref, ID, ImportRef, ImportID, Status, CurrentRef string
		Head                                             bool
	}
}

// pwfImportWith previews a file with the importer's choices and, when apply,
// applies exactly that review with the same choices.
func pwfImportWith(t *testing.T, f *testharness.Flow, file []byte, opts string, apply bool) (pwfImportOptionsView, int, string) {
	t.Helper()
	prev := pwfRaw(t, f, http.MethodPost, "/v1/process/import?"+opts, file)
	if prev.Code != http.StatusOK || !apply {
		var view pwfImportOptionsView
		if prev.Code == http.StatusOK {
			testharness.DecodeJSON(t, prev, &view)
		}
		return view, prev.Code, prev.Body.String()
	}
	var view pwfImportOptionsView
	testharness.DecodeJSON(t, prev, &view)
	rec := pwfRaw(t, f, http.MethodPost, "/v1/process/import?apply=1&review="+view.ReviewDigest+"&"+opts, file)
	var applied pwfImportOptionsView
	if rec.Code == http.StatusOK {
		testharness.DecodeJSON(t, rec, &applied)
	}
	return applied, rec.Code, rec.Body.String()
}

func pwfStoredTemplate(t *testing.T, f *testharness.Flow, id string) (*model.Template, string) {
	t.Helper()
	rec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/"+id, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view struct {
		Template   *model.Template `json:"template"`
		CurrentRef string          `json:"currentRef"`
	}
	testharness.DecodeJSON(t, rec, &view)
	return view.Template, view.CurrentRef
}

// A root that pins its child's version: mapping the child's role rewrites
// the child, so the root is re-pinned to the child's new hash.
func pwfPinnedWorkflow(t *testing.T, f *testharness.Flow) []byte {
	t.Helper()
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/pwf-review", map[string]any{"source": pwfChildSource})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var child struct{ SemanticHash string }
	testharness.DecodeJSON(t, rec, &child)
	root := "apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: pwf-pinned\nname: Pinned\nstart: s\n" +
		"nodes:\n  s:\n    type: start\n    next:\n      pass: build\n" +
		"  build:\n    type: task\n    performer:\n      kind: agent\n      profile: dev\n      prompt: Build it\n    next:\n      pass: review\n" +
		"  review:\n    type: subprocess\n    subprocess:\n      templateRef: pwf-review\n      version: sha256:" + child.SemanticHash +
		"\n      inputs:\n        target: x\n    next:\n      pass: done\n" +
		"  done:\n    type: end\n"
	rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/pwf-pinned", map[string]any{"source": root})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	exp := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-pinned/export", nil)
	require.Equal(t, http.StatusOK, exp.Code, exp.Body.String())
	return exp.Body.Bytes()
}

func TestProcessWorkflowImportMapsRoles(t *testing.T) {
	f, root := processAuthoringFlow(t)
	file := pwfPinnedWorkflow(t, f)
	require.NoError(t, os.RemoveAll(root))
	f.HaveGroup("webshop")
	f.HaveMemberWithRole("webshop", "qa00-1111-2222-3333-4444", "qa")

	// The review lists every role with what its steps would use; an
	// unmapped role keeps its name and is flagged when nothing here has it.
	view, code, body := pwfImportWith(t, f, file, "", false)
	require.Equal(t, http.StatusOK, code, body)
	require.Len(t, view.RoleMap, 2)
	assert.Equal(t, "dev", view.RoleMap[0].Name)
	assert.Equal(t, "reviewer", view.RoleMap[1].Name)
	assert.Equal(t, []string{"pwf-review:review"}, view.RoleMap[1].Steps)
	assert.False(t, view.RoleMap[1].Known, "no member has the reviewer role here")
	assert.Contains(t, view.LocalRoles, "qa")

	mapped, code, body := pwfImportWith(t, f, file, "map="+url.QueryEscape("reviewer=qa"), false)
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "qa", mapped.RoleMap[1].Target)
	assert.True(t, mapped.RoleMap[1].Mapped)
	assert.True(t, mapped.RoleMap[1].Known)
	assert.NotEqual(t, view.ReviewDigest, mapped.ReviewDigest, "the mapping is part of the review")
	for _, row := range mapped.Templates {
		assert.NotEmpty(t, row.ImportRef, "%s is rewritten: the child by the mapping, the root by re-pinning", row.ID)
		assert.Equal(t, row.ID, row.ImportID, "a mapping keeps ids")
	}

	// An apply with choices other than the reviewed ones is stale.
	stale := pwfRaw(t, f, http.MethodPost, "/v1/process/import?apply=1&review="+view.ReviewDigest+"&map=reviewer%3Dqa", file)
	assert.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())

	applied, code, body := pwfImportWith(t, f, file, "map="+url.QueryEscape("reviewer=qa"), true)
	require.Equal(t, http.StatusOK, code, body)
	require.True(t, applied.Applied)
	child, childRef := pwfStoredTemplate(t, f, "pwf-review")
	assert.Equal(t, "qa", child.Nodes["review"].Performer.Profile)
	parent, parentRef := pwfStoredTemplate(t, f, "pwf-pinned")
	assert.Equal(t, "dev", parent.Nodes["build"].Performer.Profile, "unmapped roles are kept")
	assert.Equal(t, childRef, "pwf-review@"+parent.Nodes["review"].Subprocess.Version, "the root pins the rewritten child")
	for _, row := range applied.Templates {
		want := map[string]string{"pwf-review": childRef, "pwf-pinned": parentRef}[row.ID]
		assert.Equal(t, want, row.ImportRef, "the stored ref is the reviewed one")
	}

	// Mapping a role the workflow does not use is refused.
	_, code, _ = pwfImportWith(t, f, file, "map="+url.QueryEscape("ops=qa"), false)
	assert.Equal(t, http.StatusBadRequest, code)
}

func TestProcessWorkflowImportRenamesOnCollisionAndPinsToAGroup(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	for id, source := range map[string]string{"pwf-review": pwfChildSource, "pwf-ship": pwfRootSource} {
		rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/"+id, map[string]any{"source": source})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}
	exp := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-ship/export", nil)
	require.Equal(t, http.StatusOK, exp.Code, exp.Body.String())
	file := exp.Body.Bytes()
	f.HaveGroup("webshop")

	// The local child diverges from the file's.
	cur := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/pwf-review", nil)
	var head struct{ SourceHash, CurrentRef string }
	testharness.DecodeJSON(t, cur, &head)
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/pwf-review",
		map[string]any{"source": pwfChildSource + "description: local edit\n", "sourceHash": head.SourceHash})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	_, localChild := pwfStoredTemplate(t, f, "pwf-review")

	view, code, body := pwfImportWith(t, f, file, "rename=1&group=webshop", false)
	require.Equal(t, http.StatusOK, code, body)
	assert.Zero(t, view.Updates, "renaming replaces nothing")
	ids := map[string]string{}
	for _, row := range view.Templates {
		ids[row.ID] = row.ImportID
		assert.Equal(t, "new", row.Status, row.ID)
	}
	// The root references the renamed child, so it differs from the local
	// root and is renamed too.
	assert.Equal(t, map[string]string{"pwf-review": "pwf-review-2", "pwf-ship": "pwf-ship-2"}, ids)
	assert.Equal(t, "pwf-ship-2", view.RootID)

	applied, code, body := pwfImportWith(t, f, file, "rename=1&group=webshop", true)
	require.Equal(t, http.StatusOK, code, body)
	assert.True(t, applied.Pinned)
	_, stillLocal := pwfStoredTemplate(t, f, "pwf-review")
	assert.Equal(t, localChild, stillLocal, "the local template is untouched")
	ship, _ := pwfStoredTemplate(t, f, "pwf-ship-2")
	assert.Equal(t, "pwf-review-2", ship.Nodes["review"].Subprocess.TemplateRef)

	ctx := processTemplateRequest(t, f, http.MethodGet, "/v1/process/context", nil)
	require.Equal(t, http.StatusOK, ctx.Code, ctx.Body.String())
	var context struct {
		Groups []struct {
			Group     string `json:"group"`
			Processes []struct {
				TemplateID string `json:"templateId"`
			} `json:"processes"`
		} `json:"groups"`
	}
	testharness.DecodeJSON(t, ctx, &context)
	var pinned []string
	for _, g := range context.Groups {
		if g.Group == "webshop" {
			for _, p := range g.Processes {
				pinned = append(pinned, p.TemplateID)
			}
		}
	}
	assert.Equal(t, []string{"pwf-ship-2"}, pinned)

	// A group the caller cannot pin to is refused at preview.
	_, code, body = pwfImportWith(t, f, file, "group=nope", false)
	assert.Equal(t, http.StatusNotFound, code, body)
	assert.True(t, strings.Contains(body, "no such group"))
}
