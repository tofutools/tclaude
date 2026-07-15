import { computed, signal } from '@preact/signals';

function normalizeTarget(prefill = {}) {
  const mode = prefill.targetMode === 'group' ? 'group' : 'solo';
  return {
    mode,
    target: mode === 'solo' ? String(prefill.target || '') : '',
    groupName: mode === 'group' ? String(prefill.groupName || '') : '',
    scopeGroup: String(prefill.scopeGroup || ''),
  };
}

export function createMessageAccessDialogState() {
  const dialog = signal(null);
  const picker = signal(null);
  const cronTarget = signal(normalizeTarget());
  const view = computed(() => ({
    dialog: dialog.value,
    picker: picker.value,
    cronTarget: cronTarget.value,
  }));
  let pickerResolve = null;
  let cronModeListener = null;
  let nextLaunchID = 0;

  function open(descriptor) {
    // Repeated/programmatic launchers must not clobber a draft or retarget its
    // eventual mutation. The existing dialog keeps ownership until closed.
    if (dialog.value) return false;
    dialog.value = { ...descriptor, launchID: ++nextLaunchID };
    return true;
  }

  function close() {
    finishPicker('');
    dialog.value = null;
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

  function configureCronTarget(prefill = {}) {
    const next = normalizeTarget(prefill);
    cronTarget.value = next;
    cronModeListener?.(next.mode);
  }

  function setCronTargetMode(mode) {
    const normalized = mode === 'group' ? 'group' : 'solo';
    if (cronTarget.value.mode === normalized) return;
    cronTarget.value = { ...cronTarget.value, mode: normalized };
    cronModeListener?.(normalized);
  }

  function setCronTargetValue(patch) {
    cronTarget.value = { ...cronTarget.value, ...patch };
  }

  function readCronTarget() {
    const current = cronTarget.value;
    if (current.mode === 'group') {
      return { mode: 'group', target: current.groupName ? `group:${current.groupName}` : '' };
    }
    return { mode: 'solo', target: current.target.trim() };
  }

  function setCronTargetModeListener(listener) {
    cronModeListener = typeof listener === 'function' ? listener : null;
    cronModeListener?.(cronTarget.value.mode);
    return () => { if (cronModeListener === listener) cronModeListener = null; };
  }

  function dispose() {
    close();
    finishPicker('');
    cronModeListener = null;
  }

  return Object.freeze({
    dialog, picker, cronTarget, view,
    openMessage(prefill = {}) {
      return open({ kind: 'message', prefill: { ...prefill } });
    },
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
    close, pickAgent, finishPicker,
    configureCronTarget, setCronTargetMode, setCronTargetValue,
    readCronTarget, setCronTargetModeListener, dispose,
  });
}
