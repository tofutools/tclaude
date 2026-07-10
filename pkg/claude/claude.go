package claude

import (
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/ask"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/terminal"
	"github.com/tofutools/tclaude/pkg/claude/conv"
	"github.com/tofutools/tclaude/pkg/claude/dbcmd"
	"github.com/tofutools/tclaude/pkg/claude/memoryfiles"
	"github.com/tofutools/tclaude/pkg/claude/processcmd"
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
	var logLevel string
	cmd := boa.CmdT[session.NewParams]{
		Use:         "claude",
		Short:       "Claude Code utilities",
		Long:        "Claude Code utilities.\n\nWhen run without a subcommand, starts a new Claude session in the current directory.",
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
			agent.Cmd(),
			agentd.Cmd(),
			memoryfiles.Cmd(),
			processcmd.Cmd(),
			dbcmd.Cmd(),
			ask.Cmd(),
			remoteaccess.Cmd(),
		},
		RunFunc: func(params *session.NewParams, cmd *cobra.Command, args []string) {
			if err := session.RunNew(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
	cmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if err := config.RelocateLegacyState(); err != nil {
			return fmt.Errorf("relocate legacy tclaude state: %w", err)
		}
		cfg, cfgErr := config.Load()
		finalLogLevel := logLevel
		if !cmd.Flags().Changed("log-level") && cfgErr == nil && cfg.LogLevel != "" {
			finalLogLevel = cfg.LogLevel
		}
		common.SetupLogging(common.ParseLogLevel(finalLogLevel))
		// Terminal preference, tier 2: the config file's `terminal`
		// field. The agentd serve --terminal flag (tier 1) overrides
		// this later, in runServe. Applies process-wide so every
		// command that opens a terminal — agentd, `session new` —
		// honours it.
		if cfgErr == nil && cfg.Terminal != "" {
			terminal.SetPreferred(cfg.Terminal)
		}
		return nil
	}
	cmd.Args = cobra.ArbitraryArgs
	cmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	session.RegisterJoinGroupCompletion(cmd)
	return cmd
}
