package agentd

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/managedinstructions"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/launchspec"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// spawnRequest is one POST /v1/groups/{name}/spawn on its way to
// executeSpawn: the decoded request plus what each step of handleGroupSpawn
// has resolved so far. The steps are methods on it, run in order by
// handleGroupSpawn; each returns nil to go on or a *spawnFailure to refuse
// the spawn, and handleGroupSpawn is the one place that writes the refusal.
type spawnRequest struct {
	g *db.AgentGroup

	// body is the request as it is resolved: steps trim, default and fill it
	// in. requestedSpawnConfigJSON and explicitLaunch keep what the caller
	// actually sent, taken before any step changed it.
	body                     agent.SpawnRequest
	requestedSpawnConfigJSON string
	explicitLaunch           map[string]db.ExplicitLaunchValue

	// Set by authorize.
	externalSpawn          bool
	sandboxProfileForScope string
	spawnerConvID          string
	spawnSudoGrant         int64
	teleportAuthority      *teleportLandingAuthority
	sandboxScopePinned     bool
	// releaseExternalChild gives back an external steward's child slot. It is
	// a no-op unless authorize claimed one, and handleGroupSpawn defers it.
	releaseExternalChild func()

	// Set by the normalize steps.
	spawnUsageProfiles []int64
	oneShotName        string
	attachments        []string
	permOverrides      map[string]db.PermissionOverride
	replyToConv        string
	timeout            time.Duration

	// Set by resolveProfiles.
	namedProfile       *db.SpawnProfile
	namedProfileHandle string
	groupProfile       *db.SpawnProfile
	globalProfile      *db.SpawnProfile
	relaunchTiers      []*db.SpawnProfile
	relaunchExplicit   map[string]string
	profileTiers       []launchProfileTier
	harnessSource      string

	// Set by resolveLaunchTarget.
	cwd            string
	worktreePath   string
	worktreeBranch string
	h              *harness.Harness

	// Set by resolveLaunchValues.
	fastMode       bool
	fastModeSet    bool
	fastModeNote   string
	fastModeSource string

	// launchValues is resolveLaunchValues' whole resolution: each value
	// field's tier source and skip note, read by the echo and the spawn params.
	launchValues launchValueResolution

	// Set by resolveSandboxImplementation.
	sandboxImplNote   string
	sandboxImplSource string

	// Set by resolveLaunchBools.
	launchBools             launchBoolResolution
	sshWorkaround           bool
	piDrive                 bool
	includeGroupContext     bool
	includeGroupContextNote string

	// Set by resolveIdentity.
	identityNotes []string
	identity      launchIdentity

	// Set by resolveContextFeatures.
	contextFeatures     map[string]string
	profileContext      string
	profileContextNote  string
	contextFeaturesNote string

	// Set by buildLaunchEcho.
	resolvedLaunch *agent.ResolvedLaunch

	// Set by resolveHarnessModes.
	effort             string
	model              string
	harnessBuiltinMode string
	approvalPolicy     string
	toolGovernance     string
	autoReview         bool

	// Set by resolveSandboxPolicy.
	effectiveSandbox    sandboxpolicy.Snapshot
	sshWorkaroundIntent bool

	// Set by proveLaunchDirs.
	proofDirs                []string
	proofToken               string
	codexGitCommonDir        string
	codexGitCommonDirPinned  bool
	gitWorktreeWriteDirs     []string
	autoTrustSiblingWorktree bool

	// Set by resolveHarnessOptions.
	askTimeout        string
	autoCompactWindow string
	trustDir          bool

	// Set by resolveRemoteControl.
	remoteControl bool
}

// errSpawnResponseWritten is the failure a step returns when a shared helper
// it called has already written the response (a refusal, or an ask-human
// approval in flight). writeSpawnFailure writes nothing more for it.
var errSpawnResponseWritten = &spawnFailure{}

// writeSpawnFailure writes a step's refusal as the HTTP error response.
func writeSpawnFailure(w http.ResponseWriter, fail *spawnFailure) {
	if fail == errSpawnResponseWritten {
		return
	}
	writeError(w, fail.Status, fail.Kind, fail.Msg)
}

// decodeSpawnRequest reads the request body and refuses a non-interactive
// spawn that carries settings only an interactive agent can use.
//
// requireGroupPermission also hands back the caller's conv-id: a real
// agent (e.g. a PO orchestrating workers) resolves to its conv-id,
// the human resolves to "". It is the default reply-to target for
// the startup briefing assembled further down. Owners of g pass
// without an explicit groups.members.spawn grant (owner-state default); the
// spawn guardrails below still bind them (member cap, rate limit) and
// already treat an owner as allowed for the group restriction.
//
// agent.SpawnRequest is the single shared request shape — the same
// type `tclaude agent spawn`, `tclaude --join-group`, and the
// dashboard's spawn modal marshal — so the wire contract can't drift
// between the CLI and the dashboard. See its doc comment for the
// per-field semantics.
//
// It is decoded BEFORE the gate because groups.members.spawn may be scoped by
// spawn_profile, and the gate can only evaluate that against the profile
// this spawn will actually launch with. decodeSpawnBody restores r.Body so
// the ask-human popup still previews the full request.
func decodeSpawnRequest(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) (*spawnRequest, *spawnFailure) {
	req := &spawnRequest{g: g, releaseExternalChild: func() {}}
	body := &req.body
	requestedSpawnConfigJSON, decoded := decodeSpawnBody(w, r, body)
	if !decoded {
		return nil, errSpawnResponseWritten
	}
	req.requestedSpawnConfigJSON = requestedSpawnConfigJSON
	// Taken before any profile tier fills the request in.
	req.explicitLaunch = captureExplicitLaunch(body, time.Now())
	if body.NonInteractive && (body.ReplyTo != "" || body.Role != "" ||
		body.RoleRef != "" || len(body.RoleRefs) > 0 || body.Descr != "" ||
		body.TaskURL != "" || body.TaskLabel != "" || body.AutoFocus || body.AutoFocusWeb ||
		body.IsOwner || len(body.PermissionOverrides) > 0 || len(body.UsageProfiles) > 0 || body.RemoteControl != nil ||
		body.CopilotAPI != nil || body.CodexAppServer != nil || body.PiDrive != nil || body.AutoReview) {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_request",
			"non-interactive spawn does not accept identity (other than name), messaging, or interactive drive settings"}
	}
	return req, nil
}

// authorize admits the caller: the spawn permission (or a teleport landing's
// authority), the external-steward ceiling, the group's state, and the
// operator-only sandbox opt-outs.
//
// Preserve the caller's decoded wire parameters before profile/default
// resolution, generated-name assignment, normalization, or permission
// attenuation mutates body below. Resolved and running launch state have
// their own durable records; this is the requested side of the comparison.
// The spawn_profile the gate judges is the RESOLVED one — request profile,
// else the group default, else the global default — never the raw request
// field. "" (no profile resolves, or a named one that does not exist)
// leaves the dimension undescribed, which a scoped grant cannot satisfy:
// an inline launch shape is not a named profile and must not pass a
// profile-pinned grant. An unscoped grant is unaffected, so this reaches
// the existing 400 for a bad profile name exactly as before.
//
// The sandbox_profile the gate judges is likewise the RESOLVED one — the
// request selection, else the ambient group/global assignment the launch
// will inherit — so a grant naming the operator's default profile does not
// force every caller to restate it. The check further down refuses the
// launch modes that would drop that inherited tier again.
func (req *spawnRequest) authorize(w http.ResponseWriter, r *http.Request) *spawnFailure {
	g, body := req.g, &req.body
	req.externalSpawn = peerFromContext(r.Context()).ExternalSteward
	if req.externalSpawn && !prepareExternalStewardSpawn(w, r, body) {
		return errSpawnResponseWritten
	}
	req.sandboxProfileForScope = resolvedSandboxProfileNameForScope(g, body.SandboxProfile)
	spawnActionContext := ActionContext{
		Group: g.Name, SpawnProfile: resolvedSpawnProfileNameForScope(g, body.Profile),
		SandboxProfile: req.sandboxProfileForScope,
	}
	req.teleportAuthority = teleportLandingFromRequest(r)
	if req.teleportAuthority != nil {
		if req.teleportAuthority.record.Landing.GroupID != g.ID {
			return &spawnFailure{403, "teleport_group", "teleport landing group changed"}
		}
		if err := req.teleportAuthority.check(); err != nil {
			return &spawnFailure{403, "teleport_revoked", err.Error()}
		}
	} else {
		var ok bool
		req.spawnerConvID, ok = requireSpawnPermission(w, r, g, spawnActionContext)
		if !ok {
			return errSpawnResponseWritten
		}
		// Read here: later gates overwrite what is on the request.
		req.spawnSudoGrant = authorizedSudoGrantIDForRequest(r)
	}
	// Whether the grant that admitted this spawn was CONDITIONED on the sandbox
	// profile, as opposed to merely being evaluated alongside it. Only that
	// caller has to keep the profile load-bearing all the way to the launch;
	// binding an unscoped or ownership-derived grant to it would refuse spawns
	// that had nothing to do with the scope feature. See the check below.
	req.sandboxScopePinned = scopePinsDimension(r, req.spawnerConvID,
		authorizedPermissionForRequest(r, ""), spawnActionContext, ScopeDimSandboxProfile)
	if req.externalSpawn {
		slug := authorizedPermissionForRequest(r, "")
		if slug != PermAgentSpawn || !req.sandboxScopePinned ||
			!scopePinsDimension(r, req.spawnerConvID, slug, spawnActionContext, ScopeDimGroup) ||
			!scopePinsDimension(r, req.spawnerConvID, slug, spawnActionContext, ScopeDimSpawnProfile) {
			return &spawnFailure{http.StatusForbidden, externalStewardCeilingCode,
				"an external steward starts workers only through its pinned agent.spawn grant"}
		}
		if strings.TrimSpace(g.DefaultCwd) == "" {
			return &spawnFailure{http.StatusConflict, "external_steward_no_directory",
				fmt.Sprintf("group %q has no default directory, which is where an external steward's worker has to start", g.Name)}
		}
		release, ok := claimExternalStewardChild(w, req.spawnerConvID)
		if !ok {
			return errSpawnResponseWritten
		}
		req.releaseExternalChild = release
	}
	if !requireGroupActive(w, g) {
		return errSpawnResponseWritten
	}
	if !routeMembershipMutationAllowed(w, g) {
		return errSpawnResponseWritten
	}
	if body.AllowUnenforcedSandbox && !isDashboardSpawnPeer(r) && !fedSpawnSandboxAuthorized(r) {
		return &spawnFailure{http.StatusForbidden, "unenforced_sandbox_override_restricted",
			"only the human operator may allow an unenforced sandbox through the dashboard spawn dialog"}
	}
	if fail := rejectBreakGlassSpawn(body.BreakGlassAcknowledged); fail != nil {
		return fail
	}
	body.SandboxProfile = strings.TrimSpace(body.SandboxProfile)
	if body.OmitSandboxProfiles && body.SandboxProfile != "" {
		return &spawnFailure{http.StatusBadRequest, "invalid_sandbox_profile",
			"sandbox_profile and omit_sandbox_profiles are mutually exclusive"}
	}
	if body.OmitSandboxProfiles && classify(peerFromContext(r.Context())) != classHuman && !fedSpawnSandboxAuthorized(r) {
		return &spawnFailure{http.StatusForbidden, "sandbox_profile_restricted",
			"only the human operator may omit inherited sandbox profiles"}
	}
	return nil
}

// validateLaunchOptions checks the request's harness config, relaunch policy
// and usage profiles before anything resolves against them.
func (req *spawnRequest) validateLaunchOptions(w http.ResponseWriter, r *http.Request) *spawnFailure {
	body := &req.body
	if !validateSpawnHarnessConfig(w, r, body.HarnessConfig) {
		return errSpawnResponseWritten
	}
	if err := launchspec.ValidateOverrides(body.RelaunchPolicy, true); err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_relaunch_policy", err.Error()}
	}
	spawnUsageProfiles, usageOK := resolveSpawnUsageProfiles(w, r, body.UsageProfiles)
	if !usageOK {
		return errSpawnResponseWritten
	}
	req.spawnUsageProfiles = spawnUsageProfiles
	return nil
}

// checkGuardrails applies the spawn guardrails.
//
// Spawn guardrails — runaway-prevention for an agent that the human
// granted `groups.members.spawn`. The group's hard member cap (binds the human
// too) and — for agent callers only (spawnerConvID != "") — the group
// restriction run here, before any subprocess is launched, so a rejected
// spawn costs nothing. The third guardrail, the per-caller rate limit,
// is claimed after the validation gates below (claimSpawnRateSlot) so a
// refused request — including the dir write-proof challenge round-trip —
// never burns a slot. See spawn_guardrails.go.
func (req *spawnRequest) checkGuardrails(r *http.Request) *spawnFailure {
	g, body, spawnerConvID := req.g, &req.body, req.spawnerConvID
	if body.NonInteractive && r.Context().Value(federationJobContextKey{}) == nil {
		// A one-shot does not consume a group seat. Keep the caller's group
		// restriction and the rate limit while leaving max_members for members.
		if spawnerConvID != "" && authorizedPermissionForRequest(r, "") != PermAgentSpawn {
			if fail := checkSpawnGroupRestriction(g, spawnerConvID); fail != nil {
				return fail
			}
		}
	} else if fail := checkSpawnGuardrails(g, spawnerConvID,
		authorizedPermissionForRequest(r, "") == PermAgentSpawn); fail != nil {
		return fail
	}
	return nil
}

// normalizeInitialMessage trims and bounds the task brief.
//
// The initial message is delivered to the new agent's inbox as an
// agent_messages row — not typed into its tmux pane — so newlines
// survive verbatim and a multi-line task brief arrives intact. We
// only cap the length and reject NUL / escape / other non-text
// control characters that would corrupt an `inbox read` render.
func (req *spawnRequest) normalizeInitialMessage() *spawnFailure {
	body := &req.body
	body.InitialMessage = strings.TrimSpace(body.InitialMessage)
	if !isValidInitialMessage(body.InitialMessage) {
		return &spawnFailure{http.StatusBadRequest, "invalid_initial_message",
			fmt.Sprintf("initial_message must be at most %d characters; newlines and tabs "+
				"are allowed (it is delivered to the agent's inbox, not typed into "+
				"its pane), but other control characters are not", agent.MaxInitialMessageBytes)}
	}
	if body.NonInteractive && body.InitialMessage == "" && len(cleanRoleRefs(body.PromptRefs)) == 0 {
		return &spawnFailure{http.StatusBadRequest, "invalid_initial_message", "non_interactive requires initial_message"}
	}
	return nil
}

// normalizeName trims the agent name, normalizes or refuses an invalid one,
// and keeps a one-shot's explicit name for its tmux session.
//
// Reject an invalid agent name at the boundary rather than silently
// dropping it downstream (executeSpawn only applies a name that clears
// isValidRenameTitle). An empty name stays valid — the agent gets an
// auto-generated label; a non-empty one must be a safe token. The CLI
// (agent.IsValidSpawnName) and dashboard mirror this, but this is the
// authoritative gate for the user-facing spawn surfaces: `tclaude agent
// spawn`, `--join-group`, and the dashboard modal all POST through here.
// (The group-template instantiator builds names as group+template and
// calls executeSpawn directly, bypassing this gate; it falls back to the
// downstream isValidRenameTitle silent-drop — see handleTemplateInstantiate.)
func (req *spawnRequest) normalizeName() *spawnFailure {
	body := &req.body
	body.Name = strings.TrimSpace(body.Name)
	// Auto-normalize an invalid name to the safe branch-token charset when
	// config's agent.spawn_name_normalize is on (the default), so any name a
	// human types "just works" — "code reviewer!" lands as "code-reviewer"
	// rather than 400ing. The CLI and dashboard normalize client-side too, so
	// this is usually a no-op here (NormalizeSpawnName is idempotent); it is
	// the authoritative backstop for a raw POST. Read config live so a Config
	// tab toggle takes effect without a daemon restart. Disabled (explicit
	// false) keeps the strict reject below.
	if !isValidSpawnName(body.Name) {
		if cfg, _ := config.Load(); cfg.SpawnNameNormalizeEnabled() {
			body.Name = agent.NormalizeSpawnName(body.Name)
		}
	}
	if !isValidSpawnName(body.Name) {
		return &spawnFailure{http.StatusBadRequest, "invalid_name",
			fmt.Sprintf("name must be 1-%d characters from [A-Za-z0-9_-] (letters, "+
				"digits, underscore, dash); spaces, punctuation, and unicode are not "+
				"allowed (the name doubles as a git worktree branch name and becomes "+
				"the conversation title)", agent.MaxSpawnNameLen)}
	}
	// A one-shot's only use for a name is its tmux session, and only an
	// explicit one: profile agent_name and the derived group label below
	// would name a member the one-shot never becomes.
	if body.NonInteractive {
		req.oneShotName = body.Name
	}
	return nil
}

// normalizeAttachments cleans and bounds the attachment paths.
//
// Attachment paths (uploaded files / pasted screenshots from the dashboard's
// /api/spawn-attachments endpoint) are folded into the startup briefing as an
// "Attached files" section. Clean + bound them the same way as the initial
// message — they share its inbox render and inline-launch path.
func (req *spawnRequest) normalizeAttachments() *spawnFailure {
	attachments, attErr := sanitizeSpawnAttachments(req.body.Attachments)
	if attErr != "" {
		return &spawnFailure{http.StatusBadRequest, "invalid_attachments", attErr}
	}
	req.attachments = attachments
	return nil
}

// normalizePermissionOverrides validates the shape of the birth-time
// permission overrides.
//
// Birth-time access controls: make the new agent a group owner and/or seed
// its permanent per-slug permission overrides, the same grants the
// Edit-agent modal applies to a live agent — but applied at enrollment so
// the agent's first turn already has them. SHAPE is validated here, at the
// boundary, before any subprocess launches: every override slug must be
// registered and every effect in {grant,deny} ("default"/"" carries no
// override and is dropped).
//
// The PRIVILEGE gate runs further down instead, once the profile tier stack
// has had its say: a spawn that merely NAMES a profile inherits that
// profile's owner flag and overrides, so gating the raw request body would
// check a value the launch does not use. See the birth-time access block
// after the launch fields resolve.
func (req *spawnRequest) normalizePermissionOverrides() *spawnFailure {
	permOverrides, povErr := normalizeSpawnPermissionOverrides(req.body.PermissionOverrides)
	if povErr != "" {
		return &spawnFailure{http.StatusBadRequest, "invalid_permission_overrides", povErr}
	}
	req.permOverrides = permOverrides
	return nil
}

// resolveReplyTo picks the startup briefing's sender.
//
// Resolve the startup briefing's sender. Default: the spawn
// requester (an agent → its conv-id; a human → ""). An explicit
// reply_to selector overrides it — the knob a coordinator uses to
// route a worker's replies to a third agent rather than itself.
func (req *spawnRequest) resolveReplyTo(w http.ResponseWriter, r *http.Request) *spawnFailure {
	spawnerConvID := req.spawnerConvID
	req.replyToConv = spawnerConvID
	if rt := strings.TrimSpace(req.body.ReplyTo); rt != "" {
		res, _, rtErr := agent.ResolveSelector(rt)
		if rtErr != nil {
			return &spawnFailure{http.StatusBadRequest, "invalid_reply_to",
				fmt.Sprintf("reply_to %q: %v", rt, rtErr)}
		}
		// The briefing is from this actor, so its child could answer a Steward
		// the spawner may not write to.
		if spawnerConvID != "" && res.ConvID != spawnerConvID && isLiveSteward(res.ConvID) {
			if _, _, ok := resolveMessageRouting(w, r, spawnerConvID, res.ConvID); !ok {
				return errSpawnResponseWritten
			}
		}
		req.replyToConv = res.ConvID
	}
	return nil
}

// resolveTimeout sets how long the spawn waits for its conversation: 30s by
// default, the request's timeout_seconds capped at 5 minutes otherwise.
func (req *spawnRequest) resolveTimeout() {
	req.timeout = 30 * time.Second
	if req.body.TimeoutSeconds > 0 {
		req.timeout = time.Duration(req.body.TimeoutSeconds) * time.Second
		if req.timeout > 5*time.Minute {
			req.timeout = 5 * time.Minute
		}
	}
}

// applyDefaultCwd fills a blank cwd in.
//
// When the request leaves cwd blank, fall back to the group's
// default_cwd (the "group default start dir" set via the
// dashboard or `groups set-default-dir`). This makes the default
// reach every spawn path — CLI, API, dashboard — not just the
// dashboard's client-side prefill. Past an empty default_cwd comes
// the directory the CLI ran in; with neither, cwd stays blank and
// resolveSpawnCwd inherits the daemon's own cwd as before.
func (req *spawnRequest) applyDefaultCwd() {
	req.body.Cwd = spawnRequestCwd(req.body.Cwd, req.body.CallerCwd, req.g)
}

// resolveProfiles loads the profile tier stack — the named profile, then the
// group and global defaults — gates the relaunch overrides against it, and
// resolves the harness through it.
func (req *spawnRequest) resolveProfiles(w http.ResponseWriter, r *http.Request) *spawnFailure {
	body := &req.body
	spawnerConvID := req.spawnerConvID
	g := req.g
	// Resolve the harness independently through the complete chain. Other launch
	// fields never pin it: explicit request > named CLI profile > group default
	// profile > global default profile > Claude. Field candidates are validated
	// against this resolved harness below.
	var namedProfile *db.SpawnProfile
	namedProfileHandle := ""
	if name := strings.TrimSpace(body.Profile); name != "" {
		var profileErr error
		namedProfileHandle = name
		namedProfile, profileErr = db.ResolveSpawnProfile(name)
		if profileErr != nil || namedProfile == nil {
			return &spawnFailure{http.StatusBadRequest, "invalid_profile", fmt.Sprintf("spawn profile %q does not exist", name)}
		}
		if fail := profileSpawnFailure(namedProfile, spawnerConvID); fail != nil {
			return fail
		}
	}
	// The spawn pipeline's default-tier loader, which the dashboard's live
	// relaunch read (spawnRelaunchPreview) shares.
	groupProfile, globalProfile := launchDefaultProfiles(g, &spawnParams{})
	for _, prof := range []*db.SpawnProfile{groupProfile, globalProfile} {
		if fail := profileSpawnFailure(prof, spawnerConvID); fail != nil {
			return fail
		}
	}
	namedProfileSource := profileSource(namedProfile, agent.ProvCLIProfileSource)
	if namedProfile != nil && namedProfileHandle != namedProfile.Name {
		namedProfileSource = fmt.Sprintf(`profile %q via alias %q`, namedProfile.Name, namedProfileHandle)
	}
	relaunchTiers := []*db.SpawnProfile{namedProfile, groupProfile, globalProfile}
	relaunchExplicit, relaunchOK := gateSpawnRelaunchOverrides(w, r, body.RelaunchPolicy, relaunchTiers)
	if !relaunchOK {
		return errSpawnResponseWritten
	}
	profileTiers := []launchProfileTier{
		{profile: namedProfile, source: namedProfileSource},
		{profile: groupProfile, source: profileSource(groupProfile, agent.ProvGroupProfileSource),
			defaultTier: true},
		{profile: globalProfile, source: profileSource(globalProfile, agent.ProvGlobalProfileSource),
			defaultTier: true},
	}
	harnessSource := agent.ProvExplicit
	if strings.TrimSpace(body.Harness) == "" {
		harnessSource = agent.ProvHarnessDefault
		for _, tier := range profileTiers {
			if tier.profile != nil {
				body.Harness = harnessOrDefault(tier.profile.Harness)
				harnessSource = tier.source
				break
			}
		}
	}
	if strings.TrimSpace(body.Harness) == "" {
		body.Harness = harness.DefaultName
	}
	req.namedProfile = namedProfile
	req.namedProfileHandle = namedProfileHandle
	req.groupProfile = groupProfile
	req.globalProfile = globalProfile
	req.relaunchTiers = relaunchTiers
	req.relaunchExplicit = relaunchExplicit
	req.profileTiers = profileTiers
	req.harnessSource = harnessSource
	return nil
}

// resolveLaunchTarget validates the cwd and worktree, resolves the harness and
// checks it against federation placement and the cross-harness spawn policy.
func (req *spawnRequest) resolveLaunchTarget(r *http.Request) *spawnFailure {
	body := &req.body
	spawnerConvID := req.spawnerConvID
	g := req.g
	// Validate the requested cwd before doing any work. Expands "~",
	// makes the path absolute, and confirms it exists as a directory.
	// Catching a bad cwd here turns what used to be a silent 30s
	// conv-id-poll timeout into an immediate, actionable error.
	cwd, cwdErr := resolveSpawnCwd(body.Cwd)
	if cwdErr != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_cwd", cwdErr.Error()}
	}

	// Validate the optional worktree dir the same way — it must exist
	// (the dashboard creates it just before spawning). Caught here so
	// a stale path becomes an immediate 400 rather than a welcome
	// message pointing the agent at a directory that isn't there.
	var worktreePath string
	if strings.TrimSpace(body.WorktreePath) != "" {
		wt, wtErr := resolveSpawnCwd(body.WorktreePath)
		if wtErr != nil {
			return &spawnFailure{http.StatusBadRequest, "invalid_worktree", wtErr.Error()}
		}
		worktreePath = wt
	}
	worktreeBranch := strings.TrimSpace(body.WorktreeBranch)

	// Resolve the requested harness (default Claude Code). An unknown
	// name is a 400 here rather than a silent failure once the forked
	// session exits. The chosen harness's ModelCatalog then validates
	// effort/model below, so a Codex spawn is checked against Codex's
	// rules (rejects Claude Code slugs, accepts effort levels) instead of
	// Claude Code's.
	h, harnessErr := resolveSpawnHarness(body.Harness)
	if harnessErr != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_harness", harnessErr.Error()}
	}
	if constraints, ok := r.Context().Value(federationPlacementConstraintsKey{}).(string); ok {
		if err := validateLocalPlacementRequirements(constraints, h.Name); err != nil {
			return &spawnFailure{409, "node_incompatible", err.Error()}
		}
	}
	if body.NonInteractive && h.Name == harness.ShellName && body.IncludeGroupContext != nil && *body.IncludeGroupContext {
		return &spawnFailure{http.StatusBadRequest, "invalid_group_context",
			"--group-context is not meaningful for a shell command"}
	}
	// Cross-harness spawn policy is evaluated only after the complete profile
	// stack has resolved the target vendor. That closes the indirect path where
	// an agent omits --harness but a group/global default profile flips it.
	if fail := spawnHarnessPolicyFailure(g, spawnerConvID, h.Name); fail != nil {
		return fail
	}
	req.cwd = cwd
	req.worktreePath = worktreePath
	req.worktreeBranch = worktreeBranch
	req.h = h
	return nil
}

// resolveLaunchValues resolves the string/int launch value fields (model proxy
// first, with its permission gate, then model, effort, sandbox mode, approval,
// tools, timeouts and context window) and fast mode through the tier stack.
func (req *spawnRequest) resolveLaunchValues(r *http.Request) *spawnFailure {
	body := &req.body
	spawnerConvID := req.spawnerConvID
	profileTiers := req.profileTiers
	h := req.h
	// The string/int launch value fields ride one table
	// (launch_value_fields.go). The model proxy is resolved on its own first so
	// its permission gate still refuses before any other field is judged.
	launchValues := launchValueResolution{Values: launchValueFields{
		ModelProxy: body.ModelProxy, Model: body.Model, Effort: body.Effort,
		Sandbox: body.HarnessBuiltinMode, Approval: body.ApprovalPolicy, Tools: body.ToolGovernance,
		AskUserQuestionTimeout: body.AskUserQuestionTimeout, AutoCompactWindow: body.AutoCompactWindow,
		ContextWindowMax: body.ContextWindowMax,
	}}
	if fail := resolveLaunchValueFields(h, profileTiers, &launchValues, launchValueOptions{}, launchValueModelProxy); fail != nil {
		return fail
	}
	body.ModelProxy = launchValues.Values.ModelProxy
	if body.ModelProxy != "" && body.ModelProxy != "off" && spawnerConvID != "" {
		peer, name, err := resolveModelProxyReference(body.ModelProxy)
		if err != nil {
			return &spawnFailure{400, "invalid_model_proxy", err.Error()}
		}
		allowed, _, err := permissionAllowsAction(r, spawnerConvID, PermModelsProxy, ActionContext{RemotePeer: peer.InstanceID, HTTPProxy: name})
		if err != nil || !allowed {
			return &spawnFailure{403, "permission_denied", "models.proxy is required for this model gateway"}
		}
	}
	validateModel := func(raw string) (string, error) {
		value, err := h.Models.ValidateModel(raw)
		if err == nil {
			return value, nil
		}
		other := "codex"
		if h.Name == "codex" {
			other = harness.DefaultName
		}
		return "", fmt.Errorf("model %q is not valid for %s; pass --harness %s or a matching --model: %w",
			strings.TrimSpace(raw), h.Name, other, err)
	}
	if fail := resolveLaunchValueFields(h, profileTiers, &launchValues, launchValueOptions{validateModel: validateModel},
		allLaunchValueFields[launchValueModel:]...); fail != nil {
		return fail
	}
	resolvedValues := launchValues.Values
	body.Model, body.Effort = resolvedValues.Model, resolvedValues.Effort
	body.HarnessBuiltinMode, body.ApprovalPolicy, body.ToolGovernance = resolvedValues.Sandbox, resolvedValues.Approval, resolvedValues.Tools
	body.AskUserQuestionTimeout, body.AutoCompactWindow = resolvedValues.AskUserQuestionTimeout, resolvedValues.AutoCompactWindow
	body.ContextWindowMax = resolvedValues.ContextWindowMax
	// Fast mode is a bool-family field, resolved after the value fields.
	var fieldFail *spawnFailure
	var fastMode, fastModeSet bool
	var fastModeNote, fastModeSource string
	requestedFastMode := strings.TrimSpace(body.FastMode)
	if requestedFastMode != "" {
		normalizedFastMode, fastErr := harness.ResolveFastModeFlag(h, requestedFastMode)
		if fastErr != nil {
			fieldFail = &spawnFailure{Status: http.StatusBadRequest, Kind: "invalid_request", Msg: fastErr.Error()}
		} else {
			requestedFastMode = normalizedFastMode
			fastModeSource = "explicit"
			switch requestedFastMode {
			case harness.FastModeOn:
				fastMode, fastModeSet = true, true
			case harness.FastModeOff:
				fastModeSet = true
			}
		}
	} else {
		var fastModeBools launchBoolResolution
		_, fieldFail = resolveLaunchBoolFields(h, profileTiers, &fastModeBools, launchBoolFastMode)
		fastMode, fastModeSet = fastModeBools.Value[launchBoolFastMode], fastModeBools.Set[launchBoolFastMode]
		fastModeSource, fastModeNote = fastModeBools.Source[launchBoolFastMode], fastModeBools.Note[launchBoolFastMode]
	}
	if fieldFail != nil {
		return fieldFail
	}
	req.fastMode = fastMode
	req.fastModeSet = fastModeSet
	req.fastModeNote = fastModeNote
	req.fastModeSource = fastModeSource
	// The tier that chose each value is kept, not discarded. For the sandbox it
	// is the only party that can say a global/group default profile forced the
	// containment; the badge would otherwise credit "this launch", i.e. the operator.
	req.launchValues = launchValues
	return nil
}

// resolveSandboxImplementation resolves the sandbox implementation through the
// tier stack and refuses one this host or harness posture cannot run.
func (req *spawnRequest) resolveSandboxImplementation() *spawnFailure {
	body := &req.body
	profileTiers := req.profileTiers
	h := req.h
	var fieldFail *spawnFailure
	// The sandbox IMPLEMENTATION rides the same tier stack, but only its HARNESS
	// applicability is validated inside the validator. That placement is the
	// whole design: a lower-tier profile pinned to a different harness is
	// ambient configuration, so it is skipped, disclosed, and falls through —
	// while an explicit request, or a profile that claims this harness, fails
	// loudly. Whether the HOST can run the layer is a separate gate below,
	// because tier fallthrough would turn "no bwrap on this box" into a silent
	// downgrade to harness-builtin. See sandbox_implementation.go.
	var sandboxImplNote string
	var sandboxImplSource string
	body.SandboxImplementation, sandboxImplSource, sandboxImplNote, fieldFail = resolveSandboxImplementationLaunchField(
		h, body.SandboxImplementation, profileTiers)
	if fieldFail != nil {
		return fieldFail
	}
	// Host gate, on the RESOLVED value and whichever tier supplied it. Never
	// falls through; refuses naming the missing capability. Probed live, so an
	// operator who just installed bwrap is not refused by a stale answer.
	if fail := withInstallDefaultRemedy(
		sandboxImplementationHostFailure(h.Name, body.SandboxImplementation), sandboxImplSource); fail != nil {
		return fail
	}
	// Posture gate, beside the host gate and on the same terms: a harness whose
	// own OS sandbox tclaude cannot switch off has to have its configuration
	// verified at every launch, or the recorded single-boundary claim outlives
	// the configuration it was made about.
	if fail := withInstallDefaultRemedy(
		sandboxImplementationPostureFailure(h.Name, body.SandboxImplementation), sandboxImplSource); fail != nil {
		return fail
	}
	req.sandboxImplNote = sandboxImplNote
	req.sandboxImplSource = sandboxImplSource
	return nil
}

// resolveLaunchBools resolves the bool launch fields through the tier stack:
// auto-review, SSH workaround, trust dir, auto-memory, peer messaging, the
// Copilot, pi and Codex drives, and group-context inclusion.
func (req *spawnRequest) resolveLaunchBools() *spawnFailure {
	body := &req.body
	profileTiers := req.profileTiers
	h := req.h
	// The shared bool fields ride launchBoolFieldSpecs, in this boundary's
	// own order (fast mode was resolved above, with its explicit flag parsed).
	var launchBools launchBoolResolution
	launchBools.Value[launchBoolAutoReview], launchBools.Set[launchBoolAutoReview] = body.AutoReview, body.AutoReviewSpecified()
	launchBools.Value[launchBoolSSHWorkaround], launchBools.Set[launchBoolSSHWorkaround] = body.SSHWorkaround != nil && *body.SSHWorkaround, body.SSHWorkaround != nil
	launchBools.Value[launchBoolTrustDir], launchBools.Set[launchBoolTrustDir] = body.TrustDir, body.TrustDirSpecified()
	launchBools.Value[launchBoolCopilotAPI], launchBools.Set[launchBoolCopilotAPI] = body.CopilotAPI != nil && *body.CopilotAPI, body.CopilotAPI != nil
	launchBools.Value[launchBoolCodexAppServer], launchBools.Set[launchBoolCodexAppServer] = body.CodexAppServer != nil && *body.CodexAppServer, body.CodexAppServer != nil
	if _, fail := resolveLaunchBoolFields(h, profileTiers, &launchBools,
		launchBoolAutoReview, launchBoolSSHWorkaround, launchBoolTrustDir); fail != nil {
		return fail
	}
	body.AutoReview = launchBools.Value[launchBoolAutoReview]
	sshWorkaround, sshWorkaroundSet := launchBools.Value[launchBoolSSHWorkaround], launchBools.Set[launchBoolSSHWorkaround]
	if !sshWorkaroundSet {
		sshWorkaround, _ = harness.ResolveSSHWorkaround(h, nil)
	}
	body.TrustDir = launchBools.Value[launchBoolTrustDir]
	// auto_memory and peer_messaging ride the shared table (TCL-1107). Unset
	// everywhere resolves to FALSE — see launchBoolFieldSpecs for why the
	// load-bearing default is off for both.
	launchBools.Value[launchBoolAutoMemory], launchBools.Set[launchBoolAutoMemory] = body.AutoMemory != nil && *body.AutoMemory, body.AutoMemory != nil
	launchBools.Value[launchBoolPeerMessaging], launchBools.Set[launchBoolPeerMessaging] = body.PeerMessaging != nil && *body.PeerMessaging, body.PeerMessaging != nil
	if _, fail := resolveLaunchBoolFields(h, profileTiers, &launchBools, launchBoolAutoMemory, launchBoolPeerMessaging); fail != nil {
		return fail
	}
	// The Copilot drive rides the same tier stack, with the same fallback the
	// send-keys path has always had: unset everywhere is FALSE. Only an explicit
	// opt-in (spawn body or a matching Copilot profile) selects the API drive, so
	// a launch nobody steered is byte-for-byte the launch it was before this
	// field existed. See TCL-1053.
	if _, fail := resolveLaunchBoolFields(h, profileTiers, &launchBools, launchBoolCopilotAPI); fail != nil {
		return fail
	}
	// The pi extension drive is an explicit per-launch opt-in, never a profile
	// default: it changes how every delivery reaches the agent, so only the
	// spawn that asks for it gets it.
	piDrive := body.PiDrive != nil && *body.PiDrive
	if piDrive && h.Name != harness.PiName {
		return &spawnFailure{http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("pi_drive is a pi-only drive; harness %q has none", h.Name)}
	}
	if _, fail := resolveLaunchBoolFields(h, profileTiers, &launchBools, launchBoolCodexAppServer); fail != nil {
		return fail
	}
	// Group-context inclusion rides the same tier stack, minus the harness gate:
	// it decides what the new agent is TOLD, not how its harness runs, so a
	// profile authored for another vendor still speaks (harnessAgnosticLaunchField).
	// Resolving it here rather than only client-side is what lets a caller that
	// merely NAMES a profile honour its toggle — the agentd TUI does exactly that,
	// while `tclaude agent spawn` and the dashboard both send an explicit flag.
	// Unset at every tier keeps the long-standing default: include.
	launchBools.Value[launchBoolIncludeGroupContext] = body.IncludeGroupContext != nil && *body.IncludeGroupContext
	launchBools.Set[launchBoolIncludeGroupContext] = body.IncludeGroupContext != nil
	if _, fail := resolveLaunchBoolFields(h, profileTiers, &launchBools, launchBoolIncludeGroupContext); fail != nil {
		return fail
	}
	includeGroupContext := launchBools.Value[launchBoolIncludeGroupContext]
	includeGroupContextSet := launchBools.Set[launchBoolIncludeGroupContext]
	includeGroupContextSource := launchBools.Source[launchBoolIncludeGroupContext]
	if !includeGroupContextSet {
		includeGroupContext = true
	}
	if body.NonInteractive {
		includeGroupContext = body.IncludeGroupContext != nil && *body.IncludeGroupContext
	}
	// Disclose a tier nobody typed at this launch. A DEFAULT profile silently
	// withholding the group's shared guidance is exactly the action-at-a-distance
	// the launch echo exists to surface: the operator sees WHICH tier decided,
	// not just an agent that mysteriously arrived unbriefed. The resolver's own
	// note channel stays silent for this field — it only speaks on the harness
	// mismatch this field skips — so the disclosure is built here.
	includeGroupContextNote := ""
	if includeGroupContextSet && includeGroupContextSource != agent.ProvExplicit {
		includeGroupContextNote = fmt.Sprintf("%s include_group_context = %v",
			includeGroupContextSource, includeGroupContext)
	}
	req.launchBools = launchBools
	req.sshWorkaround = sshWorkaround
	req.piDrive = piDrive
	req.includeGroupContext = includeGroupContext
	req.includeGroupContextNote = includeGroupContextNote
	return nil
}

// resolveIdentity resolves who the new agent is: its name, roles, description,
// saved prompts, task brief, auto-focus and birth-time access controls.
func (req *spawnRequest) resolveIdentity(r *http.Request) *spawnFailure {
	body := &req.body
	spawnerConvID := req.spawnerConvID
	permOverrides := req.permOverrides
	g := req.g
	profileTiers := req.profileTiers
	h := req.h
	var fieldFail *spawnFailure
	// Identity, the auto-focus toggle and the birth-time access controls ride
	// the same tier stack, and for the same reason group-context inclusion does:
	// a caller that merely NAMES a profile must get what that profile says. The
	// agentd TUI's spawn form is exactly such a caller — it has boxes for the
	// name and the brief only, so role, descr, auto-focus, the owner flag and
	// the permission overrides reached the daemon unspoken and were dropped.
	//
	// PRESENCE on the wire, not a non-empty value, is what counts as the caller
	// speaking (see resolveIdentityLaunchField). That is what keeps a dashboard
	// operator who clears a profile-prefilled Role from having it restored.
	var identityNotes []string
	var profileNameNote string
	body.Name, _, profileNameNote = resolveIdentityLaunchField(
		nameField, body.Name, body.NameSpecified(), profileTiers,
		func(p *db.SpawnProfile) string { return p.AgentName },
		func(raw string) (string, bool) {
			// A profile's agent_name is held to looser rules than a spawn name
			// (it may carry spaces and punctuation), so put it through the same
			// normalize-then-validate an explicit name took at the boundary. One
			// that still cannot be a spawn name is skipped and disclosed rather
			// than failing a launch nobody typed it into.
			if !isValidSpawnName(raw) {
				if cfg, _ := config.Load(); cfg.SpawnNameNormalizeEnabled() {
					raw = agent.NormalizeSpawnName(raw)
				}
			}
			return raw, raw != "" && isValidSpawnName(raw)
		})
	if profileNameNote != "" {
		identityNotes = append(identityNotes, profileNameNote)
	}
	// Receiver-owned worker defaults override role and launch-profile grants.
	// This server-side snapshot cannot be supplied by a remote requester.
	federationJob := r.Context().Value(federationJobContextKey{}) != nil
	var workerDefaults map[string]db.PermissionOverride
	if reservedID, ok := r.Context().Value(reservedAgentIDContextKey{}).(string); ok {
		defaults, err := db.GetFederationWorkerDefaults(reservedID)
		if err != nil {
			return &spawnFailure{500, "worker_defaults", err.Error()}
		}
		if defaults != nil && len(defaults.Permissions) > 0 {
			if body.NonInteractive && !federationJob {
				return &spawnFailure{400, "worker_defaults", "profile worker defaults require an enrolled agent"}
			}
			workerDefaults = defaults.Permissions
		}
	}
	// Roles, role label, description, saved prompts, startup context and the
	// birth-time access controls, with their access gate: the same resolution
	// a trigger, schedule or process worker gets (resolveLaunchIdentity).
	identity, identityFail := resolveLaunchIdentity(launchIdentityRequest{
		body: *body, overrides: permOverrides, group: g, harness: h.Name, tiers: profileTiers,
		spawner: spawnerConvID, request: r, oneShot: body.NonInteractive && !federationJob,
		nonInteractive: body.NonInteractive, workerDefaults: workerDefaults})
	if identityFail != nil {
		return identityFail
	}
	identityNotes = append(identityNotes, identity.notes...)
	body.RoleRefs = identity.roleRefs
	if body.RoleRefs == nil {
		body.RoleRefs = []string{}
	}
	body.RoleRef = ""
	if len(body.RoleRefs) > 0 {
		body.RoleRef = body.RoleRefs[0]
	}
	body.Role, body.Descr = identity.role, identity.descr
	// A profile's initial_message is a replaceable task default: it fills the
	// brief only when the caller sent none. Durable per-profile guidance is
	// startup_context, resolved separately above and never overridden by a task.
	body.InitialMessage, _, _ = resolveIdentityLaunchField(
		initialMessageField, body.InitialMessage, body.InitialMessageSpecified(), profileTiers,
		func(p *db.SpawnProfile) string { return p.InitialMessage }, nil)
	promptRefs := identity.promptRefs
	if len(promptRefs) > 0 {
		composed, fail := composeSavedPrompts(promptRefs, body.InitialMessage)
		if fail != nil {
			return fail
		}
		body.InitialMessage = composed
	}
	body.PromptRefs = promptRefs
	if h.Name == harness.ShellName && !body.NonInteractive && body.InitialMessage != "" {
		return &spawnFailure{http.StatusBadRequest, "invalid_initial_message",
			"shell spawn requires non_interactive when initial_message is set"}
	}
	// Name an otherwise unnamed group spawn before building spawnParams and its
	// durable/audit snapshots. executeSpawn retains the same fallback for
	// non-HTTP adapters, but the shared HTTP path must record the name it
	// actually launches rather than an empty pre-derivation value.
	if strings.TrimSpace(body.Name) == "" {
		body.Name = derivedGroupSpawnName(g.Name, time.Now(), randomLabelToken())
	}
	// No disclosure note for auto_focus: the resolver's note channel only speaks
	// on the harness mismatch this field skips, and a terminal window opening is
	// its own announcement.
	body.AutoFocus, _, _, _, fieldFail = resolveBoolLaunchField(
		autoFocusField, body.AutoFocus, body.AutoFocusSpecified(), h.Name, profileTiers,
		func(p *db.SpawnProfile) *bool { return p.AutoFocus },
		func(v bool) (bool, error) { return v, nil })
	if fieldFail != nil {
		return fieldFail
	}
	// Fold the access decisions back onto the working body for subsequent
	// resolution. The original request was captured before this mutation;
	// resolved launch state is persisted separately.
	body.IsOwner, body.PermissionOverrides = identity.isOwner, identity.overrides
	req.identityNotes = identityNotes
	req.identity = identity
	return nil
}

// resolveContextFeatures resolves the startup-context trims and the profile
// guidance block.
func (req *spawnRequest) resolveContextFeatures() *spawnFailure {
	body := &req.body
	profileTiers := req.profileTiers
	h := req.h
	identity := req.identity
	// Startup-context trims ride the tier stack whole rather than merging — see
	// resolveContextFeaturesLaunchField. Unset everywhere means "trim nothing",
	// so an agent's startup context only shrinks when someone asked for it.
	requestedFeatures := map[string]string{}
	if body.ContextFeatures != nil {
		requestedFeatures = *body.ContextFeatures
	}
	contextFeatures, cfNote, cfFail := resolveContextFeaturesLaunchField(
		requestedFeatures, body.ContextFeatures != nil, h, profileTiers)
	if cfFail != nil {
		return cfFail
	}
	contextFeaturesNote := cfNote
	// Profile guidance resolves as one whole text block from the highest
	// compatible tier. It has no per-spawn override: unlike initial_message this
	// is policy attached to the selected model/profile, not a task default.
	profileContext, profileContextNote := identity.startupContext, identity.startupContextNote
	req.contextFeatures = contextFeatures
	req.profileContext = profileContext
	req.profileContextNote = profileContextNote
	req.contextFeaturesNote = contextFeaturesNote
	return nil
}

// buildLaunchEcho assembles the resolved-launch echo: each field's value and
// the tier that chose it, plus every disclosure note.
func (req *spawnRequest) buildLaunchEcho() {
	body := &req.body
	relaunchTiers := req.relaunchTiers
	relaunchExplicit := req.relaunchExplicit
	harnessSource := req.harnessSource
	h := req.h
	modelProxyNote := req.launchValues.Note[launchValueModelProxy]
	modelSource := req.launchValues.Source[launchValueModel]
	modelNote := req.launchValues.Note[launchValueModel]
	effortSource := req.launchValues.Source[launchValueEffort]
	effortNote := req.launchValues.Note[launchValueEffort]
	sandboxNote := req.launchValues.Note[launchValueSandbox]
	approvalNote := req.launchValues.Note[launchValueApproval]
	toolsNote := req.launchValues.Note[launchValueTools]
	askTimeoutNote := req.launchValues.Note[launchValueAskUserQuestionTimeout]
	autoCompactWindowNote := req.launchValues.Note[launchValueAutoCompactWindow]
	contextWindowMaxSource := req.launchValues.Source[launchValueContextWindowMax]
	contextWindowMaxNote := req.launchValues.Note[launchValueContextWindowMax]
	fastMode := req.fastMode
	fastModeSet := req.fastModeSet
	fastModeNote := req.fastModeNote
	fastModeSource := req.fastModeSource
	sandboxImplNote := req.sandboxImplNote
	sandboxImplSource := req.sandboxImplSource
	autoMemoryNote := req.launchBools.Note[launchBoolAutoMemory]
	peerMessagingNote := req.launchBools.Note[launchBoolPeerMessaging]
	contextFeaturesNote := req.contextFeaturesNote
	autoReviewNote := req.launchBools.Note[launchBoolAutoReview]
	sshWorkaroundNote := req.launchBools.Note[launchBoolSSHWorkaround]
	trustDirNote := req.launchBools.Note[launchBoolTrustDir]
	copilotAPI := req.launchBools.Value[launchBoolCopilotAPI]
	copilotAPISource := req.launchBools.Source[launchBoolCopilotAPI]
	copilotAPINote := req.launchBools.Note[launchBoolCopilotAPI]
	codexAppServer := req.launchBools.Value[launchBoolCodexAppServer]
	codexAppServerSource := req.launchBools.Source[launchBoolCodexAppServer]
	codexAppServerNote := req.launchBools.Note[launchBoolCodexAppServer]
	includeGroupContextNote := req.includeGroupContextNote
	identityNotes := req.identityNotes
	profileContextNote := req.profileContextNote
	contextWindowMaxValue := ""
	if body.ContextWindowMax > 0 {
		contextWindowMaxValue = strconv.FormatInt(body.ContextWindowMax, 10)
	}
	copilotAPIValue := ""
	if copilotAPI {
		copilotAPIValue = "api"
	}
	codexAppServerValue := ""
	if h.Name == harness.CodexName {
		codexAppServerValue = "send-keys"
		if codexAppServer {
			codexAppServerValue = "app-server"
		}
	}
	fastModeValue := ""
	if fastModeSet {
		fastModeValue = harness.FastModeOff
		if fastMode {
			fastModeValue = harness.FastModeOn
		}
	}
	resolvedLaunch := &agent.ResolvedLaunch{
		Harness: agent.ResolvedField{Value: h.Name, Source: harnessSource},
		Model:   agent.ResolvedField{Value: body.Model, Source: modelSource, Note: modelNote},
		Effort:  agent.ResolvedField{Value: body.Effort, Source: effortSource, Note: effortNote},
		ContextWindowMax: agent.ResolvedField{
			Value: contextWindowMaxValue, Source: contextWindowMaxSource,
		},
		// Named for what it selects, not for the flag that selects it: "api" reads
		// correctly whether or not send-keys is still the other option.
		CopilotAPI:     agent.ResolvedField{Value: copilotAPIValue, Source: copilotAPISource},
		CodexAppServer: agent.ResolvedField{Value: codexAppServerValue, Source: codexAppServerSource},
		FastMode:       agent.ResolvedField{Value: fastModeValue, Source: fastModeSource},
	}
	// A one-shot is never resumed and records no rows, so it shows none.
	if !body.NonInteractive {
		resolvedLaunch.RelaunchPolicy = resolveSpawnRelaunchRows(relaunchExplicit, relaunchTiers)
	}
	resolvedLaunch.SandboxImpl = agent.ResolvedField{
		Value: body.SandboxImplementation, Source: sandboxImplSource, Note: sandboxImplNote}
	// The note rides the echoed field, like Harness/Model/Effort. It ALSO enters
	// Notes only when the value is blank, because the echo suppresses the
	// "Sandbox impl:" line for a default-off launch — and a disclosure with no
	// line to sit on would otherwise vanish. Adding it unconditionally would
	// print it twice whenever a later tier did supply a value.
	if body.SandboxImplementation == "" && sandboxImplNote != "" {
		resolvedLaunch.Notes = append(resolvedLaunch.Notes, sandboxImplNote)
	}
	for _, note := range append([]string{modelProxyNote, sandboxNote, approvalNote, toolsNote, askTimeoutNote, autoCompactWindowNote, contextWindowMaxNote, copilotAPINote, codexAppServerNote, fastModeNote, autoReviewNote, trustDirNote, autoMemoryNote, peerMessagingNote, sshWorkaroundNote, contextFeaturesNote, profileContextNote, includeGroupContextNote}, identityNotes...) {
		if note != "" {
			resolvedLaunch.Notes = append(resolvedLaunch.Notes, note)
		}
	}
	req.resolvedLaunch = resolvedLaunch
}

// resolveHarnessModes resolves the harness-native modes on the resolved fields:
// sandbox mode, approval policy, tool governance and auto-review, warns about an
// autonomous-but-unconfined launch, and refuses a Codex cwd that would expose
// tclaude's own state.
func (req *spawnRequest) resolveHarnessModes() *spawnFailure {
	var fieldFail *spawnFailure
	body := &req.body
	cwd := req.cwd
	h := req.h
	resolvedLaunch := req.resolvedLaunch
	spawnerConvID := req.spawnerConvID
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
		return &spawnFailure{http.StatusBadRequest, "invalid_sandbox", sbErr.Error()}
	}
	harnessBuiltinMode, fieldFail = resolveSandboxImplementationMode(
		h, harnessBuiltinMode, body.SandboxImplementation)
	if fieldFail != nil {
		return fieldFail
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
		return &spawnFailure{http.StatusBadRequest, "invalid_approval", apErr.Error()}
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
		return &spawnFailure{http.StatusBadRequest, "invalid_tools", tgErr.Error()}
	}
	if h.Name == harness.OpenCodeName {
		// Refused here rather than at launch so the preview says so too.
		if err := harness.ValidateOpenCodeToolsCombination(
			harnessBuiltinMode, approvalPolicy, toolGovernance); err != nil {
			return &spawnFailure{http.StatusUnprocessableEntity, "invalid_tools", err.Error()}
		}
		// Said on every OpenCode launch, so an agent that never delegates (or
		// one that runs tools tclaude never audited) is not a mystery.
		note := harness.OpenCodeSubagentsOffNote
		if policy, _ := harness.ParseOpenCodeToolGovernance(toolGovernance); policy.AllTools() {
			note = harness.OpenCodeAllToolsNote
		}
		resolvedLaunch.Notes = append(resolvedLaunch.Notes, note)
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
		return &spawnFailure{http.StatusBadRequest, "invalid_auto_review", arErr.Error()}
	}
	if body.NonInteractive {
		autoReview = false
	}
	if home, herr := os.UserHomeDir(); herr == nil && harness.CodexSandboxCwdConflict(harnessBuiltinMode, cwd, home) {
		return &spawnFailure{http.StatusBadRequest, "invalid_cwd", fmt.Sprintf(
			"refusing to spawn a %s agent in %q under sandbox %q: it would expose "+
				"~/.tclaude / ~/.codex / ~/.claude to the agent's writes; spawn in a "+
				"project subdirectory or set sandbox %q to opt out",
			h.Name, cwd, harnessBuiltinMode, harness.SandboxDangerFull)}
	}
	req.effort = effort
	req.model = model
	req.harnessBuiltinMode = harnessBuiltinMode
	req.approvalPolicy = approvalPolicy
	req.toolGovernance = toolGovernance
	req.autoReview = autoReview
	return nil
}

// checkSandboxLineage refuses a child looser than its caller (sandbox mode and
// approval lineage), a sandbox-profile selection the launch mode would drop or
// not enforce, and an external steward's worker that could act unasked.
func (req *spawnRequest) checkSandboxLineage() *spawnFailure {
	body := &req.body
	approvalPolicy := req.approvalPolicy
	autoReview := req.autoReview
	externalSpawn := req.externalSpawn
	h := req.h
	harnessBuiltinMode := req.harnessBuiltinMode
	sandboxProfileForScope := req.sandboxProfileForScope
	sandboxScopePinned := req.sandboxScopePinned
	spawnerConvID := req.spawnerConvID
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
		return fieldFail
	}
	if fail := spawnSandboxLineageFailure(
		spawnerConvID, h.Name, childLaunchSandbox, body.SandboxImplementation); fail != nil {
		return fail
	}
	if fail := spawnApprovalLineageFailure(spawnerConvID, h.Name, approvalPolicy, autoReview); fail != nil {
		return fail
	}

	// danger-full-access is Codex's explicit raw no-sandbox launch. It cannot
	// carry the managed permission profile that represents tclaude filesystem
	// policy, so omit every sandbox-profile tier (global, group, and explicit)
	// instead of resolving a policy that must fail capability validation later.
	// The dashboard mirrors this by forcing its selector to the visible "none"
	// state; this server-side rule also covers CLI callers and older tabs.
	if spawnerConvID != "" && body.SandboxProfile != "" &&
		sandboxProfilesDisabled(h.Name, harnessBuiltinMode, body.SandboxImplementation) {
		return &spawnFailure{http.StatusForbidden, "sandbox_profile_restricted",
			"the resolved launch mode omits sandbox profiles and cannot satisfy a named sandbox-profile selection"}
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
		return &spawnFailure{http.StatusForbidden, "sandbox_profile_restricted",
			fmt.Sprintf("this spawn is authorized by a grant scoped to sandbox profile %q, "+
				"and the resolved launch mode would not enforce it", sandboxProfileForScope)}
	}
	if externalSpawn && !externalStewardApprovalHolds(h.Name, childLaunchSandbox) {
		return &spawnFailure{http.StatusForbidden, externalStewardCeilingCode,
			"this launch can run commands without asking a human: Claude's own sandbox does, and so does OpenCode's bash; an external steward's worker can't use either"}
	}

	return nil
}

// resolveSandboxPolicy resolves the effective sandbox snapshot: the profile
// chain, harness config, containment in the caller's own sandbox, the launch
// environment, capability and access checks, and the SSH workaround.
func (req *spawnRequest) resolveSandboxPolicy() *spawnFailure {
	g := req.g
	body := &req.body
	copilotAPI := req.launchBools.Value[launchBoolCopilotAPI]
	cwd := req.cwd
	externalSpawn := req.externalSpawn
	h := req.h
	harnessBuiltinMode := req.harnessBuiltinMode
	profileTiers := req.profileTiers
	resolvedLaunch := req.resolvedLaunch
	spawnerConvID := req.spawnerConvID
	sshWorkaround := req.sshWorkaround
	effectiveSandbox, fail := resolveChildSandbox(childSandboxInput{
		group:               g,
		harnessName:         h.Name,
		harnessBuiltinMode:  harnessBuiltinMode,
		implementation:      body.SandboxImplementation,
		sandboxProfile:      body.SandboxProfile,
		omitSandboxProfiles: body.OmitSandboxProfiles,
		harnessConfig:       body.HarnessConfig,
		spawnerConvID:       spawnerConvID,
		externalSpawn:       externalSpawn,
	})
	if fail != nil {
		return fail
	}
	launchEnvironment, launchEnvironmentOverrides, envErr := resolveCommonLaunchEnvironmentParts(g, profileTiers, body.Environment)
	if envErr != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_environment", envErr.Error()}
	}
	if _, envErr := sandboxpolicy.MergeEnvironment(effectiveSandbox.Effective.Environment, launchEnvironment); envErr != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_environment", envErr.Error()}
	}
	effectiveSandbox.LaunchEnvironment = launchEnvironment
	effectiveSandbox.RefreshGroupEnvironment = true
	effectiveSandbox.LaunchEnvironmentOverrides = launchEnvironmentOverrides
	effectiveSandbox.ResolutionGroupID = g.ID
	if fail := sandboxProfileCapabilityFailure(
		h.Name, harnessBuiltinMode, &effectiveSandbox, body.SandboxImplementation); fail != nil {
		return fail
	}
	if fail := copilotAPILoopbackFailure(
		copilotAPI, &effectiveSandbox, body.SandboxImplementation); fail != nil {
		return fail
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
		return fail
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
	req.effectiveSandbox = effectiveSandbox
	req.sshWorkaroundIntent = sshWorkaroundIntent
	return nil
}

// proveLaunchDirs makes an agent caller prove it can write every directory the
// child would get write access to, and pins those directories to the paths the
// proof was verified in.
func (req *spawnRequest) proveLaunchDirs(w http.ResponseWriter, r *http.Request) *spawnFailure {
	body := &req.body
	cwd := req.cwd
	externalSpawn := req.externalSpawn
	h := req.h
	harnessBuiltinMode := req.harnessBuiltinMode
	spawnerConvID := req.spawnerConvID
	worktreePath := req.worktreePath
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
			return &spawnFailure{http.StatusInternalServerError, "io", gerr.Error()}
		}
		if home, herr := os.UserHomeDir(); herr == nil {
			gitWorktreeWriteDirs = harness.GitWorktreeWriteDirs(cwd, codexGitCommonDir, home)
		}
	}
	autoTrustSiblingWorktree, trustLayoutErr := defaultSiblingWorktreeTrust(h.Name, cwd, codexGitCommonDir)
	if trustLayoutErr != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", trustLayoutErr.Error()}
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
			return errSpawnResponseWritten
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
	req.proofDirs = proofDirs
	req.proofToken = proofToken
	req.codexGitCommonDir = codexGitCommonDir
	req.codexGitCommonDirPinned = codexGitCommonDirPinned
	req.gitWorktreeWriteDirs = gitWorktreeWriteDirs
	req.autoTrustSiblingWorktree = autoTrustSiblingWorktree
	req.cwd = cwd
	req.worktreePath = worktreePath
	return nil
}

// resolveHarnessOptions resolves the AskUserQuestion timeout, the
// auto-compaction window and directory trust for the harness, and refuses a
// trust request the caller has not earned.
func (req *spawnRequest) resolveHarnessOptions() *spawnFailure {
	body := &req.body
	autoTrustSiblingWorktree := req.autoTrustSiblingWorktree
	cwd := req.cwd
	h := req.h
	proofDirs := req.proofDirs
	proofToken := req.proofToken
	spawnerConvID := req.spawnerConvID
	// Resolve the AskUserQuestion idle-timeout for the chosen harness: a
	// Claude-Code-only settings.json override (never|60s|5m|10m) delivered via
	// `--settings`, so an explicit value for a harness with no AskUserQuestion
	// dialog (Codex) is a 400 here rather than a flag silently dropped. There is
	// no forced default (inherit/blank → "" = no override) — enabling
	// auto-continue for an unattended agent is an explicit per-agent / profile
	// opt-in, already overlaid from the group default profile above.
	askTimeout, atErr := harness.ResolveAskTimeoutMode(h, body.AskUserQuestionTimeout)
	if atErr != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_ask_user_question_timeout", atErr.Error()}
	}

	// Same gate for the auto-compaction window, already normalized and overlaid
	// from the profile tiers above. Re-resolving here keeps this boundary
	// self-sufficient rather than trusting the earlier pass, exactly as the
	// ask-timeout re-resolve does.
	autoCompactWindow, acwErr := harness.ResolveAutoCompactWindow(h, body.AutoCompactWindow)
	if acwErr != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_auto_compact_window", acwErr.Error()}
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
		return &spawnFailure{http.StatusBadRequest, "invalid_trust_dir", tdErr.Error()}
	}
	if autoTrustSiblingWorktree {
		trustDir = true
	}
	if spawnerConvID != "" && trustDir && !autoTrustSiblingWorktree {
		callerOwned, ownErr := callerOwnedDirTrustProved(spawnerConvID, cwd, proofToken, proofDirs)
		if ownErr != nil {
			return &spawnFailure{http.StatusInternalServerError, "io", ownErr.Error()}
		}
		if !callerOwned {
			return &spawnFailure{http.StatusForbidden, "trust_dir_restricted", trustDirRestrictedMessage}
		}
	}
	req.askTimeout = askTimeout
	req.autoCompactWindow = autoCompactWindow
	req.trustDir = trustDir
	return nil
}

// resolveRemoteControl resolves remote control through the request, the group
// policy and the profile tiers, and records the toggles in the launch echo.
func (req *spawnRequest) resolveRemoteControl() *spawnFailure {
	g := req.g
	body := &req.body
	autoMemory := req.launchBools.Value[launchBoolAutoMemory]
	h := req.h
	launchBools := req.launchBools
	peerMessaging := req.launchBools.Value[launchBoolPeerMessaging]
	profileTiers := req.profileTiers
	resolvedLaunch := req.resolvedLaunch
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
		return rcFail
	}
	if body.NonInteractive {
		remoteControl, remoteControlSource = false, provRemoteControlNonInteractive
	}
	resolvedLaunch.RemoteControl = resolvedToggle(remoteControl, remoteControlSource)
	resolvedLaunch.AutoMemory = resolvedToggle(autoMemory, launchBools.Source[launchBoolAutoMemory])
	resolvedLaunch.PeerMessaging = resolvedToggle(peerMessaging, launchBools.Source[launchBoolPeerMessaging])
	req.remoteControl = remoteControl
	return nil
}

// validateTaskRef refuses a malformed task-reference link or label.
func (req *spawnRequest) validateTaskRef() *spawnFailure {
	body := &req.body
	// Validate an explicit task-reference link at the spawn boundary so a
	// bad URL fails the request with a clear 400, rather than being
	// silently dropped when enrollSpawnedConv tries to persist it. Empty
	// means "no link". The scheme check is the same http(s) guard the
	// standalone task endpoints apply — keeps `javascript:`/`data:` URLs
	// out of the dashboard's Task-column href.
	if u := strings.TrimSpace(body.TaskURL); u != "" {
		if err := validateTaskRefURL(u); err != nil {
			return &spawnFailure{http.StatusBadRequest, "invalid_task_url", err.Error()}
		}
		if err := validateTaskRefLabel(strings.TrimSpace(body.TaskLabel)); err != nil {
			return &spawnFailure{http.StatusBadRequest, "invalid_task_label", err.Error()}
		}
	}

	return nil
}

// queueOrClaimRateSlot queues a spawn that does not fit the node's capacity,
// answering with the queued state, or else claims the spawn's rate-limit slot.
func (req *spawnRequest) queueOrClaimRateSlot(w http.ResponseWriter, r *http.Request) *spawnFailure {
	body := &req.body
	g := req.g
	attachments := req.attachments
	cwd := req.cwd
	externalSpawn := req.externalSpawn
	proofDirs := req.proofDirs
	proofToken := req.proofToken
	requestedSpawnConfigJSON := req.requestedSpawnConfigJSON
	spawnSudoGrant := req.spawnSudoGrant
	spawnerConvID := req.spawnerConvID
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
		return errSpawnResponseWritten
	}
	if remotePrincipal, _ := r.Context().Value(federationSpawnRateKey{}).(string); remotePrincipal != "" {
		if fail := claimDaemonSpawnRateSlot(remotePrincipal); fail != nil {
			return fail
		}
	} else if spawnQueueReplayFrom(r) == nil {
		if fail := claimSpawnRateSlot(spawnerConvID); fail != nil {
			return fail
		}
	}
	return nil
}

// buildSpawnParams assembles the spawnParams executeSpawn launches from: the
// resolved request, the launch authorities the request context carries
// (teleport, permission carry, federation identity), the group context, and the
// audit record of what was resolved.
func (req *spawnRequest) buildSpawnParams(r *http.Request) (*spawnParams, *spawnFailure) {
	body := &req.body
	g := req.g
	approvalPolicy := req.approvalPolicy
	askTimeout := req.askTimeout
	attachments := req.attachments
	autoCompactWindow := req.autoCompactWindow
	autoMemory := req.launchBools.Value[launchBoolAutoMemory]
	autoReview := req.autoReview
	autoReviewSet := req.launchBools.Set[launchBoolAutoReview]
	codexAppServer := req.launchBools.Value[launchBoolCodexAppServer]
	codexAppServerSet := req.launchBools.Set[launchBoolCodexAppServer]
	codexAppServerSource := req.launchBools.Source[launchBoolCodexAppServer]
	codexGitCommonDir := req.codexGitCommonDir
	codexGitCommonDirPinned := req.codexGitCommonDirPinned
	contextFeatures := req.contextFeatures
	contextWindowMaxSource := req.launchValues.Source[launchValueContextWindowMax]
	copilotAPI := req.launchBools.Value[launchBoolCopilotAPI]
	copilotAPISet := req.launchBools.Set[launchBoolCopilotAPI]
	copilotAPISource := req.launchBools.Source[launchBoolCopilotAPI]
	cwd := req.cwd
	effectiveSandbox := req.effectiveSandbox
	effort := req.effort
	effortSource := req.launchValues.Source[launchValueEffort]
	externalSpawn := req.externalSpawn
	fastMode := req.fastMode
	fastModeSet := req.fastModeSet
	fastModeSource := req.fastModeSource
	gitWorktreeWriteDirs := req.gitWorktreeWriteDirs
	globalProfile := req.globalProfile
	groupProfile := req.groupProfile
	h := req.h
	harnessBuiltinMode := req.harnessBuiltinMode
	harnessSource := req.harnessSource
	identity := req.identity
	includeGroupContext := req.includeGroupContext
	model := req.model
	modelSource := req.launchValues.Source[launchValueModel]
	namedProfile := req.namedProfile
	namedProfileHandle := req.namedProfileHandle
	peerMessaging := req.launchBools.Value[launchBoolPeerMessaging]
	piDrive := req.piDrive
	profileContext := req.profileContext
	proofDirs := req.proofDirs
	proofToken := req.proofToken
	remoteControl := req.remoteControl
	replyToConv := req.replyToConv
	requestedSpawnConfigJSON := req.requestedSpawnConfigJSON
	resolvedLaunch := req.resolvedLaunch
	sandboxImplSource := req.sandboxImplSource
	sandboxSource := req.launchValues.Source[launchValueSandbox]
	spawnerConvID := req.spawnerConvID
	sshWorkaroundIntent := req.sshWorkaroundIntent
	sshWorkaroundSource := req.launchBools.Source[launchBoolSSHWorkaround]
	teleportAuthority := req.teleportAuthority
	timeout := req.timeout
	toolGovernance := req.toolGovernance
	trustDir := req.trustDir
	trustDirSet := req.launchBools.Set[launchBoolTrustDir]
	worktreeBranch := req.worktreeBranch
	worktreePath := req.worktreePath
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
			return nil, &spawnFailure{503, "teleport_provenance", "teleport provenance unavailable"}
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
			return nil, &spawnFailure{400, "claude_subscription", "a peer's Claude subscription runs an ordinary Claude Code worker only"}
		}
		if p.ModelProxy != "" && p.ModelProxy != "off" {
			return nil, &spawnFailure{400, "claude_subscription", "a worker on a peer's Claude subscription cannot also use a model gateway"}
		}
		p.ModelProxy = ""
	}
	return &p, nil
}

// serveOneShot runs a non-interactive spawn and answers the request itself: a
// one-shot bypasses executeSpawn, and holds its sudo grant use until the
// response is written.
func (req *spawnRequest) serveOneShot(w http.ResponseWriter, r *http.Request, p *spawnParams) {
	body := &req.body
	g := req.g
	oneShotName := req.oneShotName
	spawnSudoGrant := req.spawnSudoGrant
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
	result, runErr := runNonInteractiveSpawn(r.Context(), *p, body.RunTimeoutSeconds)
	if runErr != nil {
		writeError(w, runErr.Status, runErr.Kind, runErr.Msg)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// finishSpawnParams adds what only the interactive launch carries: the usage
// profiles, the spawn-copy tiers, the explicit launch values and the relaunch
// overrides.
func (req *spawnRequest) finishSpawnParams(p *spawnParams) {
	explicitLaunch := req.explicitLaunch
	globalProfile := req.globalProfile
	groupProfile := req.groupProfile
	namedProfile := req.namedProfile
	relaunchExplicit := req.relaunchExplicit
	relaunchTiers := req.relaunchTiers
	spawnUsageProfiles := req.spawnUsageProfiles
	p.UsageProfileIDs, p.UsageSpawnProfile = spawnUsageProfiles, namedProfile
	p.spawnCopyCallerTiers = spawnCopyTiers(spawnCopyTier(db.SpawnCopyTierNamed, namedProfile))
	p.spawnCopyDefaultTiers = spawnCopyTiers(spawnCopyTier(db.SpawnCopyTierGroupDefault, groupProfile),
		spawnCopyTier(db.SpawnCopyTierGlobalDefault, globalProfile))
	p.spawnCopyResolved = true
	p.explicitLaunch = explicitLaunch
	p.relaunchOverrides = resolveRelaunchOverrides(relaunchExplicit, relaunchTiers)
}

// reconcileLaunchEcho corrects the launch echo to what executeSpawn actually
// launched, and records the final resolution in the audit.
func (req *spawnRequest) reconcileLaunchEcho(r *http.Request, p *spawnParams, outcome *spawnOutcome) {
	globalProfile := req.globalProfile
	groupProfile := req.groupProfile
	namedProfile := req.namedProfile
	namedProfileHandle := req.namedProfileHandle
	resolvedLaunch := req.resolvedLaunch
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
	setAuditSpawnResolved(r, spawnAuditResolution(*p, resolvedLaunch, namedProfileHandle, map[string]*db.SpawnProfile{
		"explicit":       namedProfile,
		"group_default":  groupProfile,
		"global_default": globalProfile,
	}))
}

// spawnResponse is the success body: where the agent landed, how to attach,
// its stable id, the resolved launch, and the task-link and focus state.
func (req *spawnRequest) spawnResponse(p *spawnParams, outcome *spawnOutcome) map[string]any {
	g := req.g
	resolvedLaunch := req.resolvedLaunch
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
			resp["focus_ws"] = spawnFocusWSPath(outcome.Label, aid)
		}
	}
	return resp
}
