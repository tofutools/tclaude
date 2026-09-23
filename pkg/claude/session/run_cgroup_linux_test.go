//go:build linux

package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// fakeRunCgroupTree builds a delegated root with one agent cgroup holding the
// caller, and points the cgroup and proc roots at it.
func fakeRunCgroupTree(t *testing.T, callerRel string, agentFiles map[string]string) (delegation, agentDir string) {
	t.Helper()
	oldRoot, oldProc := resourceCgroupRoot, resourceProcRoot
	resourceCgroupRoot = t.TempDir()
	resourceProcRoot = t.TempDir()
	t.Cleanup(func() { resourceCgroupRoot, resourceProcRoot = oldRoot, oldProc })
	delegation = filepath.Join(resourceCgroupRoot, "agentd.service")
	agentDir = filepath.Join(delegation, "tclaude-agent")
	require.NoError(t, os.MkdirAll(agentDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(delegation, "cgroup.controllers"), []byte("cpu memory pids"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(delegation, "cgroup.subtree_control"), []byte("cpu memory pids"), 0o644))
	for name, value := range agentFiles {
		require.NoError(t, os.WriteFile(filepath.Join(agentDir, name), []byte(value+"\n"), 0o644))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(resourceProcRoot, "4242"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(resourceProcRoot, "4242", "cgroup"),
		[]byte("0::/"+callerRel+"\n"), 0o644))
	t.Setenv(ResourceDelegationDirEnv, delegation)
	return delegation, agentDir
}

func TestPrepareRunCgroupClampsToCallerCeilings(t *testing.T) {
	delegation, _ := fakeRunCgroupTree(t, "agentd.service/tclaude-agent", map[string]string{
		"memory.max": "1073741824",
		"cpu.max":    "50000 100000",
		"pids.max":   "max",
	})
	pids := uint64(10)
	dir, applied, release, err := PrepareRunCgroup(4242, sandboxpolicy.ResourceLimits{
		Memory: "4GiB", PIDs: &pids,
	})
	require.NoError(t, err)
	t.Cleanup(release)
	require.Equal(t, delegation, filepath.Dir(dir))
	require.True(t, strings.HasPrefix(filepath.Base(dir), "tclaude-"))
	require.Equal(t, uint64(1<<30), applied.MemoryBytes, "a looser memory request is clamped")
	require.NotNil(t, applied.CPU)
	require.Equal(t, 0.5, *applied.CPU, "an unrequested axis inherits the caller's ceiling")
	require.NotNil(t, applied.PIDs)
	require.Equal(t, uint64(10), *applied.PIDs, "a tighter request is kept")

	read := func(name string) string {
		raw, readErr := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, readErr)
		return strings.TrimSpace(string(raw))
	}
	require.Equal(t, "1073741824", read("memory.max"))
	require.Equal(t, "50000 100000", read("cpu.max"))
	require.Equal(t, "10", read("pids.max"))
	require.Equal(t, "4242", read("cgroup.procs"), "the caller is moved into the new cgroup")
}

func TestPrepareRunCgroupWithoutCeilingsKeepsRequest(t *testing.T) {
	fakeRunCgroupTree(t, "agentd.service/tclaude-agent", nil)
	cpu := 2.0
	_, applied, release, err := PrepareRunCgroup(4242, sandboxpolicy.ResourceLimits{CPU: &cpu})
	require.NoError(t, err)
	t.Cleanup(release)
	require.NotNil(t, applied.CPU)
	require.Equal(t, 2.0, *applied.CPU)
	require.Empty(t, applied.Memory)
	require.Nil(t, applied.PIDs)
}

func TestPrepareRunCgroupRefusesCallerOutsideDelegation(t *testing.T) {
	fakeRunCgroupTree(t, "user.slice/session-1.scope", nil)
	_, _, _, err := PrepareRunCgroup(4242, sandboxpolicy.ResourceLimits{})
	require.ErrorContains(t, err, "outside agentd's delegated subtree")
}
