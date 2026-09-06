package app_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestAcceptedDecisionRecoversGraphTransitionAfterCASConflict(t *testing.T) {
	ctx := context.Background()
	store, service, now := regressionService(t)
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: ptr(regressionDecisionGraph()), Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	racing := app.New(graphConflictStore{Store: store}, providers.NewRegistry()).WithClock(func() time.Time { return now })
	result, err := racing.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "answer"}, DecisionID: run.Decisions[0].ID, ExpectedWindowRevision: 1, Answer: "approve", Reason: "reviewed"})
	require.NoError(t, err)
	require.Equal(t, model.DecisionAnswered, result.Window.State)

	restarted := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now.Add(time.Minute) })
	_, err = restarted.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	got, err := store.WorkRun(ctx, run.Run.ID)
	require.NoError(t, err)
	require.Equal(t, model.WorkRunSucceeded, got.Run.State)
}

func TestExpiredGraphSettlesWithoutPoisoningSweep(t *testing.T) {
	ctx := context.Background()
	store, service, now := regressionService(t)
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "wait", Nodes: []model.WorkNode{{ID: "wait", Kind: model.WorkNodeWait, Wait: &model.WaitPolicy{Duration: time.Minute}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "wait", To: "done"}}}
	_, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "expired"}, ID: "expired", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Minute)}})
	require.NoError(t, err)
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "later"}, ID: "later", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(4 * time.Hour)}})
	require.NoError(t, err)
	service.WithClock(func() time.Time { return now.Add(2 * time.Hour) })
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	first, err := store.WorkRun(ctx, "expired")
	require.NoError(t, err)
	require.Equal(t, model.WorkOutcomeExpired, first.Run.Outcome)
	second, err := store.WorkRun(ctx, "later")
	require.NoError(t, err)
	require.Equal(t, model.WorkRunSucceeded, second.Run.State)
}

func TestUnsupportedExternalWaitIsRejectedAtAdmission(t *testing.T) {
	_, service, now := regressionService(t)
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "wait", Nodes: []model.WorkNode{{ID: "wait", Kind: model.WorkNodeWait, Wait: &model.WaitPolicy{Until: "artifact:approved"}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "wait", To: "done"}}}
	_, err := service.StartProcess(context.Background(), app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.ErrorIs(t, err, app.ErrInvalid)
}

func TestFailedGraphRemainsPendingWhileSiblingOwnsEffect(t *testing.T) {
	ctx := context.Background()
	store, _, now := regressionService(t)
	provider := &preparedWorkProvider{}
	service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	for _, id := range []model.AgentID{"reviewer_a", "reviewer_b"} {
		_, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: id, Name: string(id), Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}})
		require.NoError(t, err)
	}
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "parallel", Nodes: []model.WorkNode{{ID: "parallel", Kind: model.WorkNodeFork}, {ID: "a", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "reviewer_a", Brief: "review"}}}, {ID: "b", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "reviewer_b", Brief: "review"}}}, {ID: "join", Kind: model.WorkNodeJoin, Join: &model.JoinPolicy{Mode: model.JoinAny}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "parallel", To: "a"}, {From: "parallel", To: "b"}, {From: "a", To: "join"}, {From: "b", To: "join"}, {From: "join", To: "done"}}}
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "parallel", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	for range 2 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	running, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: run.Run.ID})
	require.NoError(t, err)
	a := nodeAttempt(t, running.Run, "a")
	failed, err := service.RecordNodeEvidence(ctx, app.RecordNodeEvidenceRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "fail"}, Attempt: a.Ref, ExpectedRunRevision: running.Run.Revision, Disposition: model.WorkOutcomeRejected, Detail: "failed"})
	require.NoError(t, err)
	require.Equal(t, model.WorkControlDraining, failed.Run.ControlState)
	pending, err := store.PendingWorkRuns(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, pending)
}

func TestProgramReleaseRechecksEveryPinnedAuthority(t *testing.T) {
	ctx := context.Background()
	store, service, now := regressionService(t)
	host := &programHostFake{}
	service.WithProgramHost(host)
	createAgent(t, ctx, service, model.OperatorPrincipal(), "owner")
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: t.TempDir(), ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	profile, err := service.SaveProgramProfile(ctx, app.SaveProgramProfileRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "profile"}, ID: "profile", RevisionID: "profile_v1", Name: "profile", Executable: "check", Sandbox: model.SandboxWorkspaceWrite, Timeout: time.Minute, OutputLimitBytes: 1024, EffectAuthority: []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}, {Action: model.ActionInspectWorkspace, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}}})
	require.NoError(t, err)
	subject := model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "owner"}
	for _, grant := range []model.AuthorityGrant{{ID: "start", Subject: subject, Action: model.ActionStartWork, Resource: model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: "run"}}, {ID: "execute", Subject: subject, Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: "workspace"}}, {ID: "inspect", Subject: subject, Action: model.ActionInspectWorkspace, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: "workspace"}}} {
		_, err = service.PutGrant(ctx, app.PutGrantRequest{Principal: model.OperatorPrincipal(), Grant: grant})
		require.NoError(t, err)
	}
	graph := programGraph(profile)
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.AgentPrincipal("owner"), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Scope: model.WorkScope{WorkspaceID: "workspace"}, AuthorizedProgramProfiles: []model.ProgramProfileRef{{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}}, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: model.OperatorPrincipal(), GrantID: "inspect", ExpectedRevision: 1}))
	_, _ = service.ReconcilePendingWork(ctx)
	require.Zero(t, host.prepares)
}

func TestDisabledRuleSuppressesUnadmittedOccurrence(t *testing.T) {
	ctx := context.Background()
	store, service, now := regressionService(t)
	request := app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, ID: "rule", RevisionID: "rule_v1", Name: "rule", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: model.AutomationDelegation{Actions: []model.Action{model.ActionStartWork}, Resources: []model.ResourceSelector{{Kind: model.ResourceAutomationRule, AutomationRuleID: "rule"}}, ExpiresAt: now.Add(time.Hour)}, Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute, Anchor: now.Add(time.Hour)}}, Action: model.AutomationAction{Kind: model.AutomationStartWork, Work: &model.WorkStart{InlineGraph: ptr(regressionDecisionGraph()), Deadline: now.Add(time.Hour)}}, Policy: regressionOccurrencePolicy()}
	rule, err := service.SaveAutomationRule(ctx, request)
	require.NoError(t, err)
	occurrence, err := service.RunRuleNow(ctx, app.RunRuleNowRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "fire"}, RuleID: rule.Rule.ID, ExpectedRuleRevision: rule.Rule.Revision, OccurrenceID: "occurrence", SourceOccurrenceKey: "click"})
	require.NoError(t, err)
	request.Context.RequestID, request.RevisionID, request.ExpectedRevision, request.Enabled = "disable", "rule_v2", rule.Rule.Revision, false
	base := service
	service = app.New(disableGraphAdmissionStore{Store: store, before: func() {
		_, disableErr := base.SaveAutomationRule(ctx, request)
		require.NoError(t, disableErr)
	}}, providers.NewRegistry()).WithClock(func() time.Time { return now })
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	got, err := store.Occurrence(ctx, occurrence.Occurrence.ID)
	require.NoError(t, err)
	require.Equal(t, model.OccurrenceDenied, got.Occurrence.State)
	require.Empty(t, got.Occurrence.WorkRunID)
}

func TestDisabledRuleCannotRaceMessageAdmission(t *testing.T) {
	ctx := context.Background()
	store, service, now := regressionService(t)
	createAgent(t, ctx, service, model.OperatorPrincipal(), "recipient")
	request := app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, ID: "rule", RevisionID: "rule_v1", Name: "rule", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: model.AutomationDelegation{Actions: []model.Action{model.ActionSendMessage}, Resources: []model.ResourceSelector{{Kind: model.ResourceAgent, AgentID: "recipient"}}, ExpiresAt: now.Add(time.Hour)}, Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute, Anchor: now.Add(time.Hour)}}, Action: model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: "check", AgentIDs: []model.AgentID{"recipient"}}}, Policy: regressionOccurrencePolicy()}
	rule, err := service.SaveAutomationRule(ctx, request)
	require.NoError(t, err)
	occurrence, err := service.RunRuleNow(ctx, app.RunRuleNowRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "fire"}, RuleID: rule.Rule.ID, ExpectedRuleRevision: rule.Rule.Revision, OccurrenceID: "occurrence", SourceOccurrenceKey: "click", Recipients: []model.AgentID{"recipient"}})
	require.NoError(t, err)
	request.Context.RequestID, request.RevisionID, request.ExpectedRevision, request.Enabled = "disable", "rule_v2", rule.Rule.Revision, false
	base := service
	service = app.New(disableMessageAdmissionStore{Store: store, before: func() {
		_, disableErr := base.SaveAutomationRule(ctx, request)
		require.NoError(t, disableErr)
	}}, providers.NewRegistry()).WithClock(func() time.Time { return now })
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	got, err := store.Occurrence(ctx, occurrence.Occurrence.ID)
	require.NoError(t, err)
	require.Equal(t, model.OccurrenceDenied, got.Occurrence.State)
	require.Empty(t, got.Occurrence.OperationID)
}

func TestDisabledRuleCannotRaceTeamAdmission(t *testing.T) {
	ctx := context.Background()
	store, service, now := regressionService(t)
	desired := model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "builder", Name: "builder", Desired: desired, Required: true}}, Waves: []model.TeamWave{{ID: "build", MemberKeys: []string{"builder"}, RequiredReady: true}}}
	definition, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "team"}, Draft: app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "team", Team: &team}})
	require.NoError(t, err)
	teamRef := model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionTeam}
	request := app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, ID: "rule", RevisionID: "rule_v1", Name: "rule", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: model.AutomationDelegation{Actions: []model.Action{model.ActionRunAutomation, model.ActionStartWork, model.ActionLaunch}, Resources: []model.ResourceSelector{{Kind: model.ResourceAutomationRule, AutomationRuleID: "rule"}, {Kind: model.ResourceGroupPeers, GroupID: "group"}}, Bounds: model.ConfigurationBounds{Harnesses: []string{desired.Harness}, Models: []string{desired.Model}, WorkingDirectoryRoots: []string{desired.WorkingDirectory}, ApprovalModes: []model.ApprovalMode{desired.Approval}, SandboxModes: []model.SandboxMode{desired.Sandbox}}, ExpiresAt: now.Add(time.Hour)}, Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute, Anchor: now.Add(time.Hour)}}, Action: model.AutomationAction{Kind: model.AutomationDeployTeam, Team: &model.TeamInstantiation{Definition: teamRef, Mission: "ship", GroupID: "group"}}, Policy: regressionOccurrencePolicy(), Dependencies: []model.DefinitionRef{teamRef}}
	rule, err := service.SaveAutomationRule(ctx, request)
	require.NoError(t, err)
	occurrence, err := service.RunRuleNow(ctx, app.RunRuleNowRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "fire"}, RuleID: rule.Rule.ID, ExpectedRuleRevision: rule.Rule.Revision, OccurrenceID: "occurrence", SourceOccurrenceKey: "click"})
	require.NoError(t, err)
	request.Context.RequestID, request.RevisionID, request.ExpectedRevision, request.Enabled = "disable", "rule_v2", rule.Rule.Revision, false
	base := service
	service = app.New(disableTeamAdmissionStore{Store: store, before: func() {
		_, disableErr := base.SaveAutomationRule(ctx, request)
		require.NoError(t, disableErr)
	}}, providers.NewRegistry()).WithClock(func() time.Time { return now })
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	got, err := store.Occurrence(ctx, occurrence.Occurrence.ID)
	require.NoError(t, err)
	require.Equal(t, model.OccurrenceDenied, got.Occurrence.State)
	require.Empty(t, got.Occurrence.DeploymentID)
}

func TestGraphCannotLaunchRetiredAgent(t *testing.T) {
	ctx := context.Background()
	store, _, now := regressionService(t)
	provider := &preparedWorkProvider{}
	service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	agent, err := service.CreateAgent(ctx, app.CreateAgentRequest{
		Context: operator,
		ID:      "worker",
		Name:    "worker",
		Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite, WorkingDirectory: t.TempDir()},
	})
	require.NoError(t, err)
	retired, err := service.RetireAgent(ctx, app.RetireAgentRequest{Context: operator, ID: agent.Agent.ID, ExpectedRevision: agent.Agent.Revision, Reason: "finished"})
	require.NoError(t, err)
	_, err = service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "direct"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.Agent.ID, ExpectedRevision: retired.Agent.Revision}}})
	require.ErrorIs(t, err, app.ErrConflict)
	graph := model.WorkGraph{
		CompilerVersion: "1",
		EntryNodeID:     "task",
		Nodes: []model.WorkNode{
			{ID: "task", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: agent.Agent.ID, Brief: "work"}}},
			{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
		},
		Edges: []model.WorkEdge{{From: "task", To: "done"}},
	}
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: effect(operator, "graph"), ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	_, _ = service.ReconcilePendingWork(ctx)
	require.Empty(t, provider.preparations, "graph issuance must enforce the same retired lifecycle gate as ordinary launch")
}

func TestAgentRetirementRacingGraphIssuancePreventsLaunch(t *testing.T) {
	ctx := context.Background()
	store, _, now := regressionService(t)
	provider := &preparedWorkProvider{}
	operator := model.OperatorPrincipal()
	base := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	agent, err := base.CreateAgent(ctx, app.CreateAgentRequest{
		Context: operator,
		ID:      "worker",
		Name:    "worker",
		Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite, WorkingDirectory: t.TempDir()},
	})
	require.NoError(t, err)
	racingStore := &retireGraphLaunchStore{Store: store, before: func() {
		_, retireErr := store.RetireAgent(ctx, agent.Agent.ID, agent.Agent.Revision, operator, "retired during issuance", now)
		require.NoError(t, retireErr)
	}}
	service := app.New(racingStore, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	graph := model.WorkGraph{
		CompilerVersion: "1",
		EntryNodeID:     "task",
		Nodes: []model.WorkNode{
			{ID: "task", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: agent.Agent.ID, Brief: "work"}}},
			{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
		},
		Edges: []model.WorkEdge{{From: "task", To: "done"}},
	}
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: effect(operator, "graph_race"), ID: "race_run", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	_, _ = service.ReconcilePendingWork(ctx)
	require.Empty(t, provider.preparations, "retirement committed before issuance must prevent a fresh graph launch")
}

func TestScheduledAutomationDeploysTeamThroughDelegatedEffects(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 6, 20, 0, 0, 0, time.UTC)
	provider := &preparedWorkProvider{}
	service := app.New(lostTeamLinkReplyStore{Store: store}, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	directory := t.TempDir()
	desired := model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: directory, Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "builder", Name: "builder", Desired: desired, Required: true}}, Waves: []model.TeamWave{{ID: "build", MemberKeys: []string{"builder"}, RequiredReady: true}}}
	definition, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "team"}, Draft: app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "team", Team: &team}})
	require.NoError(t, err)
	teamRef := model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionTeam}
	delegation := model.AutomationDelegation{Actions: []model.Action{model.ActionRunAutomation, model.ActionStartWork, model.ActionLaunch}, Resources: []model.ResourceSelector{{Kind: model.ResourceAutomationRule, AutomationRuleID: "deploy_rule"}, {Kind: model.ResourceGroupPeers, GroupID: "deployed_group"}}, Bounds: model.ConfigurationBounds{Harnesses: []string{desired.Harness}, Models: []string{desired.Model}, WorkingDirectoryRoots: []string{directory}, ApprovalModes: []model.ApprovalMode{desired.Approval}, SandboxModes: []model.SandboxMode{desired.Sandbox}}, ExpiresAt: now.Add(time.Hour)}
	ruleRequest := app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "rule"}, ID: "deploy_rule", RevisionID: "deploy_rule_v1", Name: "deploy", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: delegation, Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute, Anchor: now.Add(time.Minute)}}, Action: model.AutomationAction{Kind: model.AutomationDeployTeam, Team: &model.TeamInstantiation{Definition: teamRef, Mission: "ship", GroupID: "deployed_group"}}, Policy: regressionOccurrencePolicy(), Dependencies: []model.DefinitionRef{teamRef}}
	rule, err := service.SaveAutomationRule(ctx, ruleRequest)
	require.NoError(t, err)
	now = now.Add(time.Minute)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	ruleRequest.Context.RequestID, ruleRequest.RevisionID, ruleRequest.ExpectedRevision, ruleRequest.Enabled = "disable", "deploy_rule_v2", rule.Rule.Revision, false
	_, err = service.SaveAutomationRule(ctx, ruleRequest)
	require.NoError(t, err)
	for range 3 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	occurrences, err := service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: model.OperatorPrincipal(), RuleID: "deploy_rule"})
	require.NoError(t, err)
	require.Len(t, occurrences, 1)
	require.Equal(t, model.OccurrenceDelivered, occurrences[0].Occurrence.State)
	require.NotEmpty(t, occurrences[0].Occurrence.DeploymentID)
}

func TestOutcomePolicyPreventsUnqualifiedSuccess(t *testing.T) {
	ctx := context.Background()
	store, service, now := regressionService(t)
	graph := regressionDecisionGraph()
	graph.Nodes = append(graph.Nodes, model.WorkNode{ID: "check", Kind: model.WorkNodeWait, Wait: &model.WaitPolicy{Duration: time.Minute}})
	graph.Edges = []model.WorkEdge{{From: "approve", To: "done", Verdict: "approve"}, {From: "approve", To: "check", Verdict: "reject"}, {From: "check", To: "done"}}
	graph.Outcome = model.WorkGraphOutcomePolicy{RequiredNodes: []model.WorkNodeID{"check"}, ArtifactRevision: "artifact-sha", HumanJudgment: true}
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	_, err = service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "answer"}, DecisionID: run.Decisions[0].ID, ExpectedWindowRevision: 1, Answer: "approve", Reason: "direct path"})
	require.NoError(t, err)
	got, err := store.WorkRun(ctx, run.Run.ID)
	require.NoError(t, err)
	require.Equal(t, model.WorkRunFailed, got.Run.State)
}

func TestAuthoredDeploymentFlagCannotCompleteOrdinaryTask(t *testing.T) {
	ctx := context.Background()
	store, _, now := regressionService(t)
	provider := &preparedWorkProvider{}
	service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	_, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "agent", Name: "agent", Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite, WorkingDirectory: t.TempDir()}})
	require.NoError(t, err)
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "task", Nodes: []model.WorkNode{{ID: "task", Kind: model.WorkNodeTask, Input: map[string]json.RawMessage{"deployment_launch": json.RawMessage("true")}, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "agent", Brief: "work"}}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "task", To: "done"}}}
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	got, err := store.WorkRun(ctx, run.Run.ID)
	require.NoError(t, err)
	require.Equal(t, model.WorkRunRunning, got.Run.State)
}

func TestStandingGuidanceRequiresTargetAndInteractionAuthority(t *testing.T) {
	for _, test := range []struct {
		name       string
		target     model.AgentID
		delegation model.AutomationDelegation
	}{
		{name: "wrong target", target: "other", delegation: model.AutomationDelegation{Actions: []model.Action{model.ActionInteract}, Resources: []model.ResourceSelector{{Kind: model.ResourceAgent, AgentID: "other"}}}},
		{name: "wrong action", target: "worker", delegation: model.AutomationDelegation{Actions: []model.Action{model.ActionRunAutomation}, Resources: []model.ResourceSelector{{Kind: model.ResourceAutomationRule, AutomationRuleID: "standing"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, _, now := regressionService(t)
			provider := &preparedWorkProvider{}
			service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now }).WithCallbackIngress(noopCallbackIngress{})
			_, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "worker", Name: "worker", Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}})
			require.NoError(t, err)
			test.delegation.ExpiresAt = now.Add(time.Hour)
			_, err = service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "rule"}, ID: "standing", RevisionID: "standing_v1", Name: "standing", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: test.delegation, Condition: model.AutomationCondition{Kind: model.AutomationStandingOrder, StandingOrder: &model.StandingOrderCondition{FactKind: "prompt", Pattern: "review", Timing: model.StandingOrderSameContinuation, DispatchDeadline: time.Minute}}, Action: model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: "guidance", AgentIDs: []model.AgentID{test.target}}}, Policy: regressionOccurrencePolicy()})
			require.NoError(t, err)
			graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "task", Nodes: []model.WorkNode{{ID: "task", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "worker", Brief: "work"}}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "task", To: "done"}}}
			_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
			require.NoError(t, err)
			_, err = service.ReconcilePendingWork(ctx)
			require.NoError(t, err)
			_, err = provider.preparation.NativeGuidance.EvaluateNativeGuidance(ctx, ports.NormalizedNativeEvent{EventID: "event", Kind: "prompt", ObservedAt: now, OccurredAt: now, NativeCorrelation: "turn", Payload: json.RawMessage(`{"text":"review"}`), Timing: model.StandingOrderSameContinuation})
			require.Error(t, err)
		})
	}
}

func TestTargetedStandingGuidanceRequiresCallbackIngress(t *testing.T) {
	ctx := context.Background()
	store, _, now := regressionService(t)
	provider := &preparedWorkProvider{}
	service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	_, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "worker", Name: "worker", Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	_, err = service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "rule"}, ID: "standing", RevisionID: "standing_v1", Name: "standing", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: model.AutomationDelegation{Actions: []model.Action{model.ActionInteract}, Resources: []model.ResourceSelector{{Kind: model.ResourceAgent, AgentID: "worker"}}, ExpiresAt: now.Add(time.Hour)}, Condition: model.AutomationCondition{Kind: model.AutomationStandingOrder, StandingOrder: &model.StandingOrderCondition{FactKind: "prompt", Pattern: "review", Timing: model.StandingOrderSameContinuation, DispatchDeadline: time.Minute}}, Action: model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: "guidance", AgentIDs: []model.AgentID{"worker"}}}, Policy: regressionOccurrencePolicy()})
	require.NoError(t, err)
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "task", Nodes: []model.WorkNode{{ID: "task", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "worker", Brief: "work"}}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "task", To: "done"}}}
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.ErrorIs(t, err, app.ErrUnavailable)
	require.Empty(t, provider.preparations)
}

func TestDefinitionDefaultsAndPerformerBindingsMaterialize(t *testing.T) {
	ctx := context.Background()
	store, _, now := regressionService(t)
	provider := &preparedWorkProvider{}
	service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	_, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "worker", Name: "worker", Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "task", Nodes: []model.WorkNode{{ID: "task", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{MemberKey: "implementer", Brief: "work"}}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "task", To: "done"}}}
	definition, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "definition"}, Draft: app.DefinitionDraft{ID: "process", RevisionID: "process_v1", Name: "process", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "process", Parameters: []model.ParameterDeclaration{{Name: "mode", Type: model.ParameterString, Required: true, Default: json.RawMessage(`"safe"`)}}, Process: &model.ProcessDefinition{Graph: graph}}})
	require.NoError(t, err)
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{Definition: &model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionProcess}, PerformerBindings: map[string]model.Performer{"implementer": {Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "worker"}}}, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	require.JSONEq(t, `"safe"`, string(run.Run.Parameters["mode"]))
	require.Equal(t, model.AgentID("worker"), run.Run.Graph.Nodes[0].Performer.Agent.AgentID)
	require.Empty(t, run.Run.Graph.Nodes[0].Performer.Agent.MemberKey)
}

func regressionService(t *testing.T) (*sqlite.Store, *app.Service, time.Time) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 6, 20, 0, 0, 0, time.UTC)
	return store, app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now }), now
}

func regressionDecisionGraph() model.WorkGraph {
	return model.WorkGraph{CompilerVersion: "1", EntryNodeID: "approve", Nodes: []model.WorkNode{{ID: "approve", Kind: model.WorkNodeDecision, Decision: &model.DecisionNode{Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"approve", "reject"}, ExpiresAfter: time.Hour}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "approve", To: "done"}}}
}

func regressionOccurrencePolicy() model.OccurrencePolicy {
	return model.OccurrencePolicy{MissedTicks: model.MissedTickSkip, OfflineDelivery: model.OfflineSkip, ExpiresAfter: time.Hour, Overlap: model.OverlapForbid, MaxActive: 1, Deadline: time.Minute, Retry: model.RetryPolicy{MaxAttempts: 1}}
}

type graphConflictStore struct{ app.Store }

func (graphConflictStore) ApplyGraphTransition(context.Context, app.GraphTransition) (app.WorkRunRecord, error) {
	return app.WorkRunRecord{}, app.ErrConflict
}

type retireGraphLaunchStore struct {
	app.Store
	before func()
	once   sync.Once
}

func (s *retireGraphLaunchStore) ApplyGraphTransition(ctx context.Context, transition app.GraphTransition) (app.WorkRunRecord, error) {
	if transition.Execution != nil {
		s.once.Do(s.before)
	}
	return s.Store.ApplyGraphTransition(ctx, transition)
}

type disableGraphAdmissionStore struct {
	app.Store
	before func()
}

func (s disableGraphAdmissionStore) CreateGraphWorkRun(ctx context.Context, run model.WorkRun, windows []model.DecisionWindow) (app.WorkRunRecord, bool, error) {
	s.before()
	return s.Store.CreateGraphWorkRun(ctx, run, windows)
}

type disableMessageAdmissionStore struct {
	app.Store
	before func()
}

func (s disableMessageAdmissionStore) CreateMessage(ctx context.Context, message model.Message, requestID model.RequestID, operationID model.OperationID, authority []model.AuthorityRequest) (app.MessageAdmissionResult, error) {
	s.before()
	return s.Store.CreateMessage(ctx, message, requestID, operationID, authority)
}

type disableTeamAdmissionStore struct {
	app.Store
	before func()
}

type lostTeamLinkReplyStore struct{ app.Store }

func (s lostTeamLinkReplyStore) UpdateOccurrence(ctx context.Context, id model.OccurrenceID, revision model.Revision, state model.OccurrenceState, operationID model.OperationID, workRunID model.WorkRunID, deploymentID model.DeploymentID, recipients []model.OccurrenceRecipient, at time.Time) (app.OccurrenceRecord, error) {
	record, err := s.Store.UpdateOccurrence(ctx, id, revision, state, operationID, workRunID, deploymentID, recipients, at)
	if err == nil && state == model.OccurrenceAdmitted && deploymentID != "" {
		return record, context.DeadlineExceeded
	}
	return record, err
}

func (s disableTeamAdmissionStore) CreateTeamDeployment(ctx context.Context, deployment model.TeamDeployment, group model.Group, agents []model.Agent, principal model.Principal, at time.Time) (model.TeamDeployment, bool, error) {
	s.before()
	return s.Store.CreateTeamDeployment(ctx, deployment, group, agents, principal, at)
}

func ptr[T any](value T) *T { return &value }
