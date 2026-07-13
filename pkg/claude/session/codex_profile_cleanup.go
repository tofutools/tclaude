package session

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
)

type codexProfileCleanupParams struct {
	Path string `long:"path" required:"true" help:"Managed Codex launch-profile path"`
}

// codexProfileCleanupCmd is an internal pane-lifecycle command. agentd owns
// immediate fsnotify promotion; this command is the final reconciliation and
// cleanup fallback for a daemon that was stopped or missed an event.
func codexProfileCleanupCmd() *cobra.Command {
	cmd := boa.CmdT[codexProfileCleanupParams]{
		Use:         "codex-profile-cleanup",
		Short:       "Reconcile and remove a managed Codex launch profile",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *codexProfileCleanupParams, _ *cobra.Command, _ []string) {
			if err := cleanupCodexLaunchProfile(p.Path); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
	cmd.Hidden = true
	return cmd
}

func cleanupCodexLaunchProfile(path string) error {
	report, promoteErr := harness.PromoteCodexLaunchProfileApprovals(path)
	if promoteErr != nil {
		// Cleanup must not retain a proof-scoped authority file indefinitely.
		// Promotion fails closed; report it and continue removing the profile.
		slog.Warn("codex profile cleanup: approval promotion skipped", "path", path, "error", promoteErr)
	} else {
		if len(report.Conflicts) > 0 {
			slog.Warn("codex profile cleanup: kept existing global approval decisions",
				"path", path, "conflicts", report.Conflicts)
		}
		if report.Added > 0 {
			slog.Info("codex profile cleanup: persisted app-tool Always allow choice",
				"path", path, "added", report.Added, "already_present", report.Existing)
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove managed Codex launch profile: %w", err)
	}
	return nil
}

// CodexProfileCleanupShell is the shell fragment run after Codex exits. A
// tclaude binary that predates the hidden command falls back to the historical
// rm-only cleanup, preserving compatibility across self-upgrades.
func CodexProfileCleanupShell(path string) string {
	quoted := clcommon.ShellQuoteArg(path)
	return "tclaude session codex-profile-cleanup --path " + quoted +
		" >/dev/null 2>&1 || rm -f -- " + quoted
}
