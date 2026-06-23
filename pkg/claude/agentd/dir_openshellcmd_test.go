package agentd

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/terminal"
)

// TestOpenShellCmdFor_AppleScriptIsTerse pins the iTerm2 / Terminal.app
// shape: those launchers keystroke into a shell that's already
// interactive (the AppleScript default-profile window), so no
// `exec ${SHELL}` keepalive is needed and adding one just round-trips
// fish → sh → fish for nothing. The result must be exactly `cd '<dir>'`.
func TestOpenShellCmdFor_AppleScriptIsTerse(t *testing.T) {
	for _, id := range []string{terminal.IDITerm2, terminal.IDTerminalApp} {
		t.Run(id, func(t *testing.T) {
			got := openShellCmdFor("/Users/me/some dir", id)
			const want = `cd '/Users/me/some dir'`
			if got != want {
				t.Errorf("openShellCmdFor(%q): got %q, want %q", id, got, want)
			}
		})
	}
}

// TestOpenShellCmdFor_NonAppleScriptCarriesKeepalive pins the sh-c
// form: every non-AppleScript launcher (Linux terminals, WSL, macOS
// CLI terminals via loginShellArgv) wraps the payload in `sh -c`
// (directly or transitively), so the cd alone would let that wrapping
// shell exit and close the window. The trailing
// `&& exec sh -c 'exec "${SHELL:-bash}"'` is the keepalive; sh — not
// the outer shell, which may be fish — evaluates the `${SHELL:-bash}`.
func TestOpenShellCmdFor_NonAppleScriptCarriesKeepalive(t *testing.T) {
	for _, id := range []string{
		terminal.IDGhostty, terminal.IDKitty, terminal.IDWezterm,
		terminal.IDAlacritty, terminal.IDFoot, terminal.IDKonsole,
		terminal.IDGnomeTerminal, terminal.IDXfce4Terminal,
		terminal.IDXTermEmulator, terminal.IDXterm,
		"", /* unresolved — safe default is the keepalive form */
	} {
		t.Run(id, func(t *testing.T) {
			got := openShellCmdFor("/Users/me/some dir", id)
			const want = `cd '/Users/me/some dir' && exec sh -c 'exec "${SHELL:-bash}"'`
			if got != want {
				t.Errorf("openShellCmdFor(%q): got %q, want %q", id, got, want)
			}
		})
	}
}

// TestOpenShellCmdFor_ParsesInEveryShell is the regression check for
// the fish-on-macOS-iTerm2 bug: openShellCmd used to be
//
//	cd '<dir>' && exec "${SHELL:-bash}"
//
// which fish refuses to parse — `${VAR:-default}` is POSIX, not fish.
// The keepalive form's `${SHELL:-bash}` is now safely inside single
// quotes that only sh interprets. Both shapes the function produces
// (terse AppleScript and keepalive sh-c) must parse cleanly in every
// shell we might land in: fish (the user's macOS login shell), bash,
// zsh, POSIX sh.
//
// If a future edit reintroduces a bare `${…}` outside single quotes,
// the fish leg fails immediately.
func TestOpenShellCmdFor_ParsesInEveryShell(t *testing.T) {
	const dir = "/Users/me/some dir/with spaces"
	shapes := []struct {
		name string
		cmd  string
	}{
		{"AppleScript", openShellCmdFor(dir, terminal.IDITerm2)},
		{"keepalive", openShellCmdFor(dir, terminal.IDGhostty)},
	}
	shells := []struct {
		bin  string
		args []string
	}{
		{"fish", []string{"--no-execute"}},
		{"bash", []string{"-n"}},
		{"zsh", []string{"-n"}},
		{"sh", []string{"-n"}},
	}
	for _, sh := range shells {
		for _, sp := range shapes {
			t.Run(sh.bin+"/"+sp.name, func(t *testing.T) {
				path, err := exec.LookPath(sh.bin)
				if err != nil {
					t.Skipf("%s not installed", sh.bin)
				}
				argv := append(sh.args, "-c", sp.cmd)
				out, runErr := exec.Command(path, argv...).CombinedOutput()
				var exitErr *exec.ExitError
				if errors.As(runErr, &exitErr) || runErr != nil {
					t.Fatalf("%s rejected the %s shape:\nerr: %v\noutput: %s\ncmd: %s",
						sh.bin, sp.name, runErr, strings.TrimRight(string(out), "\n"), sp.cmd)
				}
			})
		}
	}
}
