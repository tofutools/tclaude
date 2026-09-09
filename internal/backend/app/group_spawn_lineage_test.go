package app_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/providers/codex"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func ownerLineageFixture(t *testing.T, paths ...string) (*sqlite.Store, *app.Service, app.CreateGroupMemberRequest, model.Execution) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	if len(paths) != 0 {
		path = paths[0]
	}
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	svc := app.New(store, providers.NewRegistry(&codex.Provider{}))
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "codex", Model: "parent-model", WorkingDirectory: t.TempDir(), Approval: model.ApprovalNever, Sandbox: model.SandboxWorkspaceWrite}
	parent, err := svc.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "owner", Name: "Owner", Desired: desired})
	require.NoError(t, err)
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team", Members: []model.AgentID{parent.Agent.ID}})
	require.NoError(t, err)
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "other", Name: "Other", Members: []model.AgentID{parent.Agent.ID}})
	require.NoError(t, err)
	owned, err := svc.SetGroupOwner(ctx, app.SetGroupOwnerRequest{Principal: op, GroupID: "team", OwnerAgentIDs: []model.AgentID{parent.Agent.ID}, ExpectedGroupRevision: 1})
	require.NoError(t, err)
	child := desired
	child.Model = "different-worker-model"
	child.WorkingDirectory = t.TempDir()
	saved, err := svc.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile"}, ID: "worker", RevisionID: "one", Name: "Worker", Desired: child})
	require.NoError(t, err)
	_, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "team", Profile: &saved.Revision.Ref})
	require.NoError(t, err)
	now := time.Now().UTC()
	execution := model.Execution{ID: "parent_exec", AgentID: parent.Agent.ID, ConversationID: "parent_conversation", State: model.ExecutionReserved, Attempt: 1, Revision: 1, CreatedAt: now, UpdatedAt: now, Spec: model.ResolvedExecutionSpec{ExecutionID: "parent_exec", AgentID: parent.Agent.ID, ConversationID: "parent_conversation", Attempt: 1, Harness: desired.Harness, Model: desired.Model, WorkingDirectory: desired.WorkingDirectory, Approval: desired.Approval, Sandbox: desired.Sandbox}}
	_, err = store.AdmitLaunch(ctx, app.LaunchAdmission{AgentID: parent.Agent.ID, Expected: parent.Agent.Revision, Execution: execution, Operation: model.Operation{ID: "parent_launch", RequestID: "parent_launch", Kind: model.OperationLaunch, Principal: op, ExecutionID: execution.ID, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now}, Authority: model.AuthorityRequest{Principal: op, Action: model.ActionLaunch, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: parent.Agent.ID}, RequestedConfiguration: &desired}})
	require.NoError(t, err)
	payload, _ := json.Marshal(map[string]string{"execution_id": string(execution.ID)})
	evidence, err := model.NewProviderEvidence("codex", 1, payload)
	require.NoError(t, err)
	execution, err = store.RecordRecovery(ctx, execution.ID, model.ExecutionRunning, nil, evidence, now)
	require.NoError(t, err)
	return store, svc, app.CreateGroupMemberRequest{Context: app.RequestContext{Principal: model.AgentPrincipal(parent.Agent.ID), RequestID: "spawn"}, GroupID: "team", ID: "child", Name: "Child", ExpectedGroupRevision: owned.Group.Revision, ExpectedDefaultRevision: 1}, execution
}

func TestGroupOwnerSpawnUsesRunningLineageWithoutParentModelOrDirectoryLimits(t *testing.T) {
	ctx := context.Background()
	store, svc, in, parent := ownerLineageFixture(t)
	result, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.NotEqual(t, parent.Spec.Model, result.Agent.Desired.Model)
	require.NotEqual(t, parent.Spec.WorkingDirectory, result.Agent.Desired.WorkingDirectory)
	require.Contains(t, result.Group.Members, model.AgentID("child"))
	replay, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.True(t, replay.Repeated)
	require.Equal(t, result.Agent, replay.Agent)
	changed := in
	changed.Name = "Changed"
	_, err = svc.CreateGroupMember(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = store.RecordRecovery(ctx, parent.ID, model.ExecutionExited, nil, parent.Evidence, time.Now())
	require.NoError(t, err)
	_, err = svc.CreateGroupMember(ctx, in)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	retained, err := store.Agent(ctx, "child")
	require.NoError(t, err)
	require.Equal(t, result.Agent, retained)
}

func TestGroupOwnerSpawnRejectsBroaderOrUnknownLineage(t *testing.T) {
	for _, mode := range []string{"sandbox", "reviewer", "unknown", "outside", "denial"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store, svc, in, parent := ownerLineageFixture(t)
			switch mode {
			case "sandbox":
				sandbox := model.SandboxUnconfined
				in.ConfigurationOverrides = &model.ConfigurationOptions{Sandbox: &sandbox}
			case "reviewer":
				approval := model.ApprovalOnRequest
				review := true
				in.ConfigurationOverrides = &model.ConfigurationOptions{Approval: &approval, AutoReview: &review}
			case "unknown":
				_, err := store.RecordRecovery(ctx, parent.ID, model.ExecutionRunning, nil, model.ProviderEvidence{Provider: "codex", Version: 99, Payload: []byte("unknown")}, time.Now())
				require.NoError(t, err)
			case "outside":
				in.GroupID = "other"
				in.ExpectedGroupRevision = 1
				in.ExpectedDefaultRevision = 0
			case "denial":
				_, err := svc.PutDenial(ctx, app.PutDenialRequest{Principal: model.OperatorPrincipal(), Denial: model.AuthorityDenial{ID: "no_spawn", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "owner"}, Action: model.ActionCreateGroupMember}})
				require.NoError(t, err)
			}
			_, err := svc.CreateGroupMember(ctx, in)
			require.ErrorIs(t, err, app.ErrUnauthorized)
			_, err = store.Agent(ctx, "child")
			require.ErrorIs(t, err, app.ErrNotFound)
		})
	}
}

// The provider boundary is simulated; policy classification is the production
// Codex implementation and admission/effect permits use the real SQLite store.
type lineageCodexProvider struct {
	launchBriefProvider
	releases int
}

func (*lineageCodexProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{HostSandbox: true}
}
func (*lineageCodexProvider) Name() string { return "codex" }
func (*lineageCodexProvider) ApprovalPosture(d model.DesiredConfiguration) model.ApprovalPosture {
	return (&codex.Provider{}).ApprovalPosture(d)
}
func (*lineageCodexProvider) RequestedSandboxPosture(s model.ResolvedExecutionSpec) model.SandboxPosture {
	return (&codex.Provider{}).RequestedSandboxPosture(s)
}
func (*lineageCodexProvider) RecordedSandboxPosture(e model.Execution) model.SandboxPosture {
	return (&codex.Provider{}).RecordedSandboxPosture(e)
}
func (p *lineageCodexProvider) Prepare(_ context.Context, r ports.PreparationRequest) (ports.PreparedAttempt, error) {
	return &lineageCodexAttempt{preparedWorkAttempt: preparedWorkAttempt{request: r}, owner: p}, nil
}

type lineageCodexAttempt struct {
	preparedWorkAttempt
	owner *lineageCodexProvider
}

func (p *lineageCodexAttempt) Describe() ports.PreparedDescription {
	d := ports.PreparedDescription{ExecutionID: p.request.Spec.ExecutionID, Attempt: p.request.Spec.Attempt, Topology: ports.TopologyIndependentServer, Requirements: ports.RuntimeRequirements{WorkingDirectory: p.request.Spec.WorkingDirectory, Loopback: &ports.LoopbackRequirement{Protocol: "http"}}, EffectivePolicy: ports.EffectivePolicy{Approval: p.request.Spec.Approval, Sandbox: p.request.Spec.Sandbox, ApprovalEnforced: true, SandboxEnforced: true}}
	if p.request.Spec.HostSandbox != nil {
		d.HostSandboxPolicyHash = p.request.Spec.HostSandbox.PolicyHash
	}
	d.Evidence = model.ProviderEvidence{Provider: "codex", Version: 1, Payload: []byte(`{"execution_id":"` + string(p.request.Spec.ExecutionID) + `"}`)}
	return d
}
func (p *lineageCodexAttempt) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, err
	}
	p.owner.releases++
	return ports.ReleaseResult{State: ports.ReleaseUncertain, Evidence: p.Describe().Evidence}, nil
}

type lineageAdmissionRace struct {
	*sqlite.Store
	before func()
}

func (s *lineageAdmissionRace) AdmitGroupMember(ctx context.Context, in app.GroupMemberAdmission) (app.GroupMemberResult, error) {
	s.before()
	return s.Store.AdmitGroupMember(ctx, in)
}
func TestGroupOwnerSpawnRechecksParentAtPublication(t *testing.T) {
	ctx := context.Background()
	store, _, in, parent := ownerLineageFixture(t)
	racing := &lineageAdmissionRace{Store: store, before: func() {
		_, err := store.RecordRecovery(ctx, parent.ID, model.ExecutionExited, nil, parent.Evidence, time.Now())
		require.NoError(t, err)
	}}
	svc := app.New(racing, providers.NewRegistry(&codex.Provider{}))
	_, err := svc.CreateGroupMember(ctx, in)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	_, err = store.Agent(ctx, "child")
	require.ErrorIs(t, err, app.ErrNotFound)
	group, err := store.Group(ctx, "team")
	require.NoError(t, err)
	require.Equal(t, in.ExpectedGroupRevision, group.Revision)
}
func TestGroupOwnerSpawnLaunchReceiptRetainsLineageAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reopen.sqlite")
	store, _, in, parent := ownerLineageFixture(t, path)
	provider := &lineageCodexProvider{}
	svc := app.New(store, providers.NewRegistry(provider))
	in.Launch = &app.GroupMemberLaunch{}
	out, err := svc.CreateGroupMember(ctx, in)
	require.ErrorIs(t, err, app.ErrUncertain)
	require.NotNil(t, out.Operation)
	require.Equal(t, 1, provider.releases)
	// New profile values govern future effects, not the already committed receipt.
	updated := out.Agent.Desired
	updated.Model = "later-model"
	_, err = svc.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "edit"}, ID: "worker", RevisionID: "two", ExpectedRevision: 1, Name: "Worker", Desired: updated})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	// Receipt authorization must survive reopening without fresh provider setup.
	svc = app.New(store, providers.NewRegistry())
	replay, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.True(t, replay.Repeated)
	require.Equal(t, out.Operation.Operation.ID, replay.Operation.Operation.ID)
	require.Equal(t, 1, provider.releases)
	_, err = store.RecordRecovery(ctx, parent.ID, model.ExecutionExited, nil, parent.Evidence, time.Now())
	require.NoError(t, err)
	_, err = svc.CreateGroupMember(ctx, in)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	require.Equal(t, 1, provider.releases)
}

func TestGroupOwnerSpawnPreservesHostSandboxSelectionAtLaunch(t *testing.T) {
	ctx := context.Background()
	store, _, in, _ := ownerLineageFixture(t)
	provider := &lineageCodexProvider{}
	inspector, err := host.NewSandboxPathInspector([]string{t.TempDir()})
	require.NoError(t, err)
	svc := app.New(store, providers.NewRegistry(provider)).WithSandboxPathInspector(inspector)
	op := model.OperatorPrincipal()
	profile, err := svc.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "sandbox"}, ID: "child_sandbox", Name: "Child sandbox", Policy: model.SandboxPolicy{Environment: model.Environment{"SANDBOX": "child"}}})
	require.NoError(t, err)
	_, err = svc.SaveSandboxDefaults(ctx, app.SaveSandboxDefaultsRequest{Context: app.RequestContext{Principal: op, RequestID: "sandbox_defaults"}, Groups: map[model.GroupID]model.SandboxProfileID{"team": profile.Profile.ID}})
	require.NoError(t, err)
	in.Launch = &app.GroupMemberLaunch{}
	out, err := svc.CreateGroupMember(ctx, in)
	require.ErrorIs(t, err, app.ErrUncertain)
	require.NotNil(t, out.Operation)
	require.NotNil(t, out.Operation.Execution.Spec.HostSandbox)
	require.Equal(t, model.GroupID("team"), out.Operation.Execution.Spec.HostSandbox.GroupID)
	require.NotEmpty(t, out.Operation.Execution.Spec.HostSandbox.PolicyHash)
	require.Equal(t, profile.Profile.ID, out.Operation.Execution.Spec.HostSandbox.Scopes[0].Ref.ProfileID)
	// Committed authorization uses the original resolved boundary after defaults change.
	_, err = svc.SaveSandboxDefaults(ctx, app.SaveSandboxDefaultsRequest{Context: app.RequestContext{Principal: op, RequestID: "clear_sandbox_defaults"}, ExpectedRevision: 1})
	require.NoError(t, err)
	svc = app.New(store, providers.NewRegistry())
	replay, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.True(t, replay.Repeated)
	require.Equal(t, out.Operation.Execution.ID, replay.Operation.Execution.ID)
	require.Equal(t, 1, provider.releases)
}
func TestGroupOwnerSpawnKeepsExplicitOwnerConfigurationBounds(t *testing.T) {
	ctx := context.Background()
	store, svc, in, _ := ownerLineageFixture(t)
	owned, err := svc.SetGroupOwner(ctx, app.SetGroupOwnerRequest{Principal: model.OperatorPrincipal(), GroupID: "team", OwnerAgentIDs: []model.AgentID{"owner"}, ExpectedGroupRevision: in.ExpectedGroupRevision, Bounds: model.ConfigurationBounds{Models: []string{"restricted-model"}}})
	require.NoError(t, err)
	in.ExpectedGroupRevision = owned.Group.Revision
	_, err = svc.CreateGroupMember(ctx, in)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	_, err = store.Agent(ctx, "child")
	require.ErrorIs(t, err, app.ErrNotFound)
}
