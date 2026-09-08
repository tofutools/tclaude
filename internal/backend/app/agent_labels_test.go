package app_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	db "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestAgentLabelsSurviveReopenAndOmittedUpdatesWithoutAuthority(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := db.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	service := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	labels := model.AgentLabels{Role: "reviewer", Description: "Literal <b>instructions</b>\nsecond line"}
	desired := model.DesiredConfiguration{Harness: "codex", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	created, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "worker", Name: "Worker", Desired: desired, Labels: labels})
	require.NoError(t, err)
	require.Equal(t, labels, created.Agent.Labels)
	update := app.UpdateAgentRequest{Context: op, ID: "worker", ExpectedRevision: created.Agent.Revision, Name: "Renamed", Desired: desired}
	updated, err := service.UpdateAgent(ctx, update)
	require.NoError(t, err)
	require.Equal(t, labels, updated.Agent.Labels, "older clients omit labels")
	clear := model.AgentLabels{}
	update.Labels = &clear
	_, err = service.UpdateAgent(ctx, update)
	require.ErrorIs(t, err, app.ErrConflict)
	require.NoError(t, store.Close())
	store, err = db.Open(path)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry())
	retained, err := store.Agent(ctx, "worker")
	require.NoError(t, err)
	require.Equal(t, labels, retained.Labels)
	authority, err := store.AuthorityState(ctx)
	require.NoError(t, err)
	require.Empty(t, authority.Assignments)
	require.Empty(t, authority.Grants)
	update.ExpectedRevision = retained.Revision
	cleared, err := service.UpdateAgent(ctx, update)
	require.NoError(t, err)
	require.Equal(t, clear, cleared.Agent.Labels)
}

func TestAgentGroupLabelsEndAtomicallyWithMembership(t *testing.T) {
	for _, disband := range []bool{false, true} {
		t.Run(map[bool]string{false: "remove_rejoin", true: "disband"}[disband], func(t *testing.T) {
			ctx := context.Background()
			store, err := db.Open(filepath.Join(t.TempDir(), "db"))
			require.NoError(t, err)
			defer store.Close()
			service := app.New(store, providers.NewRegistry())
			op := model.OperatorPrincipal()
			desired := model.DesiredConfiguration{Harness: "codex", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
			labels := model.AgentLabels{Role: "fallback", Groups: map[model.GroupID]model.AgentDisplayLabels{"team": {Role: "reviewer", Description: "Private team context"}, "keep": {Role: "author"}}}
			created, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "worker", Name: "Worker", Desired: desired, Labels: labels})
			require.NoError(t, err)
			group, err := service.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team", Members: []model.AgentID{"worker"}})
			require.NoError(t, err)
			if disband {
				_, err = service.DisbandGroup(ctx, app.DisbandGroupRequest{Context: app.RequestContext{Principal: op, RequestID: "disband"}, ID: "team", ExpectedRevision: group.Group.Revision})
			} else {
				var removed app.GroupResult
				removed, err = service.UpdateGroup(ctx, app.UpdateGroupRequest{Context: op, ID: "team", Name: "Team", ExpectedRevision: group.Group.Revision})
				require.NoError(t, err)
				_, err = service.UpdateGroup(ctx, app.UpdateGroupRequest{Context: op, ID: "team", Name: "Team", Members: []model.AgentID{"worker"}, ExpectedRevision: removed.Group.Revision})
			}
			require.NoError(t, err)
			current, err := store.Agent(ctx, "worker")
			require.NoError(t, err)
			require.NotContains(t, current.Labels.Groups, model.GroupID("team"))
			require.Equal(t, labels.Groups["keep"], current.Labels.Groups["keep"])
			require.Equal(t, "fallback", current.Labels.Role)
			require.Greater(t, current.Revision, created.Agent.Revision)
			_, err = service.UpdateAgent(ctx, app.UpdateAgentRequest{Context: op, ID: "worker", Name: "Worker", Desired: desired, Labels: &labels, ExpectedRevision: created.Agent.Revision})
			require.ErrorIs(t, err, app.ErrConflict)
		})
	}
}
