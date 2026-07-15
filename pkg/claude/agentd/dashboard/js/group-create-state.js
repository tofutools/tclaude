import { signal } from '@preact/signals';

export function createGroupCreateState({ getSnapshot = () => null } = {}) {
  const dialog = signal(null);
  let generation = 0;

  function open(presetTemplate = '', parentGroup = '') {
    const snapshot = getSnapshot() || {};
    const currentGeneration = ++generation;
    dialog.value = Object.freeze({
      key: `group-create:${currentGeneration}`,
      generation: currentGeneration,
      presetTemplate: String(presetTemplate || ''),
      parentGroup: String(parentGroup || ''),
      templates: Object.freeze([...(snapshot.templates || [])]),
      groups: Object.freeze([...(snapshot.groups || [])]),
    });
  }

  function close() {
    generation += 1;
    dialog.value = null;
  }

  return Object.freeze({
    dialog,
    open,
    close,
    dispose: close,
    isCurrent(value) {
      return !!dialog.value && dialog.value.generation === value;
    },
  });
}
