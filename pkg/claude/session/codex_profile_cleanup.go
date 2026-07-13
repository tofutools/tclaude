package session

import (
	"errors"
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
	if !harness.IsCodexAgentLaunchProfilePath(path) {
		return fmt.Errorf("refusing to clean up non-managed Codex profile path %q", path)
	}
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect managed Codex launch profile: %w", err)
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("refusing to clean up managed Codex profile that is not a regular file")
	}

	report, promoteErr := harness.PromoteCodexLaunchProfileApprovals(path)
	if promoteErr != nil {
		if errors.Is(promoteErr, os.ErrNotExist) {
			return nil
		}
		if !harness.IsCodexLaunchProfileValidationError(promoteErr) {
			// Preserve the only copy of the explicit choice when the persistent
			// config is temporarily unreadable/unwritable. agentd's startup scan
			// can retry the still-valid profile later.
			return fmt.Errorf("persist Codex profile approvals (profile retained at %s): %w", path, promoteErr)
		}
		// A malformed profile cannot be reconciled safely. It is deliberately
		// removed without promoting anything.
		slog.Warn("codex profile cleanup: removing invalid managed profile", "path", path, "error", promoteErr)
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
	after, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reinspect managed Codex launch profile: %w", err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return fmt.Errorf("managed Codex launch profile changed during cleanup; refusing removal")
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
	cleanup := clcommon.DetectAbsoluteCmd("session", "codex-profile-cleanup")
	return codexProfileCleanupShell(path, cleanup)
}

func codexProfileCleanupShell(path, cleanup string) string {
	quoted := clcommon.ShellQuoteArg(path)
	// Probe --help before the legacy rm fallback. A current binary may return
	// non-zero because it intentionally retained a profile after a transient
	// merge failure; that must not be mistaken for an old binary and deleted.
	return cleanup + " --path " + quoted + " >/dev/null || { " + cleanup +
		" --help >/dev/null 2>&1 || rm -f -- " + quoted + "; }"
}
