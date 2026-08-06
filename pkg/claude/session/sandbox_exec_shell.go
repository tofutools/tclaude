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
// Every wrapper that renders this prefix must also locate it through
// sandboxExecShellPrefix rather than a literal, so a command can still be
// rewritten after the fact (tclaudeLayerPreserveRouteHelperFD) without the two
// spellings drifting apart.
func sandboxExecShellPrefix() string {
	return " -- " + clcommon.ShellQuoteArg(clcommon.BootstrapShellPath()) + " -c "
}
