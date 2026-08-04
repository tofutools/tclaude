// Package agentd implements `tclaude agentd` — a foreground HTTP daemon
// that owns the agent-coordination data plane (groups, members, messages,
// tokens, tmux delivery).
package agentd

import (
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/cli"
	"github.com/tofutools/tclaude/pkg/common"
)

const (
	daemonShort = "Run the agent coordination daemon (HTTP, foreground)"
	daemonLong  = "Foreground HTTP server that handles cross-session agent messaging. Run from a non-sandboxed shell so it can reach the tmux socket and the SQLite DB."
)

// subCmds returns the daemon's subcommands. Both entry points below — the
// `tclaude agentd` subcommand and the standalone `tclaude-agentd` binary —
// mount the same set, so the two ways of starting the daemon never drift.
func subCmds() []*cobra.Command {
	return []*cobra.Command{
		serveCmd(),
	}
}

// Cmd returns the `tclaude agentd` cobra command.
func Cmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "agentd",
		Short:       daemonShort,
		Long:        daemonLong,
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     subCmds(),
	}.ToCobra()
}

// RootCmd returns the root command of the standalone `tclaude-agentd` binary,
// where the daemon's subcommands sit directly under the binary itself:
// `tclaude-agentd serve` is the same code path as `tclaude agentd serve`.
func RootCmd() *cobra.Command {
	cmd := boa.CmdT[struct{}]{
		Use:         "tclaude-agentd",
		Short:       daemonShort,
		Long:        daemonLong + "\n\nThis is the standalone daemon binary; `tclaude agentd` in the main tclaude CLI runs the same daemon.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     subCmds(),
	}.ToCobra()
	cli.ConfigureRoot(cmd)
	return cmd
}
