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
	page.MustElementR(".sandbox-editor label", "^Allow pack net-anthropic$").MustElement("input").MustClick()
	page.MustElementR(".sandbox-editor summary", "^Local access · net-local$").MustClick()
	page.MustElementR(".sandbox-editor label", "^Deny pack net-local$").MustElement("input").MustClick()
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
	require.True(t, page.MustElementR(".sandbox-editor label", "^Allow pack net-anthropic$").MustElement("input").MustProperty("checked").Bool())
}
