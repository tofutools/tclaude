import { signal } from '@preact/signals';
import { nextGroupCloneName } from './group-create-model.js';
import { nodeSnapshot } from './node-api.js';

export function createGroupCreateState({ getSnapshot = () => null } = {}) {
  const dialog = signal(null);
  let generation = 0;
  // A presentation setting of this node, even for a group on another: a peer's
  // snapshot does not carry it.
  const showAttachment = () => {
    const mode = getSnapshot()?.group_attachments_mode;
    return Boolean(mode) && mode !== 'off';
  };

  // node names another node's group (a subgroup made there, from that
  // node's snapshot); blank is the page's.
  function open(presetTemplate = '', parentGroup = '', node = '') {
    const snapshot = nodeSnapshot(node) || getSnapshot() || {};
    const currentGeneration = ++generation;
    dialog.value = Object.freeze({
      key: `group-create:${currentGeneration}`,
      generation: currentGeneration,
      presetTemplate: String(presetTemplate || ''),
      parentGroup: String(parentGroup || ''),
      ...(node ? { node: String(node) } : {}),
      templates: Object.freeze([...(snapshot.templates || [])]),
      showAttachment: showAttachment(),
      groups: Object.freeze([...(snapshot.groups || [])]),
    });
  }

  // node names another node's group (the fused Groups view): the clone is
  // made there, from that node's snapshot.
  function openClone(groupName, placement = null, node = '') {
    const snapshot = nodeSnapshot(node) || getSnapshot() || {};
    const groups = [...(snapshot.groups || [])];
    const source = groups.find((item) => item.name === groupName) || null;
    const currentGeneration = ++generation;
    dialog.value = Object.freeze({
      key: `group-create:${currentGeneration}`,
      generation: currentGeneration,
      presetTemplate: '',
      parentGroup: '',
      cloneGroup: String(groupName || ''),
      ...(node ? { node: String(node) } : {}),
      defaultName: nextGroupCloneName(groups, groupName),
      placement: placement || {
        parent: source?.parent || '', anchor: groupName, before: false,
      },
      templates: Object.freeze([...(snapshot.templates || [])]),
      showAttachment: showAttachment(),
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
