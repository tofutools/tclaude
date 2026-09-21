package agentd

import (
	"bytes"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// --- per-request process-table view for the brokered endpoints ---
//
// A brokered hook or statusline callback is authenticated by walking the
// caller's process ancestry twice: once to resolve a candidate session row
// (hookSessionRowForPID) and once to prove the claimed pane is an ancestor
// (proveTclaudeLayerCaller). Each hop reads the process's parent and name.
// On Linux those are /proc reads and cost nothing. On macOS there is no
// /proc and every read is a `ps` fork — two per hop per walk, so one render
// cost ten to twenty forks. With ten or twenty agents each rendering a few
// times a second, the daemon forked `ps` on the order of a thousand times a
// second.
//
// That was slow enough to lose the race against the statusline client's
// three-second give-up: the client exited while the daemon was still
// between the two walks, the second walk asked `ps` for the parent of a
// pid that no longer existed, got 0, and the proof reported "pane pid is
// not an ancestor of caller pid" — a misconfiguration message for what
// was a timeout. The row was badged 🚫 for a render the client retried
// anyway.
//
// brokerProcTable fixes both halves without adding load that scales with
// the agent count:
//
//   - A daemon-wide SNAPSHOT of the process table (`ps -axo pid,ppid,comm`,
//     about 30ms for ~800 processes) is refreshed at most once per
//     brokerProcSnapshotTTL, single-flight, however many requests arrive.
//     Long-lived ancestors — the harness, its wrappers, the pane shell —
//     are answered from it for free.
//   - Pids the snapshot does not know (the render process itself and the
//     `sh -c` that spawned it are new every time) are read ONCE per request
//     with a single combined `ps -o ppid=,comm=` fork and memoised, so the
//     proof walk reuses what the resolution walk read. Typically two small
//     forks per request instead of ten to twenty, and the proof no longer
//     depends on the caller still being alive after the first walk.
//   - Liveness is a signal-0 probe: a syscall, never a fork.
//
// On Linux the snapshot is off and the per-pid reads are the /proc readers
// the walks used before; only the memoisation applies.
//
// Nothing about the trust boundary moves: the walks still key on host pids
// the kernel reports and rows the daemon recorded, and a snapshot is the
// kernel's own process table read once rather than many times.

// procTableEntry is one process as a read saw it.
type procTableEntry struct {
	ppid int
	// name is the process's name as the reader reports it. When exeName is
	// true it is the executable's basename: macOS `ps -o comm=` prints the
	// executable path, the same value GetProcessName AND GetProcessExeName
	// return there, so no second read can say more. When false it is the
	// Linux comm, and the harness check consults /proc/<pid>/exe as usual.
	name    string
	exeName bool
}

// brokerProcSnapshotTTL is how long a whole-table snapshot is served before
// the next request refreshes it. Long-lived ancestors do not change inside
// it; a brand-new pid falls through to a per-pid read regardless, so the TTL
// only bounds how stale a long-lived ancestor's parent may read — and a
// process's parent changes only when it is orphaned to init.
const brokerProcSnapshotTTL = 2 * time.Second

// brokerProcCommandTimeout bounds each `ps`. A wedged `ps` degrades to
// "unknown" rather than stalling the request.
const brokerProcCommandTimeout = 2 * time.Second

// brokerProcSnapshot returns the current whole-table snapshot, or nil when
// the platform has cheap per-pid reads (Linux) or the read failed. Indirected
// so tests that install a synthetic process tree over the per-pid readers can
// turn the snapshot off and be served by that tree alone.
var brokerProcSnapshot = cachedProcSnapshot

// readProcEntry reads one pid's parent and name, or ppid 0 when the process
// is unknown. Indirected for the same reason; the test default composes the
// per-pid readers so synthetic trees keep working.
var readProcEntry = readProcEntryDefault

// procAlive reports whether a pid exists. Indirected for the same reason.
var procAlive = session.IsProcessAlive

var procSnapshotCache struct {
	mu      sync.Mutex
	table   map[int]procTableEntry
	taken   time.Time
	loading bool
	done    chan struct{}
}

// cachedProcSnapshot serves the shared snapshot, refreshing it single-flight
// when it is older than brokerProcSnapshotTTL. Callers arriving during a
// refresh wait for it rather than forking their own.
func cachedProcSnapshot() map[int]procTableEntry {
	return cachedProcSnapshotOn(runtime.GOOS)
}

// snapshotProcTableFn is the whole-table read behind the cache, indirected
// so the single-flight property can be pinned without forking ps.
var snapshotProcTableFn = snapshotProcTable

func cachedProcSnapshotOn(goos string) map[int]procTableEntry {
	if goos != "darwin" {
		return nil
	}
	c := &procSnapshotCache
	for {
		c.mu.Lock()
		if c.table != nil && time.Since(c.taken) < brokerProcSnapshotTTL {
			t := c.table
			c.mu.Unlock()
			return t
		}
		if c.loading {
			done := c.done
			c.mu.Unlock()
			<-done
			continue
		}
		c.loading = true
		c.done = make(chan struct{})
		done := c.done
		c.mu.Unlock()

		table := snapshotProcTableFn()

		c.mu.Lock()
		if table != nil {
			c.table, c.taken = table, time.Now()
		}
		c.loading = false
		close(done)
		c.mu.Unlock()
		return table
	}
}

func snapshotProcTable() map[int]procTableEntry {
	cmd := exec.Command("ps", "-axo", "pid=,ppid=,comm=")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := runCommandWithTimeout(cmd, brokerProcCommandTimeout); err != nil {
		slog.Debug("broker: process-table snapshot failed; falling back to per-pid reads", "error", err, "module", "hooks")
		return nil
	}
	table := parseProcTable(out.String())
	if len(table) == 0 {
		return nil
	}
	return table
}

// parseProcTable parses `ps -axo pid=,ppid=,comm=` output. comm may contain
// spaces (an application bundle path), so only the first two fields are
// split; the remainder is the command, reduced to its basename.
func parseProcTable(out string) map[int]procTableEntry {
	table := map[int]procTableEntry{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil || pid <= 0 {
			continue
		}
		name := ""
		if len(fields) > 2 {
			name = filepath.Base(strings.Join(fields[2:], " "))
		}
		table[pid] = procTableEntry{ppid: ppid, name: name, exeName: true}
	}
	return table
}

// readProcEntryDefault reads one pid. On macOS a single `ps` yields both the
// parent and the executable path, replacing the two forks the separate
// readers cost. Elsewhere it composes the per-pid readers.
func readProcEntryDefault(pid int) procTableEntry {
	if runtime.GOOS != "darwin" {
		return procTableEntry{ppid: procParent(pid), name: procName(pid)}
	}
	cmd := exec.Command("ps", "-o", "ppid=,comm=", "-p", strconv.Itoa(pid))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := runCommandWithTimeout(cmd, brokerProcCommandTimeout); err != nil {
		return procTableEntry{}
	}
	fields := strings.Fields(out.String())
	if len(fields) < 1 {
		return procTableEntry{}
	}
	ppid, err := strconv.Atoi(fields[0])
	if err != nil {
		return procTableEntry{}
	}
	name := ""
	if len(fields) > 1 {
		name = filepath.Base(strings.Join(fields[1:], " "))
	}
	return procTableEntry{ppid: ppid, name: name, exeName: true}
}

// brokerProcTable is the view one brokered request walks: the shared
// snapshot plus a per-request memo of everything read outside it.
type brokerProcTable struct {
	snap map[int]procTableEntry
	memo map[int]procTableEntry
}

func newBrokerProcTable() *brokerProcTable {
	return &brokerProcTable{snap: brokerProcSnapshot(), memo: map[int]procTableEntry{}}
}

func (t *brokerProcTable) entry(pid int) procTableEntry {
	if e, ok := t.memo[pid]; ok {
		return e
	}
	if e, ok := t.snap[pid]; ok {
		return e
	}
	e := readProcEntry(pid)
	t.memo[pid] = e
	return e
}

// parent returns the parent pid, or 0 when the process is unknown — the same
// contract as session.GetParentPID.
func (t *brokerProcTable) parent(pid int) int {
	return t.entry(pid).ppid
}

// harnessName is harnessNameAt served from the table: the harness runtime
// name of the process at pid, or "" when it is not one.
func (t *brokerProcTable) harnessName(pid int) string {
	e := t.entry(pid)
	if e.exeName {
		if session.IsHarnessProcessName(e.name) {
			return e.name
		}
		return ""
	}
	return harnessNameAt(pid, e.name)
}

// exists reports whether the process is present right now.
func (t *brokerProcTable) exists(pid int) bool {
	return pid > 0 && procAlive(pid)
}
