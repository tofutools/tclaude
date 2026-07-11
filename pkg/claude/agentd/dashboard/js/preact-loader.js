// This source module intentionally has no Preact imports. A missing or broken
// runtime module must not prevent the legacy dashboard module graph from
// linking; future feature islands can use the same load-then-claim boundary.
import { registerFeatureState } from './feature-state-registry.js';

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
  try {
    // Keep import() behind named promises: the repository's intentionally
    // small module-graph scanner recognizes dynamic imports in expressions,
    // while a bare import(...) line is intentionally rejected as ambiguous.
    const islandModule = import('./jobs-island.js');
    const stateModule = import('./jobs-state.js');
    const actionsModule = import('./jobs-actions.js');
    const [{ mountJobsIsland }, { jobsState }, { createJobsActions }] =
      await Promise.all([islandModule, stateModule, actionsModule]);
    const jobsActions = createJobsActions(actionDependencies);
    const unregister = registerFeatureState('jobs', jobsState);
    try {
      const unmount = mountJobsIsland({ host, badgeHost, state: jobsState, actions: jobsActions });
      return () => {
        unmount();
        unregister();
      };
    } catch (error) {
      unregister();
      throw error;
    }
  } catch (error) {
    host.innerHTML = '';
    const failure = document.createElement('div');
    failure.className = 'jobs-error';
    failure.setAttribute('role', 'alert');
    failure.textContent = `Jobs failed to load: ${error?.message || error}`;
    host.append(failure);
    console.error('Jobs Preact island unavailable.', error);
    return null;
  }
}
