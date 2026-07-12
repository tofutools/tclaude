// This source module intentionally has no Preact imports. A missing or broken
// runtime module must not prevent the legacy dashboard module graph from
// linking; future feature islands can use the same load-then-claim boundary.
import { mountFeatureIsland } from './island-lifecycle.js';

export async function mountPreactRuntimeProbe() {
  const host = document.createElement('span');
  host.id = 'preact-runtime-probe';
  host.hidden = true;
  host.dataset.state = 'loading';
  document.body.append(host);

  try {
    const { mountPreactProbe } = await import('./preact-probe.js');
    mountPreactProbe(host);
    // Preact and Signals schedule their render flush in microtasks. Two turns
    // prove both the initial render and the signal-driven update completed.
    await Promise.resolve();
    await Promise.resolve();
    const probe = host.querySelector('[data-preact-probe="ready"]');
    if (!probe || probe.textContent !== 'ready') {
      throw new Error('Preact/Signals runtime probe did not become ready');
    }
    host.dataset.state = 'ready';
  } catch (error) {
    host.dataset.state = 'failed';
    console.warn('Preact runtime probe unavailable; legacy dashboard remains active.', error);
  }
}

// The Jobs pilot is the first production island. Keep the dynamic boundary so
// a corrupt optional asset produces a visible feature-local error rather than
// preventing the rest of the dashboard entry module from booting.
export async function mountJobsFeature(actionDependencies) {
  const host = document.querySelector('#jobs-root');
  const badgeHost = document.querySelector('#jobs-badge-root');
  if (!host || !badgeHost) return null;
  return mountFeatureIsland({
    name: 'jobs',
    label: 'Jobs',
    hosts: [host, badgeHost],
    failureClass: 'jobs-error',
    load: async () => {
      // Keep import() behind named promises: the repository's intentionally
      // small module-graph scanner recognizes dynamic imports in expressions,
      // while a bare import(...) line is intentionally rejected as ambiguous.
      const islandModule = import('./jobs-island.js');
      const stateModule = import('./jobs-state.js');
      const actionsModule = import('./jobs-actions.js');
      const [{ mountJobsIsland }, { jobsState }, { createJobsActions }] =
        await Promise.all([islandModule, stateModule, actionsModule]);
      const jobsActions = createJobsActions(actionDependencies);
      return {
        state: jobsState,
        mount: (registerCleanup) => mountJobsIsland({
          host, badgeHost, state: jobsState, actions: jobsActions, registerCleanup,
        }),
      };
    },
  });
}

// Plugins is the second bounded migration and follows the same guarded
// lifecycle as Jobs. Its tab, nav badge, and modal are one ownership unit.
export async function mountPluginsFeature(actionDependencies) {
  const host = document.querySelector('#plugins-root');
  const badgeHost = document.querySelector('#plugins-badge-root');
  const modalHost = document.querySelector('#plugins-modal-root');
  if (!host || !badgeHost || !modalHost) return null;
  return mountFeatureIsland({
    name: 'plugins',
    label: 'Plugins',
    hosts: [host, badgeHost, modalHost],
    failureClass: 'plugins-error',
    load: async () => {
      const islandModule = import('./plugins-island.js');
      const stateModule = import('./plugins-state.js');
      const actionsModule = import('./plugins-actions.js');
      const [{ mountPluginsIsland }, { pluginsState }, { createPluginsActions }] =
        await Promise.all([islandModule, stateModule, actionsModule]);
      const pluginsActions = createPluginsActions({ ...actionDependencies, state: pluginsState });
      return {
        state: pluginsState,
        mount: (registerCleanup) => mountPluginsIsland({
          host, badgeHost, modalHost, state: pluginsState, actions: pluginsActions, registerCleanup,
        }),
      };
    },
  });
}

export async function mountCostsFeature(actionDependencies = {}) {
  const host = document.querySelector('#costs-root');
  if (!host) return null;
  return mountFeatureIsland({
    name: 'costs', label: 'Costs', hosts: [host], failureClass: 'costs-error',
    load: async () => {
      const islandModule = import('./costs-island.js');
      const stateModule = import('./costs-state.js');
      const actionsModule = import('./costs-actions.js');
      const [{ mountCostsIsland }, { costsState }, { createCostsActions }] =
        await Promise.all([islandModule, stateModule, actionsModule]);
      const costsActions = createCostsActions({ state: costsState, ...actionDependencies });
      return {
        state: costsState,
        mount: (registerCleanup) => mountCostsIsland({
          host, state: costsState, actions: costsActions, registerCleanup,
        }),
      };
    },
  });
}
