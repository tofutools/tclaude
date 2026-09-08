package browser

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers/copilot"
)

// Only the native workload is replaced. Selection, materialization, admission,
// credentials, release permits and durable execution projection are production.
type sandboxBrowserProvider struct {
	accessBrowserProvider
	requests chan ports.PreparationRequest
}

func (*sandboxBrowserProvider) Name() string { return "claude" }
func (*sandboxBrowserProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{HostSandbox: true, LaunchPolicy: &ports.PolicyRequirements{SupportedApproval: []model.ApprovalMode{model.ApprovalSupervised}, SupportedSandbox: []model.SandboxMode{model.SandboxWorkspaceWrite}}}
}
func (p *sandboxBrowserProvider) Prepare(ctx context.Context, r ports.PreparationRequest) (ports.PreparedAttempt, error) {
	prepared, err := p.accessBrowserProvider.Prepare(ctx, r)
	if err != nil {
		return nil, err
	}
	p.requests <- r
	return &sandboxBrowserPrepared{accessBrowserPrepared: *prepared.(*accessBrowserPrepared)}, nil
}

type sandboxBrowserPrepared struct{ accessBrowserPrepared }

func (p *sandboxBrowserPrepared) Describe() ports.PreparedDescription {
	d := p.accessBrowserPrepared.Describe()
	if p.spec.HostSandbox != nil {
		d.HostSandboxPolicyHash = p.spec.HostSandbox.PolicyHash
	}
	d.Evidence.Provider = "claude"
	return d
}

func (p *sandboxBrowserPrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, err
	}
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: &accessBrowserRuntime{id: p.spec.ExecutionID}, Evidence: p.Describe().Evidence}, nil
}

func TestBrowserSavedSandboxSelectionUsesUpdatedProfile(t *testing.T) {
	provider := &sandboxBrowserProvider{accessBrowserProvider: accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 1)}, requests: make(chan ports.PreparationRequest, 1)}
	ctx, page, operator := processEditorBrowser(t, provider, &copilot.Provider{})
	var source app.SandboxProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles", map[string]any{"request_id": "source", "id": "source", "name": "Saved boundary", "policy": model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate}}, &source))
	page.MustElement("#new-agent").MustClick()
	page.MustElement("#editor option[value='profile:source']")
	page.MustElement("#editor [name=host_sandbox]").MustSelect("Saved boundary")
	page.MustElementR("#editor [aria-label='Configured launch support']", "Host sandbox preparation is configured")
	page.MustElement("#editor [name=harness]").MustSelect("copilot")
	page.MustElementR("#editor [aria-label='Configured launch support']", "Unsupported host sandbox selection")
	page.MustElement("#editor [name=harness]").MustSelect("claude")
	page.MustElementR("#editor [aria-label='Configured launch support']", "Host sandbox preparation is configured")
	page.MustElement("#editor [name=name]").MustInput("Sandbox worker")
	page.MustElement("#editor [name=model]").MustInput("fixture")
	page.MustElement("#editor [name=cwd]").MustInput(t.TempDir())
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var snapshot struct {
		Agents     []model.Agent `json:"agents"`
		Executions []struct {
			Spec  model.ResolvedExecutionSpec
			State model.ExecutionState
		} `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Empty(t, snapshot.Executions)
	selected := snapshot.Agents[0].Desired.HostSandbox
	require.NotNil(t, selected)
	require.Equal(t, model.SandboxProfileRef{ProfileID: source.Profile.ID}, selected.Scopes[0].Ref)
	require.Empty(t, selected.PolicyHash)
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles", map[string]any{"request_id": "later", "id": "source", "name": "Later policy", "expected_revision": source.Profile.Revision, "policy": model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate, Environment: model.Environment{"LATER": "value"}}}, nil))
	// A saved configuration/default retains the profile ID and uses its updated content.
	page.MustElementR("#roster button", "^Save settings as configuration$").MustClick()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Sandbox default")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustWait(`()=>document.querySelector("#sandbox-profiles").textContent.includes("Later policy")`)
	page.MustElementR("#configuration-list button", "^Use as default$").MustClick()
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#configuration-list button", "^Create from global$").MustClick()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Default worker")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElement("[data-tab=groups]").MustClick()
	page.MustElement("[aria-label='Search agents']").MustInput("Default worker")
	page.MustElementR("#roster button", "^Select visible$").MustClick()
	page.MustElementR("#roster button", "^Start selected$").MustClick()
	page.MustElement("#editor [name=confirm]").MustInput("START")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.Contains(t, page.MustElement("#roster [role=status]").MustText(), "start: Accepted")
	select {
	case request := <-provider.requests:
		require.NotNil(t, request.HostSandboxPolicy)
		materialized, err := request.HostSandboxPolicy.LaunchSelection()
		require.NoError(t, err)
		require.True(t, model.SameSandboxProfiles(selected, &materialized))
		require.Equal(t, "value", request.HostSandboxPolicy.Composition.Values.Environment["LATER"])
	case <-ctx.Done():
		t.Fatal("sandbox preparation was not reached")
	}
	snapshot.Executions = nil
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Executions, 1)
	require.True(t, model.SameSandboxProfiles(selected, snapshot.Executions[0].Spec.HostSandbox))
	require.Contains(t, []model.ExecutionState{model.ExecutionReleased, model.ExecutionRunning}, snapshot.Executions[0].State)
}
