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
    close() { dialog.value = null; },
  });
}
