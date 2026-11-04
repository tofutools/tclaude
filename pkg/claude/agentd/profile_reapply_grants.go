package agentd

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Re-applying an edited spawn profile's grants (tcl-fg2kqk). At birth a
// profile's tier gives the agent its roles' grants (all of its roles), with
// the profile's own permission overrides on top, and each row it wrote
// records the profile as its birth_source. A re-apply recomputes that from
// the agent's copy of the profile and from the profile now, and changes only
// the rows that still carry the profile's record (db.ReapplyBirthPermissions):
// explicit, sudo and changed-since grants stay.
//
// A slug the profile adds is added only to a part (its roles, or its
// overrides) that took effect at the spawn, which the agent's rows show: a
// part the spawn request or a higher tier replaced never applied, and an
// edit must not make it apply. Nor is a slug the agent lost since its spawn
// (revoked) put back. Conferral is judged against the re-applier
// (reapplyCopyFor: profileConferralFailure over the two copies), so widening
// takes permissions.grant plus attenuation and narrowing takes nothing.
// Group ownership (is_owner) has no birth record yet, so it is not
// re-applied.

// profileBirthGrants is what profile p's tier gives at birth: the grants of
// its roles (withRoles), overlaid by its permission overrides
// (withOverrides), and per slug the role it comes from ("" for an override).
func profileBirthGrants(p *db.SpawnProfile, withOverrides, withRoles bool) (map[string]db.PermissionOverride, map[string]string, error) {
	out, roleOf := map[string]db.PermissionOverride{}, map[string]string{}
	if p == nil {
		return out, roleOf, nil
	}
	if withRoles {
		grants, by, err := profileRoleGrantsBy(p, true)
		if err != nil {
			return nil, nil, err
		}
		out, roleOf = grants, by
	}
	if withOverrides {
		overrides, invalid := normalizeSpawnPermissionOverrides(p.PermissionOverrides)
		if invalid != "" {
			return nil, nil, fmt.Errorf("profile %q permission overrides: %s", p.Name, invalid)
		}
		for slug, o := range overrides {
			out[slug] = o
			delete(roleOf, slug)
		}
	}
	return out, roleOf, nil
}

// reapplyGrantParts is which parts of profileID's birth access took effect
// on agentID, read from the rows that still carry its record, and those rows
// (slug → role).
type reapplyGrantParts struct {
	overrides, roles bool
	rows             map[string]string
}

func readReapplyGrantParts(agentID string, profileID int64) (reapplyGrantParts, error) {
	rows, err := db.AgentBirthPermissionRows(agentID, profileID)
	if err != nil {
		return reapplyGrantParts{}, err
	}
	parts := reapplyGrantParts{rows: rows}
	for _, role := range rows {
		if role == "" {
			parts.overrides = true
		} else {
			parts.roles = true
		}
	}
	return parts, nil
}

// inEffect is p with only the parts that took effect, for judging conferral
// over exactly what a re-apply can write.
func (parts reapplyGrantParts) inEffect(p *db.SpawnProfile) *db.SpawnProfile {
	out := *p
	if !parts.roles {
		out.Role, out.RoleRef, out.RoleRefs = "", "", nil
	}
	if !parts.overrides {
		out.PermissionOverrides = nil
	}
	return &out
}

// reapplyGrantPlan is what re-applying profileID to agentID does to its
// grants: before is the agent's copy of the profile, after the profile now,
// over the parts that took effect.
func reapplyGrantPlan(profileID int64, parts reapplyGrantParts, before, after *db.SpawnProfile, grantedBy string) (*db.BirthPermissionReapply, error) {
	was, _, err := profileBirthGrants(before, parts.overrides, parts.roles)
	if err != nil {
		return nil, err
	}
	now, roleOf, err := profileBirthGrants(after, parts.overrides, parts.roles)
	if err != nil {
		return nil, err
	}
	plan := &db.BirthPermissionReapply{ProfileID: profileID, GrantedBy: grantedBy}
	planned := map[string]bool{}
	for _, slug := range changedGrantSlugs(was, now) {
		_, had := was[slug]
		n, has := now[slug]
		change := db.BirthPermissionChange{Slug: slug, MayAdd: !had,
			Source: db.PermissionBirthSource{ProfileID: profileID, Role: roleOf[slug]}}
		if has {
			want := n
			change.Want = &want
		}
		plan.Changes = append(plan.Changes, change)
		planned[slug] = true
	}
	// A row a role gave at the spawn whose slug the profile no longer gives
	// goes too, even when the role was edited or deleted since, so the old
	// copy's roles, read from the library now, no longer show it. Only ever a
	// removal: a row's value is not otherwise changed outside the edit.
	for slug, role := range parts.rows {
		if _, has := now[slug]; has || role == "" || planned[slug] {
			continue
		}
		plan.Changes = append(plan.Changes, db.BirthPermissionChange{Slug: slug})
	}
	sort.Slice(plan.Changes, func(i, j int) bool { return plan.Changes[i].Slug < plan.Changes[j].Slug })
	return plan, nil
}

// changedGrantSlugs lists, sorted, the slugs whose grant differs between was
// and now.
func changedGrantSlugs(was, now map[string]db.PermissionOverride) []string {
	var out []string
	for slug, o := range was {
		if n, ok := now[slug]; !ok || n != o {
			out = append(out, slug)
		}
	}
	for slug := range now {
		if _, ok := was[slug]; !ok {
			out = append(out, slug)
		}
	}
	sort.Strings(out)
	return out
}

// reapplyGrantsGranter is the granted_by a re-applied grant records.
func reapplyGrantsGranter(caller, profile string) string {
	return fmt.Sprintf("re-apply of profile %s by %s", profile, granterLabel(caller))
}

// auditReappliedGrants records each grant a re-apply changed, as its own
// audit row naming the agent.
func auditReappliedGrants(r *http.Request, agentID, convID, profile string, plan *db.BirthPermissionReapply) {
	if plan == nil {
		return
	}
	kind, actorConv, label := auditActor(r, auditSourceForPath(r.URL.Path))
	for _, res := range plan.Results {
		switch res.Outcome {
		case db.BirthPermissionChanged, db.BirthPermissionRemoved, db.BirthPermissionAdded:
		default:
			continue
		}
		if _, err := db.InsertAuditLog(db.AuditLogEntry{
			ActorKind: kind, ActorConv: actorConv, ActorLabel: label, Verb: "permissions.reapply",
			TargetConv: convID, TargetAgent: agentID, TargetLabel: auditConvLabel(convID),
			Detail: auditClip(fmt.Sprintf("%s %s: %s → %s (profile %s)", res.Slug, res.Outcome, orNone(res.From), orNone(res.To), profile), 240),
			Method: r.Method, Path: r.URL.Path, Status: http.StatusOK, Source: auditSourceForPath(r.URL.Path),
		}); err != nil {
			slog.Warn("profile re-apply: audit grant change failed", "agent", agentID, "slug", res.Slug, "error", err)
		}
	}
}
