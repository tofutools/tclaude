package db

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// PermissionOverride is ONE birth-time permission override: an effect plus an
// optional scope. It is the value type of every blueprint that seeds an
// agent's permanent per-slug overrides at birth — a spawn profile, a
// template-local inline profile, a pending-spawn row, a spawn request.
//
// It exists as a struct rather than a bare effect string so a scope can never
// be dropped by a caller that copies only "the effect". Dropping a scope
// WIDENS the grant, which is the one direction a permission shape must never
// silently drift in.
//
// Wire/storage shape is deliberately a union, and the unscoped arm is the
// pre-scope shape byte-for-byte:
//
//	"grant"                                  // unscoped (every legacy row)
//	{"effect":"grant","scope":{"group":["a"]}} // scoped
//
// so every stored column, export envelope and API payload written before
// scopes existed still decodes, and re-encoding an unscoped override produces
// exactly what it produced before.
type PermissionOverride struct {
	// Effect is PermEffectGrant or PermEffectDeny.
	Effect string
	// Scope is the CANONICAL permission-scope JSON, "" for an unscoped
	// override. This layer stores it verbatim: validation and
	// canonicalization belong to the agentd wire boundary, which owns the
	// dimension registry. A deny never carries one (a deny is unconditional
	// by design — see the scoped-permissions design doc).
	Scope string
}

// Grant builds an unscoped grant override — the shape every pre-scope caller
// meant when it wrote map[string]string{slug: "grant"}.
func Grant() PermissionOverride { return PermissionOverride{Effect: PermEffectGrant} }

// Deny builds a deny override. Denies never carry a scope.
func Deny() PermissionOverride { return PermissionOverride{Effect: PermEffectDeny} }

// UnscopedOverride wraps a bare effect string.
func UnscopedOverride(effect string) PermissionOverride {
	return PermissionOverride{Effect: effect}
}

// ScopedOverride wraps an effect plus an already-canonical scope JSON.
func ScopedOverride(effect, scope string) PermissionOverride {
	return PermissionOverride{Effect: effect, Scope: scope}
}

// UnscopedOverrides lifts a legacy slug→effect map into the scoped shape. It is
// the bridge for callers (and tests) that genuinely have no scope to carry.
func UnscopedOverrides(in map[string]string) map[string]PermissionOverride {
	if in == nil {
		return nil
	}
	out := make(map[string]PermissionOverride, len(in))
	for slug, effect := range in {
		out[slug] = PermissionOverride{Effect: effect}
	}
	return out
}

// OverrideEffects projects the scoped shape back down to slug→effect, for
// readers that genuinely only care about the effect (comparisons in the
// dashboard editor, audit summaries, "which slugs does this blueprint touch").
// Never use it on a WRITE path: it discards scopes.
func OverrideEffects(in map[string]PermissionOverride) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for slug, override := range in {
		out[slug] = override.Effect
	}
	return out
}

// SortedOverrideSlugs returns the map's slugs in a deterministic order, so a
// deploy/grant report reads the same on every run (Go map order is not).
func SortedOverrideSlugs(in map[string]PermissionOverride) []string {
	out := make([]string, 0, len(in))
	for slug := range in {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}

// Display renders an override for a human-facing one-line summary: "grant"
// for the unscoped form, "grant[group=a,b spawn_profile=p1]" when scoped. It
// mirrors agentd's permissionScopeDisplay so a CLI listing and the daemon's
// provenance column read the same, without this layer taking a dependency on
// the dimension registry that lives up there.
func (o PermissionOverride) Display() string {
	if o.Scope == "" {
		return o.Effect
	}
	var dims map[string][]string
	if err := json.Unmarshal([]byte(o.Scope), &dims); err != nil || len(dims) == 0 {
		// Say the scope is there but unreadable rather than rendering the
		// override as unscoped, which would read as strictly more authority.
		return o.Effect + "[unreadable scope]"
	}
	names := make([]string, 0, len(dims))
	for dim := range dims {
		names = append(names, dim)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, dim := range names {
		parts = append(parts, dim+"="+strings.Join(dims[dim], ","))
	}
	return o.Effect + "[" + strings.Join(parts, " ") + "]"
}

// PermissionGrant is a slug plus an optional scope — the LIST form of the same
// idea, used where a blueprint carries default grants rather than tri-state
// overrides (a role's default permission set, a template agent's legacy inline
// grants). Only grants live in these lists, so there is no effect to carry.
//
// Its JSON is the same union, and its unscoped arm is the bare slug string
// every stored list already holds:
//
//	"human.notify"                                    // unscoped
//	{"slug":"routes.publish","scope":{"group":["a"]}} // scoped
type PermissionGrant struct {
	Slug string
	// Scope is the canonical permission-scope JSON, "" for unscoped. Same
	// contract as PermissionOverride.Scope: validated at the wire boundary,
	// stored verbatim here.
	Scope string
}

// UnscopedGrants lifts a bare slug list into the scoped shape.
func UnscopedGrants(slugs []string) []PermissionGrant {
	if slugs == nil {
		return nil
	}
	out := make([]PermissionGrant, 0, len(slugs))
	for _, slug := range slugs {
		out = append(out, PermissionGrant{Slug: slug})
	}
	return out
}

// GrantSlugs projects the list back down to bare slugs, for readers that only
// need to know WHICH permissions a blueprint mentions (counts, one-line
// summaries, membership checks). Never use it on a WRITE path: it discards
// scopes.
func GrantSlugs(grants []PermissionGrant) []string {
	out := make([]string, 0, len(grants))
	for _, grant := range grants {
		out = append(out, grant.Slug)
	}
	return out
}

// Display renders one grant for a human-facing summary: the bare slug when
// unscoped, "slug[group=a,b]" when scoped.
func (g PermissionGrant) Display() string {
	if g.Scope == "" {
		return g.Slug
	}
	return g.Slug + PermissionOverride{Effect: "", Scope: g.Scope}.Display()
}

type permissionGrantJSON struct {
	Slug  string          `json:"slug"`
	Scope json.RawMessage `json:"scope,omitempty"`
}

func (g PermissionGrant) MarshalJSON() ([]byte, error) {
	if g.Scope == "" {
		return json.Marshal(g.Slug)
	}
	if !json.Valid([]byte(g.Scope)) {
		// Same fail-closed reasoning as PermissionOverride: emitting the bare
		// slug would publish the grant as unscoped, silently widening it.
		return nil, fmt.Errorf("permission grant %q carries invalid scope JSON", g.Slug)
	}
	return json.Marshal(permissionGrantJSON{Slug: g.Slug, Scope: json.RawMessage(g.Scope)})
}

func (g *PermissionGrant) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		*g = PermissionGrant{}
		return nil
	}
	if trimmed[0] == '"' {
		var slug string
		if err := json.Unmarshal(b, &slug); err != nil {
			return err
		}
		*g = PermissionGrant{Slug: slug}
		return nil
	}
	var wire permissionGrantJSON
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	scope := strings.TrimSpace(string(wire.Scope))
	if scope == "null" || scope == "{}" {
		scope = ""
	}
	*g = PermissionGrant{Slug: wire.Slug, Scope: scope}
	return nil
}

// permissionOverrideJSON is the object arm of the union.
type permissionOverrideJSON struct {
	Effect string          `json:"effect"`
	Scope  json.RawMessage `json:"scope,omitempty"`
}

// MarshalJSON emits the bare-string arm for an unscoped override so a
// blueprint that never used scopes serializes exactly as it always did.
func (o PermissionOverride) MarshalJSON() ([]byte, error) {
	if o.Scope == "" {
		return json.Marshal(o.Effect)
	}
	if !json.Valid([]byte(o.Scope)) {
		// Refusing is the fail-closed answer: emitting the bare effect
		// instead would publish the grant as UNSCOPED, silently widening it.
		return nil, fmt.Errorf("permission override %q carries invalid scope JSON", o.Effect)
	}
	return json.Marshal(permissionOverrideJSON{Effect: o.Effect, Scope: json.RawMessage(o.Scope)})
}

// UnmarshalJSON accepts both arms of the union.
func (o *PermissionOverride) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		*o = PermissionOverride{}
		return nil
	}
	if trimmed[0] == '"' {
		var effect string
		if err := json.Unmarshal(b, &effect); err != nil {
			return err
		}
		*o = PermissionOverride{Effect: effect}
		return nil
	}
	var wire permissionOverrideJSON
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	scope := strings.TrimSpace(string(wire.Scope))
	if scope == "null" || scope == "{}" {
		scope = ""
	}
	*o = PermissionOverride{Effect: wire.Effect, Scope: scope}
	return nil
}
