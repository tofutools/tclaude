package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	db "github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

func TestDisplayRoleRoutingUsesCurrentGroupLabelsWithoutAuthority(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "codex", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	for _, id := range []model.AgentID{"worker", "observer"} {
		labels := model.AgentLabels{Role: "reviewer"}
		if id == "worker" {
			labels.Groups = map[model.GroupID]model.AgentDisplayLabels{"team": {Role: " Reviewer "}, "other": {Role: "author"}}
		} else {
			labels.Groups = map[model.GroupID]model.AgentDisplayLabels{"team": {}}
		}
		_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: id, Name: string(id), Desired: desired, Labels: &labels})
		require.NoError(t, err)
	}
	for _, id := range []model.GroupID{"team", "other"} {
		_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: id, Name: string(id), Members: []model.AgentID{"worker", "observer"}})
		require.NoError(t, err)
	}
	audience := model.MessageAudience{GroupID: "team", RoleLabel: "reviewer"}
	ids, err := store.ResolveMessageAudience(ctx, audience)
	require.NoError(t, err)
	require.Equal(t, []model.AgentID{"worker"}, ids)
	ids, err = store.ResolveMessageAudience(ctx, model.MessageAudience{GroupID: "other", RoleLabel: "author"})
	require.NoError(t, err)
	require.Equal(t, []model.AgentID{"worker"}, ids)
	state, err := store.AuthorityState(ctx)
	require.NoError(t, err)
	require.Empty(t, state.Assignments)
	require.Empty(t, state.Grants)
	worker, err := store.Agent(ctx, "worker")
	require.NoError(t, err)
	worker.Labels.Groups["team"] = model.AgentDisplayLabels{Role: "author"}
	_, err = service.UpdateAgent(ctx, app.UpdateAgentRequest{Context: op, ID: worker.ID, ExpectedRevision: worker.Revision, Name: worker.Name, Desired: worker.Desired, Labels: &worker.Labels})
	require.NoError(t, err)
	ids, err = store.ResolveMessageAudience(ctx, audience)
	require.NoError(t, err)
	require.Empty(t, ids, "eligibility is evaluated against current labels at every resolution")
	for _, invalid := range []model.MessageAudience{{RoleLabel: "reviewer"}, {GroupID: "team", RoleLabel: "reviewer", RoleID: "permission"}, {GroupID: "team", RoleLabel: "reviewer", AgentIDs: []model.AgentID{"observer"}}} {
		_, err = store.ResolveMessageAudience(ctx, invalid)
		require.ErrorIs(t, err, app.ErrInvalid)
	}
}
