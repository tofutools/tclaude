import { computed, signal } from '@preact/signals';

export function createActionDialogState() {
  const dialog = signal(null);
  const view = computed(() => ({ dialog: dialog.value }));
  let choiceResolve = null;

  function openChoice(descriptor) {
    // Choice dialogs are promise-backed compatibility seams. Refuse a second
    // open instead of retargeting the live prompt and orphaning its caller.
    if (dialog.value || choiceResolve) return Promise.resolve(null);
    dialog.value = descriptor;
    return new Promise((resolve) => { choiceResolve = resolve; });
  }

  function finishChoice(result) {
    const resolve = choiceResolve;
    choiceResolve = null;
    dialog.value = null;
    resolve?.(result);
  }

  function close() {
    if (choiceResolve) finishChoice(null);
    else dialog.value = null;
  }

  return Object.freeze({
    dialog,
    view,
    openClone({ conv, label = '', cwd = '' }) {
      dialog.value = { kind: 'clone-agent', conv, label, cwd: cwd || '' };
    },
    openReincarnate({ conv, label = '' }) {
      dialog.value = { kind: 'reincarnate-agent', conv, label };
    },
    openNest({ group }) {
      dialog.value = { kind: 'nest-group', group };
    },
    openTaskLink({ conv, agentLabel = '', url = '', taskLabel = '' }) {
      // Single-instance guard: while a task-link dialog is live, a repeated or
      // programmatic open must not replace the in-progress draft or retarget
      // Save at a different agent. Refusing the second open preserves the legacy
      // controller's invariant — the existing dialog keeps its focus containment.
      if (dialog.value?.kind === 'task-link') return;
      dialog.value = { kind: 'task-link', conv, agentLabel, url, taskLabel };
    },
    openPresetClone({ kind, kindWizard, source, create }) {
      if (dialog.value) return false;
      dialog.value = { kind: 'preset-clone', presetKind: kind, kindWizard, source, create };
      return true;
    },
    openExport({ conv, label = '' }) {
      if (dialog.value) return false;
      dialog.value = { kind: 'agent-export', conv, label };
      return true;
    },
    openTerminalDirectory({ label = '' }) {
      return openChoice({ kind: 'terminal-directory', label });
    },
    finishChoice,
    close,
    dispose: close,
  });
}
