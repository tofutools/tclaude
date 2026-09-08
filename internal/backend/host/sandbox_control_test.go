//go:build linux || darwin

package host

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestSandboxControlRejectsIncompleteCommandBeforePublication(t *testing.T) {
	private := t.TempDir()
	require.NoError(t, os.Chmod(private, 0700))
	inspector, err := NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	planner, err := NewSandboxLaunchPreparer(SandboxLaunchConfig{Inspector: inspector, Wrapper: "/bin/sh", Bootstrap: "/bin/sh", Artifacts: private})
	require.NoError(t, err)
	selected, materialized := materializedLaunchPolicy(t, inspector, model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate})
	for _, arguments := range [][]string{nil, {SandboxControlFDArgument, SandboxControlFDArgument}} {
		_, err := planner.PrepareControl(context.Background(), selected, materialized, ProcessSpec{Executable: "/bin/sh", Args: arguments, Directory: "/"}, 1234)
		require.ErrorContains(t, err, "descriptor argument")
	}
	_, err = planner.Prepare(context.Background(), selected, materialized, ProcessSpec{Executable: "/bin/sh", Args: []string{SandboxControlFDArgument}, Directory: "/"})
	require.ErrorContains(t, err, "descriptor argument")
	entries, err := os.ReadDir(private)
	require.NoError(t, err)
	require.Empty(t, entries)
}
