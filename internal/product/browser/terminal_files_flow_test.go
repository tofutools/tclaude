package browser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type fileBrowserProvider struct {
	*terminalBrowserProvider
	root string
}
type fileBrowserPrepared struct {
	ports.PreparedAttempt
	root string
}
type fileBrowserRuntime struct {
	ports.Runtime
	root string
}

func (p *fileBrowserProvider) Prepare(ctx context.Context, in ports.PreparationRequest) (ports.PreparedAttempt, error) {
	a, err := p.terminalBrowserProvider.Prepare(ctx, in)
	return &fileBrowserPrepared{a, p.root}, err
}
func (p *fileBrowserPrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	r, err := p.PreparedAttempt.Release(ctx, permit)
	r.Runtime = &fileBrowserRuntime{r.Runtime, p.root}
	return r, err
}
func (r *fileBrowserRuntime) StageTerminalFile(ctx context.Context, in ports.StageTerminalFileRequest) (ports.StageTerminalFileResult, error) {
	return host.StageTerminalFile(ctx, r.root, r.ExecutionID(), in)
}

func TestBrowserTerminalFilesStageWithoutSendingAndKeepExactPane(t *testing.T) {
	root := t.TempDir()
	provider := &fileBrowserProvider{&terminalBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, runtimes: map[model.AgentID]*terminalBrowserRuntime{}}, root}
	ctx, page, operator := processEditorBrowser(t, provider)
	for _, id := range []string{"alpha", "beta"} {
		desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": desired}, nil))
		require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "launch_" + id, "target": map[string]any{"agent": map[string]any{"agent_id": id, "expected_revision": 1}}}, nil))
	}
	page.MustElement("#refresh").MustClick()
	for _, id := range []string{"alpha", "beta"} {
		page.MustElement("[data-tab=groups]").MustClick()
		page.MustElementR("#roster .row", id).MustElementR("button", "^Attach$").MustClick()
		page.MustElementR("#terminal-status", "Attached · "+id)
	}
	page.MustElementR("#terminal-tabs button", "^alpha$").MustClick()
	page.MustElement("#terminal-files summary").MustClick()
	page.MustWait(`() => !document.querySelector('[aria-label="Choose terminal upload"]').disabled`)
	source := filepath.Join(t.TempDir(), "image.png")
	content := []byte("image bytes from user")
	require.NoError(t, os.WriteFile(source, content, 0600))
	page.MustElement("[aria-label='Choose terminal upload']").MustSetFiles(source)
	page.MustElementR("#terminal-files [role=status]", "Nothing uploaded")
	_, err := os.Stat(filepath.Join(root, "uploads"))
	require.True(t, os.IsNotExist(err))
	page.MustElementR("#terminal-files button", "^Upload to selected execution$").MustClick()
	page.MustElementR("#terminal-files [role=status]", "No input sent")
	path := page.MustEval(`() => terminalFiles.receipt.NativePath`).Str()
	bytes, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, content, bytes)
	require.Empty(t, page.MustElement("[aria-label='Terminal draft']").MustProperty("value").Str())
	page.MustElementR("#terminal-files button", "^Upload to selected execution$").MustClick()
	page.MustElementR("#terminal-files [role=status]", "No input sent")
	entries, err := os.ReadDir(filepath.Join(root, "uploads"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	page.MustElementR("#terminal-files button", "^Insert uploaded path into draft$").MustClick()
	page.MustElementR("#terminal-files [role=status]", "no input has been sent")
	require.Contains(t, page.MustElement("[aria-label='Terminal draft']").MustProperty("value").Str(), path)
	require.False(t, page.MustEval(`() => terminalTools.lines(terminals.selected).join('\n').includes('uploads/')`).Bool())
	page.MustElementR("#terminal-tabs button", "^beta$").MustClick()
	require.True(t, page.MustElementR("#terminal-files button", "^Insert uploaded path into draft$").MustProperty("disabled").Bool())
	page.MustElementR("#terminal-tools button", "^Send draft$").MustClick()
	page.MustElementR("#error", "Target current pane explicitly")
	page.MustElement("[aria-label='Choose terminal upload']").MustSetFiles(source)
	page.MustEval(`() => { const original=window.fetch;window.fetch=async (...args)=>{const response=await original(...args);if(args[0]==='/v2/terminal-files'){await new Promise(resolve=>window.finishUpload=resolve)}return response} }`)
	page.MustElementR("#terminal-files button", "^Upload to selected execution$").MustClick()
	page.MustWait(`() => !!window.finishUpload`)
	page.MustElement("#close-terminal").MustClick()
	page.MustEval(`() => window.finishUpload()`)
	page.MustWait(`() => !terminalFiles.busy && terminalFiles.receipt===null`)
	page.MustElement("#reconnect-terminal").MustClick()
	page.MustElementR("#terminal-status", "Attached · beta")
	page.MustWait(`() => !document.querySelector('[aria-label="Choose terminal upload"]').disabled`)
	require.True(t, page.MustElementR("#terminal-files button", "^Insert uploaded path into draft$").MustProperty("disabled").Bool())
	page.MustElement("[aria-label='Choose terminal upload']").MustSetFiles(source)
	page.MustEval(`() => {window.finishUpload=null}`)
	page.MustElementR("#terminal-files button", "^Upload to selected execution$").MustClick()
	page.MustWait(`() => !!window.finishUpload`)
	page.MustElement("#logout").MustClick()
	page.MustElementR("#connection", "Signed out")
	page.MustEval(`() => window.finishUpload()`)
	page.MustWait(`() => terminalFiles.receipt===null && terminalFiles.intent===null && !terminalFiles.busy`)
	require.Empty(t, page.MustElement("[aria-label='Terminal draft']").MustProperty("value").Str())
	require.Zero(t, provider.runtime("alpha").stops.Load())
	require.Zero(t, provider.runtime("beta").stops.Load())
}
