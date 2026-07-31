//go:build linux

package session

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// readProcTable snapshots the parent links of every process in /proc.
//
// Only /proc/<pid>/stat is read here — one small file per process. Reading
// every /proc/<pid>/cmdline up front would double the syscalls on a busy
// host for data the caller almost never needs (it wants the argv of one
// agent's subtree, not of all several hundred processes), so cmdline stays
// lazy and is read per descendant during the walk.
func readProcTable() (procTable, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return procTable{}, false
	}
	parent := make(map[int]int, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue // not a process directory
		}
		// A process can exit mid-scan; that is normal, not a failure.
		if ppid, ok := readProcStatPPID(pid); ok {
			parent[pid] = ppid
		}
	}
	if len(parent) == 0 {
		return procTable{}, false
	}
	return procTable{parent: parent, cmdline: readProcCmdline}, true
}

// readProcStatPPID parses the ppid out of /proc/<pid>/stat. The comm field
// is parenthesised and may itself contain spaces and parens, so the parse
// starts after its LAST closing paren — the same discipline GetParentPID
// uses.
func readProcStatPPID(pid int) (int, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	s := string(data)
	close := strings.LastIndex(s, ")")
	if close < 0 || close+2 >= len(s) {
		return 0, false
	}
	fields := strings.Fields(s[close+2:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// readProcCmdline returns a process's full argv joined by spaces.
// /proc/<pid>/cmdline is NUL-separated and NUL-terminated. A kernel thread
// (or a process that exited) has an empty one, which is reported as "" —
// the walk simply skips it.
func readProcCmdline(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || len(data) == 0 {
		return ""
	}
	return strings.Join(strings.FieldsFunc(string(data), func(r rune) bool { return r == 0 }), " ")
}

// descendantCommandLinesViaChildren walks DOWN from rootPID using the kernel's
// own child list — /proc/<pid>/task/<tid>/children — reading only the agent's
// subtree instead of reconstructing it from every process on the host.
//
// supported=false means this kernel cannot answer (the children file needs
// CONFIG_PROC_CHILDREN, and a container may hide it), and the caller falls back
// to the full-table walk. It is reported separately from ok so an unsupported
// kernel degrades to the slower-but-equivalent path rather than to "cannot
// tell", which would leave every ledger to its TTL.
//
// Children are per-THREAD: a process forked from a non-main thread is listed
// under that thread's tid, so every entry in /proc/<pid>/task must be read. The
// task list is one directory read and a typical agent has few threads' worth of
// children, so a whole subtree costs a handful of reads against the ~235 the
// full scan needs on a modest host.
//
// The concurrency caveat is the same one the full scan already lives with: a
// process may fork or exit mid-walk, so this is a best-effort snapshot either
// way. It matters little here — the thing being matched is a background shell
// that outlives the turn, not something appearing and vanishing between reads.
func descendantCommandLinesViaChildren(rootPID int) (out []string, ok bool, supported bool) {
	rootChildren, err := readProcChildren(rootPID)
	if err != nil {
		// The root's own children file is the support probe. A root that has
		// exited reports ok=false through the caller's liveness check before
		// reaching here, so a failure now means the interface is unavailable.
		return nil, false, false
	}
	visited := map[int]struct{}{rootPID: {}}
	queue := rootChildren
	for len(queue) > 0 && len(visited) < maxProcTreeNodes {
		pid := queue[0]
		queue = queue[1:]
		if _, seen := visited[pid]; seen {
			continue
		}
		visited[pid] = struct{}{}
		if cmd := readProcCmdline(pid); cmd != "" {
			out = append(out, cmd)
		}
		// A descendant that exits mid-walk simply contributes no children;
		// that is normal, not a failure of the whole enumeration.
		if kids, err := readProcChildren(pid); err == nil {
			queue = append(queue, kids...)
		}
	}
	return out, true, true
}

// readProcChildren returns the pids the kernel reports as children of pid,
// unioned across all of its threads.
func readProcChildren(pid int) ([]int, error) {
	taskDir := filepath.Join("/proc", strconv.Itoa(pid), "task")
	tids, err := os.ReadDir(taskDir)
	if err != nil {
		return nil, err
	}
	var out []int
	unreadable := 0
	for _, tid := range tids {
		data, err := os.ReadFile(filepath.Join(taskDir, tid.Name(), "children"))
		if err != nil {
			// One thread exiting mid-read is normal; every thread failing means
			// the children interface is not available for this process.
			unreadable++
			continue
		}
		for _, field := range strings.Fields(string(data)) {
			if child, err := strconv.Atoi(field); err == nil && child > 0 {
				out = append(out, child)
			}
		}
	}
	if unreadable == len(tids) {
		return nil, fs.ErrNotExist
	}
	return out, nil
}
