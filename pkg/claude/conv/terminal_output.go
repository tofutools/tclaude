package conv

import "github.com/tofutools/tclaude/pkg/common/termtext"

// cliStdout and cliStderr are where the conv commands print. They print
// conversation titles, which an agent can set by renaming itself, so on a
// terminal both are cleaned of what the terminal would interpret (termtext).
// Piped or redirected output, and a child process handed the terminal, are
// untouched.
var (
	cliStdout = termtext.Stdout
	cliStderr = termtext.Stderr
)
