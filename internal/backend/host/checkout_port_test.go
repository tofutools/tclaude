//go:build linux || darwin

package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type checkoutPermit struct {
	operation model.OperationID
	consumed  bool
}

func (p *checkoutPermit) OperationID() model.OperationID { return p.operation }
func (p *checkoutPermit) Consume(context.Context) error {
	p.consumed = true
	return nil
}

func TestWorkspaceHostRequiresExactReceiptAndChecksDirtyBeforePermit(t *testing.T) {
	repository := checkoutTestRepository(t)
	host, err := NewCheckoutHost("git")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "worker")
	createPermit := &checkoutPermit{operation: "operation_create"}
	created, err := host.CreateCheckout(context.Background(), ports.CheckoutCreateRequest{
		WorkspaceID: "workspace_one",
		Intent: model.WorkspaceIntent{Repository: repository, IntendedPath: path,
			Branch: "feature/port", BaseRevision: "HEAD", Provenance: model.WorkspacePlatformCreated,
			Ownership: model.WorkspaceOwned},
	}, createPermit)
	require.NoError(t, err)
	require.True(t, createPermit.consumed)
	require.Equal(t, workspaceResourceOwner, created.Resource.Owner)

	forged := created.Resource
	forged.Payload = append([]byte(nil), forged.Payload...)
	forged.Payload[len(forged.Payload)-1] ^= 1
	removePermit := &checkoutPermit{operation: "operation_remove"}
	_, err = host.RemoveCheckout(context.Background(), ports.CheckoutRemoveRequest{
		WorkspaceID: "workspace_one", Resource: forged,
	}, removePermit)
	require.Error(t, err)
	require.False(t, removePermit.consumed)
	require.DirExists(t, path)

	require.NoError(t, os.WriteFile(filepath.Join(path, "dirty"), []byte("retain\n"), 0o600))
	removePermit = &checkoutPermit{operation: "operation_remove"}
	_, err = host.RemoveCheckout(context.Background(), ports.CheckoutRemoveRequest{
		WorkspaceID: "workspace_one", Resource: created.Resource,
	}, removePermit)
	require.ErrorIs(t, err, ErrCheckoutDirty)
	require.False(t, removePermit.consumed)

	result, err := host.RemoveCheckout(context.Background(), ports.CheckoutRemoveRequest{
		WorkspaceID: "workspace_one", Resource: created.Resource, Destructive: true,
	}, removePermit)
	require.NoError(t, err)
	require.True(t, removePermit.consumed)
	require.Equal(t, ports.EffectAccepted, result.Disposition)
	require.NoDirExists(t, path)
}

func TestWorkspaceHostRestoresRetainedCommittedBranch(t *testing.T) {
	repository := checkoutTestRepository(t)
	host, err := NewCheckoutHost("git")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "worker")
	intent := model.WorkspaceIntent{
		Repository: repository, IntendedPath: path, Branch: "feature/restore-port", BaseRevision: "HEAD",
		Provenance: model.WorkspacePlatformCreated, Ownership: model.WorkspaceOwned,
	}
	created, err := host.CreateCheckout(context.Background(), ports.CheckoutCreateRequest{
		WorkspaceID: "workspace_restore", Intent: intent,
	}, &checkoutPermit{operation: "operation_create"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(path, "worker.txt"), []byte("committed worker result\n"), 0o600))
	checkoutGit(t, path, "add", "worker.txt")
	checkoutGit(t, path, "commit", "-m", "worker result")

	inspected, err := host.InspectWorkspace(context.Background(), model.Workspace{
		ID: "workspace_restore", Intent: intent, Resource: created.Resource,
	})
	require.NoError(t, err)
	removePermit := &checkoutPermit{operation: "operation_remove"}
	removed, err := host.RemoveCheckout(context.Background(), ports.CheckoutRemoveRequest{
		WorkspaceID: "workspace_restore", Observation: inspected.Observation, Resource: created.Resource,
	}, removePermit)
	require.NoError(t, err)
	require.True(t, removePermit.consumed)
	require.Equal(t, inspected.Observation.Revision, removed.Observation.Revision)
	require.NoDirExists(t, path)

	restorePermit := &checkoutPermit{operation: "operation_restore"}
	restored, err := host.RestoreCheckout(context.Background(), ports.CheckoutRestoreRequest{
		WorkspaceID: "workspace_restore", Intent: intent, Observation: removed.Observation, Resource: removed.Resource,
	}, restorePermit)
	require.NoError(t, err)
	require.True(t, restorePermit.consumed)
	require.Equal(t, ports.EffectAccepted, restored.Disposition)
	require.Equal(t, inspected.Observation.Revision, restored.Observation.Revision)
	require.FileExists(t, filepath.Join(path, "worker.txt"))
	restoredEvidence, err := decodeWorkspaceResource(restored.Resource)
	require.NoError(t, err)
	createdEvidence, err := decodeWorkspaceResource(created.Resource)
	require.NoError(t, err)
	require.Equal(t, createdEvidence.OwnerToken, restoredEvidence.OwnerToken)
}

func TestWorkspaceHostRestoreRefusesMovedRetainedBranchBeforePermit(t *testing.T) {
	repository := checkoutTestRepository(t)
	host, err := NewCheckoutHost("git")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "worker")
	intent := model.WorkspaceIntent{
		Repository: repository, IntendedPath: path, Branch: "feature/moved-restore", BaseRevision: "HEAD",
		Provenance: model.WorkspacePlatformCreated, Ownership: model.WorkspaceOwned,
	}
	created, err := host.CreateCheckout(context.Background(), ports.CheckoutCreateRequest{
		WorkspaceID: "workspace_moved", Intent: intent,
	}, &checkoutPermit{operation: "operation_create"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(path, "worker.txt"), []byte("worker result\n"), 0o600))
	checkoutGit(t, path, "add", "worker.txt")
	checkoutGit(t, path, "commit", "-m", "worker result")
	inspected, err := host.InspectWorkspace(context.Background(), model.Workspace{
		ID: "workspace_moved", Intent: intent, Resource: created.Resource,
	})
	require.NoError(t, err)
	removed, err := host.RemoveCheckout(context.Background(), ports.CheckoutRemoveRequest{
		WorkspaceID: "workspace_moved", Observation: inspected.Observation, Resource: created.Resource,
	}, &checkoutPermit{operation: "operation_remove"})
	require.NoError(t, err)
	createdEvidence, err := decodeWorkspaceResource(created.Resource)
	require.NoError(t, err)
	checkoutGit(t, repository, "update-ref", "refs/heads/feature/moved-restore", createdEvidence.InitialCommit)

	restorePermit := &checkoutPermit{operation: "operation_restore"}
	result, err := host.RestoreCheckout(context.Background(), ports.CheckoutRestoreRequest{
		WorkspaceID: "workspace_moved", Intent: intent, Observation: removed.Observation, Resource: removed.Resource,
	}, restorePermit)
	require.ErrorIs(t, err, ErrCheckoutIdentityMismatch)
	require.Equal(t, ports.EffectRefused, result.Disposition)
	require.False(t, restorePermit.consumed)
	require.Equal(t, removed.Resource, result.Resource)
	require.NoDirExists(t, path)
}
