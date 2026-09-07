package browser

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserRosterFiltersAndBulkRetirementPreserveConcurrentChanges(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	for _, state := range []model.ExecutionState{model.ExecutionReserved, model.ExecutionPrepared, model.ExecutionReleased, model.ExecutionRunning, model.ExecutionExited, model.ExecutionFailed, model.ExecutionUnknown} {
		require.True(t, page.MustEval(`state => Array.from(document.querySelector('[aria-label="State filter"]').options).some(option=>option.value===state)`, string(state)).Bool(), "missing execution state %s", state)
	}
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, name := range []string{"Alpha", "Beta", "Gamma"} {
		d := desired
		if name == "Gamma" {
			d.Harness = "claude"
		}
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": strings.ToLower(name), "name": name, "desired": d}, nil))
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "group", "name": "Builders", "members": []string{"alpha", "beta"}}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#roster .name", "Alpha")
	page.MustElement("[aria-label='Harness filter']").MustSelect("codex")
	require.NotContains(t, page.MustElement("#roster").MustText(), "Gamma")
	page.MustElementR("#roster button", "^Select visible$").MustClick()
	page.MustElementR("#roster button", "^Retire selected$").MustClick()
	// A concurrent configuration change after selection must not be silently
	// picked up by the bulk action's revision-checked request.
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/agents/beta", map[string]any{"name": "Beta updated", "expected_revision": 1, "desired": desired}, nil))
	page.MustElement("#editor [name=confirm]").MustInput("RETIRE")
	page.MustElement("#editor [name=reason]").MustInput("Finish selected workers")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.Contains(t, page.MustElement("#roster [role=status]").MustText(), "Alpha · retire: Accepted")
	require.Contains(t, page.MustElement("#roster [role=status]").MustText(), "Beta · retire: Failed")
	var snapshot struct {
		Agents []model.Agent `json:"agents"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	for _, a := range snapshot.Agents {
		if a.ID == "alpha" {
			require.Equal(t, model.AgentRetired, a.Lifecycle)
		} else {
			require.Equal(t, model.AgentActive, a.Lifecycle)
		}
	}
	page.MustElement("[aria-label='Search agents']").MustInput("Alpha")
	page.MustElementR("#roster button", "^Select visible$").MustClick()
	page.MustElementR("#roster button", "^Reactivate selected$").MustClick()
	page.MustElement("#editor [name=confirm]").MustInput("REACTIVATE")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#roster [role=status]", "Alpha · reactivate: Accepted")
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	for _, a := range snapshot.Agents {
		require.Equal(t, model.AgentActive, a.Lifecycle)
	}
}

// Only the native workload is doubled; bulk requests use the real authenticated
// transport, operation admission, credential delivery and SQLite projections.
type rosterProvider struct{ accessBrowserProvider }

func (p *rosterProvider) Prepare(ctx context.Context, req ports.PreparationRequest) (ports.PreparedAttempt, error) {
	prepared, err := p.accessBrowserProvider.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	return &rosterPrepared{accessBrowserPrepared: *prepared.(*accessBrowserPrepared)}, nil
}

type rosterPrepared struct{ accessBrowserPrepared }

func (p *rosterPrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, err
	}
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: &rosterRuntime{accessBrowserRuntime: accessBrowserRuntime{id: p.spec.ExecutionID}}, Evidence: p.Describe().Evidence}, nil
}

type rosterRuntime struct{ accessBrowserRuntime }

func (*rosterRuntime) Stop(context.Context, ports.StopRequest) (ports.StopResult, error) {
	return ports.StopResult{Disposition: ports.EffectAccepted, Acknowledged: true, Exited: true}, nil
}
func TestBrowserRosterStartsAndStopsSelectedExecutions(t *testing.T) {
	p := &rosterProvider{accessBrowserProvider: accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 4)}}
	ctx, page, operator := processEditorBrowser(t, p)
	desired := model.DesiredConfiguration{Harness: "access-fixture", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, id := range []string{"alpha", "beta"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": desired}, nil))
	}
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#roster .name", "alpha")
	for _, action := range []string{"Start", "Stop"} {
		page.MustElementR("#roster button", "^Select visible$").MustClick()
		page.MustElementR("#roster button", "^"+action+" selected$").MustClick()
		page.MustElement("#editor [name=confirm]").MustInput(strings.ToUpper(action))
		page.MustElement("#editor button[type=submit]").MustClick()
		page.MustElement("#editor").MustWaitInvisible()
		report := page.MustElement("#roster [role=status]").MustText()
		require.Contains(t, report, "alpha · "+strings.ToLower(action)+": Accepted")
		require.Contains(t, report, "beta · "+strings.ToLower(action)+": Accepted")
	}
	var snapshot struct {
		Executions []struct {
			State string `json:"state"`
		} `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Executions, 2)
	for _, execution := range snapshot.Executions {
		require.Equal(t, "exited", execution.State)
	}
}
