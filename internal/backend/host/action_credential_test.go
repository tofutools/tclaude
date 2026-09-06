//go:build linux || darwin

package host

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestActionCredentialResourcePrepareRenewRecoverAndRemove(t *testing.T) {
	root := t.TempDir()
	host := ActionCredentialHost{PrivateRoot: filepath.Join(root, "private")}
	resource, err := host.Prepare("delivery-one", []byte("generation-one"))
	require.NoError(t, err)

	require.Equal(t, []byte("generation-one"), mustReadCredential(t, resource.Path()))
	requirePermissions(t, filepath.Dir(resource.Path()), 0o700)
	requirePermissions(t, resource.Path(), 0o600)

	require.NoError(t, resource.Replace([]byte("generation-two")))
	require.Equal(t, []byte("generation-two"), mustReadCredential(t, resource.Path()))
	requirePermissions(t, resource.Path(), 0o600)

	recovered, err := host.Recover(resource.Path())
	require.NoError(t, err)
	require.Equal(t, resource.Path(), recovered.Path())
	require.NoError(t, recovered.Verify())

	require.NoError(t, recovered.Remove())
	require.NoDirExists(t, filepath.Dir(resource.Path()))
}

func TestActionCredentialRenewalIsAtomicForConcurrentReaders(t *testing.T) {
	host := ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "private")}
	oldBearer := []byte("old-credential-material")
	newBearer := []byte("new-credential-material-with-a-different-length")
	resource, err := host.Prepare("delivery-atomic", oldBearer)
	require.NoError(t, err)

	var readers sync.WaitGroup
	errors := make(chan []byte, 1)
	start := make(chan struct{})
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for range 500 {
				value, readErr := os.ReadFile(resource.Path())
				if readErr != nil || (!equalBytes(value, oldBearer) && !equalBytes(value, newBearer)) {
					select {
					case errors <- value:
					default:
					}
					return
				}
			}
		}()
	}
	close(start)
	for range 100 {
		require.NoError(t, resource.Replace(newBearer))
		require.NoError(t, resource.Replace(oldBearer))
	}
	readers.Wait()
	select {
	case value := <-errors:
		t.Fatalf("reader observed partial credential %q", value)
	default:
	}
}

func TestActionCredentialRecoveryRejectsUnprotectedOrForeignResources(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	host := ActionCredentialHost{PrivateRoot: root}
	resource, err := host.Prepare("delivery-recovery", []byte("bearer"))
	require.NoError(t, err)

	require.NoError(t, os.Chmod(resource.Path(), 0o644))
	_, err = host.Recover(resource.Path())
	require.ErrorContains(t, err, "protected regular file")

	foreign := filepath.Join(t.TempDir(), "action-credential-forged", actionCredentialFilename)
	require.NoError(t, os.MkdirAll(filepath.Dir(foreign), 0o700))
	require.NoError(t, os.WriteFile(foreign, []byte("bearer"), 0o600))
	_, err = host.Recover(foreign)
	require.ErrorContains(t, err, "outside private storage")
}

func TestActionCredentialDeliveryImplementsSharedLifecycle(t *testing.T) {
	host := ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "private")}
	expires := time.Now().Add(time.Hour)
	first, err := host.PrepareActionCredential(context.Background(), ports.ActionCredentialMaterial{
		ExecutionID: "execution_delivery", Generation: 1, DeliveryID: "opaque-delivery",
		Secret: []byte("first"), ExpiresAt: expires,
	})
	require.NoError(t, err)
	require.NotEmpty(t, first.FileIdentity)
	require.NotContains(t, first.Resource, "opaque-delivery")

	second, err := host.RotateActionCredential(context.Background(), first, ports.ActionCredentialMaterial{
		ExecutionID: "execution_delivery", Generation: 2, DeliveryID: "opaque-delivery",
		Secret: []byte("second"), ExpiresAt: expires,
	})
	require.NoError(t, err)
	require.NotEqual(t, first.FileIdentity, second.FileIdentity)
	require.Equal(t, []byte("second"), mustReadCredential(t, second.Resource))

	proof, err := host.InspectActionCredential(context.Background(), model.ExecutionAccessBinding{
		ExecutionID: "execution_delivery", Generation: 2, DeliveryID: "opaque-delivery",
		State: model.ExecutionAccessSuspended, ExpiresAt: expires,
	})
	require.NoError(t, err)
	require.Equal(t, second.FileIdentity, proof.FileIdentity)
	require.Equal(t, second.Resource, proof.Resource)
	_, err = host.InspectActionCredential(context.Background(), model.ExecutionAccessBinding{
		ExecutionID: "execution_delivery", Generation: 1, DeliveryID: "opaque-delivery",
		State: model.ExecutionAccessSuspended, ExpiresAt: expires,
	})
	require.ErrorContains(t, err, "does not match recovery binding")

	_, err = host.RotateActionCredential(context.Background(), first, ports.ActionCredentialMaterial{
		ExecutionID: "execution_delivery", Generation: 3, DeliveryID: "opaque-delivery",
		Secret: []byte("third"), ExpiresAt: expires,
	})
	require.ErrorContains(t, err, "stale file identity")
	require.NoError(t, host.RemoveActionCredential(context.Background(), second))
	require.NoFileExists(t, second.Resource)
}

func TestActionCredentialRecoveryRejectsBearerPublishedBeforeBinding(t *testing.T) {
	host := ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "private")}
	expires := time.Now().Add(time.Hour)
	receipt, err := host.PrepareActionCredential(context.Background(), ports.ActionCredentialMaterial{
		ExecutionID: "execution_crash", Generation: 1, DeliveryID: "delivery-crash",
		Secret: []byte("first"), ExpiresAt: expires,
	})
	require.NoError(t, err)

	resource, err := host.Recover(receipt.Resource)
	require.NoError(t, err)
	require.NoError(t, resource.Replace([]byte("second")), "simulate a crash before renewed binding publication")
	_, err = host.InspectActionCredential(context.Background(), model.ExecutionAccessBinding{
		ExecutionID: "execution_crash", Generation: 1, DeliveryID: "delivery-crash",
		State: model.ExecutionAccessSuspended, ExpiresAt: expires,
	})
	require.ErrorContains(t, err, "does not match recovery binding")
}

func mustReadCredential(t *testing.T, path string) []byte {
	t.Helper()
	value, err := os.ReadFile(path)
	require.NoError(t, err)
	return value
}

func requirePermissions(t *testing.T, path string, expected os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, expected, info.Mode().Perm())
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
