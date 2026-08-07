//go:build darwin

package portowner

import (
	"os/exec"
	"strconv"
	"strings"
)

// ProcessOwnsLoopbackPort fails closed unless lsof proves that the launched
// process, or a descendant wrapper of it, owns the listener.
//
// macOS has no /proc, so this asks lsof which pids hold a LISTENing socket on
// 127.0.0.1:port and then walks each answer UP the parent chain looking for
// rootPID. That is the reverse of the Linux walk and it is the right direction
// here: `ps` gives a child-to-parent map cheaply, while enumerating descendants
// would mean inverting it first.
func ProcessOwnsLoopbackPort(rootPID, port int) bool {
	if rootPID <= 1 || port <= 0 || port > 65535 {
		return false
	}
	out, err := exec.Command("lsof", "-nP",
		"-iTCP@127.0.0.1:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fp").Output()
	if err != nil {
		return false
	}
	owners := map[int]struct{}{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if !strings.HasPrefix(line, "p") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimPrefix(line, "p"))
		if err == nil {
			owners[pid] = struct{}{}
		}
	}
	if _, found := owners[rootPID]; found {
		return true
	}

	processes, err := exec.Command("ps", "-ax", "-o", "pid=,ppid=").Output()
	if err != nil {
		return false
	}
	parents := map[int]int{}
	for line := range strings.SplitSeq(string(processes), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		if pidErr == nil && ppidErr == nil {
			parents[pid] = ppid
		}
	}
	for owner := range owners {
		for depth := 0; owner > 1 && depth < 256; depth++ {
			if owner == rootPID {
				return true
			}
			parent, found := parents[owner]
			if !found || parent == owner {
				break
			}
			owner = parent
		}
	}
	return false
}

// HasLoopbackListener reports whether ANY process is listening on
// 127.0.0.1:port, so a caller can tell "never came up" from "someone else won
// the port" without touching the socket. It says nothing about whose it is.
func HasLoopbackListener(port int) bool {
	if port <= 0 || port > 65535 {
		return false
	}
	out, err := exec.Command("lsof", "-nP",
		"-iTCP@127.0.0.1:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fp").Output()
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.HasPrefix(line, "p") {
			return true
		}
	}
	return false
}

// ProcessInSubtree has no Darwin implementation and therefore fails closed.
// The ownership proof above does not need it: it walks parents from the
// listener rather than descendants from the root.
func ProcessInSubtree(_, _ int) bool { return false }

// ProcessSubtree reports only the root itself on Darwin. Callers use it to
// bound bookkeeping, not to make a trust decision — ProcessOwnsLoopbackPort is
// the trust decision, and it does its own parent walk.
func ProcessSubtree(rootPID int) []int {
	if rootPID <= 1 {
		return nil
	}
	return []int{rootPID}
}
