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

  function openClone(groupName, placement = null) {
    const snapshot = getSnapshot() || {};
    const groups = [...(snapshot.groups || [])];
    const source = groups.find((item) => item.name === groupName) || null;
    const match = /^(.*?)-(?:c|clone)-\d+$/.exec(groupName);
    const prefix = `${match ? match[1] : groupName}-c-`;
    const used = new Set(groups
      .filter((item) => item.name?.startsWith(prefix))
      .map((item) => Number.parseInt(item.name.slice(prefix.length), 10))
      .filter(Number.isInteger));
    let suffix = 1;
    while (used.has(suffix)) suffix += 1;
    const currentGeneration = ++generation;
    dialog.value = Object.freeze({
      key: `group-create:${currentGeneration}`,
      generation: currentGeneration,
      presetTemplate: '',
      parentGroup: '',
      cloneGroup: String(groupName || ''),
      defaultName: `${prefix}${suffix}`,
      placement: placement || {
        parent: source?.parent || '', anchor: groupName, before: false,
      },
      templates: Object.freeze([...(snapshot.templates || [])]),
      groups: Object.freeze(groups),
    });
  }

  function close() {
    generation += 1;
    dialog.value = null;
  }

  return Object.freeze({
    dialog,
    open,
    openClone,
    close,
    dispose: close,
    isCurrent(value) {
      return !!dialog.value && dialog.value.generation === value;
    },
  });
}
