package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestResolveMessageAudiencePinsActiveAgentGroupAndRoleMembers(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "audience.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	now := time.Unix(1, 0).UTC()
	for _, id := range []model.AgentID{"agent_direct", "agent_group", "agent_role", "agent_retired"} {
		require.NoError(t, store.CreateAgent(ctx, model.Agent{ID: id, Name: string(id), Desired: model.DesiredConfiguration{Harness: "fake", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	}
	require.NoError(t, store.CreateGroup(ctx, model.Group{ID: "group_one", Name: "One", Members: []model.AgentID{"agent_group", "agent_retired"}, Revision: 1, CreatedAt: now, UpdatedAt: now}, model.ConfigurationBounds{}))
	_, err = store.PutRole(ctx, model.Role{ID: "reviewer", Name: "Reviewer", Revision: 1, CreatedAt: now, UpdatedAt: now}, 0)
	require.NoError(t, err)
	for _, id := range []model.AgentID{"agent_role", "agent_retired"} {
		_, err = store.PutRoleAssignment(ctx, model.RoleAssignment{RoleID: "reviewer", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: id}, Resource: model.ResourceSelector{Kind: model.ResourceSelf}, Revision: 1, CreatedAt: now, UpdatedAt: now}, 0)
		require.NoError(t, err)
	}
	_, err = store.db.ExecContext(ctx, `UPDATE agents SET lifecycle_state='retired' WHERE id='agent_retired'`)
	require.NoError(t, err)

	got, err := store.ResolveMessageAudience(ctx, model.MessageAudience{AgentIDs: []model.AgentID{"agent_direct", "agent_group", "missing"}, GroupID: "group_one", RoleID: "reviewer"})
	require.NoError(t, err)
	require.Equal(t, []model.AgentID{"agent_direct", "agent_group", "agent_role"}, got)
}
