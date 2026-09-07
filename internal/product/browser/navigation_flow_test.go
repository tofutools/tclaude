package browser

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestBrowserNavigationURLsHistoryAndCommandPicker(t *testing.T) {
	_, page, _ := processEditorBrowser(t)
	page.MustElement("[data-tab=messages]").MustClick()
	page.MustWait(`() => new URL(location.href).searchParams.get('tab')==='messages'`)
	page.MustElement("[data-tab=workspaces]").MustClick()
	page.MustEval(`() => history.back()`)
	page.MustWait(`() => !document.querySelector('#messages').hidden`)
	require.Contains(t, page.MustInfo().URL, "tab=messages")
	page.MustEval(`() => history.forward()`)
	page.MustWait(`() => !document.querySelector('#workspaces').hidden`)
	page.MustReload()
	page.MustElementR("#connection", "^Updated ")
	page.MustWait(`() => !document.querySelector('#workspaces').hidden && !document.querySelector('main').inert`)
	page.MustEval(`() => document.dispatchEvent(new KeyboardEvent('keydown',{code:'KeyK',key:'k',ctrlKey:true,bubbles:true}))`)
	page.MustElement("#command-picker[open] input").MustInput("New group")
	page.MustElementR("#command-picker button", "^New group$").MustClick()
	page.MustElementR("#editor-title", "^New group$")
	require.False(t, page.MustElement("#command-picker").MustProperty("open").Bool())
	page.MustElement("#cancel").MustClick()
	page.MustElement("#open-commands").MustClick()
	page.MustElement("#command-picker input").MustInput("History")
	page.MustElementR("#command-picker button", "^History$").MustClick()
	page.MustWait(`() => !document.querySelector('#history').hidden`)
	require.Contains(t, page.MustInfo().URL, "tab=history")
	page.MustEval(`() => {const url=new URL(location.href);url.searchParams.set('tab','not-a-workspace');history.replaceState(null,'',url)}`)
	page.MustReload()
	page.MustElementR("#connection", "^Updated ")
	page.MustWait(`() => !document.querySelector('#groups').hidden && new URL(location.href).searchParams.get('tab')==='groups'`)
}
