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
    openReincarnate({ conv, label = '', node = '' }) {
      return !!open({ kind: 'reincarnate-agent', conv, label, ...(node ? { node } : {}) });
    },
    openNest({ group, node = '' }) {
      return !!open({ kind: 'nest-group', group, node });
    },
    openTaskLink({ conv, node = '', agentLabel = '', url = '', taskLabel = '' }) {
      return !!open({ kind: 'task-link', conv, ...(node ? { node } : {}), agentLabel, url, taskLabel });
    },
    openGroupAttachment({ group, node = '', url = '', attachmentLabel = '' }) {
      return !!open({ kind: 'group-attachment', group, node, url, attachmentLabel });
    },
		openGroupSettings({ group, node = '' }) {
			return !!open({ kind: 'group-settings', group, node });
		},
    openPresetClone({ kind, kindWizard, source, create }) {
      return !!open({ kind: 'preset-clone', presetKind: kind, kindWizard, source, create });
    },
    openExport({ conv, label = '' }) {
      return !!open({ kind: 'agent-export', conv, label });
    },
    // The dialog deliberately carries no current implementation: the row's
    // recorded value comes from the last LAUNCH, while what this dialog edits is
    // durable relaunch intent, and those differ the moment an assignment lands.
    // It loads the authoritative posture itself when it opens.
    openSandboxImpl({ conv, label = '', harness = '' }) {
      return !!open({ kind: 'sandbox-impl', conv, label, harness });
    },
    // launch-target carries the agent and the mode the ⚙ menu picked
    // (launch-target-dialog.js); the dialog loads targets itself.
    openLaunchTarget({ mode = 'move', agent }) {
      return !!open({ kind: 'launch-target', mode, agent });
    },
    openTerminalDirectory({ label = '' }) {
      return openChoice({ kind: 'terminal-directory', label });
    },
    // Resolves to the act of one of the four terminal row actions the ⚙
    // "terminals & windows…" item stands for, or null when cancelled.
    openTerminalChooser({ label = '', cwd = '' }) {
      return openChoice({ kind: 'terminal-chooser', label, cwd });
    },
    finishChoice,
    close,
    dispose() { close(); },
  });
}
