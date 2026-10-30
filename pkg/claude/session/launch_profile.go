package session

import (
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
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
	if err := adoptResumePassthrough(params); err != nil {
		return err
	}
	if err := resolveResumeLast(params); err != nil {
		return err
	}
	if err := expandAgentResumeSelector(params); err != nil {
		return err
	}
	if params.NoDaemon && strings.TrimSpace(params.JoinGroup) != "" {
		return fmt.Errorf("--no-daemon cannot be combined with --join-group")
	}
	if params.NoDaemon {
		params.AutoJoinGroup = false
		params.AutoJoinOrCreateGroup = false
		if err := validateUnmatchedGroupSpawnFlags(params); err != nil {
			return fmt.Errorf("--no-daemon: %w", err)
		}
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
	if params.ManagedLaunch || params.NoDaemon || strings.TrimSpace(params.Resume) != "" || params.Shell ||
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
	// Whatever the record left unknown falls back to the default profile, the
	// same baseline a fresh terminal launch gets.
	if err := applyResumeDefaultLaunchProfile(params, explicit); err != nil {
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
		"--owner":                 params.Owner,
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
// the same launch defaults as the dashboard: harness/model/effort, and — when
// the profile targets the launch's harness — its launch posture (sandbox,
// sandbox implementation, approval, and the other per-launch knobs; see
// applyProfileLaunchPosture). The human stays the trust root: a profile is
// configuration they chose, and every field still yields to an explicit flag.
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

	if profileMatchesHarness && !explicit.has("model-proxy") && params.ModelProxy == "" {
		params.ModelProxy = prof.ModelProxy
	}
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
	if profileMatchesHarness {
		applyProfileLaunchPosture(params, explicit, prof)
	}
	return nil
}

// applyResumeDefaultLaunchProfile fills a terminal `-r` from the global default
// spawn profile, for the posture fields the conversation never recorded.
//
// It runs AFTER applyRecordedLaunchPosture, so the order is explicit flag >
// recorded posture > profile. A recorded field — including one recorded as
// "nothing pinned" — is the conversation's own decision and is never replaced
// by an ambient default. Harness, model and effort are not touched: the harness
// is the conversation's, and the harness itself remembers its model.
func applyResumeDefaultLaunchProfile(params *NewParams, explicit explicitLaunchFields) error {
	if params.ManagedLaunch || strings.TrimSpace(params.Resume) == "" || params.Shell ||
		strings.TrimSpace(params.Harness) == ShellHarnessName {
		return nil
	}
	h, err := harness.Resolve(strings.TrimSpace(params.Harness))
	if err != nil {
		return nil // runNew reports the unknown harness with its own message
	}
	prof, err := db.GlobalDefaultSpawnProfile()
	if err != nil {
		slog.Warn("session new: failed to load global default profile", "error", err)
		return nil
	}
	if prof == nil || prof.Disabled {
		// A disabled profile blocks fresh launches; a resume of an existing
		// conversation simply gets no defaults from it.
		return nil
	}
	profileHarness := strings.TrimSpace(prof.Harness)
	if profileHarness == "" {
		profileHarness = harness.DefaultName
	}
	if profileHarness != h.Name {
		return nil
	}
	skip := explicitLaunchFields{}
	for flag := range explicit {
		skip[flag] = true
	}
	for flag := range params.recordedLaunchFlags {
		skip[flag] = true
	}
	applyProfileLaunchPosture(params, skip, prof)
	return nil
}

// applyProfileLaunchPosture fills the launch-posture flags the caller omitted
// from a spawn profile the caller has already matched to the launch's harness.
// runNew validates every filled value exactly as if it had been typed.
//
// Left out on purpose:
//   - copilot-api / codex-app-server: both drives exist only under agentd,
//     which allocates their channel before forking session new.
//   - identity, group-context, ownership and environment fields: agent-spawn
//     concepts with no direct-session counterpart.
func applyProfileLaunchPosture(params *NewParams, explicit explicitLaunchFields, prof *db.SpawnProfile) {
	fillString := func(flag string, dst *string, value string) {
		value = strings.TrimSpace(value)
		if value == "" || explicit.has(flag) || strings.TrimSpace(*dst) != "" {
			return
		}
		*dst = value
	}
	fillBool := func(flag string, dst *bool, value *bool) {
		if value == nil || explicit.has(flag) || *dst {
			return
		}
		*dst = *value
	}
	// Sandbox mode, implementation and a Codex permission profile are one
	// containment decision: the implementation can force the mode, and a
	// permission profile excludes --sandbox. Taking half of it from the profile
	// would override (or collide with) the half the caller typed or the
	// conversation recorded, so any one of them decided keeps the profile out
	// of all of them.
	sandboxDecided := explicit.has("sandbox") || explicit.has("sandbox-impl") ||
		explicit.has("permission-profile") ||
		strings.TrimSpace(params.Sandbox) != "" || strings.TrimSpace(params.SandboxImpl) != "" ||
		strings.TrimSpace(params.PermissionProfile) != ""
	if !sandboxDecided {
		fillString("sandbox", &params.Sandbox, prof.Sandbox)
		if params.Sandbox != "" && strings.TrimSpace(params.SandboxChosenBy) == "" {
			// Attribute the containment to the profile, as the daemon does,
			// rather than letting runNew record a flag the human never typed.
			// Same shape as agent.ProvGlobalProfileSource (an import cycle away).
			params.SandboxChosenBy = `global default profile "` + prof.Name + `"`
		}
		if impl := strings.TrimSpace(prof.SandboxImplementation); impl != "" {
			params.SandboxImpl = impl
			// A profile-supplied implementation is a fresh decision, never a
			// replay, so runNew must judge its applicability even on a resume.
			params.sandboxImplExplicit = true
		}
	}
	fillString("ask-for-approval", &params.Approval, prof.Approval)
	fillString("tools", &params.ToolGovernance, prof.ToolGovernance)
	fillString("ask-user-question-timeout", &params.AskUserQuestionTimeout, prof.AskUserQuestionTimeout)
	fillString("auto-compact-window", &params.AutoCompactWindow, prof.AutoCompactWindow)
	if !explicit.has("context-window-max") && params.ContextWindowMax == 0 {
		params.ContextWindowMax = prof.ContextWindowMax
	}
	fillBool("auto-review", &params.AutoReview, prof.AutoReview)
	fillBool("trust-dir", &params.TrustDir, prof.TrustDir)
	fillBool("remote-control", &params.RemoteControl, prof.RemoteControl)
	fillBool("auto-memory", &params.AutoMemory, prof.AutoMemory)
	fillBool("peer-messaging", &params.PeerMessaging, prof.PeerMessaging)
	if len(prof.ContextFeatures) > 0 {
		fillString("context-features", &params.ContextFeatures, formatContextFeaturesFlag(prof.ContextFeatures))
	}
	if prof.FastMode != nil {
		mode := "off"
		if *prof.FastMode {
			mode = "on"
		}
		fillString("fast-mode", &params.FastMode, mode)
	}
}

// formatContextFeaturesFlag renders a profile's feature map in the
// --context-features spelling (slug=state pairs, comma-separated, sorted).
func formatContextFeaturesFlag(features map[string]string) string {
	slugs := make([]string, 0, len(features))
	for slug := range features {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	parts := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		parts = append(parts, slug+"="+features[slug])
	}
	return strings.Join(parts, ",")
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
		if name == harness.ShellName {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		h, ok := harness.Get(name)
		if !ok || h.Spawn == nil || h.Models == nil || !h.Spawnable() {
			continue
		}
		if _, err := lookPath(h.Spawn.Binary()); err == nil {
			return h.Name
		}
	}
	return ""
}
