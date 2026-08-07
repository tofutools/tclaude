package opencodeapi

import (
	"net"
	"net/url"
	"strconv"

	"github.com/tofutools/tclaude/pkg/claude/common/portowner"
)

// ProcessOwnsEndpoint verifies that the managed process, or a descendant
// wrapper of it, owns the listener behind endpoint.
//
// The proof itself lives in portowner, shared with every other tclaude harness
// that reaches a locally launched process over loopback TCP. What stays here is
// only OpenCode's endpoint SPELLING: its runtime carries a URL, while the proof
// is about a port. A malformed endpoint, or one naming no usable port, fails
// closed exactly as an unprovable listener does.
func ProcessOwnsEndpoint(rootPID int, endpoint string) bool {
	port, ok := endpointPort(endpoint)
	if !ok {
		return false
	}
	return portowner.ProcessOwnsLoopbackPort(rootPID, port)
}

// ProcessInSubtree reports whether candidatePID belongs to rootPID's subtree.
func ProcessInSubtree(rootPID, candidatePID int) bool {
	return portowner.ProcessInSubtree(rootPID, candidatePID)
}

// RecordedProcessSubtree returns the pids attributable to rootPID.
func RecordedProcessSubtree(rootPID int) []int {
	return portowner.ProcessSubtree(rootPID)
}

func endpointPort(endpoint string) (int, bool) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return 0, false
	}
	_, rawPort, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return 0, false
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || port == 0 {
		return 0, false
	}
	return int(port), true
}
