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

	// The plain (non-force) attach must NOT carry --force: it keeps the
	// unforced "already attached in another terminal" bail + native-window
	// focus behaviour the spawn/pending focus paths rely on.
	if strings.Contains(cmd, "--force") {
		t.Fatalf("openAttachCmd must not pass --force; got: %s", cmd)
	}
}

// TestOpenAttachCmdForceCarriesForce pins the web-window open path's attach:
// openAttachCmdForce must pass `--force` (→ tmux `attach-session -d`) so it
// atomically detaches any client already on the session and lands, instead of
// bailing "already attached in another terminal" and letting runPTYOverWS's
// teardown drop the old window (the bug it fixes). Everything else matches
// openAttachCmd — same `tclaude session attach`, same shell-quoted label, same
// `exec ` prefix off Windows.
func TestOpenAttachCmdForceCarriesForce(t *testing.T) {
	cmd := openAttachCmdForce("worker-abc123")

	if !strings.Contains(cmd, "--force") {
		t.Fatalf("openAttachCmdForce should pass --force so the attach detaches the old client; got: %s", cmd)
	}
	if !strings.Contains(cmd, "session attach") {
		t.Fatalf("openAttachCmdForce should invoke `tclaude session attach`; got: %s", cmd)
	}
	if !strings.Contains(cmd, "'worker-abc123'") {
		t.Fatalf("openAttachCmdForce should pass the label as a single-quoted shell word; got: %s", cmd)
	}

	wantExec := runtime.GOOS != "windows"
	if hasExec := strings.HasPrefix(cmd, "exec "); wantExec != hasExec {
		t.Fatalf("openAttachCmdForce exec-prefix mismatch on %s (want %v); got: %s", runtime.GOOS, wantExec, cmd)
	}

	// --force is a flag on `session attach`, so it must precede the label
	// (positional arg) — otherwise cobra would treat it as a second positional.
	if fi, li := strings.Index(cmd, "--force"), strings.Index(cmd, "'worker-abc123'"); fi < 0 || fi > li {
		t.Fatalf("--force must come before the label; got: %s", cmd)
	}
}
