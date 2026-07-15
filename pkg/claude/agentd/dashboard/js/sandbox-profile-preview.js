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

export function composeSandboxProfilePreview(applied, byName = {}) {
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
  const scopes = applied.map((item) => `${item.scope}:${item.profile.name}`).join(' → ')
    || 'no profiles applied';
  const grants = [...filesystem]
    .map(([path, value]) => `${value.access} ${path} (${value.scope})`).join(' · ');
  const keys = [...environment].map(([name, scope]) => `${name} (${scope})`).join(', ');
  const ownedKeys = [...owned].map(([name, scope]) => `${name} (${scope})`).join(', ');
  const problems = state.problems.size
    ? ` · ⚠ unresolved includes: ${[...state.problems].sort().join(', ')}` : '';
  return `${scopes}${grants ? ` · ${grants}` : ''}${keys ? ` · env: ${keys}` : ''}`
    + `${ownedKeys ? ` · agent dirs: ${ownedKeys}` : ''}`
    + `${network ? ` · network: ${network}` : ''}${problems}`;
}
