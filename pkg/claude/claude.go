package claude

import (
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude/conv"
	"github.com/tofutools/tclaude/pkg/claude/credentials"
	claudegit "github.com/tofutools/tclaude/pkg/claude/git"
	"github.com/tofutools/tclaude/pkg/claude/selftest"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/setup"
	"github.com/tofutools/tclaude/pkg/claude/stats"
	"github.com/tofutools/tclaude/pkg/claude/statusbar"
	"github.com/tofutools/tclaude/pkg/claude/usage"
	"github.com/tofutools/tclaude/pkg/claude/web"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

// Cmd returns the claude subcommand for use in other binaries.
func Cmd() *cobra.Command {
	cmd := boa.CmdT[session.NewParams]{
		Use:         "claude",
		Short:       "Claude Code utilities",
		Long:        "Claude Code utilities.\n\nWhen run without a subcommand, starts a new Claude session in the current directory.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			conv.Cmd(),
			credentials.Cmd(),
			session.Cmd(),
			claudegit.Cmd(),
			worktree.Cmd(),
			stats.Cmd(),
			usage.Cmd(),
			setup.Cmd(),
			statusbar.Cmd(),
			selftest.Cmd(),
			web.Cmd(),
		},
		RunFunc: func(params *session.NewParams, cmd *cobra.Command, args []string) {
			if err := session.RunNew(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
	cmd.Args = cobra.ArbitraryArgs
	return cmd
}
