// Command tclaude is the full tclaude CLI. The repository also builds a
// standalone daemon binary in cmd/tclaude-agentd; both share the entry
// sequence in pkg/claude/cli.
package main

import (
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude"
	"github.com/tofutools/tclaude/pkg/claude/cli"
)

func main() {
	cli.Main(version, func() *cobra.Command {
		cmd := claude.Cmd()
		cmd.Use = "tclaude"
		return cmd
	})
}

// version, when non-empty, is the version stamped at build time via
// -ldflags "-X main.version=...". Both the GoReleaser release builds and the
// Homebrew formula inject it. It is empty for a plain `go build`.
var version string
