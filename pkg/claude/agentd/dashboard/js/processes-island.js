import { h, render, Fragment } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { isModifiedClick, relTime } from './helpers.js';
import { WORKLIST_VIEWS, actorLabel, dueBucket, fmtAge, fmtDue, groupWaitingOn, isActionable, kindMeta, nudgeLine } from './process-worklist-core.js';

const html = htm.bind(h);
const WORKLIST_TITLES = {
  'my-work': 'Pending items assigned to you (the operator)',
  'waiting-on': 'Everything pending, grouped by whom it waits on',
  due: 'Pending items with a deadline inside 24h or already past it',
  blocked: 'Nodes that exhausted their budget and wait on a retry / skip / cancel decision',
  decision: 'Decision nodes waiting on a human verdict',
  review: 'Gate-stage items waiting on a human review',
  recent: 'Items created or resolved within the last 24h',
};

function RequestBody({ request, label, retry, children }) {
  if (request.phase === 'loading' || request.phase === 'idle') return html`<p class="muted">Loading ${label}…</p>`;
  if (request.phase === 'error' && !children) return html`<div role="alert" class="island-error">Could not load ${label}: ${request.error} <button onClick=${retry}>retry</button></div>`;
  return html`<${Fragment}>${request.phase === 'error' && html`<div role="alert" class="island-error">Refresh failed: ${request.error} <button onClick=${retry}>retry</button></div>`}${children}</${Fragment}>`;
}

function Templates({ current, actions }) {
  const rows = current.templates;
  return html`<div id="process-panel-templates" class="process-panel active" role="tabpanel" aria-label="Process templates">
    <div class="filter-bar process-toolbar"><strong>Reusable process graphs</strong><span class="spacer"></span><button id="process-template-new" class="process-action primary" type="button" onClick=${() => actions.openEditor('new-process', true)}>+ new template</button></div>
    <div id="process-templates-list" class="process-list" aria-busy=${current.requests.templates.phase === 'loading'}>
      <${RequestBody} request=${current.requests.templates} label="templates" retry=${() => actions.load('templates')}>
        ${rows.length === 0 ? html`<div class="process-placeholder"><h3>No process templates yet</h3><p>Create a blank template to start shaping a repeatable graph.</p></div>` : html`<table><thead><tr><th>Template</th><th>Description</th><th>Latest</th><th>Versions</th><th></th></tr></thead><tbody>
          ${rows.map((template) => { const latest = template.latestVersion || {}; const hash = (latest.semanticHash || '').slice(0, 10); return html`<tr key=${template.id} data-process-template=${template.id}><td><strong>${template.name || template.id}</strong><div class="process-secondary">${template.id}</div></td><td class="process-description">${template.description || '—'}</td><td><span class="process-hash" title=${latest.semanticHash || ''}>${hash || '—'}</span></td><td>${template.versionCount || 0}</td><td class="process-actions"><button class="process-action" data-process-action="edit" data-id=${template.id} type="button" onClick=${() => actions.openEditor(template.id)}>open</button><button class="process-action" data-process-action="instantiate" data-id=${template.id} type="button" onClick=${() => current.state.setNotice(`Run creation for ${template.id} lands in a later ticket.`)}>instantiate</button></td></tr>`; })}
        </tbody></table>`}
      </${RequestBody}>
    </div>
  </div>`;
}

function Runs({ current, actions }) {
  const highlighted = current.highlightedRun;
  useEffect(() => {
    if (!highlighted) return;
    const row = document.querySelector(`[data-process-run="${CSS.escape(highlighted)}"]`);
    if (!row) { current.state.setNotice(`Run ${highlighted} is not in the runs list.`); return; }
    row.scrollIntoView({ block: 'center', behavior: 'smooth' }); row.classList.add('wl-run-flash');
    current.state.setHighlightedRun(null);
  }, [highlighted, current.runs]);
  return html`<div id="process-panel-runs" class="process-panel active" role="tabpanel" aria-label="Process runs">
    <div class="filter-bar process-toolbar"><strong>Instantiated runs</strong><span class="spacer"></span><button id="process-runs-refresh" class="process-action" type="button" onClick=${() => actions.load('runs')}>↻ refresh</button></div>
    <div id="process-runs-list" class="process-list" aria-busy=${current.requests.runs.phase === 'loading'}><${RequestBody} request=${current.requests.runs} label="runs" retry=${() => actions.load('runs')}>
      ${current.runs.length === 0 ? html`<div class="process-placeholder"><h3>No process runs yet</h3><p>Instantiate a template to create a durable run.</p></div>` : html`<table><thead><tr><th>Run</th><th>Template</th><th>Status</th><th>Started</th><th>Current activity</th><th></th></tr></thead><tbody>${current.runs.map((run) => html`<tr key=${run.id} data-process-run=${run.id}><td><strong>${run.id}</strong></td><td><span class="process-hash" title=${run.templateRef || ''}>${shortProcessRef(run.templateRef) || '—'}</span></td><td><span class="process-status">${run.status || 'unknown'}</span></td><td>${run.started ? relTime(run.started) : '—'}</td><td>${run.currentActivity || '—'}</td><td class="process-actions"><button class="process-action" data-process-action="view" data-id=${run.id} type="button" onClick=${() => actions.openViewer(run.id)}>open</button></td></tr>`)}</tbody></table>`}
    </${RequestBody}></div>
  </div>`;
}

function shortProcessRef(ref) { const marker = '@sha256:'; const at = ref?.indexOf(marker) ?? -1; return at < 0 ? (ref || '') : `${ref.slice(0, at)}${marker}${ref.slice(at + marker.length, at + marker.length + 10)}`; }

function ItemActions({ item, current, actions }) {
  if (item.kind === 'agent-obligation') return html`<span class="process-secondary" title="Agent obligations are reported by the working agent through the run/node report route with a durable evidence ref — they cannot be resolved from this list.">agent reports via evidence</span>`;
  if (!isActionable(item)) return '—';
  const missing = current.missingComments.has(item.id);
  const submit = async (event, action) => { const ok = await actions.submitWorklistAction(item.id, action); if (!ok && current.state.missingComments.value.has(item.id)) event.currentTarget.closest('td')?.querySelector('input')?.focus(); };
  return html`<${Fragment}><input class=${`wl-comment${missing ? ' wl-comment-missing' : ''}`} type="text" data-worklist-comment=${item.id} placeholder="Comment (required)" value=${current.drafts[item.id] || ''} aria-label=${`Comment for ${item.summary || item.id}`} onInput=${(event) => current.state.setDraft(item.id, event.currentTarget.value)} /><div class="wl-action-row">${(item.availableActions || []).map((action) => html`<button key=${action} disabled=${current.mutation.busy} class="process-action wl-action" data-worklist-action=${action} data-worklist-item=${item.id} type="button" onClick=${(event) => submit(event, action)}>${action}</button>`)}</div></${Fragment}>`;
}

function WorkItemRow({ item, current, actions, now }) {
  const meta = kindMeta(item.kind); const bucket = dueBucket(item, now); const nudge = nudgeLine(item.nudge);
  const cls = ['wl-row', bucket ? `wl-${bucket}` : '', item.status !== 'pending' ? 'wl-resolved' : ''].filter(Boolean).join(' ');
  return html`<tr key=${item.id} class=${cls} data-key=${item.id}><td class="wl-kind"><span class="wl-glyph">${meta.glyph}</span> ${meta.label}${item.status !== 'pending' && html` <span class="process-status">${item.status}</span>`}</td><td class="wl-main"><div class="wl-summary">${item.summary || '—'}</div>${nudge && html`<div class=${`wl-nudge process-secondary${item.nudge?.paused ? ' wl-paused' : ''}`}>${nudge}</div>`}</td><td class="wl-where"><button class="wl-link" data-worklist-run=${item.run} type="button" onClick=${() => actions.openRunInList(item.run)}>${item.run}</button><div class="process-secondary"><button class="wl-link wl-link-node" data-worklist-run=${item.run} type="button" onClick=${() => actions.openRunInList(item.run)}>${item.node}</button>${item.attempt > 1 ? ` · attempt ${item.attempt}` : ''}</div></td><td class="wl-assignee">${actorLabel(item.assignee)}</td><td class="wl-age" title=${item.createdAt || 'not recorded'}>${fmtAge(item.createdAt, now)}</td><td class="wl-due" title=${item.dueAt || 'no deadline recorded'}>${fmtDue(item.dueAt, now)}</td><td class="wl-actions"><${ItemActions} item=${item} current=${current} actions=${actions} /></td></tr>`;
}

function Worklist({ current, actions }) {
  const now = Date.now(); const work = current.worklist; const rows = current.worklistRows;
  let tableRows = rows.map((item) => html`<${WorkItemRow} key=${item.id} item=${item} current=${current} actions=${actions} now=${now} />`);
  if (current.worklistView === 'waiting-on') tableRows = groupWaitingOn(rows).flatMap((group) => [html`<tr key=${`who-${group.assignee || 'unassigned'}`} class="wl-group-head"><td colspan="7">Waiting on ${group.label} · ${group.items.length}</td></tr>`, ...group.items.map((item) => html`<${WorkItemRow} key=${item.id} item=${item} current=${current} actions=${actions} now=${now} />`)]);
  const pending = (work.items || []).filter((item) => item.status === 'pending').length;
  const emptyTitle = !work.items?.length && current.runs.length === 0 ? 'No process runs yet' : rows.length ? '' : `Nothing in “${WORKLIST_VIEWS.find((view) => view.key === current.worklistView)?.label || current.worklistView}”`;
  return html`<div id="process-panel-worklist" class="process-panel active" role="tabpanel" aria-label="Process worklist"><div class="filter-bar process-toolbar"><div class="process-worklist-views" role="group" aria-label="Worklist views">${WORKLIST_VIEWS.map((view) => html`<button key=${view.key} class=${`process-view-chip${current.worklistView === view.key ? ' active' : ''}`} data-worklist-view=${view.key} type="button" aria-pressed=${current.worklistView === view.key} title=${WORKLIST_TITLES[view.key]} onClick=${() => current.state.setWorklistView(view.key)}>${view.label}<span class="wl-view-count">${current.worklistCounts[view.key] || ''}</span></button>`)}</div><span class="spacer"></span><button id="process-worklist-refresh" class="process-action" type="button" onClick=${() => actions.load('worklist')}>↻ refresh</button></div>
    <div id="process-worklist-degraded" class="wl-degraded" role="alert" hidden=${!work.degradedRuns?.length}>${work.degradedRuns?.length ? html`<${Fragment}><span class="wl-degraded-glyph">⚠</span> ${work.degradedRuns.length} run${work.degradedRuns.length === 1 ? '' : 's'} could not be read (their work items are missing from this list): ${work.degradedRuns.map((run) => html`<span key=${run.run} class="wl-degraded-run" title=${run.error || ''}>${run.run}</span>`)}</${Fragment}>` : null}</div>
    <div id="process-worklist-list" class="process-list" aria-busy=${current.requests.worklist.phase === 'loading'}><${RequestBody} request=${current.requests.worklist} label="worklist" retry=${() => actions.load('worklist')}>
      ${rows.length ? html`<table><thead><tr><th>Kind</th><th>Work item</th><th>Run / node</th><th>Assignee</th><th>Age</th><th>Due</th><th>Actions</th></tr></thead><tbody>${tableRows}</tbody></table>` : html`<div class="process-placeholder"><h3>${emptyTitle}</h3><p>${pending ? `${pending} pending item${pending === 1 ? '' : 's'} in other views.` : current.runs.length ? 'No process run is waiting on anyone.' : 'The worklist fills as instantiated runs wait on people or hit blocks.'}</p></div>`}
    </${RequestBody}></div></div>`;
}

export function ProcessEditorBoundary({ spec, state, confirmDiscard, openEditor = null }) {
  const mountRef = useRef(null);
  const [error, setError] = useState('');
  useEffect(() => {
    let disposed = false; let editor = null;
    setError('');
    const loadEditor = openEditor || (async (mount, value) => {
      const { openTemplateEditor } = await import('./process-editor.js');
      return openTemplateEditor(mount, value);
    });
    loadEditor(mountRef.current, { id: spec.id, blank: spec.blank, config: { confirmDiscard } }).then((value) => { editor = value; if (disposed) editor?.destroy?.(); else state.setEditor(editor); }).catch((error) => { if (!disposed) { setError(error.message); state.setNotice(`Could not open editor: ${error.message}`); } });
    return () => { disposed = true; state.setEditor(null); editor?.destroy?.(); };
  }, [spec.key]);
  return html`<div id="process-editor-canvas" ref=${mountRef} class="process-canvas-mount" data-process-mount="editor">${error && html`<div class="process-placeholder" role="alert">Could not open editor: ${error}</div>`}</div>`;
}

export function ProcessesApp({ state, actions, confirmDiscard }) {
  const current = { ...state.view.value, state };
  useEffect(() => { if (current.active) void actions.refreshActive(); }, [current.active]);
  useEffect(() => { const poll = () => {
    const view = state.view.value;
    if (!view.active) return;
    if ((view.subtab === 'templates' || view.subtab === 'worklist')
        && view.requests[view.subtab].phase !== 'loading') void actions.load(view.subtab, { quiet: true });
  }; document.addEventListener('tclaude:snapshot', poll); return () => document.removeEventListener('tclaude:snapshot', poll); }, []);
  useEffect(() => { const reselected = (event) => { if (event.detail?.tab === 'processes' && state.view.value.active) void actions.refreshActive(); }; document.addEventListener('tclaude:tab-reselected', reselected); return () => document.removeEventListener('tclaude:tab-reselected', reselected); }, []);
  const navigate = async (event, name) => { if (isModifiedClick(event)) return; event.preventDefault(); await actions.activateSubtab(name); };
  const subtabKey = (event) => { if (event.key === ' ' || event.key === 'Spacebar') { event.preventDefault(); event.currentTarget.click(); } };
  const spec = current.canvas;
  return html`<div class="processes-island"><div class="process-subnav" role="tablist" aria-label="Process views">${['templates', 'runs', 'worklist'].map((name) => html`<a key=${name} class=${`process-subtab${current.subtab === name ? ' active' : ''}`} data-process-subtab=${name} href=${`/processes/${name}`} role="tab" aria-selected=${current.subtab === name} onClick=${(event) => navigate(event, name)} onKeyDown=${subtabKey}>${name[0].toUpperCase() + name.slice(1)}${name === 'worklist' && html`<span id="process-worklist-badge" class="tab-badge warn" hidden=${current.actionable === 0}>${current.actionable}</span>`}</a>`)}<span class="spacer"></span><span id="process-notice" class="process-notice" role="status">${current.notice}</span></div>
    ${spec ? html`<div id=${spec.kind === 'editor' ? 'process-editor-view' : 'process-viewer-view'} class="process-canvas-view"><button class="process-action" data-process-close-view type="button" onClick=${actions.closeCanvas}>← ${current.subtab}</button>${spec.kind === 'editor' ? html`<${ProcessEditorBoundary} spec=${spec} state=${state} confirmDiscard=${confirmDiscard} />` : html`<div id="process-viewer-canvas" class="process-canvas-mount" data-process-mount="viewer"><h3>Run: ${spec.id}</h3><p>Live viewer mount point. The viewer ticket takes over this canvas.</p></div>`}</div>` : current.subtab === 'templates' ? html`<${Templates} current=${current} actions=${actions} />` : current.subtab === 'runs' ? html`<${Runs} current=${current} actions=${actions} />` : html`<${Worklist} current=${current} actions=${actions} />`}
  </div>`;
}

export function mountProcessesIsland({ host, state, actions, confirmDiscard, registerCleanup }) {
  render(html`<${ProcessesApp} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} />`, host);
  // Rendering null unmounts ProcessEditorBoundary, the sole owner of editor /
  // graph disposal. Do not destroy through state here as well.
  registerCleanup(() => render(null, host));
}
