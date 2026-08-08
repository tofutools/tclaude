package session

import (
	"fmt"
	"net"
)

// ResolveCopilotAPIPort settles which loopback port an API-backed Copilot pane
// binds, and refuses the two combinations that cannot mean anything.
//
// A port without the drive is refused rather than ignored. The pane would run
// on send-keys with no embedded server at all, so silently dropping the number
// would leave whoever passed it believing they had configured a channel that
// was never built.
//
// The drive WITH a port is the agentd case and the common one: the daemon
// allocated it before forking this process precisely so it holds the number
// before the pane exists (TCL-1054). It is taken as given, only range-checked.
//
// The drive WITHOUT a port allocates one here. That is the direct
// `tclaude session new --copilot-api` case, where there is no daemon waiting on
// the answer — nothing outside this process needs to have known the number in
// advance, so choosing it late costs nothing. It is NOT a fallback for a
// daemon-driven launch: agentd always passes the port it allocated, and a
// second allocation here would hand the pane a port the daemon is not watching.
//
// Allocation binds 127.0.0.1:0 and closes it, inheriting the same
// bind-close-exec gap agentd's allocator has, for the same reason: copilot
// cannot be given a pre-bound listener.
func ResolveCopilotAPIPort(copilotAPI bool, requested int) (int, error) {
	if !copilotAPI {
		if requested != 0 {
			return 0, fmt.Errorf(
				"--copilot-api-port %d needs --copilot-api: without the API drive the pane "+
					"runs no embedded server and the port would bind nothing", requested)
		}
		return 0, nil
	}
	if requested != 0 {
		if requested < 1 || requested > 65535 {
			return 0, fmt.Errorf(
				"--copilot-api-port %d is not a usable TCP port (want 1-65535)", requested)
		}
		return requested, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate a Copilot API port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, fmt.Errorf("release allocated Copilot API port %d: %w", port, err)
	}
	return port, nil
}
