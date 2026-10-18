import { signal } from '@preact/signals';
export function createGitRepositoriesState() {
  const dialog = signal(null);
  let sequence = 0;
  let resolveCurrent;
  function finish() {
    dialog.value = null;
    resolveCurrent?.();
    resolveCurrent = null;
  }
  return {
    dialog,
    open(mode, group, node = '') {
      if (dialog.value) return Promise.resolve();
      dialog.value = Object.freeze({ key: ++sequence, mode, group, node: String(node || '') });
      return new Promise((resolve) => { resolveCurrent = resolve; });
    },
    close: finish,
    dispose: finish,
  };
}
