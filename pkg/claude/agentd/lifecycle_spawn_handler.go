package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/managedinstructions"
	"github.com/tofutools/tclaude/pkg/claude/harness"
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
	if fail := req.resolveHarnessModes(); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.checkSandboxLineage(); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.resolveSandboxPolicy(); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.proveLaunchDirs(w, r); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.resolveHarnessOptions(); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.resolveRemoteControl(); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if fail := req.validateTaskRef(); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}

	// The steps below have not moved onto spawnRequest yet; they read the
	// request through these locals.
	body := &req.body
	requestedSpawnConfigJSON, explicitLaunch, externalSpawn := req.requestedSpawnConfigJSON, req.explicitLaunch, req.externalSpawn
	spawnerConvID, spawnSudoGrant, teleportAuthority := req.spawnerConvID, req.spawnSudoGrant, req.teleportAuthority
	spawnUsageProfiles, oneShotName, attachments := req.spawnUsageProfiles, req.oneShotName, req.attachments
	replyToConv, timeout, namedProfile := req.replyToConv, req.timeout, req.namedProfile
	namedProfileHandle, groupProfile, globalProfile := req.namedProfileHandle, req.groupProfile, req.globalProfile
	relaunchTiers, relaunchExplicit, harnessSource := req.relaunchTiers, req.relaunchExplicit, req.harnessSource
	cwd, worktreePath, worktreeBranch := req.cwd, req.worktreePath, req.worktreeBranch
	h, modelSource, effortSource := req.h, req.modelSource, req.effortSource
	sandboxSource, contextWindowMaxSource, fastMode := req.sandboxSource, req.contextWindowMaxSource, req.fastMode
	fastModeSet, fastModeSource, sandboxImplSource := req.fastModeSet, req.fastModeSource, req.sandboxImplSource
	autoReviewSet, sshWorkaroundSource, trustDirSet := req.autoReviewSet, req.sshWorkaroundSource, req.trustDirSet
	autoMemory, peerMessaging, copilotAPI := req.autoMemory, req.peerMessaging, req.copilotAPI
	copilotAPISet, copilotAPISource, piDrive := req.copilotAPISet, req.copilotAPISource, req.piDrive
	codexAppServer, codexAppServerSet, codexAppServerSource := req.codexAppServer, req.codexAppServerSet, req.codexAppServerSource
	includeGroupContext, identity, contextFeatures := req.includeGroupContext, req.identity, req.contextFeatures
	profileContext, resolvedLaunch, effort := req.profileContext, req.resolvedLaunch, req.effort
	model, harnessBuiltinMode, approvalPolicy := req.model, req.harnessBuiltinMode, req.approvalPolicy
	toolGovernance, autoReview, effectiveSandbox := req.toolGovernance, req.autoReview, req.effectiveSandbox
	sshWorkaroundIntent, proofDirs, proofToken := req.sshWorkaroundIntent, req.proofDirs, req.proofToken
	codexGitCommonDir, codexGitCommonDirPinned, gitWorktreeWriteDirs := req.codexGitCommonDir, req.codexGitCommonDirPinned, req.gitWorktreeWriteDirs
	askTimeout, autoCompactWindow, trustDir := req.askTimeout, req.autoCompactWindow, req.trustDir
	remoteControl := req.remoteControl

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
