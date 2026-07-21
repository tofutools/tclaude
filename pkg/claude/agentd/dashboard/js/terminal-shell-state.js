import { computed, signal } from '@preact/signals';
import { dashPrefs } from './prefs.js';
import { normalizeSeed } from './terminals-core.js';

export const TERMINAL_PANE_ORDER_KEY = 'tclaude.dash.terminals.order';
export const MAX_REMEMBERED_TERMINAL_PANES = 512;
export const MAX_TERMINAL_PANE_ORDER_BYTES = 60 * 1024;

function boundPreferredOrder(keys) {
  const bounded = [];
  const encoder = new TextEncoder();
  let bytes = 2; // JSON array brackets.
  for (const key of keys) {
    if (bounded.length >= MAX_REMEMBERED_TERMINAL_PANES) break;
    const entryBytes = encoder.encode(JSON.stringify(key)).byteLength + (bounded.length ? 1 : 0);
    if (bytes + entryBytes > MAX_TERMINAL_PANE_ORDER_BYTES) continue;
    bounded.push(key);
    bytes += entryBytes;
  }
  return bounded;
}

export function terminalSeedKey(seed) {
  return seed.key || seed.ws;
}

export function createTerminalShellState({ prefs = dashPrefs, persistOrder = true } = {}) {
  const panes = signal([]);
  const activeKey = signal(null);
  const modal = signal(null);
  const revealRequest = signal(0);
  const view = computed(() => ({
    panes: panes.value,
    activeKey: activeKey.value,
    modal: modal.value,
    count: panes.value.length,
    revealRequest: revealRequest.value,
  }));
  let paneSequence = 0;
  let modalSequence = 0;
  let preferredOrder = null;

  function readPreferredOrder() {
    if (preferredOrder) return preferredOrder;
    try {
      const parsed = JSON.parse(prefs.getItem(TERMINAL_PANE_ORDER_KEY) || '[]');
      preferredOrder = Array.isArray(parsed)
        ? boundPreferredOrder([...new Set(parsed.filter((key) => typeof key === 'string' && key))])
        : [];
    } catch (_) {
      preferredOrder = [];
    }
    return preferredOrder;
  }

  function persistPreferredOrder(visibleKeys = panes.value.map((pane) => pane.key)) {
    const visible = new Set(visibleKeys);
    preferredOrder = boundPreferredOrder([
      ...visibleKeys,
      ...readPreferredOrder().filter((key) => !visible.has(key)),
    ]);
    if (!persistOrder) return;
    try { prefs.setItem(TERMINAL_PANE_ORDER_KEY, JSON.stringify(preferredOrder)); } catch (_) {}
  }

  function sortByPreferredOrder(items) {
    const rank = new Map(readPreferredOrder().map((key, index) => [key, index]));
    return items
      .map((pane, index) => ({ pane, index }))
      .sort((a, b) => (rank.get(a.pane.key) ?? Number.MAX_SAFE_INTEGER)
        - (rank.get(b.pane.key) ?? Number.MAX_SAFE_INTEGER) || a.index - b.index)
      .map(({ pane }) => pane);
  }

  function commitPaneOrder(next) {
    if (next.every((pane, index) => panes.value[index] === pane)) return null;
    panes.value = next;
    persistPreferredOrder();
    return Object.freeze({
      pane: next.find((pane) => pane.key === activeKey.value) || null,
      panes: next,
    });
  }

  function requestReveal() {
    revealRequest.value += 1;
  }

  function openPane(raw, { reveal = true } = {}) {
    const seed = normalizeSeed(raw);
    if (!seed) return null;
    const key = terminalSeedKey(seed);
    const existing = panes.value.find((pane) => pane.key === key);
    if (existing) {
      activeKey.value = key;
      if (reveal) requestReveal();
      return existing;
    }
    paneSequence += 1;
    const pane = Object.freeze({
      id: `terminal-pane-${paneSequence}`,
      key,
      label: seed.label || 'terminal',
      seed: Object.freeze({ ...seed }),
    });
    const preferred = readPreferredOrder();
    if (!preferred.includes(key)) preferred.push(key);
    panes.value = sortByPreferredOrder([...panes.value, pane]);
    activeKey.value = key;
    if (reveal) requestReveal();
    return pane;
  }

  function activatePane(key, { reveal = true } = {}) {
    if (!panes.value.some((pane) => pane.key === key)) return false;
    activeKey.value = key;
    if (reveal) requestReveal();
    return true;
  }

  function removePanes(keys) {
    const current = panes.value;
    const wanted = new Set(keys || []);
    const removed = current.filter((candidate) => wanted.has(candidate.key));
    if (!removed.length) return [];
    const next = current.filter((candidate) => !wanted.has(candidate.key));
    const previousActive = activeKey.value;
    panes.value = next;
    if (!next.some((candidate) => candidate.key === previousActive)) {
      const previousIndex = current.findIndex((candidate) => candidate.key === previousActive);
      const successor = previousIndex < 0 ? null : current.slice(previousIndex + 1)
        .find((candidate) => !wanted.has(candidate.key));
      const predecessor = previousIndex < 0 ? null : current.slice(0, previousIndex).reverse()
        .find((candidate) => !wanted.has(candidate.key));
      activeKey.value = successor?.key || predecessor?.key || next[0]?.key || null;
    }
    return removed;
  }

  function removePane(key) {
    return removePanes([key])[0] || null;
  }

  function movePane(key, toIndex) {
    const current = panes.value;
    const fromIndex = current.findIndex((pane) => pane.key === key);
    if (fromIndex < 0 || current.length < 2 || !Number.isInteger(toIndex)) return null;
    const destination = Math.max(0, Math.min(current.length - 1, toIndex));
    if (destination === fromIndex) return null;
    const next = [...current];
    const [pane] = next.splice(fromIndex, 1);
    next.splice(destination, 0, pane);
    if (!commitPaneOrder(next)) return null;
    return Object.freeze({ pane, index: destination, count: next.length });
  }

  function reorderPane(key, targetKey, { after = false } = {}) {
    if (key === targetKey) return null;
    const current = panes.value;
    const pane = current.find((candidate) => candidate.key === key);
    if (!pane || !current.some((candidate) => candidate.key === targetKey)) return null;
    const next = current.filter((candidate) => candidate.key !== key);
    const targetIndex = next.findIndex((candidate) => candidate.key === targetKey);
    const destination = targetIndex + (after ? 1 : 0);
    next.splice(destination, 0, pane);
    if (!commitPaneOrder(next)) return null;
    return Object.freeze({ pane, index: destination, count: next.length });
  }

  function movePaneByOffset(key, offset) {
    const index = panes.value.findIndex((pane) => pane.key === key);
    if (index < 0 || !Number.isInteger(offset) || offset === 0) return null;
    return movePane(key, index + offset);
  }

  function openModal({ wsPath, ws, label = '', hideConv = null, initialRetry = false } = {}) {
    const seed = normalizeSeed({
      ws: wsPath || ws, label, hideConv: hideConv || null, initialRetry: initialRetry === true,
    });
    if (!seed) return null;
    modalSequence += 1;
    const descriptor = Object.freeze({
      id: `terminal-modal-${modalSequence}`,
      label: seed.label || '',
      seed: Object.freeze({ ...seed }),
    });
    modal.value = descriptor;
    return descriptor;
  }

  function closeModal(id) {
    if (!modal.value || (id && modal.value.id !== id)) return null;
    const descriptor = modal.value;
    modal.value = null;
    return descriptor;
  }

  function findPaneKey(selectors) {
    const wanted = new Set(selectors || []);
    if (!wanted.size) return null;
    return panes.value.find((pane) => wanted.has(pane.seed.agent))?.key || null;
  }

  function dispose() {
    panes.value = [];
    activeKey.value = null;
    modal.value = null;
  }

  return Object.freeze({
    panes,
    activeKey,
    modal,
    revealRequest,
    view,
    openPane,
    activatePane,
    removePane,
    removePanes,
    movePane,
    reorderPane,
    movePaneByOffset,
    openModal,
    closeModal,
    findPaneKey,
    requestReveal,
    dispose,
  });
}

export const terminalShellState = createTerminalShellState();
