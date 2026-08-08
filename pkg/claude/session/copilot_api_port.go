package session

import "fmt"

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
// The drive WITHOUT a port is refused, and this function used to ALLOCATE one
// instead — the defect TCL-1084 fixed. The allocation was written for a direct
// `tclaude session new --copilot-api`, on the reasoning that nothing outside this
// process needed the number in advance. True, and beside the point: nothing
// outside this process was going to DIAL it either. Only agentd creates the RPC
// session and holds the connection, so a locally-chosen port produced a pane
// with an unauthenticated loopback endpoint that nothing would ever drive.
//
// It is a backstop rather than the gate. resolveCopilotAPIDriveForLaunch decides
// this earlier and with the reason attached, because it can tell an operator who
// TYPED --copilot-api from a relaunch that inherited it, and it must run before
// any port is chosen. Reaching this refusal means the drive survived that gate
// without a port, which no path should manage; it stays because the alternative
// to a refusal here is a silently useless listener, and because it makes
// appendCopilotAPIPortFlag's contract in agentd true in both of its clauses
// rather than one.
func ResolveCopilotAPIPort(copilotAPI bool, requested int) (int, error) {
	if !copilotAPI {
		if requested != 0 {
			return 0, fmt.Errorf(
				"--copilot-api-port %d needs --copilot-api: without the API drive the pane "+
					"runs no embedded server and the port would bind nothing", requested)
		}
		return 0, nil
	}
	if requested == 0 {
		return 0, fmt.Errorf(
			"--copilot-api needs the port tclaude agentd allocated for it: the embedded " +
				"JSON-RPC server is only reachable by the daemon that created its session, so " +
				"a port chosen here would bind an unauthenticated loopback endpoint with " +
				"nothing driving it. Launch the agent through tclaude agentd, or launch it " +
				"without --copilot-api")
	}
	if requested < 1 || requested > 65535 {
		return 0, fmt.Errorf(
			"--copilot-api-port %d is not a usable TCP port (want 1-65535)", requested)
	}
	return requested, nil
}
