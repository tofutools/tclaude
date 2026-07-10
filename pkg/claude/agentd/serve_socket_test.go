package agentd

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareSocketPath(t *testing.T) {
	t.Run("refuses regular file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agentd.sock")
		require.NoError(t, os.WriteFile(path, []byte("private"), 0o600))
		err := prepareSocketPath(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "refusing to remove non-socket")
		got, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assert.Equal(t, "private", string(got))
	})

	t.Run("refuses live socket", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agentd.sock")
		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		err = prepareSocketPath(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already listening")
	})

	t.Run("removes stale socket", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agentd.sock")
		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		require.NoError(t, ln.Close())
		require.NoError(t, prepareSocketPath(path))
		_, err = os.Lstat(path)
		assert.True(t, os.IsNotExist(err))
	})
}
