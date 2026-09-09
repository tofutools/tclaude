package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserConfigurationAliasesEditResolveAndClear(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "save", "id": "profile", "revision_id": "one", "name": "Primary", "desired": model.DesiredConfiguration{Harness: "claude", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}}, nil))
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustWait(`() => Array.from(document.querySelectorAll('#configuration-list button')).some(b=>b.textContent==='Edit configuration')`)
	page.MustEval(`() => Array.from(document.querySelectorAll('#configuration-list button')).find(b=>b.textContent==='Edit configuration').click()`)
	page.MustElement("#editor [name=aliases]").MustInput("reviewer\nbackup reviewer")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open && !submitting`)
	page.MustElementR("#configuration-list p", "Aliases: reviewer, backup reviewer")
	var resolved app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/resolve/reviewer", nil, &resolved))
	require.Equal(t, model.ConfigurationProfileID("profile"), resolved.Profile.ID)
	page.MustReload()
	page.MustElementR("#connection", "^Updated")
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustWait(`() => Array.from(document.querySelectorAll('#configuration-list button')).some(b=>b.textContent==='Edit configuration')`)
	page.MustEval(`() => Array.from(document.querySelectorAll('#configuration-list button')).find(b=>b.textContent==='Edit configuration').click()`)
	require.Equal(t, "reviewer\nbackup reviewer", page.MustElement("#editor [name=aliases]").MustProperty("value").String())
	page.MustElement("#editor [name=aliases]").MustSelectAllText().MustInput("")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open && !submitting`)
	require.Error(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/resolve/reviewer", nil, &resolved))
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/resolve/Primary", nil, &resolved))
}
