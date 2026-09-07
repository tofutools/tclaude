package browser

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserDirectoryChoiceOnlyFillsExplicitWorkingDirectory(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page = page.Timeout(40 * time.Second)
	root := t.TempDir()
	child := filepath.Join(root, "project")
	require.NoError(t, os.Mkdir(child, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "private.txt"), []byte("not displayed"), 0600))
	canonical, err := filepath.EvalSymlinks(child)
	require.NoError(t, err)
	var listing model.DirectoryListing
	require.NoError(t, operator.Call(ctx, "GET", "/v2/directories?path="+url.QueryEscape(root), nil, &listing))
	require.Len(t, listing.Directories, 1)
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElement("#new-configuration").MustClick()
	page.MustElement("#editor [name=cwd]").MustInput(root)
	page.MustElementR("#editor button", "^Browse directories$").MustClick()
	page.MustElementR("#directory-picker .directory-list button", "^project$").MustClick()
	page.MustElementR("#directory-picker button", "^Use this directory$").MustClick()
	page.MustWait(`() => !document.querySelector('#directory-picker')`)
	require.Equal(t, canonical, page.MustElement("#editor [name=cwd]").MustProperty("value").Str())
	page.MustElementR("#editor button", "^Browse directories$").MustClick()
	page.MustElementR("#directory-picker button", "^Parent directory$").MustClick()
	page.MustElementR("#directory-picker .directory-list button", "^project$")
	page.MustElementR("#directory-picker button", "^Cancel directory selection$").MustClick()
	require.Equal(t, canonical, page.MustElement("#editor [name=cwd]").MustProperty("value").Str())
	// Delay a real directory response, edit the path, and prove that the old
	// result cannot become an eligible selection for the new text.
	page.MustEval(`() => {const original=window.fetch;window.fetch=async (...args)=>{const response=await original(...args);if(String(args[0]).startsWith('/v2/directories?'))return new Promise(resolve=>{window.releaseDirectory=()=>resolve(response)});return response};window.restoreDirectoryFetch=()=>{window.fetch=original}}`)
	page.MustElementR("#editor button", "^Browse directories$").MustClick()
	page.MustWait(`() => typeof window.releaseDirectory === 'function'`)
	page.MustElement("#directory-picker input[aria-label='Host directory path']").MustSelectAllText().MustInput("/not-the-returned-directory")
	page.MustEval(`async () => {window.releaseDirectory();await new Promise(requestAnimationFrame);await new Promise(requestAnimationFrame);window.restoreDirectoryFetch()}`)
	require.True(t, page.MustElementR("#directory-picker button", "^Use this directory$").MustProperty("disabled").Bool())
	page.MustElementR("#directory-picker button", "^Cancel directory selection$").MustClick()
	require.Equal(t, canonical, page.MustElement("#editor [name=cwd]").MustProperty("value").Str())
	var profiles []model.ConfigurationProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles", nil, &profiles))
	require.Empty(t, profiles)
	page.MustElement("#editor button[value=cancel]").MustClick()
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New team template$").MustClick()
	page.MustElementR("#team-editor button", "^Add member$").MustClick()
	page.MustElement("#team-editor [name=cwd]").MustInput(root)
	page.MustElementR("#team-editor button", "^Browse directories$").MustClick()
	page.MustElementR("#directory-picker .directory-list button", "^project$").MustClick()
	page.MustElementR("#directory-picker button", "^Use this directory$").MustClick()
	page.MustWait(`() => !document.querySelector('#directory-picker')`)
	require.Equal(t, canonical, page.MustElement("#team-editor [name=cwd]").MustProperty("value").Str())
}
