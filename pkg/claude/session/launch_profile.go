package session

import (
	"errors"
	"fmt"
	"io"
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
	if strings.TrimSpace(params.JoinGroup) != "" {
		// An explicit join hands off to the spawn wire, which has no field for
		// a solo-only flag: refuse it rather than lose it (--permission-profile
		// would otherwise vanish from a Codex agent without a word).
		if solo := explicitSoloOnlyFlags(explicit); len(solo) > 0 {
			return fmt.Errorf("%s %s to a solo session and can't be used with --join-group. %s",
				joinFlags(solo), pluralVerb(solo, "applies", "apply"), soloJoinHint(solo))
		}
	}
	if strings.TrimSpace(params.JoinGroup) == "" {
		if automaticGroupEligible(params, explicit) {
			cfg, _ := config.Load()
			resolveAutomaticGroupConfig(params, explicit, cfg)
			if err := resolveSoloLaunchFlags(params, explicit, autoJoinNoticeOut); err != nil {
				return err
			}
		} else {
			// The struct's default:true keeps help honest, but direct-only modes
			// (resume, shell, pass-through, managed children) must remain solo.
			params.AutoJoinGroup = false
			params.AutoJoinOrCreateGroup = false
			// A human typing a group-only flag on one of these would lose it.
			// Only typed flags count: a managed child's argv is agentd's, and a
			// resume may have its posture filled in from the record.
			if !params.ManagedLaunch {
				if typed := explicitGroupSpawnOnlyFlags(params, explicit); len(typed) > 0 {
					return fmt.Errorf("%s only %s when a launch joins a group, and a resume, shell or pass-through launch never does; drop %s",
						joinFlags(typed), pluralVerb(typed, "applies", "apply"), joinFlags(typed))
				}
			}
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
		clcommon.ShouldRunClaudeDirect(clcommon.ExtractClaudeExtraArgs()) ||
		hasResumePassthroughArg(clcommon.ExtractClaudeExtraArgs()) {
		return false
	}
	return true
}

// soloOnlyLaunchFlags are direct-session controls with no counterpart on the
// group spawn wire. An explicit one keeps a launch solo rather than being
// discarded by an automatic group match; resolveSoloLaunchFlags says so.
//
// --label is here because the spawn request carries no label: agentd mints
// it (from the agent's name under agent.spawn_label_from_name), so -n is the
// group-launch equivalent. --initial-prompt is NOT here: on a group launch
// RunJoinGroup carries it as the startup brief, the same as -i.
var soloOnlyLaunchFlags = []string{
	"label", "wait-for-rate-limit", "global", "permission-profile",
	"session-id", "send-keys", "copilot-api-port",
}

func explicitSoloOnlyFlags(explicit explicitLaunchFields) []string {
	var flags []string
	for _, flag := range soloOnlyLaunchFlags {
		if explicit.has(flag) {
			flags = append(flags, "--"+flag)
		}
	}
	return flags
}

// soloLaunchHookForTest, when set, replaces the solo tail of a terminal
// launch so the CLI-level auto-join tests can observe "this launch went solo"
// without starting tmux. Always nil in production.
var soloLaunchHookForTest func(params *NewParams) error

// autoJoinNoticeOut receives the "kept solo" notice; a variable for tests.
var autoJoinNoticeOut io.Writer = cliStderr

// AutomaticGroupMatcher names the active group whose default directory
// matches this launch's directory ("" when none). Set by the agent package,
// which owns directory matching; nil leaves the solo-flag notice group-less.
var AutomaticGroupMatcher func(params *NewParams) (string, error)

// resolveSoloLaunchFlags runs once directory auto-join is resolved from flags
// and config. It is where a launch that would have joined a group but is kept
// solo by a solo-only flag says so, and where a group-spawn flag that a solo
// launch would drop is refused instead of ignored:
//
//   - an explicit --auto-join-group / --auto-join-or-create-group together
//     with a solo-only flag is a contradiction and an error;
//   - a group-spawn flag (-i, --sandbox-profile, ...) on a launch that cannot
//     join is an error naming what keeps it solo;
//   - otherwise a solo-only flag switches auto-join off for this launch, with
//     a one-line notice when a group would have been joined or created.
func resolveSoloLaunchFlags(params *NewParams, explicit explicitLaunchFields, stderr io.Writer) error {
	solo := explicitSoloOnlyFlags(explicit)
	spawnOnly := groupSpawnOnlyFlags(params)
	joining := params.AutoJoinGroup || params.AutoJoinOrCreateGroup
	if !joining {
		if len(spawnOnly) > 0 {
			return fmt.Errorf("%s only %s when this launch joins a group, but auto-join is off (--auto-join-group=false or config session.auto_join_group). To join, pass --join-group GROUP; to stay solo, drop %s",
				joinFlags(spawnOnly), pluralVerb(spawnOnly, "applies", "apply"), joinFlags(spawnOnly))
		}
		return nil
	}
	if len(solo) == 0 {
		return nil
	}
	subject := fmt.Sprintf("%s %s this session solo", joinFlags(solo), pluralVerb(solo, "keeps", "keep"))
	for _, request := range []struct {
		flag string
		on   bool
	}{
		{"auto-join-group", params.AutoJoinGroup},
		{"auto-join-or-create-group", params.AutoJoinOrCreateGroup},
	} {
		if request.on && explicit.has(request.flag) {
			return fmt.Errorf("--%s asks to join a group, but %s. %s; to stay solo, drop --%s",
				request.flag, subject, soloJoinHint(solo), request.flag)
		}
	}

	group := ""
	if AutomaticGroupMatcher != nil {
		// A lookup failure (say, an ambiguous directory) only costs the notice
		// its group name: this launch stays solo either way.
		group, _ = AutomaticGroupMatcher(params)
	}
	consequence := ""
	switch {
	case group != "":
		consequence = fmt.Sprintf(", so it won't join group %q", group)
	case params.AutoJoinOrCreateGroup:
		consequence = ", so no group is created for this directory"
	}
	params.AutoJoinGroup = false
	params.AutoJoinOrCreateGroup = false

	if len(spawnOnly) > 0 && consequence == "" {
		// Nothing to join here even without the solo flag, so a "to join" hint
		// would only lead to the no-match error.
		return fmt.Errorf("%s %s a group launch, but %s and no group matches this directory; pass --join-group GROUP without %s, or drop %s",
			joinFlags(spawnOnly), pluralVerb(spawnOnly, "needs", "need"), subject, joinFlags(solo), joinFlags(spawnOnly))
	}
	if len(spawnOnly) > 0 {
		return fmt.Errorf("%s %s a group launch, but %s%s. %s; to stay solo, drop %s",
			joinFlags(spawnOnly), pluralVerb(spawnOnly, "needs", "need"), subject, consequence,
			soloJoinHint(solo), joinFlags(spawnOnly))
	}
	if consequence != "" {
		fmt.Fprintf(stderr, "%s%s. %s; to stay solo, pass --auto-join-group=false.\n",
			subject, consequence, soloJoinHint(solo))
	}
	return nil
}

// soloJoinHint is the "how to join instead" half of a solo-flag message.
// --label maps to -n, the agent name a group launch derives its label from.
func soloJoinHint(solo []string) string {
	var drop []string
	label := false
	for _, flag := range solo {
		if flag == "--label" {
			label = true
			continue
		}
		drop = append(drop, flag)
	}
	switch {
	case label && len(drop) == 0:
		return "To join with a custom name, use -n NAME instead of --label"
	case label:
		return fmt.Sprintf("To join, drop %s and use -n NAME instead of --label", joinFlags(drop))
	default:
		return fmt.Sprintf("To join, drop %s", joinFlags(drop))
	}
}

func joinFlags(flags []string) string {
	switch len(flags) {
	case 0:
		return ""
	case 1:
		return flags[0]
	default:
		return strings.Join(flags[:len(flags)-1], ", ") + " and " + flags[len(flags)-1]
	}
}

func pluralVerb(flags []string, one, many string) string {
	if len(flags) == 1 {
		return one
	}
	return many
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
// carried posture that changes the launch is disclosed on cliStderr.
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
	// Everything below is the solo tail: group discovery has fallen through.
	if soloLaunchHookForTest != nil {
		return soloLaunchHookForTest(params)
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

// groupSpawnOnlyFlags lists, in a stable order, the dashboard/agent-spawn
// flags this launch carries that only a group spawn can honour. A solo launch
// would drop them, so every solo fall-through refuses them instead.
func groupSpawnOnlyFlags(params *NewParams) []string {
	used := []struct {
		flag    string
		present bool
	}{
		{"--profile", strings.TrimSpace(params.Profile) != ""},
		{"--sandbox-profile", strings.TrimSpace(params.SandboxProfile) != ""},
		{"--omit-sandbox-profiles", params.OmitSandboxProfiles},
		{"--worktree", strings.TrimSpace(params.Worktree) != ""},
		{"--worktree-base", strings.TrimSpace(params.WorktreeBase) != ""},
		{"--worktree-repo", strings.TrimSpace(params.WorktreeRepo) != ""},
		{"--initial-message", strings.TrimSpace(params.InitialMessage) != ""},
		{"--file", strings.TrimSpace(params.File) != ""},
		{"--reply-to", strings.TrimSpace(params.ReplyTo) != ""},
		{"--timeout", strings.TrimSpace(params.SpawnTimeout) != ""},
		{"--ask-human", strings.TrimSpace(params.AskHuman) != ""},
		{"--auto-focus", params.AutoFocus},
		{"--no-group-context", params.NoGroupContext},
		{"--task", strings.TrimSpace(params.Task) != ""},
		{"--task-label", strings.TrimSpace(params.TaskLabel) != ""},
		{"--owner", params.Owner},
		{"--no-owner", params.NoOwner},
		{"--role", strings.TrimSpace(params.Role) != ""},
		{"--descr", strings.TrimSpace(params.Descr) != ""},
	}
	var flags []string
	for _, u := range used {
		if u.present {
			flags = append(flags, u.flag)
		}
	}
	return flags
}

func explicitGroupSpawnOnlyFlags(params *NewParams, explicit explicitLaunchFields) []string {
	var typed []string
	for _, flag := range groupSpawnOnlyFlags(params) {
		if explicit.has(strings.TrimPrefix(flag, "--")) {
			typed = append(typed, flag)
		}
	}
	return typed
}

// validateUnmatchedGroupSpawnFlags prevents a dashboard/agent-spawn-compatible
// flag from being silently ignored when automatic discovery legitimately
// falls through to a solo session.
func validateUnmatchedGroupSpawnFlags(params *NewParams) error {
	if flags := groupSpawnOnlyFlags(params); len(flags) > 0 {
		return fmt.Errorf("%s %s to a group spawn, but no group matches this directory; pass --join-group, enable --auto-join-or-create-group, or omit %s",
			joinFlags(flags), pluralVerb(flags, "applies", "apply"), pluralVerb(flags, "the flag", "the flags"))
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
