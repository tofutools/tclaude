package agentd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Per-group owner scopes (§6 of the scoped-permissions design, TCL-1071).
//
// Group ownership structurally confers a set of slugs — the owner-implied
// BYPASS, which fills the permUndecided gap at every gate that takes one (see
// PermSlug.OwnerScope). Until now that bypass was all-or-nothing: an owner
// either got the whole slug or was not an owner.
//
// A group may now carry an owner-scope map, slug → scope, that CONFINES that
// bypass for that group. {"groups.spawn":{"spawn_profile":["p1"]}} on group g1
// means: an owner of g1 with no grant of its own may spawn into g1 with
// profile p1, and is refused (popup, then 403) with p2 or an inline profile.
//
// Three properties are load-bearing, and each is settled operator policy:
//
//   - It narrows ONLY the bypass. An EXPLICIT grant the owner holds resolves
//     first, under the ordinary precedence, and is untouched — an operator who
//     wants that narrowed edits the grant's own scope, which is individually
//     controllable. So an owner holding an unscoped groups.spawn grant is
//     unaffected by any owner-scope map.
//   - It is PER GROUP. An owner of g1 (narrowed) and g2 (not) acting on g2 is
//     unaffected; acting on g1 is confined. Every bypass site therefore has to
//     pick the RIGHT group's map, which is why the helpers below take a group
//     (or enumerate owned groups) rather than a boolean "is an owner".
//   - Where no target group is in context, a narrowed map fails CLOSED for
//     that group's contribution: an owner-scope map that cannot be evaluated
//     must never read as "unrestricted".
//
// Deny still suppresses the bypass entirely, and grants stay monotonic: a map
// can only take reach AWAY from the structural bypass, never add any.

// ownerBypassFunc is the structural owner bypass a gate hands to
// requirePermissionEx. It receives the resolved caller, the slug under
// evaluation and the request's ActionContext — the last two because the
// bypass is itself narrowable per group, so "is this caller an owner" is no
// longer a sufficient question.
type ownerBypassFunc func(convID, slug string, actx ActionContext) bool

// ownerScopeMap is a group's owner-scope map in evaluated form: slug → the
// scope that slug's owner-implied bypass is confined to for this group. A slug
// ABSENT from the map is unrestricted (today's bypass); the map never widens.
type ownerScopeMap map[string]PermissionScope

// parseOwnerScopes parses, validates and canonicalizes an owner-scope map from
// the wire. Empty, null and {} all mean "no narrowing" and persist as "", so a
// group that never had one keeps its historical representation byte for byte.
//
// The per-slug scope goes through exactly the validation a GRANT scope does —
// same dimension registry, same declared-dimension rule — so an operator
// cannot narrow an owner bypass along a dimension no gate evaluates.
func parseOwnerScopes(raw json.RawMessage) (ownerScopeMap, string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, "", nil
	}
	if len(trimmed) > permissionScopeMaxJSONBytes {
		return nil, "", fmt.Errorf("owner scopes exceed %d bytes", permissionScopeMaxJSONBytes)
	}
	if trimmed[0] != '{' {
		return nil, "", fmt.Errorf("owner scopes must be a JSON object mapping a permission slug to a scope")
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &wire); err != nil {
		return nil, "", fmt.Errorf("invalid owner scopes: %w", err)
	}
	if wire == nil {
		return nil, "", fmt.Errorf("owner scopes must be a JSON object mapping a permission slug to a scope")
	}
	out := make(ownerScopeMap, len(wire))
	for rawSlug, rawScope := range wire {
		slug := strings.TrimSpace(rawSlug)
		if slug == "" {
			return nil, "", fmt.Errorf("owner scopes contain an empty permission slug")
		}
		if slug != rawSlug {
			return nil, "", fmt.Errorf("owner-scope slug %q must not contain surrounding whitespace", rawSlug)
		}
		if !IsKnownPermSlug(slug) {
			return nil, "", fmt.Errorf("unknown permission slug %q", slug)
		}
		if !IsOwnerImpliedSlug(slug) {
			// Narrowing something ownership never conferred would silently do
			// nothing; say so rather than storing a no-op the operator will
			// later read as protection.
			return nil, "", fmt.Errorf("permission %q has no owner-implied bypass to narrow", slug)
		}
		scope, _, err := parsePermissionScope(rawScope)
		if err != nil {
			return nil, "", fmt.Errorf("permission %q: %w", slug, err)
		}
		if len(scope) == 0 {
			// {} would parse as "unscoped", i.e. exactly the unrestricted
			// bypass the operator was trying to constrain. Refuse the
			// ambiguity instead of storing the widest possible reading of a
			// value written to narrow.
			return nil, "", fmt.Errorf("permission %q: an owner scope must name at least one dimension "+
				"(omit the slug entirely to leave its owner bypass unrestricted)", slug)
		}
		if err := validatePermissionScopeForSlug(slug, scope); err != nil {
			return nil, "", err
		}
		out[slug] = scope
	}
	if len(out) == 0 {
		return nil, "", nil
	}
	// json.Marshal sorts map keys, so the canonical form is stable and two
	// equivalent maps compare equal in storage and in tests.
	canonical, err := json.Marshal(out)
	if err != nil {
		return nil, "", fmt.Errorf("encode owner scopes: %w", err)
	}
	if len(canonical) > permissionScopeMaxJSONBytes {
		return nil, "", fmt.Errorf("owner scopes exceed %d bytes", permissionScopeMaxJSONBytes)
	}
	return out, string(canonical), nil
}

// canonicalOwnerScopes validates a raw wire value and returns its canonical
// storage form ("" = no narrowing). It is the single write-path boundary: the
// group PATCH endpoint, the template editor and the deploy path all pass
// through it, so no path can persist a map the gate would then refuse to read.
func canonicalOwnerScopes(raw string) (string, error) {
	_, canonical, err := parseOwnerScopes(json.RawMessage(strings.TrimSpace(raw)))
	return canonical, err
}

// ownerScopesForEval decodes a persisted owner-scope map for AUTHORIZATION.
//
// Like permissionScopeForEval, a value this build cannot decode is an ERROR,
// never a silent "unrestricted". A map naming a dimension only a newer daemon
// understands must not read as a wildcard — that is the exact inversion of
// what the operator wrote.
func ownerScopesForEval(raw string) (ownerScopeMap, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	scopes, _, err := parseOwnerScopes(json.RawMessage(raw))
	return scopes, err
}

// ownerBypassPermittedForGroup reports whether g's owner-implied bypass for
// slug may fire for the action actx describes.
//
// It answers ONLY the narrowing question — the caller has already established
// that convID owns g. Three outcomes:
//
//   - g has no owner-scope map, or none for slug → true (today's bypass).
//   - g narrows slug and actx satisfies the scope → true.
//   - g narrows slug and actx does not satisfy it (including an actx that says
//     nothing about a constrained dimension) → false, and the gate falls
//     through to the popup, then 403. This is the fail-closed case a site with
//     no target group in context also lands in.
//
// An undecodable map denies the bypass for the whole group. That is louder
// than ignoring it, and it is the only reading that cannot widen authority.
func ownerBypassPermittedForGroup(g *db.AgentGroup, convID, slug string, actx ActionContext) bool {
	if g == nil {
		return false
	}
	scopes, err := ownerScopesForEval(g.OwnerScopesJSON)
	if err != nil {
		slog.Warn("permissions: undecodable group owner scopes ignored (bypass fails closed)",
			"group", g.Name, "slug", slug, "error", err)
		return false
	}
	scope, narrowed := scopes[slug]
	if !narrowed {
		return true
	}
	return permissionScopeSatisfied(convID, scope, actx)
}

// ownerBypassPermittedForGroupID is ownerBypassPermittedForGroup for a caller
// that holds only the group id. A group that cannot be read denies the bypass:
// the narrowing lives on the row, so an unread row is an unknown narrowing.
func ownerBypassPermittedForGroupID(groupID int64, convID, slug string, actx ActionContext) bool {
	g, err := db.GetAgentGroupByID(groupID)
	if err != nil || g == nil {
		slog.Warn("permissions: owner-scope group lookup failed (bypass fails closed)",
			"group_id", groupID, "slug", slug, "error", err)
		return false
	}
	return ownerBypassPermittedForGroup(g, convID, slug, actx)
}

// ownerOfGroupPermitting is the group-scoped bypass predicate: convID owns g
// AND g's owner-scope map permits slug for this action. It replaces the bare
// db.IsAgentGroupOwner test at every gate whose bypass reach is the group.
//
// It fills in the group dimension when the gate did not. Every caller is a gate
// whose action's subject IS g — that is what makes owning g relevant at all —
// so "which group does this act on" is known here even at the sites that pass
// no context (the group-link gates). Without this, a map written as
// {"groups.link.add": {"group": ["g1"]}} ON g1 would refuse every time instead
// of confining the bypass to g1, which reads as a revoke rather than the
// narrowing the operator wrote.
//
// Deliberately NOT done in ownsAnyGroupPermitting: there the owned group is not
// the action's subject, and asserting it were would be fail-OPEN.
func ownerOfGroupPermitting(g *db.AgentGroup, convID, slug string, actx ActionContext) bool {
	if g == nil {
		return false
	}
	owns, err := db.IsAgentGroupOwner(g.ID, convID)
	if err != nil || !owns {
		return false
	}
	if actx.Group == "" {
		actx.Group = g.Name
	}
	return ownerBypassPermittedForGroup(g, convID, slug, actx)
}

// ownsAnyGroupPermitting backs the bypasses that are scoped to NO particular
// group — human.notify, process.runs.read, and the worktree prepare/discard
// pair — where owning anything at all marks the caller as a coordinating role.
//
// It is the multi-group rule in its plainest form: the caller passes if ANY
// owned group would permit the action. A group that narrows the slug
// contributes only when the ActionContext satisfies its scope, so at a site
// that describes nothing (the ownsAnyGroup sites describe no group and no
// profile) a narrowed group contributes NOTHING and an unnarrowed one still
// carries the caller. An owner whose ONLY group narrows the slug is therefore
// refused here — the fail-closed answer the design mandates when the target
// group is absent from context.
func ownsAnyGroupPermitting(convID, slug string, actx ActionContext) bool {
	owned, err := db.ListGroupsOwnedBy(convID)
	if err != nil {
		slog.Warn("permissions: owned-group lookup failed", "conv", convID, "error", err)
		return false
	}
	for _, id := range owned {
		if ownerBypassPermittedForGroupID(id, convID, slug, actx) {
			return true
		}
	}
	return false
}

// ownerOfGroupContainingPermitting is the cross-agent form: convID owns at
// least one group containing targetConv whose owner-scope map permits slug for
// this action. Narrowing is evaluated per candidate group, so an owner of a
// narrowed g1 and an unnarrowed g2 that both contain the target still passes
// through g2 — the same union the plain multi-group rule gives.
//
// Each candidate is evaluated with the GROUP DIMENSION set to that candidate,
// because the authority being exercised flows through exactly that group: it is
// the group the caller owns and the group the target is a member of. The
// cross-agent gate itself cannot fill this in — it targets an agent, and which
// group carries the bypass is only decided here, one candidate at a time.
//
// It cannot widen anything: a map on g1 naming g2 still refuses, since only g1's
// own map is consulted while g1 is the candidate. Without it, the obvious
// narrowing {"agent.retire": {"group": ["g1"]}} on g1 would refuse every time
// and read as a revoke rather than the confinement the operator wrote.
func ownerOfGroupContainingPermitting(convID, targetConv, slug string, actx ActionContext) bool {
	ids, err := db.ListOwnedGroupIDsContaining(convID, targetConv)
	if err != nil {
		slog.Warn("permissions: owner-of-group-containing lookup failed",
			"conv", convID, "target", targetConv, "error", err)
		return false
	}
	for _, id := range ids {
		g, err := db.GetAgentGroupByID(id)
		if err != nil || g == nil {
			slog.Warn("permissions: owner-scope group lookup failed (bypass fails closed)",
				"group_id", id, "slug", slug, "error", err)
			continue
		}
		candidate := actx
		if candidate.Group == "" {
			candidate.Group = g.Name
		}
		if ownerBypassPermittedForGroup(g, convID, slug, candidate) {
			return true
		}
	}
	return false
}

// ownerTierEntry is what group ownership confers on one agent for one slug,
// AFTER per-group narrowing, as the effective-permissions listing renders it
// and as attenuation reads it.
//
// Unrestricted means at least one owned group confers the slug with no
// narrowing — union with unrestricted is unrestricted, the same rule the grant
// tiers apply to an unscoped row. Otherwise Scopes is the union of the
// narrowings across owned groups, and the owner may act within any one of them.
type ownerTierEntry struct {
	Unrestricted bool
	Scopes       []PermissionScope
}

// ownerImpliedTier maps slug → what ownership confers. An absent slug is not
// conferred by ownership at all.
type ownerImpliedTier map[string]ownerTierEntry

// slugs returns the conferred slugs, sorted — the shape the listing's
// candidate enumeration wants.
func (t ownerImpliedTier) slugs() []string {
	if len(t) == 0 {
		return nil
	}
	out := make([]string, 0, len(t))
	for slug := range t {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}

// satisfiedBy reports whether the owner tier authorizes slug for this action —
// the LISTING's read of the same question the gate asks. Kept next to the gate
// helpers deliberately: listing == gate is a guarded invariant, and the two
// must not grow separate notions of "the owner may do this".
func (t ownerImpliedTier) satisfiedBy(convID, slug string, actx ActionContext) bool {
	entry, ok := t[slug]
	if !ok {
		return false
	}
	if entry.Unrestricted {
		return true
	}
	for _, scope := range entry.Scopes {
		if permissionScopeSatisfied(convID, scope, actx) {
			return true
		}
	}
	return false
}

// ownerImpliedTierFor computes what ownership confers on convID across every
// group it owns, applying each group's own narrowing.
//
// A group whose map is undecodable contributes NOTHING (it cannot widen and
// must not silently confer), matching what the gate does with the same row. A
// DB error degrades to "not an owner", the pre-existing behaviour: owner perms
// go un-annotated rather than failing the whole listing.
func ownerImpliedTierFor(convID string) ownerImpliedTier {
	owned, err := db.ListGroupsOwnedBy(convID)
	if err != nil {
		slog.Warn("permissions: owned-group lookup failed", "conv", convID, "error", err)
		return nil
	}
	if len(owned) == 0 {
		return nil
	}
	implied := OwnerImpliedSlugs()
	tier := ownerImpliedTier{}
	for _, id := range owned {
		g, err := db.GetAgentGroupByID(id)
		if err != nil || g == nil {
			continue
		}
		scopes, err := ownerScopesForEval(g.OwnerScopesJSON)
		if err != nil {
			slog.Warn("permissions: undecodable group owner scopes ignored in listing",
				"group", g.Name, "error", err)
			continue
		}
		for _, slug := range implied {
			entry := tier[slug]
			if entry.Unrestricted {
				continue
			}
			scope, narrowed := scopes[slug]
			if !narrowed {
				entry.Unrestricted = true
				entry.Scopes = nil
			} else {
				entry.Scopes = append(entry.Scopes, scope)
			}
			tier[slug] = entry
		}
	}
	return tier
}

// ownerScopeDisplay renders the owner tier's reach for one slug, appended to
// the "owner:<scope>" provenance so a reader sees the NARROWING as well as the
// call-site family. Unrestricted renders as nothing extra, which is exactly
// what pre-Phase-6 clients already display.
func ownerScopeDisplay(entry ownerTierEntry) string {
	if entry.Unrestricted || len(entry.Scopes) == 0 {
		return ""
	}
	seen := map[string]bool{}
	var rendered []string
	for _, scope := range entry.Scopes {
		rendered = appendUnique(rendered, seen, permissionScopeDisplay(scope))
	}
	if len(rendered) == 0 {
		return ""
	}
	sort.Strings(rendered)
	parts := make([]string, len(rendered))
	for i, display := range rendered {
		parts[i] = "[" + display + "]"
	}
	return " " + strings.Join(parts, " OR ")
}
