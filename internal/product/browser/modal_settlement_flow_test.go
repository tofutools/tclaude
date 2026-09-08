package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserModalWaitsForCommittedSaveRefresh(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "team", "name": "Team"}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management h3", "Team")
	page.MustElementR("#group-management button", "^Edit name and members$").MustClick()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("First save")
	page.MustEval(`()=>{const original=window.fetch.bind(window);window.fetch=async(...args)=>{const response=await original(...args);if(String(args[0]).endsWith('/v2/snapshot')){window.fetch=original;await new Promise(resolve=>window.releaseSavedSnapshot=resolve)}return response}}`)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`()=>typeof window.releaseSavedSnapshot==='function'`)
	require.True(t, page.MustEval(`()=>document.getElementById('editor').open`).Bool())
	require.True(t, page.MustElement("#cancel").MustProperty("disabled").Bool())
	require.True(t, page.MustElement("#editor button[type=submit]").MustProperty("disabled").Bool())
	require.True(t, page.MustEval(`()=>{const event=new Event('cancel',{cancelable:true});document.getElementById('editor').dispatchEvent(event);return event.defaultPrevented}`).Bool())
	var snapshot struct{ Groups []model.Group }
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Groups, 1)
	require.Equal(t, "First save", snapshot.Groups[0].Name)
	page.MustEval(`()=>window.releaseSavedSnapshot()`)
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#group-management button", "^Edit name and members$").MustClick()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Second save")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Equal(t, "Second save", snapshot.Groups[0].Name)
}
