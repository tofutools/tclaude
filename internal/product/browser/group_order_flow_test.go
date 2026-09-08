package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserGroupSiblingOrderPersistsWithoutChangingGroups(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	for _, id := range []string{"parent", "child_a", "child_b", "other"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": id, "name": id}, nil))
	}
	for _, id := range []string{"child_a", "child_b"} {
		require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/"+id+"/parent", map[string]any{"request_id": "nest_" + id, "parent_group_id": "parent", "expected_revision": 1}, nil))
	}
	var before, after struct {
		Groups []model.Group `json:"groups"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &before))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("summary", "^Group settings$").MustClick()
	order := `() => Array.from(document.querySelectorAll('#group-management [data-group-id=parent] > .group-children > article')).map(e=>e.dataset.groupId).join(',')`
	require.Equal(t, "child_a,child_b", page.MustEval(order).Str())
	page.MustElementR("#group-management [data-group-id=child_b] button", "^Move group earlier$").MustClick()
	page.MustElementR("#group-order-status", "preferences saved")
	require.Equal(t, "child_b,child_a", page.MustEval(order).Str())
	var prefs struct{ Preferences model.PresentationPreferences }
	require.NoError(t, operator.Call(ctx, "GET", "/v2/presentation", nil, &prefs))
	require.Equal(t, []model.GroupID{"child_b", "child_a", "other", "parent"}, prefs.Preferences.GroupOrder)
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &after))
	require.Equal(t, before, after)
	page.MustReload()
	page.MustElementR("#connection", "Updated ")
	require.Equal(t, "child_b,child_a", page.MustEval(order).Str())
	page.MustElementR("summary", "^Group settings$").MustClick()
	// A concurrent preference change must leave the stale order visibly unsaved.
	prefs.Preferences.MusicVolume = .42
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/presentation", map[string]any{"preferences": prefs.Preferences, "expected_revision": prefs.Preferences.Revision}, nil))
	page.MustElementR("#group-management [data-group-id=child_b] button", "^Move group later$").MustClick()
	page.MustElementR("#group-order-status", "Preferences not saved")
	page.MustElementR("#group-management button", "^Reload saved group order$").MustClick()
	page.MustElementR("#group-order-status", "preferences loaded")
	require.Equal(t, "child_b,child_a", page.MustEval(order).Str())

	page.MustElementR("#group-management [data-group-id=child_b] button", "^Move group later$").MustClick()
	page.MustElementR("#group-order-status", "preferences saved")
	require.Equal(t, "child_a,child_b", page.MustEval(order).Str())
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &after))
	require.Equal(t, before, after)
}
