//go:build darwin

package terminal

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestEscapeAppleScript(t *testing.T) {
	cases := []struct{ in, want string }{
		{`plain`, `plain`},
		{`with "quotes"`, `with \"quotes\"`},
		{`back\slash`, `back\\slash`},
		{`both \ and "`, `both \\ and \"`},
	}
	for _, c := range cases {
		if got := escapeAppleScript(c.in); got != c.want {
			t.Errorf("escapeAppleScript(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestITermScriptPrefersDefaultProfile pins the two properties that keep
// iTerm2 spawning out of the launchd-PATH trap: a default-profile window
// (login shell with full PATH) and `write text` to run the command.
func TestITermScriptPrefersDefaultProfile(t *testing.T) {
	s := iTermScript(`tclaude session attach foo`)
	for _, want := range []string{
		`tell application "iTerm2"`,
		`create window with default profile`,
		`write text "tclaude session attach foo"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("iTermScript missing %q\nscript:\n%s", want, s)
		}
	}
}

func TestScriptsEscapeEmbeddedQuotes(t *testing.T) {
	cmd := `echo "hi"`
	if got := iTermScript(cmd); !strings.Contains(got, `\"hi\"`) {
		t.Errorf("iTermScript did not escape quotes: %s", got)
	}
	if got := terminalAppScript(cmd); !strings.Contains(got, `\"hi\"`) {
		t.Errorf("terminalAppScript did not escape quotes: %s", got)
	}
}

// TestLoginShellArgvShape pins the structure of the login-shell wrap:
// a /bin/sh trampoline that exec's the user's $SHELL as a login shell.
func TestLoginShellArgvShape(t *testing.T) {
	argv := loginShellArgv(`printf hi`)
	if len(argv) != 3 || argv[0] != "/bin/sh" || argv[1] != "-c" {
		t.Fatalf("loginShellArgv = %q, want [/bin/sh -c <script>]", argv)
	}
	if !strings.HasPrefix(argv[2], `exec "${SHELL:-/bin/zsh}" -l -c `) {
		t.Fatalf("loginShellArgv script %q does not exec $SHELL as a login shell", argv[2])
	}
}

// TestLoginShellWrapRoundTrip is the real proof: it runs the wrapped
// command through an actual shell and checks the original command —
// single quotes and all — survived the quoting intact. This is the
// macOS analogue of the Linux "command reaches argv verbatim" check.
func TestLoginShellWrapRoundTrip(t *testing.T) {
	// A command that itself contains single quotes — the case naive
	// string-concat quoting mangles.
	argv := loginShellArgv(`printf '%s' TCLAUDE_RT_OK`)
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("running login-shell wrap failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "TCLAUDE_RT_OK") {
		t.Fatalf("wrapped command output %q does not contain the expected marker", out)
	}
}

// TestDarwinCLIArgv pins the exec argv for each CLI-launched terminal:
// terminal-specific flags, then the login-shell wrap passed as separate
// argv elements (never a re-quoted string).
func TestDarwinCLIArgv(t *testing.T) {
	const bin = "/Applications/Example.app/Contents/MacOS/example"
	const command = `cd '/Users/me/my repo' && exec sh -c 'exec "${SHELL:-bash}"'`
	wrap := loginShellArgv(command)

	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{"ghostty", ghosttyArgv(bin, command), append([]string{bin, "-e"}, wrap...)},
		{"kitty", kittyArgv(bin, command), append([]string{bin}, wrap...)},
		{"wezterm", weztermArgv(bin, command), append([]string{bin, "start", "--"}, wrap...)},
		{"alacritty", alacrittyArgv(bin, command), append([]string{bin, "-e"}, wrap...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !reflect.DeepEqual(tc.got, tc.want) {
				t.Fatalf("argv = %q, want %q", tc.got, tc.want)
			}
		})
	}
}

// TestSingleQuote checks that singleQuote produces a shell word that an
// actual shell evaluates back to the original string — verified by
// round-tripping through /bin/sh.
func TestSingleQuote(t *testing.T) {
	for _, s := range []string{
		`plain`,
		`with spaces`,
		`embedded 'single' quotes`,
		`cd '/Users/me/my repo' && exec sh -c 'exec "${SHELL:-bash}"'`,
	} {
		out, err := exec.Command("/bin/sh", "-c", "printf %s "+singleQuote(s)).Output()
		if err != nil {
			t.Fatalf("singleQuote(%q): shell error: %v", s, err)
		}
		if string(out) != s {
			t.Errorf("singleQuote(%q) round-tripped to %q", s, out)
		}
	}
}
