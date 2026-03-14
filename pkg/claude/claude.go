package claude

import (
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/conv"
	claudegit "github.com/tofutools/tclaude/pkg/claude/git"
	"github.com/tofutools/tclaude/pkg/claude/selftest"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/setup"
	"github.com/tofutools/tclaude/pkg/claude/stats"
	"github.com/tofutools/tclaude/pkg/claude/statusbar"
	"github.com/tofutools/tclaude/pkg/claude/task"
	"github.com/tofutools/tclaude/pkg/claude/usage"
	"github.com/tofutools/tclaude/pkg/claude/web"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
	"github.com/tofutools/tclaude/pkg/common"
)

// Cmd returns the claude subcommand for use in other binaries.
func Cmd() *cobra.Command {
	var logLevel string
	cmd := boa.CmdT[session.NewParams]{
		Use:         "claude",
		Short:       "Claude Code utilities",
		Long:        "Claude Code utilities.\n\nWhen run without a subcommand, starts a new Claude session in the current directory.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			conv.Cmd(),
			session.Cmd(),
			claudegit.Cmd(),
			worktree.Cmd(),
			stats.Cmd(),
			usage.Cmd(),
			setup.Cmd(),
			statusbar.Cmd(),
			selftest.Cmd(),
			web.Cmd(),
			task.Cmd(),
		},
		RunFunc: func(params *session.NewParams, cmd *cobra.Command, args []string) {
			if err := session.RunNew(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
	cmd.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		finalLogLevel := logLevel
		if !cmd.Flags().Changed("log-level") {
			if cfg, err := config.Load(); err == nil && cfg.LogLevel != "" {
				finalLogLevel = cfg.LogLevel
			}
		}
		common.SetupLogging(common.ParseLogLevel(finalLogLevel))
	}
	cmd.Args = cobra.ArbitraryArgs
	cmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	return cmd
}
