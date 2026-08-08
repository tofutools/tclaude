package agentd

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Attenuation-only delegation (§7 of the scoped-permissions design).
//
// An agent that mints permissions for another agent — at spawn, through
// /v1/permissions/grant, or by summoning a scribe — must not hand out MORE
// authority than it holds itself. Before scopes existed there was nothing to
// compare: every grant was the whole slug, so "holds permissions.grant" was the
// only question. A scoped grant makes the question real, and its most
// dangerous answer is the quiet one: an agent whose own groups.spawn is pinned
// to one profile minting a child an UNSCOPED groups.spawn, then acting through
// the child.
//
// So: for every conferred grant, the granter's effective scope for that slug
// must COVER the conferred scope. A violation REFUSES the whole request. It
// deliberately never clamps the conferred scope down to a covered one — a
// silently narrowed grant looks like it worked, and the agent discovers the
// truth as a mystery 403 several turns later.
//
// What this check is NOT: it does not add a requirement that a granter hold
// the slug it confers. permissions.grant has always been recursive (see its
// registry entry), and an orchestrator routinely grants workers slugs it has
// no use for itself. This check constrains the SHAPE of a conferred grant
// against the granter's own shape, and only when the granter's own hold on
// that slug is scoped. Tightening delegation to hold-it-to-grant-it is a
// separate policy decision, not this one.

// conferredGrant is one permission a request is about to mint onto another
// agent: the slug plus the canonical scope JSON it would be written with
// ("" = unscoped, the widest form).
type conferredGrant struct {
	Slug  string
	Scope string
}

// conferredGrantsFromOverrides projects a birth-time override map onto the
// grants that actually confer authority. Denies are skipped: a deny only ever
// REMOVES capability, so it can never be an escalation and needs no cover.
func conferredGrantsFromOverrides(overrides map[string]db.PermissionOverride) []conferredGrant {
	out := make([]conferredGrant, 0, len(overrides))
	for _, slug := range db.SortedOverrideSlugs(overrides) {
		override := overrides[slug]
		if override.Effect != db.PermEffectGrant {
			continue
		}
		out = append(out, conferredGrant{Slug: slug, Scope: override.Scope})
	}
	return out
}

// permissionScopeCovers reports whether granter authorizes at least everything
// conferred authorizes — "conferred is no wider than granter".
//
// A scope is a conjunction over dimensions of a disjunction over matchers, so
// containment reduces to two rules:
//
//   - Every dimension the granter constrains must ALSO appear in conferred.
//     A dimension conferred leaves out is unconstrained there, which admits
//     values the granter's own list excludes.
//   - For a shared dimension, conferred's matchers must be a SUBSET of the
//     granter's.
//
// Dimensions conferred constrains but the granter does not are free: extra
// constraints only narrow.
//
// An unscoped granter (len == 0) covers everything, which is why every
// pre-scope grant keeps delegating exactly as it did. An unscoped conferred
// scope against a scoped granter fails the first rule, which is the case this
// whole mechanism exists for.
//
// @selectors are compared STRUCTURALLY: "@descendants" covers "@descendants",
// and nothing else covers or is covered by it. That is deliberate and
// fail-closed in both directions — deciding whether a concrete agent id falls
// inside "@descendants" needs the lineage walk that Phase 5 owns, and neither
// unproven answer may be assumed. It does mean a granter holding
// {target_agent:[@descendants]} may confer {target_agent:[@descendants]} to an
// agent that is not its own descendant, whose descendant set is then not a
// subset of the granter's. For the spawn path that cannot happen (the child IS
// a descendant); for a cross-agent grant it is a known, disclosed
// approximation that lineage evaluation can tighten later.
func permissionScopeCovers(granter, conferred PermissionScope) bool {
	if len(granter) == 0 {
		return true
	}
	for dim, granterMatchers := range granter {
		conferredMatchers, ok := conferred[dim]
		if !ok || len(conferredMatchers) == 0 {
			return false
		}
		allowed := make(map[string]bool, len(granterMatchers))
		for _, matcher := range granterMatchers {
			allowed[matcher] = true
		}
		for _, matcher := range conferredMatchers {
			if !allowed[matcher] {
				return false
			}
		}
	}
	return true
}

// granterScopesForSlug returns the granter's own winning-tier scopes for slug,
// or (nil, false) when the granter imposes no scope constraint at all.
//
// "No constraint" covers three cases that are different for authorization but
// identical for delegation shape:
//
//   - the granter is the human (convID ""), who is unconstrained by design;
//   - the granter's winning tier holds at least one UNSCOPED row (tier union
//     with unscoped is unscoped — the same rule evalPermissionScope applies);
//   - nothing grants the granter this slug, so there is no shape to attenuate
//     against (see the "what this check is NOT" note above).
//
// It uses the plain resolver, NOT the group-owner bypass: owner-state confers
// the group-lifecycle slugs structurally and carries no scope, so treating it
// as a granting source here would read every group owner as an unscoped
// granter of those slugs.
func granterScopesForSlug(granterConvID, slug string) ([]PermissionScope, bool) {
	if granterConvID == "" {
		return nil, false
	}
	cfg, _ := config.Load()
	v := resolvePermissionVerdict(granterConvID, slug, cfg.HasDefaultPermission(slug))
	if v.Resolution != permAllow || len(v.ScopeJSON) == 0 {
		return nil, false
	}
	scopes := make([]PermissionScope, 0, len(v.ScopeJSON))
	for _, raw := range v.ScopeJSON {
		scope, err := permissionScopeForEval(raw)
		if err != nil {
			// A row the gate refuses to decode authorizes nothing, so it
			// cannot underwrite a delegation either. Skipping it can only
			// narrow what this granter may confer.
			continue
		}
		if len(scope) == 0 {
			return nil, false // an unscoped row absorbs the tier
		}
		scopes = append(scopes, scope)
	}
	if len(scopes) == 0 {
		// Every row in the winning tier was undecodable: the granter can act
		// on nothing through this slug, so it may confer nothing through it.
		return []PermissionScope{}, true
	}
	return scopes, true
}

// checkGrantAttenuation refuses a mint whose conferred grants are wider than
// the granter's own. It returns nil for a human granter and for every grant
// the granter holds unscoped, so it is a no-op on every pre-scope deployment.
//
// Cover is evaluated per granting ROW, not across the union of the granter's
// rows: an agent whose authority for a slug comes from two disjoint scoped
// grants must confer within ONE of them. That is stricter than the gate's own
// union semantics, and deliberately so — the union of two products is not a
// product, so a "conferred ⊆ union" test would have to reason about shapes
// that cannot be written as a single scope anyway.
func checkGrantAttenuation(granterConvID string, grants []conferredGrant) error {
	if granterConvID == "" || len(grants) == 0 {
		return nil
	}
	for _, grant := range grants {
		granterScopes, scoped := granterScopesForSlug(granterConvID, grant.Slug)
		if !scoped {
			continue
		}
		conferred, err := permissionScopeForEval(grant.Scope)
		if err != nil {
			return fmt.Errorf("permission %q: conferred scope is unreadable", grant.Slug)
		}
		covered := false
		for _, granterScope := range granterScopes {
			if permissionScopeCovers(granterScope, conferred) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		return fmt.Errorf(
			"permission %q: you may not grant more than you hold — your own %s is limited to %s, "+
				"but this would confer %s; re-issue it within your own scope, or ask the operator to widen your grant",
			grant.Slug, grant.Slug, renderGranterScopes(granterScopes), renderConferredScope(conferred))
	}
	return nil
}

// renderGranterScopes renders the granter's own scopes for the refusal
// message, so the agent can see exactly what it may confer instead of
// guessing. An empty set means "nothing" — every row was undecodable.
func renderGranterScopes(scopes []PermissionScope) string {
	if len(scopes) == 0 {
		return "nothing (its stored scope cannot be read by this build)"
	}
	rendered := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		rendered = append(rendered, "["+permissionScopeDisplay(scope)+"]")
	}
	sort.Strings(rendered)
	return strings.Join(rendered, " OR ")
}

func renderConferredScope(scope PermissionScope) string {
	if len(scope) == 0 {
		return "an UNSCOPED grant"
	}
	return "[" + permissionScopeDisplay(scope) + "]"
}

// normalizeBlueprintGrants validates a default-grant list authored on a role
// or a template agent, and returns it canonicalized. It is the list twin of
// normalizeSpawnPermissionOverrides and the same boundary: every slug must be
// registered, and every scope must parse, name only dimensions its slug
// declares, and be stored in canonical form.
//
// A blank slug is dropped rather than refused, keeping the pre-scope leniency
// for an editor that posts an empty row.
func normalizeBlueprintGrants(in []db.PermissionGrant) ([]db.PermissionGrant, *spawnFailure) {
	out := []db.PermissionGrant{}
	for _, grant := range in {
		slug := strings.TrimSpace(grant.Slug)
		if slug == "" {
			continue
		}
		if !IsKnownPermSlug(slug) {
			return nil, &spawnFailure{http.StatusBadRequest, "unknown_slug",
				fmt.Sprintf("unknown permission slug %q. Known slugs: %s.", slug, strings.Join(knownSlugs(), ", "))}
		}
		canonical, err := canonicalPermissionScopeForSlug(slug, strings.TrimSpace(grant.Scope))
		if err != nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_scope",
				fmt.Sprintf("permission %q: %v", slug, err)}
		}
		out = append(out, db.PermissionGrant{Slug: slug, Scope: canonical})
	}
	return out, nil
}

// checkTemplateDeployAttenuation applies the same rule to a template roster,
// over EVERY wave rather than just the first: a later wave's grants are minted
// by the same deploy request and must clear the same bar, and the background
// wave runner is far too late to refuse.
//
// An agent whose access does not resolve (vanished or disabled profile) is
// skipped: it will never spawn, so it confers nothing, and reporting that here
// would pre-empt the per-agent error the deploy already gives it.
func checkTemplateDeployAttenuation(agents []db.GroupTemplateAgent, callerConvID string) error {
	if callerConvID == "" || len(agents) == 0 {
		return nil
	}
	for _, a := range agents {
		var role *db.Role
		if ref := strings.TrimSpace(a.RoleRef); ref != "" {
			if rl, err := db.GetRole(ref); err == nil {
				role = rl
			}
		}
		_, overrides, fail := resolveTemplateAgentAccess(a, role)
		if fail != nil {
			continue
		}
		grants := make([]conferredGrant, 0, len(overrides))
		for _, ov := range overrides {
			if ov.Override.Effect != db.PermEffectGrant {
				continue
			}
			grants = append(grants, conferredGrant{Slug: ov.Slug, Scope: ov.Override.Scope})
		}
		if err := checkGrantAttenuation(callerConvID, grants); err != nil {
			return fmt.Errorf("template agent %q: %w", a.Name, err)
		}
	}
	return nil
}

// agentPermissionOverrides reads a live agent's complete per-slug override set
// in the birth-time shape, effects and scopes together.
func agentPermissionOverrides(convID string) (map[string]db.PermissionOverride, error) {
	rows, err := db.ListAgentPermissionOverrideRowsForConv(convID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]db.PermissionOverride, len(rows))
	for _, row := range rows {
		out[row.Slug] = db.PermissionOverride{Effect: row.Effect, Scope: row.ScopeJSON}
	}
	return out, nil
}

// grantedOverridesForConv is the GRANT-only projection the capture/snapshot
// paths trace a live agent with. A read failure yields an empty map, matching
// what those best-effort tracers did with the slug list before scopes existed.
func grantedOverridesForConv(convID string) map[string]db.PermissionOverride {
	all, err := agentPermissionOverrides(convID)
	if err != nil {
		return map[string]db.PermissionOverride{}
	}
	out := make(map[string]db.PermissionOverride, len(all))
	for slug, override := range all {
		if slug == "" || override.Effect != db.PermEffectGrant {
			continue
		}
		out[slug] = override
	}
	return out
}
