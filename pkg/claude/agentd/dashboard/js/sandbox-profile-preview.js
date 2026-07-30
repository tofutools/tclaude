import { sandboxProfileLayersInline } from './resolved-defaults.js';

function flattenProfile(profile, byName, state) {
  const filesystem = new Map();
  const environment = new Map();
  const owned = new Map();
  let network = '';
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
  if (profile.network_access) network = profile.network_access;
  return { filesystem, environment, owned, network };
}

// composeSandboxProfilePolicy mirrors the daemon's composition semantics for
// the client-side preview: the deny > write > read lattice for ordinary
// grants. Strictness is expressed entirely through ordinary deny rows plus
// narrower read/write reopens, so there is no separate read-baseline
// mechanism to compose. Profiles that still carry retired JSON fields — the
// read-baseline pair, and the protected-access grant TCL-791 removed — are
// ignored here rather than rendered: the daemon refuses the latter outright at
// every input surface, and preview composition is not the place to explain
// that. The daemon remains authoritative; this only previews.
export function composeSandboxProfilePolicy(applied, byName = {}) {
  const filesystem = new Map();
  const environment = new Map();
  const owned = new Map();
  let network = '';
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
    if (flattened.network) network = `${flattened.network} (${scope})`;
  }
  // Same phrasing as the sandbox-profile editor's always-visible layer row, so
  // the launch dialog and the editor describe one composition in one vocabulary.
  const layerContext = {};
  for (const { scope, profile } of applied) layerContext[scope] = profile.name;
  const scopes = sandboxProfileLayersInline(
    layerContext, 'none — no sandbox profile applies to this launch',
  );
  const grants = [...filesystem]
    .map(([path, value]) => `${value.access} ${path} (${value.scope})`).join(' · ');
  const keys = [...environment].map(([name, scope]) => `${name} (${scope})`).join(', ');
  const ownedKeys = [...owned].map(([name, scope]) => `${name} (${scope})`).join(', ');
  // The ⚠ prefix below is load-bearing: HelpField lifts everything from the
  // first ⚠ onward into an always-visible caveat, so an unresolved include
  // stays on screen in the spawn dialog instead of collapsing into the [?]
  // disclosure.
  const problems = state.problems.size
    ? ` · ⚠ unresolved includes: ${[...state.problems].sort().join(', ')}` : '';
  const text = `${scopes}${grants ? ` · ${grants}` : ''}${keys ? ` · env: ${keys}` : ''}`
    + `${ownedKeys ? ` · agent dirs: ${ownedKeys}` : ''}`
    + `${network ? ` · network: ${network}` : ''}${problems}`;
  return { text };
}

export function composeSandboxProfilePreview(applied, byName = {}) {
  return composeSandboxProfilePolicy(applied, byName).text;
}
