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
