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
// bypass for that group. {"groups.members.spawn":{"spawn_profile":["p1"]}} on group g1
// means: an owner of g1 with no grant of its own may spawn into g1 with
// profile p1, and is refused (popup, then 403) with p2 or an inline profile.
//
// Three properties are load-bearing, and each is settled operator policy:
//
//   - It narrows ONLY the bypass. An EXPLICIT grant the owner holds resolves
//     first, under the ordinary precedence, and is untouched — an operator who
//     wants that narrowed edits the grant's own scope, which is individually
//     controllable. So an owner holding an unscoped groups.members.spawn grant is
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

func ownerPermissionMatch(convID, slug string, actx ActionContext) (bool, string) {
	entry, ok := ownerImpliedTierFor(convID)[slug]
	if !ok || !entry.confers() {
		return false, ""
	}
	if entry.Unrestricted {
		return true, ""
	}
	for _, scope := range entry.Scopes {
		if permissionScopeSatisfied(convID, scope, actx) {
			return true, permissionScopeDisplay(scope)
		}
	}
	return false, ""
}

func memberPermissionPermitted(convID, slug string, actx ActionContext) bool {
	if !IsMemberImpliedSlug(slug) || actx.structuralGroup == "" {
		return false
	}
	g, err := db.GetAgentGroupByName(actx.structuralGroup)
	if err != nil || g == nil || g.IsArchived() {
		return false
	}
	member, err := db.FindMemberInGroup(g.ID, convID)
	return err == nil && member != nil
}

func structuralPermissionMatch(convID, slug string, actx ActionContext) (bool, string) {
	if ok, scope := ownerPermissionMatch(convID, slug, actx); ok {
		return true, scope
	}
	return memberPermissionPermitted(convID, slug, actx), ""
}

// ownerTierEntry is what group ownership confers on one agent for one slug,
// AFTER per-group narrowing, as the effective-permissions listing renders it
// and as attenuation reads it.
//
// Unrestricted means at least one owned group confers the slug with no
// narrowing — union with unrestricted is unrestricted, the same rule the grant
// tiers apply to an unscoped row. Otherwise Scopes is the union of the
// narrowings across owned groups, and the owner may act within any one of them.
//
// Degraded records that some owned group's contribution could NOT be
// determined — its row would not read, or its map does not decode on this
// build. Such a group confers nothing at the gate, and this flag is what stops
// that from reading as "this agent has no owner-shape at all" in attenuation,
// which would be the widest possible answer to a question the daemon just
// failed to answer. It is exactly the treatment granterScopesForSlug already
// gives a grant tier whose every row is undecodable.
type ownerTierEntry struct {
	Unrestricted bool
	Degraded     bool
	Scopes       []PermissionScope
}

// confers reports whether the entry actually authorizes anything. A
// Degraded-only entry does not: the gate refuses it, so the listing must not
// report the slug as effective (listing == gate).
func (e ownerTierEntry) confers() bool {
	return e.Unrestricted || len(e.Scopes) > 0
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

// satisfiedBy answers, at the TIER level, whether ownership authorizes slug for
// this action. The gate never calls it — it walks the owned groups one at a
// time, which is what lets it pick the right group's map — so this exists to
// pin the tier's union semantics against that walk in tests. Keep the two
// answering the same question.
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
// A group whose map is undecodable contributes NOTHING to what the owner may
// do (it cannot widen and must not silently confer), matching what the gate
// does with the same row — but it is recorded as Degraded rather than simply
// skipped. "We could not read this group's narrowing" and "this agent owns
// nothing" are the same shape and must not be the same answer: the second is
// legitimately unconstrained for delegation, the first is a failure that would
// otherwise hand the owner an unbounded conferral through the very map that
// was supposed to bound it.
//
// An agent that owns NOTHING still yields a nil tier — the pre-existing
// reading, and the one delegation depends on (permissions.grant is recursive).
func ownerImpliedTierFor(convID string) ownerImpliedTier {
	implied := OwnerImpliedSlugs()
	degradedTier := func() ownerImpliedTier {
		tier := ownerImpliedTier{}
		for _, slug := range implied {
			tier[slug] = ownerTierEntry{Degraded: true}
		}
		return tier
	}
	owned, err := db.ListOwnedGroupScopes(convID)
	if err != nil {
		// We do not know WHETHER this agent owns groups, let alone what they
		// narrow. Degrading to "not an owner" would be a guess in the widening
		// direction for every owner-implied slug.
		slog.Warn("permissions: owned-group lookup failed (owner tier degraded)",
			"conv", convID, "error", err)
		return degradedTier()
	}
	if len(owned) == 0 {
		return nil
	}
	tier := ownerImpliedTier{}
	for _, g := range owned {
		scopes, err := ownerScopesForEval(g.OwnerScopesJSON)
		degraded := err != nil
		if degraded {
			slog.Warn("permissions: undecodable group owner scopes (owner tier degraded)",
				"group", g.Name, "error", err)
		}
		for _, slug := range implied {
			entry := tier[slug]
			if degraded {
				entry.Degraded = true
				tier[slug] = entry
				continue
			}
			constraint := scopes[slug]
			if containsScopeDim(permissionScopeDimsForSlug(slug), ScopeDimGroup) {
				scope, ok := ownerDerivedGroupScope(g.Name, constraint)
				if ok {
					entry.Scopes = append(entry.Scopes, scope)
				}
			} else if len(constraint) == 0 {
				entry.Unrestricted = true
				entry.Scopes = nil
			} else if !entry.Unrestricted {
				entry.Scopes = append(entry.Scopes, clonePermissionScope(constraint))
			}
			tier[slug] = entry
		}
	}
	return tier
}

// ownerDerivedGroupScope intersects the mandatory owned-group boundary with
// the group's optional owner constraint. A conflicting explicit group matcher
// contributes no grant; it can never widen ownership to another group.
func ownerDerivedGroupScope(group string, constraint PermissionScope) (PermissionScope, bool) {
	out := clonePermissionScope(constraint)
	if matchers, constrained := out[ScopeDimGroup]; constrained {
		matched := false
		for _, matcher := range matchers {
			if matcher == group {
				matched = true
				break
			}
		}
		if !matched {
			return nil, false
		}
	}
	out[ScopeDimGroup] = []string{group}
	return out, true
}

func clonePermissionScope(in PermissionScope) PermissionScope {
	out := make(PermissionScope, len(in)+1)
	for dim, matchers := range in {
		out[dim] = append([]string(nil), matchers...)
	}
	return out
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
