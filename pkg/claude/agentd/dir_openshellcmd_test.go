package agentd

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestOpenShellCmdParsesInEveryShell is the regression check for the
// fish-on-macOS-iTerm2 bug: openShellCmd's output was
//
//	cd '<dir>' && exec "${SHELL:-bash}"
//
// which fish refuses to parse — `${VAR:-default}` is POSIX, not fish.
// The AppleScript driver (iTerm2 / Terminal.app) types this directly
// into the user's interactive shell, and the macOS CLI path routes
// through `$SHELL -l -c <command>` (loginShellArgv), so when that
// shell is fish, the open-terminal button fails with
// "fish: ${ is not a valid variable in fish."
//
// The fix wraps the parameter expansion in `sh -c '…'`: the
// single-quoted body is opaque to fish (and any other outer shell),
// and sh evaluates it. This test pins that by running the produced
// command through each shell's parse-only mode — if a future edit
// reintroduces a bare `${…}` outside the single-quoted body, the
// fish leg fails immediately.
func TestOpenShellCmdParsesInEveryShell(t *testing.T) {
	cmd := openShellCmd("/Users/me/some dir/with spaces")

	cases := []struct {
		bin  string
		args []string // appended *before* `-c <cmd>`
	}{
		{"fish", []string{"--no-execute"}},
		{"bash", []string{"-n"}},
		{"zsh", []string{"-n"}},
		{"sh", []string{"-n"}},
	}
	for _, c := range cases {
		t.Run(c.bin, func(t *testing.T) {
			path, err := exec.LookPath(c.bin)
			if err != nil {
				t.Skipf("%s not installed", c.bin)
			}
			argv := append(c.args, "-c", cmd)
			out, runErr := exec.Command(path, argv...).CombinedOutput()
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) || runErr != nil {
				t.Fatalf("%s rejected openShellCmd output:\nerr: %v\noutput: %s\ncmd: %s",
					c.bin, runErr, strings.TrimRight(string(out), "\n"), cmd)
			}
		})
	}
}
