//go:build linux

package session

import (
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
