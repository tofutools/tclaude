package session

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// explicitLaunchFields records which launch flags the caller actually typed,
// keyed by flag name. Cobra's Changed() is the ONLY place the distinction
// between "omitted" and "explicitly set to the default" still exists — by the
// time a flag reaches NewParams, `--auto-memory=false` and no flag at all are
// the same bool. Both the global-default profile and the relaunch carryover
// fill omitted fields only, so both need this.
//
// A map rather than a struct so a new entry in launchCarryoverFields needs no
// parallel edit here.
type explicitLaunchFields map[string]bool

func (e explicitLaunchFields) has(flag string) bool { return e[flag] }

// RunNewFromCommand launches a session from a Cobra CLI surface while
// preserving the distinction between an omitted launch flag and an explicitly
// empty one. The dashboard/global profile only fills omitted fields, and on a
// --resume so does the recorded launch posture.
func RunNewFromCommand(params *NewParams, cmd *cobra.Command) error {
	explicit := explicitLaunchFields{}
	cmd.Flags().Visit(func(f *pflag.Flag) { explicit[f.Name] = true })
	return runNewWithGlobalDefault(params, explicit)
}

// RunNew is the exported programmatic entry point. Programmatic callers retain
// their historical raw launch behavior for the global default profile; only the
// two direct Cobra entrypoints opt into terminal default resolution through
// RunNewFromCommand.
//
// The relaunch carryover is NOT part of that opt-in, though, and applies here
// too: it is not a convenience default, it is what stops a resume from erasing
// the record. `worktree add` resumes a copied conversation through this entry
// point, so leaving it out would keep a second `-r` path outside the contract.
// With no Cobra command there is no Changed() to consult, so the table falls
// back to "a non-zero value on params is the caller's own" — which is exactly
// what a programmatic caller expresses.
func RunNew(params *NewParams) error {
	if err := applyRecordedLaunchPosture(params, nil); err != nil {
		return err
	}
	return runNew(params)
}

func runNewWithGlobalDefault(params *NewParams, explicit explicitLaunchFields) error {
	if err := applyGlobalDefaultLaunchProfile(params, explicit); err != nil {
		return err
	}
	// A --resume must relaunch the conversation the way it was launched, so the
	// recorded posture fills every flag the caller left out. This runs BEFORE
	// runNew because runNew validates and records straight off params: a field
	// still blank here is what gets asserted onto the fresh session row, and the
	// durable projection turns that assertion into permanent loss (TCL-730).
	if err := applyRecordedLaunchPosture(params, explicit); err != nil {
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
		if !explicit.has("harness") && strings.TrimSpace(params.Harness) == "" {
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

	if !explicit.has("harness") && strings.TrimSpace(params.Harness) == "" {
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

	if !explicit.has("model") && strings.TrimSpace(params.Model) == "" {
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
	if !explicit.has("effort") && strings.TrimSpace(params.Effort) == "" {
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
