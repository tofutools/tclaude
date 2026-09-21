package agentd

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The snapshot is parsed from `ps -axo pid=,ppid=,comm=`. comm on macOS is
// the executable PATH and may contain spaces (application bundles), so the
// parser must split only the two leading numeric fields.
func TestParseProcTable(t *testing.T) {
	out := "    1     0 /sbin/launchd\n" +
		"  261     1 /Applications/Orb Stack.app/Contents/MacOS/OrbStack\n" +
		" 4143     1 tmux\n" +
		"75501  4143 /bin/bash\n" +
		"75555 75554 " + harness.CodexName + "\n" +
		"garbage line\n" +
		"  -5     1 negative\n" +
		"\n"
	table := parseProcTable(out)
	require.Len(t, table, 5)
	assert.Equal(t, procTableEntry{ppid: 0, name: "launchd", exeName: true}, table[1])
	assert.Equal(t, procTableEntry{ppid: 1, name: "OrbStack", exeName: true}, table[261],
		"a path with spaces keeps its basename")
	assert.Equal(t, procTableEntry{ppid: 4143, name: "bash", exeName: true}, table[75501])
	assert.Equal(t, procTableEntry{ppid: 75554, name: harness.CodexName, exeName: true}, table[75555])
	_, negative := table[-5]
	assert.False(t, negative)
}

func swapProcReadersForTest(t *testing.T) {
	t.Helper()
	prevSnap, prevRead, prevAlive := brokerProcSnapshot, readProcEntry, procAlive
	prevParent, prevName, prevExe := procParent, procName, procExeName
	t.Cleanup(func() {
		brokerProcSnapshot, readProcEntry, procAlive = prevSnap, prevRead, prevAlive
		procParent, procName, procExeName = prevParent, prevName, prevExe
	})
}

// Long-lived ancestors come from the snapshot for free; a pid the snapshot
// does not know is read once per request and then served from the memo, so
// the second walk over the same chain costs nothing — and, more to the
// point, cannot observe the caller having exited in between.
func TestBrokerProcTable_ReadsAnUnknownPidOnce(t *testing.T) {
	swapProcReadersForTest(t)
	brokerProcSnapshot = func() map[int]procTableEntry {
		return map[int]procTableEntry{
			10: {ppid: 1, name: "sh", exeName: true},
			20: {ppid: 10, name: harness.CodexName, exeName: true},
		}
	}
	reads := map[int]int{}
	readProcEntry = func(pid int) procTableEntry {
		reads[pid]++
		switch pid {
		case 30:
			return procTableEntry{ppid: 20, name: "sh", exeName: true}
		case 40:
			return procTableEntry{ppid: 30, name: "tclaude", exeName: true}
		}
		return procTableEntry{}
	}

	table := newBrokerProcTable()
	for range 2 { // two walks, as the endpoints do
		cur := 40
		var chain []int
		for cur > 1 {
			chain = append(chain, cur)
			cur = table.parent(cur)
		}
		assert.Equal(t, []int{40, 30, 20, 10}, chain)
	}
	assert.Equal(t, map[int]int{30: 1, 40: 1}, reads,
		"only the pids outside the snapshot are read, and each exactly once per request")
	assert.Equal(t, harness.CodexName, table.harnessName(20))
	assert.Equal(t, "", table.harnessName(10))
	assert.Equal(t, 0, table.parent(999), "an unknown pid has no parent, like GetParentPID")
	assert.Equal(t, map[int]int{30: 1, 40: 1, 999: 1}, reads)
}

// exists is a liveness probe, never a fork and never a snapshot lookup: the
// caller is a brand-new pid every render, so snapshot presence would report
// every fresh caller as gone.
func TestBrokerProcTable_ExistsIsALiveProbe(t *testing.T) {
	swapProcReadersForTest(t)
	brokerProcSnapshot = func() map[int]procTableEntry { return map[int]procTableEntry{} }
	readProcEntry = func(int) procTableEntry { t.Fatal("exists must not read the process table"); return procTableEntry{} }
	procAlive = func(pid int) bool { return pid == 77 }
	table := newBrokerProcTable()
	assert.True(t, table.exists(77))
	assert.False(t, table.exists(78))
	assert.False(t, table.exists(0))
}

// Without an exe-grade name (Linux comm), the harness check falls back to
// harnessNameAt, which consults /proc/<pid>/exe — the Copilot main-thread
// rename case must keep working through the table.
func TestBrokerProcTable_CommNameFallsBackToExe(t *testing.T) {
	swapProcReadersForTest(t)
	brokerProcSnapshot = func() map[int]procTableEntry { return nil }
	readProcEntry = func(pid int) procTableEntry { return procTableEntry{ppid: 1, name: "MainThread"} }
	procExeName = func(pid int) string { return harness.CopilotName }
	table := newBrokerProcTable()
	assert.Equal(t, harness.CopilotName, table.harnessName(5))
}

// The whole-table read is the one cost that must not scale with the agent
// count: N concurrent requests inside the TTL share ONE snapshot.
func TestCachedProcSnapshot_IsSingleFlightWithinTTL(t *testing.T) {
	prevSnapshot := snapshotProcTableFn
	t.Cleanup(func() {
		snapshotProcTableFn = prevSnapshot
		procSnapshotCache.mu.Lock()
		procSnapshotCache.table, procSnapshotCache.taken = nil, time.Time{}
		procSnapshotCache.mu.Unlock()
	})
	procSnapshotCache.mu.Lock()
	procSnapshotCache.table, procSnapshotCache.taken = nil, time.Time{}
	procSnapshotCache.mu.Unlock()

	var forks atomic.Int32
	release := make(chan struct{})
	snapshotProcTableFn = func() map[int]procTableEntry {
		forks.Add(1)
		<-release
		return map[int]procTableEntry{1: {ppid: 0, name: "launchd", exeName: true}}
	}

	var wg sync.WaitGroup
	results := make([]map[int]procTableEntry, 20)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = cachedProcSnapshotOn("darwin")
		}()
	}
	// Let every goroutine reach the cache before the one fork completes.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.EqualValues(t, 1, forks.Load(), "twenty concurrent requests must share one ps")
	for _, r := range results {
		require.NotNil(t, r)
	}
	assert.EqualValues(t, 1, forks.Load())
	_ = cachedProcSnapshotOn("darwin")
	assert.EqualValues(t, 1, forks.Load(), "a request inside the TTL is served from the cache")
	assert.Nil(t, cachedProcSnapshotOn("linux"), "Linux never snapshots")
}
