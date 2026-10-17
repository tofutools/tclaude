// dock-processes.js -- the dock's Processes section (tcl-rzik1e): the process
// template library at the bottom of the right panel, after Agent profiles,
// Group templates and Roles. It reuses the dock's section and card markup, but
// reads its own data: process templates are not on the dashboard snapshot.
//
// - A card shows the template's name, version count, a one-line summary and
//   the groups it is pinned to. Dragging it onto a group pins it there
//   (dock-dnd.js, kind 'processes').
// - Clicking a card opens a preview: the graph as a not-yet-started run, then
//   Start in <group>, Edit, Pin to group, unpin (× on a pinned group) and
//   share (.pwf).
// - ⤒ and dropping a .pwf on the section import through the existing review.
// - ⧉ opens Automations › Processes, where templates are managed and deleted.
// - The card ⚙ menu carries the library's shared rows (dock-library.js):
//   rename and clone (both through a name dialog), pin to top, copy the id,
//   share as .pwf, delete. The dock's search filters the cards.

import { h } from 'preact';
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { wizWord } from './slop.js';
import { ProcessMiniGraph } from './process-kit.js';
import { processContext, processRunApi } from './process-context.js';
import { WorkflowExportButton, WorkflowImportButton } from './process-workflow-share.js';
import { handOffProcessDraft } from './process-draft-handoff.js';
import { requestProcessStart } from './process-start-handoff.js';
import { dashPrefs } from './prefs.js';
import { groupsState } from './groups-state.js';
import { showRunsLens } from './groups-focus.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { processTemplateDeleteConfirmation } from './process-template-delete.js';
import { managementController } from './management-controller.js';
import { exportWorkflow } from './process-workflow-share.js';
import {
  LIBRARY_EVENT, cloneProcess, dropPin, filterSection, freeName, leafTexts, orderPinned, pinnedNames,
  renameItem, togglePin,
} from './dock-library.js';

const html = htm.bind(h);

// PROCESS_LIBRARY_EVENT asks every library view to reload, after a pin from
// a drop or anything else that changes what it lists.
export const PROCESS_LIBRARY_EVENT = 'tclaude:process-library-changed';

export function processSummaryText(row) {
  const summary = row.summary || {};
  const parts = [`${summary.steps || 0} step${summary.steps === 1 ? '' : 's'}`];
  if (summary.roles?.length) parts.push(summary.roles.join(', '));
  if (summary.commands) parts.push(`${summary.commands} command${summary.commands === 1 ? '' : 's'}`);
  if (summary.asks) parts.push(summary.asks === 1 ? 'asks you once' : `asks you ${summary.asks}×`);
  return parts.join(' · ');
}

async function libraryRows(all, fetchImpl) {
  const response = await fetchImpl(`/v1/process/templates${all ? '?all=1' : ''}`, { credentials: 'same-origin' });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
  return body.templates || [];
}

// templateAsRun draws a stored template with the run renderer: every step
// planned, edges labelled with their outcome.
export function templateAsRun(view) {
  const template = view?.template || {};
  const nodes = Object.entries(template.nodes || {}).map(([id, node]) => ({
    id, kind: node?.type || 'task', state: 'planned', label: node?.name || id,
    performer: node?.performer ? { kind: node.performer.kind } : undefined,
  }));
  const edges = (view?.edges || []).filter((edge) => edge.from && edge.to)
    .map((edge) => ({ from: edge.from, to: edge.to, label: edge.outcome }));
  return { nodes, edges };
}

export async function pinToGroup(group, templateId, { api = processRunApi, doc = document, context = processContext } = {}) {
  await api.pinProcess(group, templateId);
  doc.dispatchEvent(new CustomEvent(PROCESS_LIBRARY_EVENT));
  // The group's saved-process chips read the process context.
  void context?.refresh?.();
}

// unpinFromGroup removes the template from a group's processes; the template
// stays in the library.
export async function unpinFromGroup(group, templateId, { api = processRunApi, doc = document, context = processContext } = {}) {
  await api.unpinProcess(group, templateId);
  doc.dispatchEvent(new CustomEvent(PROCESS_LIBRARY_EVENT));
  void context?.refresh?.();
}

// deleteFromLibrary asks first (the same confirmation as Automations ›
// Processes), deletes the template with its whole version history, and asks
// every library view and the group chips to reload. False when declined.
export async function deleteFromLibrary(row, {
  confirm = shellConfirm, notify = shellToast, fetchImpl = (...args) => fetch(...args),
  doc = document, context = processContext,
} = {}) {
  const wizard = doc.body?.classList?.contains('wizard');
  if (!(await confirm(processTemplateDeleteConfirmation({ ...row, wizard })))) return false;
  const response = await fetchImpl(`/v1/process/templates/${encodeURIComponent(row.id)}`, {
    method: 'DELETE', credentials: 'same-origin',
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok && response.status !== 404) {
    throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
  }
  notify?.(`deleted process template ${String(row.name || '').trim() || row.id}`);
  doc.dispatchEvent(new CustomEvent(PROCESS_LIBRARY_EVENT));
  void context?.refresh?.();
  return true;
}

function Preview({ row, anchor, groups, onClose, onChanged, onDelete, fetchImpl }) {
  const [view, setView] = useState(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const ref = useRef(null);
  useEffect(() => {
    let live = true;
    fetchImpl(`/v1/process/templates/${encodeURIComponent(row.id)}`, { credentials: 'same-origin' })
      .then((response) => response.json().then((body) => (response.ok ? body : Promise.reject(new Error(body.message || body.error || response.statusText)))))
      .then((body) => { if (live) setView(body); })
      .catch((cause) => { if (live) setError(cause.message); });
    return () => { live = false; };
  }, [row.id]);
  useEffect(() => {
    // The delete confirmation is a shell dialog outside the preview; answering
    // it (or cancelling with Escape) must not also close the preview.
    const inConfirm = (event) => Boolean(event.target?.closest?.('#shell-confirm-root'));
    const onKey = (event) => { if (event.key === 'Escape' && !inConfirm(event)) onClose(); };
    const onDown = (event) => {
      if (inConfirm(event)) return;
      if (!ref.current?.contains(event.target) && !anchor?.contains(event.target)) onClose();
    };
    document.addEventListener('keydown', onKey);
    document.addEventListener('pointerdown', onDown);
    return () => {
      document.removeEventListener('keydown', onKey);
      document.removeEventListener('pointerdown', onDown);
    };
  }, [anchor, onClose]);
  const rect = anchor?.getBoundingClientRect?.();
  const style = rect ? `top:${Math.max(8, Math.min(rect.top, window.innerHeight - 340))}px;right:${Math.max(8, window.innerWidth - rect.left + 10)}px` : '';
  const pinned = new Set(row.pinnedGroups || []);
  const change = async (group, act) => {
    if (!group || busy) return;
    setBusy(true);
    setError('');
    try {
      await act(group, row.id);
      onChanged();
    } catch (cause) {
      setError(cause.message);
    } finally {
      setBusy(false);
    }
  };
  const pin = (group) => change(group, pinToGroup);
  const unpin = (group) => change(group, unpinFromGroup);
  const pick = (event, act) => {
    const group = event.currentTarget.value;
    event.currentTarget.value = '';
    act(group);
  };
  return html`<div ref=${ref} class="dock-process-preview" role="dialog" aria-label=${`${row.name || row.id} preview`} style=${style} data-dock-process-preview=${row.id}>
    <div class="dpp-head"><b>${row.name || row.id}</b><span class="dpp-meta">v${row.versionCount || 1}${row.listed === false ? ' · group-only' : ''}</span>
      <span class="dpp-spacer"></span><button type="button" class="dpp-close" aria-label="Close" onClick=${onClose}>✕</button></div>
    ${row.description && html`<p class="dpp-desc">${row.description}</p>`}
    ${pinned.size > 0 && html`<div class="dpp-pins" data-dock-process-pins>Pinned to
      ${[...pinned].map((group) => html`<span key=${group} class="dock-chip dock-process-pin">${group}<button type="button" class="dpp-unpin"
        data-dock-process-unpin=${group} disabled=${busy} aria-label=${`Unpin from ${group}`} title=${`Unpin from ${group} (the template stays in the library)`}
        onClick=${() => unpin(group)}>×</button></span>`)}</div>`}
    <div class="dpp-graph">${view ? html`<${ProcessMiniGraph} run=${templateAsRun(view)} compact=${true} />` : !error && html`<span class="dpp-meta">Loading…</span>`}</div>
    <div class="dpp-acts">
      <select class="dpp-start" aria-label="Start in group" data-dock-process-start onChange=${(event) => pick(event, (group) => { onClose(); requestProcessStart({ group, templateId: row.id }, { prefs: dashPrefs, setFocus: (lens) => showRunsLens(groupsState, lens), rerender: groupsState.rerender }); })}>
        <option value="">${wizWord('▶ Start in…', '▶ Begin in…')}</option>
        ${groups.map((group) => html`<option key=${group} value=${group}>${group}</option>`)}
      </select>
      <button type="button" class="process-action" data-dock-process-edit onClick=${() => { onClose(); handOffProcessDraft({ id: row.id, name: row.name || row.id }); }}>✎ Edit</button>
      <select class="dpp-pin" aria-label="Pin to group" disabled=${busy} data-dock-process-pin onChange=${(event) => pick(event, pin)}>
        <option value="">Pin to group…</option>
        ${groups.filter((group) => !pinned.has(group)).map((group) => html`<option key=${group} value=${group}>${group}</option>`)}
      </select>
      <${WorkflowExportButton} id=${row.id} />
      <span class="dpp-spacer"></span>
      <button type="button" class="process-action process-action-danger" data-dock-process-delete onClick=${() => onDelete(row)}>${wizWord('Delete', 'Dispel')}</button>
    </div>
    ${error && html`<div class="island-error" role="alert">${error}</div>`}
  </div>`;
}

// askProcessName opens the library's name dialog for a rename or a clone.
function askProcessName({ title, blurb, initial, okLabel, submit, controller = managementController }) {
  controller().openLibraryName({ title, blurb, initial, okLabel, submit });
}

function ProcessCard({
  row, open, onOpen, openMenu, setOpenMenu, onDelete, clipHost = null, pinned = false, onMenu = () => {},
}) {
  const ref = useRef(null);
  const name = row.name || row.id;
  // The ⚙ menu shares the dock's one-open-menu state, so the dock's outside
  // click and Escape close it like every other card's.
  const menuKey = `processes:${row.id}`;
  const menuOpen = openMenu === menuKey;
  const menuRef = useRef(null);
  const cogRef = useRef(null);
  const [opensUp, setOpensUp] = useState(false);
  // This is the dock's last section, so a bottom card's menu would open below
  // the scroll area; flip it up like the other sections' cards do.
  useLayoutEffect(() => {
    if (!menuOpen || !menuRef.current || !cogRef.current) {
      setOpensUp(false);
      return;
    }
    const clip = clipHost?.getBoundingClientRect() || { top: 0, bottom: window.innerHeight };
    const menuRect = menuRef.current.getBoundingClientRect();
    const cogTop = cogRef.current.getBoundingClientRect().top;
    setOpensUp(menuRect.bottom > clip.bottom && menuRect.height < cogTop - clip.top);
  }, [menuOpen, clipHost]);
  const act = (event, action) => {
    event.preventDefault();
    setOpenMenu(null);
    action();
  };
  // The body is not a <button>: the dock's drag gesture stands down on
  // buttons, and the whole card should drag.
  const toggle = () => onOpen(open ? null : { row, anchor: ref.current });
  return html`<div ref=${ref} class=${`dock-card dock-process-card${open ? ' is-open' : ''}`} draggable="true"
      data-dock-kind="processes" data-dock-name=${row.id} data-dock-label=${name}
      title=${wizWord('Click to preview · drag onto a group to pin it there', 'Click to scry · drag onto a party to bind it')}>
    <span class="dock-grip" aria-hidden="true">⠿</span>
    <div role="button" tabIndex="0" class="dock-card-body dock-process-open" aria-expanded=${open ? 'true' : 'false'}
      onClick=${toggle} onKeyDown=${(event) => { if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); toggle(); } }}>
      <span class="dock-card-name">${pinned ? html`<span class="dock-card-pin" title="Pinned to the top" aria-label="Pinned">📌</span>` : null}${name} <span class="dock-process-version">v${row.versionCount || 1}</span></span>
      <span class="dock-process-summary">${processSummaryText(row)}</span>
      <span class="dock-chips dock-chips-compact">${(row.pinnedGroups || []).length
        ? row.pinnedGroups.map((group) => html`<span key=${group} class="dock-chip dock-process-pin">${group}</span>`)
        : html`<span class="dock-chip dock-chip-more">not pinned</span>`}</span>
    </div>
    <span class="dock-card-actions">
      <button ref=${cogRef} type="button" class="dock-card-manage" data-dock-act="card-menu" data-dock-kind="processes" data-dock-name=${row.id}
        aria-haspopup="menu" aria-expanded=${menuOpen ? 'true' : 'false'} title="More actions" aria-label=${`Actions for ${name}`}
        onClick=${(event) => { event.preventDefault(); setOpenMenu(menuOpen ? null : menuKey); }}>⚙</button>
      <div ref=${menuRef} class=${`dock-card-menu${menuOpen ? ' open' : ''}${opensUp ? ' opens-up' : ''}`} role="menu" aria-label=${name}>
        <button type="button" role="menuitem" class="dock-card-menu-item" data-dock-act="edit-item" data-dock-kind="processes" data-dock-name=${row.id}
          onClick=${(event) => act(event, () => handOffProcessDraft({ id: row.id, name }))}>Edit</button>
        ${[
          ['clone-item', wizWord('Clone', 'Mirror')],
          ['rename-item', 'Rename…'],
          ['pin-item', pinned ? 'Unpin' : 'Pin to top'],
          ['copy-item', 'Copy id'],
          ['export-item', 'Export .pwf'],
        ].map(([key, label]) => html`<button key=${key} type="button" role="menuitem" class="dock-card-menu-item" data-dock-act=${key}
          data-dock-kind="processes" data-dock-name=${row.id} onClick=${(event) => act(event, () => onMenu(key, row))}>${label}</button>`)}
        <button type="button" role="menuitem" class="dock-card-menu-item danger" data-dock-act="delete-item" data-dock-kind="processes" data-dock-name=${row.id}
          onClick=${(event) => act(event, () => onDelete(row))}>${wizWord('Delete', 'Dispel')}</button>
      </div>
    </span>
  </div>`;
}

// DockProcesses is the section. groups are the real groups' names, for the
// Start in / Pin to pickers.
export function DockProcesses({
  groups = [], isSectionOpen, setSectionOpen, openMenu = null, setOpenMenu = () => {}, clipHost = null,
  fetchImpl = (...args) => fetch(...args), confirm = shellConfirm,
  terms = [], prefs = dashPrefs, notify = shellToast, onMatches = null, controller = managementController,
}) {
  const [open, setOpen] = useState(() => isSectionOpen('processes'));
  const [, setPinsVersion] = useState(0);
  const searching = terms.length > 0;
  const [all, setAll] = useState(false);
  const [rows, setRows] = useState(null);
  const [error, setError] = useState('');
  const [preview, setPreview] = useState(null);
  const [dropped, setDropped] = useState(null);
  const [dropOver, setDropOver] = useState(false);
  const load = useCallback(() => {
    libraryRows(all, fetchImpl).then((next) => { setRows(next); setError(''); }).catch((cause) => setError(cause.message));
  }, [all]);
  useEffect(() => { if (open || searching) load(); }, [open, searching, load]);
  useEffect(() => {
    document.addEventListener(PROCESS_LIBRARY_EVENT, load);
    document.addEventListener(LIBRARY_EVENT, load);
    return () => {
      document.removeEventListener(PROCESS_LIBRARY_EVENT, load);
      document.removeEventListener(LIBRARY_EVENT, load);
    };
  }, [load]);
  const remove = async (row) => {
    try {
      if (!(await deleteFromLibrary(row, { confirm, fetchImpl }))) return;
      dropPin(prefs, 'processes', row.id);
      if (preview?.row.id === row.id) setPreview(null);
    } catch (cause) {
      setError(`Delete failed: ${cause.message}`);
    }
  };
  // Pins key on the template id, which a rename keeps.
  const pins = pinnedNames(prefs, 'processes');
  const { items: matched } = filterSection({
    terms,
    titles: [wizWord('Processes', 'Rites'), 'processes', 'rites', 'workflows'],
    items: rows || [],
    textsOf: (row) => [row.name, row.id, processSummaryText(row), ...leafTexts(row)],
  });
  const shown = orderPinned(matched, (row) => row.id, pins);
  useEffect(() => { onMatches?.('processes', searching && rows === null ? null : shown.length); }, [shown.length, searching, rows === null]);
  const menu = async (key, row) => {
    const name = String(row.name || '').trim() || row.id;
    if (key === 'pin-item') {
      togglePin(prefs, 'processes', row.id);
      setPinsVersion((value) => value + 1);
    } else if (key === 'copy-item') {
      try {
        await navigator.clipboard.writeText(row.id);
        notify?.(`copied ${row.id}`);
      } catch {
        notify?.('copy failed: the clipboard is not available here', true);
      }
    } else if (key === 'export-item') {
      const failed = await exportWorkflow(row.id, { fetchImpl }).catch((cause) => cause.message);
      if (failed) notify?.(`export failed: ${failed}`, true);
    } else if (key === 'rename-item') {
      askProcessName({
        controller,
        title: `Rename ${wizWord('process', 'rite')}: ${name}`,
        blurb: 'Saved as a new version; the history and the groups it is pinned to stay.',
        initial: name,
        okLabel: 'Rename',
        submit: async (to) => {
          if (to === name) return;
          await renameItem({ section: 'processes', name: row.id, to, fetchImpl });
          notify?.(`renamed ${name} → ${to}`);
          document.dispatchEvent(new CustomEvent(PROCESS_LIBRARY_EVENT));
          void processContext?.refresh?.();
        },
      });
    } else if (key === 'clone-item') {
      askProcessName({
        controller,
        title: `${wizWord('Clone', 'Mirror')} ${wizWord('process', 'rite')}: ${name}`,
        blurb: 'A new template with the same steps, starting its own history; it is not pinned to any group.',
        initial: freeName(name, (rows || []).map((other) => other.name)),
        okLabel: wizWord('Create copy', 'Mirror it'),
        submit: async (to) => {
          await cloneProcess({ id: row.id, to, fetchImpl });
          notify?.(`created ${to}`);
          document.dispatchEvent(new CustomEvent(PROCESS_LIBRARY_EVENT));
        },
      });
    }
  };
  const fileDrag = (event) => [...(event.dataTransfer?.types || [])].includes('Files');
  const current = preview && rows?.find((row) => row.id === preview.row.id);
  if (searching && rows !== null && !shown.length) return null;
  return html`<details class="dock-section dock-processes" data-key="processes" open=${open || searching}
      onToggle=${(event) => { if (searching) return; const next = event.currentTarget.hasAttribute('open'); setOpen(next); setSectionOpen('processes', next); }}
      onDragOver=${(event) => { if (!fileDrag(event)) return; event.preventDefault(); setDropOver(true); }}
      onDragLeave=${(event) => { if (!event.currentTarget.contains(event.relatedTarget)) setDropOver(false); }}
      onDrop=${(event) => {
        if (!fileDrag(event)) return;
        event.preventDefault();
        setDropOver(false);
        const file = event.dataTransfer.files?.[0];
        if (file) setDropped(file);
      }}>
    <summary class="dock-section-head">
      <span class="dock-section-title">
        <span class="dock-section-chevron" aria-hidden="true">▸</span>
        <span class="dock-section-icon" aria-hidden="true">◈</span>
        ${wizWord('Processes', 'Rites')}
        <span class="dock-section-count">${rows ? (searching && shown.length !== rows.length ? `${shown.length}/${rows.length}` : rows.length) : ''}</span>
      </span>
      <button type="button" class="dock-section-manage" data-dock-act="manage-all" data-dock-kind="processes"
        title="Open Automations › Processes"
        onClick=${(event) => { event.preventDefault(); document.querySelector('nav [data-tab="processes"]')?.click(); }}>⧉</button>
    </summary>
    <div class="dock-section-items">
      <div class="dock-process-tools">
        <span class="dock-process-toggle" role="group" aria-label="Which templates">
          <button type="button" class=${all ? '' : 'on'} aria-pressed=${all ? 'false' : 'true'} onClick=${() => setAll(false)}>library</button>
          <button type="button" class=${all ? 'on' : ''} aria-pressed=${all ? 'true' : 'false'} data-dock-process-all onClick=${() => setAll(true)}>all</button>
        </span>
        <span class="dpp-spacer"></span>
        <${WorkflowImportButton} id="" className="dock-process-import" label="⤒ import" dropped=${dropped} onImported=${load} />
        <button type="button" class="dock-process-new" title="New process template" onClick=${() => handOffProcessDraft({ create: true })}>＋</button>
      </div>
      ${error && html`<div class="dock-empty island-error" role="alert">${error}</div>`}
      ${rows && !rows.length && !error && html`<div class="dock-empty">(${wizWord('no processes in the library yet', 'no rites in the grimoire yet')})</div>`}
      ${shown.map((row) => html`<${ProcessCard} key=${row.id} row=${row} open=${preview?.row.id === row.id} onOpen=${setPreview}
        openMenu=${openMenu} setOpenMenu=${setOpenMenu} onDelete=${remove} clipHost=${clipHost}
        pinned=${pins.includes(row.id)} onMenu=${menu} />`)}
      <div class=${`dock-process-drop${dropOver ? ' is-over' : ''}`}>⤓ ${wizWord('drop a .pwf here to import it', 'drop a .pwf scroll here')}</div>
    </div>
    ${current && html`<${Preview} row=${current} anchor=${preview.anchor} groups=${groups} fetchImpl=${fetchImpl}
      onClose=${() => setPreview(null)} onChanged=${load} onDelete=${remove} />`}
  </details>`;
}
