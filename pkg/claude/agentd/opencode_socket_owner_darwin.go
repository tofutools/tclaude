//go:build darwin

package agentd

import (
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
)

// macOS has no /proc socket-inode view. lsof and ps are part of the base OS and
// let us fail closed unless the managed process (or a child wrapper launched)
// owns the listener.
func openCodeProcessOwnsEndpoint(rootPID int, endpoint string) bool {
	if rootPID <= 1 {
		return false
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return false
	}
	out, err := exec.Command("lsof", "-nP",
		"-iTCP@127.0.0.1:"+port, "-sTCP:LISTEN", "-Fp").Output()
	if err != nil {
		return false
	}
	owners := map[int]struct{}{}
	for _, line := range strings.Split(string(out), "\n") {
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
	for _, line := range strings.Split(string(processes), "\n") {
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
