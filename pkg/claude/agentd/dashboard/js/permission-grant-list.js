// Blueprint grant lists — a role's default permissions, a template agent's
// inline grants. Each entry is either a bare slug string (every pre-scope
// blueprint, and everything the checkbox UIs author) or {slug, scope} when the
// grant was narrowed.
//
// These helpers exist so a UI that can only express "on / off" never has to
// look inside an entry. The rule they enforce is one-directional: a scope this
// UI cannot edit is CARRIED THROUGH untouched, never flattened to its bare
// slug. Flattening would silently widen a grant the operator deliberately
// narrowed — from a save they made for an unrelated reason.

export function grantSlug(entry) {
  return typeof entry === 'string' ? entry : String(entry?.slug || '');
}

export function grantScope(entry) {
  return typeof entry === 'string' ? null : entry?.scope || null;
}

// grantScopeLabel renders a scope the way the daemon's provenance line and the
// permission editor's chips do, so one grant reads identically everywhere.
export function grantScopeLabel(entry) {
  const scope = grantScope(entry);
  if (!scope) return '';
  return Object.entries(scope)
    .filter(([, values]) => (values || []).length)
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([dim, values]) => `${dim}=${values.join(',')}`)
    .join(' ');
}

export function hasGrant(list, slug) {
  return (list || []).some((entry) => grantSlug(entry) === slug);
}

// toggleGrant removes every entry for the slug, or appends a bare (unscoped)
// one. Removing by SLUG rather than by identity is what keeps a scoped entry
// from surviving as a hidden duplicate of a box the operator just unticked.
export function toggleGrant(list, slug) {
  const current = list || [];
  if (hasGrant(current, slug)) return current.filter((entry) => grantSlug(entry) !== slug);
  return [...current, slug];
}

// grantToOverride projects one grant entry onto the birth-time override union
// ("grant", or {effect, scope}) used by spawn profiles and inline profiles —
// the shape the "extract to profile" path writes. The scope travels with it.
export function grantToOverride(entry) {
  const scope = grantScope(entry);
  return scope ? { effect: 'grant', scope } : 'grant';
}

// grantListToOverrides and grantOverridesToList bridge blueprint permissions
// (roles/templates) to the shared buffered permission editor's override union.
// Blueprint permissions are grant-only, so a defensive deny in the returned
// map is ignored rather than leaking an unsupported effect into the role wire
// shape.
export function grantListToOverrides(list) {
  return Object.fromEntries((list || []).map((entry) => [grantSlug(entry), grantToOverride(entry)]));
}

export function grantOverridesToList(overrides) {
  return Object.entries(overrides || {})
    .filter(([, override]) => (typeof override === 'string' ? override : override?.effect) === 'grant')
    .map(([slug, override]) => {
      const scope = typeof override === 'object' && override ? override.scope : null;
      return scope && Object.keys(scope).length ? { slug, scope } : slug;
    });
}
