import { computed, signal } from '@preact/signals';

function snapshotDescriptor(value) {
  if (Array.isArray(value)) return Object.freeze(value.map(snapshotDescriptor));
  if (!value || typeof value !== 'object') return value;
  const prototype = Object.getPrototypeOf(value);
  if (prototype !== Object.prototype && prototype !== null) return value;
  return Object.freeze(Object.fromEntries(
    Object.entries(value).map(([key, item]) => [key, snapshotDescriptor(item)]),
  ));
}

export function createActionDialogState() {
  const dialog = signal(null);
  const view = computed(() => ({ dialog: dialog.value }));
  let nextLaunchID = 0;
  let choice = null;

  function open(descriptor) {
    // The live descriptor is the sole owner across every action-dialog family.
    // Refusing collisions preserves component-local drafts and keeps promise-
    // backed prompts attached to the caller which opened them.
    if (dialog.value || choice) return null;
    const owner = snapshotDescriptor({ ...descriptor, launchID: ++nextLaunchID });
    dialog.value = owner;
    return owner;
  }

  function openChoice(descriptor) {
    const owner = open(descriptor);
    if (!owner) return Promise.resolve(null);
    return new Promise((resolve) => { choice = { owner, resolve }; });
  }

  function finishChoice(owner, result) {
    if (!choice || choice.owner !== owner || dialog.value !== owner) return false;
    const { resolve } = choice;
    choice = null;
    dialog.value = null;
    resolve(result);
    return true;
  }

  function close(owner) {
    // Calls without an owner are reserved for lifecycle cleanup and explicit
    // controller-level cancellation. Consumers pass their captured descriptor,
    // making a late async completion harmless after sequential reuse.
    const expected = arguments.length ? owner : dialog.value;
    if (!expected || dialog.value !== expected) return false;
    if (choice?.owner === expected) return finishChoice(expected, null);
    dialog.value = null;
    return true;
  }

  return Object.freeze({
    dialog,
    view,
    openClone({ conv, label = '', cwd = '' }) {
      return !!open({ kind: 'clone-agent', conv, label, cwd: cwd || '' });
    },
    openReincarnate({ conv, label = '' }) {
      return !!open({ kind: 'reincarnate-agent', conv, label });
    },
    openNest({ group }) {
      return !!open({ kind: 'nest-group', group });
    },
    openTaskLink({ conv, agentLabel = '', url = '', taskLabel = '' }) {
      return !!open({ kind: 'task-link', conv, agentLabel, url, taskLabel });
    },
    openPresetClone({ kind, kindWizard, source, create }) {
      return !!open({ kind: 'preset-clone', presetKind: kind, kindWizard, source, create });
    },
    openExport({ conv, label = '' }) {
      return !!open({ kind: 'agent-export', conv, label });
    },
    openTerminalDirectory({ label = '' }) {
      return openChoice({ kind: 'terminal-directory', label });
    },
    finishChoice,
    close,
    dispose() { close(); },
  });
}
