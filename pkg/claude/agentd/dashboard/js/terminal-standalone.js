// Lifecycle controller for the standalone /terminals?solo=1 pop-out. Preact
// owns the page's single stable root; xterm remains behind the same opaque
// adapter as the dashboard shell.

import { createTerminalShellActions } from './terminal-shell-actions.js';
import { mountStandaloneTerminalShell } from './terminal-shell-island.js';
import { createTerminalShellState } from './terminal-shell-state.js';
import {
  decodeTerminalOpenHash, requestTerminalReattach, terminalDashboardURL,
} from './terminal-handoff.js';
import { normalizeSeed } from './terminals-core.js';

export { decodeTerminalOpenHash };

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
} = {}) {
  if (!host) throw new TypeError('standalone terminal page requires a host');
  if (typeof initPrefs !== 'function' || typeof initThemeSync !== 'function') {
    throw new TypeError('standalone terminal page requires preference initializers');
  }

  // A solo pop-out has no tab strip, so it must not overwrite the dashboard's
  // persisted presentation order merely by attaching its one pane.
  const state = createTerminalShellState({ persistOrder: false });
  const detachConversations = new Set();
  let actions = null;
  let mountCleanup = null;
  let prefsReady = false;
  let soloSeed = null;
  let startPromise = null;
  let disposed = false;

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
    return seed;
  }

  function onHashChange() {
    if (prefsReady) consumeHash();
  }

  function onAuthExpired(event) {
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
    if (mountCleanup) mountCleanup();
    else actions.dispose();
    mountCleanup = null;
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
      mountCleanup = mountShell({ host, state, actions, widgetFactory });
      prefsReady = true;
      consumeHash();
      return true;
    });
    return startPromise;
  }

  return Object.freeze({ state, actions, start, consumeHash, dispose });
}
