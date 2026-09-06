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
