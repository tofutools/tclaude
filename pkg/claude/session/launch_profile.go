package session

import (
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
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
	recordLaunchFlagPresence(params, explicit)
	if strings.TrimSpace(params.Cwd) != "" {
		if explicit.has("dir") {
			return fmt.Errorf("--cwd and --dir are aliases; pass only one")
		}
		params.Dir = params.Cwd
	}
	if strings.TrimSpace(params.JoinGroup) == "" {
		if automaticGroupEligible(params, explicit) {
			cfg, _ := config.Load()
			resolveAutomaticGroupConfig(params, explicit, cfg)
		} else {
			// The struct's default:true keeps help honest, but direct-only modes
			// (resume, shell, pass-through, managed children) must remain solo.
			params.AutoJoinGroup = false
			params.AutoJoinOrCreateGroup = false
		}
	}
	return runNewWithGlobalDefault(params, explicit)
}

func recordLaunchFlagPresence(params *NewParams, explicit explicitLaunchFields) {
	params.sandboxImplExplicit = explicit.has("sandbox-impl")
	params.CodexAppServerSpecified = explicit.has("codex-app-server")
}

func automaticGroupEligible(params *NewParams, explicit explicitLaunchFields) bool {
	if params.ManagedLaunch || strings.TrimSpace(params.Resume) != "" || params.Shell ||
		strings.TrimSpace(params.Harness) == ShellHarnessName || params.HelpContextFeatures ||
		clcommon.ShouldRunClaudeDirect(clcommon.ExtractClaudeExtraArgs()) {
		return false
	}
	// These are deliberately solo/direct-session controls with no counterpart
	// on the group spawn wire. Their explicit use preserves historical behavior
	// instead of silently discarding them during an automatic match.
	for _, flag := range []string{
		"label", "wait-for-rate-limit", "global", "permission-profile",
		"initial-prompt", "session-id", "send-keys", "copilot-api-port",
	} {
		if explicit.has(flag) {
			return false
		}
	}
	return true
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
//
// Two consequences for programmatic callers: a resume whose recorded posture
// cannot be READ fails here rather than launching on defaults (launching would
// overwrite the record with those defaults, which is not recoverable), and a
// carried posture that changes the launch is disclosed on os.Stderr.
func RunNew(params *NewParams) error {
	// Capture caller intent before resume carryover fills an omitted field.
	params.sandboxImplExplicit = strings.TrimSpace(params.SandboxImpl) != ""
	if err := applyRecordedLaunchPosture(params, nil); err != nil {
		return err
	}
	return runNew(params)
}

func runNewWithGlobalDefault(params *NewParams, explicit explicitLaunchFields) error {
	// Group launches go through agent.RunSpawn before direct-session profile
	// filling or harness validation. That shared daemon boundary owns the full
	// explicit > selected profile > group > global > harness resolution chain.
	if strings.TrimSpace(params.JoinGroup) != "" || params.AutoJoinGroup || params.AutoJoinOrCreateGroup {
		if JoinGroupHandler == nil {
			return fmt.Errorf("group joining is not wired up in this binary")
		}
		if err := JoinGroupHandler(params); !errors.Is(err, ErrNoAutomaticGroupMatch) {
			return err
		} else if err := validateUnmatchedGroupSpawnFlags(params); err != nil {
			return err
		} else {
			// Discovery has finished and deliberately fell through. Clear the
			// request bits so the ordinary solo-session global defaults apply.
			params.AutoJoinGroup = false
			params.AutoJoinOrCreateGroup = false
		}
	}
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

func resolveAutomaticGroupConfig(params *NewParams, explicit explicitLaunchFields, cfg *config.Config) {
	if !explicit.has("auto-join-group") {
		params.AutoJoinGroup = cfg.AutoJoinGroupEnabled()
	}
	if !explicit.has("auto-join-or-create-group") {
		params.AutoJoinOrCreateGroup = cfg.AutoJoinOrCreateGroupEnabled()
	}
	// An explicit false is the documented one-launch solo escape hatch. It must
	// also suppress auto-create inherited from config; an explicitly requested
	// auto-create remains an intentional group launch.
	if explicit.has("auto-join-group") && !params.AutoJoinGroup &&
		!explicit.has("auto-join-or-create-group") {
		params.AutoJoinOrCreateGroup = false
	}
}

// validateUnmatchedGroupSpawnFlags prevents a dashboard/agent-spawn-compatible
// flag from being silently ignored when automatic discovery legitimately
// falls through to a solo session.
func validateUnmatchedGroupSpawnFlags(params *NewParams) error {
	used := map[string]bool{
		"--profile":               strings.TrimSpace(params.Profile) != "",
		"--sandbox-profile":       strings.TrimSpace(params.SandboxProfile) != "",
		"--omit-sandbox-profiles": params.OmitSandboxProfiles,
		"--worktree":              strings.TrimSpace(params.Worktree) != "",
		"--worktree-base":         strings.TrimSpace(params.WorktreeBase) != "",
		"--worktree-repo":         strings.TrimSpace(params.WorktreeRepo) != "",
		"--initial-message":       strings.TrimSpace(params.InitialMessage) != "",
		"--file":                  strings.TrimSpace(params.File) != "",
		"--reply-to":              strings.TrimSpace(params.ReplyTo) != "",
		"--timeout":               strings.TrimSpace(params.SpawnTimeout) != "",
		"--ask-human":             strings.TrimSpace(params.AskHuman) != "",
		"--auto-focus":            params.AutoFocus,
		"--no-group-context":      params.NoGroupContext,
		"--task":                  strings.TrimSpace(params.Task) != "",
		"--task-label":            strings.TrimSpace(params.TaskLabel) != "",
		"--no-owner":              params.NoOwner,
		"--role":                  strings.TrimSpace(params.Role) != "",
		"--descr":                 strings.TrimSpace(params.Descr) != "",
	}
	for flag, present := range used {
		if present {
			return fmt.Errorf("%s applies to a group spawn, but no group matches this directory; pass --join-group, enable --auto-join-or-create-group, or omit the flag", flag)
		}
	}
	return nil
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
		strings.TrimSpace(params.JoinGroup) != "" || params.AutoJoinGroup || params.AutoJoinOrCreateGroup || params.Shell ||
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

	// Model and effort belong to one harness's catalog. A global profile for a
	// different harness is ambient configuration, not intent for this launch,
	// so it must not supply either field even when the resolved harness has a
	// permissive validator. Copilot deliberately accepts future/brokered model
	// tokens, which otherwise lets a Claude default such as opus[1m] leak into
	// `tclaude --harness copilot` and reach `copilot --model`.
	if profileMatchesHarness && !explicit.has("model") && strings.TrimSpace(params.Model) == "" {
		if raw := strings.TrimSpace(prof.Model); raw != "" {
			value, validateErr := h.Models.ValidateModel(raw)
			if validateErr != nil {
				return fmt.Errorf("global default spawn profile %q: %w", prof.Name, validateErr)
			}
			params.Model = value
		}
	}
	if profileMatchesHarness && !explicit.has("effort") && strings.TrimSpace(params.Effort) == "" {
		if raw := strings.TrimSpace(prof.Effort); raw != "" {
			value, validateErr := h.Models.ValidateEffort(raw)
			if validateErr != nil {
				return fmt.Errorf("global default spawn profile %q: %w", prof.Name, validateErr)
			}
			params.Effort = value
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
