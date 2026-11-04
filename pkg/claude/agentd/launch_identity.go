package agentd

import (
	"fmt"
	"net/http"
	"strings"

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
	// request is the spawner's request, when there is one, so the owner check
	// sees the same sudo state the spawn gate did.
	request *http.Request
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
	// overrideSources names, per slug of overrides, the spawn profile (and
	// role) it came from, when a profile tier won it. A slug the request,
	// a role the request named, an inline profile or worker defaults set has
	// no entry. enrollSpawnedConv records it as the row's birth_source.
	overrideSources map[string]db.PermissionBirthSource
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
// the dedicated endpoints require: groups.owners.manage, scoped to cover the
// target group, to mint an owner (spawnerGrantCoversGroup) and
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
	// Role grants form the lowest access tier. The winning profile's (or the
	// request's) overrides narrow or replace a slug, and receiver-owned
	// federation worker defaults sit on top.
	roleGrants := map[string]db.PermissionOverride{}
	roleOf := map[string]string{}
	for _, role := range out.roles {
		for _, grant := range role.Permissions {
			if _, seen := roleOf[grant.Slug]; !seen {
				roleOf[grant.Slug] = role.Name
			}
			candidate := db.PermissionOverride{Effect: db.PermEffectGrant, Scope: grant.Scope}
			if existing, ok := roleGrants[grant.Slug]; ok && existing != candidate {
				return out, &spawnFailure{http.StatusBadRequest, "role_scope_conflict",
					fmt.Sprintf("roles grant %s with incompatible scopes; make the scopes identical or keep that grant on one role", grant.Slug)}
			}
			roleGrants[grant.Slug] = candidate
		}
	}
	parts := []overrideContribution{
		{source: roleRefsSource, field: "role grants", grants: roleGrants, roles: roleOf,
			ambient: launchTierIsDefault(tiers, roleRefsSource)},
		{source: overridesSource, field: permissionOverridesField, grants: overrides,
			ambient: launchTierIsDefault(tiers, overridesSource)},
		{grants: in.workerDefaults},
	}
	if in.oneShot {
		// A one-shot has no identity to receive role or birth-time grants.
		isOwner, parts = false, nil
	}

	if in.spawner != "" {
		mayMint := false
		if isOwner {
			var err error
			if mayMint, err = spawnerGrantCoversGroup(in.request, in.spawner, PermGroupsOwnersManage, groupName(in.group)); err != nil {
				return out, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
			}
		}
		if isOwner && !mayMint {
			if !launchTierIsDefault(tiers, isOwnerSource) {
				return out, &spawnFailure{http.StatusForbidden, "forbidden",
					"making the spawned agent a group owner requires the " + PermGroupsOwnersManage + " permission"}
			}
			out.notes = append(out.notes, fmt.Sprintf(
				"%s is_owner ignored (caller lacks %s)", isOwnerSource, PermGroupsOwnersManage))
			isOwner, isOwnerSource = false, ""
		}
		// Judge everything first. When that fails, an ambient part's GRANTS are
		// dropped and disclosed (its denies stay: a deny only narrows), and
		// what remains, the direct intent, must pass on its own or refuses.
		if fail, reason := spawnerMayConfer(in.spawner, birthConferee, parts); fail != nil {
			dropped := false
			for i := range parts {
				if parts[i].ambient && len(conferredGrantsFromOverrides(parts[i].grants)) > 0 {
					out.notes = append(out.notes, fmt.Sprintf(
						"%s %s ignored (%s)", parts[i].source, parts[i].field, reason))
					parts[i].grants = deniesOf(parts[i].grants)
					dropped = true
				}
			}
			if !dropped {
				return out, fail
			}
			if fail, _ := spawnerMayConfer(in.spawner, birthConferee, parts); fail != nil {
				return out, fail
			}
		}
	}
	overrides = mergeOverrideContributions(parts)
	out.overrideSources = birthSourcesOf(parts, tiers)
	overridesSource = ""
	for i := len(parts) - 1; i >= 0 && overridesSource == ""; i-- {
		if len(parts[i].grants) > 0 {
			overridesSource = parts[i].source
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

// birthConferee is a spawn's child: it has no stable id yet, but its lineage
// edge is mandatory during enrollment, before its overrides are written, so
// it is a descendant by construction.
var birthConferee = grantConferee{descendantByConstruction: true}

// overrideContribution is one source of a spawn's birth-time overrides.
// ambient marks a default tier nobody typed at this launch.
type overrideContribution struct {
	source, field string
	grants        map[string]db.PermissionOverride
	// roles names the role each slug came from, for the role-grant part.
	roles   map[string]string
	ambient bool
}

// birthSourcesOf names, per merged slug, the spawn profile whose part won it
// (mergeOverrideContributions: the last part that has the slug). A slug won
// by a part that is not a stored profile tier (the request, an inline
// profile, worker defaults) gets no entry, so a re-apply never treats it as
// the profile's.
func birthSourcesOf(parts []overrideContribution, tiers []launchProfileTier) map[string]db.PermissionBirthSource {
	out := map[string]db.PermissionBirthSource{}
	for _, part := range parts {
		profileID := tierProfileID(tiers, part.source)
		for slug := range part.grants {
			if profileID <= 0 {
				delete(out, slug)
				continue
			}
			out[slug] = db.PermissionBirthSource{ProfileID: profileID, Role: part.roles[slug]}
		}
	}
	return out
}

// tierProfileID is the stored profile behind a tier source, 0 for the
// request or a profile with no row (an inline profile).
func tierProfileID(tiers []launchProfileTier, source string) int64 {
	if source == "" || source == agent.ProvExplicit {
		return 0
	}
	for _, tier := range tiers {
		if tier.profile != nil && tier.source == source {
			return tier.profile.ID
		}
	}
	return 0
}

func mergeOverrideContributions(parts []overrideContribution) map[string]db.PermissionOverride {
	var out map[string]db.PermissionOverride
	for _, part := range parts {
		for slug, override := range part.grants {
			if out == nil {
				out = map[string]db.PermissionOverride{}
			}
			out[slug] = override
		}
	}
	return out
}

func deniesOf(m map[string]db.PermissionOverride) map[string]db.PermissionOverride {
	out := map[string]db.PermissionOverride{}
	for slug, override := range m {
		if override.Effect != db.PermEffectGrant {
			out[slug] = override
		}
	}
	return out
}

// spawnerMayConfer is the birth-time override gate: setting overrides takes
// permissions.grant (an ambient part made only of denies needs none, since a
// deny only narrows), and no conferred grant may be wider than the spawner's
// own (checkGrantAttenuation, on the merged map, so no tier can smuggle a
// grant in). The reason is the short form for a skip note.
func spawnerMayConfer(spawner string, conferee grantConferee, parts []overrideContribution) (*spawnFailure, string) {
	merged := mergeOverrideContributions(parts)
	conferred := conferredGrantsFromOverrides(merged)
	needsGrant := len(conferred) > 0
	for _, part := range parts {
		if !part.ambient && len(part.grants) > 0 {
			needsGrant = true
		}
	}
	if needsGrant && resolvePermission(spawner, PermPermissionsGrant) != permAllow {
		return &spawnFailure{http.StatusForbidden, "forbidden",
				"setting the spawned agent's permission overrides requires the " + PermPermissionsGrant + " permission"},
			"caller lacks " + PermPermissionsGrant
	}
	if err := checkGrantAttenuation(spawner, conferee, conferred); err != nil {
		return &spawnFailure{http.StatusForbidden, "scope_not_attenuated", err.Error()}, err.Error()
	}
	return nil, ""
}

// spawnerGrantCoversGroup reports whether spawner holds slug as a grant whose
// scope covers group g, the group the spawn lands in. It is the scope-aware
// check the owners-add endpoint makes, so a groups.owners.manage grant scoped
// to group X mints an owner in X and nowhere else, through either door. Only
// GRANTS count: group ownership's structural share of the slug does not, so an
// owner cannot mint a child owner at spawn without holding the slug.
func spawnerGrantCoversGroup(r *http.Request, spawner, slug, group string) (bool, error) {
	if group == "" {
		return false, nil
	}
	if r == nil {
		var err error
		if r, err = http.NewRequest(http.MethodPost, "http://spawn.invalid", nil); err != nil {
			return false, err
		}
	}
	v := resolvePermissionVerdictForRequest(r, spawner, slug)
	if v.Resolution != permAllow {
		return false, nil
	}
	return evalPermissionScope(v, spawner, ActionContext{Group: group, structuralGroup: group}).Satisfied, nil
}

func groupName(g *db.AgentGroup) string {
	if g == nil {
		return ""
	}
	return g.Name
}

// defaultProfileConferralFailure refuses an agent making profileName the
// default spawn profile of group when that profile carries birth-time access
// the agent could not confer itself. A group default applies to every spawn
// into the group, unattended automations included, and an operator-authored
// automation has no owner of its own to check at launch, so the authority is
// checked here, on whoever sets the default: groups.owners.manage covering
// the group when the profile makes owners, and permissions.grant plus
// attenuation for its overrides and its roles' grants. A human (caller "")
// passes, as on a spawn.
func defaultProfileConferralFailure(r *http.Request, caller, group, profileName string) *spawnFailure {
	if caller == "" || strings.TrimSpace(profileName) == "" {
		return nil
	}
	prof, err := db.ResolveSpawnProfile(profileName)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	if prof == nil {
		return nil
	}
	return profileConferralFailure(r, caller, nil, prof, group,
		"a group's default spawn profile applies to every spawn into it, automations included")
}

// profileConferralFailure refuses caller putting birth-time access it could
// not confer itself into after, a profile that reaches spawns it does not
// launch: is_owner takes groups.owners.manage covering group (every group
// when group is ""), and grants, from its overrides or its roles, take
// permissions.grant plus attenuation. When before is the profile as it was,
// only what the write adds is judged, so an edit that leaves the access alone
// needs no authority over it. why says how far the profile reaches.
func profileConferralFailure(r *http.Request, caller string, before, after *db.SpawnProfile, group, why string) *spawnFailure {
	if caller == "" || after == nil {
		return nil
	}
	if fail := profileRelaunchRowsFailure(caller, before, after, why); fail != nil {
		return fail
	}
	if isTrue(after.IsOwner) && (before == nil || !isTrue(before.IsOwner)) {
		var ok bool
		var err error
		where := "every group"
		if group == "" {
			ok, err = spawnerGrantCoversEveryGroup(r, caller, PermGroupsOwnersManage)
		} else {
			where = "group " + group
			ok, err = spawnerGrantCoversGroup(r, caller, PermGroupsOwnersManage, group)
		}
		if err != nil {
			return &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if !ok {
			return &spawnFailure{http.StatusForbidden, "forbidden", fmt.Sprintf(
				"profile %q makes its spawns group owners, which requires %s for %s (%s)",
				after.Name, PermGroupsOwnersManage, where, why)}
		}
	}
	roleGrants, err := profileRoleGrants(after)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	overrides := after.PermissionOverrides
	if before != nil {
		had, err := profileRoleGrants(before)
		if err != nil {
			return &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		for slug, override := range before.PermissionOverrides {
			had[slug] = override
		}
		roleGrants, overrides = grantsNotIn(roleGrants, had), grantsNotIn(overrides, had)
	}
	// The spawns it would reach are not the caller's descendants. Denies
	// only narrow, so they need no authority (ambient).
	if fail, _ := spawnerMayConfer(caller, grantConferee{}, []overrideContribution{
		{grants: roleGrants, ambient: true}, {grants: overrides, ambient: true}}); fail != nil {
		return &spawnFailure{fail.Status, fail.Kind, fmt.Sprintf(
			"profile %q confers permissions on its spawns: %s (%s)", after.Name, fail.Msg, why)}
	}
	return nil
}

// grantsNotIn is the grants in m that had does not already carry identically:
// what a write adds or widens. Denies are left out, since a deny only narrows.
func grantsNotIn(m, had map[string]db.PermissionOverride) map[string]db.PermissionOverride {
	out := map[string]db.PermissionOverride{}
	for slug, override := range m {
		if override.Effect != db.PermEffectGrant {
			continue
		}
		if prev, ok := had[slug]; ok && prev == override {
			continue
		}
		out[slug] = override
	}
	return out
}

// spawnerGrantCoversEveryGroup is spawnerGrantCoversGroup for access that can
// reach any group: only an unscoped grant of slug covers it.
func spawnerGrantCoversEveryGroup(r *http.Request, spawner, slug string) (bool, error) {
	if r == nil {
		var err error
		if r, err = http.NewRequest(http.MethodPost, "http://spawn.invalid", nil); err != nil {
			return false, err
		}
	}
	v := resolvePermissionVerdictForRequest(r, spawner, slug)
	if v.Resolution != permAllow {
		return false, nil
	}
	return evalPermissionScope(v, spawner, ActionContext{}).Unscoped, nil
}

func isTrue(b *bool) bool { return b != nil && *b }

// profileRoleGrants is the grants a profile's named roles confer on its
// spawns, keyed by slug. A role the library no longer has confers nothing.
func profileRoleGrants(prof *db.SpawnProfile) (map[string]db.PermissionOverride, error) {
	grants, _, err := profileRoleGrantsBy(prof, false)
	return grants, err
}

// profileRoleGrantsBy is profileRoleGrants with, per slug, the first of the
// profile's roles that grants it, as a spawn records it (birth_source.Role).
// strict refuses two roles granting one slug with different scopes, as a
// spawn does (role_scope_conflict).
func profileRoleGrantsBy(prof *db.SpawnProfile, strict bool) (map[string]db.PermissionOverride, map[string]string, error) {
	refs := prof.RoleRefs
	if len(refs) == 0 && strings.TrimSpace(prof.RoleRef) != "" {
		refs = []string{prof.RoleRef}
	}
	out, roleOf := map[string]db.PermissionOverride{}, map[string]string{}
	for _, ref := range cleanRoleRefs(refs) {
		role, err := db.GetRole(ref)
		if err != nil {
			return nil, nil, err
		}
		if role == nil {
			continue
		}
		for _, grant := range role.Permissions {
			candidate := db.PermissionOverride{Effect: db.PermEffectGrant, Scope: grant.Scope}
			if existing, ok := out[grant.Slug]; strict && ok && existing != candidate {
				return nil, nil, fmt.Errorf("roles %q and %q grant %s with different scopes", roleOf[grant.Slug], role.Name, grant.Slug)
			}
			out[grant.Slug] = candidate
			if _, seen := roleOf[grant.Slug]; !seen {
				roleOf[grant.Slug] = role.Name
			}
		}
	}
	return out, roleOf, nil
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
