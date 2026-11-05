package agentd

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/launchspec"
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
	modelProxyNote         string
	modelSource            string
	modelNote              string
	effortSource           string
	effortNote             string
	sandboxSource          string
	sandboxNote            string
	approvalNote           string
	toolsNote              string
	askTimeoutNote         string
	autoCompactWindowNote  string
	contextWindowMaxSource string
	contextWindowMaxNote   string
	fastMode               bool
	fastModeSet            bool
	fastModeNote           string
	fastModeSource         string

	// Set by resolveSandboxImplementation.
	sandboxImplNote   string
	sandboxImplSource string

	// Set by resolveLaunchBools.
	autoMemoryNote          string
	peerMessagingNote       string
	launchBools             launchBoolResolution
	autoReviewSet           bool
	autoReviewNote          string
	sshWorkaround           bool
	sshWorkaroundSource     string
	sshWorkaroundNote       string
	trustDirSet             bool
	trustDirNote            string
	autoMemory              bool
	peerMessaging           bool
	copilotAPI              bool
	copilotAPISet           bool
	copilotAPISource        string
	copilotAPINote          string
	piDrive                 bool
	codexAppServer          bool
	codexAppServerSet       bool
	codexAppServerSource    string
	codexAppServerNote      string
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
	modelProxyNote := launchValues.Note[launchValueModelProxy]
	modelSource, modelNote := launchValues.Source[launchValueModel], launchValues.Note[launchValueModel]
	effortSource, effortNote := launchValues.Source[launchValueEffort], launchValues.Note[launchValueEffort]
	// The tier that chose the sandbox is kept, not discarded: it is the only
	// party that can say a global/group default profile forced the containment,
	// and the badge would otherwise credit "this launch" — i.e. the operator.
	sandboxSource, sandboxNote := launchValues.Source[launchValueSandbox], launchValues.Note[launchValueSandbox]
	approvalNote := launchValues.Note[launchValueApproval]
	toolsNote := launchValues.Note[launchValueTools]
	askTimeoutNote := launchValues.Note[launchValueAskUserQuestionTimeout]
	autoCompactWindowNote := launchValues.Note[launchValueAutoCompactWindow]
	contextWindowMaxSource := launchValues.Source[launchValueContextWindowMax]
	contextWindowMaxNote := launchValues.Note[launchValueContextWindowMax]
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
	req.modelProxyNote = modelProxyNote
	req.modelSource = modelSource
	req.modelNote = modelNote
	req.effortSource = effortSource
	req.effortNote = effortNote
	req.sandboxSource = sandboxSource
	req.sandboxNote = sandboxNote
	req.approvalNote = approvalNote
	req.toolsNote = toolsNote
	req.askTimeoutNote = askTimeoutNote
	req.autoCompactWindowNote = autoCompactWindowNote
	req.contextWindowMaxSource = contextWindowMaxSource
	req.contextWindowMaxNote = contextWindowMaxNote
	req.fastMode = fastMode
	req.fastModeSet = fastModeSet
	req.fastModeNote = fastModeNote
	req.fastModeSource = fastModeSource
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
	var autoMemoryNote, peerMessagingNote string
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
	autoReviewSet := launchBools.Set[launchBoolAutoReview]
	autoReviewNote := launchBools.Note[launchBoolAutoReview]
	sshWorkaround, sshWorkaroundSet := launchBools.Value[launchBoolSSHWorkaround], launchBools.Set[launchBoolSSHWorkaround]
	sshWorkaroundSource, sshWorkaroundNote := launchBools.Source[launchBoolSSHWorkaround], launchBools.Note[launchBoolSSHWorkaround]
	if !sshWorkaroundSet {
		sshWorkaround, _ = harness.ResolveSSHWorkaround(h, nil)
	}
	body.TrustDir = launchBools.Value[launchBoolTrustDir]
	trustDirSet := launchBools.Set[launchBoolTrustDir]
	trustDirNote := launchBools.Note[launchBoolTrustDir]
	// auto_memory and peer_messaging ride the shared table (TCL-1107). Unset
	// everywhere resolves to FALSE — see launchBoolFieldSpecs for why the
	// load-bearing default is off for both.
	launchBools.Value[launchBoolAutoMemory], launchBools.Set[launchBoolAutoMemory] = body.AutoMemory != nil && *body.AutoMemory, body.AutoMemory != nil
	launchBools.Value[launchBoolPeerMessaging], launchBools.Set[launchBoolPeerMessaging] = body.PeerMessaging != nil && *body.PeerMessaging, body.PeerMessaging != nil
	if _, fail := resolveLaunchBoolFields(h, profileTiers, &launchBools, launchBoolAutoMemory, launchBoolPeerMessaging); fail != nil {
		return fail
	}
	autoMemory, autoMemoryNote := launchBools.Value[launchBoolAutoMemory], launchBools.Note[launchBoolAutoMemory]
	peerMessaging, peerMessagingNote := launchBools.Value[launchBoolPeerMessaging], launchBools.Note[launchBoolPeerMessaging]
	// The Copilot drive rides the same tier stack, with the same fallback the
	// send-keys path has always had: unset everywhere is FALSE. Only an explicit
	// opt-in (spawn body or a matching Copilot profile) selects the API drive, so
	// a launch nobody steered is byte-for-byte the launch it was before this
	// field existed. See TCL-1053.
	if _, fail := resolveLaunchBoolFields(h, profileTiers, &launchBools, launchBoolCopilotAPI); fail != nil {
		return fail
	}
	copilotAPI, copilotAPISet := launchBools.Value[launchBoolCopilotAPI], launchBools.Set[launchBoolCopilotAPI]
	copilotAPISource, copilotAPINote := launchBools.Source[launchBoolCopilotAPI], launchBools.Note[launchBoolCopilotAPI]
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
	codexAppServer, codexAppServerSet := launchBools.Value[launchBoolCodexAppServer], launchBools.Set[launchBoolCodexAppServer]
	codexAppServerSource, codexAppServerNote := launchBools.Source[launchBoolCodexAppServer], launchBools.Note[launchBoolCodexAppServer]
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
	req.autoMemoryNote = autoMemoryNote
	req.peerMessagingNote = peerMessagingNote
	req.launchBools = launchBools
	req.autoReviewSet = autoReviewSet
	req.autoReviewNote = autoReviewNote
	req.sshWorkaround = sshWorkaround
	req.sshWorkaroundSource = sshWorkaroundSource
	req.sshWorkaroundNote = sshWorkaroundNote
	req.trustDirSet = trustDirSet
	req.trustDirNote = trustDirNote
	req.autoMemory = autoMemory
	req.peerMessaging = peerMessaging
	req.copilotAPI = copilotAPI
	req.copilotAPISet = copilotAPISet
	req.copilotAPISource = copilotAPISource
	req.copilotAPINote = copilotAPINote
	req.piDrive = piDrive
	req.codexAppServer = codexAppServer
	req.codexAppServerSet = codexAppServerSet
	req.codexAppServerSource = codexAppServerSource
	req.codexAppServerNote = codexAppServerNote
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
	modelProxyNote := req.modelProxyNote
	modelSource := req.modelSource
	modelNote := req.modelNote
	effortSource := req.effortSource
	effortNote := req.effortNote
	sandboxNote := req.sandboxNote
	approvalNote := req.approvalNote
	toolsNote := req.toolsNote
	askTimeoutNote := req.askTimeoutNote
	autoCompactWindowNote := req.autoCompactWindowNote
	contextWindowMaxSource := req.contextWindowMaxSource
	contextWindowMaxNote := req.contextWindowMaxNote
	fastMode := req.fastMode
	fastModeSet := req.fastModeSet
	fastModeNote := req.fastModeNote
	fastModeSource := req.fastModeSource
	sandboxImplNote := req.sandboxImplNote
	sandboxImplSource := req.sandboxImplSource
	autoMemoryNote := req.autoMemoryNote
	peerMessagingNote := req.peerMessagingNote
	contextFeaturesNote := req.contextFeaturesNote
	autoReviewNote := req.autoReviewNote
	sshWorkaroundNote := req.sshWorkaroundNote
	trustDirNote := req.trustDirNote
	copilotAPI := req.copilotAPI
	copilotAPISource := req.copilotAPISource
	copilotAPINote := req.copilotAPINote
	codexAppServer := req.codexAppServer
	codexAppServerSource := req.codexAppServerSource
	codexAppServerNote := req.codexAppServerNote
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
