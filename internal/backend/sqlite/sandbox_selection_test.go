package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func selectionForPersistence() *model.SandboxSelection {
	return &model.SandboxSelection{Scopes: []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: model.SandboxProfileRef{ProfileID: "policy", RevisionID: "revision", ContentHash: strings.Repeat("a", 64)}}}, PolicyHash: strings.Repeat("b", 64)}
}

func TestHostSandboxSelectionPersistsAgentAndExecutionAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	require.NoError(t, err)
	selected := selectionForPersistence()
	now := time.Now().UTC()
	desired := model.DesiredConfiguration{HostSandbox: selected, Harness: "codex", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	require.NoError(t, store.CreateAgent(ctx, model.Agent{ID: "agent", Name: "Agent", Desired: desired, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	tx, err := store.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, insertExecution(ctx, tx, model.Execution{ID: "execution", AgentID: "agent", Workload: model.ExecutionWorkloadHarness, State: model.ExecutionReserved, Attempt: 1, Revision: 1, CreatedAt: now, UpdatedAt: now, Spec: model.ResolvedExecutionSpec{HostSandbox: model.CloneSandboxSelection(selected), Harness: desired.Harness, Model: desired.Model, WorkingDirectory: desired.WorkingDirectory, Approval: desired.Approval, Sandbox: desired.Sandbox}}))
	require.NoError(t, tx.Commit())
	require.NoError(t, store.Close())
	store, err = Open(path)
	require.NoError(t, err)
	defer store.Close()
	agent, err := store.Agent(ctx, "agent")
	require.NoError(t, err)
	require.Equal(t, selected, agent.Desired.HostSandbox)
	execution, err := store.Execution(ctx, "execution")
	require.NoError(t, err)
	require.Equal(t, selected, execution.Spec.HostSandbox)
	agent.Desired.HostSandbox.Scopes[0].Ref.RevisionID = "later"
	require.Equal(t, model.SandboxProfileRevisionID("revision"), execution.Spec.HostSandbox.Scopes[0].Ref.RevisionID)
}

func TestHostSandboxAuthorityRequiresExactPolicyAndCannotDropConfinement(t *testing.T) {
	desired := model.DesiredConfiguration{Harness: "codex", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	bounds := model.ConfigurationBounds{Harnesses: []string{desired.Harness}, Models: []string{desired.Model}, WorkingDirectoryRoots: []string{desired.WorkingDirectory}, ApprovalModes: []model.ApprovalMode{desired.Approval}, SandboxModes: []model.SandboxMode{desired.Sandbox}}
	require.True(t, configurationMatches(bounds, &desired), "old grants still match old configurations")
	desired.HostSandbox = selectionForPersistence()
	require.False(t, configurationMatches(bounds, &desired), "old grants do not authorize a new policy")
	bounds.HostSandboxPolicies = []string{desired.HostSandbox.PolicyHash}
	require.True(t, configurationMatches(bounds, &desired))
	desired.HostSandbox.PolicyHash = strings.Repeat("c", 64)
	require.False(t, configurationMatches(bounds, &desired), "different compiled content needs its own grant")
	desired.HostSandbox = nil
	require.False(t, configurationMatches(bounds, &desired), "a confined grant cannot be used to drop confinement")
}
