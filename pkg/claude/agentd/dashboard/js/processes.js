// processes.js — feature-gated Processes tab shell and REST-backed lists.
// The graph editor and live viewer own later tickets; this module leaves
// stable full-canvas mount points for them and keeps actions as stubs. The
// Worklist sub-view lives in process-worklist.js (TCL-297).

import { $, $$, isModifiedClick, esc, relTime } from './helpers.js';
import { morphInto } from './morph.js';
import { initProcessWorklist, loadProcessWorklist } from './process-worklist.js';

let activeProcessSubtab = 'templates';

export function initProcessesTab() {
  const tab = $('#tab-processes');
  if (!tab) return;
  document.querySelector('nav [data-tab="processes"]')?.addEventListener('click', () => loadProcessSubtab(activeProcessSubtab));
  tab.querySelector('.process-subnav')?.addEventListener('click', async e => {
    const button = e.target.closest('[data-process-subtab]');
    if (!button) return;
    // Real <a href> subtab links: a modified/middle click opens the location in
    // a new tab (leaving this view — and any dirty editor — untouched). A plain
    // click switches in place. preventDefault runs synchronously, BEFORE the
    // async dirty-editor confirm, so the anchor's own navigation is cancelled
    // regardless of the confirm's outcome. See isModifiedClick / bindTabs.
    if (isModifiedClick(e)) return;
    e.preventDefault();
    if (!(await confirmLeaveDirtyEditor())) return;
    activateProcessSubtab(button.dataset.processSubtab);
  });
  $('#process-runs-refresh')?.addEventListener('click', () => loadProcessRuns());
  $('#process-template-new')?.addEventListener('click', () => openProcessEditor('new-process', true));
  tab.addEventListener('click', async e => {
    const close = e.target.closest('[data-process-close-view]');
    if (close) {
      // Awaited with a swallow so a rejection (e.g. a failed dynamic import
      // inside the dirty check) never becomes an unhandled rejection. Failing
      // SAFE means staying open: closing here would be a silent discard.
      await maybeCloseProcessCanvasViews().catch(() => {});
      return;
    }
    const action = e.target.closest('button[data-process-action]');
    if (!action) return;
    const id = action.dataset.id || '';
    switch (action.dataset.processAction) {
      case 'edit': openProcessEditor(id, false); break;
      case 'instantiate': processNotice(`Run creation for ${id} lands in a later ticket.`); break;
      case 'view': openProcessViewer(id); break;
    }
  });
  initProcessWorklist();
}

export function applyProcessesTabVisibility(data) {
  const visible = !!(data && data.processes_enabled);
  document.body.classList.toggle('hide-processes', !visible);
  if (!visible) {
    const section = $('#tab-processes');
    if (section?.classList.contains('active')) {
      $$('nav [data-tab]').forEach(button => button.classList.toggle('active', button.dataset.tab === 'groups'));
      $$('main section').forEach(panel => panel.classList.toggle('active', panel.id === 'tab-groups'));
    }
  }
}

export function activateProcessSubtab(name) {
  activeProcessSubtab = name;
  closeProcessCanvasViews();
  $$('.process-subtab').forEach(button => {
    const active = button.dataset.processSubtab === name;
    button.classList.toggle('active', active);
    button.setAttribute('aria-selected', active ? 'true' : 'false');
  });
  $$('.process-panel').forEach(panel => panel.classList.toggle('active', panel.id === `process-panel-${name}`));
  loadProcessSubtab(name);
  // Tell the history router the location changed (→ /processes/<sub>). One-way
  // event so processes.js doesn't import nav-history.js; recorded as user
  // navigation (no-op during the router's own programmatic restore).
  document.dispatchEvent(new CustomEvent('tclaude:navigated'));
}

function loadProcessSubtab(name) {
  if (name === 'templates') loadProcessTemplates();
  if (name === 'runs') loadProcessRuns();
  if (name === 'worklist') loadProcessWorklist();
}

export async function processJSON(path) {
  const response = await fetch(path);
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
  return body;
}

async function loadProcessTemplates() {
  const mount = $('#process-templates-list');
  if (!mount) return;
  try {
    const body = await processJSON('/v1/process/templates');
    morphInto(mount, renderProcessTemplates(body.templates || []));
    processNotice(`${(body.templates || []).length} template${(body.templates || []).length === 1 ? '' : 's'}`);
  } catch (error) {
    morphInto(mount, `<p class="error">Could not load templates: ${esc(error.message)}</p>`);
  }
}

function renderProcessTemplates(templates) {
  if (!templates.length) return '<div class="process-placeholder"><h3>No process templates yet</h3><p>Create a blank template to start shaping a repeatable graph.</p></div>';
  const rows = templates.map(template => {
    const latest = template.latestVersion || {};
    const hash = (latest.semanticHash || '').slice(0, 10);
    return `<tr data-process-template="${esc(template.id)}">
      <td><strong>${esc(template.name || template.id)}</strong><div class="process-secondary">${esc(template.id)}</div></td>
      <td class="process-description">${esc(template.description || '—')}</td>
      <td><span class="process-hash" title="${esc(latest.semanticHash || '')}">${esc(hash || '—')}</span></td>
      <td>${esc(template.versionCount || 0)}</td>
      <td class="process-actions"><button class="process-action" data-process-action="edit" data-id="${esc(template.id)}" type="button">open</button><button class="process-action" data-process-action="instantiate" data-id="${esc(template.id)}" type="button">instantiate</button></td>
    </tr>`;
  }).join('');
  return `<table><thead><tr><th>Template</th><th>Description</th><th>Latest</th><th>Versions</th><th></th></tr></thead><tbody>${rows}</tbody></table>`;
}

async function loadProcessRuns() {
  const mount = $('#process-runs-list');
  if (!mount) return;
  try {
    const body = await processJSON('/v1/process/runs');
    morphInto(mount, renderProcessRuns(body.runs || []));
    processNotice(`${(body.runs || []).length} run${(body.runs || []).length === 1 ? '' : 's'}`);
  } catch (error) {
    morphInto(mount, `<p class="error">Could not load runs: ${esc(error.message)}</p>`);
  }
}

function renderProcessRuns(runs) {
  if (!runs.length) return '<div class="process-placeholder"><h3>No process runs yet</h3><p>Instantiate a template to create a durable run.</p></div>';
  const rows = runs.map(run => `<tr data-process-run="${esc(run.id)}">
    <td><strong>${esc(run.id)}</strong></td>
    <td><span class="process-hash" title="${esc(run.templateRef || '')}">${esc(shortProcessRef(run.templateRef) || '—')}</span></td>
    <td><span class="process-status">${esc(run.status || 'unknown')}</span></td>
    <td>${run.started ? esc(relTime(run.started)) : '—'}</td>
    <td>${esc(run.currentActivity || '—')}</td>
    <td class="process-actions"><button class="process-action" data-process-action="view" data-id="${esc(run.id)}" type="button">open</button></td>
  </tr>`).join('');
  return `<table><thead><tr><th>Run</th><th>Template</th><th>Status</th><th>Started</th><th>Current activity</th><th></th></tr></thead><tbody>${rows}</tbody></table>`;
}

function shortProcessRef(ref) {
  if (!ref) return '';
  const marker = '@sha256:';
  const at = ref.indexOf(marker);
  if (at < 0) return ref;
  return `${ref.slice(0, at)}${marker}${ref.slice(at + marker.length, at + marker.length + 10)}`;
}

async function openProcessEditor(id, blank) {
  $$('.process-panel').forEach(panel => panel.classList.remove('active'));
  $('#process-viewer-view').hidden = true;
  $('#process-editor-view').hidden = false;
  const mount = $('#process-editor-canvas');
  processNotice(blank ? 'Blank template scaffold ready.' : `Opening ${id}.`);
  try {
    // Dynamic import keeps the graph/editor modules out of the flag-off page
    // load; this only runs after the feature-gated tab is open.
    const { openTemplateEditor } = await import('./process-editor.js');
    await openTemplateEditor(mount, { id, blank });
  } catch (error) {
    mount.replaceChildren();
    mount.insertAdjacentHTML('beforeend', `<p class="error">Could not open editor: ${esc(error.message)}</p>`);
  }
}

function openProcessViewer(id) {
  $$('.process-panel').forEach(panel => panel.classList.remove('active'));
  $('#process-editor-view').hidden = true;
  $('#process-viewer-view').hidden = false;
  $('#process-viewer-canvas').innerHTML = `<h3>Run: ${esc(id)}</h3><p>Live viewer mount point. The viewer ticket takes over this canvas.</p>`;
  processNotice(`Opening run ${id}.`);
}

function closeProcessCanvasViews() {
  const editor = $('#process-editor-canvas').__processEditor;
  editor?.destroy?.();
  $('#process-editor-view').hidden = true;
  $('#process-viewer-view').hidden = true;
  $$('.process-panel').forEach(panel => panel.classList.toggle('active', panel.id === `process-panel-${activeProcessSubtab}`));
  if (activeProcessSubtab === 'templates') loadProcessTemplates();
}

// Leaving the editor with unsaved edits needs an explicit confirmation —
// closing/switching views is a navigation gesture, never a silent discard.
async function confirmLeaveDirtyEditor() {
  const editor = $('#process-editor-canvas').__processEditor;
  if (!editor?.model?.dirty) return true;
  const { confirmDiscard } = await import('./refresh.js');
  return confirmDiscard();
}

async function maybeCloseProcessCanvasViews() {
  if (!(await confirmLeaveDirtyEditor())) return;
  closeProcessCanvasViews();
}

export function processNotice(message) {
  const notice = $('#process-notice');
  if (notice) notice.textContent = message;
}
