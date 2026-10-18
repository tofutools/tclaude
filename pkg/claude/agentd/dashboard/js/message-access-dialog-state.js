import { computed, signal } from '@preact/signals';

export function createMessageAccessDialogState({ canRestoreFocus = () => true } = {}) {
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
        if (!dialog.value && canRestoreFocus()) closed.restoreFocus();
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
    openMessage(prefill = {}, node = '', nodeName = '') {
      return open({ kind: 'message', prefill: { ...prefill }, node: String(node || ''), nodeName: String(nodeName || '') });
    },
    openOperatorMessage(context = {}) {
      const allLive = !!context.allLive;
      if (!allLive && !context.agent) return false;
      return open({
        kind: 'operator-message',
        agent: allLive ? '' : String(context.agent),
        label: String(context.label || (allLive ? 'all live agents' : context.agent)),
        allLive,
        restoreFocus: typeof context.restoreFocus === 'function' ? context.restoreFocus : null,
      });
    },
    dialogKind() { return dialog.value?.kind || ''; },
    openHumanReply(context = {}) {
      return open({ kind: 'human-reply', context: { ...context } });
    },
    openSudoGrant({ agentID = '', node = '' } = {}) {
      return open({ kind: 'sudo-grant', agentID: String(agentID || ''), node: String(node || '') });
    },
    openAgentPermissions({ conv, label = '', node = '' }) {
      return open({ kind: 'permissions', mode: 'agent', conv, label, node: String(node || '') });
    },
    openGroupPermissions({ group, grants = [], scopes = {}, unreadable = [], ownerScopes = {}, node = '' }) {
      return open({ kind: 'permissions', mode: 'group', group, grants: [...grants], scopes: { ...scopes }, unreadable: [...unreadable], ownerScopes: { ...ownerScopes }, node: String(node || '') });
    },
    openBufferedPermissions(options = {}) {
      return open({ kind: 'permissions', ...options, mode: 'buffer', overrides: { ...(options.overrides || {}) } });
    },
    // Startup-context trims (TCL-597). Always buffered: the dialog edits a draft
    // the spawn form or profile editor owns and hands back on save, never a live
    // agent — trimming a running agent's startup context is meaningless, since
    // that context was loaded at launch. The catalog rides the descriptor because
    // it is harness-specific and the caller is the one that knows which harness
    // the draft selected.
    openContextFeatures(options = {}) {
      return open({
        kind: 'context-features',
        ...options,
        catalog: [...(options.catalog || [])],
        selection: { ...(options.selection || {}) },
      });
    },
    close, pickAgent, finishPicker, dispose,
  });
}
