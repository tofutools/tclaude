import { signal } from '@preact/signals';

export function createWorktreeCleanupState() {
  const dialog = signal(null);
  let sequence = 0;
  let resolveCurrent = null;

  function open(group = '') {
    if (dialog.value || resolveCurrent) return Promise.resolve(null);
    dialog.value = Object.freeze({
      key: `worktree-cleanup:${++sequence}`,
      descriptor: Object.freeze({ group: String(group || '') }),
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
    open,
    finish,
    close: () => finish(null),
    dispose: () => finish(null),
  });
}
