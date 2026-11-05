package agentd

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// templateAgentLaunch is the per-role launch profile of one template agent
// (JOH-239): a by-name spawn-profile reference plus inline launch overrides.
// It is the shape validateTemplateAgentLaunch returns (blanks preserved, for
// storage) and — after resolveTemplateAgentLaunch fills the referenced profile
// + harness secure defaults — the resolved shape the instantiator threads into
// spawnParams.
type templateAgentLaunch struct {
	ModelProxy string
	// PromptRefs is the saved-prompt set the agent's brief opens with: the
	// referenced profile's, else the group default profile's, else the
	// group's own default prompts, else the global default profile's.
	PromptRefs           []string
	SpawnProfile         string
	Environment          []sandboxpolicy.EnvironmentEntry
	EnvironmentOverrides []sandboxpolicy.EnvironmentEntry
	Harness              string
	Model                string
	Effort               string
	Sandbox              string
	// SandboxSource names the tier that CHOSE Sandbox above (an explicit field
	// on the template agent, the template's inline profile, the referenced
	// profile, the role's, the group default, or the global default). The
	// instantiator threads it into spawnParams so a wave-deployed agent's badge
	// names the profile that imposed its containment rather than crediting an
	// operator who only clicked deploy.
	SandboxSource  string
	Approval       string
	ToolGovernance string
	// TrustDir / AutoReview are the two *bool launch toggles a referenced spawn
	// profile carries (the same pair applyDefaultProfile overlays from a group's
	// default profile). They are resolved (not just validated) here so the
	// template instantiator threads them into spawnParams — without this a
	// profile's trust_dir never reaches a template-spawned agent, leaving it
	// frozen on the trust-folder dialog (JOH-205 regression for the template
	// path). Off by default; only a referenced profile turns them on.
	TrustDir   bool
	AutoReview bool
	// Set bits preserve an explicit false from the winning profile tier when
	// executeSpawn applies group/global safety-net defaults.
	TrustDirSet   bool
	AutoReviewSet bool
	// RemoteControl / AskUserQuestionTimeout are the remaining launch fields a
	// spawn profile (template-local or referenced) carries, resolved from the
	// profile tiers like TrustDir/AutoReview and threaded into spawnParams so a
	// template deploy honours them. Both default off/"".
	RemoteControl bool
	// AutoMemory keeps Claude Code's auto memory on for a template-deployed
	// agent. Resolved from the profile tiers like RemoteControl; defaults off,
	// which makes the launch inject CLAUDE_CODE_DISABLE_AUTO_MEMORY=1.
	AutoMemory bool
	// PeerMessaging keeps Claude Code's own cross-session messaging on for a
	// template-deployed agent. Resolved from the profile tiers like AutoMemory;
	// defaults off, which makes the launch inject the refusal.
	PeerMessaging bool
	// UsageSpawnProfile is the referenced registry profile, whose linked usage
	// profiles the deployed agent inherits exactly as a direct spawn naming it
	// would. Nil for an inline-only or profile-less member (an inline profile
	// has no registry row, so nothing can be linked to it).
	UsageSpawnProfile *db.SpawnProfile
	// SpawnCopyTiers are the template's own profile tiers (inline, then the
	// referenced profile), recorded as the deployed agent's spawn copy above
	// the group and global defaults.
	SpawnCopyTiers []db.SpawnCopyTier
	// RelaunchOverrides are the relaunch rows the referenced, group default
	// and global default profiles give the deployed agent (launch slice 11).
	RelaunchOverrides []db.RelaunchPolicyOverride
	// DefaultProfiles are the group and global default profiles this
	// resolution read; the spawn resolves its remaining fields from the same
	// ones rather than reading them again.
	DefaultProfiles *pinnedDefaultProfiles
	// IncludeGroupContext is whether the deploy folds the group's (template's)
	// shared context into this agent's brief. On unless a profile tier — the
	// referenced profile, or the group/global default — turns it off (TCL-1107).
	// Role and process blocks are added either way.
	IncludeGroupContext bool
	// SSHWorkaround is the resolved Git-over-SSH compatibility posture.
	// It defaults on for a managed Codex launch or any tclaude-layer launch,
	// then the effective sandbox clamps it to shapes that need it. A referenced
	// or inline profile may disable it.
	SSHWorkaround    bool
	SSHWorkaroundSet bool
	// ContextFeatures is the startup-context trim map a template-deployed agent
	// launches with, resolved from the profile tiers (whole, not merged — see
	// resolveContextFeaturesLaunchField). Empty means the agent keeps Claude
	// Code's own startup context.
	ContextFeatures map[string]string
	// StartupContext is the guidance carried by the highest compatible profile
	// tier. It is delivered separately from the template agent's task brief.
	StartupContext string
	// ContextFeaturesSet distinguishes "traced this member and it trims nothing"
	// from "could not trace it at all" (a pruned session row). The map alone
	// cannot: ResolveContextFeatures returns nil for an all-default map, so both
	// cases arrive as len()==0. The re-snapshot merge needs the difference — see
	// mergeSnapshotInlineProfile — mirroring AutoReviewSet.
	ContextFeaturesSet     bool
	AskUserQuestionTimeout string
	// SandboxImplementation is the resolved owner of OS-level confinement for a
	// template-deployed agent ("" = unset, so the spawn boundary's own default
	// applies). Resolved from the profile tiers like AskUserQuestionTimeout and
	// handed to the spawn as an explicit request, so a host that cannot run the
	// layer refuses the deploy loudly instead of degrading it.
	SandboxImplementation string
	// AutoCompactWindow is the auto-compaction context capacity (in tokens) a
	// template-deployed agent launches with, resolved from the profile tiers like
	// AskUserQuestionTimeout. "" leaves the model's own threshold in charge.
	// There is no template-LOCAL spelling for it: a template pins the window by
	// referencing a spawn profile that carries one.
	AutoCompactWindow string
	// ContextWindowMax is tclaude's configured Copilot context cap. It is
	// separate from the observed context snapshot and is carried into the
	// durable relaunch profile by the shared spawn core.
	ContextWindowMax int64
	// CopilotAPI selects the API-backed Copilot drive for a template-deployed
	// agent. Like AutoCompactWindow there is no template-LOCAL spelling: a
	// template selects it by referencing a Copilot spawn profile that carries it.
	// CopilotAPISet records that a tier actually spoke, mirroring AutoReviewSet.
	CopilotAPI    bool
	CopilotAPISet bool
	// CodexAppServer selects the private app-server-backed Codex drive. It is
	// profile-only and remains off unless a launch tier explicitly selects it.
	CodexAppServer    bool
	CodexAppServerSet bool
	// CopilotAPISource / SSHWorkaroundSource name the tier that CHOSE the value,
	// threaded into spawnParams exactly like SandboxSource. Without them the
	// deploy passes only the Set bit, the spawn boundary re-resolves an
	// already-set field as ProvExplicit, and every template-deployed agent
	// records a false "an operator chose this" for a value the resolver defaulted
	// (TCL-1090).
	CopilotAPISource     string
	CodexAppServerSource string
	SSHWorkaroundSource  string
	// HarnessSource / ModelSource / EffortSource / ContextWindowMaxSource /
	// FastModeSource / SandboxImplementationSource are the attributions for the
	// remaining fields the launch echo renders. This resolver has always computed
	// them and thrown them away; the deploy result now carries an echo, and
	// without these every template-pinned value would be re-resolved at the spawn
	// boundary as ProvExplicit — telling an operator "explicit" for a model their
	// ROLE PROFILE chose, which names no tier they can go and change (TCL-1097).
	HarnessSource               string
	ModelSource                 string
	EffortSource                string
	ContextWindowMaxSource      string
	FastModeSource              string
	SandboxImplementationSource string
	FastMode                    bool
	FastModeSet                 bool
	// Notes disclose profile-tier fields that were skipped because they are not
	// valid for the independently resolved harness. They ride the per-agent
	// instantiate result so template launches have the same least-surprise
	// disclosure as direct spawns.
	Notes []string
	// Info / Warnings are the launch-posture channels of the same echo, kept
	// apart from Notes for the reason the direct spawn path keeps them apart: a
	// warning is about the agent's blast radius, not about which tier won a
	// field, and a renderer must be able to label it as such.
	Info     []string
	Warnings []string
	// ObservedNotChosen names the launch fields this member was OBSERVED to carry
	// but that no operator chose — the harness default resolved them. The
	// snapshot deliberately does not pin these (a template is a specification,
	// not a recording), so they are listed here to be disclosed per-agent instead
	// of vanishing. Dropping them silently is the half of TCL-1090 that would
	// have been easy to ship and impossible to notice.
	ObservedNotChosen []string
	// Traced records the bare fact that this member was observed at all — that
	// traceMemberLaunch got past both of its early returns. It is NOT "traced and
	// useful"; do not widen it to mean that.
	//
	// The re-snapshot merge needs it because a blank Harness is ambiguous in the
	// worst possible way: THE CORRECT OBSERVATION IS SPELLED EXACTLY LIKE THE
	// ABSENCE OF ONE. A genuinely-traced Claude member yields a blank harness by
	// design (see below — the default is deliberately not stamped), and so does a
	// member that could not be observed at all. Keying the merge's carry-forward
	// on "the traced harness is blank" would therefore preserve a stale harness
	// pin for an agent an operator had genuinely moved to Claude, and drag that
	// harness's fields along behind it. Mirrors ContextFeaturesSet / AutoReviewSet
	// / CopilotAPISet, which exist for exactly this ambiguity (TCL-1083).
	Traced bool
}

// validateTemplateAgentLaunch validates one template agent's per-role launch
// profile at SAVE time and returns the normalized fields to store (JOH-239).
// It checks the referenced spawn profile exists and validates the inline
// overrides against the harness they will launch on — the agent's inline
// harness wins, else the template-local profile's, else the referenced
// profile's, else the default (Claude Code) — the same adoption order the
// instantiate-time overlay walks. A referenced role is intentionally irrelevant
// here: roles supply behavior and access, while spawn profiles own launch policy.
// Blank fields stay blank (Validate*, not Resolve*): the launch boundary applies
// its own secure defaults at instantiate.
func validateTemplateAgentLaunch(agentName string, a templateAgentJSON, inline *db.SpawnProfile, _ *db.Role) (templateAgentLaunch, *spawnFailure) {
	profRef := strings.TrimSpace(a.SpawnProfile)
	var refProfile *db.SpawnProfile
	if profRef != "" {
		p, err := db.ResolveSpawnProfile(profRef)
		if err != nil {
			return templateAgentLaunch{}, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if p == nil {
			return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_profile",
				fmt.Sprintf("agent %q: spawn_profile %q does not name a spawn profile — %s. "+
					"Create it first (`tclaude agent profiles create`, needs profiles.manage) or clear spawn_profile.",
					agentName, profRef, availableHint("spawn profiles", knownProfileNames()))}
		}
		profRef = p.Name
		refProfile = p
	}
	inlineHarness := strings.TrimSpace(a.Harness)
	valHarness := inlineHarness
	if valHarness == "" && inline != nil {
		valHarness = inline.Harness
	}
	if valHarness == "" && refProfile != nil {
		valHarness = refProfile.Harness
	}
	h, err := harness.ResolveSpawnable(valHarness)
	if err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_harness",
			fmt.Sprintf("agent %q: %s", agentName, err.Error())}
	}
	model, err := h.Models.ValidateModel(strings.TrimSpace(a.Model))
	if err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_model",
			fmt.Sprintf("agent %q: %s", agentName, err.Error())}
	}
	effort, err := h.Models.ValidateEffort(strings.TrimSpace(a.Effort))
	if err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_effort",
			fmt.Sprintf("agent %q: %s", agentName, err.Error())}
	}
	sandbox, err := harness.ValidateHarnessBuiltinMode(h, a.Sandbox)
	if err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_sandbox",
			fmt.Sprintf("agent %q: %s", agentName, err.Error())}
	}
	approval, err := harness.ValidateApprovalPolicy(h, a.Approval)
	if err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_approval",
			fmt.Sprintf("agent %q: %s", agentName, err.Error())}
	}
	// The template-local profile's fields were validated against its OWN
	// harness by buildInlineProfileFromJSON (blank = the Claude default). When
	// the agent's harness and the inline profile's harness are both blank, the
	// instantiate-time overlay leaves the accumulator's harness OPEN, so a
	// referenced spawn profile beneath can still impose a different harness —
	// and the inline fields (validated as Claude) would ride onto it, making
	// the template saveable but never instantiable (e.g. profile_inline.model
	// "opus" over a Codex spawn_profile ref). Revalidate the inline fields
	// against the effective harness resolved above so that shape is rejected
	// at save, with a message that names the actual conflict. (When the inline
	// profile's harness IS set, valHarness already picked it up — or, if the
	// agent's own harness mismatches it, the overlay drops the whole inline
	// tier at instantiate, so there is nothing to revalidate.)
	if inline != nil && inlineHarness == "" && harnessOrDefault(inline.Harness) != h.Name {
		if fail := validateInlineProfileForHarness(agentName, h, inline); fail != nil {
			return templateAgentLaunch{}, fail
		}
	}
	// Store the inline harness as typed (blank stays blank so it falls through to
	// the profile at instantiate), NOT the resolved validation harness.
	return templateAgentLaunch{
		SpawnProfile: profRef,
		Harness:      inlineHarness,
		Model:        model,
		Effort:       effort,
		Sandbox:      sandbox,
		Approval:     approval,
	}, nil
}

// validateInlineProfileForHarness re-checks a template-local profile's launch
// fields against a harness other than the one they were validated under at
// buildInlineProfileFromJSON time — the harness the instantiate-time overlay
// will actually adopt (see the call site in validateTemplateAgentLaunch). The
// per-field checks mirror buildProfileFromJSON's; blank fields stay no-ops.
func validateInlineProfileForHarness(agentName string, h *harness.Harness, p *db.SpawnProfile) *spawnFailure {
	wrap := func(status int, kind, msg string) *spawnFailure {
		return &spawnFailure{status, kind, fmt.Sprintf(
			"agent %q: profile_inline: %s — the custom config has no harness of its own, so its fields "+
				"apply on the %q launch this agent resolves to; align them with that harness or set "+
				"profile_inline.harness explicitly", agentName, msg, h.Name)}
	}
	if _, err := h.Models.ValidateModel(p.Model); err != nil {
		return wrap(http.StatusBadRequest, "invalid_model", err.Error())
	}
	if _, err := h.Models.ValidateEffort(p.Effort); err != nil {
		return wrap(http.StatusBadRequest, "invalid_effort", err.Error())
	}
	if _, err := harness.ValidateHarnessBuiltinMode(h, p.Sandbox); err != nil {
		return wrap(http.StatusBadRequest, "invalid_sandbox", err.Error())
	}
	if _, err := validateSandboxImplementationForHarness(h, p.SandboxImplementation); err != nil {
		return wrap(sandboxImplementationValidationStatus(err),
			"invalid_"+sandboxImplementationField, err.Error())
	}
	if _, err := harness.ValidateApprovalPolicy(h, p.Approval); err != nil {
		return wrap(http.StatusBadRequest, "invalid_approval", err.Error())
	}
	if _, err := harness.ValidateToolGovernance(h, p.ToolGovernance); err != nil {
		return wrap(http.StatusBadRequest, "invalid_tools", err.Error())
	}
	if _, err := harness.ResolveAskTimeoutMode(h, p.AskUserQuestionTimeout); err != nil {
		return wrap(http.StatusBadRequest, "invalid_ask_user_question_timeout", err.Error())
	}
	if p.AutoReview != nil {
		if _, err := harness.ResolveAutoReview(h, *p.AutoReview); err != nil {
			return wrap(http.StatusBadRequest, "invalid_auto_review", err.Error())
		}
	}
	if p.TrustDir != nil {
		if _, err := harness.ResolveTrustDir(h, *p.TrustDir); err != nil {
			return wrap(http.StatusBadRequest, "invalid_trust_dir", err.Error())
		}
	}
	if p.RemoteControl != nil {
		if _, err := harness.ResolveRemoteControl(h, *p.RemoteControl); err != nil {
			return wrap(http.StatusBadRequest, "invalid_remote_control", err.Error())
		}
	}
	if p.PeerMessaging != nil {
		if _, err := harness.ResolvePeerMessaging(h, p.PeerMessaging); err != nil {
			return wrap(http.StatusBadRequest, "invalid_peer_messaging", err.Error())
		}
	}
	if p.AutoMemory != nil {
		if _, err := harness.ResolveAutoMemory(h, p.AutoMemory); err != nil {
			return wrap(http.StatusBadRequest, "invalid_auto_memory", err.Error())
		}
	}
	if p.SSHWorkaround != nil {
		if _, err := harness.ResolveSSHWorkaround(h, p.SSHWorkaround); err != nil {
			return wrap(http.StatusBadRequest, "invalid_ssh_workaround", err.Error())
		}
	}
	if p.CopilotAPI != nil {
		if _, err := harness.ResolveCopilotAPI(h, p.CopilotAPI); err != nil {
			return wrap(http.StatusBadRequest, "invalid_copilot_api", err.Error())
		}
	}
	if p.CodexAppServer != nil {
		if _, err := harness.ResolveCodexAppServer(h, p.CodexAppServer); err != nil {
			return wrap(http.StatusBadRequest, "invalid_codex_app_server", err.Error())
		}
	}
	if p.FastMode != nil {
		if _, err := harness.ResolveFastMode(h, p.FastMode); err != nil {
			return wrap(http.StatusBadRequest, "invalid_fast_mode", err.Error())
		}
	}
	if len(p.ContextFeatures) > 0 {
		if _, err := harness.ResolveContextFeatures(h, p.ContextFeatures); err != nil {
			return wrap(http.StatusBadRequest, "invalid_context_features", err.Error())
		}
	}
	return nil
}

// resolveTemplateAgentLaunch computes the effective launch fields for one
// instantiated template agent (JOH-239 + JOH-240). Resolution order, highest
// priority first:
//
//	per-agent inline override → per-agent spawn profile →
//	  group default spawn profile → global default profile → harness secure default
//
// The last two tiers were missing until TCL-1110, on the strength of a comment
// here that said "a freshly-instantiated group carries no default profile". A
// deploy into an EXISTING group is the ordinary case and that group can very
// well have one, so the deploy launched on `claude` where a direct spawn into
// the same group launched on the profile's harness. The two default tiers are
// consulted HERE, for the harness only, because the harness gates every other
// field's validation: applyDefaultProfile fills the rest afterwards from the
// same two profiles, but by then the harness is already chosen and a value it
// skipped as foreign cannot be reconsidered.
//
// As with direct spawn, the harness resolves through the complete chain first.
// Each remaining field then resolves independently: inline agent values are
// explicit and fail loudly, while profile-like tiers validate against the chosen
// harness and a foreign invalid value is skipped with a disclosure note. g is
// the group being deployed into (nil = none, e.g. a pure resolution test). caller is the spawning
// agent's conv-id (empty for a human): it lets a genuinely defaulted approval
// posture be narrowed without losing that provenance during template
// resolution, and it is the identity the two default profiles are gated
// against.
//
// cwd is the resolved instantiation cwd; it drives the Codex sandbox cwd-safety
// guard so a template can't spawn a workspace-write Codex agent at/above $HOME.
//
// Returns a typed failure (recorded per-agent by the instantiator, never fatal
// to the rest of the roster) if a referenced profile vanished or a resolved
// value is invalid for the harness. The returned Harness is the resolved
// canonical name (e.g. "claude"); SpawnProfile is left empty (already consumed).
func resolveTemplateAgentLaunch(g *db.AgentGroup, a db.GroupTemplateAgent, _ *db.Role, cwd, caller string) (templateAgentLaunch, *spawnFailure) {
	var refProfile *db.SpawnProfile
	if ref := strings.TrimSpace(a.SpawnProfile); ref != "" || a.SpawnProfileID > 0 {
		var prof *db.SpawnProfile
		var err error
		if a.SpawnProfileID > 0 {
			prof, err = db.GetSpawnProfileByID(a.SpawnProfileID)
		} else {
			prof, err = db.ResolveSpawnProfile(ref)
		}
		if err != nil {
			return templateAgentLaunch{}, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if prof == nil {
			return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_profile",
				fmt.Sprintf("references spawn profile %q which no longer exists", ref)}
		}
		if fail := profileSpawnFailure(prof, caller); fail != nil {
			return templateAgentLaunch{}, fail
		}
		refProfile = prof
	}
	inlineProfile := a.ProfileInline
	if inlineProfile != nil && strings.TrimSpace(inlineProfile.Name) == "" {
		copy := *inlineProfile
		copy.Name = "profile_inline"
		inlineProfile = &copy
	}
	tiers := []launchProfileTier{
		{profile: inlineProfile, source: `template profile_inline`},
		{profile: refProfile, source: profileSource(refProfile, agent.ProvCLIProfileSource)},
	}

	// The two ambient tiers, in the same order and with the same wording the
	// direct spawn boundary uses (TCL-1110). They are NOT appended to `tiers`
	// above, because this ticket is the harness and only the harness: nothing
	// downstream can undo a per-field validation run against the wrong one, so
	// that field cannot wait for the spawn boundary the way others can.
	//
	// Do NOT read that as "the other fields are handled at the boundary". They are
	// not, and this is measured rather than assumed (TCL-1107): a differential
	// between one launch through handleGroupSpawn and one through the executeSpawn
	// safety net — same group, same default profile, neither pinning anything —
	// disagreed about SEVEN fields. TCL-1107 closed remote_control,
	// auto_memory, peer_messaging, include_group_context and context_features:
	// they resolve over these default tiers too, below. TCL-1119 (launch
	// slice 5) closed the containment fields: sandbox mode and implementation,
	// approval, tool governance and the SSH workaround resolve over the full
	// stack before the harness default applies, as on a direct spawn.
	//
	// Gated up front on BOTH profiles rather than only the one that wins, so a
	// deploy blocked by a profile the caller may not use fails with that reason
	// instead of with whatever the other profile's harness happens to make
	// invalid. applyDefaultProfile runs the same gate again later; this one only
	// moves the message earlier.
	groupProfile := groupDefaultProfile(g)
	globalProfile := globalDefaultProfile()
	for _, prof := range []*db.SpawnProfile{groupProfile, globalProfile} {
		if fail := profileSpawnFailure(prof, caller); fail != nil {
			return templateAgentLaunch{}, fail
		}
	}
	defaultTiers := []launchProfileTier{
		{profile: groupProfile, source: profileSource(groupProfile, agent.ProvGroupProfileSource),
			defaultTier: true},
		{profile: globalProfile, source: profileSource(globalProfile, agent.ProvGlobalProfileSource),
			defaultTier: true},
	}

	// Resolve harness independently. Partial inline-profile tiers only pin
	// a harness when they name one; registry profiles pin their default harness
	// even when the stored value is blank, matching direct spawn.
	// harnessSource tracks the tier alongside the walk rather than being
	// reconstructed afterwards: the walk short-circuits, so after it runs the
	// value alone can no longer say which tier produced it.
	harnessSource := ""
	harnessName := strings.TrimSpace(a.Harness)
	if harnessName != "" {
		harnessSource = agent.ProvExplicit
	}
	if harnessName == "" && inlineProfile != nil {
		harnessName = strings.TrimSpace(inlineProfile.Harness)
		if harnessName != "" {
			harnessSource = tiers[0].source
		}
	}
	if harnessName == "" && refProfile != nil {
		harnessName = harnessOrDefault(refProfile.Harness)
		harnessSource = tiers[1].source
	}
	// The ambient tiers, last. Like the registry-profile tiers above, a default
	// profile pins its harness even when the stored value is blank — that is
	// harnessOrDefault, and it is what makes "the group defaults to claude" a
	// decision the group made rather than an absence.
	for _, tier := range defaultTiers {
		if harnessName != "" {
			break
		}
		if tier.profile == nil {
			continue
		}
		harnessName = harnessOrDefault(tier.profile.Harness)
		harnessSource = tier.source
	}
	if harnessName == "" {
		harnessSource = agent.ProvHarnessDefault
	}

	h, err := resolveSpawnHarness(harnessName)
	if err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_harness", err.Error()}
	}
	var fail *spawnFailure
	var notes []string
	// From here on the harness is DECIDED, so every failure below is a failure
	// judged against a harness that some tier chose — and after TCL-1110 that tier
	// can be the group's or the workspace's default profile, which nobody typed at
	// this deploy. `"opus" is a Claude Code model` is an unreadable complaint about
	// a template whose author never mentioned Codex; the same sentence next to
	// `harness: codex (group default profile "codex-high")` is a fixable one.
	//
	// So a failure raised past this point carries the harness attribution out with
	// it instead of the zero launch, and the wave runner renders it (cold review of
	// TCL-1110). Failures raised ABOVE — a profile that vanished, a profile the
	// caller may not use — deliberately keep returning the zero value: no harness
	// has been chosen yet there, and inventing one would name a tier that did not
	// decide anything.
	failed := func(f *spawnFailure) (templateAgentLaunch, *spawnFailure) {
		return templateAgentLaunch{
			Harness:       h.Name,
			HarnessSource: harnessSource,
			Notes:         append([]string(nil), notes...),
		}, f
	}
	// The string/int launch value fields ride the same table as the direct
	// spawn and the executeSpawn safety net (launch_value_fields.go). A
	// template agent can only state model, effort, sandbox and approval; the
	// rest come from the tiers alone.
	// They walk the template's own tiers and then the group and global default
	// profiles, the stack a direct spawn walks, so the harness defaults below
	// apply only where no tier spoke.
	allTiers := append(append([]launchProfileTier(nil), tiers...), defaultTiers...)
	launchValues := launchValueResolution{Values: launchValueFields{
		Model: a.Model, Effort: a.Effort, Sandbox: a.Sandbox, Approval: a.Approval,
	}}
	if fail := resolveLaunchValueFields(h, allTiers, &launchValues, launchValueOptions{}); fail != nil {
		notes = append(notes, launchValues.Notes()...)
		return failed(fail)
	}
	notes = append(notes, launchValues.Notes()...)
	resolvedValues := launchValues.Values
	modelProxy, model, effort := resolvedValues.ModelProxy, resolvedValues.Model, resolvedValues.Effort
	sandbox, approval, toolGovernance := resolvedValues.Sandbox, resolvedValues.Approval, resolvedValues.Tools
	askTimeout, autoCompactWindow := resolvedValues.AskUserQuestionTimeout, resolvedValues.AutoCompactWindow
	contextWindowMax := resolvedValues.ContextWindowMax
	modelSource, effortSource := launchValues.Source[launchValueModel], launchValues.Source[launchValueEffort]
	sandboxSource, approvalSource := launchValues.Source[launchValueSandbox], launchValues.Source[launchValueApproval]
	contextWindowMaxSource := launchValues.Source[launchValueContextWindowMax]
	// The sandbox IMPLEMENTATION rides the same tier stack. Only the harness gate
	// runs here; the host-capability gate belongs to the launch, which the deploy
	// reaches by handing this value to the spawn boundary as an explicit request.
	sandboxImplementation, sandboxImplementationSource, note, fail := resolveSandboxImplementationLaunchField(
		h, "", allTiers)
	if fail != nil {
		return failed(fail)
	}
	if note != "" {
		notes = append(notes, note)
	}

	if sandbox, err = harness.ResolveHarnessBuiltinMode(h, sandbox); err != nil {
		return failed(&spawnFailure{http.StatusBadRequest, "invalid_sandbox", err.Error()})
	}
	if approval, err = harness.ResolveApprovalPolicy(h, approval); err != nil {
		return failed(&spawnFailure{http.StatusBadRequest, "invalid_approval", err.Error()})
	}
	if toolGovernance, err = harness.ResolveToolGovernance(h, toolGovernance); err != nil {
		return failed(&spawnFailure{http.StatusBadRequest, "invalid_tools", err.Error()})
	}
	// Preserve the semantic difference between an omitted approval and a posture
	// chosen by the template/profile chain. Before TCL-585 this resolver applied
	// the harness default early, so executeSpawn saw a non-empty value and could
	// no longer narrow bare template/wave children to an agent caller's posture.
	// Explicit/profile choices remain untouched and still fail loudly if they
	// exceed the caller's lineage authority.
	if approvalSource == agent.ProvHarnessDefault {
		defaultApproval := approval
		approval = narrowDefaultApprovalToCaller(caller, h.Name, approval)
		if approval != defaultApproval {
			notes = append(notes, callerNarrowedApprovalNote(approval, defaultApproval))
		}
	}
	// Same spawn-posture check the HTTP spawn boundary runs, at the equivalent
	// point: both axes final. Covers the Claude TCL-586 pairing and OpenCode's
	// look-like-a-sandbox access-control mode.
	//
	// Kept in their OWN channels since TCL-1097: the deploy result now carries a
	// full launch echo, which has the Info and Warnings lists the direct spawn
	// path has always had. They used to be flattened into Notes because there was
	// nowhere else to put them, and the renderer this ticket added would then have
	// printed "unattended agent, no OS sandbox" under a `note:` label — a warning
	// about the agent's blast radius, styled as one more provenance footnote.
	info := harness.SpawnSandboxInfo(h, sandbox)
	warnings := harness.SpawnSandboxWarnings(h, approval, sandbox, cwd,
		spawnUsesTclaudeLayer(sandboxImplementation))
	// Resolve the two *bool launch toggles against the chosen harness — the
	// same gate handleGroupSpawn/applyDefaultProfile apply. nil (no profile
	// spoke) collapses to false = off. ResolveTrustDir/ResolveAutoReview reject
	// a true request on a harness that has no such concept (auto-review off
	// Codex; trust-dir off a harness with no trust dialog); in practice a
	// profile carrying either is validated at save and only adopted above when
	// the harness matched, so this won't fire — but a mismatch is a clean
	// per-agent 400, not a silent drop.
	// The shared bool fields ride launchBoolFieldSpecs; a template has no
	// explicit spelling for any of them, so the tiers alone decide.
	var launchBools launchBoolResolution
	boolNotes, fail := resolveLaunchBoolFields(h, tiers, &launchBools, launchBoolTrustDir, launchBoolAutoReview)
	notes = append(notes, boolNotes...)
	if fail != nil {
		return failed(fail)
	}
	trustDir, trustDirSet := launchBools.Value[launchBoolTrustDir], launchBools.Set[launchBoolTrustDir]
	autoReview, autoReviewSet := launchBools.Value[launchBoolAutoReview], launchBools.Set[launchBoolAutoReview]
	// remote_control, auto_memory, peer_messaging and include_group_context
	// resolve over the FULL stack — the template's own tiers, then the group's
	// and the workspace's default profiles — with the group's remote-control
	// policy above the profiles, through the resolver every spawn path shares
	// (TCL-1107). Before it, only the template tiers spoke here, and the
	// group policy never did. A template has no explicit spelling for any of
	// them. Unset everywhere keeps the old answers: the three toggles off, the
	// group context included.
	toggleTiers := allTiers
	var groupRemoteControl *bool
	if g != nil {
		groupRemoteControl = g.RemoteControl
	}
	remoteControl, note, fail := resolveRemoteControlLaunch(h, toggleTiers, groupRemoteControl, nil)
	if note != "" {
		notes = append(notes, note)
	}
	if fail != nil {
		return failed(fail)
	}
	boolNotes, fail = resolveLaunchBoolFields(h, toggleTiers, &launchBools,
		launchBoolAutoMemory, launchBoolPeerMessaging, launchBoolIncludeGroupContext)
	notes = append(notes, boolNotes...)
	if fail != nil {
		return failed(fail)
	}
	autoMemory := launchBools.Value[launchBoolAutoMemory]
	peerMessaging := launchBools.Value[launchBoolPeerMessaging]
	includeGroupContext := true
	if launchBools.Set[launchBoolIncludeGroupContext] {
		includeGroupContext = launchBools.Value[launchBoolIncludeGroupContext]
		notes = append(notes, fmt.Sprintf("%s include_group_context = %v",
			launchBools.Source[launchBoolIncludeGroupContext], includeGroupContext))
	}
	// The Copilot drive rides the same pattern, with the same default: unset
	// resolves to off, i.e. a template-deployed Copilot agent stays on send-keys
	// unless a profile tier explicitly asked for the API. There is no
	// template-LOCAL spelling — a template selects the drive by referencing a
	// Copilot spawn profile that carries one. See TCL-1053.
	boolNotes, fail = resolveLaunchBoolFields(h, tiers, &launchBools,
		launchBoolCopilotAPI, launchBoolCodexAppServer, launchBoolFastMode)
	notes = append(notes, boolNotes...)
	if fail != nil {
		return failed(fail)
	}
	// The SSH note is reported after the eligibility note below, as before.
	sshNotes, fail := resolveLaunchBoolFields(h, allTiers, &launchBools, launchBoolSSHWorkaround)
	if fail != nil {
		return failed(fail)
	}
	copilotAPI, copilotAPISet, copilotAPISource := launchBools.Value[launchBoolCopilotAPI], launchBools.Set[launchBoolCopilotAPI], launchBools.Source[launchBoolCopilotAPI]
	codexAppServer, codexAppServerSet, codexAppServerSource := launchBools.Value[launchBoolCodexAppServer], launchBools.Set[launchBoolCodexAppServer], launchBools.Source[launchBoolCodexAppServer]
	fastMode, fastModeSet, fastModeSource := launchBools.Value[launchBoolFastMode], launchBools.Set[launchBoolFastMode], launchBools.Source[launchBoolFastMode]
	sshWorkaround, sshWorkaroundSet, sshWorkaroundSource := launchBools.Value[launchBoolSSHWorkaround], launchBools.Set[launchBoolSSHWorkaround], launchBools.Source[launchBoolSSHWorkaround]
	if !sshWorkaroundSet {
		sshWorkaround, _ = harness.ResolveSSHWorkaround(h, nil)
		sshWorkaroundSet = true
		// The Set bit above now says "this launch has a posture"; the ATTRIBUTION
		// has to keep saying who decided it, which was nobody. Leaving it blank
		// would let the spawn boundary re-resolve it as "explicit" — the template
		// path passes the Set bit through, and an already-set field reads as an
		// explicit request — stamping a false decision onto a pure default.
		sshWorkaroundSource = agent.ProvHarnessDefault
	}
	if !sshWorkaroundImplementationEligible(h.Name, sandbox, sandboxImplementation) {
		if sshWorkaround {
			notes = append(notes,
				"SSH workaround disabled because this launch cannot use a supported ownership-translating sandbox")
		}
		sshWorkaround = false
	}
	notes = append(notes, sshNotes...)
	// Startup-context trims: no explicit per-deploy request exists, so the tiers
	// alone decide — the template's own, then the group and global defaults, as
	// on every other path. Resolved whole rather than merged, like every other
	// consumer.
	contextFeatures, cfNote, fail := resolveContextFeaturesLaunchField(nil, false, h, toggleTiers)
	if fail != nil {
		return failed(fail)
	}
	if cfNote != "" {
		notes = append(notes, cfNote)
	}
	startupContext, ctxNote := resolveProfileStartupContext(h.Name, tiers)
	if ctxNote != "" {
		notes = append(notes, ctxNote)
	}
	environment, environmentOverrides, envErr := resolveCommonLaunchEnvironmentParts(g, allTiers, nil)
	if envErr != nil {
		return failed(&spawnFailure{http.StatusBadRequest, "invalid_environment", envErr.Error()})
	}
	// Codex sandbox cwd-safety: a writable Codex sandbox confines writes to the
	// cwd subtree, so a cwd at/above $HOME would expose ~/.tclaude / ~/.codex /
	// ~/.claude. Refuse per-agent here, mirroring handleGroupSpawn's guard.
	if home, herr := os.UserHomeDir(); herr == nil && harness.CodexSandboxCwdConflict(sandbox, cwd, home) {
		return failed(&spawnFailure{http.StatusBadRequest, "invalid_cwd", fmt.Sprintf(
			"refusing to spawn a %s agent in %q under sandbox %q: it would expose "+
				"~/.tclaude / ~/.codex / ~/.claude to the agent's writes", h.Name, cwd, sandbox)})
	}

	return templateAgentLaunch{
		Environment:            environment,
		EnvironmentOverrides:   environmentOverrides,
		Harness:                h.Name,
		Model:                  model,
		ModelProxy:             modelProxy,
		Effort:                 effort,
		Sandbox:                sandbox,
		SandboxSource:          sandboxSource,
		Approval:               approval,
		ToolGovernance:         toolGovernance,
		TrustDir:               trustDir,
		TrustDirSet:            trustDirSet,
		AutoReview:             autoReview,
		AutoReviewSet:          autoReviewSet,
		RemoteControl:          remoteControl,
		AutoMemory:             autoMemory,
		PeerMessaging:          peerMessaging,
		IncludeGroupContext:    includeGroupContext,
		UsageSpawnProfile:      refProfile,
		DefaultProfiles:        &pinnedDefaultProfiles{group: groupProfile, global: globalProfile},
		SpawnCopyTiers:         spawnCopyTiers(spawnCopyTier(db.SpawnCopyTierInline, inlineProfile), spawnCopyTier(db.SpawnCopyTierNamed, refProfile)),
		RelaunchOverrides:      resolveRelaunchOverrides(nil, []*db.SpawnProfile{refProfile, groupProfile, globalProfile}),
		SSHWorkaround:          sshWorkaround,
		SSHWorkaroundSet:       sshWorkaroundSet,
		ContextFeatures:        contextFeatures,
		StartupContext:         startupContext,
		AskUserQuestionTimeout: askTimeout,
		AutoCompactWindow:      autoCompactWindow,
		ContextWindowMax:       contextWindowMax,
		CopilotAPI:             copilotAPI,
		CopilotAPISet:          copilotAPISet,
		CopilotAPISource:       copilotAPISource,
		CodexAppServer:         codexAppServer,
		CodexAppServerSet:      codexAppServerSet,
		CodexAppServerSource:   codexAppServerSource,
		SSHWorkaroundSource:    sshWorkaroundSource,
		// The template tiers this resolver walked. The spawn boundary re-resolves
		// these fields against the group/global defaults and keeps whichever
		// attribution actually names a decider (preferResolvedSource), so seeding
		// them here is what stops a template-pinned value being reported as
		// "explicit" by the re-resolution that finds its Set bit already raised.
		HarnessSource:               harnessSource,
		ModelSource:                 modelSource,
		EffortSource:                effortSource,
		ContextWindowMaxSource:      contextWindowMaxSource,
		FastModeSource:              fastModeSource,
		SandboxImplementationSource: sandboxImplementationSource,
		FastMode:                    fastMode,
		FastModeSet:                 fastModeSet,
		SandboxImplementation:       sandboxImplementation,
		Notes:                       notes,
		Info:                        info,
		Warnings:                    warnings,
		PromptRefs: resolvePromptRefsLaunchField(agent.SpawnRequest{}, g,
			append(append([]launchProfileTier(nil), tiers...), defaultTiers...)),
	}, nil
}

// launchValueWasChosen reports whether a durable launch value carries the mark
// of a DECISION rather than of a resolver landing somewhere (TCL-1090).
//
// The rule is deliberately asymmetric: only a POSITIVE record that the harness
// default decided the value counts as "nobody chose it". A nil source is a
// legacy record written before the attribution existed, and it means unknown —
// never "unchosen".
//
// Getting that direction wrong is not a wash. Reading unknown as unchosen would
// drop a genuine, curated send-keys opt-out made before this field shipped, and
// a group default saying true would then move that agent onto the API drive the
// operator had deliberately kept it off. Reading it as chosen merely leaves the
// pre-existing wart in place for old records. One of those errors is a silent
// acquisition of an unverified drive; the other is the status quo.
//
// The EMPTY string is unknown too, and deliberately so — it is not the same as
// ProvHarnessDefault. relaunchProfileForSpawn always writes a non-nil pointer, so
// a launch path that never threaded a source records "" rather than nil; and
// HarnessBuiltinModeSource, the field this one mirrors, uses "" to mean "erase
// the previous attribution". Both spellings mean "nothing here says who decided",
// which is exactly the case that must stay conservative. Only a POSITIVE
// "the harness default decided this" suppresses the value.
func launchValueWasChosen(source *string) bool {
	if source == nil {
		return true
	}
	return strings.TrimSpace(*source) != agent.ProvHarnessDefault
}

// copilotAPIPointer renders a traced launch's Copilot drive as the tri-state a
// snapshot's inline profile carries: nil when nothing recorded a posture, so
// the snapshot stays silent on the axis rather than asserting send-keys for an
// agent that predates the field.
func copilotAPIPointer(launch templateAgentLaunch) *bool {
	if !launch.CopilotAPISet {
		return nil
	}
	value := launch.CopilotAPI
	return &value
}

func codexAppServerPointer(launch templateAgentLaunch) *bool {
	if !launch.CodexAppServerSet {
		return nil
	}
	value := launch.CodexAppServer
	return &value
}

func fastModePointer(launch templateAgentLaunch) *bool {
	if !launch.FastModeSet {
		return nil
	}
	value := launch.FastMode
	return &value
}

// traceMemberLaunch re-traces a live group member's OBSERVABLE launch fields
// from its most-recent session row for a from-group template snapshot (JOH-239)
// — harness, model, effort, sandbox, approval, and auto-review. Each field is
// normalized through the traced harness's catalog
// and dropped to "" if it doesn't validate (e.g. the session's model DISPLAY
// alias rather than the resume-safe model_id), so a snapshot never stores a
// value that would fail at the next instantiate. A member with no session row
// (pruned) or no observable value yields all-blank — "inherit the group
// default", the pre-JOH-239 behaviour.
func traceMemberLaunch(convID string) templateAgentLaunch {
	prof, err := db.SessionLaunchProfileForConv(convID)
	if err != nil || prof == (db.SessionLaunchProfile{}) {
		return templateAgentLaunch{}
	}
	h, err := harness.ResolveSpawnable(prof.Harness)
	if err != nil {
		return templateAgentLaunch{}
	}
	// Past both early returns: this member WAS observed. Everything below may
	// still come back blank — that is a finding about the member, not a failure
	// to look, and Traced is what lets the re-snapshot merge tell those apart.
	out := templateAgentLaunch{Traced: true}
	if prof.ModelProxySet {
		out.ModelProxy = prof.ModelProxy
		if out.ModelProxy == "" {
			out.ModelProxy = "off"
		}
	}
	// Store the harness only when it differs from the default, so a plain Claude
	// member round-trips to a blank (inherit) harness rather than a noisy
	// explicit "claude" on every agent.
	if harnessOrDefault(prof.Harness) != harness.DefaultName {
		out.Harness = h.Name
	}
	if m, err := h.Models.ValidateModel(prof.ModelID); err == nil {
		out.Model = m
	}
	if e, err := h.Models.ValidateEffort(prof.Effort); err == nil {
		out.Effort = e
	}
	if s, err := harness.ValidateHarnessBuiltinMode(h, prof.HarnessBuiltinMode); err == nil {
		out.Sandbox = s
	}
	if a, err := harness.ValidateApprovalPolicy(h, prof.ApprovalPolicy); err == nil && a != "" {
		out.Approval = a
		if ar, err := harness.ResolveAutoReview(h, prof.ApprovalAutoReview); err == nil {
			out.AutoReview = ar
			out.AutoReviewSet = true
		}
	}
	// The startup-context trims this member actually launched with. Observable
	// (recorded on the session row / durable relaunch profile), so a re-snapshot
	// captures a lean member as lean instead of quietly widening the template.
	if features, err := db.ContextFeaturesForConv(convID); err == nil {
		if resolved, err := harness.ResolveContextFeatures(h, features); err == nil {
			out.ContextFeatures = resolved
			// Observed — even when the answer is "trims nothing". That is what lets
			// a re-snapshot record an un-trimmed member as un-trimmed instead of
			// falling back to the template's previous trims forever.
			out.ContextFeaturesSet = true
		}
	}
	// Likewise the auto-compaction window this member actually launched with, so a
	// re-snapshot of a group whose agents compact early records that instead of
	// quietly handing every future deploy the model's full window.
	if window, err := db.AutoCompactWindowForConv(convID); err == nil {
		if resolved, err := harness.ResolveAutoCompactWindow(h, window); err == nil {
			out.AutoCompactWindow = resolved
		}
	}
	// The sandbox implementation this member launched under, recorded ONLY when
	// it differs from the default — the same rule the harness uses a few lines
	// above, and for the same reason. Every session row carries an explicit
	// harness-builtin (that is what migrate v160 backfilled), so tracing it
	// verbatim would stamp an explicit PIN into every from-group snapshot and
	// quietly convert templates that previously fell through into templates that
	// override their lower tiers.
	if resolved, err := validateSandboxImplementationForHarness(h, prof.SandboxImplementation); err == nil &&
		resolved != string(sandboxpolicy.ImplementationHarnessBuiltin) {
		out.SandboxImplementation = resolved
	}
	if durable, err := db.AgentRelaunchProfileForConv(convID); err == nil &&
		durable != nil && durable.SSHWorkaround != nil {
		if resolved, err := harness.ResolveSSHWorkaround(h, durable.SSHWorkaround); err == nil {
			if launchValueWasChosen(durable.SSHWorkaroundSource) {
				out.SSHWorkaround = resolved
				out.SSHWorkaroundSet = true
			} else {
				out.ObservedNotChosen = append(out.ObservedNotChosen, "ssh_workaround")
			}
		}
	}
	if durable, err := db.AgentRelaunchProfileForConv(convID); err == nil && durable != nil {
		if durable.ConfiguredContextWindowMax != nil {
			if value, err := harness.ResolveCopilotContextWindow(h, *durable.ConfiguredContextWindowMax); err == nil {
				out.ContextWindowMax = value
			}
		}
		if durable.CopilotAPI != nil {
			if value, err := harness.ResolveCopilotAPI(h, durable.CopilotAPI); err == nil {
				// A template is a SPECIFICATION for future launches, not a recording
				// of history. An un-chosen value has no business becoming a spec
				// line: it would assert a decision nobody made, durably and on every
				// later deploy, and it outranks the group and global tiers when it
				// does. The observation is still worth surfacing — as a disclosure,
				// which is where facts go — rather than as a pin.
				if launchValueWasChosen(durable.CopilotAPISource) {
					out.CopilotAPI = value
					out.CopilotAPISet = true
				} else {
					out.ObservedNotChosen = append(out.ObservedNotChosen, "copilot_api")
				}
			}
		}
		if durable.CodexAppServer != nil {
			if value, err := harness.ResolveCodexAppServer(h, durable.CodexAppServer); err == nil {
				if launchValueWasChosen(durable.CodexAppServerSource) {
					out.CodexAppServer = value
					out.CodexAppServerSet = true
				} else {
					out.ObservedNotChosen = append(out.ObservedNotChosen, "codex_app_server")
				}
			}
		}
		if durable.FastMode != nil {
			if _, err := harness.ResolveFastMode(h, durable.FastMode); err == nil {
				out.FastMode = *durable.FastMode
				out.FastModeSet = true
			}
		}
	}
	return out
}
