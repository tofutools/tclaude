import { setArcanePaletteEnabled } from './terminal-theme.js';
import { encodeTerminalOpenHash } from './terminal-handoff.js';
import { shellToast } from './shell-state.js';
import { detachWindowFeatures } from './terminal-drag-out.js';
import { terminalAttachConfig } from './terminal-attach-config.js';
import { openBoardOverlay } from './board-overlay-state.js';
import { openOverlayWindow } from './overlay-instances.js';
import { agentSessionSeed, overlayablePane, terminalWindowOf } from './terminal-window-model.js';
import { companionAvailable, floatInCompanion } from './companion-facade.js';

export function createTerminalShellActions({
  state,
  fetchImpl = globalThis.fetch,
  windowRef = globalThis.window,
  documentRef = globalThis.document,
  notify = shellToast,
  onReattachPane = null,
  onPaneDisconnected = null,
} = {}) {
  if (!state) throw new TypeError('terminal shell actions require state');
  const widgets = new Map();
  let disposed = false;
  let floatingAgents = 0;

  function registerWidget(id, widget) {
    if (disposed) {
      widget.dispose();
      return () => {};
    }
    widgets.get(id)?.dispose();
    widgets.set(id, widget);
    return () => {
      if (widgets.get(id) === widget) widgets.delete(id);
    };
  }

  function widgetFor(id) {
    return widgets.get(id) || null;
  }

  async function hideSeed(seed) {
    const conv = seed?.hideConv;
    if (!conv) return;
    try {
      const response = await fetchImpl(`/api/hide/${encodeURIComponent(conv)}`, {
        method: 'POST', credentials: 'same-origin',
      });
      if (!response.ok) console.warn('terminal detach (hide) failed:', response.status);
    } catch (error) {
      console.warn('terminal detach (hide) request error:', error);
    }
  }

  function openPane(seed, options) {
    if (disposed) return null;
    return state.openPane(seed, options);
  }

  // waitForPaneSettled resolves once the pane under key has attached and its
  // first redraw has been parsed (see the widget's settled), or false on
  // timeout. The widget registers on mount, a tick after the pane appears.
  function waitForPaneSettled(key, { quietMs, timeoutMs = 6000 } = {}) {
    const started = Date.now();
    return new Promise((resolve) => {
      const poll = () => {
        const pane = state.panes.value.find((candidate) => candidate.key === key);
        // A board has no attach to wait for.
        if (pane?.seed.board) { resolve(true); return; }
        const widget = pane && widgets.get(pane.id);
        const remaining = timeoutMs - (Date.now() - started);
        if (widget?.settled) {
          widget.settled({ quietMs, timeoutMs: Math.max(0, remaining) }).then(resolve, () => resolve(false));
          return;
        }
        if (disposed || remaining <= 0) { resolve(false); return; }
        setTimeout(poll, 50);
      };
      poll();
    });
  }

  async function receiveHandoffPane(seed) {
    if (disposed) return null;
    const key = seed?.key || seed?.ws;
    if (state.panes.value.some((pane) => pane.key === key)) {
      // The same terminal may have been reopened in the dashboard while its
      // pop-out was alive. Replace that widget after the pop-out's detach has
      // landed so reattach never acknowledges a stale/disconnected instance.
      await closePane(key, { skipDetach: true });
    }
    return openPane(seed);
  }

  function activatePane(key, options) {
    return !disposed && state.activatePane(key, options);
  }

  function reorderPane(key, targetKey, options) {
    return disposed ? null : state.reorderPane(key, targetKey, options);
  }

  function movePaneByOffset(key, offset) {
    return disposed ? null : state.movePaneByOffset(key, offset);
  }

  function movePaneOutsideGroup(key, groupID, options) {
    return disposed ? null : state.movePaneOutsideGroup(key, groupID, options);
  }

  function moveGroup(groupID, anchorKey, options) {
    return disposed ? null : state.moveGroup(groupID, anchorKey, options);
  }

  function moveGroupByOffset(groupID, offset) {
    return disposed ? null : state.moveGroupByOffset(groupID, offset);
  }

  // Tab-stack commands are thin pass-throughs: grouping is view state, so the
  // actions layer only adds the disposed guard every other pane command has.
  // closeGroupPanes is the one that is not, because closing terminals has to go
  // through the same widget-dispose + detach path as any other close.
  function createGroup(descriptor) {
    return disposed ? null : state.createGroup(descriptor);
  }

  function renameGroup(id, name) {
    return disposed ? null : state.renameGroup(id, name);
  }

  function setGroupColor(id, color) {
    return disposed ? null : state.setGroupColor(id, color);
  }

  function setGroupCollapsed(id, collapsed) {
    return disposed ? null : state.setGroupCollapsed(id, collapsed);
  }

  function toggleGroupCollapsed(id) {
    return disposed ? null : state.toggleGroupCollapsed(id);
  }

  function dissolveGroup(id) {
    return !disposed && state.dissolveGroup(id);
  }

  function assignPaneToGroup(key, groupID) {
    return !disposed && state.assignPaneToGroup(key, groupID);
  }

  function closeGroupPanes(id) {
    if (disposed) return Promise.resolve();
    return closePanes(state.groupMembers(id).map((pane) => pane.key));
  }

  async function closePanes(keys, { skipDetach = false } = {}) {
    if (disposed) return;
    const wanted = new Set(keys || []);
    const panes = state.panes.value.filter((candidate) => wanted.has(candidate.key));
    if (!panes.length) return;
    for (const pane of panes) widgetFor(pane.id)?.dispose();
    state.removePanes(panes.map((pane) => pane.key));
    if (!skipDetach) await Promise.all(panes.map((pane) => hideSeed(pane.seed)));
  }

  function closePane(key, options) {
    return closePanes([key], options);
  }

  function closeOtherPanes(key) {
    if (!state.panes.value.some((pane) => pane.key === key)) return Promise.resolve();
    return closePanes(state.panes.value
      .filter((pane) => pane.key !== key)
      .map((pane) => pane.key));
  }

  function closeAllPanes() {
    return closePanes(state.panes.value.map((pane) => pane.key));
  }

  function closeForHide(selectors) {
    const wanted = new Set(selectors || []);
    for (const pane of [...state.panes.value]) {
      if (pane.seed.hideConv && wanted.has(pane.seed.hideConv)) {
        void closePane(pane.key, { skipDetach: true });
      }
    }
  }

  function closeForAgents(selectors) {
    const wanted = new Set(selectors || []);
    for (const pane of [...state.panes.value]) {
      if (pane.seed.agent && wanted.has(pane.seed.agent)) void closePane(pane.key);
    }
  }

  function focusForSelectors(selectors, options) {
    const key = state.findPaneKey(selectors);
    return key ? state.activatePane(key, options) : false;
  }

  function hasAgentPane(selectors) {
    const wanted = new Set(selectors || []);
    return state.panes.value.some((pane) => wanted.has(pane.seed.agent) && agentSessionSeed(pane.seed));
  }

  // `detachTo` decides tab vs. window. Browsers open a tab when a window is
  // asked for with no features and a separate window when any are given, so the
  // drag gesture passes the pane size and release point it measured and gets a
  // window; the ⧉ tab button and the tab context menu pass nothing and keep
  // getting an ordinary tab. The caller measures the DOM, this owns the window.
  async function popOutPane(key, { detachTo = null } = {}) {
    const pane = state.panes.value.find((candidate) => candidate.key === key);
    if (!pane) return false;
    const features = detachTo
      ? detachWindowFeatures({ ...detachTo, screen: windowRef.screen })
      : '';
    let target = null;
    try { target = windowRef.open('about:blank', '_blank', features || undefined); }
    catch (_) { target = null; }
    if (!target) {
      // A blocked pop-up leaves the pane exactly where it is — nothing is lost,
      // but the gesture must say so. Drag-out especially: the strip has just
      // promised the terminal was about to leave.
      notify('terminal detach blocked: allow pop-ups for this dashboard to detach a terminal', true);
      return false;
    }
    const seed = {
      ...(pane.seed.board ? { board: pane.seed.board } : { ws: pane.seed.ws }),
      label: pane.label,
      key: pane.seed.key,
      hideConv: pane.seed.hideConv,
      agent: pane.seed.agent,
      harness: pane.seed.harness,
      remote: pane.seed.remote || undefined,
      initialRetry: true,
      wizard: documentRef.body.classList.contains('wizard'),
      terminalAttach: terminalAttachConfig(),
    };
    await closePane(key);
    try { target.location.replace(`/terminals?solo=1${encodeTerminalOpenHash(seed)}`); }
    catch (_) { /* target closed while detach was landing */ }
    return true;
  }

  // openPaneInOverlay moves a board, a shell or an agent from its tab into a
  // floating window. A board's editor saves what it holds as it goes, and a
  // shell's tmux session outlives the tab's view of it. An agent goes to its chat
  // window, which attaches only after this tab let go: the tab's own detach
  // would drop a chat that got there first.
  async function openPaneInOverlay(key) {
    const pane = state.panes.value.find((candidate) => candidate.key === key);
    if (disposed || !pane) return false;
    if (pane.seed.board) {
      await closePane(key);
      openBoardOverlay(pane.seed.board, pane.label);
      return true;
    }
    if (overlayablePane(pane)) {
      await closePane(key, { skipDetach: true });
      openOverlayWindow(terminalWindowOf(pane));
      return true;
    }
    if (!pane.seed.agent || !companionAvailable()) return false;
    floatingAgents += 1;
    try {
      await closePane(key);
      if (floatInCompanion(pane.seed.agent)) return true;
      openPane(pane.seed);
      return false;
    } finally {
      floatingAgents -= 1;
    }
  }

  // An agent on its way to its chat window: the tab has gone, the window hasn't
  // come yet.
  const floatingAgent = () => floatingAgents > 0;

  function paneDisconnected(key) {
    const pane = state.panes.value.find((candidate) => candidate.key === key);
    if (pane) onPaneDisconnected?.(pane);
  }

  function reattachPane(key) {
    if (disposed || typeof onReattachPane !== 'function') return Promise.resolve(false);
    const pane = state.panes.value.find((candidate) => candidate.key === key);
    return pane ? Promise.resolve(onReattachPane(pane)) : Promise.resolve(false);
  }

  function dispose() {
    if (disposed) return;
    disposed = true;
    for (const widget of widgets.values()) widget.dispose();
    widgets.clear();
    state.dispose();
  }

  return Object.freeze({
    registerWidget,
    paneDisconnected,
    waitForPaneSettled,
    widgetFor,
    openPane,
    setPaneLabel: (key, label) => !disposed && state.setPaneLabel(key, label),
    receiveHandoffPane,
    activatePane,
    reorderPane,
    movePaneByOffset,
    movePaneOutsideGroup,
    moveGroup,
    moveGroupByOffset,
    createGroup,
    renameGroup,
    setGroupColor,
    setGroupCollapsed,
    toggleGroupCollapsed,
    dissolveGroup,
    assignPaneToGroup,
    closeGroupPanes,
    closePane,
    closeOtherPanes,
    closeAllPanes,
    closeForHide,
    closeForAgents,
    focusForSelectors,
    popOutPane,
    openPaneInOverlay,
    hasAgentPane,
    floatingAgent,
    reattachPane,
    setArcanePaletteEnabled,
    dispose,
  });
}
