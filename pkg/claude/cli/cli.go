// Package cli holds the process-level entry sequence and root-command
// wiring shared by every tclaude binary. The repository ships more than one
// binary — the full `tclaude` CLI and the standalone `tclaude-agentd` daemon
// — and both need the same probe dispatch, logging setup, version stamping,
// legacy-state relocation and terminal preference handling. Keeping that here
// means the binaries duplicate behaviour without duplicating source.
package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/terminal"
	"github.com/tofutools/tclaude/pkg/claude/probehelper"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
)

// Main runs a tclaude binary end to end and never returns: it dispatches
// internal probe-helper invocations, sets up logging, builds the root command
// via newRoot, executes it, closes the database and exits.
//
// version is the value stamped into the binary's own main.version at build
// time via -ldflags; it is empty for a plain `go build`.
func Main(version string, newRoot func() *cobra.Command) {
	if handled, code := probehelper.Dispatch(os.Args); handled {
		os.Exit(code)
	}
	common.SetupLogging(slog.LevelInfo)
	exitCode := run(version, newRoot)
	db.Close()
	os.Exit(exitCode)
}

func run(version string, newRoot func() *cobra.Command) int {
	buildversion.SetStampedVersion(version)
	cmd := newRoot()
	cmd.Version = buildversion.AppVersion()
	if err := boa.Execute(cmd); err != nil {
		return 1
	}
	return 0
}

// ConfigureRoot installs the persistent behaviour every tclaude root command
// shares: the --log-level flag and the pre-run that relocates legacy state,
// resolves the effective log level from config, and applies the configured
// terminal preference process-wide.
func ConfigureRoot(cmd *cobra.Command) {
	var logLevel string
	cmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	cmd.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
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
}
