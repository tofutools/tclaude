package app_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestProcessDefinitionPinsProgramProfileAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	now := time.Date(2026, 9, 6, 20, 0, 0, 0, time.UTC)
	sequence := 0
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now }).WithIDGenerator(func(prefix string) string { sequence++; return prefix + "stable_" + string(rune('a'+sequence)) })
	operator := model.OperatorPrincipal()

	profile, err := service.SaveProgramProfile(ctx, app.SaveProgramProfileRequest{
		Context: app.RequestContext{Principal: operator, RequestID: "save_profile"}, ID: "check", RevisionID: "check_v1", Name: "bounded check",
		Executable: "go", ArgumentPrefix: []string{"test"}, Sandbox: model.SandboxWorkspaceWrite,
		Timeout: time.Minute, OutputLimitBytes: 1 << 20,
		EffectAuthority: []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}},
	})
	require.NoError(t, err)

	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "check_node", Nodes: []model.WorkNode{
		{ID: "check_node", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerProgram, Program: &model.ProgramPerformer{Profile: model.ProgramProfileRef{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}}}},
		{ID: "done_node", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
	}, Edges: []model.WorkEdge{{From: "check_node", To: "done_node"}}, Outcome: model.WorkGraphOutcomePolicy{RequiredNodes: []model.WorkNodeID{"check_node"}}}
	saveDefinition := app.SaveDefinitionRequest{Context: app.RequestContext{Principal: operator, RequestID: "save_definition"}, Draft: app.DefinitionDraft{ID: "verify", RevisionID: "verify_v1", Name: "verify", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "kind: process", Parameters: []model.ParameterDeclaration{{Name: "package", Type: model.ParameterString, Required: true}}, Process: &model.ProcessDefinition{Graph: graph}}}
	definition, err := service.SaveDefinition(ctx, saveDefinition)
	require.NoError(t, err)
	repeatedDefinition, err := service.SaveDefinition(ctx, saveDefinition)
	require.NoError(t, err)
	require.Equal(t, definition.Revision.ID, repeatedDefinition.Revision.ID)
	changedRequest := saveDefinition
	changedRequest.Draft.Source = "kind: changed"
	_, err = service.SaveDefinition(ctx, changedRequest)
	require.ErrorIs(t, err, app.ErrConflict)

	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: operator, RequestID: "start_process"}, ID: "run_verify", Start: model.WorkStart{Definition: &model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionProcess}, AuthorizedProgramProfiles: []model.ProgramProfileRef{{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}}, Parameters: map[string]json.RawMessage{"package": json.RawMessage(`"./internal/backend/..."`)}, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunRunning, run.Run.State)
	require.Equal(t, model.NodeAttemptReady, run.Run.NodeAttempts[0].State)
	require.NotEmpty(t, run.Run.NodeAttempts[0].Ref.ActivationID)
	require.Empty(t, run.Run.NodeAttempts[0].Ref.IssuanceID, "unissued work is safe to resume")

	updatedDraft := saveDefinition.Draft
	updatedDraft.RevisionID = "verify_v2"
	updatedDraft.Source = "kind: process\nrevision: 2"
	updated, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: operator, RequestID: "save_definition_v2"}, Draft: updatedDraft, ExpectedRevision: definition.Definition.Revision})
	require.NoError(t, err)
	require.NotEqual(t, definition.Revision.ContentHash, updated.Revision.ContentHash)

	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	restarted := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now.Add(time.Minute) })
	inspected, err := restarted.InspectWork(ctx, app.InspectWorkRequest{Principal: operator, WorkRunID: run.Run.ID})
	require.NoError(t, err)
	require.Equal(t, definition.Revision.ContentHash, inspected.Run.DefinitionClosure[0].ContentHash)
	require.Equal(t, profile.Revision.ID, inspected.Run.AuthorizedPrograms[0].RevisionID)
	require.Equal(t, run.Run.NodeAttempts[0].Ref, inspected.Run.NodeAttempts[0].Ref)
}

func TestProgramGraphUsesDurableIssuanceAndAcknowledgesOutput(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 6, 21, 0, 0, 0, time.UTC)
	host := &programHostFake{}
	service := app.New(store, providers.NewRegistry()).WithProgramHost(host).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: t.TempDir(), ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	profile, err := service.SaveProgramProfile(ctx, app.SaveProgramProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "profile"}, ID: "check", RevisionID: "check_v1", Name: "check", Executable: "check", Sandbox: model.SandboxWorkspaceWrite, Timeout: time.Minute, OutputLimitBytes: 1024, EffectAuthority: []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}}})
	require.NoError(t, err)
	graph := programGraph(profile)
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: operator, RequestID: "start"}, ID: "program_run", Start: model.WorkStart{InlineGraph: &graph, Scope: model.WorkScope{WorkspaceID: "workspace"}, AuthorizedProgramProfiles: []model.ProgramProfileRef{{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}}, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	report, err := service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	require.Empty(t, report.Uncertain)
	finished, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: operator, WorkRunID: run.Run.ID})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunSucceeded, finished.Run.State)
	require.Equal(t, model.WorkOutcomeVerified, finished.Run.Outcome)
	var issuance model.WorkIssuanceID
	for _, attempt := range finished.Run.NodeAttempts {
		if attempt.Ref.NodeID == "program" {
			issuance = attempt.Ref.IssuanceID
		}
	}
	require.NotEmpty(t, issuance)
	require.Equal(t, 1, host.prepares)
	require.True(t, host.runtime.releasedResources)
	uses, err := store.ActiveWorkspaceUses(ctx, "workspace")
	require.NoError(t, err)
	require.Empty(t, uses)
}

func TestUncertainProgramReleaseIsNeverReplayed(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 6, 21, 0, 0, 0, time.UTC)
	host := &programHostFake{uncertainRelease: true}
	service := app.New(store, providers.NewRegistry()).WithProgramHost(host).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: t.TempDir(), ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	profile, err := service.SaveProgramProfile(ctx, app.SaveProgramProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "profile"}, ID: "check", RevisionID: "check_v1", Name: "check", Executable: "check", Sandbox: model.SandboxWorkspaceWrite, Timeout: time.Minute, OutputLimitBytes: 1024, EffectAuthority: []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}}})
	require.NoError(t, err)
	graph := programGraph(profile)
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: operator, RequestID: "start"}, ID: "program_run", Start: model.WorkStart{InlineGraph: &graph, Scope: model.WorkScope{WorkspaceID: "workspace"}, AuthorizedProgramProfiles: []model.ProgramProfileRef{{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}}, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	first, err := service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	require.Equal(t, []model.WorkRunID{"program_run"}, first.Uncertain)
	second, err := service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	require.Equal(t, []model.WorkRunID{"program_run"}, second.Uncertain)
	require.Equal(t, 1, host.prepares)
}

func TestAgentPreparedWorkThenHumanDecisionAdvancesExactGraph(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 6, 22, 0, 0, 0, time.UTC)
	provider := &preparedWorkProvider{}
	service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "worker", Name: "worker", Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "work", Nodes: []model.WorkNode{
		{ID: "work", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "worker", ContextPolicy: model.AgentContextFresh, Brief: "review the exact change"}}},
		{ID: "approve", Kind: model.WorkNodeDecision, Name: "approve", Decision: &model.DecisionNode{Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"approve", "reject"}, ExpiresAfter: time.Hour}},
		{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
	}, Edges: []model.WorkEdge{{From: "work", To: "approve"}, {From: "approve", To: "done"}}, Outcome: model.WorkGraphOutcomePolicy{RequiredNodes: []model.WorkNodeID{"work", "approve"}}}
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: operator, RequestID: "start_agent_graph"}, ID: "agent_graph", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(2 * time.Hour)}})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	running, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: operator, WorkRunID: run.Run.ID})
	require.NoError(t, err)
	require.Equal(t, model.NodeAttemptRunning, running.Run.NodeAttempts[0].State)
	require.NotEmpty(t, running.Run.NodeAttempts[0].Ref.IssuanceID)
	require.NotNil(t, provider.preparation.InitialInput)
	require.True(t, provider.preparation.InitialInput.RequiredBeforeFirstWork)
	require.Equal(t, "review the exact change", provider.preparation.InitialInput.Body)
	require.Equal(t, string(running.Run.NodeAttempts[0].Ref.IssuanceID), provider.preparation.InitialInput.Correlation)
	passed := true
	waiting, err := service.RecordNodeEvidence(ctx, app.RecordNodeEvidenceRequest{Context: app.RequestContext{Principal: operator, RequestID: "agent_evidence"}, Attempt: running.Run.NodeAttempts[0].Ref, ExpectedRunRevision: running.Run.Revision, Kind: model.WorkEvidenceVerification, ArtifactRevision: "artifact-v1", Passed: &passed, Detail: "verified"})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunWaiting, waiting.Run.State)
	require.Len(t, waiting.NodeEvidence, 1)
	require.Len(t, waiting.Decisions, 1)
	answered, err := service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: operator, RequestID: "approve"}, DecisionID: waiting.Decisions[0].ID, ExpectedWindowRevision: waiting.Decisions[0].Revision, Answer: "approve", Reason: "human review complete"})
	require.NoError(t, err)
	require.Equal(t, model.DecisionAnswered, answered.Window.State)
	finished, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: operator, WorkRunID: run.Run.ID})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunSucceeded, finished.Run.State)
}

func TestScheduledOccurrencePinsTickAndDeduplicatesAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	now := time.Date(2026, 9, 6, 23, 0, 0, 0, time.UTC)
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "approve", Nodes: []model.WorkNode{{ID: "approve", Kind: model.WorkNodeDecision, Name: "scheduled approval", Decision: &model.DecisionNode{Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"approve"}, ExpiresAfter: time.Hour}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "approve", To: "done"}}}
	rule, err := service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: operator, RequestID: "save_schedule"}, ID: "schedule", RevisionID: "schedule_v1", Name: "schedule", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: model.AutomationDelegation{Actions: []model.Action{model.ActionStartWork}, Resources: []model.ResourceSelector{{Kind: model.ResourceAutomationRule, AutomationRuleID: "schedule"}}, ExpiresAt: now.Add(24 * time.Hour)}, Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute, Anchor: now}}, Action: model.AutomationAction{Kind: model.AutomationStartWork, Work: &model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}}, Policy: model.OccurrencePolicy{MissedTicks: model.MissedTickSkip, OfflineDelivery: model.OfflineQueue, ExpiresAfter: time.Hour, Overlap: model.OverlapForbid, MaxActive: 1, Deadline: time.Hour, Retry: model.RetryPolicy{MaxAttempts: 1}}})
	require.NoError(t, err)
	first, err := service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	require.Len(t, first.Occurrences, 1)
	occurrences, err := service.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: operator, RuleID: rule.Rule.ID})
	require.NoError(t, err)
	require.Len(t, occurrences, 1)
	require.Equal(t, model.OccurrenceAdmitted, occurrences[0].Occurrence.State)
	require.NotEmpty(t, occurrences[0].Occurrence.WorkRunID)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	restarted := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	_, err = restarted.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	occurrences, err = restarted.ListOccurrences(ctx, app.ListOccurrencesRequest{Principal: operator, RuleID: rule.Rule.ID})
	require.NoError(t, err)
	require.Len(t, occurrences, 1)
	require.Equal(t, model.AutomationRuleRevisionID("schedule_v1"), occurrences[0].Occurrence.RuleRevisionID)
}

func TestPinnedTeamDeploymentLaunchesWavesWithPreparedBriefings(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	provider := &preparedWorkProvider{}
	service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "builder", Name: "builder", Desired: desired, Required: true, BriefingIDs: []string{"mission"}}, {Key: "reviewer", Name: "reviewer", Desired: desired, Required: true, BriefingIDs: []string{"mission"}}}, Waves: []model.TeamWave{{ID: "build", MemberKeys: []string{"builder"}, RequiredReady: true, RequiredBriefs: true}, {ID: "review", MemberKeys: []string{"reviewer"}, DependsOn: []string{"build"}, RequiredReady: true, RequiredBriefs: true}}, Briefings: []model.TeamBriefing{{ID: "mission", Body: "use the pinned definition", Timing: model.BriefingBeforeFirstWork, Required: true, MemberKeys: []string{"builder", "reviewer"}}}}
	definition, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: operator, RequestID: "save_team"}, Draft: app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "team", Team: &team}})
	require.NoError(t, err)
	deployed, err := service.DeployTeam(ctx, app.DeployTeamRequest{Context: app.RequestContext{Principal: operator, RequestID: "deploy"}, DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionTeam}, Mission: "ship safely", GroupID: "deployment_group"}})
	require.NoError(t, err)
	require.Equal(t, model.DeploymentDeploying, deployed.Deployment.State)
	for range 3 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	ready, err := service.GetTeamDeployment(ctx, app.GetTeamDeploymentRequest{Principal: operator, DeploymentID: deployed.Deployment.ID})
	require.NoError(t, err)
	require.Equal(t, model.DeploymentReady, ready.Deployment.State)
	require.Len(t, provider.preparations, 2)
	for _, preparation := range provider.preparations {
		require.NotNil(t, preparation.InitialInput)
		require.Contains(t, preparation.InitialInput.Body, "ship safely")
		require.Contains(t, preparation.InitialInput.Body, "use the pinned definition")
	}
}

func TestJoinAnyDrainsAndLateFailurePreventsSuccess(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC)
	provider := &preparedWorkProvider{}
	service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	for _, id := range []model.AgentID{"reviewer_a", "reviewer_b"} {
		_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: id, Name: string(id), Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}})
		require.NoError(t, err)
	}
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "parallel", Nodes: []model.WorkNode{{ID: "parallel", Kind: model.WorkNodeFork}, {ID: "review_a", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "reviewer_a", Brief: "review"}}}, {ID: "review_b", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "reviewer_b", Brief: "review"}}}, {ID: "first", Kind: model.WorkNodeJoin, Join: &model.JoinPolicy{Mode: model.JoinAny}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "parallel", To: "review_a"}, {From: "parallel", To: "review_b"}, {From: "review_a", To: "first"}, {From: "review_b", To: "first"}, {From: "first", To: "done"}}}
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: operator, RequestID: "start_parallel"}, ID: "parallel_run", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	for range 2 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	running, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: operator, WorkRunID: run.Run.ID})
	require.NoError(t, err)
	a := nodeAttempt(t, running.Run, "review_a")
	b := nodeAttempt(t, running.Run, "review_b")
	passed := true
	draining, err := service.RecordNodeEvidence(ctx, app.RecordNodeEvidenceRequest{Context: app.RequestContext{Principal: operator, RequestID: "review_a_pass"}, Attempt: a.Ref, ExpectedRunRevision: running.Run.Revision, Kind: model.WorkEvidenceVerification, Passed: &passed, Disposition: model.WorkOutcomeVerified, Detail: "first review passed"})
	require.NoError(t, err)
	require.Equal(t, model.WorkControlDraining, draining.Run.ControlState)
	require.Equal(t, model.WorkRunRunning, draining.Run.State)
	passed = false
	failed, err := service.RecordNodeEvidence(ctx, app.RecordNodeEvidenceRequest{Context: app.RequestContext{Principal: operator, RequestID: "review_b_fail"}, Attempt: b.Ref, ExpectedRunRevision: draining.Run.Revision, Kind: model.WorkEvidenceVerification, Passed: &passed, Disposition: model.WorkOutcomeRejected, Detail: "late review found defect"})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunFailed, failed.Run.State)
	require.Equal(t, model.WorkOutcomeRejected, failed.Run.Outcome)
}

func nodeAttempt(t *testing.T, run model.WorkRun, id model.WorkNodeID) model.WorkNodeAttempt {
	t.Helper()
	for _, attempt := range run.NodeAttempts {
		if attempt.Ref.NodeID == id {
			return attempt
		}
	}
	t.Fatalf("missing node attempt %s", id)
	return model.WorkNodeAttempt{}
}

func programGraph(profile app.ProgramProfileResult) model.WorkGraph {
	return model.WorkGraph{CompilerVersion: "1", EntryNodeID: "program", Nodes: []model.WorkNode{
		{ID: "program", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerProgram, Program: &model.ProgramPerformer{Profile: model.ProgramProfileRef{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}}}},
		{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
	}, Edges: []model.WorkEdge{{From: "program", To: "done"}}, Outcome: model.WorkGraphOutcomePolicy{RequiredNodes: []model.WorkNodeID{"program"}}}
}

type programHostFake struct {
	prepares         int
	uncertainRelease bool
	runtime          programRuntimeFake
}

func (h *programHostFake) PrepareProgram(_ context.Context, request ports.ProgramPreparationRequest) (ports.PreparedProgram, error) {
	h.prepares++
	h.runtime.id = request.Execution.ID
	return &preparedProgramFake{host: h, request: request}, nil
}

func (h *programHostFake) RecoverProgram(_ context.Context, request ports.ProgramRecoveryRequest) (ports.ProgramRecoveryResult, error) {
	h.runtime.id = request.Execution.ID
	observation, _ := h.runtime.ObserveProgram(context.Background())
	return ports.ProgramRecoveryResult{State: ports.RecoveryExited, Runtime: &h.runtime, Observation: observation, Evidence: observation.Evidence}, nil
}

type preparedProgramFake struct {
	host    *programHostFake
	request ports.ProgramPreparationRequest
}

func (p *preparedProgramFake) Describe() ports.ProgramPreparedDescription {
	return ports.ProgramPreparedDescription{ExecutionID: p.request.Execution.ID, Attempt: p.request.Execution.Attempt, EffectivePolicy: ports.ProgramEffectivePolicy{Sandbox: p.request.Profile.Sandbox, Enforced: true}, Evidence: programEvidence()}
}
func (p *preparedProgramFake) Abort(context.Context) error { return nil }
func (p *preparedProgramFake) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ProgramReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ProgramReleaseResult{}, err
	}
	if p.host.uncertainRelease {
		return ports.ProgramReleaseResult{State: ports.ReleaseUncertain, Evidence: programEvidence()}, nil
	}
	return ports.ProgramReleaseResult{State: ports.ReleaseStarted, Runtime: &p.host.runtime, Evidence: programEvidence()}, nil
}

type programRuntimeFake struct {
	id                model.ExecutionID
	releasedResources bool
}

func (r *programRuntimeFake) ExecutionID() model.ExecutionID { return r.id }
func (r *programRuntimeFake) ObserveProgram(context.Context) (ports.ProgramObservation, error) {
	code := 0
	return ports.ProgramObservation{ObservedAt: time.Now(), Workload: ports.WorkloadExited, ExitCode: &code, Stdout: ports.ProgramOutput{Data: []byte("ok"), MediaType: "text/plain"}, Evidence: programEvidence()}, nil
}
func (r *programRuntimeFake) StopProgram(context.Context, ports.StopRequest) (ports.StopResult, error) {
	return ports.StopResult{Disposition: ports.EffectAccepted}, nil
}
func (r *programRuntimeFake) ReleaseProgramResources(_ context.Context, evidence model.ProviderEvidence) error {
	if evidence.Provider == "fake-program" {
		r.releasedResources = true
	}
	return nil
}

func programEvidence() model.ProviderEvidence {
	return model.ProviderEvidence{Provider: "fake-program", Version: 1, Payload: []byte("opaque")}
}

type preparedWorkProvider struct {
	preparation  ports.PreparationRequest
	preparations []ports.PreparationRequest
}

func (*preparedWorkProvider) Name() string { return "prepared-work" }
func (*preparedWorkProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{PreparedInitialInput: true}
}
func (p *preparedWorkProvider) Prepare(_ context.Context, request ports.PreparationRequest) (ports.PreparedAttempt, error) {
	p.preparation = request
	p.preparations = append(p.preparations, request)
	return &preparedWorkAttempt{request: request}, nil
}
func (*preparedWorkProvider) Recover(context.Context, ports.RecoveryRequest) (ports.RecoveryResult, error) {
	return ports.RecoveryResult{State: ports.RecoveryUnknown}, nil
}

type preparedWorkAttempt struct{ request ports.PreparationRequest }

func (p *preparedWorkAttempt) Describe() ports.PreparedDescription {
	return ports.PreparedDescription{ExecutionID: p.request.Spec.ExecutionID, Attempt: p.request.Spec.Attempt, Topology: ports.TopologyIndependentServer, Requirements: ports.RuntimeRequirements{WorkingDirectory: p.request.Spec.WorkingDirectory, Loopback: &ports.LoopbackRequirement{Protocol: "http"}}, EffectivePolicy: ports.EffectivePolicy{Approval: p.request.Spec.Approval, Sandbox: p.request.Spec.Sandbox, ApprovalEnforced: true, SandboxEnforced: true}, Evidence: model.ProviderEvidence{Provider: "prepared-work", Version: 1, Payload: []byte("opaque")}, InitialInput: &ports.PreparedInitialInputDescription{Correlation: p.request.InitialInput.Correlation, Supported: true}}
}
func (*preparedWorkAttempt) Abort(context.Context) error { return nil }
func (p *preparedWorkAttempt) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, err
	}
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: &preparedWorkRuntime{id: p.request.Spec.ExecutionID}, Evidence: model.ProviderEvidence{Provider: "prepared-work", Version: 1, Payload: []byte("opaque")}}, nil
}

type preparedWorkRuntime struct{ id model.ExecutionID }

func (r *preparedWorkRuntime) ExecutionID() model.ExecutionID { return r.id }
func (*preparedWorkRuntime) Observe(context.Context) (ports.Observation, error) {
	return ports.Observation{ObservedAt: time.Now(), Workload: ports.WorkloadRunning, Context: ports.ContextReady, Evidence: model.ProviderEvidence{Provider: "prepared-work", Version: 1, Payload: []byte("opaque")}}, nil
}
func (*preparedWorkRuntime) Interact(context.Context, ports.Interaction) (ports.InteractionResult, error) {
	return ports.InteractionResult{Disposition: ports.EffectAccepted}, nil
}
func (*preparedWorkRuntime) Attach(context.Context, ports.AttachmentRequest) (ports.AttachmentResult, error) {
	return ports.AttachmentResult{Disposition: ports.EffectUnsupported}, nil
}
func (*preparedWorkRuntime) ChangeContext(context.Context, ports.ContextChange) (ports.ContextChangeResult, error) {
	return ports.ContextChangeResult{Disposition: ports.EffectUnsupported}, nil
}
func (*preparedWorkRuntime) Stop(context.Context, ports.StopRequest) (ports.StopResult, error) {
	return ports.StopResult{Disposition: ports.EffectAccepted, Acknowledged: true, Exited: true}, nil
}

func TestDefinitionValidationRejectsGraphCycleAndAcceptsRequiredPreparedBriefing(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service := app.New(store, providers.NewRegistry())
	operator := model.OperatorPrincipal()

	cyclic := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "one", Nodes: []model.WorkNode{{ID: "one", Kind: model.WorkNodeFork}, {ID: "two", Kind: model.WorkNodeFork}}, Edges: []model.WorkEdge{{From: "one", To: "two"}, {From: "two", To: "one"}}}
	_, err = service.ValidateDefinition(context.Background(), app.ValidateDefinitionRequest{Principal: operator, Draft: app.DefinitionDraft{ID: "bad", Name: "bad", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "bad", Process: &model.ProcessDefinition{Graph: cyclic}}})
	require.ErrorIs(t, err, app.ErrInvalid)

	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared,
		Members:   []model.TeamMemberSpec{{Key: "reviewer", Name: "reviewer", Required: true, BriefingIDs: []string{"mission"}}},
		Waves:     []model.TeamWave{{ID: "first", MemberKeys: []string{"reviewer"}, RequiredReady: true, RequiredBriefs: true}},
		Briefings: []model.TeamBriefing{{ID: "mission", Body: "Review before other work", Timing: model.BriefingBeforeFirstWork, Required: true, MemberKeys: []string{"reviewer"}}},
	}
	result, err := service.ValidateDefinition(context.Background(), app.ValidateDefinitionRequest{Principal: operator, Draft: app.DefinitionDraft{ID: "team", Name: "team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "kind: team", Team: &team}})
	require.NoError(t, err)
	require.Equal(t, model.BriefingBeforeFirstWork, result.Revision.Team.Briefings[0].Timing)
}

func TestManualOccurrencePinsRecipientSnapshotAndDeduplicates(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 6, 20, 0, 0, 0, time.UTC)
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	rule, err := service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: operator, RequestID: "save_rule"}, ID: "reminder", RevisionID: "reminder_v1", Name: "reminder", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute}}, Action: model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: "review"}}, Policy: model.OccurrencePolicy{MissedTicks: model.MissedTickSkip, OfflineDelivery: model.OfflineSkip, ExpiresAfter: time.Hour, Overlap: model.OverlapForbid, MaxActive: 1, Deadline: time.Minute, Retry: model.RetryPolicy{MaxAttempts: 1}}})
	require.NoError(t, err)

	request := app.RunRuleNowRequest{Context: app.RequestContext{Principal: operator, RequestID: "run_now"}, RuleID: rule.Rule.ID, ExpectedRuleRevision: rule.Rule.Revision, OccurrenceID: "occurrence_one", SourceOccurrenceKey: "operator_click_1", Recipients: []model.AgentID{"reviewer_a", "reviewer_a", "reviewer_b"}}
	first, err := service.RunRuleNow(ctx, request)
	require.NoError(t, err)
	second, err := service.RunRuleNow(ctx, request)
	require.NoError(t, err)
	require.Equal(t, first.Occurrence, second.Occurrence)
	require.Len(t, first.Occurrence.Recipients, 2)

	request.OccurrenceID = "occurrence_two"
	_, err = service.RunRuleNow(ctx, request)
	require.ErrorIs(t, err, app.ErrConflict)
}

func TestHumanDecisionWindowIsExactVersionedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 6, 20, 0, 0, 0, time.UTC)
	sequence := 0
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now }).WithIDGenerator(func(prefix string) string { sequence++; return prefix + "exact_" + string(rune('a'+sequence)) })
	operator := model.OperatorPrincipal()
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "approve", Nodes: []model.WorkNode{
		{ID: "approve", Kind: model.WorkNodeDecision, Name: "Ship it?", Decision: &model.DecisionNode{Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"approve", "reject"}, ExpiresAfter: time.Hour}},
		{ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}},
	}, Edges: []model.WorkEdge{{From: "approve", To: "done"}}}
	run, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: operator, RequestID: "start_decision"}, ID: "decision_run", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(2 * time.Hour)}})
	require.NoError(t, err)
	require.Equal(t, model.NodeAttemptWaiting, run.Run.NodeAttempts[0].State)
	require.Len(t, run.Decisions, 1)

	pending, err := service.ListPendingDecisions(ctx, app.ListPendingDecisionsRequest{Principal: operator})
	require.NoError(t, err)
	require.Len(t, pending, 1)
	request := app.SubmitDecisionRequest{Context: app.RequestContext{Principal: operator, RequestID: "answer_decision"}, DecisionID: pending[0].Window.ID, ExpectedWindowRevision: pending[0].Window.Revision, Answer: "approve", Reason: "reviewed exact artifact"}
	answered, err := service.SubmitDecision(ctx, request)
	require.NoError(t, err)
	require.Equal(t, model.DecisionAnswered, answered.Window.State)
	require.Equal(t, model.Revision(2), answered.Window.Revision)
	repeated, err := service.SubmitDecision(ctx, request)
	require.NoError(t, err)
	require.Equal(t, answered, repeated)

	request.Context.RequestID = "stale_answer"
	_, err = service.SubmitDecision(ctx, request)
	require.ErrorIs(t, err, app.ErrConflict)
}
