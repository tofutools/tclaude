import { computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { createRequestLifecycle } from './request-lifecycle.js';
import { processScribeSessions } from './process-scribe.js';

export function createProcessesState({ activeTab = dashboardState.activeTab } = {}) {
  const subtab = signal('templates');
  const canvas = signal(null);
  const notice = signal('');
  const templates = signal(null);
  const rename = signal(null);
  const create = signal(null);
  const mutation = signal({ busy: false, error: null });
  let editor = null;

  const templatesRequest = createRequestLifecycle({
    payload: templates,
    retainPayloadOnRefresh: true,
    retainPayloadOnError: true,
  });

  function setSubtab(value) { if (value === 'templates') subtab.value = value; }
  function setCanvas(value) { canvas.value = value; }
  function setNotice(value) { notice.value = String(value || ''); }
  function beginMutation() {
    if (mutation.value.busy) return false;
    mutation.value = { busy: true, error: null };
    return true;
  }
  function endMutation(error = null) { mutation.value = { busy: false, error }; }
  function setEditor(value) { editor = value; }
  function currentEditor() { return editor; }
  function setRename(value) { rename.value = value; }
  function setCreate(value) { create.value = value; }

  const navLocation = computed(() => {
    const loc = { tab: 'processes', subtab: 'templates' };
    const open = canvas.value;
    if (open?.kind === 'editor' && !open.blank && open.id) loc.selection = String(open.id);
    return loc;
  });

  const view = computed(() => ({
    active: activeTab.value === 'processes',
    subtab: 'templates',
    canvas: canvas.value,
    notice: notice.value,
    scribes: processScribeSessions(dashboardState.snapshot.value),
    templates: templates.value?.templates || [],
    rename: rename.value,
    create: create.value,
    mutation: mutation.value,
    requests: { templates: templatesRequest.request.value },
  }));

  return Object.freeze({
    subtab, canvas, notice, templates, rename, create, mutation, view, location: navLocation,
    templatesRequest, setSubtab, setCanvas, setNotice, beginMutation, endMutation,
    setEditor, currentEditor, setRename, setCreate,
  });
}

export const processesState = createProcessesState();
