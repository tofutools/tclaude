import { signal } from '@preact/signals';

export function createAgentSpawnState({ getSnapshot = () => null } = {}) {
  const dialog = signal(null);
  let generation = 0;

  function open(options = {}) {
    const snapshot = getSnapshot() || {};
    const currentGeneration = ++generation;
    dialog.value = Object.freeze({
      key: `agent-spawn:${currentGeneration}`,
      generation: currentGeneration,
      options: Object.freeze({
        groupName: String(options?.groupName || ''),
        defaultGroup: String(options?.defaultGroup || ''),
        profileName: String(options?.profileName || ''),
        role: String(options?.role || ''),
      }),
      groups: Object.freeze([...(snapshot.groups || [])]),
      harnesses: Object.freeze([...(snapshot.harnesses || [])]),
      sandboxProfiles: Object.freeze([...(snapshot.sandbox_profiles || [])]),
      userDefaultModel: String(snapshot.user_default_model || ''),
      normalizeNames: snapshot.spawn_name_normalize !== false,
      sandboxRevision: 0,
    });
  }

  function close() {
    generation += 1;
    dialog.value = null;
  }

  function refreshSandboxPolicy() {
    if (!dialog.value) return;
    dialog.value = Object.freeze({
      ...dialog.value,
      sandboxRevision: dialog.value.sandboxRevision + 1,
    });
  }

  return Object.freeze({
    dialog,
    open,
    close,
    refreshSandboxPolicy,
    dispose: close,
    isCurrent(value) {
      return !!dialog.value && dialog.value.generation === value;
    },
  });
}
