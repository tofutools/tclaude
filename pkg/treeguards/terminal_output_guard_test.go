package treeguards

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The commands that print agent- or peer-authored text (message subjects and
// bodies, display names, conversation titles an agent can set by renaming
// itself, a peer's names and errors) print through their package's
// cliStdout/cliStderr, which are termtext.Stdout/Stderr: cleaned on a
// terminal of the control sequences it would interpret, untouched when piped
// (tcl-lokhg0, tcl-7gm2ec). A new raw os.Stdout, os.Stderr or fmt.Print* in
// these packages fails here.
//
// The exceptions are listed by line, not by file, so a file with one
// legitimate raw use cannot hide a second: a TUI or an attach that owns the
// raw terminal (it draws with escapes of its own, and cleans what it shows),
// a terminal check, or a child process handed the terminal.
var terminalOutputPackages = []string{
	"pkg/claude/agent",
	"pkg/claude/conv",
	"pkg/claude/federationcmd",
	"pkg/claude/session",
}

var rawStdio = regexp.MustCompile(`\bos\.(Stdout|Stderr)\b|\bfmt\.Print(f|ln)?\(`)

// terminalHandoff is a line that hands the raw terminal on rather than
// printing to it: a child process's stdio (cmd.Stdout = os.Stdout), or a
// question about the terminal itself (os.Stdout.Fd()). Neither prints.
var terminalHandoff = regexp.MustCompile(`^\w+\.(Stdin|Stdout|Stderr)\b[\w., ]*= |\bos\.(Stdin|Stdout|Stderr)\.Fd\(\)`)

// rawStdioAllowed holds "<package>/<file>\x00<trimmed line>" for each line
// that may use the raw streams, with the reason beside it.
var rawStdioAllowed = map[string]string{
	// The inbox TUI draws on the raw terminal and cleans its message view.
	"pkg/claude/agent/inbox_watch.go\x00os.Exit(runInboxWatch(p, os.Stdout, os.Stderr))": "TUI",
	// attach streams a remote terminal: its bytes are the peer's screen.
	"pkg/claude/federationcmd/attach.go\x00os.Exit(runAttach(cmd.Context(), p, os.Stdin, os.Stdout, os.Stderr))": "raw remote terminal",
	// The OSC title sequence is ours; termTitle cleans the title inside it.
	"pkg/claude/session/attach.go\x00fmt.Fprintf(os.Stdout, \"\\033]0;%s\\007\", termTitle(title))": "terminal title",
}

func TestCommandsPrintThroughTheTerminalWriters(t *testing.T) {
	root := moduleRoot(t)
	for _, pkg := range terminalOutputPackages {
		files, err := filepath.Glob(filepath.Join(root, pkg, "*.go"))
		require.NoError(t, err)
		require.NotEmpty(t, files, pkg)
		for _, path := range files {
			name := filepath.Base(path)
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			for i, line := range strings.Split(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "//") || !rawStdio.MatchString(line) || terminalHandoff.MatchString(trimmed) {
					continue
				}
				if _, ok := rawStdioAllowed[pkg+"/"+name+"\x00"+trimmed]; ok {
					continue
				}
				t.Errorf("%s/%s:%d prints to the raw terminal; use cliStdout/cliStderr (termtext): %s", pkg, name, i+1, trimmed)
			}
		}
	}
}

func TestTerminalHandoffMatchesOnlyHandoffs(t *testing.T) {
	for _, line := range []string{
		"cmd.Stdout = os.Stdout",
		"child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr",
		"if width, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && width > 0 {",
	} {
		require.True(t, terminalHandoff.MatchString(line), line)
	}
	for _, line := range []string{
		`fmt.Fprintf(os.Stdout, "%s\n", title)`,
		"out := os.Stdout",
		"w = os.Stderr",
		`fmt.Println(cmd.Stdout == nil, os.Stdout)`,
	} {
		require.False(t, terminalHandoff.MatchString(line), line)
	}
}
