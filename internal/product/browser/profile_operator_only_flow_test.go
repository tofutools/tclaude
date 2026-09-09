package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"testing"
)

func TestBrowserProfileOperatorOnlyEditReopenAndClear(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "save", "id": "profile", "revision_id": "one", "name": "Restricted worker", "options": map[string]any{}}, nil))
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustWait(`() => {const edit=[...document.querySelectorAll('#configuration-list button')].find(b=>b.dataset.uiText==='Edit configuration');if(!edit)return false;edit.click();return true}`)
	checkbox := page.MustElement("#editor [name=operator_only]")
	require.Equal(t, "checkbox", checkbox.MustProperty("type").String())
	require.False(t, checkbox.MustProperty("required").Bool())
	checkbox.MustClick()
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open && !submitting`)
	page.MustElementR("#configuration-list p", "Operator-only creation")
	var saved app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/profile", nil, &saved))
	require.True(t, saved.Profile.OperatorOnly)
	page.MustReload()
	page.MustElementR("#connection", "^Updated")
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustWait(`() => {const edit=[...document.querySelectorAll('#configuration-list button')].find(b=>b.dataset.uiText==='Edit configuration');if(!edit)return false;edit.click();return true}`)
	checkbox = page.MustElement("#editor [name=operator_only]")
	require.True(t, checkbox.MustProperty("checked").Bool())
	checkbox.MustClick()
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open && !submitting`)
	saved = app.ConfigurationProfileResult{}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/profile", nil, &saved))
	require.False(t, saved.Profile.OperatorOnly)
}
