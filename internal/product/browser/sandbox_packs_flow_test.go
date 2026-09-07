package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserSandboxDestinationPacksShowEntriesAndRetainBothPolarities(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("main:not([inert])")
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElementR("#configurations summary", "^Sandbox profiles$").MustClick()
	page.MustElementR("#sandbox-profiles button", "^New sandbox profile$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Sandbox profile name']").MustInput("Network bundles")
	page.MustElementR(".sandbox-editor summary", "^Network$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Network baseline']").MustSelect("deny")
	page.MustElementR(".sandbox-editor summary", "^Destination packs$").MustClick()
	page.MustElementR(".sandbox-editor summary", "^Anthropic API · net-anthropic$").MustClick()
	page.MustElementR(".sandbox-editor pre", "api.anthropic.com")
	page.MustElement(".sandbox-editor [aria-label='Pack mode net-anthropic']").MustSelect("allow")
	page.MustElementR(".sandbox-editor summary", "^Local access · net-local$").MustClick()
	page.MustElement(".sandbox-editor [aria-label='Pack mode net-local']").MustSelect("deny")
	page.MustElementR(".sandbox-editor button", "^Save sandbox profile$").MustClick()
	page.MustWait(`()=>!document.querySelector('.sandbox-editor')`)
	var catalog []model.SandboxProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles", nil, &catalog))
	require.Len(t, catalog, 1)
	var read app.SandboxProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles/"+string(catalog[0].ID), nil, &read))
	require.Equal(t, []string{"net-anthropic"}, read.Revision.Policy.Network.Packs)
	require.Equal(t, []string{"net-local"}, read.Revision.Policy.Network.DenyPacks)
	page.MustElementR("#sandbox-profiles button", "^Edit sandbox profile$").MustClick()
	page.MustElementR(".sandbox-editor summary", "^Network$").MustClick()
	page.MustElementR(".sandbox-editor summary", "^Destination packs$").MustClick()
	page.MustElementR(".sandbox-editor summary", "^Anthropic API · net-anthropic$").MustClick()
	require.Equal(t, "allow", page.MustElement(".sandbox-editor [aria-label='Pack mode net-anthropic']").MustProperty("value").Str())
	page.MustElement(".sandbox-editor [aria-label='Pack mode net-anthropic']").MustSelect("deny")
	require.Empty(t, page.MustElement(".sandbox-editor [aria-label='Allowed pack IDs (one per line)']").MustProperty("value").Str())
	page.MustElement(".sandbox-editor [aria-label='Pack mode net-anthropic']").MustSelect("off")
	require.Equal(t, "net-local", page.MustElement(".sandbox-editor [aria-label='Denied pack IDs (one per line)']").MustProperty("value").Str())
	page.MustElement(".sandbox-editor [aria-label='Allowed pack IDs (one per line)']").MustInput("net-typo")
	page.MustElementR(".sandbox-editor button", "^Save sandbox profile$").MustClick()
	page.MustElementR(".sandbox-editor [role=alert]", "Unknown or stale destination pack: net-typo")
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles/"+string(catalog[0].ID), nil, &read))
	require.Equal(t, model.Revision(1), read.Profile.Revision)
	page.MustElement(".sandbox-editor [aria-label='Allowed pack IDs (one per line)']").MustSelectAllText().MustInput("net-anthropic\nnet-anthropic")
	page.MustElementR(".sandbox-editor button", "^Save sandbox profile$").MustClick()
	page.MustElementR(".sandbox-editor [role=alert]", "Duplicate or conflicting destination pack")
	page.MustElement(".sandbox-editor [aria-label='Pack mode net-anthropic']").MustSelect("deny")
	page.MustElementR(".sandbox-editor button", "^Save sandbox profile$").MustClick()
	page.MustWait(`()=>!document.querySelector('.sandbox-editor')`)
	read = app.SandboxProfileResult{}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/sandbox-profiles/"+string(catalog[0].ID), nil, &read))
	require.Empty(t, read.Revision.Policy.Network.Packs)
	require.Equal(t, []string{"net-local", "net-anthropic"}, read.Revision.Policy.Network.DenyPacks)
}
