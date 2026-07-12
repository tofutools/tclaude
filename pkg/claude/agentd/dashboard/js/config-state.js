import { computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';

export function createConfigState({ activeTab = dashboardState.activeTab } = {}) {
  const phase = signal('idle');
  const dirty = signal(false);
  const error = signal(null);
  const metadata = signal(null);
  const lists = signal({});
  const diff = signal(null);
  let settleDiff = null;
  const view = computed(() => ({ active: activeTab.value === 'config', phase: phase.value, dirty: dirty.value, error: error.value, metadata: metadata.value }));
  const lifecycle = Object.freeze({
    loading() { phase.value = 'loading'; error.value = null; },
    loaded(data) { phase.value = 'ready'; dirty.value = false; error.value = null; metadata.value = data; },
    saving() { phase.value = 'saving'; error.value = null; },
    ready() { phase.value = 'ready'; },
    saved(data) { phase.value = 'ready'; dirty.value = false; error.value = null; metadata.value = data; },
    failed(value) { phase.value = 'error'; error.value = value?.message || String(value); },
  });
  function markDirty() { if (phase.value === 'ready' || phase.value === 'error') dirty.value = true; }
  function confirmDiff(beforeRaw, afterRaw, malformed, path) {
    cancelDiff(false);
    return new Promise(resolve => {
      settleDiff = resolve;
      diff.value = { beforeRaw, afterRaw, malformed, path };
    });
  }
  function cancelDiff(result = false) {
    const resolve = settleDiff;
    settleDiff = null;
    diff.value = null;
    resolve?.(result);
  }
  const listController = Object.freeze({
    get(id) { return lists.value[id] || []; },
    set(id, values) { lists.value = { ...lists.value, [id]: values }; },
  });
  return Object.freeze({ phase, dirty, error, metadata, lists, diff, listController, view, lifecycle, markDirty, confirmDiff, cancelDiff });
}
export const configState = createConfigState();
