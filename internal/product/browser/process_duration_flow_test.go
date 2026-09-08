package browser

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserProcessDurationsRemainExactThroughImportEditAndReopen(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	source := `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: precise_wait
start: wait
nodes:
  wait:
    type: wait
    name: Precise wait
    wait: {duration: 9007199254740993ns}
    next: done
  done: {type: end}
`
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^Import legacy process$").MustClick()
	page.MustElement("#process-import textarea").MustInput(source)
	page.MustElementR("#process-import button", "^Inspect source$").MustClick()
	page.MustElementR("#process-import button", "^Preview converted draft$").MustClick()
	page.MustElementR("#process-import [role=status]", "Converted draft is unsaved")
	page.MustElementR("#process-import button", "^Open unsaved copy$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	var defs []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &defs))
	require.Len(t, defs, 1)
	read := func(want time.Duration) {
		var result app.DefinitionResult
		require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(defs[0].ID), nil, &result))
		for _, n := range result.Revision.Process.Graph.Nodes {
			if n.ID == "wait" {
				require.Equal(t, want, n.Wait.Duration)
				return
			}
		}
		t.Fatal("wait missing")
	}
	read(9007199254740993)
	var exported string
	for i, tc := range []struct {
		text  string
		nanos time.Duration
	}{{"9223372036.854775807", time.Duration(1<<63 - 1)}, {"0.000000015", 15}} {
		page.MustElementR("#process-editor button", "^Close editor$").MustClick()
		page.MustElementR("#definition-list button", "^Edit process$").MustClick()
		page.MustElement("#process-editor-canvas .process-node[aria-label='Precise wait, wait']").MustClick()
		want := "9007199.254740993"
		if i == 1 {
			want = "9223372036.854775807"
		}
		require.Equal(t, want, page.MustElement("#process-inspector [name=duration]").MustProperty("value").String())
		page.MustElement("#process-inspector [name=duration]").MustSelectAllText().MustInput(tc.text)
		page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
		page.MustElementR("#process-editor button", "^Save revision$").MustClick()
		page.MustElementR("#process-editor-message", fmt.Sprintf("Revision %d · saved", i+2))
		read(tc.nanos)
		if i == 0 {
			page.MustEval(`()=>{const original=URL.createObjectURL;URL.createObjectURL=blob=>{blob.text().then(text=>window.durationExport=text);return original(blob)}}`)
			page.MustElementR("#process-editor button", "^Export$").MustClick()
			page.MustWait(`()=>!!window.durationExport`)
			exported = page.MustEval(`()=>window.durationExport`).String()
		}
	}
	file := filepath.Join(t.TempDir(), "exact-duration.json")
	require.NoError(t, os.WriteFile(file, []byte(exported), 0600))
	choose := page.MustHandleFileDialog()
	page.MustElementR("#process-editor button", "^Import copy$").MustClick()
	choose(file)
	page.MustElementR("#process-editor-message", "New process · unsaved changes")
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &defs))
	require.Len(t, defs, 2)
	found := false
	for _, d := range defs {
		var result app.DefinitionResult
		require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(d.ID), nil, &result))
		for _, n := range result.Revision.Process.Graph.Nodes {
			if n.Wait != nil && n.Wait.Duration == time.Duration(1<<63-1) {
				found = true
			}
		}
	}
	require.True(t, found, "export/import must retain the exact maximum duration")

}
