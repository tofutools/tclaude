package app_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestPartialProfileOptionsPersistOmissionsAndExactRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := testService(store, newFakeProvider())
	off := false
	options := &model.ConfigurationOptions{AutoReview: &off, Environment: model.Environment{"LITERAL": "$HOME\n spaced "}}
	req := app.SaveConfigurationProfileRequest{Context: effect(model.OperatorPrincipal(), "save_partial"), ID: "partial", RevisionID: "first", Name: "Reusable options", Options: options}
	saved, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, saved.Revision.Options)
	require.Nil(t, saved.Revision.Options.Harness)
	require.Nil(t, saved.Revision.Options.WorkingDirectory)
	require.Nil(t, saved.Revision.Options.Approval)
	require.True(t, saved.Revision.Desired.Equal(model.DesiredConfiguration{}))
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	service = testService(store, newFakeProvider())
	reopened, err := service.GetConfigurationProfile(ctx, model.OperatorPrincipal(), saved.Revision.Ref)
	require.NoError(t, err)
	require.Equal(t, saved, reopened)
	replayed, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.Equal(t, saved, replayed)
	changed := *options
	changed.AutoReview = nil
	req.Options = &changed
	_, err = service.SaveConfigurationProfile(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict, "omission is not the same intent as explicit false")
}

func TestPartialProfileOptionsValidateAuthoredFieldsWithoutRequiringDefaults(t *testing.T) {
	ctx := context.Background()
	_, service, _ := regressionService(t)
	bad := model.ApprovalMode("not-a-policy")
	req := app.SaveConfigurationProfileRequest{Context: effect(model.OperatorPrincipal(), "invalid_partial"), ID: "partial", RevisionID: "first", Name: "Partial", Options: &model.ConfigurationOptions{Approval: &bad}}
	_, err := service.SaveConfigurationProfile(ctx, req)
	require.ErrorIs(t, err, app.ErrInvalid)
	claude, enabled := "claude", true
	req.Options = &model.ConfigurationOptions{Harness: &claude, AutoReview: &enabled}
	_, err = service.SaveConfigurationProfile(ctx, req)
	require.ErrorIs(t, err, app.ErrInvalid)
	req.Options = &model.ConfigurationOptions{}
	req.Desired.Harness = "claude"
	_, err = service.SaveConfigurationProfile(ctx, req)
	require.ErrorIs(t, err, app.ErrInvalid)
	req.Desired = model.DesiredConfiguration{}
	_, err = service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err, "rejected saves must not consume the request identity")
}

type partialOptionsProvider struct {
	*fakeProvider
	name   string
	policy ports.PolicyRequirements
}

func (p *partialOptionsProvider) Name() string { return p.name }
func (p *partialOptionsProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{LaunchPolicy: &p.policy}
}

func TestPartialProfilesResolveCurrentDefaultsAndPreserveCreatedAgentSettings(t *testing.T) {
	ctx := context.Background()
	store, _, _ := regressionService(t)
	codex := &partialOptionsProvider{fakeProvider: newFakeProvider(), name: "codex", policy: ports.PolicyRequirements{DefaultApproval: model.ApprovalOnRequest, DefaultSandbox: model.SandboxWorkspaceWrite, SupportedApproval: []model.ApprovalMode{model.ApprovalOnRequest}, SupportedSandbox: []model.SandboxMode{model.SandboxWorkspaceWrite}}}
	copilot := &partialOptionsProvider{fakeProvider: newFakeProvider(), name: "copilot", policy: ports.PolicyRequirements{DefaultApproval: model.ApprovalAutomatic, DefaultSandbox: model.SandboxUnconfined, SupportedApproval: []model.ApprovalMode{model.ApprovalAutomatic}, SupportedSandbox: []model.SandboxMode{model.SandboxUnconfined}}}
	service := app.New(store, providers.NewRegistry(codex, copilot))
	op := model.OperatorPrincipal()
	globalReq := app.SaveConfigurationProfileRequest{Context: effect(op, "global"), ID: "global", RevisionID: "first", Name: "Global", Desired: model.DesiredConfiguration{Harness: "codex", Model: "global-model", Effort: "medium", WorkingDirectory: t.TempDir(), Approval: model.ApprovalOnRequest, Sandbox: model.SandboxWorkspaceWrite, AutoReview: true, FastMode: model.FastModeOn, Environment: model.Environment{"FROM_GLOBAL": "retained"}}}
	global, err := service.SaveConfigurationProfile(ctx, globalReq)
	require.NoError(t, err)
	_, err = service.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: effect(op, "defaults"), Global: &global.Revision.Ref})
	require.NoError(t, err)
	chosen, off := "member-model", false
	partial, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "partial"), ID: "partial", RevisionID: "first", Name: "Partial", Options: &model.ConfigurationOptions{Model: &chosen, AutoReview: &off}})
	require.NoError(t, err)
	first, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "first", Name: "First", ConfigurationProfile: &partial.Revision.Ref})
	require.NoError(t, err)
	require.Equal(t, "member-model", first.Agent.Desired.Model)
	require.Equal(t, "medium", first.Agent.Desired.Effort)
	require.False(t, first.Agent.Desired.AutoReview)
	require.Equal(t, model.FastModeOn, first.Agent.Desired.FastMode)
	globalReq.Context.RequestID, globalReq.RevisionID, globalReq.ExpectedRevision = "global_edit", "second", 1
	globalReq.Desired.Effort = "high"
	_, err = service.SaveConfigurationProfile(ctx, globalReq)
	require.NoError(t, err)
	second, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "second", Name: "Second", ConfigurationProfile: &partial.Revision.Ref})
	require.NoError(t, err)
	require.Equal(t, "high", second.Agent.Desired.Effort)
	retained, err := store.Agent(ctx, first.Agent.ID)
	require.NoError(t, err)
	require.Equal(t, "medium", retained.Desired.Effort)
	harness := "copilot"
	foreign, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "foreign"), ID: "foreign", RevisionID: "first", Name: "Copilot", Options: &model.ConfigurationOptions{Harness: &harness}})
	require.NoError(t, err)
	third, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "third", Name: "Third", ConfigurationProfile: &foreign.Revision.Ref})
	require.NoError(t, err)
	require.Empty(t, third.Agent.Desired.Model)
	require.Empty(t, third.Agent.Desired.Effort)
	require.Empty(t, third.Agent.Desired.FastMode)
	require.False(t, third.Agent.Desired.AutoReview)
	require.Equal(t, model.ApprovalAutomatic, third.Agent.Desired.Approval)
	require.Equal(t, model.SandboxUnconfined, third.Agent.Desired.Sandbox)
	require.Equal(t, "retained", third.Agent.Desired.Environment["FROM_GLOBAL"])
}

func TestPartialProfileTransferPreservesOptionsAndRetry(t *testing.T) {
	ctx := context.Background()
	_, service, _ := regressionService(t)
	off := false
	bundle := app.ConfigurationBundle{Format: app.ConfigurationBundleFormat, Version: 1, Profiles: []app.ConfigurationBundleEntry{{Key: "portable", Name: "Portable", Options: &model.ConfigurationOptions{AutoReview: &off, Environment: model.Environment{"VALUE": "literal"}}}}}
	inspected, err := service.InspectConfigurationBundle(ctx, model.OperatorPrincipal(), bundle)
	require.NoError(t, err)
	req := app.ImportConfigurationsRequest{Context: effect(model.OperatorPrincipal(), "import_partial"), Bundle: inspected, Selections: []app.ConfigurationImportSelection{{Key: "portable", ID: "restored", RevisionID: "first", Name: "Restored"}}}
	imported, err := service.ImportConfigurations(ctx, req)
	require.NoError(t, err)
	require.Len(t, imported.Profiles, 1)
	ref := imported.Profiles[0].Revision.Ref
	read, err := service.GetConfigurationProfile(ctx, model.OperatorPrincipal(), ref)
	require.NoError(t, err)
	require.Equal(t, bundle.Profiles[0].Options, read.Revision.Options)
	require.Nil(t, read.Revision.Options.Harness)
	require.NotNil(t, read.Revision.Options.AutoReview)
	require.False(t, *read.Revision.Options.AutoReview)
	repeated, err := service.ImportConfigurations(ctx, req)
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	changed := *req.Bundle.Profiles[0].Options
	changed.AutoReview = nil
	req.Bundle.Profiles[0].Options = &changed
	_, err = service.ImportConfigurations(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
}

func TestPartialGroupProfileFencesInheritedEditAndRetainsCommittedRetry(t *testing.T) {
	ctx := context.Background()
	store, _, _ := regressionService(t)
	provider := &partialOptionsProvider{fakeProvider: newFakeProvider(), name: "codex", policy: ports.PolicyRequirements{DefaultApproval: model.ApprovalOnRequest, DefaultSandbox: model.SandboxWorkspaceWrite, SupportedApproval: []model.ApprovalMode{model.ApprovalOnRequest}, SupportedSandbox: []model.SandboxMode{model.SandboxWorkspaceWrite}}}
	registry := providers.NewRegistry(provider)
	service := app.New(store, registry)
	op := model.OperatorPrincipal()
	_, err := service.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team"})
	require.NoError(t, err)
	globalReq := app.SaveConfigurationProfileRequest{Context: effect(op, "global"), ID: "global", RevisionID: "first", Name: "Global", Desired: model.DesiredConfiguration{Harness: "codex", Model: "first", WorkingDirectory: t.TempDir(), Approval: model.ApprovalOnRequest, Sandbox: model.SandboxWorkspaceWrite}}
	global, err := service.SaveConfigurationProfile(ctx, globalReq)
	require.NoError(t, err)
	_, err = service.SaveConfigurationDefaults(ctx, app.SaveConfigurationDefaultsRequest{Context: effect(op, "defaults"), Global: &global.Revision.Ref})
	require.NoError(t, err)
	partial, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "partial"), ID: "partial", RevisionID: "first", Name: "Partial", Options: &model.ConfigurationOptions{}})
	require.NoError(t, err)
	groupDefaults, err := service.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "team", Profile: &partial.Revision.Ref})
	require.NoError(t, err)
	wrapped := &editingGroupProfileStore{Store: store, before: func() {
		globalReq.Context.RequestID, globalReq.RevisionID, globalReq.ExpectedRevision = "global_edit", "second", 1
		globalReq.Desired.Model = "second"
		_, err := service.SaveConfigurationProfile(ctx, globalReq)
		require.NoError(t, err)
	}}
	racing := app.New(wrapped, registry)
	req := app.CreateGroupMemberRequest{Context: effect(op, "member"), GroupID: "team", ID: "member", Name: "Member", ExpectedGroupRevision: 1, ExpectedDefaultRevision: groupDefaults.Revision}
	_, err = racing.CreateGroupMember(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = store.Agent(ctx, "member")
	require.ErrorIs(t, err, app.ErrNotFound)
	created, err := service.CreateGroupMember(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "second", created.Agent.Desired.Model)
	globalReq.Context.RequestID, globalReq.RevisionID, globalReq.ExpectedRevision = "global_later", "third", 2
	globalReq.Desired.Model = "third"
	_, err = service.SaveConfigurationProfile(ctx, globalReq)
	require.NoError(t, err)
	repeated, err := service.CreateGroupMember(ctx, req)
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	require.Equal(t, "second", repeated.Agent.Desired.Model)
}

func TestPartialProfileLaunchOverridesLeaveReusableProfileUnchanged(t *testing.T) {
	ctx := context.Background()
	store, _, _ := regressionService(t)
	codex := &partialOptionsProvider{fakeProvider: newFakeProvider(), name: "codex", policy: ports.PolicyRequirements{DefaultApproval: model.ApprovalOnRequest, DefaultSandbox: model.SandboxWorkspaceWrite, SupportedApproval: []model.ApprovalMode{model.ApprovalOnRequest}, SupportedSandbox: []model.SandboxMode{model.SandboxWorkspaceWrite}}}
	service := app.New(store, providers.NewRegistry(codex))
	op := model.OperatorPrincipal()
	profileModel := "reusable-model"
	profile, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "portable"), ID: "portable", RevisionID: "one", Name: "Portable", Options: &model.ConfigurationOptions{Model: &profileModel, Environment: model.Environment{"SOURCE": "profile"}}})
	require.NoError(t, err)
	harness, cwd, off := "codex", t.TempDir(), false
	launch := &model.ConfigurationOptions{Harness: &harness, WorkingDirectory: &cwd, AutoReview: &off, Environment: model.Environment{"SOURCE": "launch"}}
	req := app.CreateAgentRequest{Context: op, ID: "created", Name: "Created", ConfigurationProfile: &profile.Revision.Ref, ConfigurationOverrides: launch}
	created, err := service.CreateAgent(ctx, req)
	require.NoError(t, err, "explicit harness must resolve before provider lookup; Claude is not configured")
	require.Equal(t, cwd, created.Agent.Desired.WorkingDirectory)
	require.Equal(t, model.ApprovalOnRequest, created.Agent.Desired.Approval)
	require.Equal(t, "launch", created.Agent.Desired.Environment["SOURCE"])
	require.Equal(t, profileModel, created.Agent.Desired.Model)
	require.False(t, created.Agent.Desired.AutoReview)
	cwd = t.TempDir()
	updated, err := service.UpdateAgent(ctx, app.UpdateAgentRequest{Context: op, ID: created.Agent.ID, ExpectedRevision: created.Agent.Revision, Name: "Updated", ConfigurationProfile: &profile.Revision.Ref, ConfigurationOverrides: launch})
	require.NoError(t, err)
	require.Equal(t, cwd, updated.Agent.Desired.WorkingDirectory)
	reopened, err := service.GetConfigurationProfile(ctx, op, profile.Revision.Ref)
	require.NoError(t, err)
	require.Equal(t, profile, reopened)
	require.Nil(t, reopened.Revision.Options.Harness)
	require.Nil(t, reopened.Revision.Options.WorkingDirectory)

	bad := model.ApprovalBypassPermissions
	launch.Approval = &bad
	req.ID = "invalid"
	_, err = service.CreateAgent(ctx, req)
	require.ErrorIs(t, err, app.ErrInvalid, "explicit foreign policy must be refused, not skipped")
	_, err = store.Agent(ctx, req.ID)
	require.ErrorIs(t, err, app.ErrNotFound)
	launch.Approval = nil
	empty := ""
	launch.WorkingDirectory = &empty
	_, err = service.CreateAgent(ctx, req)
	require.ErrorIs(t, err, app.ErrInvalid, "an actual agent still needs a working directory")
}

func TestPartialGroupProfileAcceptsLaunchContextAndFencesRetryIntent(t *testing.T) {
	ctx := context.Background()
	store, _, _ := regressionService(t)
	provider := &partialOptionsProvider{fakeProvider: newFakeProvider(), name: "codex", policy: ports.PolicyRequirements{DefaultApproval: model.ApprovalOnRequest, DefaultSandbox: model.SandboxWorkspaceWrite, SupportedApproval: []model.ApprovalMode{model.ApprovalOnRequest}, SupportedSandbox: []model.SandboxMode{model.SandboxWorkspaceWrite}}}
	service := app.New(store, providers.NewRegistry(provider))
	op := model.OperatorPrincipal()
	_, err := service.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team"})
	require.NoError(t, err)
	profile, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "portable"), ID: "portable", RevisionID: "one", Name: "Portable", Options: &model.ConfigurationOptions{}})
	require.NoError(t, err)
	defaults, err := service.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "team", Profile: &profile.Revision.Ref})
	require.NoError(t, err, "selecting a reusable default requires neither a fabricated cwd nor an ambient harness")
	cwd, harness, off := t.TempDir(), "codex", false
	req := app.CreateGroupMemberRequest{Context: effect(op, "create_member"), GroupID: "team", ID: "member", Name: "Member", ExpectedGroupRevision: 1, ExpectedDefaultRevision: defaults.Revision, ConfigurationOverrides: &model.ConfigurationOptions{Harness: &harness, WorkingDirectory: &cwd, AutoReview: &off}}
	created, err := service.CreateGroupMember(ctx, req)
	require.NoError(t, err)
	require.Equal(t, cwd, created.Agent.Desired.WorkingDirectory)
	require.Equal(t, "codex", created.Agent.Desired.Harness)
	replayed, err := service.CreateGroupMember(ctx, req)
	require.NoError(t, err)
	require.True(t, replayed.Repeated)
	require.Equal(t, created.Agent, replayed.Agent)
	changed := *req.ConfigurationOverrides
	changed.AutoReview = nil
	req.ConfigurationOverrides = &changed
	_, err = service.CreateGroupMember(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict, "even false versus omission is different launch intent")
	profileAfter, err := service.GetConfigurationProfile(ctx, op, profile.Revision.Ref)
	require.NoError(t, err)
	require.Equal(t, profile, profileAfter)
}

func TestPartialProfileDefaultSelectionDefersLaunchContext(t *testing.T) {
	ctx := context.Background()
	store, _, _ := regressionService(t)
	provider := &partialOptionsProvider{fakeProvider: newFakeProvider(), name: "codex", policy: ports.PolicyRequirements{DefaultApproval: model.ApprovalOnRequest, DefaultSandbox: model.SandboxWorkspaceWrite, SupportedApproval: []model.ApprovalMode{model.ApprovalOnRequest}, SupportedSandbox: []model.SandboxMode{model.SandboxWorkspaceWrite}}}
	service := app.New(store, providers.NewRegistry(provider))
	op := model.OperatorPrincipal()
	profileReq := app.SaveConfigurationProfileRequest{Context: effect(op, "portable"), ID: "portable", RevisionID: "one", Name: "Portable", Options: &model.ConfigurationOptions{}}
	profile, err := service.SaveConfigurationProfile(ctx, profileReq)
	require.NoError(t, err)
	defaultsReq := app.SaveConfigurationDefaultsRequest{Context: effect(op, "select"), Global: &profile.Revision.Ref, Harnesses: map[string]model.ConfigurationProfileRef{"codex": profile.Revision.Ref}}
	defaults, err := service.SaveConfigurationDefaults(ctx, defaultsReq)
	require.NoError(t, err, "saving inherited intent must not require the fallback provider or cwd")
	cwd := t.TempDir()
	created, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "default_agent", Name: "Default", ConfigurationDefault: "codex", ConfigurationOverrides: &model.ConfigurationOptions{WorkingDirectory: &cwd}})
	require.NoError(t, err)
	require.Equal(t, "codex", created.Agent.Desired.Harness)
	require.Equal(t, cwd, created.Agent.Desired.WorkingDirectory)
	claude := "claude"
	profileReq.Options = &model.ConfigurationOptions{Harness: &claude}
	profileReq.RevisionID, profileReq.ExpectedRevision, profileReq.Context.RequestID = "two", 1, "edit"
	_, err = service.SaveConfigurationProfile(ctx, profileReq)
	require.NoError(t, err)
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "mismatch", Name: "Mismatch", ConfigurationDefault: "codex", ConfigurationOverrides: &model.ConfigurationOptions{WorkingDirectory: &cwd}})
	require.ErrorIs(t, err, app.ErrConflict, "a changed explicit harness must invalidate its old per-harness selection")
	replay, err := service.SaveConfigurationDefaults(ctx, defaultsReq)
	require.NoError(t, err)
	require.Equal(t, defaults, replay, "completed receipt must survive later profile changes")
}

func TestPartialTeamProfileValidatesNativeOverridesAgainstAuthoredHarness(t *testing.T) {
	ctx := context.Background()
	_, service, _ := regressionService(t)
	op := model.OperatorPrincipal()
	fast, auto, tools := model.FastModeOn, true, model.ToolGovernanceDeny
	for _, tc := range []struct {
		name      string
		harness   string
		overrides model.TeamProfileOverrides
	}{
		{"fast", "codex", model.TeamProfileOverrides{FastMode: &fast}},
		{"review", "codex", model.TeamProfileOverrides{AutoReview: &auto}},
		{"tools", "opencode", model.TeamProfileOverrides{ToolGovernance: &tools}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, harness := range []string{"", tc.harness, "claude"} {
				id := model.ConfigurationProfileID(tc.name + "_" + harness + "profile")
				options := &model.ConfigurationOptions{}
				if harness != "" {
					options.Harness = &harness
				}
				_, err := service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, model.RequestID(id)), ID: id, RevisionID: "one", Name: string(id), Options: options})
				require.NoError(t, err)
				team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", ProfileID: id, Overrides: &tc.overrides}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}}}}
				draft := app.DefinitionDraft{ID: model.DefinitionID(id), RevisionID: "one", Name: string(id), Source: "partial native options", Kind: model.DefinitionTeam, SchemaVersion: 1, Team: &team}
				_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: op, Draft: draft})
				if harness == "claude" {
					require.ErrorIs(t, err, app.ErrInvalid)
				} else {
					require.NoError(t, err)
				}
			}
		})
	}
}
