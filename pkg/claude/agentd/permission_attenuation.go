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
// dangerous answer is the quiet one: an agent whose own groups.members.spawn is pinned
// to one profile minting a child an UNSCOPED groups.members.spawn, then acting through
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
//
// That leniency is only safe because the granter cannot MANUFACTURE the
// unheld state for itself. Two guards keep it honest, and both belong to this
// rule rather than to the endpoints they live in:
//
//   - a DENY confers nothing (not "unconstrained"), so denying yourself a
//     narrowly scoped slug does not buy you the right to re-grant it wide;
//   - selfScopeShedRefused blocks an agent from revoking its own scoped grant.
//
// A residual remains by design: a DIFFERENT agent holding permissions.revoke
// can strip A's scoped grant, after which A is genuinely unheld and the
// leniency applies. That is collusion between two permission administrators,
// and narrowing it belongs with the wider delegation-policy question above.

// conferredGrant is one permission a request is about to mint onto another
// agent: the slug plus the canonical scope JSON it would be written with
// ("" = unscoped, the widest form).
type conferredGrant struct {
	Slug  string
	Scope string
}

// grantConferee carries the actor a grant will authorize. Birth-time grants
// are checked before the child exists (and therefore before its lineage edge
// can be written), so their descendant relationship is explicit rather than
// inferred from a temporarily missing agent id. Enrollment later treats a
// missing lineage edge as fatal; see recordSpawnLineage.
type grantConferee struct {
	agentID                  string
	descendantByConstruction bool
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
// @selectors compare structurally here. checkGrantAttenuation adds the
// lineage-dependent half: a structurally covered selector may only be
// conferred to the granter itself or an actor in the granter's descendant set.
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
// "No constraint" covers two cases that are different for authorization but
// identical for delegation shape:
//
//   - the granter's winning tier holds at least one UNSCOPED row (tier union
//     with unscoped is unscoped — the same rule evalPermissionScope applies);
//   - nothing grants the granter this slug, so there is no shape to attenuate
//     against (see the "what this check is NOT" note above).
//
// Positive sources compose. A scoped explicit grant for group B and an owner
// grant for group A therefore attenuate against A OR B, just as the action
// gate authorizes either. An unscoped row from either source absorbs the union.
func granterScopesForSlug(src permSources, cfg *config.Config, ownerTier ownerImpliedTier, slug string) ([]PermissionScope, bool) {
	v := resolveEffectivePermissionVerdictFrom(src, slug,
		cfg.HasDefaultPermission(slug), cfg.HasDefaultPermission(PermGroupsAdmin))
	if v.Resolution == permDeny {
		// A granter DENIED a slug may confer nothing through it. Reading a deny
		// as "unconstrained" (the same answer as "does not hold it") would make
		// the whole rule sheddable: an agent whose grant is narrowly scoped
		// could deny the slug ON ITSELF, which replaces the scoped row, and then
		// confer it unscoped because it no longer "holds" a scope to attenuate
		// against. An empty scope set is the fail-closed reading, and it costs
		// nothing legitimate — an agent conferring a capability it is itself
		// forbidden is not a flow worth preserving.
		return []PermissionScope{}, true
	}
	if v.Resolution == permUndecided {
		// Nothing explicit speaks for the granter. Its structural owner
		// bypass may — and if every owned group narrows the slug, that
		// narrowing is the whole of the granter's authority for it.
		entry, conferred := ownerTier[slug]
		if !conferred || entry.Unrestricted {
			return nil, false
		}
		// A DEGRADED entry reaches here with no usable scopes: the daemon
		// could not read what one of the owner's groups narrows. That is a
		// failure to answer, not an answer of "unconstrained" — the same
		// fail-closed reading the all-rows-undecodable grant tier gets above.
		// An empty scope set means the granter may confer nothing through it.
		if len(entry.Scopes) == 0 {
			return []PermissionScope{}, true
		}
		return entry.Scopes, true
	}
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
	if entry, conferred := ownerTier[slug]; conferred {
		if entry.Unrestricted {
			return nil, false
		}
		scopes = append(scopes, entry.Scopes...)
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
func checkGrantAttenuation(granterConvID string, conferee grantConferee, grants []conferredGrant) error {
	if granterConvID == "" || len(grants) == 0 {
		return nil
	}
	// Read the granter's permission sources ONCE. A spawn conferring a dozen
	// overrides would otherwise pay a config load plus three queries per slug
	// for one unchanging caller state.
	src := loadPermSources(granterConvID)
	cfg, _ := config.Load()
	// The owner tier is read once too, and only matters for a slug the
	// granter holds through ownership alone (see granterScopesForSlug).
	ownerTier := ownerImpliedTierFor(granterConvID)
	var granterAgentID string
	for _, grant := range grants {
		granterScopes, scoped := granterScopesForSlug(src, cfg, ownerTier, grant.Slug)
		if !scoped {
			continue
		}
		conferred, err := permissionScopeForEval(grant.Scope)
		if err != nil {
			return fmt.Errorf("permission %q: conferred scope is unreadable", grant.Slug)
		}
		hasRelativeSelector := scopeContainsDescendantSelector(conferred)
		confereeIsSelf := false
		confereeIsDescendant := conferee.descendantByConstruction
		if hasRelativeSelector && !confereeIsDescendant {
			if granterAgentID == "" {
				granterAgentID, err = db.AgentIDForConv(granterConvID)
				if err != nil {
					return fmt.Errorf("permission %q: could not resolve your stable agent identity to check selector attenuation: %w", grant.Slug, err)
				}
				if granterAgentID == "" {
					return fmt.Errorf("permission %q: your stable agent identity is unavailable, so the conferee cannot be proven inside your descendant set", grant.Slug)
				}
			}
			confereeIsSelf = conferee.agentID == granterAgentID
			if !confereeIsSelf {
				if conferee.agentID == "" {
					return fmt.Errorf("permission %q: the conferee has no stable agent identity, so it cannot be proven inside your descendant set", grant.Slug)
				}
				confereeIsDescendant, err = db.IsAgentDescendant(granterAgentID, conferee.agentID)
				if err != nil {
					return fmt.Errorf("permission %q: could not check whether the conferee is inside your descendant set: %w", grant.Slug, err)
				}
			}
		}
		covered := false
		for _, granterScope := range granterScopes {
			covers := permissionScopeCovers(granterScope, conferred)
			if hasRelativeSelector && confereeIsDescendant {
				covers = permissionScopeCoversForDescendantConferee(granterScope, conferred)
			}
			if covers {
				covered = true
				break
			}
		}
		if covered {
			if !hasRelativeSelector || confereeIsSelf || confereeIsDescendant {
				continue
			}
			return fmt.Errorf(
				"permission %q: a target_agent selector using @descendants or @self-spawned may only be conferred to yourself or an agent inside your descendant set; the conferee is outside it",
				grant.Slug)
		}
		return fmt.Errorf(
			"permission %q: you may not grant more than you hold — your own %s is limited to %s, "+
				"but this would confer %s; re-issue it within your own scope, or ask the operator to widen your grant",
			grant.Slug, grant.Slug, renderGranterScopes(granterScopes), renderConferredScope(conferred))
	}
	return nil
}

// permissionScopeCoversForDescendantConferee is the relational-selector arm
// of cover. @descendants remains covariant down a realistic lineage tree. A
// descendant's @self-spawned set, however, consists of the granter's deeper
// descendants, not its direct children, so an identical @self-spawned matcher
// is not enough: the particular covering row must include @descendants.
//
// Accepted residual: lineage evaluation has a 64-edge safety horizon. At a
// 65+-level spawn chain, shifting that relative horizon down one edge can admit
// a leaf the granter's own @descendants evaluation no longer reaches. That
// extreme chain-plus-delegation case remains bounded by the shared evaluator
// limit rather than making cover depth-aware here.
func permissionScopeCoversForDescendantConferee(granter, conferred PermissionScope) bool {
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
			if dim == ScopeDimTargetAgent && matcher == "@self-spawned" {
				if !allowed["@descendants"] {
					return false
				}
				continue
			}
			if !allowed[matcher] {
				return false
			}
		}
	}
	return true
}

func scopeContainsDescendantSelector(scope PermissionScope) bool {
	for _, matcher := range scope[ScopeDimTargetAgent] {
		if matcher == "@descendants" || matcher == "@self-spawned" {
			return true
		}
	}
	return false
}

// renderGranterScopes renders the granter's own scopes for the refusal
// message, so the agent can see exactly what it may confer instead of
// guessing. An empty set means "nothing" — every row was undecodable.
func renderGranterScopes(scopes []PermissionScope) string {
	if len(scopes) == 0 {
		return "nothing (you hold no usable grant for it — it is denied, or its stored scope cannot be read by this build)"
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

// selfScopeShedRefused blocks an agent from REVOKING its own scoped grant.
//
// Attenuation asks "what is your own scope for this slug", and answers
// "unconstrained" when the granter holds no scope — the settled reading, since
// permissions.grant is recursive and an orchestrator routinely grants slugs it
// does not hold. That answer is only safe if the granter cannot MANUFACTURE
// the unheld state: an agent whose grant is pinned to one group could otherwise
// revoke its own narrowed row and immediately confer the slug unscoped, on
// itself or on a child, with the pin gone.
//
// So: your own narrowing is not yours to remove. It costs nothing legitimate —
// an agent reducing its own authority can still deny itself the slug outright
// (which granterScopesForSlug reads as "confers nothing"), and an operator or a
// manager acting on a DIFFERENT agent is untouched.
//
// Returns true when the response has been written.
func selfScopeShedRefused(w http.ResponseWriter, callerConvID string, target *resolvedTarget, slug string) bool {
	if callerConvID == "" || target == nil || target.Sentinel || target.Key != callerConvID {
		return false
	}
	rows, err := db.ListAgentPermissionOverrideRowsForConv(callerConvID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"could not read your own permission overrides: "+err.Error())
		return true
	}
	for _, row := range rows {
		if row.Slug != slug || row.Effect != db.PermEffectGrant || row.ScopeJSON == "" {
			continue
		}
		writeError(w, http.StatusForbidden, "scope_not_attenuated", fmt.Sprintf(
			"permission %q: you may not remove the scope the operator put on your own grant — "+
				"clearing it would let you re-acquire %s unscoped. Ask the operator to change it, "+
				"or deny yourself the slug instead", slug, slug))
		return true
	}
	return false
}

// normalizeBlueprintGrants validates a default-grant list authored on a role
// or a template agent, and returns it canonicalized. It is the list twin of
// normalizeSpawnPermissionOverrides and the same boundary: every slug must be
// registered, and every scope must parse, name only dimensions its slug
// declares, and be stored in canonical form.
//
// A blank slug is dropped rather than refused, keeping the pre-scope leniency
// for an editor that posts an empty row.
//
// A repeated slug is REFUSED rather than deduped to one of its entries. When
// entries were bare slugs a duplicate was meaningless; now [{S, narrow}, S] is
// two different grants of S, and the tiering that consumes this list is
// last-wins — so silently keeping either one picks a capability level the
// author did not state. A list posted by an editor that cannot represent
// scopes will land here rather than quietly deploying the unscoped arm.
func normalizeBlueprintGrants(in []db.PermissionGrant) ([]db.PermissionGrant, *spawnFailure) {
	out := []db.PermissionGrant{}
	seen := map[string]bool{}
	for _, grant := range in {
		slug := strings.TrimSpace(grant.Slug)
		if slug == "" {
			continue
		}
		if !IsKnownPermSlug(slug) {
			return nil, &spawnFailure{http.StatusBadRequest, "unknown_slug",
				fmt.Sprintf("unknown permission slug %q. Known slugs: %s.", slug, strings.Join(knownSlugs(), ", "))}
		}
		if seen[slug] {
			return nil, &spawnFailure{http.StatusBadRequest, "duplicate_slug",
				fmt.Sprintf("permission %q is listed more than once; list it exactly once, "+
					"with the scope you want", slug)}
		}
		seen[slug] = true
		canonical, err := canonicalPermissionScopeForSlug(slug, strings.TrimSpace(grant.Scope))
		if err != nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_scope",
				fmt.Sprintf("permission %q: %v", slug, err)}
		}
		out = append(out, db.PermissionGrant{
			Slug: slug, Scope: canonical, ScopeSpecified: grant.ScopeSpecified,
		})
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
		roles, roleFail := resolveTemplateAgentRoles(a)
		if roleFail != nil {
			// Fail rather than judge a smaller grant set than the wave runner may
			// confer. Profile resolution failures are reported by deploy as well.
			return fmt.Errorf("template agent %q: could not resolve role to check what this deploy would confer: %s",
				a.Name, roleFail.Msg)
		}
		_, overrides, fail := resolveTemplateAgentAccess(a, roles)
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
		if err := checkGrantAttenuation(callerConvID, grantConferee{descendantByConstruction: true}, grants); err != nil {
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
