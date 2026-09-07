package browser

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserSandboxAuthoringPreservesLiteralRulesAndPinnedIncludes(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	var parent app.SandboxProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles", map[string]any{"request_id": "parent-create", "id": "sandbox_parent", "name": "Shared rules", "policy": model.SandboxPolicy{Environment: model.Environment{"PARENT": "first"}}}, &parent))
	files := t.TempDir()
	source := filepath.Join(files, "input.txt")
	marker := filepath.Join(files, "must-not-exist")
	require.NoError(t, os.WriteFile(source, []byte("content"), 0600))
	page.MustElement("main:not([inert])")
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElementR("#configurations summary", "^Sandbox profiles$").MustClick()
	page.MustElementR("#sandbox-profiles button", "^New sandbox profile$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Sandbox profile name']").MustInput("Isolated coding")
	page.MustElementR(".sandbox-editor summary", "^Included profiles").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Include sandbox profile']").MustSelect("Shared rules · sandbox_parent · policy " + string(parent.Revision.Ref.RevisionID))
	page.MustElementR(".sandbox-editor button", "^Add included revision$").MustClick()
	page.MustElementR(".sandbox-editor code", string(parent.Revision.Ref.RevisionID))
	page.MustElementR(".sandbox-editor summary", "^Filesystem").MustClick()
	page.MustElementR(".sandbox-editor button", "^Add filesystem rule$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Host path 1']").MustInput(source)
	page.MustElement(".sandbox-editor [aria-label='Expected kind 1']").MustSelect("file")
	page.MustElementR(".sandbox-editor summary", "^Environment and generated directories$").MustClick()
	page.MustElementR(".sandbox-editor button", "^Add environment variable$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Environment variable name 1']").MustInput("LITERAL")
	page.MustElement(".sandbox-editor [aria-label='Literal environment value 1']").MustInput("literal $(not-executed)\nnext")
	page.MustElement(".sandbox-editor [aria-label='Generated directory variable names (one per line)']").MustInput("CACHE_A\nCACHE_B")
	page.MustElementR(".sandbox-editor summary", "^Network$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Network baseline']").MustSelect("deny")
	page.MustElementR(".sandbox-editor button", "^Add allowed destination$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Host 1']").MustInput("example.com")
	page.MustElement(".sandbox-editor [aria-label='Ports (comma separated) 1']").MustInput("443,8443")
	page.MustElementR(".sandbox-editor summary", "^Pre-launch setup").MustClick()
	page.MustElementR(".sandbox-editor button", "^Add setup block$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Setup name 1']").MustInput("prepare")
	script := fmt.Sprintf("touch %q\nprintf 'literal'\n", marker)
	page.MustElement(".sandbox-editor [aria-label='Setup script 1']").MustInput(script)
	page.MustElement(".sandbox-editor [aria-label='Export names (one per line) 1']").MustInput("PATH\nTOOLS")
	page.MustElementR(".sandbox-editor button", "^Inspect host paths$").MustClick()
	page.MustElementR("[aria-label='Sandbox path preview']", "available")
	page.MustElementR(".sandbox-editor summary", "^Combined include policy$").MustClick()
	page.MustElementR(".sandbox-editor code", "^PARENT$")
	page.MustElementR(".sandbox-editor pre", "^first$")

	require.NoFileExists(t, marker)
	page.MustEval(`()=>{const original=window.fetch.bind(window);window.fetch=async(...args)=>{const result=await original(...args);if(String(args[0]).endsWith('/v2/sandbox-profiles')&&args[1]?.method==='POST'){window.fetch=original;throw new Error('simulated sandbox response loss')}return result}}`)
	page.MustElementR(".sandbox-editor button", "^Save sandbox profile$").MustClick()
	page.MustElementR(".sandbox-editor [role=alert]", "simulated sandbox response loss")
	page.MustElementR(".sandbox-editor button", "^Save sandbox profile$").MustClick()
	page.MustWait(`()=>!document.querySelector('.sandbox-editor')`)
	var profiles []model.SandboxProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles", nil, &profiles))
	require.Len(t, profiles, 2)
	var id model.SandboxProfileID
	for _, profile := range profiles {
		if profile.Name == "Isolated coding" {
			id = profile.ID
			require.Equal(t, model.Revision(1), profile.Revision)
		}
	}
	require.NotEmpty(t, id)
	var saved app.SandboxProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles/"+string(id), nil, &saved))
	require.Equal(t, script, saved.Revision.Policy.PreLaunch[0].Script)
	require.Equal(t, []string{"PATH", "TOOLS"}, saved.Revision.Policy.PreLaunch[0].Exports)
	require.Equal(t, []string{"CACHE_A", "CACHE_B"}, saved.Revision.Policy.AgentDirectories)
	require.Equal(t, "literal $(not-executed)\nnext", saved.Revision.Policy.Environment["LITERAL"])
	require.Equal(t, []uint16{443, 8443}, saved.Revision.Policy.Network.Allow[0].Ports)
	require.Equal(t, []model.SandboxProfileRef{parent.Revision.Ref}, saved.Revision.Policy.Includes)
	page.MustReload().MustWaitLoad()
	page.MustElement("main:not([inert])")
	page.MustElementR("#configurations summary", "^Sandbox profiles$").MustClick()
	page.MustElementR("#sandbox-profiles article", "Isolated coding").MustElementR("button", "^Edit sandbox profile$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Sandbox profile name']").MustSelectAllText().MustInput("Local unsaved edit")
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles", map[string]any{"request_id": "overlap", "id": id, "expected_revision": 1, "name": "Other edit", "policy": saved.Revision.Policy}, nil))
	page.MustElementR(".sandbox-editor button", "^Save sandbox profile$").MustClick()
	page.MustElementR(".sandbox-editor [role=alert]", "The saved state changed")
	require.Equal(t, "Local unsaved edit", page.MustElement(".sandbox-editor [aria-label='Sandbox profile name']").MustProperty("value").Str())
	require.NoFileExists(t, marker)
}

func TestBrowserSandboxCopyArchiveAndDraftDiscardRemainOffline(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	var source app.SandboxProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles", map[string]any{"request_id": "source", "id": "sandbox_source", "name": "Source", "policy": model.SandboxPolicy{Environment: model.Environment{"VALUE": "source"}, Resources: model.SandboxResources{Memory: "1GiB", CPU: ".5"}}}, &source))
	page.MustElement("main:not([inert])")
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElementR("#configurations summary", "^Sandbox profiles$").MustClick()
	page.MustElementR("#sandbox-profiles button", "^Copy sandbox profile$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Sandbox profile name']").MustSelectAllText().MustInput("Independent copy")
	page.MustElementR(".sandbox-editor summary", "^Environment and generated directories$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Literal environment value 1']").MustSelectAllText().MustInput("copy")
	page.MustElementR(".sandbox-editor button", "^Save sandbox profile$").MustClick()
	page.MustWait(`()=>!document.querySelector('.sandbox-editor')`)
	var profiles []model.SandboxProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles", nil, &profiles))
	require.Len(t, profiles, 2)
	var copied model.SandboxProfile
	for _, profile := range profiles {
		if profile.Name == "Independent copy" {
			copied = profile
		}
	}
	require.NotEmpty(t, copied.ID)
	var original app.SandboxProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles/sandbox_source", nil, &original))
	require.Equal(t, source.Revision, original.Revision)
	page.MustElementR("#sandbox-profiles article", "Independent copy").MustElementR("button", "^Archive sandbox profile$").MustClick()
	page.MustElement("#editor [name=confirm]").MustInput(string(copied.ID))
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting`)
	page.MustElement("#sandbox-profiles [aria-label='Sandbox profile status']").MustSelect("archived")
	page.MustElementR("#sandbox-profiles article", "Independent copy").MustElementR("button", "^Restore sandbox profile$").MustClick()
	page.MustElement("#editor [name=confirm]").MustInput(string(copied.ID))
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting`)
	page.MustElement("#sandbox-profiles [aria-label='Sandbox profile status']").MustSelect("active")
	page.MustElementR("#sandbox-profiles article", "Independent copy").MustElementR("button", "^Edit sandbox profile$").MustClick()
	page.MustElementR(".sandbox-editor p", "Policy revision 1 · Lifecycle revision 3")
	page.MustElementR("#sandbox-profiles article", "Independent copy").MustElementR("p", "Policy revision "+string(copied.HeadRevisionID))
	page.MustElementR(".sandbox-editor summary", "^Temporary filesystems").MustClick()
	page.MustElementR(".sandbox-editor button", "^Add tmpfs mount$").MustClick()
	// A button-only row addition must participate in discard protection.
	page.MustEval(`()=>{window.sandboxDiscardPrompts=0;window.confirm=()=>{window.sandboxDiscardPrompts++;return false}}`)
	page.MustElementR(".sandbox-editor button", "^Cancel$").MustClick()
	require.Equal(t, 1, page.MustEval(`()=>window.sandboxDiscardPrompts`).Int())
	require.True(t, page.MustEval(`()=>!!document.querySelector('.sandbox-editor')`).Bool())
	page.MustEval(`()=>window.confirm=()=>true`)
	page.MustElementR(".sandbox-editor button", "^Cancel$").MustClick()
	page.MustWait(`()=>!document.querySelector('.sandbox-editor')`)
	var read app.SandboxProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles/"+string(copied.ID), nil, &read))
	require.Equal(t, copied.HeadRevisionID, read.Revision.Ref.RevisionID)
	require.Equal(t, model.Revision(1), read.Revision.Number)
	require.Equal(t, model.Revision(3), read.Profile.Revision)
	require.Empty(t, read.Revision.Policy.Tmpfs)
	require.Equal(t, "copy", read.Revision.Policy.Environment["VALUE"])
	require.Equal(t, source.Revision.Policy.Resources, read.Revision.Policy.Resources)
	var snapshot struct {
		Executions []any `json:"executions"`
		WorkRuns   []any `json:"work_runs"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Executions)
	require.Empty(t, snapshot.WorkRuns)
}

func TestBrowserSandboxLatePreviewCannotReopenSignedOutDraft(t *testing.T) {
	_, page, _ := processEditorBrowser(t)
	page.MustElement("main:not([inert])")
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElementR("#configurations summary", "^Sandbox profiles$").MustClick()
	page.MustElementR("#sandbox-profiles button", "^New sandbox profile$").MustClick()
	page.MustEval(`()=>{const original=window.fetch.bind(window);window.fetch=async(...args)=>{const response=await original(...args);if(String(args[0]).endsWith('/v2/sandbox-profiles/preview')){window.fetch=original;window.previewReceived=true;await new Promise(resolve=>window.releasePreview=resolve)}return response}}`)
	page.MustElementR(".sandbox-editor button", "^Inspect host paths$").MustClick()
	page.MustWait(`()=>window.previewReceived===true`)
	page.MustEval(`()=>document.dispatchEvent(new Event('workspace-signout'))`)
	page.MustWait(`()=>!document.querySelector('.sandbox-editor')`)
	page.MustEval(`()=>window.releasePreview()`)
	page.MustEval(`()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)))`)
	require.False(t, page.MustHas(".sandbox-editor"))
	require.False(t, page.MustHas("[aria-label='Sandbox path preview']"))
}
