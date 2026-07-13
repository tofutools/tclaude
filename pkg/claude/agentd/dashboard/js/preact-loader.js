// This source module intentionally has no Preact imports. A missing or broken
// runtime module must not prevent the legacy dashboard module graph from
// linking; future feature islands can use the same load-then-claim boundary.
import { createIslandDescriptor, mountFeatureIsland, mountIslandDescriptor } from './island-lifecycle.js';

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

const groupsDescriptor = createIslandDescriptor({
  name: 'groups',
  label: 'Groups',
  hosts: { filterHost: '#groups-filter-root', listHost: '#groups-list' },
  failureClass: 'groups-error',
  load: async ({ hosts: { filterHost, listHost }, dependencies }) => {
    const islandModule = import('./groups-island.js');
    const stateModule = import('./groups-state.js');
    const actionsModule = import('./groups-actions.js');
    const renderModule = import('./render.js');
    const [
      { mountGroupsIsland }, { groupsState }, { createGroupsActions }, { renderGroups },
    ] = await Promise.all([islandModule, stateModule, actionsModule, renderModule]);
    const actions = createGroupsActions({ state: groupsState, ...dependencies });
    return {
      state: groupsState,
      mount: (registerCleanup) => mountGroupsIsland({
        filterHost, listHost, state: groupsState, actions,
        renderGroupsHTML: renderGroups, registerCleanup,
      }),
    };
  },
});

export function mountGroupsFeature(dependencies = {}) {
  return mountIslandDescriptor(groupsDescriptor, dependencies);
}

// The Jobs pilot is the first production island. Keep the dynamic boundary so
// a corrupt optional asset produces a visible feature-local error rather than
// preventing the rest of the dashboard entry module from booting.
const jobsDescriptor = createIslandDescriptor({
    name: 'jobs',
    label: 'Jobs',
    hosts: { host: '#jobs-root', badgeHost: '#jobs-badge-root' },
    failureClass: 'jobs-error',
    load: async ({ hosts: { host, badgeHost }, dependencies: actionDependencies }) => {
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

export async function mountJobsFeature(actionDependencies) {
  return mountIslandDescriptor(jobsDescriptor, actionDependencies);
}

// Plugins is the second bounded migration and follows the same guarded
// lifecycle as Jobs. Its tab, nav badge, and modal are one ownership unit.
const pluginsDescriptor = createIslandDescriptor({
    name: 'plugins',
    label: 'Plugins',
    hosts: {
      host: '#plugins-root', badgeHost: '#plugins-badge-root', modalHost: '#plugins-modal-root',
    },
    failureClass: 'plugins-error',
    load: async ({ hosts: { host, badgeHost, modalHost }, dependencies: actionDependencies }) => {
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

export async function mountPluginsFeature(actionDependencies) {
  return mountIslandDescriptor(pluginsDescriptor, actionDependencies);
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

export async function mountAccessFeature(actionDependencies) {
  const host = document.querySelector('#access-root');
  if (!host) return null;
  return mountFeatureIsland({
    name: 'access', label: 'Access', hosts: [host], failureClass: 'access-error',
    load: async () => {
      const islandModule = import('./access-island.js');
      const stateModule = import('./access-state.js');
      const actionsModule = import('./access-actions.js');
      const [{ mountAccessIsland }, { accessState }, { createAccessActions }] =
        await Promise.all([islandModule, stateModule, actionsModule]);
      const accessActions = createAccessActions({ ...actionDependencies, state: accessState });
      return {
        state: accessState,
        mount: (registerCleanup) => mountAccessIsland({
          host, state: accessState, actions: accessActions, registerCleanup,
        }),
      };
    },
  });
}

const messagesDescriptor = createIslandDescriptor({
  name: 'messages', label: 'Messages', hosts: { host: '#messages-root' }, failureClass: 'messages-error',
  load: async ({ hosts: { host } }) => {
    const islandModule = import('./mail-island.js');
    const legacyModule = import('./mail.js');
    const [{ mountMailIsland }, { mailController }] = await Promise.all([islandModule, legacyModule]);
    return {
      state: mailController.state,
      mount: (registerCleanup) => mountMailIsland({ host, controller: mailController, registerCleanup }),
    };
  },
});

export function mountMessagesFeature(dependencies = {}) {
  return mountIslandDescriptor(messagesDescriptor, dependencies);
}

export async function mountLogsFeature(actionDependencies = {}) {
  const host = document.querySelector('#logs-root');
  if (!host) return null;
  return mountFeatureIsland({
    name: 'logs', label: 'Logs', hosts: [host], failureClass: 'logs-error',
    load: async () => {
      const islandModule = import('./logs-island.js');
      const stateModule = import('./logs-state.js');
      const actionsModule = import('./logs-actions.js');
      const [{ mountLogsIsland }, { logsState }, { createLogsActions }] =
        await Promise.all([islandModule, stateModule, actionsModule]);
      const logsActions = createLogsActions({ state: logsState, ...actionDependencies });
      return {
        state: logsState,
        mount: (registerCleanup) => mountLogsIsland({
          host, state: logsState, actions: logsActions, registerCleanup,
        }),
      };
    },
  });
}

export async function mountAuditFeature(actionDependencies = {}) {
  const host = document.querySelector('#audit-root');
  if (!host) return null;
  return mountFeatureIsland({
    name: 'audit', label: 'Audit', hosts: [host], failureClass: 'audit-error',
    load: async () => {
      const islandModule = import('./audit-island.js');
      const stateModule = import('./audit-state.js');
      const actionsModule = import('./audit-actions.js');
      const [{ mountAuditIsland }, { auditState }, { createAuditActions }] = await Promise.all([islandModule, stateModule, actionsModule]);
      const auditActions = createAuditActions({ state: auditState, ...actionDependencies });
      return { state: auditState, mount: (registerCleanup) => mountAuditIsland({ host, state: auditState, actions: auditActions, registerCleanup }) };
    },
  });
}

export async function mountConfigFeature(dependencies = {}) {
  const host = document.querySelector('#config-root');
  if (!host) return null;
  return mountFeatureIsland({
    name: 'config', label: 'Config', hosts: [host], failureClass: 'config-error',
    load: async () => {
      const islandModule = import('./config-island.js');
      const stateModule = import('./config-state.js');
      const [{ mountConfigIsland }, { configState }] = await Promise.all([islandModule, stateModule]);
      return { state: configState, mount: (registerCleanup) => mountConfigIsland({ host, state: configState, dependencies, registerCleanup }) };
    },
  });
}

const processesDescriptor = createIslandDescriptor({
  name: 'processes', label: 'Processes', hosts: { host: '#processes-root' }, failureClass: 'processes-error',
  load: async ({ hosts: { host }, dependencies }) => {
    const islandModule = import('./processes-island.js');
    const stateModule = import('./processes-state.js');
    const actionsModule = import('./processes-actions.js');
    const [{ mountProcessesIsland }, { processesState }, { createProcessesActions }] =
      await Promise.all([islandModule, stateModule, actionsModule]);
    const actions = createProcessesActions({ state: processesState, ...dependencies });
    return { state: processesState, mount: (registerCleanup) => mountProcessesIsland({ host, state: processesState, actions, registerCleanup }) };
  },
});

export function mountProcessesFeature(dependencies = {}) {
  return mountIslandDescriptor(processesDescriptor, dependencies);
}

const managementDescriptor = createIslandDescriptor({
  name: 'management', label: 'Management', hosts: { root: '#management-root' }, primaryHost: 'root', failureClass: 'management-error',
  load: async ({ hosts, dependencies }) => {
    const islandModule = import('./management-island.js');
    const stateModule = import('./management-state.js');
    const actionsModule = import('./management-actions.js');
    const [{ mountManagementIsland }, { createManagementState }, { createManagementActions }] = await Promise.all([islandModule, stateModule, actionsModule]);
    const state = createManagementState(); const actions = createManagementActions({ state, ...dependencies });
    return { state, mount: (registerCleanup) => {
      mountManagementIsland({ host: hosts.root, state, actions, registerCleanup, ...dependencies });
    } };
  },
});

export function mountManagementFeature(dependencies = {}) {
  return mountIslandDescriptor(managementDescriptor, dependencies);
}
