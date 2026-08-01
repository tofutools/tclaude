//go:build linux

package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func fakeCurrentResourceCgroup(t *testing.T, controllers, enabled string) string {
	t.Helper()
	oldRoot := resourceCgroupRoot
	resourceCgroupRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot = oldRoot })
	dir, err := currentCgroupDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.controllers"), []byte(controllers), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(enabled), 0o644))
	return dir
}

func TestWrapResourceLimitedCommandRendersIndependentAxes(t *testing.T) {
	current := fakeCurrentResourceCgroup(t, "cpu memory io", "")
	cpu := 0.5
	wrapped, cleanup, err := wrapResourceLimitedCommand(
		"session-one", sandboxpolicy.ResourceLimits{Memory: "1.5GiB", CPU: &cpu}, "exec harness",
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	entries, err := os.ReadDir(current)
	require.NoError(t, err)
	var cgroup string
	for _, entry := range entries {
		if entry.IsDir() && len(entry.Name()) > len("tclaude-") && entry.Name()[:len("tclaude-")] == "tclaude-" {
			cgroup = filepath.Join(current, entry.Name())
		}
	}
	require.NotEmpty(t, cgroup)
	memory, err := os.ReadFile(filepath.Join(cgroup, "memory.max"))
	require.NoError(t, err)
	assert.Equal(t, "1610612736", string(memory))
	cpuMax, err := os.ReadFile(filepath.Join(cgroup, "cpu.max"))
	require.NoError(t, err)
	assert.Equal(t, "50000 100000", string(cpuMax))
	assert.Contains(t, wrapped, "resource-limit-exec")
	assert.Contains(t, wrapped, "exec harness")
	enabled, err := os.ReadFile(filepath.Join(current, "cgroup.subtree_control"))
	require.NoError(t, err)
	assert.Equal(t, "+memory +cpu", string(enabled))
}

func TestWrapResourceLimitedCommandRequiresOnlyConfiguredController(t *testing.T) {
	current := fakeCurrentResourceCgroup(t, "memory", "memory")
	wrapped, cleanup, err := wrapResourceLimitedCommand(
		"memory-only", sandboxpolicy.ResourceLimits{Memory: "512MB"}, "harness",
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.NotEmpty(t, wrapped)
	enabled, err := os.ReadFile(filepath.Join(current, "cgroup.subtree_control"))
	require.NoError(t, err)
	assert.Equal(t, "memory", string(enabled), "already-enabled controller is not rewritten")
}

func TestWrapResourceLimitedCommandFailsWhenControllerIsNotDelegated(t *testing.T) {
	fakeCurrentResourceCgroup(t, "memory", "")
	cpu := 1.0
	_, _, err := wrapResourceLimitedCommand(
		"cpu-missing", sandboxpolicy.ResourceLimits{CPU: &cpu}, "harness",
	)
	assert.ErrorContains(t, err, "Delegate=yes")
}
