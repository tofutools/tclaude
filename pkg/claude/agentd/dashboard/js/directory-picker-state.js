import { signal } from '@preact/signals';

export function createDirectoryPickerState() {
  const request = signal(null);
  let settle = null;
  const finish = (result = { canceled: true }) => {
    const resolve = settle;
    settle = null;
    request.value = null;
    resolve?.(result);
  };
  return Object.freeze({
    request,
    open(options = {}) {
      if (settle) return Promise.resolve({ error: 'a directory picker is already open' });
      request.value = {
        startDir: String(options.startDir || '').trim(),
        title: String(options.title || '').trim() || 'Select a directory',
      };
      return new Promise((resolve) => { settle = resolve; });
    },
    finish,
  });
}
