import { buildWorklistAction, isDestructiveAction, retainedActionKey } from './process-worklist-core.js';

export async function processJSON(path, fetchImpl = fetch) {
  const response = await fetchImpl(path, { credentials: 'same-origin' });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
  return body;
}

export function createProcessesActions({
  state,
  fetchImpl = fetch,
  confirm = async () => true,
  confirmDiscard = async () => true,
  notify = () => {},
  dispatchNavigated = () => document.dispatchEvent(new CustomEvent('tclaude:navigated')),
} = {}) {
  const actionKeys = new Map();

  async function load(name, { quiet = false } = {}) {
    const lifecycle = ({ templates: state.templatesRequest, runs: state.runsRequest, worklist: state.worklistRequest })[name];
    const path = ({ templates: '/v1/process/templates', runs: '/v1/process/runs', worklist: '/v1/process/worklist' })[name];
    if (!lifecycle || !path) return false;
    const token = lifecycle.beginRequest();
    try {
      const body = await processJSON(path, fetchImpl);
      if (!lifecycle.commitRequest(token, body)) return false;
      if (name === 'worklist') {
        const items = body.items || [];
        state.pruneWorklistState(items);
        const live = new Set(items.map((item) => item.id));
        for (const payload of actionKeys.keys()) {
          if (!live.has(payload.slice(0, payload.indexOf('\x00')))) actionKeys.delete(payload);
        }
        if (!quiet) state.setNotice(`${state.view.value.actionable} actionable item${state.view.value.actionable === 1 ? '' : 's'}`);
        if (!items.length && state.runs.value === null) void load('runs', { quiet: true });
      } else if (!quiet) {
        const rows = body[name] || [];
        state.setNotice(`${rows.length} ${name === 'templates' ? `template${rows.length === 1 ? '' : 's'}` : `run${rows.length === 1 ? '' : 's'}`}`);
      }
      return true;
    } catch (error) {
      if (!lifecycle.failRequest(token, error)) return false;
      if (!quiet) state.setNotice(`${name} failed: ${error.message}`);
      return false;
    }
  }

  async function canLeaveEditor() {
    const editor = state.currentEditor();
    return !editor?.model?.dirty || confirmDiscard();
  }
  async function activateSubtab(name, { navigate = true } = {}) {
    if (!(await canLeaveEditor())) return false;
    state.setEditor(null); state.setCanvas(null); state.setSubtab(name);
    await load(name);
    if (navigate) dispatchNavigated();
    return true;
  }
  async function openEditor(id, blank = false) {
    state.setCanvas({ kind: 'editor', id, blank, key: `${id}:${blank}:${Date.now()}` });
    state.setNotice(blank ? 'Blank template scaffold ready.' : `Opening ${id}.`);
  }
  function openViewer(id) { state.setCanvas({ kind: 'viewer', id, key: id }); state.setNotice(`Opening run ${id}.`); }
  async function closeCanvas() {
    if (!(await canLeaveEditor())) return false;
    state.setEditor(null); state.setCanvas(null); await load(state.subtab.value); return true;
  }
  async function openRunInList(id) {
    if (!(await activateSubtab('runs'))) return;
    state.setHighlightedRun(id);
  }

  async function submitWorklistAction(itemID, action) {
    const item = state.worklist.value?.items?.find((candidate) => candidate.id === itemID);
    const comment = (state.drafts.value[itemID] || '').trim();
    if (!item) return false;
    if (!comment) { state.requireComment(itemID); state.setNotice('A comment is required for every worklist action.'); return false; }
    if (!state.beginMutation()) return false;
    try {
      if (isDestructiveAction(action)) {
        const ok = await confirm({
          title: `${action} — are you sure?`,
          body: `“${action}” on ${item.node} (run ${item.run}) is recorded durably in the run's audit log and drives the run forward.`,
          meta: item.summary || '', okLabel: action,
        });
        if (!ok) return false;
      }
      const { payload, key } = retainedActionKey(actionKeys, item, action, comment);
      const request = buildWorklistAction(item, action, comment, key);
      if (!request) { state.setNotice(`“${action}” is no longer available for this item.`); return false; }
      const response = await fetchImpl(request.path, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin',
        body: JSON.stringify(request.body),
      });
      const body = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
      actionKeys.delete(payload); state.setDraft(itemID, ''); notify(`${request.body.action} recorded for ${item.node}`);
      return true;
    } catch (error) {
      notify(`worklist action failed: ${error.message}`, true); return false;
    } finally {
      state.endMutation();
      void load('worklist');
    }
  }

  function refreshActive() { return load(state.subtab.value); }
  return Object.freeze({ load, activateSubtab, openEditor, openViewer, closeCanvas, openRunInList, submitWorklistAction, refreshActive });
}
