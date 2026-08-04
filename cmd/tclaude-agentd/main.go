// Command tclaude-agentd is the standalone agent-coordination daemon. It
// exposes exactly what `tclaude agentd` does, one level higher up: run
// `tclaude-agentd serve` instead of `tclaude agentd serve`. Both entry points
// call the same code in pkg/claude/agentd, so the daemon behaves identically
// whichever binary starts it.
//
// This binary is not a replacement for installing tclaude. The daemon still
// runs tclaude subcommands out of process — it forks `tclaude session new` to
// spawn agents, `tclaude setup` from the tray, and builds `tclaude session
// attach` command lines for the dashboard's terminal links — so keep tclaude
// on PATH beside it.
package main

import (
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/cli"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

func main() {
	// The daemon builds command lines that run tclaude subcommands (session
	// attach, session exit-callback). Declare that this executable is not the
	// tclaude CLI so those resolve to a real tclaude rather than to us.
	clcommon.MarkSelfNotTclaude()
	cli.Main(version, agentd.RootCmd)
}

// version, when non-empty, is the version stamped at build time via
// -ldflags "-X main.version=...". The GoReleaser release builds inject it. It
// is empty for a plain `go build`.
var version string
