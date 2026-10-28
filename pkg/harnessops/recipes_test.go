package harnessops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestPlanUpdateUsesHarnessBuiltinUpdater(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root always requires a manual command")
	}
	dir := testutil.CanonicalTempDir(t)
	for _, name := range []string{"codex", "pi"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755))
	}
	t.Setenv("PATH", dir)
	c, err := Plan(context.Background(), dir, "update", "codex")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "codex"), c.Path)
	require.Equal(t, []string{"update"}, c.Args)
	require.Equal(t, "codex update", c.Recipe)
	c, err = Plan(context.Background(), dir, "update", "pi")
	require.NoError(t, err)
	require.Equal(t, []string{"update", "--self"}, c.Args)
	_, err = Plan(context.Background(), dir, "update", "copilot")
	require.ErrorContains(t, err, "not installed")
}
