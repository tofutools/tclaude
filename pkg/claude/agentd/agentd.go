// Package agentd implements `tclaude agentd` — a foreground HTTP daemon
// that owns the agent-coordination data plane (groups, members, messages,
// tokens, tmux delivery). See the agentd design on Linear (JOH-10; original in git history).
package agentd

import (
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// Cmd returns the `tclaude agentd` cobra command.
func Cmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "agentd",
		Short:       "Run the agent coordination daemon (HTTP, foreground)",
		Long:        "Foreground HTTP server that handles cross-session agent messaging. Run from a non-sandboxed shell so it can reach the tmux socket and the SQLite DB.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			serveCmd(),
		},
	}.ToCobra()
}
