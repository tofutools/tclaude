package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/managedinstructions"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// handleGroupSpawn starts a fresh CC session and registers it in
// the group as soon as its conv-id materialises.
//
// Flow:
//  1. Pick a unique label (used as the tclaude session ID + tmux
//     session name).
//  2. Fork-exec `tclaude session new -d --global --label <label>`
//     fully detached. The wrapper exits in milliseconds; the actual
//     CC process is parented to the long-running tmux server, so
//     CC's process-ownership checks see no Claude ancestor in the
//     daemon's chain.
//  3. Poll the sessions table for that label until conv-id appears
//     (CC's first hook callback writes it). 30s default timeout.
//  4. Add the conv to the group with the supplied role/descr; the
//     `name` (when set) becomes the new agent's conversation title
//     via the post-spawn /rename injection.
//
// normalizeSpawnPermissionOverrides validates the birth-time permission
// overrides off a SpawnRequest and returns the canonical slug→override map to
// apply at enrollment. Each slug must be registered and each effect
// must be "grant" or "deny"; a "default"/"" effect is a no-op and is dropped
// (the agent inherits the global default for that slug), so an editor that
// posts every slug — most at Default — collapses to just the real overrides.
// An unknown slug or an unrecognised effect returns a non-empty human-readable
// error string (the caller maps it to a 400); the map is nil for no overrides.
//
// This is also the scope boundary for every birth-time grant: an optional
// scope is parsed, checked against the dimensions its slug declares, and
// stored CANONICALIZED, so nothing below this line has to re-validate a scope
// and the stored bytes compare equal to a gate-side canonical form. A deny
// carries no scope by design (a deny is unconditional).
func normalizeSpawnPermissionOverrides(in map[string]db.PermissionOverride) (map[string]db.PermissionOverride, string) {
	if len(in) == 0 {
		return nil, ""
	}
	out := make(map[string]db.PermissionOverride, len(in))
	for slug, override := range in {
		slug = strings.TrimSpace(slug)
		if slug == "" {
			continue
		}
		effect := strings.TrimSpace(override.Effect)
		switch effect {
		case "", "default":
			continue // no override — inherits the global default
		case db.PermEffectGrant, db.PermEffectDeny:
			if !IsKnownPermSlug(slug) {
				return nil, fmt.Sprintf("unknown permission slug %q. Known slugs: %s.",
					slug, strings.Join(knownSlugs(), ", "))
			}
		default:
			return nil, fmt.Sprintf("permission override for %q must be \"grant\", \"deny\", or \"default\"; got %q",
				slug, override.Effect)
		}
		scope := strings.TrimSpace(override.Scope)
		if scope != "" && effect == db.PermEffectDeny {
			return nil, fmt.Sprintf("permission override for %q is a deny and cannot carry a scope", slug)
		}
		canonical, err := canonicalPermissionScopeForSlug(slug, scope)
		if err != nil {
			return nil, fmt.Sprintf("permission override for %q: %v", slug, err)
		}
		out[slug] = db.PermissionOverride{Effect: effect, Scope: canonical}
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, ""
}

// Permission: groups.members.spawn (default human-only — this lets an agent
// run arbitrary CC instances on the human's machine, blast radius
// matches `agent.spawn` in the design doc).
// spawnAuditResolution is the complete, user-facing shape that reached the
// shared spawn core. It deliberately excludes write-proof tokens and other
// daemon-only capabilities, while retaining every launch/enrollment choice,
// the profile rows that participated in resolution, and the resolved
// provenance echo. Profile briefings are omitted for the same reason the raw
// request briefing is redacted: audit detail is not a prompt/content store.
// The raw request and HTTP response are captured alongside it by the audit
// middleware.
func spawnAuditProfileSnapshot(p *db.SpawnProfile) any {
	if p == nil {
		return nil
	}
	return map[string]any{
		"id":                            p.ID,
		"name":                          p.Name,
		"aliases":                       append([]string(nil), p.Aliases...),
		"disabled":                      p.Disabled,
		"disabled_reason":               p.DisabledReason,
		"operator_only":                 p.OperatorOnly,
		"harness":                       p.Harness,
		"model":                         p.Model,
		"effort":                        p.Effort,
		"sandbox":                       p.Sandbox,
		"sandbox_implementation":        p.SandboxImplementation,
		"approval":                      p.Approval,
		"tools":                         p.ToolGovernance,
		"ask_user_question_timeout":     p.AskUserQuestionTimeout,
		"auto_compact_window":           p.AutoCompactWindow,
		"context_window_max":            p.ContextWindowMax,
		"auto_review":                   p.AutoReview,
		"trust_dir":                     p.TrustDir,
		"remote_control":                p.RemoteControl,
		"auto_memory":                   p.AutoMemory,
		"peer_messaging":                p.PeerMessaging,
		"ssh_workaround":                p.SSHWorkaround,
		"context_features":              p.ContextFeatures,
		"agent_name":                    p.AgentName,
		"role":                          p.Role,
		"role_ref":                      p.RoleRef,
		"role_refs":                     p.RoleRefs,
		"descr":                         p.Descr,
		"initial_message":               redactedAuditText(p.InitialMessage),
		"startup_context":               redactedAuditText(p.StartupContext),
		"sync_worktree":                 p.SyncWorktree,
		"fetch_latest_worktree":         p.FetchLatestWorktree,
		"auto_focus":                    p.AutoFocus,
		"include_group_default_context": p.IncludeGroupDefaultContext,
		"is_owner":                      p.IsOwner,
		"permission_overrides":          p.PermissionOverrides,
		"created_at":                    p.CreatedAt,
		"updated_at":                    p.UpdatedAt,
	}
}

func spawnAuditResolution(p spawnParams, launch *agent.ResolvedLaunch, requestedProfile string, profiles map[string]*db.SpawnProfile) map[string]any {
	// The audit row records the override's full shape, scope included: an
	// unscoped one still renders as the bare "grant"/"deny" it always did.
	permissions := make(map[string]db.PermissionOverride, len(p.PermissionOverrides))
	for slug, override := range p.PermissionOverrides {
		permissions[slug] = override
	}
	features := make(map[string]string, len(p.ContextFeatures))
	for feature, state := range p.ContextFeatures {
		features[feature] = state
	}
	attachments := append([]string(nil), p.Attachments...)
	profileSnapshots := make(map[string]any, len(profiles))
	for name, profile := range profiles {
		profileSnapshots[name] = spawnAuditProfileSnapshot(profile)
	}
	return map[string]any{
		"launch":          launch,
		"profile_request": requestedProfile,
		"profiles":        profileSnapshots,
		"params": map[string]any{
			"name":                      p.Name,
			"role":                      p.Role,
			"descr":                     p.Descr,
			"task_ref_url":              p.TaskURL,
			"task_ref_label":            p.TaskLabel,
			"attachments":               attachments,
			"cwd":                       p.Cwd,
			"worktree_path":             p.WorktreePath,
			"worktree_branch":           p.WorktreeBranch,
			"auto_focus":                p.AutoFocus,
			"harness":                   p.Harness,
			"model":                     p.Model,
			"effort":                    p.Effort,
			"ssh_workaround":            p.SSHWorkaround,
			"sandbox":                   p.HarnessBuiltinMode,
			"sandbox_source":            p.HarnessBuiltinModeSource,
			"sandbox_implementation":    p.SandboxImplementation,
			"allow_unenforced_sandbox":  p.AllowUnenforcedSandbox,
			"approval":                  p.ApprovalPolicy,
			"tools":                     p.ToolGovernance,
			"auto_review":               p.AutoReview,
			"trust_dir":                 p.TrustDir,
			"remote_control":            p.RemoteControl,
			"auto_memory":               p.AutoMemory,
			"peer_messaging":            p.PeerMessaging,
			"context_features":          features,
			"auto_compact_window":       p.AutoCompactWindow,
			"context_window_max":        p.ContextWindowMax,
			"ask_user_question_timeout": p.AskUserQuestionTimeout,
			"reply_to_conv":             p.ReplyToConv,
			"spawned_by_conv":           p.SpawnedByConv,
			"include_group_context":     p.GroupContext != "",
			"profile_context":           redactedAuditText(p.ProfileContext),
			"is_owner":                  p.IsOwner,
			"permission_overrides":      permissions,
			"timeout":                   p.Timeout.String(),
			"async":                     p.Async,
		},
	}
}

// decodeSpawnBody decodes the spawn request, returns its original JSON, and
// puts the raw bytes BACK on
// r.Body, so a later ask-human popup still previews the request the agent
// actually sent (snapshotRequestBody has the same restore contract, for the
// same reason). Decoding has to happen before the permission gate — the gate
// needs the request's profile — and the gate is what may open the popup, so
// the body cannot simply be consumed here.
//
// On a malformed body it writes the 400 and returns false. That 400 now
// precedes the groups.members.spawn refusal for an unauthorized caller; it discloses
// nothing beyond "your JSON did not parse".
//
// "Was there a body" is decided from the bytes actually READ, not from
// ContentLength. A chunked request carries ContentLength -1, so testing it for
// <= 0 would hand the handler a zero-valued SpawnRequest and spawn with
// defaults — silently dropping the caller's name, profile and permission
// overrides, and leaving the spawn_profile gate to judge a profile the request
// never got to state. Only ContentLength == 0 is a declared empty body, and
// even that falls through to the same trimmed-length test.
func decodeSpawnBody(w http.ResponseWriter, r *http.Request, body *agent.SpawnRequest) (string, bool) {
	if r.Body == nil {
		return `{}`, true
	}
	// The popup's restore bound is the sibling limit and the natural ceiling: a
	// body this path cannot buffer is one snapshotRequestBody could not preview
	// either. Without it, ReadAll on an unbounded chunked stream is a trivial
	// pre-authorization memory sink — this runs BEFORE the groups.members.spawn gate.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxApprovalRestoreBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return "", false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if len(bytes.TrimSpace(raw)) == 0 {
		return `{}`, true
	}
	if err := json.Unmarshal(raw, body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return "", false
	}
	return string(raw), true
}

// resolvedSpawnProfileNameForScope answers "which named spawn profile will
// this request launch with", for the spawn_profile scope dimension only. It
// mirrors the profile precedence the full resolution below applies —
// request profile → group default → global default — and returns the profile's
// CANONICAL name, so a grant scoped to a profile still matches a request that
// reached it through an alias.
//
// It returns "" whenever no named profile resolves, including for a request
// naming one that does not exist. A scoped grant then finds the dimension
// undescribed and fails closed, so a scoped caller sees 403 rather than the
// ordinary invalid_profile 400 the unscoped caller still gets further down.
// That ordering is deliberate: the gate must not leak which profile names
// exist to a caller that is not allowed to spawn with them anyway.
func resolvedSpawnProfileNameForScope(g *db.AgentGroup, requested string) string {
	if name := strings.TrimSpace(requested); name != "" {
		prof, err := db.ResolveSpawnProfile(name)
		if err != nil || prof == nil {
			return ""
		}
		return prof.Name
	}
	for _, prof := range []*db.SpawnProfile{groupDefaultProfile(g), globalDefaultProfile()} {
		if prof != nil {
			return prof.Name
		}
	}
	return ""
}

// resolvedSandboxProfileNameForScope answers "which named sandbox profile will
// this spawn run under", for the sandbox_profile scope dimension only.
//
// An explicit request selection describes the dimension as itself. When the
// request selects nothing, the ambient assignment the launch inherits does —
// the group's, else the global one — so a grant listing the profile the
// operator already made the default admits a spawn that simply does not
// override it. Without this, a sandbox_profile-scoped grant would force every
// caller to restate the default by name, and would leave cron/trigger managed
// spawns (which never select one) unable to fire at all.
//
// Reading the most specific tier only is what the grant already means: naming
// a profile explicitly composes the global and group assignments underneath it
// too, so a grant scoped to the innermost tier has always tolerated the ones
// above it.
//
// It returns "" when nothing resolves, leaving the dimension undescribed so a
// scoped grant fails closed. Callers pair it with a post-resolution check that
// the launch mode did not omit sandbox profiles outright — see
// sandboxProfilesDisabled — because an ambient tier that gets dropped must not
// keep authorizing the spawn it no longer applies to.
// The explicit branch deliberately does NOT round-trip the name through the
// registry the way resolvedSpawnProfileNameForScope does. That sibling
// canonicalizes because spawn profiles have alternate handles — a
// spawn_profile_aliases table ResolveSpawnProfile joins — so the requested
// handle and the profile's name can legitimately differ. Sandbox profiles have
// no alias table at all: `sandbox_profiles.name` is the key, declared
// `TEXT NOT NULL UNIQUE` with no NOCASE collation, and the launch itself
// selects the explicit profile by `WHERE name = ?`. The requested string IS the
// canonical name for every name that resolves, so a lookup here could only
// change the answer for one that does not.
//
// Leaving a non-resolving name described is the better of the two readings. A
// caller whose grant does not list it is refused 403 either way, so nothing
// leaks; a caller whose grant DOES list it — an operator allowlist naming a
// profile since deleted — reaches the ordinary 400 that names the missing
// profile instead of an opaque scope refusal, which is what makes the stale
// grant diagnosable.
func resolvedSandboxProfileNameForScope(g *db.AgentGroup, requested string) string {
	if name := strings.TrimSpace(requested); name != "" {
		return name
	}
	return ambientSandboxProfileName(g)
}

// ambientSandboxProfileName is the group-then-global sandbox assignment, by
// canonical current name. Both tiers are read through their stable registry
// ids, so a renamed profile still answers as the name a grant is written
// against today.
func ambientSandboxProfileName(g *db.AgentGroup) string {
	if g != nil {
		prof, err := db.GetAgentGroupSandboxProfile(g.Name)
		if err != nil {
			slog.Warn("spawn: failed to load group sandbox profile", "group", g.Name, "error", err)
			return ""
		}
		if prof != nil {
			return prof.Name
		}
	}
	prof, err := db.GetGlobalSandboxProfile()
	if err != nil {
		slog.Warn("spawn: failed to load global sandbox profile", "error", err)
		return ""
	}
	if prof == nil {
		return ""
	}
	return prof.Name
}

func handleGroupSpawn(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	timing := config.StartupTiming("spawn_request")
	defer timing("return")
	req, fail := decodeSpawnRequest(w, r, g)
	if fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	defer func() { req.releaseExternalChild() }()
	if fail := req.authorize(w, r); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.validateLaunchOptions(w, r); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.checkGuardrails(r); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.normalizeInitialMessage(); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.normalizeName(); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.normalizeAttachments(); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.normalizePermissionOverrides(); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.resolveReplyTo(w, r); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	req.resolveTimeout()
	req.applyDefaultCwd()

	if fail := req.resolveProfiles(w, r); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.resolveLaunchTarget(r); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.resolveLaunchValues(r); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.resolveSandboxImplementation(); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.resolveLaunchBools(); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.resolveIdentity(r); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.resolveContextFeatures(); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	req.buildLaunchEcho()

	// The steps below have not moved onto spawnRequest yet; they read the
	// request through these locals.
	body := &req.body
	requestedSpawnConfigJSON, explicitLaunch := req.requestedSpawnConfigJSON, req.explicitLaunch
	externalSpawn, sandboxProfileForScope := req.externalSpawn, req.sandboxProfileForScope
	spawnerConvID, spawnSudoGrant := req.spawnerConvID, req.spawnSudoGrant
	teleportAuthority, sandboxScopePinned := req.teleportAuthority, req.sandboxScopePinned
	spawnUsageProfiles, oneShotName, attachments := req.spawnUsageProfiles, req.oneShotName, req.attachments
	replyToConv, timeout := req.replyToConv, req.timeout
	namedProfile, namedProfileHandle, groupProfile := req.namedProfile, req.namedProfileHandle, req.groupProfile
	globalProfile, relaunchTiers, relaunchExplicit := req.globalProfile, req.relaunchTiers, req.relaunchExplicit
	profileTiers, harnessSource, cwd := req.profileTiers, req.harnessSource, req.cwd
	worktreePath, worktreeBranch, h := req.worktreePath, req.worktreeBranch, req.h
	modelSource, effortSource, sandboxSource := req.modelSource, req.effortSource, req.sandboxSource
	contextWindowMaxSource, fastMode, fastModeSet := req.contextWindowMaxSource, req.fastMode, req.fastModeSet
	fastModeSource, sandboxImplSource, launchBools := req.fastModeSource, req.sandboxImplSource, req.launchBools
	autoReviewSet, sshWorkaround, sshWorkaroundSource := req.autoReviewSet, req.sshWorkaround, req.sshWorkaroundSource
	trustDirSet, autoMemory, peerMessaging := req.trustDirSet, req.autoMemory, req.peerMessaging
	copilotAPI, copilotAPISet, copilotAPISource := req.copilotAPI, req.copilotAPISet, req.copilotAPISource
	piDrive, codexAppServer, codexAppServerSet := req.piDrive, req.codexAppServer, req.codexAppServerSet
	codexAppServerSource, includeGroupContext, identity := req.codexAppServerSource, req.includeGroupContext, req.identity
	contextFeatures, profileContext, resolvedLaunch := req.contextFeatures, req.profileContext, req.resolvedLaunch
	var fieldFail *spawnFailure

	// resolveStringLaunchField already validated and normalized both values.
	effort, model := body.Effort, body.Model

	// Resolve the sandbox mode for the chosen harness: a Codex agent gets its
	// secure default (the managed tclaude-agent profile) when unset, a Claude
	// agent gets its inherit default (normalized to "" — no `--settings`
	// override), and an explicit mode is validated per-harness. Then the
	// cwd-safety guard: a writable Codex sandbox confines writes to the cwd
	// subtree, so a cwd at/above $HOME would expose ~/.tclaude / ~/.codex /
	// ~/.claude — refuse here with a clean 400 rather than after the forked
	// session times out. (Claude's `on` block protects those dirs via settings,
	// so this Codex-specific guard doesn't apply to it.)
	harnessBuiltinMode, sbErr := harness.ResolveHarnessBuiltinMode(h, body.HarnessBuiltinMode)
	if sbErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox", sbErr.Error())
		return
	}
	harnessBuiltinMode, fieldFail = resolveSandboxImplementationMode(
		h, harnessBuiltinMode, body.SandboxImplementation)
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	if h.UsesAuthoritativeServer() &&
		body.SandboxImplementation == string(sandboxpolicy.ImplementationTclaudeLayer) {
		resolvedLaunch.Notes = append(resolvedLaunch.Notes,
			clcommon.OpenCodeStatePrivateNote)
	}
	// body.ApprovalPolicy is already profile-merged above (resolveStringLaunchField
	// overlays the profile tiers without defaulting), so an empty value here means
	// NOTHING chose a posture — neither an explicit flag nor a spawn profile. Only
	// then may the harness default be narrowed to what this caller can mint; an
	// explicit or profile-set posture must fail loudly in the guard below.
	approvalUnset := strings.TrimSpace(body.ApprovalPolicy) == ""
	approvalPolicy, apErr := harness.ResolveApprovalPolicy(h, body.ApprovalPolicy)
	if apErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_approval", apErr.Error())
		return
	}
	if approvalUnset {
		defaultApprovalPolicy := approvalPolicy
		approvalPolicy = narrowDefaultApprovalToCaller(spawnerConvID, h.Name, approvalPolicy)
		if approvalPolicy != defaultApprovalPolicy {
			resolvedLaunch.Notes = append(resolvedLaunch.Notes,
				callerNarrowedApprovalNote(approvalPolicy, defaultApprovalPolicy))
		}
	}
	toolGovernance, tgErr := harness.ResolveToolGovernance(h, body.ToolGovernance)
	if tgErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_tools", tgErr.Error())
		return
	}
	// Both axes are final here (profile overlay, harness default, caller
	// narrowing), which is the only point where "autonomous but unconfined" is
	// a true statement about the launch that is about to happen. Warn, don't
	// refuse: forcing sandbox `on` would override the operator's settings.json
	// on the one axis tclaude deliberately leaves to them (TCL-586). The same
	// call also surfaces OpenCode's toothless access-control "sandbox".
	resolvedLaunch.Warnings = append(resolvedLaunch.Warnings,
		harness.SpawnSandboxWarnings(h, approvalPolicy, harnessBuiltinMode, cwd,
			spawnUsesTclaudeLayer(body.SandboxImplementation))...)
	resolvedLaunch.Info = append(resolvedLaunch.Info,
		harness.SpawnSandboxInfo(h, harnessBuiltinMode)...)
	autoReview, arErr := harness.ResolveAutoReview(h, body.AutoReview)
	if arErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_auto_review", arErr.Error())
		return
	}
	if body.NonInteractive {
		autoReview = false
	}
	if home, herr := os.UserHomeDir(); herr == nil && harness.CodexSandboxCwdConflict(harnessBuiltinMode, cwd, home) {
		writeError(w, http.StatusBadRequest, "invalid_cwd", fmt.Sprintf(
			"refusing to spawn a %s agent in %q under sandbox %q: it would expose "+
				"~/.tclaude / ~/.codex / ~/.claude to the agent's writes; spawn in a "+
				"project subdirectory or set sandbox %q to opt out",
			h.Name, cwd, harnessBuiltinMode, harness.SandboxDangerFull))
		return
	}

	// Sandbox lineage — the child may not launch with a looser sandbox MODE
	// than the caller currently has (spawn_sandbox_guard.go). Checked HERE,
	// before claimSpawnRateSlot below, so a refused escalation costs the
	// caller no rate slot. executeSpawn re-runs the same (idempotent) check
	// for the other spawn callers (templates/waves/process adapters) that
	// don't pass through this HTTP boundary.
	//
	// It judges the LAUNCH mode, not the harness-native one every gate above
	// judges: a tclaude-layer child records the single-wall posture, and the
	// guard has to reason about the posture that will actually be persisted and
	// later read back when this child spawns children of its own (TCL-989).
	childLaunchSandbox, fieldFail := resolveLaunchHarnessBuiltinMode(
		h, harnessBuiltinMode, body.SandboxImplementation)
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	if fail := spawnSandboxLineageFailure(
		spawnerConvID, h.Name, childLaunchSandbox, body.SandboxImplementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if fail := spawnApprovalLineageFailure(spawnerConvID, h.Name, approvalPolicy, autoReview); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}

	// danger-full-access is Codex's explicit raw no-sandbox launch. It cannot
	// carry the managed permission profile that represents tclaude filesystem
	// policy, so omit every sandbox-profile tier (global, group, and explicit)
	// instead of resolving a policy that must fail capability validation later.
	// The dashboard mirrors this by forcing its selector to the visible "none"
	// state; this server-side rule also covers CLI callers and older tabs.
	if spawnerConvID != "" && body.SandboxProfile != "" &&
		sandboxProfilesDisabled(h.Name, harnessBuiltinMode, body.SandboxImplementation) {
		writeError(w, http.StatusForbidden, "sandbox_profile_restricted",
			"the resolved launch mode omits sandbox profiles and cannot satisfy a named sandbox-profile selection")
		return
	}
	// A grant scoped to a sandbox profile authorized this spawn on the promise
	// that the child runs under that profile, so the profile has to actually
	// bind it. That is the ENFORCEMENT question, not the composition one: an
	// implementation that stands the OS boundary down still composes and
	// records the whole chain while enforcing none of its access rules, which
	// would satisfy the scope on paper and confine nothing.
	//
	// Only the scope-pinned caller is bound. An unscoped grant, an
	// ownership-derived one, or a scope over `group` alone never traded on the
	// profile, and an ambient assignment existing somewhere in the deployment
	// is not by itself a reason to refuse them.
	if sandboxScopePinned &&
		sandboxProfileTierInert(h.Name, harnessBuiltinMode, body.SandboxImplementation) {
		writeError(w, http.StatusForbidden, "sandbox_profile_restricted",
			fmt.Sprintf("this spawn is authorized by a grant scoped to sandbox profile %q, "+
				"and the resolved launch mode would not enforce it", sandboxProfileForScope))
		return
	}
	if externalSpawn && !externalStewardApprovalHolds(h.Name, childLaunchSandbox) {
		writeError(w, http.StatusForbidden, externalStewardCeilingCode,
			"this launch can run commands without asking a human: Claude's own sandbox does, and so does OpenCode's bash; an external steward's worker can't use either")
		return
	}
	// An omission the caller asked for is kept by every relaunch; one the
	// launch mode forces is re-judged when the mode changes (derived).
	effectiveSandbox := sandboxpolicy.OmittedProfilesSnapshot()
	if !body.OmitSandboxProfiles {
		effectiveSandbox = sandboxpolicy.DerivedOmittedProfilesSnapshot()
	}
	var policyErr error
	if !sandboxProfilesDisabled(h.Name, harnessBuiltinMode, body.SandboxImplementation) &&
		!body.OmitSandboxProfiles {
		effectiveSandbox, policyErr = db.ResolveEffectiveSandboxSnapshot(g.ID, body.SandboxProfile)
	}
	if errors.Is(policyErr, db.ErrSandboxProfileNotFound) {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile",
			fmt.Sprintf("sandbox profile %q does not exist", body.SandboxProfile))
		return
	}
	if policyErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", policyErr.Error())
		return
	}
	if applied, fail := applySpawnHarnessConfig(effectiveSandbox, body.HarnessConfig); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	} else {
		effectiveSandbox = applied
	}
	// A principal has no sandbox to contain the child. Its pinned profile is the
	// bound, and sandboxScopePinned above refused a launch that wouldn't enforce it.
	if spawnerConvID != "" && !externalSpawn {
		parentSnapshot, err := db.AgentEffectiveSandboxConfigForConv(spawnerConvID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", "load parent sandbox snapshot: "+err.Error())
			return
		}
		if parentSnapshot == nil {
			if sandboxpolicy.HasCapabilities(effectiveSandbox) {
				writeError(w, http.StatusForbidden, "sandbox_profile_restricted",
					"this parent predates effective sandbox snapshots and may not inherit custom capabilities; relaunch it under current policy or ask the human to spawn the child")
				return
			}
		} else {
			validatedParent, err := ensureAgentDirectoriesForRelaunch(*parentSnapshot)
			if err != nil {
				writeError(w, http.StatusConflict, "sandbox_profile_changed", err.Error())
				return
			}
			if err := sandboxpolicy.RequireContained(validatedParent, effectiveSandbox); err != nil {
				writeError(w, http.StatusForbidden, "sandbox_profile_restricted", err.Error())
				return
			}
		}
	}
	launchEnvironment, launchEnvironmentOverrides, envErr := resolveCommonLaunchEnvironmentParts(g, profileTiers, body.Environment)
	if envErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_environment", envErr.Error())
		return
	}
	if _, envErr := sandboxpolicy.MergeEnvironment(effectiveSandbox.Effective.Environment, launchEnvironment); envErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_environment", envErr.Error())
		return
	}
	effectiveSandbox.LaunchEnvironment = launchEnvironment
	effectiveSandbox.RefreshGroupEnvironment = true
	effectiveSandbox.LaunchEnvironmentOverrides = launchEnvironmentOverrides
	effectiveSandbox.ResolutionGroupID = g.ID
	if fail := sandboxProfileCapabilityFailure(
		h.Name, harnessBuiltinMode, &effectiveSandbox, body.SandboxImplementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if fail := copilotAPILoopbackFailure(
		copilotAPI, &effectiveSandbox, body.SandboxImplementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if _, fail := planSandboxProfileAccessForLaunch(
		h.Name, harnessBuiltinMode, &effectiveSandbox, body.SandboxImplementation,
		session.ModelTransportLaunchContext{
			Model:      body.Model,
			ModelProxy: body.ModelProxy,
			Cwd:        cwd,
		},
		body.AllowUnenforcedSandbox,
	); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	sshWorkaroundIntent := sshWorkaround
	if sshWorkaround && !codexSSHWorkaroundApplies(
		h.Name, harnessBuiltinMode, body.SandboxImplementation, &effectiveSandbox) {
		sshWorkaround = false
		resolvedLaunch.Notes = append(resolvedLaunch.Notes,
			"SSH workaround disabled because this launch does not use the Codex managed sandbox or tclaude's sandbox")
	}
	// Persist the resolved posture in the audit request as an explicit boolean,
	// including the default-on case and an operator's opt-out.
	body.SSHWorkaround = &sshWorkaround
	for _, notice := range effectiveSandbox.Effective.AccessNotices {
		resolvedLaunch.Warnings = append(resolvedLaunch.Warnings, notice.Detail)
	}
	if effectiveSandbox.ProfilesOmitted && sshWorkaround {
		sshWorkaround = false
		body.SSHWorkaround = &sshWorkaround
		resolvedLaunch.Notes = append(resolvedLaunch.Notes,
			"SSH workaround disabled because sandbox profiles were explicitly omitted")
	}
	resolvedLaunch.SandboxPolicy = agent.SummarizeSandboxPolicy(effectiveSandbox)

	// Dir write-proof — the launch-directory half of the spawn sandbox guard
	// (spawn_dir_proof.go). The lineage guard above caps the child's sandbox
	// MODE; this caps its anchor: an agent caller must prove its own sandbox
	// can write in every caller-selected directory the child would get write
	// access to (the launch cwd, plus the designated worktree when one is
	// passed), otherwise spawning a child there would be a write-permission escape.
	// Sandbox-profile roots are authorized by the lineage containment check
	// above. They are deliberately not write-proved: a grant permits writes to
	// existing descendants, but does not imply permission to create directory
	// entries in the grant root itself (for example /dev/dri).
	// The gate
	// challenges (403 write_proof_required) or verifies; on success it pins
	// cwd/worktree to the symlink-resolved paths the proof was verified in, so
	// a link swapped after verification cannot retarget the grant. Humans,
	// fully-open callers, and no-cwd-write children (Codex read-only) pass
	// untouched — requireDirWriteProof/childSandboxGrantsDirWrite decide.
	var proofDirs []string
	var proofToken string
	var codexGitCommonDir string
	codexGitCommonDirPinned := spawnUsesPinnedGitCommonDir(
		h.Name, harnessBuiltinMode, body.SandboxImplementation)
	var gitWorktreeWriteDirs []string
	if codexGitCommonDirPinned {
		var gerr error
		codexGitCommonDir, gerr = spawnGitCommonDir(
			h.Name, harnessBuiltinMode, body.SandboxImplementation, cwd)
		if gerr != nil {
			writeError(w, http.StatusInternalServerError, "io", gerr.Error())
			return
		}
		if home, herr := os.UserHomeDir(); herr == nil {
			gitWorktreeWriteDirs = harness.GitWorktreeWriteDirs(cwd, codexGitCommonDir, home)
		}
	}
	autoTrustSiblingWorktree, trustLayoutErr := defaultSiblingWorktreeTrust(h.Name, cwd, codexGitCommonDir)
	if trustLayoutErr != nil {
		writeError(w, http.StatusInternalServerError, "io", trustLayoutErr.Error())
		return
	}
	// A child with no cwd write (Codex read-only) normally needs no proof — it
	// receives no write access to anchor. But the caller-owned trust exemption
	// (spawn_dir_trust.go) justifies itself with a write capability: pre-trust
	// is safe there BECAUSE the caller could already write the harness config
	// that dir carries. So whenever that exemption is what would permit the
	// request, demand the proof regardless of the child's own sandbox — else a
	// read-only caller, which by construction can write nothing, would seed the
	// human's trust store off an unproven claim. The sibling-worktree exemption
	// is excluded: it is forced on precisely so a detached child cannot stall on
	// the trust dialog, and it carries its own proof pairing when the child
	// takes repository grants.
	if spawnerConvID != "" && !externalSpawn && (childSandboxGrantsDirWrite(
		h.Name, harnessBuiltinMode, body.SandboxImplementation) ||
		(body.TrustDir && !autoTrustSiblingWorktree)) {
		dirs := []string{cwd}
		dirs = appendUniqueDirs(dirs, worktreePath)
		dirs = appendUniqueDirs(dirs, gitWorktreeWriteDirs...)
		var resolved map[string]string
		var ok bool
		token := strings.TrimSpace(body.WriteProofToken)
		if queued := spawnQueueReplayFrom(r); queued != nil && len(queued.ProofDirs) > 0 {
			// Proved when the spawn was queued; the launch carries a fresh
			// daemon-minted proof over exactly those canonical directories.
			resolved, token, ok = queueDirWriteProof(w, queued, dirs)
		} else {
			resolved, ok = requireDirWriteProof(w, r, spawnerConvID, body.WriteProofToken, dirs)
		}
		if !ok {
			return
		}
		if resolved != nil {
			proofToken = token
			if v := resolved[cwd]; v != "" {
				cwd = v
			}
			if worktreePath != "" {
				if v := resolved[worktreePath]; v != "" {
					worktreePath = v
				}
			}
			if codexGitCommonDir != "" {
				if v := resolved[codexGitCommonDir]; v != "" {
					codexGitCommonDir = v
				}
			}
			for i, dir := range gitWorktreeWriteDirs {
				if v := resolved[dir]; v != "" {
					gitWorktreeWriteDirs[i] = v
				}
			}
			// Carry the verified, symlink-resolved dirs to executeSpawn, which
			// re-asserts they are still canonical immediately before the fork —
			// closing the window between verification here and the child's
			// launch (a swap after verification is caught, not launched into).
			proofDirs = make([]string, 0, len(dirs))
			for _, raw := range dirs {
				resolvedDir := raw
				if v := resolved[raw]; v != "" {
					resolvedDir = v
				}
				proofDirs = appendUniqueDirs(proofDirs, resolvedDir)
			}
		}
	}

	// Resolve the AskUserQuestion idle-timeout for the chosen harness: a
	// Claude-Code-only settings.json override (never|60s|5m|10m) delivered via
	// `--settings`, so an explicit value for a harness with no AskUserQuestion
	// dialog (Codex) is a 400 here rather than a flag silently dropped. There is
	// no forced default (inherit/blank → "" = no override) — enabling
	// auto-continue for an unattended agent is an explicit per-agent / profile
	// opt-in, already overlaid from the group default profile above.
	askTimeout, atErr := harness.ResolveAskTimeoutMode(h, body.AskUserQuestionTimeout)
	if atErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_ask_user_question_timeout", atErr.Error())
		return
	}

	// Same gate for the auto-compaction window, already normalized and overlaid
	// from the profile tiers above. Re-resolving here keeps this boundary
	// self-sufficient rather than trusting the earlier pass, exactly as the
	// ask-timeout re-resolve does.
	autoCompactWindow, acwErr := harness.ResolveAutoCompactWindow(h, body.AutoCompactWindow)
	if acwErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_auto_compact_window", acwErr.Error())
		return
	}

	// Gate the opt-in dir-trust request: it means something only for a harness
	// with a trust dialog tclaude can pre-seed (Claude Code, Codex) and, unlike
	// sandbox/approval, edits a config tclaude does not own — so requesting it
	// for a harness with no trust dialog is a 400 here rather than a flag
	// silently dropped. Off by default except for a verified tclaude-style
	// sibling worktree, which must be trusted before a detached child starts.
	// See JOH-205 inc4 / JOH-369.
	trustDir, tdErr := harness.ResolveTrustDir(h, body.TrustDir)
	if tdErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_trust_dir", tdErr.Error())
		return
	}
	if autoTrustSiblingWorktree {
		trustDir = true
	}
	if spawnerConvID != "" && trustDir && !autoTrustSiblingWorktree {
		callerOwned, ownErr := callerOwnedDirTrustProved(spawnerConvID, cwd, proofToken, proofDirs)
		if ownErr != nil {
			writeError(w, http.StatusInternalServerError, "io", ownErr.Error())
			return
		}
		if !callerOwned {
			writeError(w, http.StatusForbidden, "trust_dir_restricted", trustDirRestrictedMessage)
			return
		}
	}

	// Gate the explicit "start with remote control" opt-in: it is a Claude Code
	// feature (the --remote-control launch flag), so an EXPLICIT request for a
	// harness with no built-in Remote Access (Codex) is a 400 here rather than a
	// flag silently dropped. body.RemoteControl is tri-state (*bool): only a
	// non-nil request is validated here (the dashboard form always sends one for a
	// Remote-Access-capable harness; the CLI sets &true on opt-in). nil = caller
	// said nothing → the policy stack below fills it. See JOH-258. The
	// validation runs first inside resolveRemoteControlLaunch.
	// Layer the spawn-time policy stack (JOH-262, revised): an explicit per-spawn
	// value (the dashboard form / CLI flag) is AUTHORITATIVE — it overrides BOTH
	// the group's remote-control policy AND the group default profile's default,
	// so whatever the spawn form shows decides the spawn state. With it
	// unspecified (nil), the group policy wins, then the profile default, then off.
	// A policy-DERIVED force-on is then clamped to off for a harness with no Remote
	// Access — a group/profile default must not fail a Codex spawn (an EXPLICIT
	// opt-in for Codex already 400'd above). See resolveRemoteControlIntent.
	// Profile bools participate only when their profile harness matches the
	// resolved harness. This gate applies even to false: unlike string catalogs,
	// false validates everywhere and would otherwise shadow a matching lower
	// tier's true.
	//
	// Every tier participates, the NAMED profile included. The CLI still does not
	// fold remote_control in client-side — it cannot see the group's
	// remote-control policy, which must win — but the daemon can and does, so a
	// caller that merely names a profile gets its default under that policy
	// rather than losing it. That is what makes the toggle real for the agentd
	// TUI, which names a profile and sends no flag of its own. The same
	// resolver serves every other spawn path (TCL-1107).
	remoteControl, remoteControlSource, remoteControlNote, rcFail := resolveRemoteControlLaunchSource(h, profileTiers, g.RemoteControl, body.RemoteControl)
	if remoteControlNote != "" {
		resolvedLaunch.Notes = append(resolvedLaunch.Notes, remoteControlNote)
	}
	if rcFail != nil {
		writeError(w, rcFail.Status, rcFail.Kind, rcFail.Msg)
		return
	}
	if body.NonInteractive {
		remoteControl, remoteControlSource = false, provRemoteControlNonInteractive
	}
	resolvedLaunch.RemoteControl = resolvedToggle(remoteControl, remoteControlSource)
	resolvedLaunch.AutoMemory = resolvedToggle(autoMemory, launchBools.Source[launchBoolAutoMemory])
	resolvedLaunch.PeerMessaging = resolvedToggle(peerMessaging, launchBools.Source[launchBoolPeerMessaging])

	// Validate an explicit task-reference link at the spawn boundary so a
	// bad URL fails the request with a clear 400, rather than being
	// silently dropped when enrollSpawnedConv tries to persist it. Empty
	// means "no link". The scheme check is the same http(s) guard the
	// standalone task endpoints apply — keeps `javascript:`/`data:` URLs
	// out of the dashboard's Task-column href.
	if u := strings.TrimSpace(body.TaskURL); u != "" {
		if err := validateTaskRefURL(u); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_task_url", err.Error())
			return
		}
		if err := validateTaskRefLabel(strings.TrimSpace(body.TaskLabel)); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_task_label", err.Error())
			return
		}
	}

	// A spawn preview stops here, before the first side effect (the rate
	// limit slot below): it has resolved and validated the request exactly as
	// the spawn would, and answers with what it would launch. Nothing above
	// this line may write, allocate or audit: TestSpawnPreview_HasNoSideEffects
	// guards it.
	if isSpawnPreview(r) {
		writeJSON(w, http.StatusOK, spawnPreviewResponse{Resolved: resolvedLaunch})
		return
	}

	// Rate limit (guardrail 3) — claimed only now, after every validation
	// gate: a request refused above (bad arg, sandbox lineage, missing dir
	// write-proof / its challenge round-trip) costs no slot, while anything
	// past this point counts even if the spawn itself then fails — the
	// intended runaway-prevention behaviour.
	// Grant-authorized federation auto-spawns resolve launch settings as the
	// operator but retain the spawn rate limit under a durable daemon principal.
	//
	// Capacity comes first: a spawn that does not fit and may wait is queued
	// here, after every validation gate, and claims its rate slot when queued.
	// Its later launch from the queue claims none.
	if queueSpawnIfFull(w, r, g, spawnQueueCandidate{
		body: body, rawBody: requestedSpawnConfigJSON, spawnerConv: spawnerConvID, sudoGrant: spawnSudoGrant,
		proofToken: proofToken, proofDirs: proofDirs, cwd: cwd, attachments: attachments, externalPeer: externalSpawn,
	}) {
		return
	}
	if remotePrincipal, _ := r.Context().Value(federationSpawnRateKey{}).(string); remotePrincipal != "" {
		if fail := claimDaemonSpawnRateSlot(remotePrincipal); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
	} else if spawnQueueReplayFrom(r) == nil {
		if fail := claimSpawnRateSlot(spawnerConvID); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
	}

	// Hand the validated request to the shared spawn core. executeSpawn
	// owns the label → subprocess → conv-id poll → membership →
	// post-init sequence; the group-template instantiator drives the
	// same function in a loop. handleGroupSpawn keeps only the HTTP
	// shape — decode + validate above, error/JSON mapping below.
	p := spawnParams{
		Arrival:                    arrivalFromRequest(r),
		AgentID:                    reservedAgentIDFromContext(r.Context()),
		ClaudeSubscription:         claudeSubscriptionForSpawn(r.Context()),
		EffectiveSandbox:           &effectiveSandbox,
		Name:                       body.Name,
		Role:                       body.Role,
		Descr:                      body.Descr,
		TaskURL:                    strings.TrimSpace(body.TaskURL),
		TaskLabel:                  strings.TrimSpace(body.TaskLabel),
		InitialMessage:             body.InitialMessage,
		ProfileContext:             profileContext,
		Attachments:                attachments,
		Cwd:                        cwd,
		WorktreePath:               worktreePath,
		WorktreeBranch:             worktreeBranch,
		DirWriteProofDirs:          proofDirs,
		DirWriteProofToken:         proofToken,
		CwdWriteProofToken:         proofToken,
		CleanupDirWriteProof:       true,
		CodexGitCommonDir:          codexGitCommonDir,
		CodexGitCommonDirPinned:    codexGitCommonDirPinned,
		GitWorktreeWriteDirs:       gitWorktreeWriteDirs,
		GitWorktreeWriteDirsPinned: codexGitCommonDirPinned,
		AutoFocus:                  body.AutoFocus,
		AutoFocusWeb:               body.AutoFocusWeb,
		Effort:                     effort,
		Model:                      model,
		ModelProxy:                 body.ModelProxy,
		Harness:                    h.Name,
		// This boundary resolves a tier applyDefaultProfile cannot see — the CLI's
		// named --profile — so seeding its attributions is what keeps the launch's
		// own echo (and the drive-acquisition log built from it) naming `profile
		// "x"` rather than the "explicit" artifact a re-resolution would produce.
		HarnessSource:               harnessSource,
		ModelSource:                 modelSource,
		EffortSource:                effortSource,
		ContextWindowMaxSource:      contextWindowMaxSource,
		FastModeSource:              fastModeSource,
		SandboxImplementationSource: sandboxImplSource,
		SSHWorkaround:               sshWorkaroundIntent,
		SSHWorkaroundSet:            true,
		SSHWorkaroundSource:         sshWorkaroundSource,
		HarnessBuiltinMode:          harnessBuiltinMode,
		HarnessBuiltinModeSource:    sandboxSource,
		SandboxImplementation:       body.SandboxImplementation,
		AllowUnenforcedSandbox:      body.AllowUnenforcedSandbox,
		AskUserQuestionTimeout:      askTimeout,
		ApprovalPolicy:              approvalPolicy,
		ToolGovernance:              toolGovernance,
		AutoReview:                  autoReview,
		AutoReviewSet:               autoReviewSet,
		TrustDir:                    trustDir,
		TrustDirSet:                 trustDirSet,
		RemoteControl:               remoteControl,
		AutoMemory:                  autoMemory,
		PeerMessaging:               peerMessaging,
		launchTogglesResolved:       true, // resolved above over the full tier stack
		ContextFeatures:             contextFeatures,
		AutoCompactWindow:           autoCompactWindow,
		ContextWindowMax:            body.ContextWindowMax,
		CopilotAPI:                  copilotAPI,
		CopilotAPISet:               copilotAPISet,
		CopilotAPISource:            copilotAPISource,
		PiDrive:                     piDrive,
		CodexAppServer:              codexAppServer,
		CodexAppServerSet:           codexAppServerSet,
		CodexAppServerSource:        codexAppServerSource,
		FastMode:                    fastMode,
		FastModeSet:                 fastModeSet,
		ReplyToConv:                 replyToConv,
		SpawnedByConv:               spawnerConvID,
		externalStewardGated:        externalSpawn,
		IsOwner:                     identity.isOwner,
		PermissionOverrides:         identity.overrides,
		PermissionSources:           identity.overrideSources,
		Timeout:                     timeout,
		// Verbatim decoded request captured at the HTTP boundary, before the
		// working body was resolved or normalized. enrollSpawnedConv persists it
		// as the durable "what did the caller request" record.
		SpawnConfigJSON: requestedSpawnConfigJSON,
		// The HTTP spawn endpoint (dashboard + `tclaude agent spawn`) is
		// non-blocking: a spawn whose conv-id does not materialise within the
		// inline grace becomes a PENDING agent rather than hanging the request
		// — the JOH-205 spawn-freeze fix. The group-template instantiator
		// builds its own params and leaves this false, so it stays synchronous
		// (it needs the conv-id for owner/permission grants).
		Async: true,
	}
	p.capacityQueueHead = spawnQueueReplayFrom(r) != nil
	p.BundleHistory, _ = r.Context().Value(bundleHistoryContextKey{}).(*bundleHistoryLaunch)
	p.ManagedInstructionResources, _ = r.Context().Value(managedInstructionLaunchContextKey{}).([]db.ManagedInstruction)
	p.NoLaunch, _ = r.Context().Value(bundleNoLaunchContextKey{}).(bool)
	// A worker on a peer's Claude subscription whose token has not arrived
	// lands offline: it never starts on this node's own login.
	if p.ClaudeSubscription.peer != "" && p.ClaudeSubscription.token == "" {
		p.NoLaunch = true
	}
	p.capacityPreAdmitted, _ = r.Context().Value(capacityPreAdmittedKey{}).(bool)
	if teleportAuthority != nil {
		p.launchAuthority = teleportAuthority.check
		p.nodeCapacityReserved = true
		p.capacityPreAdmitted = true
	} else if offer, _ := r.Context().Value(teleportOfferContextKey{}).(*db.FederationBundleOffer); offer != nil && offer.Descriptor.Teleport != nil {
		// The admitting teleport row already holds this agent's capacity.
		p.capacityPreAdmitted = true
		row, err := db.GetFederationTeleport("in", offer.Peer, offer.Descriptor.ID)
		if err != nil || row == nil {
			writeError(w, 503, "teleport_provenance", "teleport provenance unavailable")
			return
		}
		p.launchAuthority = func() error {
			if teleportFrozen() {
				return errors.New("teleports are frozen by the operator")
			}
			if !fedPeerAllows(offer.Peer, g.ID, PermAgentsReceive) && !fedPeerAllows(offer.Peer, g.ID, PermAgentsTeleportReceive) {
				return errors.New("teleport receive authority revoked")
			}
			if _, err := pendingTeleportCredentials(offer.Peer, offer.Descriptor.Teleport.Credentials); err != nil {
				return err
			}
			return checkTeleportRepo(row, g.ID)
		}
	}
	if carry, ok := r.Context().Value(permissionCarryLaunchContextKey{}).(permissionCarryLaunch); ok && carry.Policy.Enabled {
		priorCheck := p.launchAuthority
		p.launchAuthority = func() error {
			if priorCheck != nil {
				if err := priorCheck(); err != nil {
					return err
				}
			}
			return carry.check()
		}
	}
	if identity, ok := r.Context().Value(federationIdentityLaunchKey{}).(federationIdentityLaunch); ok {
		prior := p.launchAuthority
		p.launchAuthority = func() error {
			if prior != nil {
				if e := prior(); e != nil {
					return e
				}
			}
			return db.ValidateFederationArrival(identity.Agent, identity.Offer)
		}
	}
	// An omitted include_group_context flag means opt-in — every spawn
	// path inherits the group context by default, the same way it
	// inherits default_cwd; the dashboard sends false explicitly to opt
	// out, and a profile's toggle spoke for it above.
	p.GroupContext = groupStartupContext(g, includeGroupContext)
	if body.NonInteractive && !includeGroupContext {
		// A one-shot is briefed by its prompt alone, as before the mission
		// split: no group context means no mission either.
		p.GroupContext = ""
	}
	setAuditSpawnResolved(r, spawnAuditResolution(p, resolvedLaunch, namedProfileHandle, map[string]*db.SpawnProfile{
		"explicit":       namedProfile,
		"group_default":  groupProfile,
		"global_default": globalProfile,
	}))
	if p.ClaudeSubscription.peer != "" {
		if body.NonInteractive || (p.Harness != "" && p.Harness != harness.DefaultName) {
			writeError(w, 400, "claude_subscription", "a peer's Claude subscription runs an ordinary Claude Code worker only")
			return
		}
		if p.ModelProxy != "" && p.ModelProxy != "off" {
			writeError(w, 400, "claude_subscription", "a worker on a peer's Claude subscription cannot also use a model gateway")
			return
		}
		p.ModelProxy = ""
	}
	if body.NonInteractive {
		managed, managedErr := managedinstructions.Resolve([]*db.AgentGroup{g})
		if managedErr != nil {
			writeError(w, 409, "managed_instructions", managedErr.Error())
			return
		}
		if p.Harness != harness.ShellName && len(managed) != 0 {
			writeError(w, 409, "managed_instructions", "one-shot runs with managed instructions are not supported yet; use an ordinary worker")
			return
		}
		p.OneShotName = oneShotName
		p.RemoteJob, _ = r.Context().Value(federationJobContextKey{}).(*federationJobLaunch)
		if p.RemoteJob != nil {
			p.RemoteJob.Harness = p.Harness
			p.RemoteJob.Permissions = p.PermissionOverrides
		}
		// One-shots bypass executeSpawn but spend just the same.
		if failure := checkUsageSpawnBlock(g); failure != nil {
			writeError(w, failure.Status, failure.Kind, failure.Msg)
			return
		}
		spawnUse, ok := takeSudoUse(w, spawnSudoGrant)
		if !ok {
			return
		}
		defer spawnUse.release()
		p.onLaunchAttempt = spawnUse.keep
		result, runErr := runNonInteractiveSpawn(r.Context(), p, body.RunTimeoutSeconds)
		if runErr != nil {
			writeError(w, runErr.Status, runErr.Kind, runErr.Msg)
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}

	if beforeExecuteSpawnForTest != nil {
		beforeExecuteSpawnForTest()
	}
	p.UsageProfileIDs, p.UsageSpawnProfile = spawnUsageProfiles, namedProfile
	p.spawnCopyCallerTiers = spawnCopyTiers(spawnCopyTier(db.SpawnCopyTierNamed, namedProfile))
	p.spawnCopyDefaultTiers = spawnCopyTiers(spawnCopyTier(db.SpawnCopyTierGroupDefault, groupProfile),
		spawnCopyTier(db.SpawnCopyTierGlobalDefault, globalProfile))
	p.spawnCopyResolved = true
	p.explicitLaunch = explicitLaunch
	p.relaunchOverrides = resolveRelaunchOverrides(relaunchExplicit, relaunchTiers)
	// Taken last: a used-up grant drops out of the permission sources, and every
	// check above must see the grant as it stood at the gate.
	spawnUse, ok := takeSudoUse(w, spawnSudoGrant)
	if !ok {
		return
	}
	defer spawnUse.release()
	outcome, fail := executeSpawn(g, p)
	if fail != nil {
		// The child may merely be slow, so this launch may well be running.
		if fail.Kind == spawnUnconfirmedKind {
			spawnUse.keep()
		}
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	spawnUse.keep()

	// executeSpawn intentionally re-reads default profiles as a safety net for
	// non-HTTP callers. If state changed after the handler snapshot, report the
	// values that actually reached the spawner and label the late fill honestly.
	//
	// Compared against executeSpawn's OWN echo rather than three loose strings,
	// so this covers every echoed field: before TCL-1097 a late fill of the
	// Copilot drive, the sandbox implementation, fast mode or the context cap
	// changed the launch and left this response describing the snapshot.
	// The relabel prefers the tier the final resolution NAMES and falls back to
	// the anonymous launch-default label. A difference is not always a race: for
	// fast_mode an explicit "inherit" clears the Set bit, so the overlay re-reads
	// the profile tiers the handler deliberately suppressed and a group default
	// legitimately wins here (that override is TCL-1109 and is not this echo's
	// doing). Whatever the cause, an operator needs the profile's NAME to go and
	// change it — "default profile (applied at launch)" names no tier at all, and
	// is kept only for a final source that names nothing usable either.
	launched := outcome.Resolved
	if launched == nil {
		launched = &agent.ResolvedLaunch{}
	}
	for field, final := range map[*agent.ResolvedField]agent.ResolvedField{
		&resolvedLaunch.Harness:          launched.Harness,
		&resolvedLaunch.Model:            launched.Model,
		&resolvedLaunch.Effort:           launched.Effort,
		&resolvedLaunch.ContextWindowMax: launched.ContextWindowMax,
		&resolvedLaunch.CopilotAPI:       launched.CopilotAPI,
		&resolvedLaunch.CodexAppServer:   launched.CodexAppServer,
		&resolvedLaunch.FastMode:         launched.FastMode,
		&resolvedLaunch.SandboxImpl:      launched.SandboxImpl,
	} {
		if field.Value != final.Value {
			field.Value = final.Value
			field.Source = agent.ProvLaunchDefault
			if named := strings.TrimSpace(final.Source); named != "" && named != agent.ProvExplicit {
				// Both halves, in the two places built to carry them: the tier goes in
				// Source because that is what an operator acts on, and the fact that it
				// landed late goes in Note because that is what explains the response
				// differing from what the request looked like it would produce.
				field.Source = final.Source
				field.Note = agent.ProvLaunchFillNote
			}
		}
	}
	setAuditSpawnResolved(r, spawnAuditResolution(p, resolvedLaunch, namedProfileHandle, map[string]*db.SpawnProfile{
		"explicit":       namedProfile,
		"group_default":  groupProfile,
		"global_default": globalProfile,
	}))
	resp := map[string]any{
		"group":        g.Name,
		"conv_id":      outcome.ConvID,
		"label":        outcome.Label,
		"tmux_session": outcome.TmuxSession,
		// Echo the fully-resolved launch shape + per-field provenance so the
		// caller can see WHERE harness/model/effort came from — the TCL-304
		// mistake-preventer for a blank spawn silently inheriting a default
		// profile's vendor. Values come from executeSpawn's final params so the
		// echo remains truthful even if a default profile changed mid-request.
		"resolved": resolvedLaunch,
	}
	if outcome.Label != "" {
		// An offline landing has no session to attach to.
		resp["attach_cmd"] = "tclaude session attach " + outcome.Label
	}
	// Lead with the spawned actor's stable id. Pending Codex spawns reserve it
	// before their harness conv-id materialises; inline spawns resolve it from
	// the enrolled conversation as before.
	aid := outcome.AgentID
	if aid == "" {
		aid = peerAgentID(outcome.ConvID)
	}
	if aid != "" {
		resp["agent_id"] = aid
	}
	// Echo the requested task-reference link with its verified binding
	// state (TCL-568): "bound" only when the link is readable back off the
	// enrolled actor, "pending" when the spawn went pending (the persisted
	// pending row carries the link; the sweeper binds it at enrollment) or
	// the binding couldn't be confirmed. Never claim linkage that wasn't
	// verifiably written.
	if p.TaskURL != "" {
		resp["task_ref_url"] = p.TaskURL
		resp["task_ref_state"] = taskRefBindState(outcome.ConvID, p.TaskURL)
	}
	// FocusMode is only ever non-empty when the caller asked for auto-focus.
	// "browser" means the dashboard explicitly targeted a web terminal or
	// openTerminal couldn't pop a native window; in either case the spawn modal
	// points at focus_ws (see spawnOutcome.FocusMode).
	if outcome.FocusMode != "" {
		resp["focus_mode"] = outcome.FocusMode
		if outcome.FocusMode == "browser" {
			resp["focus_ws"] = spawnFocusWSPath(outcome.Label)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
