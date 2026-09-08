package app_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

type parameterProgramHost struct {
	programHostFake
	requests []ports.ProgramPreparationRequest
}

func (h *parameterProgramHost) PrepareProgram(ctx context.Context, r ports.ProgramPreparationRequest) (ports.PreparedProgram, error) {
	h.requests = append(h.requests, r)
	return h.programHostFake.PrepareProgram(ctx, r)
}

func TestProcessParametersReachPreparedArgumentsExactlyOnceAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "params.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	now := time.Now().UTC()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: t.TempDir(), ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	profile, err := service.SaveProgramProfile(ctx, app.SaveProgramProfileRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "profile"}, ID: "check", RevisionID: "check_v1", Name: "check", Executable: "check", ArgumentPrefix: []string{"{{ params.text }}"}, Sandbox: model.SandboxWorkspaceWrite, Timeout: time.Minute, OutputLimitBytes: 1024, EffectAuthority: []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}}})
	require.NoError(t, err)
	graph := programGraph(profile)
	graph.Description = "{{ params.text }}"
	graph.Nodes[0].Performer.Program.Arguments = []string{"prefix {{ params.text }} suffix", "{{params.number}}", "{{params.absent}}"}
	graph.Nodes[0].Performer.Program.Input = json.RawMessage(`{"literal":"{{ params.text }}"}`)
	draft := app.DefinitionDraft{ID: "params", RevisionID: "params_v1", Name: "Parameter execution", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "process", Parameters: []model.ParameterDeclaration{{Name: "text", Type: model.ParameterString, Required: true}, {Name: "number", Type: model.ParameterNumber, Default: json.RawMessage(`9007199254740993`)}, {Name: "absent", Type: model.ParameterString}}, Process: &model.ProcessDefinition{ParameterSyntax: "mustache-v1", Graph: graph}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
	require.NoError(t, err)
	ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
	literal := "$(touch nope); spaces\n{{ params.number }}"
	raw, _ := json.Marshal(literal)
	start := app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{Definition: &ref, Parameters: map[string]json.RawMessage{"text": raw}, Scope: model.WorkScope{WorkspaceID: "workspace"}, AuthorizedProgramProfiles: []model.ProgramProfileRef{graph.Nodes[0].Performer.Program.Profile}, Deadline: now.Add(time.Hour)}}
	_, err = service.StartProcess(ctx, start)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	host := &parameterProgramHost{}
	service = app.New(store, providers.NewRegistry()).WithProgramHost(host)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	require.Len(t, host.requests, 1)
	require.Equal(t, []string{"prefix " + literal + " suffix", "9007199254740993", ""}, host.requests[0].Arguments)
	require.Equal(t, []string{"{{ params.text }}"}, host.requests[0].Profile.ArgumentPrefix)
	require.JSONEq(t, `{"literal":"{{ params.text }}"}`, string(host.requests[0].Input))
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	require.Len(t, host.requests, 1)
	read, err := service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: "params"})
	require.NoError(t, err)
	require.Equal(t, "prefix {{ params.text }} suffix", read.Revision.Process.Graph.Nodes[0].Performer.Program.Arguments[0])
	result, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.NoError(t, err)
	require.Equal(t, "{{ params.text }}", result.Run.Graph.Description)
	// Expansion cannot bypass the existing per-argument preparation limit.
	start.ID = "too_large"
	start.Context.RequestID = "too_large"
	raw, _ = json.Marshal(strings.Repeat("x", 5000))
	start.Start.Parameters["text"] = raw
	_, err = service.StartProcess(ctx, start)
	require.ErrorIs(t, err, app.ErrInvalid)
	_, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "too_large"})
	require.ErrorIs(t, err, app.ErrNotFound)
}

func TestProcessParameterSyntaxIsExplicitAndValidatesCompiledStages(t *testing.T) {
	ctx := context.Background()
	_, service, now := regressionService(t)
	graph := stagedHumanGraph()
	graph.Nodes[0].Stages.Plan.Performer.Human = &model.HumanPerformer{Operator: true, Prompt: "Plan {{ params.subject }}"}
	graph.Nodes[0].Performer.Human = &model.HumanPerformer{Operator: true, Prompt: "Work {{params.subject}}"}
	graph.Nodes[0].Stages.PlanApproval.Question = "Approve {{ params.subject }}?"
	draft := app.DefinitionDraft{ID: "human_params", RevisionID: "human_params_v1", Name: "Human inputs", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "process", Process: &model.ProcessDefinition{ParameterSyntax: "mustache-v1", Graph: graph}, Parameters: []model.ParameterDeclaration{{Name: "subject", Type: model.ParameterString, Default: json.RawMessage(`"release"`)}}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
	require.NoError(t, err)
	ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
	result, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "run"}, ID: "run", Start: model.WorkStart{Definition: &ref, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	require.Equal(t, "Plan release", result.Run.NodeAttempts[0].Performer.Human.Prompt)
	windows := result.Decisions
	require.NotEmpty(t, windows)
	require.Equal(t, "Plan release", windows[0].Question)
	_, err = service.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "plan_done"}, DecisionID: windows[0].ID, ExpectedWindowRevision: windows[0].Revision, Answer: "complete", Reason: "Plan prepared"})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	progress, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.NoError(t, err)
	found := false
	for _, window := range progress.Decisions {
		if window.Question == "Approve release?" {
			found = true
			require.Equal(t, []string{"approve", "rework"}, window.PermittedAnswers)
		}
	}
	require.True(t, found)

	draft.Parameters = nil
	_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
	require.ErrorIs(t, err, app.ErrInvalid)
	draft.Process.ParameterSyntax = "future"
	_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
	require.ErrorIs(t, err, app.ErrInvalid)
	draft.Process.ParameterSyntax = ""
	_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
	require.NoError(t, err)
	draft.RevisionID = "literal_v2"
	literal, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "literal"}, Draft: draft, ExpectedRevision: 1})
	require.NoError(t, err)
	ref.RevisionID = literal.Revision.ID
	ref.ContentHash = literal.Revision.ContentHash
	unchanged, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "literal_run"}, ID: "literal_run", Start: model.WorkStart{Definition: &ref, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	require.Equal(t, "Plan {{ params.subject }}", unchanged.Decisions[0].Question)
}

func TestProcessParameterEmptyQuestionKeepsAuthoredPresence(t *testing.T) {
	ctx := context.Background()
	_, service, now := regressionService(t)
	for _, question := range []string{"{{ params.question }}", ""} {
		graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "ask", Nodes: []model.WorkNode{{ID: "ask", Name: "Editor label", Kind: model.WorkNodeDecision, Decision: &model.DecisionNode{Question: question, Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"yes"}, ExpiresAfter: time.Hour}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "ask", To: "done"}}}
		id := "explicit"
		want := ""
		if question == "" {
			id = "fallback"
			want = "Editor label"
		}
		saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: model.RequestID(id)}, Draft: app.DefinitionDraft{ID: model.DefinitionID(id), RevisionID: model.DefinitionRevisionID(id + "_v1"), Name: id, Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "test", Parameters: []model.ParameterDeclaration{{Name: "question", Type: model.ParameterString}}, Process: &model.ProcessDefinition{ParameterSyntax: "mustache-v1", Graph: graph}}})
		require.NoError(t, err)
		ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
		result, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: model.RequestID(id + "_start")}, ID: model.WorkRunID(id), Start: model.WorkStart{Definition: &ref, Deadline: now.Add(time.Hour)}})
		require.NoError(t, err)
		require.Len(t, result.Decisions, 1)
		require.Equal(t, want, result.Decisions[0].Question)
		read, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: model.WorkRunID(id)})
		require.NoError(t, err)
		require.True(t, read.Run.Graph.Nodes[0].Decision.QuestionResolved)
		require.Equal(t, want, read.Run.Graph.Nodes[0].Decision.Question)
	}
}

func TestProcessParameterGrammarRejectsUnaddressableAndMalformedInput(t *testing.T) {
	ctx := context.Background()
	_, service, _ := regressionService(t)
	for _, tc := range []struct{ key, text string }{{"release-key", "{{ params.release-key }}"}, {"release", "{{ params.release-key }}"}, {"release", "{{ params.release"}, {"release", "{{ params[release] }}"}, {"release", "{{ params. }}"}} {
		t.Run(tc.key+tc.text, func(t *testing.T) {
			graph := stagedHumanGraph()
			graph.Nodes[0].Stages.Plan.Performer.Human = &model.HumanPerformer{Operator: true, Prompt: tc.text}
			draft := app.DefinitionDraft{ID: "grammar", RevisionID: "grammar_v1", Name: "grammar", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "test", Parameters: []model.ParameterDeclaration{{Name: tc.key, Type: model.ParameterString}}, Process: &model.ProcessDefinition{ParameterSyntax: "mustache-v1", Graph: graph}}
			_, err := service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
			require.ErrorIs(t, err, app.ErrInvalid)
			draft.Process.ParameterSyntax = ""
			_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
			require.NoError(t, err)
		})
	}
}

func TestProcessParameterResolvedQuestionCannotBeAuthored(t *testing.T) {
	ctx := context.Background()
	_, service, now := regressionService(t)
	for _, staged := range []bool{false, true} {
		graph := stagedHumanGraph()
		if staged {
			graph.Nodes[0].Stages.PlanApproval.QuestionResolved = true
		} else {
			graph = model.WorkGraph{CompilerVersion: "1", EntryNodeID: "ask", Nodes: []model.WorkNode{{ID: "ask", Name: "required fallback", Kind: model.WorkNodeDecision, Decision: &model.DecisionNode{QuestionResolved: true, Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"yes"}, ExpiresAfter: time.Hour}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "ask", To: "done"}}}
		}
		_, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save_marker"}, Draft: app.DefinitionDraft{ID: "marker", RevisionID: "marker_v1", Name: "marker", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "test", Process: &model.ProcessDefinition{Graph: graph}}})
		require.ErrorIs(t, err, app.ErrInvalid)
		_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start_marker"}, ID: "marker_run", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
		require.ErrorIs(t, err, app.ErrInvalid)
		_, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "marker_run"})
		require.ErrorIs(t, err, app.ErrNotFound)
	}
}
