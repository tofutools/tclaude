//go:build linux || darwin

package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProtectedFileRoundTripRejectsPermissionsAndSymlinks(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	require.NoError(t, os.Mkdir(directory, 0o700))
	path := filepath.Join(directory, "secret")
	require.NoError(t, WriteProtectedFile(path, []byte("native-secret")))

	value, err := ReadProtectedFile(path, 64)
	require.NoError(t, err)
	require.Equal(t, []byte("native-secret"), value)
	requirePermissions(t, path, 0o600)

	require.NoError(t, os.Chmod(path, 0o644))
	_, err = ReadProtectedFile(path, 64)
	require.ErrorContains(t, err, "owner-only")

	require.NoError(t, os.Remove(path))
	target := filepath.Join(directory, "target")
	require.NoError(t, os.WriteFile(target, []byte("native-secret"), 0o600))
	require.NoError(t, os.Symlink(target, path))
	_, err = ReadProtectedFile(path, 64)
	require.ErrorContains(t, err, "owner-only")
}
