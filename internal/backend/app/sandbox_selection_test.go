package app_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestLaunchSandboxSelectionResolvesServerContentBeforeSavingAgent(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	paths, err := host.NewSandboxPathInspector([]string{t.TempDir()})
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry()).WithSandboxPathInspector(paths)
	operator := model.OperatorPrincipal()
	profile, err := service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "create"}, ID: "sandbox", Name: "Sandbox", Policy: model.SandboxPolicy{Environment: model.Environment{"VALUE": "literal"}}})
	require.NoError(t, err)
	scopes := []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: profile.Revision.Ref}}
	selected, err := service.ResolveLaunchSandbox(ctx, operator, scopes)
	require.NoError(t, err)
	require.NoError(t, selected.Validate())
	desired := model.DesiredConfiguration{HostSandbox: &selected, Harness: "codex", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	created, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "agent", Name: "Agent", Desired: desired})
	require.NoError(t, err)
	require.Equal(t, &selected, created.Agent.Desired.HostSandbox)
	forged := selected.Clone()
	forged.PolicyHash = strings.Repeat("c", 64)
	desired.HostSandbox = &forged
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "forged", Name: "Forged", Desired: desired})
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = store.Agent(ctx, "forged")
	require.ErrorIs(t, err, app.ErrNotFound)
	_, err = service.SetSandboxProfileArchived(ctx, app.SetSandboxProfileArchivedRequest{Context: app.RequestContext{Principal: operator, RequestID: "archive"}, ID: profile.Profile.ID, ExpectedRevision: profile.Profile.Revision, Archived: true})
	require.NoError(t, err)
	_, err = service.ResolveLaunchSandbox(ctx, operator, scopes)
	require.ErrorIs(t, err, app.ErrConflict)
	saved, err := store.Agent(ctx, "agent")
	require.NoError(t, err)
	require.Equal(t, &selected, saved.Desired.HostSandbox, "archiving a profile does not rewrite an existing pin")
}

func TestSandboxConfigurationSaveRetryDoesNotRequireFreshHostInspection(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	paths, err := host.NewSandboxPathInspector([]string{t.TempDir()})
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry()).WithSandboxPathInspector(paths)
	operator := model.OperatorPrincipal()
	profile, err := service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "sandbox"}, ID: "sandbox", Name: "Sandbox", Policy: model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate}})
	require.NoError(t, err)
	selected, err := service.ResolveLaunchSandbox(ctx, operator, []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: profile.Revision.Ref}})
	require.NoError(t, err)
	req := app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "configuration"}, ID: "configuration", RevisionID: "revision", Name: "Configuration", Desired: model.DesiredConfiguration{HostSandbox: &selected, Harness: "codex", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}}
	saved, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	// Restarted application has no host inspector. The committed receipt still
	// answers an unchanged retry, while fresh writes require normal preparation.
	service = app.New(store, providers.NewRegistry())
	repeated, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.Equal(t, saved, repeated)
	changed := req
	changed.Name = "Changed intent"
	_, err = service.SaveConfigurationProfile(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	fresh := req
	fresh.Context.RequestID = "fresh"
	_, err = service.SaveConfigurationProfile(ctx, fresh)
	require.ErrorIs(t, err, app.ErrUnavailable)
}

func TestProviderSandboxAdmissionRequiresExactPreparationAndDoesNotReplay(t *testing.T) {
	for _, scenario := range []string{"unsupported", "missing-proof", "wrong-proof", "prepared"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "backend.sqlite")
			store, err := sqlite.Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			paths, err := host.NewSandboxPathInspector([]string{t.TempDir()})
			require.NoError(t, err)
			provider := &sandboxAdmissionProvider{supported: scenario != "unsupported", proof: scenario}
			service := app.New(store, providers.NewRegistry(provider)).WithSandboxPathInspector(paths)
			operator := model.OperatorPrincipal()
			profile, err := service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "policy"}, ID: "sandbox", Name: "Sandbox", Policy: model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate, Environment: model.Environment{"POLICY_VALUE": "retained"}}})
			require.NoError(t, err)
			selection, err := service.ResolveLaunchSandbox(ctx, operator, []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: profile.Revision.Ref}})
			require.NoError(t, err)
			agent, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "worker", Name: "Worker", Desired: model.DesiredConfiguration{HostSandbox: &selection, Harness: provider.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
			require.NoError(t, err)
			request := app.LaunchRequest{RequestContext: app.RequestContext{Principal: operator, RequestID: "launch"}, Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.Agent.ID, ExpectedRevision: agent.Agent.Revision}}, InitialMessage: "Prepared first work"}
			result, err := service.Launch(ctx, request)
			if scenario == "unsupported" {
				require.ErrorIs(t, err, app.ErrUnsupported)
				require.Empty(t, provider.preparations)
				return
			}
			require.Len(t, provider.preparations, 1)
			prepared := provider.preparations[0]
			require.NotNil(t, prepared.HostSandboxPolicy)
			resolved, resolveErr := prepared.HostSandboxPolicy.LaunchSelection()
			require.NoError(t, resolveErr)
			require.Equal(t, selection, resolved)
			require.Equal(t, "retained", prepared.HostSandboxPolicy.Composition.Values.Environment["POLICY_VALUE"])
			if scenario != "prepared" {
				require.ErrorIs(t, err, app.ErrInvalid)
				require.Zero(t, provider.releases)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, provider.releases)
			require.Equal(t, &selection, result.Execution.Spec.HostSandbox)
			require.NoError(t, store.Close())
			store, err = sqlite.Open(path)
			require.NoError(t, err)
			// Recovery of an exact request must not re-resolve policy or require a new
			// host preparer. The durable receipt retains the original selected policy.
			service = app.New(store, providers.NewRegistry(provider))
			retry, err := service.Launch(ctx, request)
			require.NoError(t, err)
			require.True(t, retry.Repeated)
			require.Equal(t, result.Operation.ID, retry.Operation.ID)
			require.Equal(t, &selection, retry.Execution.Spec.HostSandbox)
			require.Len(t, provider.preparations, 1)
			require.Equal(t, 1, provider.releases)
		})
	}
}

type sandboxAdmissionProvider struct {
	preparedWorkProvider
	supported bool
	proof     string
	releases  int
}

func (p *sandboxAdmissionProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{HostSandbox: p.supported, PreparedInitialInput: true}
}
func (p *sandboxAdmissionProvider) Prepare(_ context.Context, r ports.PreparationRequest) (ports.PreparedAttempt, error) {
	p.preparations = append(p.preparations, r)
	return &sandboxAdmissionAttempt{preparedWorkAttempt: preparedWorkAttempt{request: r}, owner: p}, nil
}

type sandboxAdmissionAttempt struct {
	preparedWorkAttempt
	owner *sandboxAdmissionProvider
}

func (p *sandboxAdmissionAttempt) Describe() ports.PreparedDescription {
	d := p.preparedWorkAttempt.Describe()
	switch p.owner.proof {
	case "prepared":
		d.HostSandboxPolicyHash = p.request.Spec.HostSandbox.PolicyHash
	case "wrong-proof":
		d.HostSandboxPolicyHash = strings.Repeat("e", 64)
	}
	return d
}
func (p *sandboxAdmissionAttempt) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	p.owner.releases++
	return p.preparedWorkAttempt.Release(ctx, permit)
}
