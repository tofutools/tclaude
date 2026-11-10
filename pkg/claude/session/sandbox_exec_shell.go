package session

import (
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// sandboxExecShellPrefix is the argv tail that hands the harness command to a
// shell INSIDE the OS sandbox — the ` -- <shell> -c ` between the wrapper's own
// arguments and the quoted command.
//
// Harness spawners return a safe shell command rather than an argv, so the
// boundary has to re-enter a shell there. That shell is bash rather than `sh`
// for the reason BootstrapShellPath documents: the command it runs is not
// tclaude's text alone once sandbox profiles carry operator-authored
// pre-launch blocks.
//
// Every wrapper that renders or locates this boundary uses the same prefix
// so the sandbox shell and command separator cannot drift apart.
func sandboxExecShellPrefix() string {
	return " -- " + clcommon.BootstrapShellCommandPrefix() + " -c "
}
