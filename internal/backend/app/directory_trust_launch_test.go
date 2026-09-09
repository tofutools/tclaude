package app_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestDirectoryTrustAgentLaunchRequiresCallerProofBeforeAdmission(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	provider := &peerMessagingProvider{newFakeProvider()}
	service := app.New(store, providers.NewRegistry(provider)).WithDirectoryWriteProof(host.DirectoryProof{})
	op := model.OperatorPrincipal()
	physicalRoot := t.TempDir()
	root := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(physicalRoot, root))
	desired := model.DesiredConfiguration{Harness: "claude", Model: "worker", WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	parent, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "parent", Name: "Parent", Desired: desired})
	require.NoError(t, err)
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(op, "parent_launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: parent.Agent.ID, ExpectedRevision: parent.Agent.Revision}}})
	require.NoError(t, err)
	desired.TrustDirectory = true
	child, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "child", Name: "Child", Desired: desired})
	require.NoError(t, err)
	caller := model.AgentPrincipal(parent.Agent.ID)
	caller.ExecutionID = launched.Execution.ID
	grant, err := service.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: model.AuthorityGrant{ID: "child_launch", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: parent.Agent.ID}, Action: model.ActionLaunch, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: child.Agent.ID}, Bounds: model.ConfigurationBounds{Harnesses: []string{"claude"}, Models: []string{"worker"}, WorkingDirectoryRoots: []string{root}, ApprovalModes: []model.ApprovalMode{desired.Approval}, SandboxModes: []model.SandboxMode{desired.Sandbox}}}})
	require.NoError(t, err)
	request := app.LaunchRequest{RequestContext: effect(caller, "child_launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: child.Agent.ID, ExpectedRevision: child.Agent.Revision}}}
	_, err = service.Launch(ctx, request)
	var proof *app.DirectoryProofRequired
	require.ErrorAs(t, err, &proof)
	require.NotEmpty(t, proof.Directories)
	unchanged, err := store.Agent(ctx, child.Agent.ID)
	require.NoError(t, err)
	require.Empty(t, unchanged.PrimaryExecutionID)
	for _, dir := range proof.Directories {
		marker := filepath.Join(dir, proof.Filename)
		require.NoFileExists(t, marker)
		require.NoError(t, os.WriteFile(marker, nil, 0600))
	}
	request.WriteProofToken = proof.Token
	result, err := service.Launch(ctx, request)
	require.NoError(t, err)
	require.True(t, result.Execution.Spec.TrustDirectory)
	physicalRoot, err = filepath.EvalSymlinks(physicalRoot)
	require.NoError(t, err)
	require.Equal(t, physicalRoot, result.Execution.Spec.WorkingDirectory)
	for _, dir := range proof.Directories {
		require.NoFileExists(t, filepath.Join(dir, proof.Filename))
	}
	request.WriteProofToken = ""
	repeated, err := service.Launch(ctx, request)
	require.NoError(t, err)
	require.Equal(t, result.Execution.ID, repeated.Execution.ID)
	require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: op, GrantID: grant.Grant.ID, ExpectedRevision: grant.Grant.Revision}))
	_, err = service.Launch(ctx, request)
	require.ErrorIs(t, err, app.ErrUnauthorized, "recorded authority still requires a current grant")
}
