import { registerFeatureState } from './feature-state-registry.js';

function requireOptions({ name, label, hosts, load }) {
  if (typeof name !== 'string' || !name) throw new TypeError('island name is required');
  if (typeof label !== 'string' || !label) throw new TypeError(`island ${name} requires a label`);
  if (!Array.isArray(hosts) || hosts.length === 0 || hosts.some((host) => !host?.dataset)) {
    throw new TypeError(`island ${name} requires element hosts`);
  }
  if (new Set(hosts).size !== hosts.length) throw new TypeError(`island ${name} hosts must be unique`);
  if (typeof load !== 'function') throw new TypeError(`island ${name} requires a loader`);
}

function claimHosts(name, hosts) {
  for (const host of hosts) {
    const owner = host.dataset.islandOwner;
    if (owner) throw new Error(`island host already owned by ${owner}`);
  }
  for (const host of hosts) host.dataset.islandOwner = name;
}

function releaseHosts(name, hosts) {
  for (const host of hosts) {
    if (host.dataset.islandOwner === name) delete host.dataset.islandOwner;
  }
}

export function renderIslandLoadFailure(host, { label, error, className = 'island-error' }) {
  host.replaceChildren();
  const failure = document.createElement('div');
  failure.className = className;
  failure.setAttribute('role', 'alert');
  failure.textContent = `${label} failed to load: ${error?.message || error}`;
  host.append(failure);
}

// The core dashboard imports this dependency-free lifecycle, while each
// feature's Preact graph stays behind its load callback. A missing optional
// feature asset therefore fails inside its claimed subtree without preventing
// the legacy dashboard module graph from linking.
export async function mountFeatureIsland({
  name, label, hosts, load, failureClass, logger = console,
}) {
  requireOptions({ name, label, hosts, load });
  claimHosts(name, hosts);
  let unregister = null;
  const cleanups = [];
  const registerCleanup = (cleanup) => {
    if (typeof cleanup !== 'function') {
      throw new TypeError(`island ${name} cleanup must be a function`);
    }
    cleanups.push(cleanup);
  };
  const runCleanups = (steps = cleanups.slice().reverse()) => {
    const failures = [];
    for (const cleanup of steps) {
      try { cleanup(); } catch (error) { failures.push({ cleanup, error }); }
    }
    return failures;
  };
  try {
    const feature = await load();
    if (!feature || typeof feature.mount !== 'function') {
      throw new TypeError(`island ${name} loader must return a mount function`);
    }
    if (feature.state) unregister = registerFeatureState(name, feature.state);
    feature.mount(registerCleanup);
    if (cleanups.length === 0) throw new TypeError(`island ${name} mount must register cleanup`);
    let cleaned = false;
    return () => {
      if (cleaned) return;
      const failures = runCleanups();
      unregister?.();
      if (failures.length > 0) {
        throw new AggregateError(failures.map(({ error }) => error), `island ${name} cleanup failed`);
      }
      releaseHosts(name, hosts);
      cleaned = true;
    };
  } catch (error) {
    // Idempotent cleanup may fail transiently because a later-created resource
    // had not finished unwinding yet. After every step has had one attempt,
    // retry only failures once; permanent failures keep host ownership claimed.
    let rollbackFailures = runCleanups();
    if (rollbackFailures.length > 0) {
      rollbackFailures = runCleanups(rollbackFailures.map(({ cleanup }) => cleanup));
    }
    unregister?.();
    for (const host of hosts.slice(1)) host.replaceChildren();
    renderIslandLoadFailure(hosts[0], { label, error, className: failureClass });
    logger.error(`${label} island unavailable.`, error);
    for (const { error: cleanupError } of rollbackFailures) {
      logger.error(`${label} island rollback failed.`, cleanupError);
    }
    return null;
  }
}
