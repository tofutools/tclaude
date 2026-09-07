package browser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserTerminalDownloadsConfineFilesAndDiscardStaleResponses(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "report.txt"), []byte("downloaded report"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "private.txt"), []byte("outside secret"), 0600))
	provider := &terminalBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, runtimes: map[model.AgentID]*terminalBrowserRuntime{}}
	ctx, page, operator := processEditorBrowser(t, provider)
	for _, id := range []string{"alpha", "beta"} {
		desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": desired}, nil))
		require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "launch_" + id, "target": map[string]any{"agent": map[string]any{"agent_id": id, "expected_revision": 1}}}, nil))
	}
	page.MustElement("#refresh").MustClick()
	for _, id := range []string{"alpha", "beta"} {
		page.MustElement("[data-tab=groups]").MustClick()
		page.MustElementR("#roster .row", id).MustElementR("button", "^Attach$").MustClick()
		page.MustElementR("#terminal-status", "Attached · "+id)
	}
	page.MustElement("#terminal-downloads summary").MustClick()
	page.MustEval(`() => {window.downloaded=[];const original=URL.createObjectURL;URL.createObjectURL=b=>{b.text().then(text=>window.downloaded.push(text));return original(b)};document.addEventListener('click',e=>{if(e.target.download)e.preventDefault()})}`)
	page.MustElement("[aria-label='Download execution file path']").MustInput("report.txt")
	page.MustElementR("#terminal-downloads button", "^Download execution file$").MustClick()
	page.MustElementR("#terminal-downloads [role=status]", "No input sent")
	page.MustWait(`() => window.downloaded.length===1`)
	require.Equal(t, "downloaded report", page.MustEval(`() => window.downloaded[0]`).Str())
	page.MustElement("[aria-label='Download execution file path']").MustSelectAllText().MustInput(filepath.Join(outside, "private.txt"))
	page.MustElementR("#terminal-downloads button", "^Download execution file$").MustClick()
	page.MustElementR("#terminal-downloads [role=status]", "File unavailable")
	require.Equal(t, 1, page.MustEval(`() => window.downloaded.length`).Int())
	// Exercise the registered xterm OSC8 activation callback. A modifier gesture
	// is required, and unsupported schemes do not open a browsing context.
	page.MustEval(`() => {window.linkOpen=[];window.open=(...args)=>window.linkOpen.push(args);terminals.selected.terminal.options.linkHandler.activate({ctrlKey:true},'javascript:alert(1)')}`)
	page.MustElementR("#terminal-downloads [role=status]", "Blocked unsupported")
	require.Zero(t, page.MustEval(`() => window.linkOpen.length`).Int())
	page.MustEval(`() => terminals.selected.terminal.options.linkHandler.activate({},'https://example.invalid/report')`)
	require.Zero(t, page.MustEval(`() => window.linkOpen.length`).Int())
	page.MustEval(`() => terminals.selected.terminal.options.linkHandler.activate({ctrlKey:true},'https://example.invalid/report')`)
	require.Equal(t, "noopener,noreferrer", page.MustEval(`() => window.linkOpen[0][2]`).Str())
	page.MustEval(`path => terminals.selected.terminal.options.linkHandler.activate({ctrlKey:true},'file://'+path)`, filepath.Join(root, "report.txt"))
	page.MustWait(`() => window.downloaded.length===2`)
	page.MustEval(`() => {const original=fetch;window.fetch=async (...args)=>{const response=await original(...args);if(String(args[0]).startsWith('/v2/execution-files?'))await new Promise(resolve=>window.finishDownload=resolve);return response}}`)
	page.MustElement("[aria-label='Download execution file path']").MustSelectAllText().MustInput("report.txt")
	page.MustElementR("#terminal-downloads button", "^Download execution file$").MustClick()
	page.MustWait(`() => !!window.finishDownload`)
	page.MustElementR("#terminal-tabs button", "^alpha$").MustClick()
	page.MustEval(`() => window.finishDownload()`)
	page.MustWait(`() => terminalDownloads.controller===null`)
	require.Equal(t, 2, page.MustEval(`() => window.downloaded.length`).Int())
	page.MustEval(`() => window.finishDownload=null`)
	page.MustElement("[aria-label='Download execution file path']").MustInput("report.txt")
	page.MustElementR("#terminal-downloads button", "^Download execution file$").MustClick()
	page.MustWait(`() => !!window.finishDownload`)
	page.MustElement("#logout").MustClick()
	page.MustElementR("#connection", "Signed out")
	page.MustEval(`() => window.finishDownload()`)
	page.MustWait(`() => terminalDownloads.controller===null && terminalDownloads.binding===null`)
	require.Equal(t, 2, page.MustEval(`() => window.downloaded.length`).Int())
	require.Zero(t, provider.runtime("alpha").stops.Load())
	require.Zero(t, provider.runtime("beta").stops.Load())
}
