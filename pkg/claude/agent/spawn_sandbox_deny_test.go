package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --sandbox-deny paths are made absolute where the caller typed them: "~" is
// the caller's home and a relative path is taken from the current directory.
func TestAbsoluteSandboxDenyPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd, err := os.Getwd()
	require.NoError(t, err)
	got, err := absoluteSandboxDenyPaths([]string{"~/secrets", "~", "rel/dir", "/abs/path"})
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(home, "secrets"), home, filepath.Join(cwd, "rel/dir"), "/abs/path"}, got)
	_, err = absoluteSandboxDenyPaths([]string{"  "})
	assert.Error(t, err)
}
