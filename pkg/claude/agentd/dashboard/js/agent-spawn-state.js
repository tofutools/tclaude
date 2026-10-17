import { signal } from '@preact/signals';
import { remoteSpawnContext } from './agent-spawn-remote.js';

export function createAgentSpawnState({ getSnapshot = () => null } = {}) {
  const dialog = signal(null);
  let generation = 0;

  function open(options = {}) {
    // A remote spawn (agent-spawn-remote.js) opens on the peer's own groups
    // and harnesses, never this node's.
    const remote = options?.remote || null;
    const snapshot = (remote ? remoteSpawnContext(remote.snapshot, remote.options) : getSnapshot()) || {};
    const currentGeneration = ++generation;
    dialog.value = Object.freeze({
      remote,
      key: `agent-spawn:${currentGeneration}`,
      generation: currentGeneration,
      options: Object.freeze({
        groupName: String(options?.groupName || ''),
        defaultGroup: String(options?.defaultGroup || ''),
        profileName: String(options?.profileName || ''),
        fastMode: options?.fastMode === true,
        role: String(options?.role || ''),
        // A queued spawn reopened for edit (or launch, with autoSpawn): its id,
        // stored draft and queue time. Null for an ordinary fresh dialog.
        queued: options?.queued?.id ? Object.freeze({
          id: options.queued.id,
          draft: Object.freeze({ ...(options.queued.draft || {}) }),
          createdAt: String(options.queued.created_at || ''),
        }) : null,
        autoSpawn: options?.autoSpawn === true && !!options?.queued?.id,
      }),
      groups: Object.freeze([...(snapshot.groups || [])]),
      harnesses: Object.freeze([...(snapshot.harnesses || [])]),
      roles: Object.freeze([...(snapshot.roles || [])]),
      // Host-wide sandbox-implementation catalog. Frozen at open like the rest,
      // so the dialog reasons about one consistent snapshot.
      sandboxImpl: Object.freeze({ ...(snapshot.sandbox_impl || {}) }),
      sandboxProfiles: Object.freeze([...(snapshot.sandbox_profiles || [])]),
      userDefaultModel: String(snapshot.user_default_model || ''),
      // Freeze the console target with the rest of the launch context. If the
      // operator changes the preference while this dialog is open, this spawn
      // should still execute the choice the form was opened under.
      defaultTerminal: snapshot.default_terminal === 'web' ? 'web' : 'native',
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
