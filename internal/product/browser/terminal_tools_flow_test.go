package browser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserTerminalToolsKeepDraftOnExactAttachment(t *testing.T) {
	provider := &terminalBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, runtimes: map[model.AgentID]*terminalBrowserRuntime{}}
	ctx, page, operator := processEditorBrowser(t, provider)
	for _, id := range []string{"alpha", "beta"} {
		desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": desired}, nil))
		require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "start_" + id, "target": map[string]any{"agent": map[string]any{"agent_id": id, "expected_revision": 1}}}, nil))
	}
	page.MustElement("#refresh").MustClick()
	for _, id := range []string{"alpha", "beta"} {
		page.MustElement("[data-tab=groups]").MustClick()
		page.MustElementR("#roster .row", id).MustElementR("button", "^Attach$").MustClick()
		page.MustElementR("#terminal-status", "Attached · "+id)
	}
	page.MustElement("#terminal-tools summary").MustClick()
	page.MustElementR("#terminal-tabs button", "^alpha$").MustClick()
	page.MustElementR("#terminal-tools button", "^Target current pane$").MustClick()
	page.MustElement("[aria-label='Terminal draft']").MustInput("界 literal $(not-executed)")
	page.MustElementR("#terminal-tabs button", "^beta$").MustClick()
	page.MustElementR("#terminal-tools button", "^Send draft$").MustClick()
	page.MustElementR("#error", "Target current pane explicitly")
	page.MustElementR("#terminal-tabs button", "^alpha$").MustClick()
	page.MustElementR("#terminal-tools button", "^Send draft$").MustClick()
	page.MustElementR("#terminal-tools button", "^Enter$").MustClick()
	page.MustWait(`() => terminalTools.lines(terminals.selected).join('\n').includes('界 literal $(not-executed)')`)
	page.MustElement("[aria-label='Find in terminal scrollback']").MustInput("literal")
	page.MustElementR("#terminal-tools button", "^Find next$").MustClick()
	require.Equal(t, "literal", page.MustEval(`() => terminals.selected.terminal.getSelection()`).Str())
	page.MustEval(`() => {window.copied='';Object.defineProperty(navigator,'clipboard',{configurable:true,value:{writeText:async t=>{window.copied=t}}});window.exported='';const original=URL.createObjectURL;URL.createObjectURL=b=>{b.text().then(t=>window.exported=t);return original(b)};document.addEventListener('click',e=>{if(e.target.download)e.preventDefault()})}`)
	page.MustElementR("#terminal-tools button", "^Copy selection$").MustClick()
	page.MustWait(`() => window.copied==='literal'`)
	page.MustElementR("#terminal-tools button", "^Export scrollback$").MustClick()
	page.MustWait(`() => window.exported.includes('$(not-executed)')`)
	require.False(t, page.MustEval(`() => terminalTools.lines([...terminals.entries.values()].find(e=>e.label==='beta')).join('\n').includes('not-executed')`).Bool())
	page.MustElementR("#terminal-tools button", "^Larger text$").MustClick()
	page.MustElementR("#terminal-tools [role=status]", "px text")
	page.MustElement("#close-terminal").MustClick()
	page.MustElement("#reconnect-terminal").MustClick()
	page.MustElementR("#terminal-status", "Attached · alpha")
	page.MustElementR("#terminal-tools button", "^Send draft$").MustClick()
	page.MustElementR("#error", "Target current pane explicitly")
	file := filepath.Join(t.TempDir(), "draft.txt")
	require.NoError(t, os.WriteFile(file, []byte("loaded draft"), 0600))
	page.MustElement("[aria-label='Load terminal text file']").MustSetFiles(file)
	page.MustElementR("#terminal-tools [role=status]", "has not been sent")
	require.Equal(t, "loaded draft", page.MustElement("[aria-label='Terminal draft']").MustProperty("value").Str())
	require.False(t, page.MustEval(`() => terminalTools.lines(terminals.selected).join('\n').includes('loaded draft')`).Bool())
	page.MustElementR("#terminal-tools button", "^Target current pane$").MustClick()
	page.MustElementR("#terminal-tools button", "^Send draft$").MustClick()
	page.MustElementR("#terminal-tools button", "^Enter$").MustClick()
	page.MustWait(`() => terminalTools.lines(terminals.selected).join('\n').includes('loaded draft')`)
	page.MustEval(`() => document.querySelector('[aria-label="Terminal draft"]').value='\x1b[31m'`)
	page.MustElementR("#terminal-tools button", "^Send draft$").MustClick()
	page.MustElementR("#error", "control bytes")
	require.Zero(t, provider.runtime("alpha").stops.Load())
	require.Zero(t, provider.runtime("beta").stops.Load())
}
