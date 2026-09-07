package browser

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

// Regression: an admitted stop can be refused by the native
// runtime without becoming an HTTP error. The roster must not report that as
// an accepted lifecycle result.
type refusedStopRosterProvider struct{ accessBrowserProvider }

func (p *refusedStopRosterProvider) Prepare(ctx context.Context, req ports.PreparationRequest) (ports.PreparedAttempt, error) {
	prepared, err := p.accessBrowserProvider.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	return &refusedStopRosterPrepared{accessBrowserPrepared: *prepared.(*accessBrowserPrepared)}, nil
}

type refusedStopRosterPrepared struct{ accessBrowserPrepared }

func (p *refusedStopRosterPrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, err
	}
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: &refusedStopRosterRuntime{accessBrowserRuntime: accessBrowserRuntime{id: p.spec.ExecutionID}}, Evidence: p.Describe().Evidence}, nil
}

type refusedStopRosterRuntime struct{ accessBrowserRuntime }

func (*refusedStopRosterRuntime) Stop(context.Context, ports.StopRequest) (ports.StopResult, error) {
	return ports.StopResult{Disposition: ports.EffectRefused}, nil
}

func TestBrowserRosterDoesNotCallRefusedStopAccepted(t *testing.T) {
	p := &refusedStopRosterProvider{accessBrowserProvider: accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 2)}}
	ctx, page, operator := processEditorBrowser(t, p)
	desired := model.DesiredConfiguration{Harness: "access-fixture", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "alpha", "name": "alpha", "desired": desired}, nil))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "launch-alpha", "target": map[string]any{"agent": map[string]any{"agent_id": "alpha", "expected_revision": 1}}}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#roster .name", "alpha")
	page.MustElementR("#roster button", "^Select visible$").MustClick()
	page.MustElementR("#roster button", "^Stop selected$").MustClick()
	page.MustElement("#editor [name=confirm]").MustInput("STOP")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var snapshot struct {
		Operations []struct {
			Kind  string `json:"kind"`
			State string `json:"state"`
		} `json:"operations"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Equal(t, "stop", snapshot.Operations[len(snapshot.Operations)-1].Kind)
	require.Equal(t, "refused", snapshot.Operations[len(snapshot.Operations)-1].State)
	report := page.MustElement("#roster [role=status]").MustText()
	require.NotContains(t, report, "stop: Accepted", report)
	require.Contains(t, report, "stop: Failed", report)
}
