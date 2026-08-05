package claude

import (
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/ask"
	"github.com/tofutools/tclaude/pkg/claude/cli"
	"github.com/tofutools/tclaude/pkg/claude/conv"
	"github.com/tofutools/tclaude/pkg/claude/dbcmd"
	"github.com/tofutools/tclaude/pkg/claude/memoryfiles"
	"github.com/tofutools/tclaude/pkg/claude/processcmd"
	"github.com/tofutools/tclaude/pkg/claude/proxy"
	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
	"github.com/tofutools/tclaude/pkg/claude/selftest"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/setup"
	"github.com/tofutools/tclaude/pkg/claude/stats"
	"github.com/tofutools/tclaude/pkg/claude/statusbar"
	"github.com/tofutools/tclaude/pkg/claude/task"
	"github.com/tofutools/tclaude/pkg/claude/usage"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
	"github.com/tofutools/tclaude/pkg/common"
)

// Cmd returns the claude subcommand for use in other binaries.
func Cmd() *cobra.Command {
	agentCmd := agent.Cmd()
	// The terminal dashboard implementation lives with agentd's TUI model,
	// but its operator-facing command belongs beside `agent dashboard`.
	agentCmd.AddCommand(agentd.TUIDashboardCmd())
	cmd := boa.CmdT[session.NewParams]{
		Use:         "claude",
		Short:       "Coding-agent utilities",
		Long:        "Coding-agent utilities.\n\nWhen run without a subcommand, starts a new coding session in the current directory.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			conv.Cmd(),
			session.Cmd(),
			worktree.Cmd(),
			stats.Cmd(),
			usage.Cmd(),
			setup.Cmd(),
			statusbar.Cmd(),
			selftest.Cmd(),
			task.Cmd(),
			agentCmd,
			proxy.Cmd(),
			agentd.Cmd(),
			memoryfiles.Cmd(),
			processcmd.Cmd(),
			dbcmd.Cmd(),
			ask.Cmd(),
			remoteaccess.Cmd(),
		},
		RunFunc: func(params *session.NewParams, cmd *cobra.Command, args []string) {
			if err := session.RunNewFromCommand(params, cmd); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
	_ = cmd.Flags().MarkHidden("managed-launch")
	cli.ConfigureRoot(cmd)
	cmd.Args = cobra.ArbitraryArgs
	session.RegisterJoinGroupCompletion(cmd)
	return cmd
}
