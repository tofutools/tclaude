// Same-origin browser-tab handoff for terminal panes. The URL hash is the
// durable/fallback carrier; postMessage targets the exact dashboard tab that
// opened a solo terminal and provides an acknowledgement before that pop-out
// closes itself.

import { normalizeSeed } from './terminals-core.js';

export const TERMINAL_REATTACH_REQUEST = 'tclaude:terminal-reattach-request';
export const TERMINAL_REATTACH_ACK = 'tclaude:terminal-reattach-ack';

let requestSequence = 0;

function locationOrigin(locationRef) {
  if (locationRef?.origin) return locationRef.origin;
  if (locationRef?.protocol && locationRef?.host) {
    return `${locationRef.protocol}//${locationRef.host}`;
  }
  return '';
}

export function encodeTerminalOpenHash(seed) {
  return '#open=' + encodeURIComponent(JSON.stringify(seed));
}

export function decodeTerminalOpenHash(hash) {
  const match = /[#&]open=([^&]+)/.exec(hash || '');
  if (!match) return null;
  try { return JSON.parse(decodeURIComponent(match[1])); }
  catch (_) { return null; }
}

export function terminalDashboardURL(seed) {
  return '/terminals' + encodeTerminalOpenHash(seed);
}

export function requestTerminalReattach({
  seed,
  targetWindow,
  windowRef = globalThis.window,
  locationRef = globalThis.location,
  timeoutMs = 1500,
  setTimeoutImpl = globalThis.setTimeout,
  clearTimeoutImpl = globalThis.clearTimeout,
} = {}) {
  const normalized = normalizeSeed(seed);
  const origin = locationOrigin(locationRef);
  if (!normalized || !targetWindow || targetWindow.closed || !origin) {
    return Promise.resolve(false);
  }

  requestSequence += 1;
  const id = `terminal-reattach-${requestSequence}`;
  return new Promise((resolve) => {
    let settled = false;
    const finish = (accepted) => {
      if (settled) return;
      settled = true;
      clearTimeoutImpl(timer);
      windowRef.removeEventListener('message', onMessage);
      resolve(accepted);
    };
    const onMessage = (event) => {
      if (event.origin !== origin || event.source !== targetWindow) return;
      if (event.data?.type !== TERMINAL_REATTACH_ACK || event.data?.id !== id) return;
      finish(event.data.accepted === true);
    };
    const timer = setTimeoutImpl(() => finish(false), timeoutMs);
    windowRef.addEventListener('message', onMessage);
    try {
      targetWindow.postMessage({
        type: TERMINAL_REATTACH_REQUEST,
        id,
        seed: { ...normalized, initialRetry: true },
      }, origin);
    } catch (_) {
      finish(false);
    }
  });
}

export function bindTerminalHandoffReceiver({
  openSeed,
  windowRef = globalThis.window,
  locationRef = globalThis.location,
  historyRef = globalThis.history,
} = {}) {
  if (typeof openSeed !== 'function') {
    throw new TypeError('terminal handoff receiver requires openSeed');
  }
  const origin = locationOrigin(locationRef);
  let disposed = false;
  let pendingHash = null;
  let pendingClaim = null;

  function pageURL(hash = '') {
    return locationRef.pathname + locationRef.search + hash;
  }

  function retain(seed) {
    const hash = encodeTerminalOpenHash(seed);
    // Claim the handoff in the opener's URL before acknowledging it. The
    // pop-out can then close immediately without racing a cold xterm load, and
    // a reload of the opener can still finish the handoff if that load fails.
    if (typeof historyRef?.replaceState !== 'function') return null;
    try {
      historyRef.replaceState(null, '', pageURL(hash));
      return hash;
    } catch (_) {
      return null;
    }
  }

  function clearRetained(hash) {
    if (locationRef.hash === hash) historyRef?.replaceState?.(null, '', pageURL());
  }

  async function accept(rawSeed) {
    const seed = normalizeSeed(rawSeed);
    if (disposed || !seed) return false;
    try {
      return Boolean(await openSeed({ ...seed, initialRetry: true }));
    } catch (error) {
      console.error('terminal handoff failed:', error);
      return false;
    }
  }

  async function onMessage(event) {
    if (disposed || !origin || event.origin !== origin || !event.source) return;
    if (event.data?.type !== TERMINAL_REATTACH_REQUEST || typeof event.data?.id !== 'string') return;
    const seed = normalizeSeed(event.data.seed);
    if (!seed) return;
    const retainedSeed = normalizeSeed(decodeTerminalOpenHash(locationRef.hash));
    if (pendingClaim || pendingHash !== null || retainedSeed) {
      // The URL is the durable single-owner slot. Reject another pop-out so it
      // can become its own dashboard instead of overwriting an acknowledged
      // handoff that is still loading (or waiting for a reload retry).
      try {
        event.source.postMessage({
          type: TERMINAL_REATTACH_ACK, id: event.data.id, accepted: false,
        }, event.origin);
      } catch (_) { /* the sender's timeout reaches the same fallback */ }
      return;
    }
    const retainedHash = retain({ ...seed, initialRetry: true });
    if (!retainedHash) return;
    pendingClaim = retainedHash;
    let acknowledged = false;
    try {
      event.source.postMessage({
        type: TERMINAL_REATTACH_ACK,
        id: event.data.id,
        // "accepted" means this exact opener has durably claimed the seed.
        // Opening can await a cold runtime load without making the sender
        // fall back into a second dashboard.
        accepted: true,
      }, event.origin);
      acknowledged = true;
    } catch (_) { /* the pop-out may have closed while the pane was opening */ }
    if (!acknowledged) {
      clearRetained(retainedHash);
      pendingClaim = null;
      return;
    }
    try { windowRef.focus(); } catch (_) { /* browser focus is best-effort */ }
    try {
      if (await accept(seed)) clearRetained(retainedHash);
    } finally {
      if (pendingClaim === retainedHash) pendingClaim = null;
    }
  }

  function consumeHash() {
    if (disposed || !locationRef) return false;
    const hash = locationRef.hash;
    const seed = normalizeSeed(decodeTerminalOpenHash(hash));
    if (!seed || pendingHash === hash) return false;
    pendingHash = hash;
    void accept(seed).then((accepted) => {
      if (accepted) clearRetained(hash);
    }).finally(() => {
      if (pendingHash === hash) pendingHash = null;
      if (locationRef.hash && locationRef.hash !== hash) consumeHash();
    });
    return true;
  }

  windowRef.addEventListener('message', onMessage);
  windowRef.addEventListener('hashchange', consumeHash);
  consumeHash();

  return () => {
    if (disposed) return;
    disposed = true;
    windowRef.removeEventListener('message', onMessage);
    windowRef.removeEventListener('hashchange', consumeHash);
  };
}
