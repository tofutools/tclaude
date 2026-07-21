import { signal, computed } from '@preact/signals';
import { createRequestLifecycle } from './request-lifecycle.js';
import { dashPrefs } from './prefs.js';

export function createManagementState() {
  const manager = signal('');
  const dialog = signal(null);
  const templateDialog = signal(null);
  const templateManager = signal(false);
  const profileFilter = signal('');
  const roleFilter = signal('');
  const sandboxFilter = signal('');
  const templateFilter = signal(dashPrefs.getItem('tclaude.dash.filter.templates') || '');
  const profiles = signal([]); const roles = signal([]); const sandboxProfiles = signal([]); const templates = signal([]); const templateGroups = signal([]);
  const profilesRequest = createRequestLifecycle({ payload: profiles, retainPayloadOnRefresh: true, retainPayloadOnError: true });
  const rolesRequest = createRequestLifecycle({ payload: roles, retainPayloadOnRefresh: true, retainPayloadOnError: true });
  const sandboxRequest = createRequestLifecycle({ payload: sandboxProfiles, retainPayloadOnRefresh: true, retainPayloadOnError: true });
  const busy = signal('');
  const error = signal('');
  const sandboxDiff = signal(null);
  // Sticky "the cached sandbox-profile registry cannot be trusted" marker,
  // set when a break-glass typed-422 recovery fails to reload the registry.
  // It lives here — NOT in the import dialog — so closing and reopening the
  // dialog cannot discard it while the stale cached registry survives; only
  // a successful authoritative registry reload clears it.
  const sandboxRegistryRecoveryRequired = signal(false);
  let settleSandboxDiff = null;
  const templateManagerCloseCallbacks = new Set();

  const view = computed(() => ({
    manager: manager.value, dialog: dialog.value, templateDialog: templateDialog.value, templateManager: templateManager.value,
    profileFilter: profileFilter.value, roleFilter: roleFilter.value, sandboxFilter: sandboxFilter.value, templateFilter: templateFilter.value,
    profiles: profiles.value || [], roles: roles.value || [], sandboxProfiles: sandboxProfiles.value || [], templates: templates.value || [], templateGroups: templateGroups.value || [],
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
    manager, dialog, templateDialog, templateManager, profileFilter, roleFilter, sandboxFilter, templateFilter, profiles, roles, sandboxProfiles, templates, templateGroups, profilesRequest, rolesRequest, sandboxRequest,
    busy, error, sandboxDiff, sandboxRegistryRecoveryRequired, view, confirmSandboxDiff, cancelSandboxDiff,
    openManager(kind) { error.value = ''; manager.value = kind; },
    closeManager() { manager.value = ''; },
    openDialog(value) { error.value = ''; dialog.value = value; },
    closeDialog() { cancelSandboxDiff(false); dialog.value = null; error.value = ''; },
    openTemplateDialog(value) { error.value = ''; templateDialog.value = value; },
    closeTemplateDialog() { templateDialog.value = null; error.value = ''; },
    openTemplateManager(options = {}) {
      if (typeof options?.onClose === 'function') {
        templateManagerCloseCallbacks.add(options.onClose);
      }
      templateManager.value = true;
    },
    closeTemplateManager() {
      if (!templateManager.value && !templateManagerCloseCallbacks.size) return;
      templateManager.value = false;
      const callbacks = [...templateManagerCloseCallbacks];
      templateManagerCloseCallbacks.clear();
      for (const callback of callbacks) {
        try { callback(); } catch (_) {}
      }
    },
    setTemplateFilter(value) {
      templateFilter.value = value;
      if (value) dashPrefs.setItem('tclaude.dash.filter.templates', value);
      else dashPrefs.removeItem('tclaude.dash.filter.templates');
    },
    updateTemplates(value, groups = []) { templates.value = Array.isArray(value) ? value : []; templateGroups.value = Array.isArray(groups) ? groups : []; },
  });
}
