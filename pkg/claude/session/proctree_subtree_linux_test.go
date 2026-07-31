//go:build linux

package session

import (
	"os/exec"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startTree launches `sh -c "sleep … & sleep …; wait"`, which gives a real
// two-level subtree below a known root, and returns that root's pid.
func startTree(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & sleep 31; wait")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	pid := cmd.Process.Pid
	// Wait for the shell to actually have forked its children.
	require.Eventually(t, func() bool {
		lines, ok := DescendantCommandLines(pid)
		return ok && len(lines) == 2
	}, 5*time.Second, 20*time.Millisecond, "test subtree never came up")
	return pid
}

// The subtree walk is a pure optimisation: it must return exactly what
// reconstructing the tree from a full process-table snapshot returns. If these
// ever disagree, the badge this feeds would start lying.
func TestDescendantCommandLines_SubtreeMatchesFullScan(t *testing.T) {
	root := startTree(t)

	viaChildren, ok, supported := descendantCommandLinesViaChildren(root)
	require.True(t, supported, "this kernel exposes /proc/<pid>/task/<tid>/children")
	require.True(t, ok)

	table, tableOK := readProcTable()
	require.True(t, tableOK)
	viaTable := descendantsFromTable(table, root)

	sort.Strings(viaChildren)
	sort.Strings(viaTable)
	assert.Equal(t, viaTable, viaChildren,
		"the cheap walk and the full scan must describe the same subtree")
	assert.Len(t, viaChildren, 2)
}

// A root that is gone must report "cannot tell" (ok=false) rather than "nothing
// running" — the distinction the ledger depends on to avoid retiring every
// entry on a host where enumeration fails.
func TestDescendantCommandLines_DeadRootCannotTell(t *testing.T) {
	lines, ok := DescendantCommandLines(0)
	assert.False(t, ok)
	assert.Empty(t, lines)
}

// An empty-but-readable children list is positive evidence of "nothing running"
// (ok=true, no lines), which is what lets a finished background shell be
// retired. It must not be confused with the unreadable case above.
func TestDescendantCommandLines_LeafReportsNothingRunning(t *testing.T) {
	root := startTree(t)
	leaves, ok, supported := descendantCommandLinesViaChildren(root)
	require.True(t, supported)
	require.True(t, ok)
	require.NotEmpty(t, leaves)

	// The `sleep` processes themselves are leaves.
	table, tableOK := readProcTable()
	require.True(t, tableOK)
	var leafPID int
	for pid, ppid := range table.parent {
		if ppid == root {
			leafPID = pid
			break
		}
	}
	require.NotZero(t, leafPID, "expected a direct child of the test root")

	lines, ok, supported := descendantCommandLinesViaChildren(leafPID)
	assert.True(t, supported)
	assert.True(t, ok, "a readable-but-empty children list is an answer")
	assert.Empty(t, lines, "a leaf has no descendants")
}

// descendantsFromTable is the pre-existing full-scan walk, kept here as the
// reference implementation the optimisation is checked against.
func descendantsFromTable(table procTable, rootPID int) []string {
	children := make(map[int][]int, len(table.parent))
	for pid, ppid := range table.parent {
		if pid != ppid {
			children[ppid] = append(children[ppid], pid)
		}
	}
	var out []string
	visited := map[int]struct{}{rootPID: {}}
	queue := append([]int(nil), children[rootPID]...)
	for len(queue) > 0 && len(visited) < maxProcTreeNodes {
		pid := queue[0]
		queue = queue[1:]
		if _, seen := visited[pid]; seen {
			continue
		}
		visited[pid] = struct{}{}
		if cmd := table.cmdline(pid); cmd != "" {
			out = append(out, cmd)
		}
		queue = append(queue, children[pid]...)
	}
	return out
}
