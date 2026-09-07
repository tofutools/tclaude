package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

func TestBrowserSandboxTransferExportsExactGraphAndRetriesIndependentImport(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	var parent, child app.SandboxProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles", map[string]any{"request_id": "parent", "id": "parent", "name": "Parent", "policy": model.SandboxPolicy{Environment: model.Environment{"VALUE": "literal $(text)"}}}, &parent))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles", map[string]any{"request_id": "child", "id": "child", "name": "Child", "policy": model.SandboxPolicy{Includes: []model.SandboxProfileRef{parent.Revision.Ref}}}, &child))
	page.MustElement("main:not([inert])")
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElementR("#configurations summary", "^Sandbox profiles$").MustClick()
	page.MustEval(`()=>{const original=URL.createObjectURL;URL.createObjectURL=blob=>{if(blob.type==='application/json')window.sandboxExport=blob.text();return original(blob)}}`)
	page.MustElementR("#sandbox-profiles article", "Child").MustElementR("button", "^Export sandbox profile$").MustClick()
	page.MustWait(`()=>!!window.sandboxExport`)
	raw := page.MustEval(`async()=>await window.sandboxExport`).Str()
	var bundle sandboxpolicy.Bundle
	require.NoError(t, json.Unmarshal([]byte(raw), &bundle))
	require.Len(t, bundle.Entries, 2)
	require.Equal(t, child.Revision.Ref, bundle.Root)
	page.MustElementR("#sandbox-profiles button", "^Import sandbox profiles$").MustClick()
	page.MustElement("[aria-label='Sandbox bundle']").MustInput(raw)
	require.True(t, page.MustEval(`()=>{const event=new Event('beforeunload',{cancelable:true});window.dispatchEvent(event);return event.defaultPrevented}`).Bool())
	page.MustElementR(".sandbox-transfer button", "^Preview sandbox import$").MustClick()
	page.MustElement(".sandbox-transfer article input").MustSelectAllText().MustInput("Imported dependency")
	page.MustEval(`()=>{const original=window.fetch;let lost=false;window.fetch=async(...args)=>{const response=await original(...args);if(String(args[0]).endsWith('/sandbox-profiles/import')&&!lost){lost=true;throw Error('lost sandbox response')}return response}}`)
	page.MustElementR(".sandbox-transfer button", "^Import independent copies$").MustClick()
	page.MustElementR(".sandbox-transfer [role=alert]", "lost sandbox response")
	page.MustElementR(".sandbox-transfer button", "^Import independent copies$").MustClick()
	page.MustWait(`()=>!document.querySelector('.sandbox-transfer')`)
	var profiles []model.SandboxProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles", nil, &profiles))
	require.Len(t, profiles, 4)
	var copied app.SandboxProfileResult
	for _, profile := range profiles {
		if profile.Name == "Child copy" {
			require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles/"+string(profile.ID), nil, &copied))
		}
	}
	require.NotEmpty(t, copied.Profile.ID)
	require.NotEqual(t, parent.Revision.Ref, copied.Revision.Policy.Includes[0])
	var closure sandboxpolicy.Closure
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles/inspect", map[string]any{"ref": copied.Revision.Ref}, &closure))
	require.Equal(t, "literal $(text)", closure.Entries[0].Policy.Environment["VALUE"])
	page.MustElementR("#sandbox-profiles button", "^Import sandbox profiles$").MustClick()
	page.MustElement("[aria-label='Sandbox bundle']").MustInput(raw)
	page.MustElementR(".sandbox-transfer button", "^Preview sandbox import$").MustClick()
	page.MustElement(".sandbox-transfer article input")
	badFile := filepath.Join(t.TempDir(), "invalid-utf8.json")
	require.NoError(t, os.WriteFile(badFile, []byte{255}, 0600))
	page.MustElement(".sandbox-transfer input[type=file]").MustSetFiles(badFile)
	page.MustWait(`()=>!!document.querySelector('.sandbox-transfer [role=alert]').textContent`)
	require.True(t, page.MustElementR(".sandbox-transfer button", "^Import independent copies$").MustProperty("disabled").Bool())
	require.Empty(t, page.MustElement("[aria-label='Sandbox bundle']").MustProperty("value").Str())
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles", nil, &profiles))
	require.Len(t, profiles, 4)
	page.MustElement("[aria-label='Sandbox bundle']").MustInput(raw)
	page.MustEval(`()=>{const original=window.fetch;window.fetch=(...args)=>String(args[0]).endsWith('/sandbox-profiles/import/inspect')?new Promise(resolve=>{window.finishSandboxImport=()=>original(...args).then(resolve)}):original(...args)}`)
	page.MustElementR(".sandbox-transfer button", "^Preview sandbox import$").MustClick()
	page.MustWait(`()=>!!window.finishSandboxImport`)
	page.MustEval(`()=>{document.dispatchEvent(new Event('workspace-signout'));window.finishSandboxImport()}`)
	page.MustWait(`()=>!document.querySelector('.sandbox-transfer')`)
}
