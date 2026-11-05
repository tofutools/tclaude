package agentd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
	// A spawn preview stops here, before the first side effect (the rate
	// limit slot below): it has resolved and validated the request exactly as
	// the spawn would, and answers with what it would launch. Nothing above
	// this line may write, allocate or audit: TestSpawnPreview_HasNoSideEffects
	// guards it.
	if isSpawnPreview(r) {
		writeJSON(w, http.StatusOK, spawnPreviewResponse{Resolved: req.resolvedLaunch})
		return
	}
	if fail := req.queueOrClaimRateSlot(w, r); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	p, fail := req.buildSpawnParams(r)
	if fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	if req.body.NonInteractive {
		req.serveOneShot(w, r, p)
		return
	}

	if beforeExecuteSpawnForTest != nil {
		beforeExecuteSpawnForTest()
	}
	req.finishSpawnParams(p)
	// Taken last: a used-up grant drops out of the permission sources, and every
	// check above must see the grant as it stood at the gate.
	spawnUse, ok := takeSudoUse(w, req.spawnSudoGrant)
	if !ok {
		return
	}
	defer spawnUse.release()
	outcome, fail := executeSpawn(g, *p)
	if fail != nil {
		// The child may merely be slow, so this launch may well be running.
		if fail.Kind == spawnUnconfirmedKind {
			spawnUse.keep()
		}
		writeSpawnFailure(w, fail)
		return
	}
	spawnUse.keep()
	req.reconcileLaunchEcho(r, p, outcome)
	writeJSON(w, http.StatusOK, req.spawnResponse(p, outcome))
}
