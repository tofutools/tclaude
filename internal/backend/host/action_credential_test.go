//go:build linux || darwin

package host

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestActionCredentialResourcePrepareRenewRecoverAndRemove(t *testing.T) {
	root := t.TempDir()
	host := ActionCredentialHost{PrivateRoot: filepath.Join(root, "private")}
	resource, err := host.Prepare([]byte("generation-one"))
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
	resource, err := host.Prepare(oldBearer)
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
	resource, err := host.Prepare([]byte("bearer"))
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
