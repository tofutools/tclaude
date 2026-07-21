//go:build darwin

package session

import (
	"os/exec"
	"strconv"
	"strings"
)

// readProcTable snapshots the process list via a single `ps` call. macOS
// has no /proc, and spawning one `ps` per descendant to read its argv would
// be far worse than asking for everything at once — so unlike the Linux
// implementation this fills the argv map eagerly and cmdline is a lookup.
//
//	-a   processes of all users
//	-x   including those with no controlling terminal (a background shell
//	     launched by an agent has none)
//	-ww  do not truncate the command column to the terminal width, which is
//	     essential here: the recorded shell command is matched against this
//	     argv, and a truncated one would silently stop matching.
func readProcTable() (procTable, bool) {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		return procTable{}, false
	}
	parent := map[int]int{}
	argv := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		pid, ppid, command, ok := parsePSLine(line)
		if !ok {
			continue
		}
		parent[pid] = ppid
		argv[pid] = command
	}
	if len(parent) == 0 {
		return procTable{}, false
	}
	return procTable{
		parent:  parent,
		cmdline: func(pid int) string { return argv[pid] },
	}, true
}

// parsePSLine splits one "  <pid> <ppid> <command…>" row. The command is
// everything after the second field and may contain any amount of
// whitespace, so it is taken as the remainder rather than field-split.
func parsePSLine(line string) (pid, ppid int, command string, ok bool) {
	rest := strings.TrimLeft(line, " \t")
	first := strings.IndexAny(rest, " \t")
	if first < 0 {
		return 0, 0, "", false
	}
	pid, err := strconv.Atoi(rest[:first])
	if err != nil {
		return 0, 0, "", false
	}
	rest = strings.TrimLeft(rest[first:], " \t")
	second := strings.IndexAny(rest, " \t")
	if second < 0 {
		return 0, 0, "", false
	}
	ppid, err = strconv.Atoi(rest[:second])
	if err != nil {
		return 0, 0, "", false
	}
	return pid, ppid, strings.TrimSpace(rest[second:]), true
}
