import { signal, computed } from '@preact/signals';
import { createRequestLifecycle } from './request-lifecycle.js';

export function createManagementState() {
  const manager = signal('');
  const dialog = signal(null);
  const profileFilter = signal('');
  const roleFilter = signal('');
  const sandboxFilter = signal('');
  const profiles = signal([]); const roles = signal([]); const sandboxProfiles = signal([]);
  const profilesRequest = createRequestLifecycle({ payload: profiles, retainPayloadOnRefresh: true, retainPayloadOnError: true });
  const rolesRequest = createRequestLifecycle({ payload: roles, retainPayloadOnRefresh: true, retainPayloadOnError: true });
  const sandboxRequest = createRequestLifecycle({ payload: sandboxProfiles, retainPayloadOnRefresh: true, retainPayloadOnError: true });
  const busy = signal('');
  const error = signal('');
  const sandboxDiff = signal(null);
  let settleSandboxDiff = null;

  const view = computed(() => ({
    manager: manager.value, dialog: dialog.value,
    profileFilter: profileFilter.value, roleFilter: roleFilter.value, sandboxFilter: sandboxFilter.value,
    profiles: profiles.value || [], roles: roles.value || [], sandboxProfiles: sandboxProfiles.value || [],
    requests: { profiles: profilesRequest.request.value, roles: rolesRequest.request.value, sandbox: sandboxRequest.request.value },
    busy: busy.value, error: error.value, sandboxDiff: sandboxDiff.value,
  }));

  function confirmSandboxDiff(before, after) {
    cancelSandboxDiff(false);
    return new Promise((resolve) => {
      settleSandboxDiff = resolve;
      sandboxDiff.value = { before, after };
    });
  }

  function cancelSandboxDiff(result = false) {
    const resolve = settleSandboxDiff;
    settleSandboxDiff = null;
    sandboxDiff.value = null;
    resolve?.(result);
  }

  return Object.freeze({
    manager, dialog, profileFilter, roleFilter, sandboxFilter, profiles, roles, sandboxProfiles, profilesRequest, rolesRequest, sandboxRequest,
    busy, error, sandboxDiff, view, confirmSandboxDiff, cancelSandboxDiff,
    openManager(kind) { error.value = ''; manager.value = kind; },
    closeManager() { manager.value = ''; },
    openDialog(value) { error.value = ''; dialog.value = value; },
    closeDialog() { cancelSandboxDiff(false); dialog.value = null; error.value = ''; },
  });
}
