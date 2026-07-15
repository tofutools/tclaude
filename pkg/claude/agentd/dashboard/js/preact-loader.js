// This source module intentionally has no Preact imports. Each bounded feature
// loads its runtime graph only after claiming its stable hosts.
import { createIslandDescriptor, mountFeatureIsland, mountIslandDescriptor } from './island-lifecycle.js';

const shellDescriptor = createIslandDescriptor({
  name: 'shell',
  label: 'Dashboard shell',
  hosts: {
    activityHost: '#shell-activity-root',
    usageHost: '#shell-usage-root',
    statusHost: '#shell-status-root',
    notifyHost: '#shell-notify-root',
    creditsHost: '#shell-credits-root',
    creditsLeaderboardHost: '#vegas-leaderboard',
    messagesBadgeHost: '#shell-messages-badge-root',
    metaHost: '#shell-meta-root',
    disconnectHost: '#shell-disconnect-root',
    confirmHost: '#shell-confirm-root',
    toastHost: '#shell-toast-root',
    paletteButtonHost: '#shell-palette-button-root',
    paletteModalHost: '#shell-palette-modal-root',
  },
  failureClass: 'shell-error',
  load: async ({ hosts, dependencies }) => {
    const islandModule = import('./shell-island.js');
    const dashboardStateModule = import('./snapshot-store.js');
    const groupsStateModule = import('./groups-state.js');
    const shellStateModule = import('./shell-state.js');
    const notifyIslandModule = import('./notify-island.js');
    const notifyStateModule = import('./notify-state.js');
    const notifyActionsModule = import('./notify-menu.js');
    const creditsIslandModule = import('./credits-island.js');
    const creditsStateModule = import('./credits-state.js');
    const paletteIslandModule = import('./palette-island.js');
    const paletteStateModule = import('./palette-state.js');
    const paletteCommandsModule = import('./palette.js');
    const [
      { mountShellIsland }, { dashboardState }, { groupsState }, { shellState },
      { mountNotifyIsland }, { notifyState }, { createNotifyActions },
      { mountCreditsIsland }, { creditsState },
      { mountPaletteIsland }, { createPaletteState }, { buildCommands },
    ] = await Promise.all([
      islandModule, dashboardStateModule, groupsStateModule, shellStateModule,
      notifyIslandModule, notifyStateModule, notifyActionsModule,
      creditsIslandModule, creditsStateModule,
      paletteIslandModule, paletteStateModule, paletteCommandsModule,
    ]);
    const notifyActions = createNotifyActions({
      state: notifyState,
      notify: dependencies.notify,
    });
    const paletteState = createPaletteState({
      snapshot: dashboardState.snapshot,
      commandBuilder: buildCommands,
      onError: dependencies.notify,
    });
    return {
      state: dashboardState,
      mount: (registerCleanup) => {
        mountShellIsland({
          hosts,
          state: dashboardState,
          groupsState,
          feedback: shellState,
          dependencies,
          registerCleanup,
        });
        mountNotifyIsland({
          host: hosts.notifyHost,
          state: notifyState,
          actions: notifyActions,
          registerCleanup,
        });
        mountCreditsIsland({
          counterHost: hosts.creditsHost,
          leaderboardHost: hosts.creditsLeaderboardHost,
          state: creditsState,
          registerCleanup,
        });
        mountPaletteIsland({
          buttonHost: hosts.paletteButtonHost,
          modalHost: hosts.paletteModalHost,
          state: paletteState,
          registerCleanup,
        });
      },
    };
  },
});

export async function mountShellFeature(dependencies = {}, lifecycleOptions) {
  const cleanup = await mountIslandDescriptor(shellDescriptor, dependencies, lifecycleOptions);
  if (typeof cleanup !== 'function') {
    // Unlike bounded feature islands, the shell owns the confirmation and
    // feedback surfaces used by the rest of bootstrap. Continuing after an
    // aggregate leaf/import failure would leave confirmation promises with no
    // mounted UI capable of resolving them.
    throw new Error('Dashboard shell failed to mount');
  }
  return cleanup;
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
      { mountGroupsIsland }, { groupsState }, { createGroupsActions }, { memberTableHTML },
    ] = await Promise.all([islandModule, stateModule, actionsModule, renderModule]);
    const actions = createGroupsActions({ state: groupsState, ...dependencies });
    const presentation = {
      memberTable: memberTableHTML,
    };
    return {
      state: groupsState,
      mount: (registerCleanup) => mountGroupsIsland({
        filterHost, listHost, state: groupsState, actions,
        presentation, registerCleanup,
      }),
    };
  },
});

export function mountGroupsFeature(dependencies = {}) {
  return mountIslandDescriptor(groupsDescriptor, dependencies);
}

const linksDescriptor = createIslandDescriptor({
  name: 'links',
  label: 'Inter-group links',
  hosts: { filterHost: '#links-filter-root', listHost: '#links-list' },
  failureClass: 'links-error',
  load: async ({ hosts: { filterHost, listHost }, dependencies }) => {
    const islandModule = import('./links-island.js');
    const stateModule = import('./links-state.js');
    const [{ mountLinksIsland }, { linksState }] = await Promise.all([
      islandModule, stateModule,
    ]);
    return {
      state: linksState,
      mount: (registerCleanup) => mountLinksIsland({
        filterHost, listHost, state: linksState, openCreate: dependencies.openCreate,
        registerCleanup,
      }),
    };
  },
});

export function mountLinksFeature(dependencies = {}) {
  return mountIslandDescriptor(linksDescriptor, dependencies);
}

const dockDescriptor = createIslandDescriptor({
  name: 'dock',
  label: 'Dock',
  hosts: { host: '#dock-body' },
  failureClass: 'dock-error',
  load: async ({ hosts: { host } }) => {
    const islandModule = import('./dock-island.js');
    const stateModule = import('./dock-state.js');
    const controllerModule = import('./dock.js');
    const [
      { mountDockIsland }, { dockState },
      { dockSections, isDockSectionOpen, setDockSectionOpen },
    ] = await Promise.all([islandModule, stateModule, controllerModule]);
    return {
      state: dockState,
      mount: (registerCleanup) => mountDockIsland({
        host, state: dockState, sections: dockSections,
        isSectionOpen: isDockSectionOpen, setSectionOpen: setDockSectionOpen,
        registerCleanup,
      }),
    };
  },
});

export function mountDockFeature(dependencies = {}) {
  return mountIslandDescriptor(dockDescriptor, dependencies);
}

// Keep the Jobs graph behind the shared dynamic boundary so a corrupt optional
// asset produces a visible feature-local error rather than preventing the rest
// of the dashboard entry module from booting.
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

// The Plugins tab, nav badge, and modal form one guarded ownership unit.
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

const debugDescriptor = createIslandDescriptor({
  name: 'debug',
  label: 'Debug',
  hosts: { host: '#debug-root' },
  failureClass: 'debug-error',
  load: async ({ hosts: { host }, dependencies }) => {
    const islandModule = import('./debug-island.js');
    const stateModule = import('./debug-state.js');
    const actionsModule = import('./debug-actions.js');
    const [{ mountDebugIsland }, { debugState }, { createDebugActions }] =
      await Promise.all([islandModule, stateModule, actionsModule]);
    const actions = createDebugActions({ state: debugState, ...dependencies });
    return {
      state: debugState,
      mount: (registerCleanup) => mountDebugIsland({
        host,
        state: debugState,
        actions,
        registerCleanup,
        pollMs: dependencies.pollMs,
        setIntervalImpl: dependencies.setIntervalImpl,
        clearIntervalImpl: dependencies.clearIntervalImpl,
      }),
    };
  },
});

export function mountDebugFeature(dependencies = {}) {
  return mountIslandDescriptor(debugDescriptor, dependencies);
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
    return { state: processesState, mount: (registerCleanup) => mountProcessesIsland({
      host, state: processesState, actions, confirmDiscard: dependencies.confirmDiscard, registerCleanup,
    }) };
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

const actionDialogsDescriptor = createIslandDescriptor({
  name: 'action-dialogs',
  label: 'Action dialogs',
  hosts: { root: '#action-dialog-root' },
  primaryHost: 'root',
  failureClass: 'action-dialog-error',
  load: async ({ hosts, dependencies }) => {
    const islandModule = import('./action-dialog-island.js');
    const stateModule = import('./action-dialog-state.js');
    const actionsModule = import('./action-dialog-actions.js');
    const [
      { mountActionDialogIsland }, { createActionDialogState }, { createActionDialogActions },
    ] = await Promise.all([islandModule, stateModule, actionsModule]);
    const state = createActionDialogState();
    const actions = createActionDialogActions({ state, ...dependencies });
    return {
      state,
      mount: (registerCleanup) => mountActionDialogIsland({
        host: hosts.root, state, actions, registerCleanup, ...dependencies,
      }),
    };
  },
});

export function mountActionDialogsFeature(dependencies = {}) {
  return mountIslandDescriptor(actionDialogsDescriptor, dependencies);
}

const directoryPickerDescriptor = createIslandDescriptor({
  name: 'directory-picker',
  label: 'Directory picker',
  hosts: { root: '#directory-picker-root' },
  primaryHost: 'root',
  failureClass: 'directory-picker-error',
  load: async ({ hosts, dependencies }) => {
    const islandModule = import('./directory-picker-island.js');
    const stateModule = import('./directory-picker-state.js');
    const actionsModule = import('./directory-picker-actions.js');
    const [
      { mountDirectoryPickerIsland }, { createDirectoryPickerState },
      { createDirectoryPickerActions },
    ] = await Promise.all([islandModule, stateModule, actionsModule]);
    const state = createDirectoryPickerState();
    const actions = createDirectoryPickerActions(dependencies);
    return {
      state,
      mount: (registerCleanup) => mountDirectoryPickerIsland({
        host: hosts.root, state, actions, registerCleanup, ...dependencies,
      }),
    };
  },
});

export function mountDirectoryPickerFeature(dependencies = {}) {
  return mountIslandDescriptor(directoryPickerDescriptor, dependencies);
}
