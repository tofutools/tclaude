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
	forged.Scopes[0].Ref.ProfileID = "missing"
	desired.HostSandbox = &forged
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "forged", Name: "Forged", Desired: desired})
	require.ErrorIs(t, err, app.ErrNotFound)
	_, err = store.Agent(ctx, "forged")
	require.ErrorIs(t, err, app.ErrNotFound)
	_, err = service.SetSandboxProfileArchived(ctx, app.SetSandboxProfileArchivedRequest{Context: app.RequestContext{Principal: operator, RequestID: "archive"}, ID: profile.Profile.ID, ExpectedRevision: profile.Profile.Revision, Archived: true})
	require.NoError(t, err)
	_, err = service.ResolveLaunchSandbox(ctx, operator, scopes)
	require.ErrorIs(t, err, app.ErrConflict)
	saved, err := store.Agent(ctx, "agent")
	require.NoError(t, err)
	require.Equal(t, &selected, saved.Desired.HostSandbox, "archiving a profile does not change the saved profile ID")
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
	fresh.ID = "fresh-profile"
	fresh.RevisionID = "fresh-revision"
	_, err = service.SaveConfigurationProfile(ctx, fresh)
	require.NoError(t, err, "saving a profile choice needs no host inspection")
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
			require.True(t, model.SameSandboxProfiles(&selection, &resolved))
			require.Equal(t, "retained", prepared.HostSandboxPolicy.Composition.Values.Environment["POLICY_VALUE"])
			if scenario != "prepared" {
				require.ErrorIs(t, err, app.ErrInvalid)
				require.Zero(t, provider.releases)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, provider.releases)
			require.True(t, model.SameSandboxProfiles(&selection, result.Execution.Spec.HostSandbox))
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
			require.Equal(t, result.Execution.Spec.HostSandbox, retry.Execution.Spec.HostSandbox)
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

// Characterizes v1 sandbox_resume.go: the saved profile ID survives rename and
// content changes; every new start resolves that ID against the current registry.
func TestSandboxProfileUpdateAppliesOnAgentRestart(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	defer store.Close()
	paths, err := host.NewSandboxPathInspector([]string{t.TempDir()})
	require.NoError(t, err)
	provider := &sandboxAdmissionProvider{supported: true, proof: "prepared"}
	service := app.New(store, providers.NewRegistry(provider)).WithSandboxPathInspector(paths)
	operator := model.OperatorPrincipal()
	profile, err := service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: request(operator, "profile"), ID: "sandbox", Name: "Sandbox", Policy: model.SandboxPolicy{Environment: model.Environment{"VALUE": "first"}}})
	require.NoError(t, err)
	choice := model.SandboxSelection{Scopes: []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: model.SandboxProfileRef{ProfileID: profile.Profile.ID}}}}
	agent, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "agent", Name: "Agent", Desired: model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite, HostSandbox: &choice}})
	require.NoError(t, err)
	launch := app.LaunchRequest{RequestContext: request(operator, "first-start"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.Agent.ID, ExpectedRevision: agent.Agent.Revision}}, InitialMessage: "Work"}
	first, err := service.Launch(ctx, launch)
	require.NoError(t, err)
	require.Equal(t, "first", provider.preparations[0].HostSandboxPolicy.Composition.Values.Environment["VALUE"])
	_, err = service.Stop(ctx, app.StopRequest{RequestContext: request(operator, "stop"), ExecutionID: first.Execution.ID})
	require.NoError(t, err)
	_, err = service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: request(operator, "edit"), ID: profile.Profile.ID, ExpectedRevision: profile.Profile.Revision, Name: "Renamed sandbox", Policy: model.SandboxPolicy{Environment: model.Environment{"VALUE": "second"}}})
	require.NoError(t, err)
	// A response retry is the same operation, not a new restart.
	repeated, err := service.Launch(ctx, launch)
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	require.Len(t, provider.preparations, 1)
	current, err := store.Agent(ctx, agent.Agent.ID)
	require.NoError(t, err)
	require.Equal(t, &choice, current.Desired.HostSandbox)
	launch.RequestID = "second-start"
	launch.Target.Agent.ExpectedRevision = current.Revision
	second, err := service.Launch(ctx, launch)
	require.NoError(t, err)
	require.NotEqual(t, first.Execution.ID, second.Execution.ID)
	require.Equal(t, "second", provider.preparations[1].HostSandboxPolicy.Composition.Values.Environment["VALUE"])
	require.Equal(t, profile.Profile.ID, second.Execution.Spec.HostSandbox.Scopes[0].Ref.ProfileID)
}

func TestSandboxIncludesFollowCurrentProfilesAndExport(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(map[bool]string{false: "ID include", true: "legacy exact include"}[exact], func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
			require.NoError(t, err)
			defer store.Close()
			paths, err := host.NewSandboxPathInspector([]string{t.TempDir()})
			require.NoError(t, err)
			service := app.New(store, providers.NewRegistry()).WithSandboxPathInspector(paths)
			operator := model.OperatorPrincipal()
			parent, err := service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: request(operator, "parent"), ID: "parent", Name: "Parent", Policy: model.SandboxPolicy{Environment: model.Environment{"VALUE": "first"}}})
			require.NoError(t, err)
			include := model.SandboxProfileRef{ProfileID: parent.Profile.ID}
			if exact {
				include = parent.Revision.Ref
			}
			policy := model.SandboxPolicy{Includes: []model.SandboxProfileRef{include}}
			child, err := service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: request(operator, "child"), ID: "child", Name: "Child", Policy: policy})
			require.NoError(t, err)
			_, err = service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: request(operator, "edit-parent"), ID: parent.Profile.ID, ExpectedRevision: parent.Profile.Revision, Name: "Renamed parent", Policy: model.SandboxPolicy{Environment: model.Environment{"VALUE": "second"}}})
			require.NoError(t, err)
			preview, err := service.PreviewSandboxPolicy(ctx, operator, policy)
			require.NoError(t, err)
			require.Equal(t, "second", preview.Composition.Values.Environment["VALUE"])
			bundle, err := service.ExportSandboxBundle(ctx, operator, child.Revision.Ref)
			require.NoError(t, err)
			require.Len(t, bundle.Entries, 2)
			require.Equal(t, "second", bundle.Entries[0].Policy.Environment["VALUE"])
			selections := []app.SandboxImportSelection{}
			for _, entry := range bundle.Entries {
				selections = append(selections, app.SandboxImportSelection{Source: entry.Ref, ID: model.SandboxProfileID("copy_" + string(entry.Ref.ProfileID)), RevisionID: model.SandboxProfileRevisionID("copy_" + string(entry.Ref.RevisionID)), Name: entry.Name + " copy"})
			}
			imported, err := service.ImportSandboxProfiles(ctx, app.ImportSandboxProfilesRequest{Context: request(operator, "import"), Bundle: bundle, Selections: selections})
			require.NoError(t, err)
			copied, err := service.GetSandboxProfile(ctx, operator, imported.Root.ProfileID)
			require.NoError(t, err)
			require.Equal(t, model.SandboxProfileID("copy_parent"), copied.Revision.Policy.Includes[0].ProfileID)
		})
	}
}
