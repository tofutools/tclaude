// Lifecycle controller for the standalone /terminals?solo=1 pop-out. Preact
// owns the page's single stable root; xterm remains behind the same opaque
// adapter as the dashboard shell.

import { signal } from '@preact/signals';
import { createTerminalShellActions } from './terminal-shell-actions.js';
import { mountStandaloneTerminalShell } from './terminal-shell-island.js';
import { createTerminalShellState } from './terminal-shell-state.js';
import {
  activeMessageAccessDialogKind, openOperatorMessageDialog,
} from './message-access-dialog-controller.js';
import { mountMessageAccessDialogsFeature } from './preact-loader.js';
import {
  decodeTerminalOpenHash, requestTerminalReattach, terminalDashboardURL,
} from './terminal-handoff.js';
import { normalizeSeed } from './terminals-core.js';
import { SNAPSHOT_HIDDEN_POLL_MS, SNAPSHOT_POLL_MS } from './snapshot-poll.js';

export { decodeTerminalOpenHash };

export const STANDALONE_SNAPSHOT_TIMEOUT_MS = 20000;

// The solo page is not the dashboard SPA, so it cannot subscribe to the
// dashboard snapshot signal. Keep its one-agent title current with the same
// 2s/10s visible/hidden cadence, but keep the lifecycle local and bounded:
// there is never more than one request or timer in flight, and stop() aborts
// both beforeunload-sensitive work and its next scheduled tick.
export function startStandaloneSnapshotPoll({
  snapshot,
  fetchImpl = globalThis.fetch,
  documentRef = globalThis.document,
  setTimeoutImpl = globalThis.setTimeout,
  clearTimeoutImpl = globalThis.clearTimeout,
  abortControllerCtor = globalThis.AbortController,
} = {}) {
  if (!snapshot || typeof fetchImpl !== 'function') return () => {};
  if (typeof setTimeoutImpl !== 'function' || typeof clearTimeoutImpl !== 'function') return () => {};

  let stopped = false;
  let timer = null;
  let timeoutTimer = null;
  let inFlight = false;
  let controller = null;

  const clearTimer = () => {
    if (timer === null) return;
    clearTimeoutImpl(timer);
    timer = null;
  };
  const clearRequestTimeout = () => {
    if (timeoutTimer === null) return;
    clearTimeoutImpl(timeoutTimer);
    timeoutTimer = null;
  };
  const schedule = () => {
    if (stopped || timer !== null) return;
    timer = setTimeoutImpl(() => {
      timer = null;
      void run();
    }, documentRef?.hidden ? SNAPSHOT_HIDDEN_POLL_MS : SNAPSHOT_POLL_MS);
  };
  const run = async () => {
    if (stopped || inFlight) return;
    inFlight = true;
    const requestController = typeof abortControllerCtor === 'function'
      ? new abortControllerCtor() : null;
    controller = requestController;
    if (requestController) {
      timeoutTimer = setTimeoutImpl(() => requestController.abort(), STANDALONE_SNAPSHOT_TIMEOUT_MS);
    }
    try {
      const response = await fetchImpl('/api/snapshot', {
        credentials: 'same-origin', cache: 'no-store',
        ...(requestController ? { signal: requestController.signal } : {}),
      });
      if (stopped || !response?.ok) return;
      const value = await response.json();
      if (!stopped && value && typeof value === 'object') snapshot.value = value;
    } catch (_) {
      // Auth-session.js owns the visible redirect. A stopped/aborted request or
      // a transient network miss should leave the last title alone quietly.
    } finally {
      clearRequestTimeout();
      if (controller === requestController) controller = null;
      inFlight = false;
      schedule();
    }
  };
  const onVisibilityChange = () => {
    clearTimer();
    if (!documentRef?.hidden) void run();
    schedule();
  };
  documentRef?.addEventListener?.('visibilitychange', onVisibilityChange);
  void run();

  return () => {
    if (stopped) return;
    stopped = true;
    clearTimer();
    clearRequestTimeout();
    controller?.abort();
    controller = null;
    documentRef?.removeEventListener?.('visibilitychange', onVisibilityChange);
  };
}

export function createStandaloneTerminalsPage({
  host,
  initPrefs,
  initThemeSync,
  widgetFactory,
  fetchImpl = globalThis.fetch,
  windowRef = globalThis.window,
  documentRef = globalThis.document,
  locationRef = globalThis.location,
  historyRef = globalThis.history,
  navigatorRef = globalThis.navigator,
  mountShell = mountStandaloneTerminalShell,
  mountMessageDialogs = mountMessageAccessDialogsFeature,
  openComposeMessage = openOperatorMessageDialog,
  composeMessageDialogKind = activeMessageAccessDialogKind,
} = {}) {
  if (!host) throw new TypeError('standalone terminal page requires a host');
  if (typeof initPrefs !== 'function' || typeof initThemeSync !== 'function') {
    throw new TypeError('standalone terminal page requires preference initializers');
  }

  // A solo pop-out has no tab strip or group controls, so attaching its one
  // pane must not rewrite remembered dashboard group presentation.
  const state = createTerminalShellState({ persistPresentation: false });
  const composeMessageReady = signal(false);
  const detachConversations = new Set();
  let actions = null;
  let mountCleanup = null;
  let messageDialogsCleanup = null;
  let messageDialogsPromise = null;
  let prefsReady = false;
  let soloSeed = null;
  let startPromise = null;
  let disposed = false;
  let authExpired = false;
  const snapshot = signal(null);
  let stopSnapshotPoll = null;

  function ensureSnapshotPoll() {
    if (stopSnapshotPoll || disposed || authExpired || !(soloSeed?.agent || soloSeed?.hideConv)) return;
    stopSnapshotPoll = startStandaloneSnapshotPoll({ snapshot, fetchImpl, documentRef });
  }

  function stopSnapshotPolling() {
    stopSnapshotPoll?.();
    stopSnapshotPoll = null;
  }

  async function reattachPane(pane) {
    if (disposed || !pane) return false;
    const seed = {
      ...pane.seed,
      label: pane.label,
      initialRetry: true,
    };
    if (seed.hideConv) detachConversations.delete(seed.hideConv);
    await actions.closePane(pane.key);

    const opener = windowRef.opener;
    const accepted = await requestTerminalReattach({
      seed, targetWindow: opener, windowRef, locationRef,
    });
    if (accepted) {
      try { opener.focus(); } catch (_) { /* browser focus is best-effort */ }
      try { windowRef.close(); } catch (_) { /* fallback below is no longer needed */ }
      return true;
    }

    const target = terminalDashboardURL(seed);
    if (typeof locationRef.replace === 'function') locationRef.replace(target);
    else locationRef.href = target;
    return true;
  }

  actions = createTerminalShellActions({
    state, fetchImpl, windowRef, documentRef, onReattachPane: reattachPane,
  });

  function consumeHash() {
    if (disposed) return null;
    const seed = normalizeSeed(decodeTerminalOpenHash(locationRef.hash));
    // Consuming the seed prevents an ordinary reload from racing the old
    // client's detach beacon against a fresh connection. Auth recovery below
    // reconstructs it deliberately when a sign-in redirect is required.
    if (locationRef.hash) {
      historyRef.replaceState(null, '', locationRef.pathname + locationRef.search);
    }
    if (!seed) return null;

    documentRef.body.classList.toggle('wizard', seed.wizard === true);
    documentRef.dispatchEvent(new windowRef.CustomEvent('tclaude:wizard', {
      detail: { active: seed.wizard === true },
    }));
    soloSeed = seed;
    actions.openPane(seed);
    if (seed.hideConv) detachConversations.add(seed.hideConv);
    ensureSnapshotPoll();
    return seed;
  }

  function onHashChange() {
    if (prefsReady) consumeHash();
  }

  function onAuthExpired(event) {
    authExpired = true;
    stopSnapshotPolling();
    if (!soloSeed || !event.detail) return;
    event.detail.returnTo = locationRef.pathname + locationRef.search
      + '#open=' + encodeURIComponent(JSON.stringify(soloSeed));
  }

  function onPageHide() {
    for (const conv of detachConversations) {
      try { navigatorRef.sendBeacon('/api/hide/' + encodeURIComponent(conv)); }
      catch (_) { /* best-effort detach while the document is leaving */ }
    }
  }

  function dispose() {
    if (disposed) return;
    disposed = true;
    windowRef.removeEventListener('hashchange', onHashChange);
    windowRef.removeEventListener('tclaude:auth-expired', onAuthExpired);
    windowRef.removeEventListener('pagehide', onPageHide);
    windowRef.removeEventListener('unload', dispose);
    stopSnapshotPolling();
    if (mountCleanup) mountCleanup();
    else actions.dispose();
    if (messageDialogsCleanup) messageDialogsCleanup();
    mountCleanup = null;
    messageDialogsCleanup = null;
  }

  windowRef.addEventListener('hashchange', onHashChange);
  windowRef.addEventListener('tclaude:auth-expired', onAuthExpired);
  windowRef.addEventListener('pagehide', onPageHide);
  windowRef.addEventListener('unload', dispose);

  function start() {
    if (startPromise) return startPromise;
    startPromise = Promise.resolve(initPrefs()).then(() => {
      if (disposed) return false;
      initThemeSync();
      if (disposed) return false;

      // The terminal is the primary surface, so connect it without waiting for
      // the optional composer module graph. An early shortcut is queued against
      // this promise and opens as soon as the shared dialog island is ready.
      messageDialogsPromise = Promise.resolve().then(() => mountMessageDialogs({
        fetchImpl,
        refresh: async () => {},
        notify: () => {},
        confirmDiscard: () => windowRef.confirm('Discard this message draft?'),
      })).then((cleanup) => {
        if (disposed) {
          cleanup?.();
          return null;
        }
        messageDialogsCleanup = cleanup;
        composeMessageReady.value = Boolean(cleanup);
        return cleanup;
      }, (error) => {
        console.error('Detached terminal message composer unavailable.', error);
        return null;
      });
      const composeWhenReady = (seed) => {
        void messageDialogsPromise.then((cleanup) => {
          if (!disposed && cleanup) openComposeMessage(seed);
        });
      };
      mountCleanup = mountShell({
        host,
        state,
        actions,
        snapshot,
        widgetFactory,
        onComposeMessage: composeWhenReady,
        composeMessageReady,
        composeMessageDialogKind,
      });
      prefsReady = true;
      consumeHash();
      return true;
    });
    return startPromise;
  }

  return Object.freeze({ state, actions, start, consumeHash, dispose });
}
