import { signal } from '@preact/signals';
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
  let settleSandboxDiff = null;
  const templateManagerCloseCallbacks = new Set();

  function confirmSandboxDiff(before, after, notices = []) {
    cancelSandboxDiff(false);
    return new Promise((resolve) => {
      settleSandboxDiff = resolve;
      sandboxDiff.value = { before, after, notices };
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
    busy, error, sandboxDiff, confirmSandboxDiff, cancelSandboxDiff,
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
