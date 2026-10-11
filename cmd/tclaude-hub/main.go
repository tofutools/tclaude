// Command tclaude-hub is the federation relay that tclaude agentd instances
// dial out to. See `tclaude-hub --help` and docs/federation.md.
package main

import (
	"github.com/tofutools/tclaude/pkg/claude/cli"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/hubcmd"
)

func main() {
	hub.RunExecGuardian()
	cli.Main(version, hubcmd.RootCmd)
}

// version is stamped at build time via -ldflags "-X main.version=...".
var version string
