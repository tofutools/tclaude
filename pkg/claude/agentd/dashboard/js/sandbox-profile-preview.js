function mergeBreakGlass(target, path, access, origins) {
  const existing = target.get(path);
  const merged = existing
    ? { access: existing.access === 'write' || access === 'write' ? 'write' : 'read', origins: [...existing.origins] }
    : { access, origins: [] };
  for (const origin of origins) {
    if (!merged.origins.includes(origin)) merged.origins.push(origin);
  }
  target.set(path, merged);
}

function flattenProfile(profile, byName, state) {
  const filesystem = new Map();
  const environment = new Map();
  const owned = new Map();
  const breakGlass = new Map();
  const readExclusions = new Map();
  let network = '';
  let readBaseline = '';
  let readBaselineOrigin = '';
  state.onPath.add(profile.name);
  for (const name of profile.includes || []) {
    if (state.onPath.has(name)) {
      state.problems.add(name);
      continue;
    }
    let flattened = state.memo.get(name);
    if (!flattened) {
      const included = byName[name];
      if (!included) {
        state.problems.add(name);
        continue;
      }
      flattened = flattenProfile(included, byName, state);
      state.memo.set(name, flattened);
    }
    for (const [path, access] of flattened.filesystem) filesystem.set(path, access);
    for (const name of flattened.environment.keys()) {
      owned.delete(name);
      environment.set(name, true);
    }
    for (const name of flattened.owned.keys()) {
      environment.delete(name);
      owned.set(name, true);
    }
    // Break-glass and the strict read baseline never fold into ordinary
    // override semantics: break-glass merges as a union (write dominates on
    // the same path) and minimal wins strictest, both keeping the profile
    // that introduced them so includes cannot hide the origin.
    for (const [path, entry] of flattened.breakGlass) mergeBreakGlass(breakGlass, path, entry.access, entry.origins);
    for (const [id, origins] of flattened.readExclusions) {
      const previous = readExclusions.get(id) || [];
      readExclusions.set(id, [...new Set([...previous, ...origins])]);
    }
    if (flattened.readBaseline === 'minimal' && readBaseline !== 'minimal') {
      readBaseline = 'minimal';
      readBaselineOrigin = flattened.readBaselineOrigin;
    }
    if (flattened.network) network = flattened.network;
  }
  state.onPath.delete(profile.name);
  for (const grant of profile.filesystem || []) filesystem.set(grant.path, grant.access);
  for (const entry of profile.environment || []) {
    owned.delete(entry.name);
    environment.set(entry.name, true);
  }
  for (const name of profile.agent_directories || []) {
    environment.delete(name);
    owned.set(name, true);
  }
  for (const rule of profile.break_glass_filesystem || []) mergeBreakGlass(breakGlass, rule.path, rule.access, [profile.name]);
  for (const id of profile.read_baseline_exclusions || []) {
    const previous = readExclusions.get(id) || [];
    if (!previous.includes(profile.name)) previous.push(profile.name);
    readExclusions.set(id, previous);
  }
  if (profile.read_baseline === 'minimal' && readBaseline !== 'minimal') {
    readBaseline = 'minimal';
    readBaselineOrigin = profile.name;
  }
  if (profile.network_access) network = profile.network_access;
  return { filesystem, environment, owned, network, breakGlass, readExclusions, readBaseline, readBaselineOrigin };
}

// composeSandboxProfilePolicy mirrors the daemon's composition semantics for
// the client-side preview: the deny > write > read lattice for ordinary
// grants, strictest-wins for read_baseline (any minimal layer makes the
// effective baseline minimal), and a provenance-preserving union for
// break_glass_filesystem where write dominates read on the same canonical
// path. The daemon remains authoritative; this only previews.
export function composeSandboxProfilePolicy(applied, byName = {}) {
  const filesystem = new Map();
  const environment = new Map();
  const owned = new Map();
  const breakGlass = new Map();
  const readExclusions = new Map();
  let network = '';
  let readBaseline = null;
  const state = { memo: new Map(), onPath: new Set(), problems: new Set() };
  for (const { scope, profile } of applied) {
    const flattened = flattenProfile(profile, byName, state);
    for (const [path, access] of flattened.filesystem) {
      const previous = filesystem.get(path);
      const rank = { read: 0, write: 1, deny: 2 };
      if (!previous || rank[access] >= rank[previous.access]) {
        filesystem.set(path, { access, scope });
      }
    }
    for (const name of flattened.environment.keys()) environment.set(name, scope);
    for (const name of flattened.owned.keys()) owned.set(name, scope);
    for (const [path, entry] of flattened.breakGlass) {
      mergeBreakGlass(breakGlass, path, entry.access, entry.origins.map((origin) => `${scope}:${origin}`));
    }
    for (const [id, origins] of flattened.readExclusions) {
      const scoped = origins.map((origin) => `${scope}:${origin}`);
      const previous = readExclusions.get(id) || [];
      readExclusions.set(id, [...new Set([...previous, ...scoped])]);
    }
    if (flattened.readBaseline === 'minimal' && !readBaseline) {
      readBaseline = { scope, profile: flattened.readBaselineOrigin };
    }
    if (flattened.network) network = `${flattened.network} (${scope})`;
  }
  const scopes = applied.map((item) => `${item.scope}:${item.profile.name}`).join(' → ')
    || 'no profiles applied';
  const grants = [...filesystem]
    .map(([path, value]) => `${value.access} ${path} (${value.scope})`).join(' · ');
  const keys = [...environment].map(([name, scope]) => `${name} (${scope})`).join(', ');
  const ownedKeys = [...owned].map(([name, scope]) => `${name} (${scope})`).join(', ');
  const breakGlassEntries = [...breakGlass].map(([path, entry]) => ({ path, access: entry.access, origins: entry.origins }));
  const readExclusionEntries = [...readExclusions].sort(([a], [b]) => a.localeCompare(b)).map(([id, origins]) => ({ id, origins }));
  const baseline = readBaseline
    ? ` · read baseline: minimal — strict (${readBaseline.scope}:${readBaseline.profile})` : '';
  const exclusions = readExclusionEntries.length
    ? ` · read restrictions: ${readExclusionEntries.map((entry) => `${entry.id} (${entry.origins.join(', ')})`).join(' · ')}` : '';
  // The ⚠ prefix is load-bearing: HelpField lifts everything from the first ⚠
  // onward into an always-visible caveat, so break-glass access stays on
  // screen in the spawn dialog instead of collapsing into the [?] disclosure.
  const breakGlassText = breakGlassEntries.length
    ? ` · ⚠ BREAK-GLASS protected access: ${breakGlassEntries
      .map((entry) => `${entry.access} ${entry.path} (${entry.origins.join(', ')})`).join(' · ')}`
      + ' — exposes protected tclaude/harness state (credentials, sessions, daemon state); exceptional debugging only'
    : '';
  const problems = state.problems.size
    ? ` · ⚠ unresolved includes: ${[...state.problems].sort().join(', ')}` : '';
  const text = `${scopes}${grants ? ` · ${grants}` : ''}${keys ? ` · env: ${keys}` : ''}`
    + `${ownedKeys ? ` · agent dirs: ${ownedKeys}` : ''}`
    + `${network ? ` · network: ${network}` : ''}${baseline}${exclusions}${breakGlassText}${problems}`;
  return { text, breakGlass: breakGlassEntries, readBaseline, readExclusions: readExclusionEntries };
}

export function composeSandboxProfilePreview(applied, byName = {}) {
  return composeSandboxProfilePolicy(applied, byName).text;
}
