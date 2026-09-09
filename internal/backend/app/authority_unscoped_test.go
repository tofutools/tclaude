package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

func TestUnscopedAuthorityPreservesActionBoundariesAndRevocation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	svc := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, id := range []model.AgentID{"caller", "first", "second"} {
		_, err = svc.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: id, Name: string(id), Desired: desired})
		require.NoError(t, err)
	}
	caller := model.AgentPrincipal("caller")
	for _, id := range []model.AgentID{"first", "second"} {
		_, err = svc.ReadStatus(ctx, app.ReadStatusRequest{Principal: caller, Target: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: id}})
		require.ErrorIs(t, err, app.ErrUnauthorized)
	}
	in := app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: "read_everywhere", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "caller"}, Action: model.ActionReadStatus, Resource: model.ResourceSelector{Kind: model.ResourceAll}}}
	saved, err := svc.PutGrant(ctx, in)
	require.NoError(t, err)
	in.Principal = caller
	in.Grant.ID = "self_grant"
	_, err = svc.PutGrant(ctx, in)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	svc = app.New(store, providers.NewRegistry())
	for _, id := range []model.AgentID{"first", "second"} {
		status, err := svc.ReadStatus(ctx, app.ReadStatusRequest{Principal: caller, Target: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: id}})
		require.NoError(t, err)
		require.Len(t, status.Agents, 1)
		require.Equal(t, id, status.Agents[0].ID)
	}
	// Unscoped resource reach never bypasses configuration allow-lists.
	_, err = svc.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: "configure_everywhere", Subject: in.Grant.Subject, Action: model.ActionUpdateConfiguration, Resource: model.ResourceSelector{Kind: model.ResourceAll}}})
	require.NoError(t, err)
	_, err = svc.UpdateAgent(ctx, app.UpdateAgentRequest{Context: caller, ID: "first", Name: "Changed", Desired: desired, ExpectedRevision: 1})
	require.ErrorIs(t, err, app.ErrUnauthorized)
	other, err := svc.ExplainAuthority(ctx, app.AuthorityExplanationRequest{Principal: caller, Action: model.ActionRetireAgent, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: "first"}})
	require.NoError(t, err)
	require.False(t, other.Decision.Allowed)
	denial, err := svc.PutDenial(ctx, app.PutDenialRequest{Principal: op, Denial: model.AuthorityDenial{ID: "deny_read", Subject: in.Grant.Subject, Action: model.ActionReadStatus}})
	require.NoError(t, err)
	target := app.ReadStatusRequest{Principal: caller, Target: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: "first"}}
	_, err = svc.ReadStatus(ctx, target)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	require.NoError(t, svc.DeleteDenial(ctx, app.DeleteDenialRequest{Principal: op, DenialID: denial.Denial.ID, ExpectedRevision: denial.Denial.Revision}))
	_, err = svc.ReadStatus(ctx, target)
	require.NoError(t, err)
	require.NoError(t, svc.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: op, GrantID: saved.Grant.ID, ExpectedRevision: saved.Grant.Revision}))
	_, err = svc.ReadStatus(ctx, target)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	in.Principal = op
	in.Grant.Resource.AgentID = "first"
	_, err = svc.PutGrant(ctx, in)
	require.ErrorIs(t, err, app.ErrInvalid, "unscoped grant cannot hide a conflicting exact target")
}
