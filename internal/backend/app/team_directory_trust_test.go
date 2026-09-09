//go:build linux || darwin

package app_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestDirectoryTrustTeamAdmissionCarriesProofIntoDeferredProcess(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := sqlite.Open(database)
	require.NoError(t, err)
	now := time.Now().UTC()
	directory := t.TempDir()
	op := model.OperatorPrincipal()
	parentProvider := &peerMessagingProvider{newFakeProvider()}
	service := app.New(store, providers.NewRegistry(parentProvider)).WithDirectoryWriteProof(host.DirectoryProof{}).WithClock(func() time.Time { return now })
	desired := model.DesiredConfiguration{Harness: "claude", Model: "worker", WorkingDirectory: directory, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	parent, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "owner", Name: "Owner", Desired: desired})
	require.NoError(t, err)
	_, err = service.Launch(ctx, app.LaunchRequest{RequestContext: effect(op, "owner_launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: parent.Agent.ID, ExpectedRevision: parent.Agent.Revision}}})
	require.NoError(t, err)
	desired.TrustDirectory = true
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: directory, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", Desired: desired, Required: true}}, Waves: []model.TeamWave{{ID: "start", MemberKeys: []string{"worker"}, RequiredReady: true}}}
	definition, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: effect(op, "team"), Draft: app.DefinitionDraft{ID: "team", RevisionID: "team_one", Name: "Team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "team", Team: &team}})
	require.NoError(t, err)
	ref := model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionTeam}
	instantiation := model.TeamInstantiation{Definition: ref, Mission: "Do the work", GroupID: "group", Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}
	owner := model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "owner"}
	delegation := model.AutomationDelegation{Actions: []model.Action{model.ActionRunAutomation, model.ActionStartWork, model.ActionLaunch, model.ActionInspectWorkspace}, Resources: []model.ResourceSelector{{Kind: model.ResourceAutomationRule, AutomationRuleID: "rule"}, {Kind: model.ResourceGroupPeers, GroupID: "group"}, {Kind: model.ResourceWorkspace, WorkspaceID: "workspace"}}, Bounds: model.ConfigurationBounds{Harnesses: []string{"claude"}, Models: []string{"worker"}, WorkingDirectoryRoots: []string{directory}, ApprovalModes: []model.ApprovalMode{desired.Approval}, SandboxModes: []model.SandboxMode{desired.Sandbox}}, ExpiresAt: now.Add(time.Hour)}
	for i, pair := range []struct {
		action   model.Action
		resource model.ResourceSelector
	}{{model.ActionRunAutomation, delegation.Resources[0]}, {model.ActionStartWork, delegation.Resources[0]}, {model.ActionLaunch, delegation.Resources[1]}, {model.ActionInspectWorkspace, delegation.Resources[2]}} {
		_, err = service.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: model.GrantID(fmt.Sprintf("grant_%d", i)), Subject: owner, Action: pair.action, Resource: pair.resource, Bounds: delegation.Bounds}})
		require.NoError(t, err)
	}
	rule, err := service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: effect(op, "rule"), ID: "rule", RevisionID: "rule_one", Name: "Rule", Enabled: true, Owner: owner, Delegation: delegation, Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute, Anchor: now.Add(time.Minute)}}, Action: model.AutomationAction{Kind: model.AutomationDeployTeam, Team: &instantiation}, Policy: regressionOccurrencePolicy(), Dependencies: []model.DefinitionRef{ref}})
	require.NoError(t, err)
	principal := model.AutomationPrincipal("occurrence", owner, delegation)
	_, _, err = store.MaterializeOccurrence(ctx, model.AutomationOccurrence{ID: "occurrence", RuleID: rule.Rule.ID, RuleRevisionID: rule.Revision.ID, SourceOccurrenceKey: "test", RequestID: "deploy", Requester: principal, ScheduledAt: now, EligibleAt: now, ExpiresAt: now.Add(time.Hour), State: model.OccurrencePending, Revision: 1, CreatedAt: now, UpdatedAt: now}, rule.Rule.Revision)
	require.NoError(t, err)
	request := app.DeployTeamRequest{Context: effect(principal, "deploy"), DeploymentID: "deployment", Instantiation: instantiation}
	_, err = service.DeployTeam(ctx, request)
	var challenge *app.DirectoryProofRequired
	require.ErrorAs(t, err, &challenge)
	_, err = store.TeamDeployment(ctx, "deployment")
	require.ErrorIs(t, err, app.ErrNotFound)
	for _, dir := range challenge.Directories {
		require.NoError(t, os.WriteFile(filepath.Join(dir, challenge.Filename), nil, 0600))
	}
	request.Context.WriteProofToken = challenge.Token
	now = now.Add(time.Second) // Generated timestamps must not change proof identity.
	deployed, err := service.DeployTeam(ctx, request)
	require.NoError(t, err)
	member := deployed.Deployment.Members["worker"]
	require.NotEmpty(t, deployed.Deployment.DirectoryTrust[member].ProvenDirectories)
	for _, dir := range challenge.Directories {
		require.NoFileExists(t, filepath.Join(dir, challenge.Filename))
	}
	require.NoError(t, store.Close())
	store, err = sqlite.Open(database)
	require.NoError(t, err)
	defer store.Close()
	retained, err := store.TeamDeployment(ctx, "deployment")
	require.NoError(t, err)
	require.Equal(t, deployed.Deployment.DirectoryTrust, retained.DirectoryTrust)
	run, err := store.WorkRun(ctx, retained.WorkRunID)
	require.NoError(t, err)
	require.NotEmpty(t, run.Run.DirectoryTrust.Nodes)
	provider := &trustWorkProvider{preparedWorkProvider: &preparedWorkProvider{}}
	service = app.New(store, providers.NewRegistry(provider)).WithDirectoryWriteProof(host.DirectoryProof{}).WithClock(func() time.Time { return now })
	for range 3 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	require.NotEmpty(t, provider.preparations)
	require.True(t, provider.preparation.Spec.TrustDirectory)
}
