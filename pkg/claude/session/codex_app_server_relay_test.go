package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConsumeCodexAppServerTokenIsOneShotAndRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "capability")
	require.NoError(t, os.WriteFile(path, []byte("launch-secret\n"), 0o600))

	token, err := consumeCodexAppServerToken(path)
	require.NoError(t, err)
	assert.Equal(t, "launch-secret", token)
	_, err = consumeCodexAppServerToken(path)
	assert.Error(t, err, "the handoff must be consumed exactly once")

	unsafe := filepath.Join(dir, "unsafe")
	require.NoError(t, os.WriteFile(unsafe, []byte("secret"), 0o644))
	_, err = consumeCodexAppServerToken(unsafe)
	assert.ErrorContains(t, err, "owned private file")

	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o600))
	symlink := filepath.Join(dir, "symlink")
	require.NoError(t, os.Symlink(target, symlink))
	_, err = consumeCodexAppServerToken(symlink)
	assert.ErrorContains(t, err, "owned private file")
}
