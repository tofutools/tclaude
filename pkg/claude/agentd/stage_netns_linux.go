//go:build linux

package agentd

import (
	"os"
	"strconv"

	"github.com/tofutools/tclaude/pkg/claude/common/portowner"
)

// stageForeignNetnsPID returns a process in root's tree that lives in a
// network namespace other than the daemon's (the agent's private sandbox
// namespace), or 0 when the whole tree shares the daemon's network.
// Breadth-first, so the shallowest such process (the sandbox's entry) wins.
func stageForeignNetnsPID(root int) int {
	self, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return 0
	}
	for _, pid := range portowner.ProcessSubtree(root) {
		ns, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/ns/net")
		if err == nil && ns != self {
			return pid
		}
	}
	return 0
}
