package session

import (
	"fmt"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// preLaunchFailExitCode is what the pane exits with when an operator's block
// fails. It matches the cwd-proof guard's refusal code: both mean "the launch
// was abandoned before the harness ever started", which is what the parent's
// readiness path and the operator's pane both need to see.
const preLaunchFailExitCode = 126

// RenderPreLaunchScript turns a snapshot's composed pre-launch blocks into the
// shell fragment that runs inside the sandbox, between the profile environment
// and the harness binary.
//
// Blocks run in the launching shell itself, not a subshell: their whole purpose
// is to leave environment behind for the harness, which a subshell would
// discard.
//
// Failure aborts the launch rather than starting a half-configured agent. An
// agent whose Playwright wrapper silently failed to install looks like a broken
// tool, and the operator debugs the wrong thing; a launch that stops with the
// block's name in the message says what actually happened. That is implemented
// with `set -e` plus an ERR trap naming the current block — the trap is what
// turns bash's bare non-zero exit into an attributable one. Both are unwound
// before the harness command, so nothing about this leaks into the harness's
// own shell semantics.
//
// Returns "" when there are no blocks, so a profile without them renders
// exactly the command it rendered before this feature existed.
func RenderPreLaunchScript(snapshot *sandboxpolicy.Snapshot) (string, error) {
	if snapshot == nil {
		return "", nil
	}
	return renderPreLaunchScript(
		snapshot.Effective.PreLaunch,
		clcommon.BootstrapShellIsBash(),
		clcommon.BootstrapShellPath(),
	)
}

// renderPreLaunchScript takes the resolved shell state as arguments so both
// branches are reachable in a test on any host, rather than only the one the
// developer happens to be sitting on.
func renderPreLaunchScript(
	blocks []sandboxpolicy.PreLaunchBlock,
	shellIsBash bool,
	shellPath string,
) (string, error) {
	if len(blocks) == 0 {
		return "", nil
	}
	// Operator-authored bash cannot be handed to whatever /bin/sh happens to
	// be. Refusing is the only honest option: running a bash script under dash
	// does not fail, it quietly does something else (TCL-1038).
	if !shellIsBash {
		return "", fmt.Errorf(
			"sandbox profile has %d pre-launch script block(s) but no bash was found on this host "+
				"(resolved %q); pre-launch blocks are bash and will not run correctly under it",
			len(blocks), shellPath)
	}

	var out strings.Builder
	out.WriteString("tclaude_pre_launch_failed() { ")
	out.WriteString("echo \"tclaude: pre-launch block '$1' failed; refusing harness launch\" >&2; ")
	out.WriteString(fmt.Sprintf("exit %d; }; ", preLaunchFailExitCode))
	out.WriteString("set -e; ")
	out.WriteString("trap 'tclaude_pre_launch_failed \"$tclaude_pre_launch_block\"' ERR; ")
	for _, b := range blocks {
		// The name rides as a quoted VALUE, never as interpolated shell text:
		// normalization bounds the charset, but a value assignment keeps that
		// from being the only thing standing between a name and the shell.
		out.WriteString("tclaude_pre_launch_block=" + clcommon.ShellQuoteArg(b.Name) + "\n")
		out.WriteString(b.Script)
		// An operator's block need not end in a newline, and without one its
		// last line would run into the next block's assignment.
		if !strings.HasSuffix(b.Script, "\n") {
			out.WriteString("\n")
		}
	}
	out.WriteString("trap - ERR; set +e; unset tclaude_pre_launch_block; ")
	return out.String(), nil
}
