package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"testing"
)

func TestBrowserRoleGuidanceAuthorsWithoutGrantingActions(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=access]").MustClick()
	page.MustElementR("#access-list button", "^Create role$").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Writer")
	page.MustElement("#editor [name=description]").MustInput("Documentation")
	page.MustElement("#editor [name=brief]").MustInput("Explain <literal> examples")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	var state app.AuthorityStateResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/authority", nil, &state))
	found := false
	for _, role := range state.Roles {
		if role.Name == "Writer" {
			found = true
			require.Empty(t, role.Actions)
			require.Equal(t, "Explain <literal> examples", role.Brief)
		}
	}
	require.True(t, found)
	page.MustReload()
	page.MustElementR("#connection", "^Updated ")
	page.MustElement("[data-tab=access]").MustClick()
	page.MustElementR("#access-list pre", "Explain <literal> examples")
}
