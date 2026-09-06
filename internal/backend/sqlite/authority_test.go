package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestOrchestrationResourceSelectorsRoundTripWithoutAuthorityWidening(t *testing.T) {
	cases := []struct {
		name   string
		first  model.ResourceSelector
		second model.ResourceSelector
	}{
		{"definition", model.ResourceSelector{Kind: model.ResourceDefinition, DefinitionID: "definition_a"}, model.ResourceSelector{Kind: model.ResourceDefinition, DefinitionID: "definition_b"}},
		{"program profile", model.ResourceSelector{Kind: model.ResourceProgramProfile, ProgramProfileID: "profile_a"}, model.ResourceSelector{Kind: model.ResourceProgramProfile, ProgramProfileID: "profile_b"}},
		{"automation rule", model.ResourceSelector{Kind: model.ResourceAutomationRule, AutomationRuleID: "rule_a"}, model.ResourceSelector{Kind: model.ResourceAutomationRule, AutomationRuleID: "rule_b"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(filepath.Join(t.TempDir(), "backend.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			now := time.Now().UTC()
			subject := model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "worker"}
			require.NoError(t, store.CreateAgent(ctx, model.Agent{ID: subject.AgentID, Name: "worker", Desired: model.DesiredConfiguration{Harness: "fake", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
			grant, err := store.PutGrant(ctx, model.AuthorityGrant{ID: "grant", Subject: subject, Action: model.ActionReadIdentity, Resource: test.first, CreatedAt: now, UpdatedAt: now}, 0)
			require.NoError(t, err)
			require.Equal(t, test.first, grant.Resource)
			require.False(t, resourceMatches(ctx, store.db, model.AgentPrincipal("worker"), grant.Resource, test.second))

			_, err = store.PutRole(ctx, model.Role{ID: "role", Name: "role", Actions: []model.Action{model.ActionReadIdentity}, CreatedAt: now, UpdatedAt: now}, 0)
			require.NoError(t, err)
			assignment, err := store.PutRoleAssignment(ctx, model.RoleAssignment{RoleID: "role", Subject: subject, Resource: test.first, CreatedAt: now, UpdatedAt: now}, 0)
			require.NoError(t, err)
			require.Equal(t, test.first, assignment.Resource)
			decision, err := store.Authorize(ctx, model.AuthorityRequest{Principal: model.AgentPrincipal("worker"), Action: model.ActionReadIdentity, Resource: test.second}, now)
			require.NoError(t, err)
			require.False(t, decision.Allowed)
		})
	}
}

func TestMalformedResourceSelectorsAreRejected(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	subject := model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "worker"}
	_, err = store.PutRole(ctx, model.Role{ID: "role", Name: "role", Actions: []model.Action{model.ActionReadIdentity}, CreatedAt: now, UpdatedAt: now}, 0)
	require.NoError(t, err)
	for _, resource := range []model.ResourceSelector{{Kind: model.ResourceDefinition}, {Kind: "unknown"}, {Kind: model.ResourceDefinition, DefinitionID: "a", AutomationRuleID: "also_a_rule"}} {
		_, err = store.PutGrant(ctx, model.AuthorityGrant{ID: model.GrantID("grant_" + string(resource.Kind)), Subject: subject, Action: model.ActionReadIdentity, Resource: resource, CreatedAt: now, UpdatedAt: now}, 0)
		require.ErrorIs(t, err, app.ErrInvalid)
		_, err = store.PutRoleAssignment(ctx, model.RoleAssignment{RoleID: "role", Subject: subject, Resource: resource, CreatedAt: now, UpdatedAt: now}, 0)
		require.ErrorIs(t, err, app.ErrInvalid)
	}
}

func TestPersistedOperationAuthorityRechecksExactOrchestrationResourceAtRelease(t *testing.T) {
	cases := []struct {
		name   string
		first  model.ResourceSelector
		second model.ResourceSelector
	}{
		{"definition", model.ResourceSelector{Kind: model.ResourceDefinition, DefinitionID: "definition_a"}, model.ResourceSelector{Kind: model.ResourceDefinition, DefinitionID: "definition_b"}},
		{"program profile", model.ResourceSelector{Kind: model.ResourceProgramProfile, ProgramProfileID: "profile_a"}, model.ResourceSelector{Kind: model.ResourceProgramProfile, ProgramProfileID: "profile_b"}},
		{"automation rule", model.ResourceSelector{Kind: model.ResourceAutomationRule, AutomationRuleID: "rule_a"}, model.ResourceSelector{Kind: model.ResourceAutomationRule, AutomationRuleID: "rule_b"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(filepath.Join(t.TempDir(), "backend.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			now := time.Now().UTC()
			subject := model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "worker"}
			grant, err := store.PutGrant(ctx, model.AuthorityGrant{ID: "grant_a", Subject: subject, Action: model.ActionReadIdentity, Resource: test.first, CreatedAt: now, UpdatedAt: now}, 0)
			require.NoError(t, err)

			tx, err := store.db.BeginTx(ctx, nil)
			require.NoError(t, err)
			execution := model.Execution{ID: "execution", State: model.ExecutionPrepared, Attempt: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
			require.NoError(t, insertExecution(ctx, tx, execution))
			operation := model.Operation{ID: "operation", RequestID: "request", Kind: model.OperationInteract, Principal: model.AgentPrincipal("worker"), ExecutionID: execution.ID, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now}
			require.NoError(t, insertOperation(ctx, tx, operation))
			request := model.AuthorityRequest{Principal: operation.Principal, Action: grant.Action, Resource: test.first}
			require.NoError(t, insertOperationAuthority(ctx, tx, operation.ID, request, model.AuthorityDecision{Allowed: true, Action: grant.Action, Resource: test.first, SourceKind: model.AuthorityDirect, SourceID: string(grant.ID), Revision: grant.Revision}))
			_, err = tx.ExecContext(ctx, `INSERT INTO release_permits(execution_id,operation_id) VALUES(?,?)`, execution.ID, operation.ID)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())

			require.NoError(t, store.DeleteGrant(ctx, grant.ID, grant.Revision))
			_, err = store.PutGrant(ctx, model.AuthorityGrant{ID: "grant_b", Subject: subject, Action: grant.Action, Resource: test.second, CreatedAt: now, UpdatedAt: now}, 0)
			require.NoError(t, err)
			require.ErrorIs(t, store.ConsumeRelease(ctx, execution.ID, operation.ID, now.Add(time.Second)), app.ErrUnauthorized)
		})
	}
}
