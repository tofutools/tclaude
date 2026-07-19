import { computed, signal } from '@preact/signals';
import { normalizeSeed } from './terminals-core.js';

export function terminalSeedKey(seed) {
  return seed.key || seed.ws;
}

export function createTerminalShellState() {
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
    panes.value = [...panes.value, pane];
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

  function removePane(key) {
    const current = panes.value;
    const pane = current.find((candidate) => candidate.key === key);
    if (!pane) return null;
    const next = current.filter((candidate) => candidate.key !== key);
    panes.value = next;
    if (activeKey.value === key) activeKey.value = next[0]?.key || null;
    return pane;
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
    openModal,
    closeModal,
    findPaneKey,
    requestReveal,
    dispose,
  });
}

export const terminalShellState = createTerminalShellState();
