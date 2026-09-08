package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/processimport"
)

const importProcessSource = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: original
start: task
nodes:
  task:
    type: task
    performer:
      kind: human
      ask: Approve?
      prompt: Preserved context
    next: done
  done:
    type: end
    result: done
`

func TestProcessImportReturnsUnsavedDraftAndRequiresOrdinarySave(t *testing.T) {
	ctx := context.Background()
	_, service, now := regressionService(t)
	_, err := service.InspectProcessImport(ctx, model.AgentPrincipal("untrusted"), importProcessSource)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	inspected, err := service.InspectProcessImport(ctx, model.OperatorPrincipal(), importProcessSource)
	require.NoError(t, err)
	require.Len(t, inspected.Requirements, 1)
	request := app.ConvertProcessImportRequest{Principal: model.OperatorPrincipal(), ID: "independent", Source: importProcessSource, Bindings: map[string]processimport.Binding{inspected.Requirements[0].Path: {Performer: model.Performer{Kind: model.PerformerHuman, Human: &model.HumanPerformer{Operator: true}}}}}
	converted, err := service.ConvertProcessImport(ctx, request)
	require.NoError(t, err)
	draft := converted.Draft
	require.Equal(t, importProcessSource, draft.Source)
	require.Equal(t, model.DefinitionID("independent"), draft.ID)
	definitions, err := service.ListDefinitions(ctx, app.ListDefinitionsRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	require.Empty(t, definitions)
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save_import"}, Draft: draft})
	require.NoError(t, err)
	ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
	result, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start_import"}, ID: "import_run", Start: model.WorkStart{Definition: &ref, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	require.Len(t, result.Decisions, 1)
	require.Equal(t, "Approve?\n\nPreserved context", result.Decisions[0].Question)
}

func TestProcessImportProgramMappingPinsLiteralExecutableWithoutEffects(t *testing.T) {
	ctx := context.Background()
	_, service, _ := regressionService(t)
	source := strings.Replace(importProcessSource, "kind: human\n      ask: Approve?\n      prompt: Preserved context", "kind: program\n      run: check\n      args: ['literal $(never execute)', '--flag']", 1)
	profile, err := service.SaveProgramProfile(ctx, app.SaveProgramProfileRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "profile"}, ID: "check", RevisionID: "check_v1", Name: "Check", Executable: "check", Sandbox: model.SandboxWorkspaceWrite, Timeout: time.Minute, OutputLimitBytes: 1024, EffectAuthority: []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}}})
	require.NoError(t, err)
	ref := model.ProgramProfileRef{ProfileID: profile.Profile.ID, RevisionID: profile.Revision.ID, ContentHash: profile.Revision.ContentHash}
	request := app.ConvertProcessImportRequest{Principal: model.OperatorPrincipal(), ID: "copy", Source: source, Bindings: map[string]processimport.Binding{"/nodes/task/performer": {Performer: model.Performer{Kind: model.PerformerProgram, Program: &model.ProgramPerformer{Profile: ref}}}}}
	converted, err := service.ConvertProcessImport(ctx, request)
	require.NoError(t, err)
	for _, node := range converted.Draft.Process.Graph.Nodes {
		if node.ID == "task" {
			require.Equal(t, ref, node.Performer.Program.Profile)
			require.Equal(t, []string{"literal $(never execute)", "--flag"}, node.Performer.Program.Arguments)
		}
	}
	request.Source = strings.Replace(source, "run: check", "run: different", 1)
	_, err = service.ConvertProcessImport(ctx, request)
	require.ErrorIs(t, err, app.ErrInvalid)
	request.Source = source
	binding := request.Bindings["/nodes/task/performer"]
	binding.Performer.Program.Profile.ProfileID = "another"
	request.Bindings["/nodes/task/performer"] = binding
	_, err = service.ConvertProcessImport(ctx, request)
	require.ErrorIs(t, err, app.ErrConflict)
	request.Principal = model.AgentPrincipal("untrusted")
	_, err = service.ConvertProcessImport(ctx, request)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	definitions, err := service.ListDefinitions(ctx, app.ListDefinitionsRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	require.Empty(t, definitions)
}
