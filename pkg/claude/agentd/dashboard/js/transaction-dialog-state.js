import { computed, signal } from '@preact/signals';

function isPlainObject(value) {
  if (!value || typeof value !== 'object') return false;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

// Dialog descriptors cross an imperative-launcher → Preact ownership seam.
// Clone and freeze their plain data at the seam so a later snapshot publish,
// caller mutation, or retry cannot silently retarget an already-visible
// destructive transaction. Non-plain values (for example a captured DOM
// opener) retain identity and are never traversed.
export function freezeDialogDescriptor(value) {
  if (Array.isArray(value)) {
    return Object.freeze(value.map((item) => freezeDialogDescriptor(item)));
  }
  if (!isPlainObject(value)) return value;
  const clone = {};
  for (const [key, item] of Object.entries(value)) {
    clone[key] = freezeDialogDescriptor(item);
  }
  return Object.freeze(clone);
}

export function createTransactionDialogState() {
  const dialog = signal(null);
  const view = computed(() => ({ dialog: dialog.value }));
  let sequence = 0;
  let resolveCurrent = null;

  function open(descriptor) {
    if (!descriptor?.kind) throw new Error('transaction dialog kind is required');
    // Promise-backed launch adapters must never be orphaned by a second click
    // or a poll-driven rerender. The first descriptor remains the sole owner.
    if (dialog.value || resolveCurrent) return Promise.resolve(null);
    const snapshot = freezeDialogDescriptor(descriptor);
    dialog.value = Object.freeze({
      key: `${snapshot.kind}:${++sequence}`,
      descriptor: snapshot,
    });
    return new Promise((resolve) => { resolveCurrent = resolve; });
  }

  function finish(result = null) {
    const resolve = resolveCurrent;
    resolveCurrent = null;
    dialog.value = null;
    resolve?.(result);
  }

  return Object.freeze({
    dialog,
    view,
    open,
    finish,
    close: () => finish(null),
    dispose: () => finish(null),
  });
}
