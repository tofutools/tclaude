import { computed, signal } from '@preact/signals';

export function createActionDialogState() {
  const dialog = signal(null);
  const view = computed(() => ({ dialog: dialog.value }));
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
    close() { dialog.value = null; },
  });
}
