package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type contextMeterProvider struct {
	accessBrowserProvider
	known atomic.Bool
}

func (p *contextMeterProvider) ProjectContextUsage(model.Execution) *model.ContextUsage {
	if !p.known.Load() {
		return nil
	}
	return &model.ContextUsage{ModelWindow: 1000000, EffectiveWindow: 450000, NativePercent: 21, UsedPercent: 46.6666667, ObservedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}
func TestBrowserContextMeterShowsEffectiveWindowAndKeepsUnknownAbsent(t *testing.T) {
	provider := &contextMeterProvider{accessBrowserProvider: accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 2)}}
	ctx, page, operator := processEditorBrowser(t, provider)
	desired := model.DesiredConfiguration{Harness: "access-fixture", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "context_worker", "name": "Context worker", "desired": desired}, nil))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "start_context", "target": map[string]any{"agent": map[string]any{"agent_id": "context_worker", "expected_revision": 1}}}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#roster .name", "Context worker")
	require.True(t, page.MustEval(`()=>!document.querySelector('#roster meter')`).Bool())
	provider.known.Store(true)
	page.MustElement("#refresh").MustClick()
	meter := page.MustElement("#roster meter[aria-label='Context usage']")
	require.InDelta(t, 46.6666667, meter.MustProperty("value").Num(), 0.00001)
	require.Contains(t, meter.MustProperty("title").Str(), "450")
	page.MustElementR("#roster span", `47% context`)
	provider.known.Store(false)
	page.MustElement("#refresh").MustClick()
	page.MustWait(`()=>!document.querySelector('#roster meter')`)
}
