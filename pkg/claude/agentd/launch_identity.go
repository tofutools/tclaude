package agentd

import (
	"fmt"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// launchIdentityRequest is what resolveLaunchIdentity resolves a spawn's roles,
// role label, description, saved prompts, startup context and birth-time
// access over. A direct spawn and a trigger, schedule or process worker build
// one each and get the same answer from the same tiers (tcl-a2e791, Q12).
type launchIdentityRequest struct {
	// body carries the explicit fields, keyed on presence: role_refs/role_ref,
	// role, descr, prompt_refs, is_owner and permission_overrides.
	body agent.SpawnRequest
	// overrides is the explicit permission-override map, already normalized.
	overrides map[string]db.PermissionOverride
	group     *db.AgentGroup
	harness   string
	tiers     []launchProfileTier
	// spawner is the agent the birth-time access is checked against. "" is the
	// human or an operator-authored automation, which passes.
	spawner string
	// oneShot is a non-interactive spawn with no enrolled identity: it takes no
	// roles, owner flag or grants.
	oneShot bool
	// nonInteractive takes only the saved prompts the caller named.
	nonInteractive bool
	// workerDefaults are receiver-owned federation worker grants, layered over
	// the role and profile grants before the access gate.
	workerDefaults map[string]db.PermissionOverride
}

type launchIdentity struct {
	roles      []*db.Role
	roleRefs   []string
	role       string
	descr      string
	promptRefs []string
	// startupContext is the profile guidance with each role's brief appended.
	startupContext     string
	startupContextNote string
	isOwner            bool
	overrides          map[string]db.PermissionOverride
	// notes disclose every access decision a tier other than the request made,
	// including a default-tier value the spawner could not confer.
	notes []string
}

// launchNotes is every disclosure the resolution made, for a launch echo.
func (id launchIdentity) launchNotes() []string {
	if id.startupContextNote == "" {
		return id.notes
	}
	return append([]string{id.startupContextNote}, id.notes...)
}

// resolveLaunchIdentity is the one resolution of a spawn's identity and
// birth-time access. Each field takes the highest tier that speaks: the
// request, the named profile, the group's default profile, the global
// default profile. Role grants are the lowest access tier, beneath the
// winning profile's permission overrides.
//
// The access gate runs on the RESOLVED values. A spawner must hold the slug
// the dedicated endpoints require: groups.owners.manage to mint an owner and
// permissions.grant to set overrides, and it may never confer a grant wider
// than its own (checkGrantAttenuation). Group ownership is deliberately not
// enough: owner-state confers only the owner-implied lifecycle slugs, so
// keying on it would let an owner mint a child holding permissions.grant.
//
// Direct intent, an explicit field or a profile the spawner NAMED, is refused
// loudly. A group or global DEFAULT profile is ambient configuration nobody
// typed at this launch, so a value from it the spawner cannot confer is
// skipped and disclosed: an operator's house default must not start refusing
// every spawn its own agents and automations make.
func resolveLaunchIdentity(in launchIdentityRequest) (launchIdentity, *spawnFailure) {
	var out launchIdentity
	tiers := in.tiers
	roleRefs, roleRefsSource := resolveRoleRefsLaunchField(in.body, tiers)
	if in.oneShot {
		roleRefs = nil
	}
	for _, ref := range roleRefs {
		role, err := db.GetRole(ref)
		if err != nil {
			return out, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if role == nil {
			return out, &spawnFailure{http.StatusBadRequest, "invalid_role",
				fmt.Sprintf("role %q does not name a role in the role library", ref)}
		}
		out.roles = append(out.roles, role)
		out.roleRefs = append(out.roleRefs, role.Name)
	}
	var roleSource string
	out.role, roleSource, _ = resolveIdentityLaunchField(
		roleField, in.body.Role, in.body.RoleSpecified(), tiers,
		func(p *db.SpawnProfile) string { return p.Role }, nil)
	if out.role == "" && roleSource == "" && len(out.roles) > 0 {
		out.role = out.roles[0].Name
	}
	out.descr, _, _ = resolveIdentityLaunchField(
		descrField, in.body.Descr, in.body.DescrSpecified(), tiers,
		func(p *db.SpawnProfile) string { return p.Descr }, nil)

	// Saved prompts compose ahead of the brief. A one-shot takes only the
	// prompts its caller named: an ambient profile/group default must never
	// rewrite a non-interactive prompt or shell command. An interactive shell
	// takes no brief at all, so a group or profile's prompts must not refuse it.
	out.promptRefs = resolvePromptRefsLaunchField(in.body, in.group, tiers)
	if in.nonInteractive && !in.body.PromptRefsSpecified() {
		out.promptRefs = nil
	}
	if in.harness == harness.ShellName && !in.nonInteractive {
		out.promptRefs = nil
	}

	// Profile guidance resolves as one whole text block from the highest
	// compatible tier; each role's brief follows it.
	out.startupContext, out.startupContextNote = resolveProfileStartupContext(in.harness, tiers)
	for _, role := range out.roles {
		out.startupContext = appendRoleBlock(out.startupContext, role.Brief)
	}

	isOwner, _, isOwnerSource, _, fail := resolveBoolLaunchField(
		isOwnerField, in.body.IsOwner, in.body.IsOwnerSpecified(), in.harness, tiers,
		func(p *db.SpawnProfile) *bool { return p.IsOwner },
		func(v bool) (bool, error) { return v, nil })
	if fail != nil {
		return out, fail
	}
	overrides, overridesSource := resolveOverridesLaunchField(
		in.overrides, in.body.PermissionOverridesSpecified(), tiers)
	if overridesSource != "" && overridesSource != agent.ProvExplicit {
		// The profile write path validated this map, but a slug can be retired
		// between the write and the launch. Re-run the boundary check rather
		// than trusting stored state, and drop-with-disclosure instead of
		// failing a spawn over a profile field nobody typed here.
		normalized, novErr := normalizeSpawnPermissionOverrides(overrides)
		if novErr != "" {
			out.notes = append(out.notes, fmt.Sprintf(
				"%s %s ignored (%s)", overridesSource, permissionOverridesField, novErr))
			overrides, overridesSource = nil, ""
		} else {
			overrides = normalized
		}
	}
	// Role grants form the lowest access tier. A spawn profile or explicit
	// per-spawn override can still narrow or replace an individual slug.
	if len(out.roles) > 0 {
		merged := make(map[string]db.PermissionOverride, len(overrides))
		for _, role := range out.roles {
			for _, grant := range role.Permissions {
				candidate := db.PermissionOverride{Effect: db.PermEffectGrant, Scope: grant.Scope}
				if existing, ok := merged[grant.Slug]; ok && existing != candidate {
					return out, &spawnFailure{http.StatusBadRequest, "role_scope_conflict",
						fmt.Sprintf("roles grant %s with incompatible scopes; make the scopes identical or keep that grant on one role", grant.Slug)}
				}
				merged[grant.Slug] = candidate
			}
		}
		for slug, override := range overrides {
			merged[slug] = override
		}
		// Preserve the strongest intent source for the gate below. An explicitly
		// selected role must not become an ambient grant just because a
		// lower-intent default profile also contributes overrides.
		if len(overrides) == 0 ||
			(!launchTierIsDefault(tiers, roleRefsSource) && launchTierIsDefault(tiers, overridesSource)) {
			overridesSource = roleRefsSource
		}
		overrides = merged
	}
	if len(in.workerDefaults) > 0 {
		if overrides == nil {
			overrides = map[string]db.PermissionOverride{}
		}
		for slug, override := range in.workerDefaults {
			overrides[slug] = override
		}
	}
	if in.oneShot {
		isOwner, overrides = false, nil
	}

	if in.spawner != "" {
		if isOwner && resolvePermission(in.spawner, PermGroupsOwnersManage) != permAllow {
			if !launchTierIsDefault(tiers, isOwnerSource) {
				return out, &spawnFailure{http.StatusForbidden, "forbidden",
					"making the spawned agent a group owner requires the " + PermGroupsOwnersManage + " permission"}
			}
			out.notes = append(out.notes, fmt.Sprintf(
				"%s is_owner ignored (caller lacks %s)", isOwnerSource, PermGroupsOwnersManage))
			isOwner, isOwnerSource = false, ""
		}
		if len(overrides) > 0 && resolvePermission(in.spawner, PermPermissionsGrant) != permAllow {
			if !launchTierIsDefault(tiers, overridesSource) {
				return out, &spawnFailure{http.StatusForbidden, "forbidden",
					"setting the spawned agent's permission overrides requires the " + PermPermissionsGrant + " permission"}
			}
			out.notes = append(out.notes, fmt.Sprintf(
				"%s %s ignored (caller lacks %s)", overridesSource, permissionOverridesField, PermPermissionsGrant))
			overrides, overridesSource = nil, ""
		}
		// Attenuation-only delegation, on the RESOLVED map, so a profile tier
		// cannot smuggle in a grant the request never named. Dropping a default
		// tier's overrides only ever narrows what the child is born with. The
		// child has no stable id yet; its lineage edge is mandatory during
		// enrollment, before these overrides are written, so it is a descendant
		// by construction. See permission_attenuation.go.
		conferee := grantConferee{descendantByConstruction: true}
		if err := checkGrantAttenuation(in.spawner, conferee, conferredGrantsFromOverrides(overrides)); err != nil {
			if !launchTierIsDefault(tiers, overridesSource) {
				return out, &spawnFailure{http.StatusForbidden, "scope_not_attenuated", err.Error()}
			}
			out.notes = append(out.notes, fmt.Sprintf(
				"%s %s ignored (%v)", overridesSource, permissionOverridesField, err))
			overrides, overridesSource = nil, ""
		}
	}
	// An agent that silently comes up owning its group, or holding grants
	// nobody asked for at this launch, is exactly the action-at-a-distance the
	// launch echo exists to surface.
	if isOwnerSource != "" && isOwnerSource != agent.ProvExplicit && isOwner {
		out.notes = append(out.notes, fmt.Sprintf("%s is_owner = true", isOwnerSource))
	}
	if overridesSource != "" && overridesSource != agent.ProvExplicit && len(overrides) > 0 {
		out.notes = append(out.notes, fmt.Sprintf(
			"%s %s (%d)", overridesSource, permissionOverridesField, len(overrides)))
	}
	out.isOwner, out.overrides = isOwner, overrides
	return out, nil
}

// defaultLaunchTiers is the tier stack beneath a named profile: the group's
// default spawn profile, then the global default profile.
func defaultLaunchTiers(g *db.AgentGroup, named *db.SpawnProfile, namedSource string) []launchProfileTier {
	groupProfile := groupDefaultProfile(g)
	globalProfile := globalDefaultProfile()
	return []launchProfileTier{
		{profile: named, source: namedSource},
		{profile: groupProfile, source: profileSource(groupProfile, agent.ProvGroupProfileSource), defaultTier: true},
		{profile: globalProfile, source: profileSource(globalProfile, agent.ProvGlobalProfileSource), defaultTier: true},
	}
}
