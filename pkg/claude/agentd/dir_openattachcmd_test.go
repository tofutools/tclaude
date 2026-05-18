package agentd

import (
	"runtime"
	"strings"
	"testing"
)

// TestOpenAttachCmdExecReplacesShell pins the "hide closes the tab"
// behaviour for macOS iTerm2 / Terminal.app: openAttachCmd must prepend
// `exec ` so the wrapping shell is replaced by tclaude instead of
// returning to a prompt when the attach exits. Without the prefix the
// AppleScript drivers — which type the command into a default-profile
// interactive shell — would leave an orphaned tab open after a hide /
// detach. See the function's doc comment for the full rationale.
func TestOpenAttachCmdExecReplacesShell(t *testing.T) {
	cmd := openAttachCmd("worker-abc123")

	wantExec := runtime.GOOS != "windows"
	hasExec := strings.HasPrefix(cmd, "exec ")
	if wantExec && !hasExec {
		t.Fatalf("openAttachCmd should start with 'exec ' on %s so the wrapping shell is replaced; got: %s",
			runtime.GOOS, cmd)
	}
	if !wantExec && hasExec {
		t.Fatalf("openAttachCmd must NOT prepend 'exec ' on windows (cmd has no exec builtin); got: %s",
			cmd)
	}

	// The label always survives shell-quoted; covered separately by the
	// shellSingleQuote tests but pinned here as the user-visible
	// behaviour of openAttachCmd itself.
	if !strings.Contains(cmd, "'worker-abc123'") {
		t.Fatalf("openAttachCmd should pass the label as a single-quoted shell word; got: %s", cmd)
	}

	// The attach always goes through the `tclaude` wrapper, never raw
	// `tmux attach` — same invariant the autofocus flow test pins at the
	// integration level.
	if !strings.Contains(cmd, "session attach") {
		t.Fatalf("openAttachCmd should invoke `tclaude session attach`; got: %s", cmd)
	}
}
