package browser

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserOptionalParametersWithoutDefaultsRemainEmptyAfterPersistence(t *testing.T) {
	for _, kind := range []model.DefinitionKind{model.DefinitionProcess, model.DefinitionTeam} {
		t.Run(string(kind), func(t *testing.T) {
			root := t.TempDir()
			provider := &automationTeamProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(root, "credentials")}, briefs: make(chan string, 4)}
			ctx, page, operator := processEditorBrowser(t, provider)
			parameters := []model.ParameterDeclaration{}
			for i, typ := range []model.ParameterType{model.ParameterNumber, model.ParameterBoolean, model.ParameterObject, model.ParameterArray} {
				p := model.ParameterDeclaration{Name: string(typ), Type: typ}
				if i%2 == 1 {
					p.Default = json.RawMessage(" null ")
				}
				parameters = append(parameters, p)
			}
			draft := app.DefinitionDraft{ID: "optional", RevisionID: "one", Name: "Optional defaults", Kind: kind, SchemaVersion: 1, Source: "optional fixture", Parameters: parameters}
			if kind == model.DefinitionProcess {
				draft.Process = &model.ProcessDefinition{Graph: model.WorkGraph{CompilerVersion: "1", EntryNodeID: "task", Nodes: []model.WorkNode{{ID: "task", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerHuman, Human: &model.HumanPerformer{Operator: true, Prompt: "Review"}}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "task", To: "done"}}}}
			} else {
				repo := filepath.Join(root, "repo")
				for _, args := range [][]string{{"init", "-b", "main", repo}, {"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "base"}} {
					output, err := exec.Command("git", args...).CombinedOutput()
					require.NoError(t, err, "%s", output)
				}
				var space app.WorkspaceResult
				require.NoError(t, operator.Call(ctx, "POST", "/v2/workspaces/create", map[string]any{"request_id": "space", "id": "space", "intent": model.WorkspaceIntent{Repository: repo, IntendedPath: filepath.Join(root, "checkout"), Branch: "worker", BaseRevision: "main", RetainOnFinish: true}}, &space))
				desired := model.DesiredConfiguration{Harness: "team-fixture", Model: "fixture", WorkingDirectory: space.Workspace.Observation.ActualPath, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
				draft.Team = &model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", Desired: desired, Required: true}}, Waves: []model.TeamWave{{ID: "wave", MemberKeys: []string{"worker"}, RequiredReady: true}}}
			}
			require.NoError(t, operator.Call(ctx, "POST", "/v2/definitions", map[string]any{"request_id": "save", "draft": draft}, nil))
			page.MustReload()
			page.MustElement("main:not([inert])")
			page.MustElementR("#connection", "^Updated")
			page.MustElement("[data-tab=processes]").MustClick()
			editor, editLabel, closeLabel, startLabel := "#process-editor", "^Edit process$", "^Close editor$", "^Start process$"
			if kind == model.DefinitionTeam {
				editor, editLabel, closeLabel, startLabel = "#team-editor", "^Edit team template$", "^Close team editor$", "^Deploy team$"
			}
			page.MustElementR("#definition-list button", editLabel).MustClick()
			for _, p := range parameters {
				page.MustElementR(editor+" button", "^Parameters$").MustClick()
				page.MustElementR(editor+" button", "^Edit "+p.Name+"$").MustClick()
				require.Empty(t, page.MustElement(editor+" [name=default]").MustProperty("value").Str(), p.Name)
			}
			page.MustElementR(editor+" button", closeLabel).MustClick()
			page.MustElementR("#definition-list button", startLabel).MustClick()
			for i := range parameters {
				require.Empty(t, page.MustElement(fmt.Sprintf("#editor [name=parameter_%d]", i)).MustProperty("value").Str())
			}
			if kind == model.DefinitionTeam {
				page.MustElement("#editor [name=mission]").MustInput("Optional defaults are absent")
			}
			require.True(t, page.MustEval(`()=>document.querySelector("#editor-form").checkValidity()`).Bool(), "the default deployment/launch form must be valid")
			page.MustElement("#editor button[type=submit]").MustClick()
			page.MustWait(`()=>!submitting`)
			require.True(t, page.MustElement("#editor-error").MustProperty("hidden").Bool(), page.MustElement("#editor-error").MustText())
			page.MustElement("#editor").MustWaitInvisible()
			page.MustWait(`()=>!submitting`)
			if kind == model.DefinitionProcess {
				var decisions []app.DecisionResult
				require.NoError(t, operator.Call(ctx, "GET", "/v2/decisions", nil, &decisions))
				require.Len(t, decisions, 1)
			} else {
				var deployments []app.TeamDeploymentResult
				require.NoError(t, operator.Call(ctx, "GET", "/v2/teams/deployments", nil, &deployments))
				require.Len(t, deployments, 1)
			}
		})
	}
}
