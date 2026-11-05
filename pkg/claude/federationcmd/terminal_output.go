package federationcmd

import "github.com/tofutools/tclaude/pkg/common/termtext"

// cliStdout and cliStderr are where the federation commands print. What they
// print is mostly a peer's: its names, labels, titles, mail and errors.
// Ingress strips C0/C1 from federated text (proto.StripControls), but a path
// that skips it, or local text quoted beside it, would still reach the
// terminal raw; so on a terminal both are cleaned (termtext). Piped or
// redirected output passes through unchanged. attach is the exception: it
// streams a remote terminal, whose bytes are the peer's screen.
var (
	cliStdout = termtext.Stdout
	cliStderr = termtext.Stderr
)
