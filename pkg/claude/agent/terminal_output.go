package agent

import "github.com/tofutools/tclaude/pkg/common/termtext"

// cliStdout and cliStderr are where the agent commands print. Almost
// everything they print can carry agent- or peer-authored text (message
// subjects and bodies, display names, group names, drawing board and page titles,
// daemon errors that quote them), so on a terminal both are cleaned of the
// control sequences the terminal would otherwise interpret (termtext). Piped
// or redirected output passes through unchanged.
var (
	cliStdout = termtext.Stdout
	cliStderr = termtext.Stderr
)
