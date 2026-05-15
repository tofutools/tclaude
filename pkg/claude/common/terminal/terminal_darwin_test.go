//go:build darwin

package terminal

import (
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
