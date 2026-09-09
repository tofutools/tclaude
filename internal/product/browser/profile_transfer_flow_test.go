package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserConfigurationTransferPreviewsRenamesAndRetries(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	desired := model.DesiredConfiguration{Harness: "claude", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "original", "id": "original", "revision_id": "one", "name": "Original", "aliases": []string{"reviewer"}, "desired": desired}, nil))
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElementR("#configuration-list button", "^Export configurations$").MustClick()
	page.MustElement("dialog[aria-label='Export configurations'] input[type=checkbox]")
	// Capture the real downloaded bundle without filesystem effects in the browser fixture.
	page.MustEval(`() => {window.originalCreateURL=URL.createObjectURL; URL.createObjectURL=blob=>{window.transferExport=blob.text();return window.originalCreateURL(blob)}}`)
	page.MustElementR("dialog[aria-label='Export configurations'] button", "^Export$").MustClick()
	page.MustWait(`() => !document.querySelector('dialog[aria-label="Export configurations"]')`)
	raw := page.MustEval(`async () => await window.transferExport`).Str()
	var bundle app.ConfigurationBundle
	require.NoError(t, json.Unmarshal([]byte(raw), &bundle))
	require.Len(t, bundle.Profiles, 1)
	require.Equal(t, desired, bundle.Profiles[0].Desired)
	require.Equal(t, []string{"reviewer"}, bundle.Profiles[0].Aliases)
	bundle.Profiles[0].Archived = true
	bundle.Profiles[0].Startup = &model.ProfileStartup{InitialMessage: "Retained brief"}
	bytes, err := json.Marshal(bundle)
	require.NoError(t, err)
	page.MustElementR("#configuration-list button", "^Import configurations$").MustClick()
	page.MustElement("[aria-label='Configuration bundle']").MustInput(string(bytes))
	page.MustElementR("dialog[aria-label='Import configurations'] button", "^Preview$").MustClick()
	page.MustElement("[aria-label='Import name for Original']").MustSelectAllText().MustInput("Imported copy")
	require.Equal(t, "reviewer", page.MustElement("[aria-label='Import aliases for Original']").MustProperty("value").String())
	page.MustElement("[aria-label='Import aliases for Original']").MustSelectAllText().MustInput("")
	page.MustEval(`() => {const original=window.fetch;let lost=false;window.fetch=async(...args)=>{const response=await original(...args);if(String(args[0]).endsWith('/configuration-transfer/import')&&!lost){lost=true;throw Error('lost response')}return response}}`)
	page.MustElementR("dialog[aria-label='Import configurations'] button", "^Import selected$").MustClick()
	page.MustElementR("dialog [role=alert]", "lost response")
	page.MustElementR("dialog[aria-label='Import configurations'] button", "^Import selected$").MustClick()
	page.MustWait(`() => !document.querySelector('dialog[aria-label="Import configurations"]')`)
	var profiles []model.ConfigurationProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles", nil, &profiles))
	require.Len(t, profiles, 2)
	page.MustElement("[aria-label='Configuration status']").MustSelect("Archived")
	page.MustElementR("#configuration-list strong", "^Imported copy$")
	page.MustElementR("#configuration-list button", "^Inspect saved revision$").MustClick()
	page.MustElementR("#configuration-list pre", "Retained brief")
	// Reuse the exported file for an explicit exact-target replacement.
	bundle.Profiles[0].Archived = false
	bytes, err = json.Marshal(bundle)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "configurations.json")
	require.NoError(t, os.WriteFile(file, bytes, 0600))
	page.MustElementR("#configuration-list button", "^Import configurations$").MustClick()
	page.MustElement("dialog input[type=file]").MustSetFiles(file)
	page.MustWait(`() => document.querySelector('[aria-label="Configuration bundle"]').value.includes('Original')`)
	require.True(t, page.MustEval(`() => {const e=new Event('beforeunload',{cancelable:true});window.dispatchEvent(e);return e.defaultPrevented}`).Bool())
	page.MustElementR("dialog[aria-label='Import configurations'] button", "^Preview$").MustClick()
	page.MustElement("[aria-label='Import action for Original']").MustSelect("Replace exact configuration")
	page.MustElement("[aria-label='Replace target for Original']").MustSelect("Original · Enabled · original · revision 1")
	page.MustElement("[aria-label='Import name for Original']").MustSelectAllText().MustInput("Replaced original")
	page.MustElementR("dialog[aria-label='Import configurations'] button", "^Import selected$").MustClick()
	page.MustWait(`() => !document.querySelector('dialog[aria-label="Import configurations"]')`)
	var replaced app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/original", nil, &replaced))
	require.Equal(t, model.Revision(2), replaced.Profile.Revision)
	require.Equal(t, "Replaced original", replaced.Profile.Name)
	// A pending inspection cannot reopen the closed dialog after sign-out invalidation.
	page.MustElementR("#configuration-list button", "^Import configurations$").MustClick()
	page.MustElement("[aria-label='Configuration bundle']").MustInput(string(bytes))
	page.MustEval(`() => {const original=window.fetch;window.fetch=(...args)=>String(args[0]).endsWith('/configuration-transfer/inspect')?new Promise(resolve=>{window.finishTransfer=()=>original(...args).then(resolve)}):original(...args)}`)
	page.MustElementR("dialog[aria-label='Import configurations'] button", "^Preview$").MustClick()
	page.MustWait(`() => !!window.finishTransfer`)
	page.MustEval(`() => {document.dispatchEvent(new Event('workspace-signout'));window.finishTransfer()}`)
	page.MustWait(`() => !document.querySelector('.profile-transfer-dialog')`)
}
