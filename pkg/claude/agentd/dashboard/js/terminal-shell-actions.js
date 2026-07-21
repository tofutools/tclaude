import { setArcanePaletteEnabled } from './terminal-theme.js';
import { encodeTerminalOpenHash } from './terminal-handoff.js';
import { shellToast } from './shell-state.js';

export function createTerminalShellActions({
  state,
  confirm = async () => false,
  fetchImpl = globalThis.fetch,
  windowRef = globalThis.window,
  documentRef = globalThis.document,
  notify = shellToast,
  onReattachPane = null,
} = {}) {
  if (!state) throw new TypeError('terminal shell actions require state');
  const widgets = new Map();
  let confirmOpen = false;
  let disposed = false;

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

  function activatePane(key) {
    return !disposed && state.activatePane(key);
  }

  function reorderPane(key, targetKey, options) {
    return disposed ? null : state.reorderPane(key, targetKey, options);
  }

  function movePaneByOffset(key, offset) {
    return disposed ? null : state.movePaneByOffset(key, offset);
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

  async function popOutPane(key) {
    const pane = state.panes.value.find((candidate) => candidate.key === key);
    if (!pane) return false;
    let target = null;
    try { target = windowRef.open('about:blank', '_blank'); } catch (_) { target = null; }
    if (!target) {
      // A blocked pop-up leaves the pane exactly where it is — nothing is lost,
      // but the gesture must say so. Drag-out especially: the strip has just
      // promised the terminal was about to leave.
      notify('terminal detach blocked: allow pop-ups for this dashboard to open a terminal in its own browser tab', true);
      return false;
    }
    const seed = {
      ws: pane.seed.ws,
      label: pane.label,
      key: pane.seed.key,
      hideConv: pane.seed.hideConv,
      agent: pane.seed.agent,
      initialRetry: true,
      wizard: documentRef.body.classList.contains('wizard'),
    };
    await closePane(key);
    try { target.location.replace(`/terminals?solo=1${encodeTerminalOpenHash(seed)}`); }
    catch (_) { /* target closed while detach was landing */ }
    return true;
  }

  function reattachPane(key) {
    if (disposed || typeof onReattachPane !== 'function') return Promise.resolve(false);
    const pane = state.panes.value.find((candidate) => candidate.key === key);
    return pane ? Promise.resolve(onReattachPane(pane)) : Promise.resolve(false);
  }

  function openModal(descriptor) {
    if (disposed) return null;
    return state.openModal(descriptor);
  }

  async function closeModal(id, { detach = false } = {}) {
    const descriptor = state.modal.value;
    if (!descriptor || (id && descriptor.id !== id)) return;
    widgetFor(descriptor.id)?.dispose();
    state.closeModal(descriptor.id);
    if (detach) await hideSeed(descriptor.seed);
  }

  async function promptModalReconnect(id) {
    if (disposed || confirmOpen || state.modal.value?.id !== id) return;
    confirmOpen = true;
    let reconnect = false;
    try {
      reconnect = await confirm({
        title: 'Terminal disconnected',
        body: 'The connection to the terminal was closed. The underlying session keeps running — reconnect to it, or close this terminal?',
        okLabel: 'Reconnect',
        cancelLabel: 'Close terminal',
      });
    } finally {
      confirmOpen = false;
    }
    if (disposed || state.modal.value?.id !== id) return;
    if (reconnect) void widgetFor(id)?.connect();
    else await closeModal(id);
  }

  function onModalDisconnect(id) {
    void promptModalReconnect(id);
  }

  async function confirmModalClose(id) {
    const descriptor = state.modal.value;
    if (disposed || confirmOpen || !descriptor || descriptor.id !== id) return;
    confirmOpen = true;
    let close = false;
    try {
      close = await confirm(descriptor.seed.hideConv ? {
        title: 'Detach terminal?',
        body: 'This only drops your view — the agent keeps running, and you can reopen it to reattach.',
        okLabel: 'Detach',
        cancelLabel: 'Keep open',
      } : {
        title: 'Close terminal?',
        body: 'The underlying session keeps running — you can reopen it to reattach.',
        okLabel: 'Close terminal',
        cancelLabel: 'Keep open',
      });
    } finally {
      confirmOpen = false;
    }
    if (disposed || state.modal.value?.id !== id) return;
    if (close) {
      await closeModal(id, { detach: true });
      return;
    }
    if (widgetFor(id)?.status() === 'disconnected') void promptModalReconnect(id);
  }

  function detachModal(id) {
    return closeModal(id, { detach: true });
  }

  async function moveModalToPane(id) {
    const descriptor = state.modal.value;
    if (!descriptor || descriptor.id !== id) return;
    const seed = {
      ws: descriptor.seed.ws,
      label: descriptor.label,
      hideConv: descriptor.seed.hideConv,
      agent: descriptor.seed.hideConv,
      initialRetry: descriptor.seed.initialRetry,
    };
    await closeModal(id, { detach: true });
    openPane(seed);
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
    widgetFor,
    openPane,
    receiveHandoffPane,
    activatePane,
    reorderPane,
    movePaneByOffset,
    closePane,
    closeOtherPanes,
    closeAllPanes,
    closeForHide,
    closeForAgents,
    focusForSelectors,
    popOutPane,
    reattachPane,
    openModal,
    closeModal,
    promptModalReconnect,
    onModalDisconnect,
    confirmModalClose,
    detachModal,
    moveModalToPane,
    setArcanePaletteEnabled,
    dispose,
  });
}
