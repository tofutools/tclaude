package browser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserExactParameterDefaultsSurviveEditAndLaunch(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	require.True(t, page.MustEval(`()=>{
 const {ExactJSON,stringifyExact}=ExactJSONTools;
 const ordinary={nested:[null,true,"literal",{omit:undefined,value:3}],omit:undefined,raw:{DefaultJSON:"not a protocol marker"}};
 if(stringifyExact(ordinary)!==JSON.stringify(ordinary))return false;
 if(stringifyExact({value:new ExactJSON('{"n":9007199254740993,"d":0.1234567890123456789}')} )!=='{"value":{"n":9007199254740993,"d":0.1234567890123456789}}')return false;
 let invalid=false,cycle=false;try{new ExactJSON('not JSON')}catch{invalid=true}const circular={};circular.self=circular;try{stringifyExact(circular)}catch{cycle=true}return invalid&&cycle;
 }`).Bool())
	const exact = "0.1234567890123456789"
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add task$").MustClick()
	page.MustElement("[aria-label='Performer kind']").MustSelect("human")
	page.MustElement("#process-inspector [name=ask]").MustInput("Amount {{ params.amount }}")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-editor button", "^Overview$").MustClick()
	page.MustElement("#process-inspector [name=parameter_syntax]").MustClick()
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Parameters$").MustClick()
	page.MustElementR("#process-inspector button", "^Add parameter$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustInput("amount")
	page.MustElement("#process-inspector [name=type]").MustSelect("number")
	page.MustElement("#process-inspector [name=default]").MustInput(exact)
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var result app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &result))
	require.Equal(t, exact, string(result.Revision.Parameters[0].Default))
	page.MustEval(`()=>{const original=URL.createObjectURL;URL.createObjectURL=blob=>{window.exactExport=blob.text();return original(blob)};document.addEventListener('click',e=>{if(e.target.download)e.preventDefault()})}`)
	page.MustElementR("#process-editor button", "^Export$").MustClick()
	page.MustWait(`()=>!!window.exactExport`)
	exported := page.MustEval(`async()=>await window.exactExport`).Str()
	require.Contains(t, exported, `"Default": `+exact)
	require.Contains(t, exported, `"DefaultJSON": "`+exact+`"`)
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElementR("#process-editor button", "^Parameters$").MustClick()
	page.MustElementR("#process-inspector button", "^Edit amount$").MustClick()
	require.Equal(t, exact, page.MustElement("#process-inspector [name=default]").MustProperty("value").Str())
	page.MustElement("#process-inspector [name=description]").MustInput("Exact amount")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 2 · saved")
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &result))
	require.Equal(t, exact, string(result.Revision.Parameters[0].Default))
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Start process$").MustClick()
	input := page.MustElement("#editor [aria-label='Exact amount']")
	require.Equal(t, exact, input.MustProperty("value").Str())
	const override = "9007199254740993"
	input.MustSelectAllText().MustInput(override)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting`)
	var decisions []app.DecisionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/decisions", nil, &decisions))
	require.Len(t, decisions, 1)
	require.Equal(t, "Amount "+override, decisions[0].Window.Question)
	file := filepath.Join(t.TempDir(), "process.json")
	require.NoError(t, os.WriteFile(file, []byte(exported), 0600))
	page.MustEval(`()=>window.previousDefinitionButton=document.querySelector('#definition-list button')`)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustWait(`()=>!window.previousDefinitionButton.isConnected`)
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	choose := page.MustHandleFileDialog()
	page.MustElementR("#process-editor button", "^Import copy$").MustClick()
	choose(file)
	page.MustElementR("#process-editor-message", "New process · unsaved changes")
	page.MustElementR("#process-editor button", "^Parameters$").MustClick()
	page.MustElementR("#process-inspector button", "^Edit amount$").MustClick()
	require.Equal(t, exact, page.MustElement("#process-inspector [name=default]").MustProperty("value").Str())
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 2)
	for _, definition := range definitions {
		require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definition.ID), nil, &result))
		require.Equal(t, exact, string(result.Revision.Parameters[0].Default))
	}

}
