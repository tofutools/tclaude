import { computed, signal } from '@preact/signals';

export function createMessageAccessDialogState() {
  const dialog = signal(null);
  const picker = signal(null);
  const view = computed(() => ({
    dialog: dialog.value,
    picker: picker.value,
  }));
  let pickerResolve = null;
  let nextLaunchID = 0;

  function open(descriptor) {
    // Repeated/programmatic launchers must not clobber a draft or retarget its
    // eventual mutation. The existing dialog keeps ownership until closed.
    if (dialog.value) return false;
    dialog.value = { ...descriptor, launchID: ++nextLaunchID };
    return true;
  }

  function close() {
    const closed = dialog.value;
    finishPicker('');
    dialog.value = null;
    if (closed?.kind === 'operator-message' && closed.restoreFocus) {
      setTimeout(() => {
        if (!dialog.value) closed.restoreFocus();
      }, 0);
    }
  }

  function pickAgent(options = {}) {
    if (picker.value || pickerResolve) return Promise.resolve('');
    picker.value = {
      kind: 'agent-picker',
      launchID: ++nextLaunchID,
      title: options.title || 'Pick target',
      identity: options.identity === 'conv' ? 'conv' : 'agent',
      includeOfflineHint: options.includeOfflineHint || '',
      showSudo: !!options.showSudo,
    };
    return new Promise((resolve) => { pickerResolve = resolve; });
  }

  function finishPicker(value = '') {
    const resolve = pickerResolve;
    pickerResolve = null;
    picker.value = null;
    resolve?.(value || '');
  }

  function dispose() {
    finishPicker('');
    dialog.value = null;
  }

  return Object.freeze({
    dialog, picker, view,
    openMessage(prefill = {}) {
      return open({ kind: 'message', prefill: { ...prefill } });
    },
    openOperatorMessage(context = {}) {
      if (!context.agent) return false;
      return open({
        kind: 'operator-message',
        agent: String(context.agent),
        label: String(context.label || context.agent),
        restoreFocus: typeof context.restoreFocus === 'function' ? context.restoreFocus : null,
      });
    },
    dialogKind() { return dialog.value?.kind || ''; },
    openHumanReply(context = {}) {
      return open({ kind: 'human-reply', context: { ...context } });
    },
    openSudoGrant({ conv = '' } = {}) {
      return open({ kind: 'sudo-grant', conv: String(conv || '') });
    },
    openAgentPermissions({ conv, label = '' }) {
      return open({ kind: 'permissions', mode: 'agent', conv, label });
    },
    openGroupPermissions({ group, grants = [] }) {
      return open({ kind: 'permissions', mode: 'group', group, grants: [...grants] });
    },
    openBufferedPermissions(options = {}) {
      return open({ kind: 'permissions', ...options, mode: 'buffer', overrides: { ...(options.overrides || {}) } });
    },
    close, pickAgent, finishPicker, dispose,
  });
}
