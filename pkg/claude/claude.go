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
	subCmds := []*cobra.Command{
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
	}
	// The proxy lends the daemon's credentials to an agent, so keep the
	// entire surface absent unless the operator has opted into it. Besides
	// keeping help honest, omitting the command (rather than hiding only its
	// leaves) makes an unconfigured `tclaude proxy ...` an unknown command.
	if proxy.Configured() {
		subCmds = append(subCmds, proxy.Cmd())
	}
	subCmds = append(subCmds,
		agentd.Cmd(),
		memoryfiles.Cmd(),
		processcmd.Cmd(),
		dbcmd.Cmd(),
		ask.Cmd(),
		remoteaccess.Cmd(),
	)
	cmd := boa.CmdT[session.NewParams]{
		Use:         "claude",
		Short:       "Coding-agent utilities",
		Long:        "Coding-agent utilities.\n\nWhen run without a subcommand, starts a new coding session in the current directory.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     subCmds,
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
