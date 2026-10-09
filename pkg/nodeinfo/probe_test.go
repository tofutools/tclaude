package nodeinfo

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeInstalledHarnessVersions(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\n[ \"$1\" = --version ] || exit 1\nprintf 'codex 1.2.3\\n'\n"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0700))
	t.Setenv("PATH", dir)
	n := Probe(context.Background())
	require.NotEmpty(t, n.OS)
	require.NotEmpty(t, n.Arch)
	require.NotEmpty(t, n.TclaudeVersion)
	require.GreaterOrEqual(t, len(n.Harnesses), 2)
	require.Equal(t, "claude", n.Harnesses[0].Name)
	require.Empty(t, n.Harnesses[0].Version)
	require.Equal(t, "codex 1.2.3", n.Harnesses[1].Version)
}
func TestProbeOutputLimitAndCancellation(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	path := filepath.Join(dir, "huge")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\ni=0\nwhile [ $i -lt 1000 ]; do printf abcdefghijklmnopqrstuvwxyz; i=$((i+1)); done\n"), 0700))
	require.Empty(t, output(context.Background(), path, "--version"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Empty(t, output(ctx, path, "--version"))
}
