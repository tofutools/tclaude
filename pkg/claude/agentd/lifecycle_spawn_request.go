package agentd

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
