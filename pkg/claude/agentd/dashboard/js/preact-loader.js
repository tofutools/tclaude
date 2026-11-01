// This source module intentionally has no Preact imports. Each bounded feature
// loads its runtime graph only after claiming its stable hosts.
import { createIslandDescriptor, mountFeatureIsland, mountIslandDescriptor } from './island-lifecycle.js';
import { registerFeatureState } from './feature-state-registry.js';

const shellDescriptor = createIslandDescriptor({
  name: 'shell',
  label: 'Dashboard shell',
  hosts: {
    activityHost: '#shell-activity-root',
    usageHost: '#shell-usage-root',
    spendLimitHost: '#spend-limit-banner-root',
    statusHost: '#shell-status-root',
    notifyHost: '#shell-notify-root',
    creditsHost: '#shell-credits-root',
    creditsLeaderboardHost: '#vegas-leaderboard',
    messagesBadgeHost: '#shell-messages-badge-root',
    metaHost: '#shell-meta-root',
    openPRsHost: '#shell-open-prs-root',
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
    const paletteRecentModule = import('./palette-recent.js');
    const prefsModule = import('./prefs.js');
    const [
      { mountShellIsland }, { dashboardState }, { groupsState }, { shellState },
      { mountNotifyIsland }, { notifyState }, { createNotifyActions },
      { mountCreditsIsland }, { creditsState },
      { mountPaletteIsland }, { createPaletteState }, { buildCommands },
      { createPrefsRecentStore }, { dashPrefs },
    ] = await Promise.all([
      islandModule, dashboardStateModule, groupsStateModule, shellStateModule,
      notifyIslandModule, notifyStateModule, notifyActionsModule,
      creditsIslandModule, creditsStateModule,
      paletteIslandModule, paletteStateModule, paletteCommandsModule,
      paletteRecentModule, prefsModule,
    ]);
    const notifyActions = createNotifyActions({
      state: notifyState,
      notify: dependencies.notify,
    });
    const paletteState = createPaletteState({
      snapshot: dashboardState.snapshot,
      commandBuilder: buildCommands,
      onError: dependencies.notify,
      recentStore: createPrefsRecentStore(dashPrefs),
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
          snapshot: dashboardState.snapshot,
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
  hosts: {
    routeMapHost: '#groups-route-map-root',
    filterHost: '#groups-filter-root',
    listHost: '#groups-list',
    memberDialogHost: '#groups-member-dialog-root',
    addMemberDialogHost: '#groups-add-member-dialog-root',
  },
  failureClass: 'groups-error',
  load: async ({ hosts: {
    routeMapHost, filterHost, listHost, memberDialogHost, addMemberDialogHost,
  }, dependencies }) => {
    const islandModule = import('./groups-island.js');
    const routeMapModule = import('./groups-route-map.js');
    const memberEditorModule = import('./member-editor-island.js');
    const addMemberDialogModule = import('./add-member-dialog-island.js');
    const stateModule = import('./groups-state.js');
    const actionsModule = import('./groups-actions.js');
    const [
      { mountGroupsIsland }, { mountGroupsRouteMap }, { mountGroupsMemberEditor },
      { mountGroupsAddMemberDialog },
      { groupsState }, { createGroupsActions },
    ] = await Promise.all([
      islandModule, routeMapModule, memberEditorModule, addMemberDialogModule, stateModule, actionsModule,
    ]);
    const actions = createGroupsActions({ state: groupsState, ...dependencies });
    return {
      state: groupsState,
      mount: (registerCleanup) => {
        // The fused Groups view runs this node's groups through these too.
        registerCleanup(registerFeatureState('groups-actions', actions));
        mountGroupsIsland({
          filterHost, listHost, state: groupsState, actions,
          registerCleanup,
        });
        mountGroupsRouteMap({ host: routeMapHost, state: groupsState, registerCleanup });
        mountGroupsMemberEditor({
          host: memberDialogHost, state: groupsState, actions,
          confirmDiscard: dependencies.confirmDiscard, registerCleanup,
        });
        mountGroupsAddMemberDialog({
          host: addMemberDialogHost, state: groupsState, actions,
          confirmDiscard: dependencies.confirmDiscard, registerCleanup,
        });
      },
    };
  },
});

export function mountGroupsFeature(dependencies = {}) {
  return mountIslandDescriptor(groupsDescriptor, dependencies);
}

// The quick reader belongs to no tab: the Groups member rows and the Terminals
// tab strip both raise it, and it draws over whichever tab is showing. Its own
// island keeps it alive when either of those features fails to mount, and
// keeps a missing reader host from taking a whole tab down with it.
const humanNotificationsDescriptor = createIslandDescriptor({
  name: 'human-notifications',
  label: 'Human notification reader',
  hosts: { host: '#human-notification-reader-root' },
  failureClass: 'human-notifications-error',
  load: async ({ hosts: { host }, dependencies }) => {
    const islandModule = import('./human-notification-reader-island.js');
    const dashboardStateModule = import('./snapshot-store.js');
    const groupsStateModule = import('./groups-state.js');
    const [{ mountHumanNotificationReaderIsland }, { dashboardState }, { groupsState }] =
      await Promise.all([islandModule, dashboardStateModule, groupsStateModule]);
    // Read the store every island sees — the reader must work even where the
    // Groups feature is absent — but push its optimistic read/unread edit into
    // the Groups store too, so the member row, the terminal tab glyph and the
    // top-bar hint all move together instead of at the next poll.
    const state = {
      snapshot: dashboardState.snapshot,
      publish: (next) => {
        dashboardState.publishLocalEdit(next);
        groupsState.publish(next);
      },
    };
    return {
      mount: (registerCleanup) => mountHumanNotificationReaderIsland({
        host, state, actions: { reportError: dependencies.notify }, registerCleanup,
      }),
    };
  },
});

export function mountHumanNotificationsFeature(dependencies = {}) {
  return mountIslandDescriptor(humanNotificationsDescriptor, dependencies);
}

const linksDescriptor = createIslandDescriptor({
  name: 'links',
  label: 'Inter-group links',
  hosts: { host: '#links-feature-root' },
  failureClass: 'links-error',
  load: async ({ hosts: { host }, dependencies }) => {
    const islandModule = import('./links-island.js');
    const stateModule = import('./links-state.js');
    const actionsModule = import('./links-actions.js');
    const [{ mountLinksIsland }, { linksState }, { createLinksActions }] = await Promise.all([
      islandModule, stateModule, actionsModule,
    ]);
    const actions = createLinksActions({ state: linksState, ...dependencies });
    return {
      state: linksState,
      mount: (registerCleanup) => mountLinksIsland({
        host, state: linksState, actions,
        confirmDiscard: dependencies.confirmDiscard, registerCleanup,
      }),
    };
  },
});

export function mountLinksFeature(dependencies = {}) {
  return mountIslandDescriptor(linksDescriptor, dependencies);
}

const spawnHarnessPolicyDescriptor = createIslandDescriptor({
  name: 'spawn-harness-policy',
  label: 'Cross-harness spawn policy',
  hosts: { host: '#spawn-harness-policy-root' },
  failureClass: 'spawn-harness-policy-error',
  load: async ({ hosts: { host }, dependencies }) => {
    const { mountSpawnHarnessPolicyIsland } = await import('./spawn-harness-policy-island.js');
    return {
      mount: (registerCleanup) => mountSpawnHarnessPolicyIsland({
        host, confirmDiscard: dependencies.confirmDiscard,
        notify: dependencies.notify, registerCleanup,
      }),
    };
  },
});

export function mountSpawnHarnessPolicyFeature(dependencies = {}) {
  return mountIslandDescriptor(spawnHarnessPolicyDescriptor, dependencies);
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
      { dockSections, isDockSectionOpen, setDockSectionOpen, dockNotify, dockNodeOps },
    ] = await Promise.all([islandModule, stateModule, controllerModule]);
    return {
      state: dockState,
      mount: (registerCleanup) => mountDockIsland({
        host, state: dockState, sections: dockSections,
        isSectionOpen: isDockSectionOpen, setSectionOpen: setDockSectionOpen,
        notify: dockNotify, nodeOps: dockNodeOps, registerCleanup,
      }),
    };
  },
});

export function mountDockFeature(dependencies = {}) {
  return mountIslandDescriptor(dockDescriptor, dependencies);
}

// The companion (tcl-6i0283): a floating window over every view that hosts
// agents' native terminals, with the Steward as its default helper.
const companionDescriptor = createIslandDescriptor({
  name: 'companion',
  label: 'Companion',
  hosts: { host: '#companion-root' },
  failureClass: 'companion-error',
  load: async ({ hosts: { host }, dependencies }) => {
    const islandModule = import('./companion-island.js');
    const snapshotStoreModule = import('./snapshot-store.js');
    const prefsModule = import('./prefs.js');
    const terminalsCoreModule = import('./terminals-core.js');
    const xtermLoaderModule = import('./xterm-loader.js');
    const terminalsTabModule = import('./terminals-tab.js');
    const [
      { mountCompanionIsland }, { dashboardState }, { dashPrefs },
      { mountTerminalWidget }, { loadXtermRuntime }, { closeTerminalsForConvs, detachWebWindow, openWebWindowPane },
    ] = await Promise.all([
      islandModule, snapshotStoreModule, prefsModule,
      terminalsCoreModule, xtermLoaderModule, terminalsTabModule,
    ]);
    return {
      mount: (registerCleanup) => mountCompanionIsland({
        host, registerCleanup,
        snapshot: dashboardState.snapshot, activeTab: dashboardState.activeTab, prefs: dashPrefs,
        notify: dependencies.notify, widgetFactory: mountTerminalWidget, loadRuntime: loadXtermRuntime,
        closeTerminalPanes: closeTerminalsForConvs, onComposeMessage: dependencies.onComposeMessage,
        openPermissions: dependencies.openPermissions, openSudo: dependencies.openSudo,
        detachWindow: (agent, label, options) => detachWebWindow(agent, label, options),
        openStewardHandover: async (agent, label) => {
          const { openStewardHandoverDialog } = await import('./action-dialog-controller.js');
          return openStewardHandoverDialog({ agent, label });
        },
        openMorph: async (agent) => {
          const controllerModule = import('./action-dialog-controller.js');
          const launchModule = import('./launch-target-dialog.js');
          const fleetModule = import('./fleet-state.js');
          const modelModule = import('./companion-model.js');
          const [{ openLaunchTargetDialog }, { launchAgent }, { fleetState }, { companionLaunchData }] = await Promise.all([
            controllerModule, launchModule, fleetModule, modelModule,
          ]);
          const fleet = fleetState.fleet.value;
          const node = fleet?.self || { id: 'local', name: 'this node' };
          return openLaunchTargetDialog({ mode: 'morph', agent: launchAgent(companionLaunchData(agent, fleet), node) });
        },
        openTerminalsPane: (agent, label, harness) => openWebWindowPane(agent, label, { harness }),
        navigate: async (path) => {
          const { navigateToPath } = await import('./nav-history.js');
          return navigateToPath(path);
        },
      }),
    };
  },
});

// The quick permissions panel: its own island, because a companion pill, a
// Terminals tab and the access banner all raise it.
const accessPanelDescriptor = createIslandDescriptor({
  name: 'access-panel',
  label: 'Permission requests',
  hosts: { host: '#access-panel-root' },
  failureClass: 'access-panel-error',
  load: async ({ hosts: { host }, dependencies }) => {
    const islandModule = import('./access-panel-island.js');
    const snapshotStoreModule = import('./snapshot-store.js');
    const [{ mountAccessPanelIsland }, { dashboardState }] = await Promise.all([islandModule, snapshotStoreModule]);
    return {
      mount: (registerCleanup) => mountAccessPanelIsland({
        host, registerCleanup, snapshot: dashboardState.snapshot,
        fetchImpl: (...args) => globalThis.fetch(...args), notify: dependencies.notify,
        openInMessages: async (id) => {
          const { focusAccessRequest } = await import('./mail-bridge.js');
          return focusAccessRequest(id);
        },
      }),
    };
  },
});

// The question and progress for the knowledge base's source download.
const knowledgeSourceDescriptor = createIslandDescriptor({
  name: 'knowledge-source',
  label: 'Source download',
  hosts: { host: '#knowledge-source-root' },
  failureClass: 'knowledge-source-error',
  load: async ({ hosts: { host }, dependencies }) => {
    const islandModule = import('./knowledge-source-island.js');
    const snapshotStoreModule = import('./snapshot-store.js');
    const [{ mountKnowledgeSourceIsland }, { dashboardState }] = await Promise.all([islandModule, snapshotStoreModule]);
    return {
      mount: (registerCleanup) => mountKnowledgeSourceIsland({
        host, registerCleanup, snapshot: dashboardState.snapshot,
        fetchImpl: (...args) => globalThis.fetch(...args), notify: dependencies.notify,
      }),
    };
  },
});

export function mountKnowledgeSourceFeature(dependencies = {}) {
  return mountIslandDescriptor(knowledgeSourceDescriptor, dependencies);
}

export function mountAccessPanelFeature(dependencies = {}) {
  return mountIslandDescriptor(accessPanelDescriptor, dependencies);
}

export function mountCompanionFeature(dependencies = {}) {
  return mountIslandDescriptor(companionDescriptor, dependencies);
}

// A board in its own floating window (tcl-h4y60w). The boards themselves are
// tabs of the terminal shell.
const boardOverlayDescriptor = createIslandDescriptor({
  name: 'board-overlay',
  label: 'Board overlay',
  hosts: { host: '#board-overlay-root' },
  failureClass: 'board-overlay-error',
  load: async ({ hosts: { host } }) => {
    const islandModule = import('./board-overlay.js');
    const snapshotStoreModule = import('./snapshot-store.js');
    const [{ mountBoardOverlayIsland }, { dashboardState }] = await Promise.all([islandModule, snapshotStoreModule]);
    return {
      mount: (registerCleanup) => mountBoardOverlayIsland({ host, registerCleanup, snapshot: dashboardState.snapshot }),
    };
  },
});

export function mountBoardOverlayFeature(dependencies = {}) {
  return mountIslandDescriptor(boardOverlayDescriptor, dependencies);
}

// Terminal chrome and open state are one ownership unit across the nav badge,
// dashboard pane stack. xterm itself remains behind
// the opaque host adapter loaded by terminal-shell-island.js.
const terminalsDescriptor = createIslandDescriptor({
  name: 'terminals',
  label: 'Terminal shell',
  hosts: {
    host: '#terminals-root',
    badgeHost: '#terminals-badge-root',
  },
  failureClass: 'terminals-error',
  load: async ({ hosts: { host, badgeHost }, dependencies }) => {
    const islandModule = import('./terminal-shell-island.js');
    const stateModule = import('./terminal-shell-state.js');
    const actionsModule = import('./terminal-shell-actions.js');
    // The dashboard snapshot is what tells a terminal tab that its agent has
    // unread human notifications. The standalone pop-out shell mounts without
    // it and simply draws no attention glyph.
    const snapshotStoreModule = import('./snapshot-store.js');
    // The node scope: a fused view shows only the ticked nodes' panes.
    const scopeModule = import('./fleet-state.js');
    const [
      { mountTerminalShellIsland }, { terminalShellState }, { createTerminalShellActions },
      { dashboardState }, { fleetState },
    ] = await Promise.all([islandModule, stateModule, actionsModule, snapshotStoreModule, scopeModule]);
    const actions = createTerminalShellActions({ state: terminalShellState, ...dependencies });
    return {
      state: terminalShellState,
      mount: (registerCleanup) => mountTerminalShellIsland({
        host, badgeHost, state: terminalShellState, actions, registerCleanup,
        snapshot: dashboardState.snapshot,
        scope: fleetState,
        widgetFactory: dependencies.widgetFactory,
        onComposeMessage: dependencies.onComposeMessage,
        composeMessageDialogKind: dependencies.composeMessageDialogKind,
      }),
    };
  },
});

export function mountTerminalsFeature(dependencies = {}) {
  return mountIslandDescriptor(terminalsDescriptor, dependencies);
}

// Keep the Jobs graph behind the shared dynamic boundary so a corrupt optional
// asset produces a visible feature-local error rather than preventing the rest
// of the dashboard entry module from booting.
const jobsDescriptor = createIslandDescriptor({
    name: 'jobs',
    label: 'Automations',
    hosts: {
      host: '#jobs-root', badgeHost: '#jobs-badge-root', dialogHost: '#jobs-cron-dialog-root',
    },
    failureClass: 'jobs-error',
    load: async ({ hosts: { host, badgeHost, dialogHost }, dependencies: actionDependencies }) => {
      // Keep import() behind named promises: the repository's intentionally
      // small module-graph scanner recognizes dynamic imports in expressions,
      // while a bare import(...) line is intentionally rejected as ambiguous.
      const islandModule = import('./jobs-island.js');
      const stateModule = import('./jobs-state.js');
      const actionsModule = import('./jobs-actions.js');
      const [{ mountJobsIsland }, { jobsState }, { createJobsActions }] =
        await Promise.all([islandModule, stateModule, actionsModule]);
      const jobsActions = createJobsActions({ state: jobsState, ...actionDependencies });
      return {
        state: jobsState,
        mount: (registerCleanup) => mountJobsIsland({
          host, badgeHost, dialogHost, state: jobsState, actions: jobsActions,
          confirmDiscard: actionDependencies.confirmDiscard, registerCleanup,
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

// The Usage tab's hub: the Overview / Quota / Costs subnav, the Overview
// itself (read off the two halves' states), and the tab's visibility.
export async function mountUsageHubFeature() {
  const host = document.querySelector('#usage-hub-root');
  if (!host) return null;
  return mountFeatureIsland({
    name: 'usage-hub', label: 'Usage', hosts: [host], failureClass: 'usage-hub-error',
    load: async () => {
      const islandModule = import('./usage-hub-island.js');
      const stateModule = import('./usage-hub-state.js');
      const quotaStateModule = import('./usage-history-state.js');
      const costsStateModule = import('./costs-state.js');
      const [{ mountUsageHubIsland }, { usageHubState }, { usageHistoryState }, { costsState }] =
        await Promise.all([islandModule, stateModule, quotaStateModule, costsStateModule]);
      return {
        state: usageHubState,
        mount: (registerCleanup) => mountUsageHubIsland({
          host, state: usageHubState, quotaState: usageHistoryState, costsState, registerCleanup,
        }),
      };
    },
  });
}

export async function mountUsageHistoryFeature(actionDependencies = {}) {
  const host = document.querySelector('#usage-root');
  if (!host) return null;
  return mountFeatureIsland({
    name: 'usage', label: 'Usage', hosts: [host], failureClass: 'usage-history-error',
    load: async () => {
      const islandModule = import('./usage-history-island.js');
      const stateModule = import('./usage-history-state.js');
      const actionsModule = import('./usage-history-actions.js');
      const [{ mountUsageHistoryIsland }, { usageHistoryState }, { createUsageHistoryActions }] =
        await Promise.all([islandModule, stateModule, actionsModule]);
      const actions = createUsageHistoryActions({ state: usageHistoryState, ...actionDependencies });
      return {
        state: usageHistoryState,
        mount: (registerCleanup) => mountUsageHistoryIsland({
          host, state: usageHistoryState, actions, registerCleanup,
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

const voiceWakeDescriptor = createIslandDescriptor({
  name: 'voice-wake',
  label: 'Wake listener',
  hosts: { host: '#voice-wake-root' },
  failureClass: 'voice-wake-error',
  load: async ({ hosts: { host }, dependencies }) => {
    const islandModule = import('./voice-wake-island.js');
    const snapshotModule = import('./snapshot-store.js');
    const [{ mountVoiceWakeIsland }, { dashboardState }] = await Promise.all([islandModule, snapshotModule]);
    return {
      state: null,
      mount: (registerCleanup) => registerCleanup(mountVoiceWakeIsland({ host, snapshot: dependencies.snapshot || dashboardState.snapshot })),
    };
  },
});

// mountVoiceWakeFeature runs on the dashboard and in detached terminal windows.
export function mountVoiceWakeFeature(dependencies = {}) {
  return mountIslandDescriptor(voiceWakeDescriptor, dependencies);
}

const messageAccessDialogsDescriptor = createIslandDescriptor({
  name: 'message-access-dialogs',
  label: 'Message and access dialogs',
  hosts: {
    dialogHost: '#message-access-dialog-root',
  },
  failureClass: 'message-access-dialog-error',
  load: async ({ hosts, dependencies }) => {
    const islandModule = import('./message-access-dialog-island.js');
    const stateModule = import('./message-access-dialog-state.js');
    const actionsModule = import('./message-access-dialog-actions.js');
    const snapshotModule = import('./snapshot-store.js');
    const overlayModule = import('./overlay-stack.js');
    const [
      { mountMessageAccessDialogIsland }, { createMessageAccessDialogState },
      { createMessageAccessDialogActions }, { dashboardState }, { hasShownOverlay },
    ] = await Promise.all([islandModule, stateModule, actionsModule, snapshotModule, overlayModule]);
    const state = createMessageAccessDialogState({ canRestoreFocus: () => !hasShownOverlay() });
    const actions = createMessageAccessDialogActions({ ...dependencies });
    return {
      state,
      mount: (registerCleanup) => mountMessageAccessDialogIsland({
        ...hosts, state, actions, snapshot: dashboardState.snapshot,
        confirmDiscard: dependencies.confirmDiscard, registerCleanup,
      }),
    };
  },
});

export function mountMessageAccessDialogsFeature(dependencies = {}) {
  return mountIslandDescriptor(messageAccessDialogsDescriptor, dependencies);
}

const toolbarProfilePickerDescriptor = createIslandDescriptor({
  name: 'toolbar-profile-picker',
  label: 'Dashboard profile picker',
  hosts: {
    profileHost: '#dashboard-default-profile-control',
    sandboxHost: '#dashboard-default-sandbox-profile-control',
  },
  failureClass: 'toolbar-profile-picker-error',
  load: async ({ hosts: { profileHost, sandboxHost }, dependencies }) => {
    const islandModule = import('./toolbar-profile-picker-island.js');
    const stateModule = import('./toolbar-profile-picker-state.js');
    const actionsModule = import('./toolbar-profile-picker-actions.js');
    const [
      { mountToolbarProfilePickerIsland }, { createToolbarProfilePickerState },
      { createToolbarProfilePickerActions },
    ] = await Promise.all([islandModule, stateModule, actionsModule]);
    const state = createToolbarProfilePickerState();
    const actions = createToolbarProfilePickerActions(dependencies);
    return {
      state,
      mount: (registerCleanup) => mountToolbarProfilePickerIsland({
        profileHost, sandboxHost, state, actions, registerCleanup,
      }),
    };
  },
});

const queuedSpawnsDescriptor = createIslandDescriptor({
  name: 'queued-spawns',
  label: 'Queued spawns',
  hosts: { host: '#queued-spawns-control' },
  failureClass: 'queued-spawns-error',
  load: async ({ hosts: { host } }) => {
    const islandModule = import('./queued-spawns-island.js');
    const stateModule = import('./snapshot-store.js');
    const [{ mountQueuedSpawnsIsland }, { dashboardState }] = await Promise.all([
      islandModule, stateModule,
    ]);
    return {
      state: dashboardState,
      mount: (registerCleanup) => mountQueuedSpawnsIsland({
        host, snapshot: dashboardState.snapshot, registerCleanup,
      }),
    };
  },
});

export function mountQueuedSpawnsFeature(dependencies = {}) {
  return mountIslandDescriptor(queuedSpawnsDescriptor, dependencies);
}

export function mountToolbarProfilePickerFeature(dependencies = {}) {
  return mountIslandDescriptor(toolbarProfilePickerDescriptor, dependencies);
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

// The Fleet island owns four hosts: the tab-bar node chips, the top-level
// bar that stands in for the tab strip while the map is open, the map tab, and
// the header marker of a peer's per-node view.
const fleetDescriptor = createIslandDescriptor({
  name: 'fleet', label: 'Fleet nodes',
  // mapHost is listed first: a load failure renders into the first host, and it
  // belongs in the map section, never in the tab bar.
  hosts: { mapHost: '#fleetview-map-root', chipsHost: '#node-chips-root', barHost: '#fleetview-toplevel-root', remoteHost: '#remote-node-root', bannerHost: '#fleetview-scope-banner-root' },
  failureClass: 'fleet-error',
  load: async ({ hosts: { chipsHost, barHost, mapHost, remoteHost, bannerHost }, dependencies }) => {
    const islandModule = import('./fleet-island.js');
    const stateModule = import('./fleet-state.js');
    const actionsModule = import('./fleet-actions.js');
    const [{ mountFleetIsland }, { fleetState }, { createFleetActions }] = await Promise.all([islandModule, stateModule, actionsModule]);
    const actions = createFleetActions({ state: fleetState, ...dependencies });
    return { state: fleetState, mount: (registerCleanup) => mountFleetIsland({ chipsHost, barHost, mapHost, remoteHost, bannerHost, state: fleetState, actions, registerCleanup, navigate: dependencies.navigate, timers: dependencies.timers }) };
  },
});

export function mountFleetFeature(dependencies = {}) {
  return mountIslandDescriptor(fleetDescriptor, dependencies);
}

// The fused Groups view is its own island: it pulls in the Groups renderer,
// and a failure there must not take the node chips down.
const fleetMergedGroupsDescriptor = createIslandDescriptor({
  name: 'fleet-merged-groups', label: 'Groups on several nodes',
  hosts: { host: '#fleetview-fused-groups-root' },
  failureClass: 'fleet-error',
  load: async ({ hosts: { host } }) => {
    const islandModule = import('./fleet-merged-island.js');
    const stateModule = import('./fleet-state.js');
    const [{ mountMergedGroupsIsland }, { fleetState }] = await Promise.all([islandModule, stateModule]);
    return { state: fleetState, mount: (registerCleanup) => mountMergedGroupsIsland({ host, state: fleetState, registerCleanup }) };
  },
});

export function mountFleetMergedGroupsFeature(dependencies = {}) {
  return mountIslandDescriptor(fleetMergedGroupsDescriptor, dependencies);
}

// Fused Messages: the ticked nodes' mail in the Messages layout.
const fusedMessagesDescriptor = createIslandDescriptor({
  name: 'fused-messages', label: 'Messages on several nodes',
  hosts: { host: '#fleetview-fused-messages-root' },
  failureClass: 'fleet-error',
  load: async ({ hosts: { host } }) => {
    const islandModule = import('./fused-mail.js');
    const stateModule = import('./fleet-state.js');
    const [{ mountFusedMailIsland }, { fleetState }] = await Promise.all([islandModule, stateModule]);
    return { state: fleetState, mount: (registerCleanup) => mountFusedMailIsland({ host, state: fleetState, registerCleanup }) };
  },
});

export function mountFusedMessagesFeature(dependencies = {}) {
  return mountIslandDescriptor(fusedMessagesDescriptor, dependencies);
}

// Fleet administration (trust, hub connection) is its own island too.
const fleetAdminDescriptor = createIslandDescriptor({
  name: 'fleet-admin', label: 'Fleet administration',
  hosts: { host: '#fleet-admin-root' },
  failureClass: 'fleet-error',
  load: async ({ hosts: { host } }) => {
    const islandModule = import('./fleet-admin-island.js');
    const actionsModule = import('./fleet-admin-actions.js');
    const stateModule = import('./fleet-state.js');
    const [{ mountFleetAdminIsland }, { createFleetAdminActions }, { fleetState }] = await Promise.all([islandModule, actionsModule, stateModule]);
    const actions = createFleetAdminActions();
    return { state: fleetState, mount: (registerCleanup) => mountFleetAdminIsland({ host, state: fleetState, actions, registerCleanup }) };
  },
});

export function mountFleetAdminFeature(dependencies = {}) {
  return mountIslandDescriptor(fleetAdminDescriptor, dependencies);
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

const profilesDescriptor = createIslandDescriptor({
  name: 'profiles',
  label: 'Profiling',
  hosts: { host: '#profiles-root' },
  failureClass: 'debug-error',
  load: async ({ hosts: { host }, dependencies }) => {
    const islandModule = import('./profiles-island.js');
    const stateModule = import('./profiles-state.js');
    const actionsModule = import('./profiles-actions.js');
    const [{ mountProfilesIsland }, { profilesState }, { createProfilesActions }] =
      await Promise.all([islandModule, stateModule, actionsModule]);
    const actions = createProfilesActions({ state: profilesState, ...dependencies });
    return {
      state: profilesState,
      mount: (registerCleanup) => mountProfilesIsland({
        host,
        state: profilesState,
        actions,
        registerCleanup,
        setTimeoutImpl: dependencies.setTimeoutImpl,
        clearTimeoutImpl: dependencies.clearTimeoutImpl,
      }),
    };
  },
});

export function mountProfilesFeature(dependencies = {}) {
  return mountIslandDescriptor(profilesDescriptor, dependencies);
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

// The Diagnostics › Doctor view and its two badges (on the Diagnostics nav tab
// and on the Doctor subtab) form one ownership unit.
export async function mountDoctorFeature(dependencies = {}) {
  const host = document.querySelector('#doctor-root');
  if (!host) return null;
  const badgeHosts = [...document.querySelectorAll('#doctor-badge-root, #doctor-subtab-badge-root')];
  return mountFeatureIsland({
    name: 'doctor', label: 'Doctor', hosts: [host], failureClass: 'doctor-error',
    load: async () => {
      const islandModule = import('./doctor-island.js');
      const stateModule = import('./doctor-state.js');
      const [{ mountDoctorIsland }, { doctorState }] = await Promise.all([islandModule, stateModule]);
      return {
        state: doctorState,
        mount: (registerCleanup) => mountDoctorIsland({
          host, badgeHosts, state: doctorState, notify: dependencies.notify, registerCleanup,
        }),
      };
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
    const dashboardStateModule = import('./snapshot-store.js');
    const [
      { mountActionDialogIsland }, { createActionDialogState }, { createActionDialogActions },
      { dashboardState },
    ] = await Promise.all([islandModule, stateModule, actionsModule, dashboardStateModule]);
    const state = createActionDialogState();
    const actions = createActionDialogActions({ state, ...dependencies });
    return {
      state,
      mount: (registerCleanup) => mountActionDialogIsland({
        host: hosts.root, state, actions, registerCleanup, ...dependencies,
        snapshot: dashboardState.snapshot,
      }),
    };
  },
});

export function mountActionDialogsFeature(dependencies = {}) {
  return mountIslandDescriptor(actionDialogsDescriptor, dependencies);
}

const transactionDialogsDescriptor = createIslandDescriptor({
  name: 'transaction-dialogs',
  label: 'Transaction dialogs',
  hosts: { root: '#transaction-dialog-root' },
  primaryHost: 'root',
  failureClass: 'transaction-dialog-error',
  load: async ({ hosts, dependencies }) => {
    const islandModule = import('./transaction-dialog-island.js');
    const stateModule = import('./transaction-dialog-state.js');
    const actionsModule = import('./transaction-dialog-actions.js');
    const [
      { mountTransactionDialogIsland },
      { createTransactionDialogState },
      { createTransactionDialogActions },
    ] = await Promise.all([islandModule, stateModule, actionsModule]);
    const state = createTransactionDialogState();
    const actions = createTransactionDialogActions({ state, ...dependencies });
    return {
      state,
      mount: (registerCleanup) => mountTransactionDialogIsland({
        host: hosts.root, state, actions,
        confirmDiscard: dependencies.confirmDiscard,
        registerCleanup,
      }),
    };
  },
});

export function mountTransactionDialogsFeature(dependencies = {}) {
  return mountIslandDescriptor(transactionDialogsDescriptor, dependencies);
}

const worktreeCleanupDescriptor = createIslandDescriptor({
  name: 'worktree-cleanup',
  label: 'Worktree cleanup',
  hosts: { root: '#worktree-cleanup-root' },
  primaryHost: 'root',
  failureClass: 'worktree-cleanup-error',
  load: async ({ hosts, dependencies }) => {
    const islandModule = import('./worktree-cleanup-island.js');
    const stateModule = import('./worktree-cleanup-state.js');
    const actionsModule = import('./worktree-cleanup-actions.js');
    const [
      { mountWorktreeCleanupIsland },
      { createWorktreeCleanupState },
      { createWorktreeCleanupActions },
    ] = await Promise.all([islandModule, stateModule, actionsModule]);
    const state = createWorktreeCleanupState();
    const actions = createWorktreeCleanupActions({ ...dependencies });
    return {
      state,
      mount: (registerCleanup) => mountWorktreeCleanupIsland({
        host: hosts.root, state, actions, registerCleanup,
      }),
    };
  },
});

export function mountWorktreeCleanupFeature(dependencies = {}) {
  return mountIslandDescriptor(worktreeCleanupDescriptor, dependencies);
}

const groupCreateDescriptor = createIslandDescriptor({
  name: 'group-create',
  label: 'Group creation',
  hosts: { root: '#group-create-root' },
  primaryHost: 'root',
  failureClass: 'group-create-error',
  load: async ({ hosts, dependencies }) => {
    const islandModule = import('./group-create-island.js');
    const stateModule = import('./group-create-state.js');
    const actionsModule = import('./group-create-actions.js');
    const [
      { mountGroupCreateIsland },
      { createGroupCreateState },
      { createGroupCreateActions },
    ] = await Promise.all([islandModule, stateModule, actionsModule]);
    const state = createGroupCreateState({ getSnapshot: dependencies.getSnapshot });
    const actions = createGroupCreateActions(dependencies);
    return {
      state,
      mount: (registerCleanup) => mountGroupCreateIsland({
        host: hosts.root, state, actions,
        confirmDiscard: dependencies.confirmDiscard,
        words: dependencies.words,
        registerCleanup,
      }),
    };
  },
});

export function mountGroupCreateFeature(dependencies = {}) {
  return mountIslandDescriptor(groupCreateDescriptor, dependencies);
}

const agentSpawnDescriptor = createIslandDescriptor({
  name: 'agent-spawn',
  label: 'Agent spawn',
  hosts: { root: '#agent-spawn-root' },
  primaryHost: 'root',
  failureClass: 'agent-spawn-error',
  load: async ({ hosts, dependencies }) => {
    const islandModule = import('./agent-spawn-island.js');
    const stateModule = import('./agent-spawn-state.js');
    const actionsModule = import('./agent-spawn-actions.js');
    const [
      { mountAgentSpawnIsland },
      { createAgentSpawnState },
      { createAgentSpawnActions },
    ] = await Promise.all([islandModule, stateModule, actionsModule]);
    const state = createAgentSpawnState({ getSnapshot: dependencies.getSnapshot });
    const actions = createAgentSpawnActions(dependencies);
    return {
      state,
      mount: (registerCleanup) => mountAgentSpawnIsland({
        host: hosts.root, state, actions,
        confirmDiscard: dependencies.confirmDiscard,
        registerCleanup,
      }),
    };
  },
});

export function mountAgentSpawnFeature(dependencies = {}) {
  return mountIslandDescriptor(agentSpawnDescriptor, dependencies);
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

const gitRepositoriesDescriptor = createIslandDescriptor({
  name: 'git-repositories', label: 'Git repositories',
  hosts: { root: '#git-repositories-root' }, primaryHost: 'root',
  failureClass: 'git-repos-error',
  load: async ({ hosts, dependencies }) => {
    const islandModule = import('./git-repositories-island.js');
    const stateModule = import('./git-repositories-state.js');
    const actionsModule = import('./git-repositories-actions.js');
    const [{ mountGitRepositoriesIsland }, { createGitRepositoriesState }, { createGitRepositoriesActions }] = await Promise.all([
      islandModule, stateModule, actionsModule,
    ]);
    const state = createGitRepositoriesState();
    const actions = createGitRepositoriesActions(dependencies);
    return { state, mount: (registerCleanup) => mountGitRepositoriesIsland({ host: hosts.root, state, actions, registerCleanup }) };
  },
});
export function mountGitRepositoriesFeature(dependencies = {}) {
  return mountIslandDescriptor(gitRepositoriesDescriptor, dependencies);
}
