package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"os"
	"path/filepath"
	"testing"
)

func TestBrowserLegacyProcessImportRequiresMappingAndExplicitSave(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	const source = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: original
params:
  amount:
    type: number
    default: 0.1234567890123456789
start: task
nodes:
  task:
    type: task
    performer:
      kind: human
      ask: Approve?
      prompt: Preserved <context>
    next: done
  done:
    type: end
    result: done
`
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^Import legacy process$").MustClick()
	page.MustElement("#process-import textarea").MustInput(source)
	page.MustElementR("#process-import button", "^Inspect source$").MustClick()
	mapping := page.MustElement("#process-import select")
	require.Equal(t, "", mapping.MustProperty("value").Str())
	page.MustElementR("#process-import button", "^Preview converted draft$").MustClick()
	page.MustElementR("#process-import [role=alert]", "Choose a mapping")
	mapping.MustSelect("Operator")
	page.MustElementR("#process-import button", "^Preview converted draft$").MustClick()
	page.MustElementR("#process-import [role=status]", "Converted draft is unsaved")
	var defs []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &defs))
	require.Empty(t, defs)
	badFile := filepath.Join(t.TempDir(), "invalid.yaml")
	require.NoError(t, os.WriteFile(badFile, []byte{255}, 0600))
	page.MustElement("#process-import input[type=file]").MustSetFiles(badFile)
	page.MustWait(`()=>!!document.querySelector('#process-import [role=alert]').textContent`)
	require.True(t, page.MustElementR("#process-import button", "^Open unsaved copy$").MustProperty("disabled").Bool())
	require.Empty(t, page.MustElement("#process-import textarea").MustProperty("value").Str())
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &defs))
	require.Empty(t, defs)
	page.MustElement("#process-import textarea").MustInput(source)
	page.MustElementR("#process-import button", "^Inspect source$").MustClick()
	page.MustElement("#process-import select").MustSelect("Operator")
	page.MustElementR("#process-import button", "^Preview converted draft$").MustClick()
	page.MustElementR("#process-import [role=status]", "Converted draft is unsaved")
	page.MustElementR("#process-import button", "^Open unsaved copy$").MustClick()
	page.MustElement("#process-editor")
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &defs))
	require.Empty(t, defs)
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &defs))
	require.Len(t, defs, 1)
	require.NotEqual(t, model.DefinitionID("original"), defs[0].ID)
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(defs[0].ID), nil, &saved))
	require.Equal(t, source, saved.Revision.Source)
	require.Equal(t, "0.1234567890123456789", string(saved.Revision.Parameters[0].Default))
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElementR("#process-editor button", "^Source$").MustClick()
	require.Equal(t, source, page.MustElement("#process-inspector [name=source]").MustProperty("value").Str())
}

func TestBrowserProcessImportCompletesPartialProfileWithoutChangingSource(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	var profile app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{
		"request_id": "portable", "id": "portable", "revision_id": "one", "name": "Portable worker",
		"options": map[string]any{"Model": "portable-model"},
	}, &profile))
	const source = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: portable-process
start: task
nodes:
  task:
    type: task
    performer:
      kind: agent
      profile: old-profile
      prompt: Perform the work
    next: done
  done:
    type: end
    result: done
`
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^Import legacy process$").MustClick()
	page.MustElement("#process-import textarea").MustInput(source)
	page.MustElementR("#process-import button", "^Inspect source$").MustClick()
	page.MustElement("#process-import select").MustSelect("Portable worker · one")
	page.MustElementR("#process-import button", "^Preview converted draft$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	require.Empty(t, page.MustElement("#editor [name=cwd]").MustProperty("value").Str())
	require.Equal(t, "portable-model", page.MustElement("#editor [name=model]").MustProperty("value").Str())
	cwd := t.TempDir()
	page.MustElement("#editor [name=cwd]").MustInput(cwd)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#process-import [role=status]", "Converted draft is unsaved")
	page.MustElementR("#process-import button", "^Open unsaved copy$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Equal(t, source, saved.Revision.Source)
	require.NotNil(t, saved.Revision.Process)
	var worker *model.DesiredConfiguration
	for _, node := range saved.Revision.Process.Graph.Nodes {
		if node.Performer != nil && node.Performer.Agent != nil {
			worker = node.Performer.Agent.CreateDesired
		}
	}
	require.NotNil(t, worker)
	require.Equal(t, cwd, worker.WorkingDirectory)
	require.Equal(t, "portable-model", worker.Model)
	var unchanged app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/portable", nil, &unchanged))
	require.Equal(t, profile, unchanged)
	var snapshot struct {
		Agents     []model.Agent     `json:"agents"`
		Executions []model.Execution `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Agents)
	require.Empty(t, snapshot.Executions)
}
