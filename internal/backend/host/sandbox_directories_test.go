//go:build linux || darwin

package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func directoryPlanner(t *testing.T, individual bool) (*SandboxLaunchPreparer, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(root, 0700))
	inspector, err := NewSandboxPathInspector([]string{root})
	require.NoError(t, err)
	planner, err := NewSandboxLaunchPreparer(SandboxLaunchConfig{Inspector: inspector, Wrapper: "/bin/sh", Bootstrap: "/bin/sh", Artifacts: root, AgentDirectoriesMountIndividually: individual})
	require.NoError(t, err)
	return planner, root
}

func TestSandboxGeneratedDirectoriesRetainActorCacheAndSeparateNewAgents(t *testing.T) {
	for _, individual := range []bool{false, true} {
		t.Run(fmt.Sprint(individual), func(t *testing.T) {
			planner, _ := directoryPlanner(t, individual)
			first := planner.ForExecution(model.ResolvedExecutionSpec{AgentID: "first", ExecutionID: "one"})
			env, resources, err := first.prepareAgentDirectories(context.Background(), []string{"GOCACHE", "BUILD_CACHE"})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(env["GOCACHE"], "retained"), []byte("cached"), 0600))
			if individual {
				require.Len(t, resources, 2)
			} else {
				require.Len(t, resources, 1)
				require.Equal(t, filepath.Dir(env["GOCACHE"]), resources[0].Path)
			}
			again, _, err := planner.ForExecution(model.ResolvedExecutionSpec{AgentID: "first", ExecutionID: "two"}).prepareAgentDirectories(context.Background(), []string{"GOCACHE", "NEW_CACHE"})
			require.NoError(t, err)
			require.Equal(t, env["GOCACHE"], again["GOCACHE"])
			require.FileExists(t, filepath.Join(again["GOCACHE"], "retained"))
			require.NotContains(t, again, "BUILD_CACHE")
			independent, _, err := planner.ForExecution(model.ResolvedExecutionSpec{AgentID: "second", ExecutionID: "three"}).prepareAgentDirectories(context.Background(), []string{"GOCACHE"})
			require.NoError(t, err)
			require.NotEqual(t, env["GOCACHE"], independent["GOCACHE"])
			require.NoFileExists(t, filepath.Join(independent["GOCACHE"], "retained"))
			// Deletion/recreation is a supported cache operation on a later launch.
			require.NoError(t, os.Remove(env["BUILD_CACHE"]))
			restored, _, err := first.prepareAgentDirectories(context.Background(), []string{"BUILD_CACHE"})
			require.NoError(t, err)
			require.Equal(t, env["BUILD_CACHE"], restored["BUILD_CACHE"])
			require.DirExists(t, restored["BUILD_CACHE"])
		})
	}
}

func TestSandboxGeneratedDirectoriesRefuseAliasesAndMissingOwner(t *testing.T) {
	planner, root := directoryPlanner(t, true)
	_, _, err := planner.prepareAgentDirectories(context.Background(), []string{"CACHE"})
	require.Error(t, err)
	actor := planner.ForExecution(model.ResolvedExecutionSpec{AgentID: "actor"})
	env, resources, err := actor.prepareAgentDirectories(context.Background(), []string{"CACHE"})
	require.NoError(t, err)
	require.NoError(t, os.Remove(env["CACHE"]))
	require.NoError(t, os.Symlink(root, env["CACHE"]))
	_, _, err = actor.prepareAgentDirectories(context.Background(), []string{"CACHE"})
	require.Error(t, err)
	_, err = planner.config.Inspector.BindSandboxProviderResources(context.Background(), resources)
	require.Error(t, err)
	for _, name := range []string{"../outside", "HOME", ""} {
		_, _, err = actor.prepareAgentDirectories(context.Background(), []string{name})
		require.Error(t, err)
	}
}

func TestSandboxGeneratedEnvironmentIsRetainedAndCannotBeOverridden(t *testing.T) {
	planner, _ := directoryPlanner(t, true)
	work := t.TempDir()
	names := make([]string, 128)
	for index := range names {
		names[index] = fmt.Sprintf("CACHE_%03d", index)
	}
	selected, materialized := materializedLaunchPolicy(t, planner.config.Inspector, model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate, AgentDirectories: names, Filesystem: []model.SandboxFilesystemRule{{HostPath: work, Access: model.SandboxFilesystemWrite}}})
	actor := planner.ForExecution(model.ResolvedExecutionSpec{AgentID: "actor"})
	artifact, err := actor.Prepare(context.Background(), selected, materialized, ProcessSpec{Executable: "/bin/sh", Directory: work, Env: []string{"CACHE_000=caller-cannot-replace"}})
	require.NoError(t, err)
	require.NoError(t, VerifySandboxChild(context.Background(), artifact))
	input, _, err := readSandboxChild(artifact)
	require.NoError(t, err)
	require.NotContains(t, input.Environment, "CACHE_000=caller-cannot-replace")
	require.Len(t, input.ProviderResources, 128)
	require.Contains(t, input.Environment, "CACHE_000="+input.ProviderResources[0].Source)
	for _, pin := range input.ProviderResources {
		require.Equal(t, model.SandboxFilesystemWrite, pin.Access)
	}
}
