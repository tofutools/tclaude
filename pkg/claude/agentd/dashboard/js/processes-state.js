import { computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { dashPrefs } from './prefs.js';
import { createRequestLifecycle } from './request-lifecycle.js';
import { WORKLIST_VIEWS, actionableCount, viewCounts, viewItems } from './process-worklist-core.js';
import { processScribeSessions } from './process-scribe.js';

const VIEW_PREF_KEY = 'tclaude.dash.worklist.view';

export function createProcessesState({ activeTab = dashboardState.activeTab, prefs = dashPrefs, now = () => Date.now() } = {}) {
  const subtab = signal('templates');
  const canvas = signal(null);
  const highlightedRun = signal(null);
  const notice = signal('');
  const templates = signal(null);
  const runs = signal(null);
  const worklist = signal(null);
  const instantiation = signal(null);
  const rename = signal(null);
  const worklistView = signal('my-work');
  const drafts = signal({});
  const missingComments = signal(new Set());
  const mutation = signal({ busy: false, error: null });
  let editor = null;

  const templatesRequest = createRequestLifecycle({ payload: templates, retainPayloadOnRefresh: true, retainPayloadOnError: true });
  const runsRequest = createRequestLifecycle({ payload: runs, retainPayloadOnRefresh: true, retainPayloadOnError: true });
  const worklistRequest = createRequestLifecycle({ payload: worklist, retainPayloadOnRefresh: true, retainPayloadOnError: true });

  function initialize() {
    const saved = prefs.getItem(VIEW_PREF_KEY);
    if (saved && WORKLIST_VIEWS.some((view) => view.key === saved)) worklistView.value = saved;
  }
  function setSubtab(value) { if (['templates', 'runs', 'worklist'].includes(value)) subtab.value = value; }
  function setCanvas(value) { canvas.value = value; }
  function setHighlightedRun(value) { highlightedRun.value = value; }
  function setNotice(value) { notice.value = String(value || ''); }
  function setWorklistView(value) {
    if (!WORKLIST_VIEWS.some((view) => view.key === value)) return false;
    worklistView.value = value; prefs.setItem(VIEW_PREF_KEY, value); return true;
  }
  function setDraft(id, value) {
    const next = { ...drafts.value };
    if (value) next[id] = value; else delete next[id];
    drafts.value = next;
    if (missingComments.value.has(id)) {
      const missing = new Set(missingComments.value); missing.delete(id); missingComments.value = missing;
    }
  }
  function requireComment(id) { const next = new Set(missingComments.value); next.add(id); missingComments.value = next; }
  function pruneWorklistState(items) {
    const live = new Set(items.map((item) => item.id));
    const nextDrafts = Object.fromEntries(Object.entries(drafts.value).filter(([id]) => live.has(id)));
    drafts.value = nextDrafts;
    missingComments.value = new Set([...missingComments.value].filter((id) => live.has(id)));
  }
  function beginMutation() { if (mutation.value.busy) return false; mutation.value = { busy: true, error: null }; return true; }
  function endMutation(error = null) { mutation.value = { busy: false, error }; }
  function setEditor(value) { editor = value; }
  function currentEditor() { return editor; }
  function setInstantiation(value) { instantiation.value = value; }
  function setRename(value) { rename.value = value; }

  const view = computed(() => {
    const at = now();
    const work = worklist.value || { items: [], degradedRuns: [] };
    return {
      active: activeTab.value === 'processes', subtab: subtab.value, canvas: canvas.value,
      highlightedRun: highlightedRun.value, notice: notice.value,
      scribes: processScribeSessions(dashboardState.snapshot.value),
      templates: templates.value?.templates || [], runs: runs.value?.runs || [],
      worklist: work, worklistView: worklistView.value,
      instantiation: instantiation.value, rename: rename.value,
      worklistRows: viewItems(work.items || [], worklistView.value, at),
      worklistCounts: viewCounts(work.items || [], at), actionable: actionableCount(work.items || []),
      drafts: drafts.value, missingComments: missingComments.value, mutation: mutation.value,
      requests: { templates: templatesRequest.request.value, runs: runsRequest.request.value, worklist: worklistRequest.request.value },
    };
  });

  initialize();
  return Object.freeze({
    subtab, canvas, highlightedRun, notice, templates, runs, worklist, instantiation, rename, worklistView, drafts, missingComments, mutation, view,
    templatesRequest, runsRequest, worklistRequest, setSubtab, setCanvas, setNotice, setWorklistView,
    setDraft, requireComment, pruneWorklistState, beginMutation, endMutation, setEditor, currentEditor, setInstantiation, setRename, setHighlightedRun,
  });
}

export const processesState = createProcessesState();
