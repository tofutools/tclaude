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
    for (const [key, grant] of flattened.filesystem) filesystem.set(key, grant);
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
  // Keyed on the SANDBOX path, because that is the position a rule occupies
  // inside the namespace and therefore what another rule would override. Keying
  // on the host path would collapse two mounts of one directory into a single
  // preview line, silently dropping an authored grant (TCL-866).
  for (const grant of profile.filesystem || []) {
    filesystem.set(sandboxGrantKey(grant), {
      access: grant.access, path: grant.path, mountPath: grant.mount_path || '',
    });
  }
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
// sandboxGrantKey identifies a rule by the position it occupies inside the
// sandbox. For a rule with no mount_path that is its host path, so composition
// is unchanged for every profile authored before TCL-866.
function sandboxGrantKey(grant) {
  const mountPath = (grant.mount_path || grant.mountPath || '').trim();
  return mountPath || grant.path;
}

// sandboxGrantLabel renders one rule's paths: "<sandbox> ← <host>" when the two
// differ, and the single path otherwise.
function sandboxGrantLabel(grant) {
  const mountPath = (grant.mountPath || '').trim();
  return mountPath && mountPath !== grant.path
    ? `${mountPath} ← ${grant.path}` : grant.path;
}

// that. The daemon remains authoritative; this only previews.
export function composeSandboxProfilePolicy(applied, byName = {}) {
  const filesystem = new Map();
  const environment = new Map();
  const owned = new Map();
  let network = '';
  const state = { memo: new Map(), onPath: new Set(), problems: new Set() };
  for (const { scope, profile } of applied) {
    const flattened = flattenProfile(profile, byName, state);
    for (const [key, grant] of flattened.filesystem) {
      const previous = filesystem.get(key);
      const rank = { read: 0, write: 1, deny: 2 };
      if (!previous || rank[grant.access] >= rank[previous.access]) {
        filesystem.set(key, { ...grant, scope });
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
  // A remapped grant names the path the agent will actually see, then the host
  // directory the authority came from. Printing the host path alone would name
  // the one path the agent will NOT see.
  const grants = [...filesystem]
    .map(([, value]) => `${value.access} ${sandboxGrantLabel(value)} (${value.scope})`).join(' · ');
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
