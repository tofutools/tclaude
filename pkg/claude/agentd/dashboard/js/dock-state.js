import { signal } from '@preact/signals';

export function createDockState() {
  const snapshot = signal(null);

  function publish(value) {
    snapshot.value = value ? { ...value } : null;
  }

  return Object.freeze({ snapshot, publish });
}

export const dockState = createDockState();
