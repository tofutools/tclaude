package harnesspath

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestManagedPathDiscoveryAndPinning(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	ambient := filepath.Join(home, "ambient")
	require.NoError(t, os.Mkdir(ambient, 0700))
	t.Setenv("PATH", ambient)
	require.NoError(t, os.WriteFile(filepath.Join(ambient, "codex"), []byte("#!/bin/sh\n"), 0700))
	require.Empty(t, ManagedExecutable("codex"))
	dir := BinaryDir(home)
	require.NoError(t, os.MkdirAll(dir, 0700))
	path := filepath.Join(dir, "codex")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"), 0700))
	require.NoError(t, Enable(home))
	found, err := exec.LookPath("codex")
	require.NoError(t, err)
	require.Equal(t, path, found)
	require.Equal(t, path, ManagedExecutable("codex"))
	first := os.Getenv("PATH")
	require.NoError(t, Enable(home))
	require.Equal(t, first, os.Getenv("PATH"))
}
