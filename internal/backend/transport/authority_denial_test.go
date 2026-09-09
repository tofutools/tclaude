package transport

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestAuthorityDenialAcceptsOrchestrationActionsThroughPublicAPI(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "worker", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	h, err := NewHandler(service, fixedCaller{model.OperatorPrincipal()})
	require.NoError(t, err)
	require.NoError(t, h.RegisterAgentAPI(service, service))
	for _, action := range []model.Action{model.ActionDisbandGroup, model.ActionReadDefinition, model.ActionManageDefinition, model.ActionManageProgramProfile, model.ActionReadProgramProfile, model.ActionExecuteProgram, model.ActionManageAutomation, model.ActionReadAutomation, model.ActionRunAutomation} {
		t.Run(string(action), func(t *testing.T) {
			id := "deny_" + strings.ReplaceAll(string(action), ".", "_")
			body, err := json.Marshal(map[string]any{"subject": model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "worker"}, "action": action})
			require.NoError(t, err)
			result := request(h, "PUT", "/v2/authority/denials/"+id, string(body), "")
			require.Equal(t, 200, result.Code, result.Body.String())
			decision, err := service.ExplainAuthority(ctx, app.AuthorityExplanationRequest{Principal: model.AgentPrincipal("worker"), Action: action, Resource: model.ResourceSelector{Kind: model.ResourceSelf}})
			require.NoError(t, err)
			require.False(t, decision.Decision.Allowed)
			require.Equal(t, model.AuthorityDenied, decision.Decision.SourceKind)
			require.Equal(t, id, decision.Decision.SourceID)
		})
	}
}
