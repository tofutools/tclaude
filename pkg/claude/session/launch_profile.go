package session

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

type explicitLaunchFields struct {
	harness bool
	model   bool
	effort  bool
}

// RunNewFromCommand launches a session from a Cobra CLI surface while
// preserving the distinction between an omitted launch flag and an explicitly
// empty one. The dashboard/global profile only fills omitted fields.
func RunNewFromCommand(params *NewParams, cmd *cobra.Command) error {
	explicit := explicitLaunchFields{
		harness: cmd.Flags().Changed("harness"),
		model:   cmd.Flags().Changed("model"),
		effort:  cmd.Flags().Changed("effort"),
	}
	return runNewWithGlobalDefault(params, explicit)
}

// RunNew is the exported programmatic entry point. Programmatic callers retain
// their historical raw launch behavior; only the two direct Cobra entrypoints
// opt into terminal default resolution through RunNewFromCommand.
func RunNew(params *NewParams) error {
	return runNew(params)
}

func runNewWithGlobalDefault(params *NewParams, explicit explicitLaunchFields) error {
	if err := applyGlobalDefaultLaunchProfile(params, explicit); err != nil {
		return err
	}
	return runNew(params)
}

// applyGlobalDefaultLaunchProfile gives fresh, human-owned terminal launches
// the same harness/model/effort baseline as the dashboard. Other profile fields
// deliberately remain agent-spawn policy: a directly attached human session
// continues to respect the harness's own sandbox and approval configuration.
func applyGlobalDefaultLaunchProfile(params *NewParams, explicit explicitLaunchFields) error {
	return applyGlobalDefaultLaunchProfileWithLookPath(params, explicit, exec.LookPath)
}

func applyGlobalDefaultLaunchProfileWithLookPath(
	params *NewParams,
	explicit explicitLaunchFields,
	lookPath func(string) (string, error),
) error {
	// Agentd already resolved the complete profile stack before forking its
	// managed session-new subprocess, so the child must remain exact.
	if params.ManagedLaunch || strings.TrimSpace(params.Resume) != "" ||
		strings.TrimSpace(params.JoinGroup) != "" || params.Shell ||
		strings.TrimSpace(params.Harness) == ShellHarnessName {
		return nil
	}

	prof, err := db.GlobalDefaultSpawnProfile()
	if err != nil {
		// Match the daemon's fail-open behavior for an unreadable/stale ambient
		// preference: a DB preference must not make the base CLI unusable.
		slog.Warn("session new: failed to load global default profile", "error", err)
		return nil
	}
	if prof == nil {
		if !explicit.harness && strings.TrimSpace(params.Harness) == "" {
			params.Harness = firstInstalledHarness(lookPath)
		}
		return nil
	}
	if prof.Disabled {
		reason := strings.TrimSpace(prof.DisabledReason)
		if reason == "" {
			reason = "no reason provided"
		}
		return fmt.Errorf("global default spawn profile %q is disabled: %s", prof.Name, reason)
	}

	if !explicit.harness && strings.TrimSpace(params.Harness) == "" {
		params.Harness = strings.TrimSpace(prof.Harness)
	}
	h, err := harness.Resolve(params.Harness)
	if err != nil {
		return fmt.Errorf("global default spawn profile %q: %w", prof.Name, err)
	}
	profileHarness := strings.TrimSpace(prof.Harness)
	if profileHarness == "" {
		profileHarness = harness.DefaultName
	}
	profileMatchesHarness := profileHarness == h.Name

	if !explicit.model && strings.TrimSpace(params.Model) == "" {
		if raw := strings.TrimSpace(prof.Model); raw != "" {
			value, validateErr := h.Models.ValidateModel(raw)
			switch {
			case validateErr == nil:
				params.Model = value
			case profileMatchesHarness:
				return fmt.Errorf("global default spawn profile %q: %w", prof.Name, validateErr)
			default:
				slog.Warn("session new: ignored global default profile model for a different harness",
					"profile", prof.Name, "harness", h.Name, "model", raw)
			}
		}
	}
	if !explicit.effort && strings.TrimSpace(params.Effort) == "" {
		if raw := strings.TrimSpace(prof.Effort); raw != "" {
			value, validateErr := h.Models.ValidateEffort(raw)
			switch {
			case validateErr == nil:
				params.Effort = value
			case profileMatchesHarness:
				return fmt.Errorf("global default spawn profile %q: %w", prof.Name, validateErr)
			default:
				slog.Warn("session new: ignored global default profile effort for a different harness",
					"profile", prof.Name, "harness", h.Name, "effort", raw)
			}
		}
	}
	return nil
}

// firstInstalledHarness chooses a spawnable registered harness whose launcher
// is on PATH. Claude Code is deliberately checked first for compatibility;
// remaining harnesses use the registry's stable sorted order. Empty means no
// registered harness launcher was found, so runNew retains its historical
// Claude fallback and produces the normal executable-not-found error at launch.
func firstInstalledHarness(lookPath func(string) (string, error)) string {
	names := append([]string{harness.DefaultName}, harness.Names()...)
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		h, ok := harness.Get(name)
		if !ok || h.Spawn == nil || h.Models == nil {
			continue
		}
		if _, err := lookPath(h.Spawn.Binary()); err == nil {
			return h.Name
		}
	}
	return ""
}
