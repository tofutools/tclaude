package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserGroupDetailsAreInertAndPreserveConflictingEdits(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "group", "name": "Group"}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Edit group details$").MustClick()
	page.MustElement("#editor [name=description]").MustInput("<img src=x onerror=alert(1)> Description")
	page.MustElement("#editor [name=mission]").MustInput("Descriptive mission")
	page.MustElement("#editor [name=url]").MustInput("javascript:alert(1)")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor-error", "Some inputs are invalid")
	page.MustElement("#editor [name=url]").MustSelectAllText().MustInput("https://example.org/task")
	page.MustElement("#editor [name=label]").MustInput("Task link")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#group-management p", "Description")
	require.False(t, page.MustHas("#group-management img"))
	require.Contains(t, page.MustElement("#roster").MustText(), "<img src=x onerror=alert(1)> Description")
	link := page.MustElementR("#group-management a", "^Task link$")
	require.Equal(t, "https://example.org/task", *link.MustAttribute("href"))
	require.Equal(t, "noopener noreferrer", *link.MustAttribute("rel"))
	page.MustReload()
	page.MustElementR("#connection", "Updated ")
	require.Contains(t, page.MustElement("#roster").MustText(), "Descriptive mission")
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Edit group details$").MustClick()
	page.MustElement("#editor [name=mission]").MustSelectAllText().MustInput("Unsaved mission")
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/group", map[string]any{"name": "Renamed", "expected_revision": 2, "members": []string{}}, nil))
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor-error", "saved state changed")
	require.Equal(t, "Unsaved mission", page.MustElement("#editor [name=mission]").MustProperty("value").Str())
	page.MustElement("#editor button[value=cancel]").MustClick()
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#group-management h3", "^Renamed$")
	page.MustElementR("#group-management button", "^Edit group details$").MustClick()
	for _, name := range []string{"description", "mission", "url", "label"} {
		page.MustElement("#editor [name=" + name + "]").MustSelectAllText().MustInput("")
	}
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var snapshot struct {
		Groups     []model.Group     `json:"groups"`
		Executions []model.Execution `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Nil(t, snapshot.Groups[0].Details)
	require.Equal(t, "Renamed", snapshot.Groups[0].Name)
	require.Empty(t, snapshot.Executions)
}
