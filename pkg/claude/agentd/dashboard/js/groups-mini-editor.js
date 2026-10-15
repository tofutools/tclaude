// groups-mini-editor.js -- the inline mini editor (mock M1, tcl-jqmrj5): a
// saved process opened in place in its group as a small graph. It is a
// constrained front end on the full editor's pieces, not a second editor:
// the draft lives in a ProcessEditModel (undo, redo, dirty, the CAS base),
// the graph is the kit's ProcessMiniGraph, and every structural edit is a
// pure process-mini-edit.js operation landed with model.applyDraft. Hover a
// step for + handles, click it for a compact inspector (the step list's
// fields), right-click it for structure, or drag a handle into empty space.
// ☰ Steps shows the same draft as the step list, ⤢ full editor opens it in
// Automations › Processes, and Save writes a new version.

import { h } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ProcessMiniGraph } from './process-kit.js';
import { ProcessEditModel } from './process-edit-model.js';
import {
  addOutcome, branch, canBranch, canDelete, canInsertAfter, canInsertBefore, canRunAlongside, changeKind, changeSummary,
  deleteOrphans, deleteStep, draftProjection, duplicate, insertAfter, insertBefore, isStep, miniKind, outcomesOf,
  removeOutcome, renameOutcome, runAlongside, setOutcomeTarget, targetsFor, updateStep,
} from './process-mini-edit.js';
import { collapseParamShorthand, setStepName, setStepText, setStepWho, stepText, stepWho, viewToStepList } from './process-step-list.js';
import { handOffProcessDraft } from './process-draft-handoff.js';
import { WhoPicker, defaultNotify, markQuickStart } from './groups-quickstart.js';

const html = htm.bind(h);

const KIND_CHOICES = Object.freeze([['agent', '🤖', 'agent'], ['you', '👤', 'you'], ['command', '⌘', 'command']]);
const VALIDATE_DELAY_MS = 400;
// A pointer that moves this far from a handle is a drag, not a click.
const DRAG_SLOP = 8;

// whoLine is a step's sub line in the graph: who does it.
function whoLine(node) {
  const who = stepWho({ node });
  if (who.mode === 'agent') return who.agent;
  if (who.mode === 'role') return `any ${who.role}`;
  if (who.mode === 'anyone') return 'anyone free';
  if (who.mode === 'you') return 'you';
  if (who.mode === 'command') return stepText({ node }) || 'a command';
  return 'pick who';
}

function viewOf(model) {
  const { template, edges, layout } = model.saveBody();
  return { template, edges, layout };
}

// floating places a popup at viewport coordinates, kept inside the window:
// the canvas scrolls, so a popup positioned inside it would be clipped.
function floating(at, width, height) {
  const w = typeof window !== 'undefined' && window.innerWidth ? window.innerWidth : 0;
  const h = typeof window !== 'undefined' && window.innerHeight ? window.innerHeight : 0;
  const left = w ? Math.max(8, Math.min(at.cx, w - width - 8)) : at.cx;
  const top = h ? Math.max(8, Math.min(at.cy, h - height - 8)) : at.cy;
  return `left:${left}px;top:${top}px`;
}

// KindChips is a row of agent / you / command buttons.
function KindChips({ onPick, disabled = '', label = '' }) {
  return html`<span class="gm-kinds" role="group" aria-label=${label || 'Pick a kind'}>
    ${KIND_CHOICES.map(([kind, glyph, text]) => html`<button key=${kind} type="button" class="gm-kind" data-mini-kind=${kind}
      disabled=${!!disabled} title=${disabled || `${label ? `${label}: ` : ''}${text}`} onClick=${() => onPick(kind)}>${glyph} ${text}</button>`)}
  </span>`;
}

// Inspector is the compact editor of one step: the step list's fields.
function Inspector({ view, id, roster, roles, onApply, onKind, onClose, onFullEditor }) {
  const node = view.template.nodes[id];
  if (!node) return null;
  const kind = miniKind(node);
  if (!isStep(node)) {
    return html`<div class="gm-inspector" data-mini-inspector=${id} role="dialog" aria-label=${`${node.name || id}`}>
      <div class="gm-row"><b>${node.name || id}</b><span class="gq-dim">${node.type}</span><span class="gq-spacer"></span>
        <button type="button" class="gq-link" aria-label="Close" onClick=${onClose}>✕</button></div>
      ${kind === 'advanced'
    ? html`<p class="gq-dim">${node.type} nodes are edited in the full editor.</p>
        <button type="button" class="gq-btn" data-mini-full-node onClick=${onFullEditor}>⤢ edit in full editor</button>`
    : html`<p class="gq-dim">The ${node.type} of the process. Hover it for + after.</p>`}
    </div>`;
  }
  const step = { id, node };
  const text = collapseParamShorthand(stepText(step));
  const outcomes = outcomesOf(view, id);
  const targets = targetsFor(view, id);
  const setWho = (who) => {
    if (who.mode === 'you' || who.mode === 'command') { onKind(who.mode); return; }
    if (kind !== 'agent') { onKind('agent', who); return; }
    onApply(updateStep(view, id, (s) => setStepWho(s, who)));
  };
  const label = { agent: 'Prompt', you: 'Asks', command: 'Runs' }[kind];
  return html`<div class="gm-inspector" data-mini-inspector=${id} role="dialog" aria-label=${`Step ${node.name || id}`}
    onKeyDown=${(event) => { if (event.key === 'Escape') { event.stopPropagation(); onClose(); } }}>
    <label class="gm-field"><span>Step</span><input type="text" data-mini-name value=${node.name || ''} placeholder=${id}
      onChange=${(event) => onApply(updateStep(view, id, (s) => setStepName(s, event.currentTarget.value)))} /></label>
    <div class="gm-field"><span>Who</span><${WhoPicker} step=${step} roster=${roster} roles=${roles} onChange=${setWho} /></div>
    <label class="gm-field"><span>${label}</span><textarea rows="3" data-mini-text value=${text}
      onChange=${(event) => onApply(updateStep(view, id, (s) => setStepText(s, event.currentTarget.value)))}></textarea></label>
    <div class="gm-field"><span>${node.type === 'decision' ? 'Outcomes' : 'Then'}</span><div class="gm-outcomes">
      ${outcomes.map(({ outcome, to }) => html`<span key=${outcome} class="gm-outcome" data-mini-outcome=${outcome}>
        ${node.type === 'decision'
    ? html`<input type="text" class="gm-outcome-name" aria-label=${`Rename ${outcome}`} value=${outcome}
          onChange=${(event) => onApply(renameOutcome(view, id, outcome, event.currentTarget.value))} />`
    : ''} →
        <select aria-label=${`After ${outcome}`} value=${to} onChange=${(event) => onApply(setOutcomeTarget(view, id, outcome, event.currentTarget.value))}>
          ${targets.map((target) => html`<option key=${target.id} value=${target.id}>${target.label}</option>`)}
        </select>
        ${node.type === 'decision' && outcomes.length > 1 && html`<button type="button" class="gq-link" aria-label=${`Remove ${outcome}`}
          onClick=${() => onApply(removeOutcome(view, id, outcome))}>✕</button>`}
      </span>`)}
      ${node.type === 'decision' && html`<button type="button" class="gm-add-outcome" data-mini-add-outcome title="Add an outcome"
        onClick=${() => onApply(addOutcome(view, id, outcomes[0]?.to || targets.find((t) => view.template.nodes[t.id]?.type === 'end')?.id || targets[0]?.id))}>+</button>`}
      ${!outcomes.length && html`<span class="gq-dim">nothing yet: hover the step for + after</span>`}
    </div></div>
    <div class="gm-row"><span class="gq-spacer"></span><button type="button" class="gq-link" onClick=${onClose}>done</button></div>
  </div>`;
}

// StructureMenu is the right-click menu of one node.
function StructureMenu({ view, menu, onRun, onWrap, onClose }) {
  const { id } = menu;
  const node = view.template.nodes[id];
  if (!node) return null;
  const step = isStep(node);
  const orphans = step || node.type === 'end' ? deleteOrphans(view, id) : [];
  const item = (key, text, why, onClick) => html`<button type="button" role="menuitem" class="gm-menu-item" data-mini-menu=${key}
    disabled=${!!why} title=${why} onClick=${onClick}>${text}</button>`;
  const kinds = (key, text, why, op) => html`<div class="gm-menu-kinds" data-mini-menu=${key}><span class=${why ? 'gm-off' : ''} title=${why}>${text}</span>
    <${KindChips} label=${text} disabled=${why} onPick=${(kind) => onRun(() => op(view, id, kind))} /></div>`;
  return html`<div class="gm-menu" role="menu" aria-label=${`Structure of ${node.name || id}`} style=${floating(menu, 400, 330)}
    onKeyDown=${(event) => { if (event.key === 'Escape') { event.stopPropagation(); onClose(); } }}>
    ${kinds('before', 'Insert step before', canInsertBefore(view, id), insertBefore)}
    ${kinds('after', 'Insert step after', canInsertAfter(view, id), insertAfter)}
    ${kinds('branch', 'Branch on outcome', step ? canBranch(view, id) : 'only a step branches', branch)}
    ${kinds('alongside', 'Run alongside (parallel)', canRunAlongside(view, id), runAlongside)}
    <hr />
    ${item('duplicate', 'Duplicate', step ? '' : 'only a step can be duplicated here', () => onRun(() => duplicate(view, id)))}
    ${item('wrap', 'Wrap in a subprocess… (full editor)', step ? '' : 'only a step can be wrapped', onWrap)}
    ${step && html`<div class="gm-menu-kinds" data-mini-menu="kind"><span>Change to</span>
      <${KindChips} label="Change to" onPick=${(kind) => onRun(() => changeKind(view, id, kind))} /></div>`}
    <hr />
    ${item('delete', orphans.length ? `Delete step (and ${orphans.join(', ')}, only reached through it)` : 'Delete step',
    canDelete(view, id), () => onRun(() => deleteStep(view, id)))}
  </div>`;
}

// MiniGraphEditor is the inline editor card of one saved process.
// `process` is the saved process ({templateId, name, listed}); `view` its
// stored edit view (with sourceHash), and `stored` the view it was opened at
// (the change summary compares against it). `changed` opens it dirty (the
// step list handed over unsaved edits).
export function MiniGraphEditor({
  group, roster, groupInfo = null, process, view, stored = view, changed = false, api, source, wizard = false,
  notify = defaultNotify, onClose, onSteps,
}) {
  const model = useRef(null);
  if (!model.current) {
    model.current = new ProcessEditModel(view);
    if (changed) model.current.savedRev = -1;
  }
  const [rev, setRev] = useState(0);
  const [hover, setHover] = useState('');
  const [selected, setSelected] = useState('');
  const [menu, setMenu] = useState(null);
  const [picker, setPicker] = useState(null);
  const [drag, setDrag] = useState(null);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [diagnostics, setDiagnostics] = useState([]);
  const [publish, setPublish] = useState(false);
  const [discarding, setDiscarding] = useState(false);
  const canvas = useRef(null);
  // dragged swallows the click that ends a handle drag.
  const dragged = useRef(false);
  const draft = useMemo(() => viewOf(model.current), [rev]);
  const draftKey = JSON.stringify(draft);
  const dirty = model.current.dirty;
  const summary = dirty ? changeSummary(stored, draft) : [];
  const name = draft.template.name || process.name;
  useEffect(() => {
    let live = true;
    if (typeof api.validateTemplate !== 'function') return undefined;
    const timer = setTimeout(() => {
      api.validateTemplate(draft).then((found) => { if (live) setDiagnostics(found); }).catch(() => { if (live) setDiagnostics([]); });
    }, VALIDATE_DELAY_MS);
    return () => { live = false; clearTimeout(timer); };
  }, [draftKey]);
  // The menu and the picker float at viewport coordinates; a scroll would
  // leave them behind, so it closes them.
  useEffect(() => {
    if (!menu && !picker) return undefined;
    const close = () => { setMenu(null); setPicker(null); };
    window.addEventListener('scroll', close, true);
    return () => window.removeEventListener('scroll', close, true);
  }, [!!menu, !!picker]);
  const errors = diagnostics.filter((d) => d.severity === 'error');
  const roles = groupInfo?.roles || null;
  // apply lands one operation's result as one undoable edit.
  const apply = (result, focus = undefined) => {
    setError('');
    model.current.applyDraft(result.view);
    setRev(model.current.rev);
    if (focus !== undefined) setSelected(focus);
  };
  const run = (op) => {
    setMenu(null);
    setPicker(null);
    try {
      const result = op();
      if (result.view === draft) return;
      apply(result, result.id && result.view.template.nodes[result.id] ? result.id : '');
    } catch (cause) {
      setError(cause.message);
    }
  };
  const undo = () => { if (model.current.undo()) { setRev(model.current.rev); setSelected(''); } };
  const stepsList = viewToStepList(draft);
  const save = async () => {
    if (busy) return false;
    setBusy('save');
    setError('');
    const savedAt = model.current.rev;
    try {
      const body = await api.saveTemplate(process.templateId, draft, { sourceHash: model.current.sourceHash, ...(publish ? { listed: true } : {}) });
      model.current.markSaved(body, savedAt);
      markQuickStart();
      notify(`Saved ${name} as a new version`);
      void source?.refresh?.();
      return true;
    } catch (cause) {
      setError(/head changed/.test(cause.message)
        ? `${name} was saved elsewhere since you opened it. Cancel and reopen it to edit the newest version.` : cause.message);
      return false;
    } finally {
      setBusy('');
    }
  };
  const fullEditor = async () => {
    if (dirty && !(await save())) return;
    handOffProcessDraft({ id: process.templateId, name });
    onClose();
  };
  // pointAt is a pointer position in the graph's own coordinates, where the
  // handles, the picker and the menu are drawn.
  const pointAt = (event) => {
    const graph = canvas.current?.querySelector?.('.pk-mini-graph');
    const box = graph?.getBoundingClientRect?.() || { left: 0, top: 0 };
    return { x: (event.clientX || 0) - box.left, y: (event.clientY || 0) - box.top };
  };
  const openMenu = (node, event) => {
    event.preventDefault();
    setPicker(null);
    setMenu({ id: node.id, cx: event.clientX || 0, cy: event.clientY || 0 });
  };
  // A handle opens the kind picker for its operation; dragged into empty
  // space, the new step lands from that step (a decision branches).
  const handle = (op, id, x, y, glyph, text, why) => html`<button type="button" class=${`gm-handle gm-handle-${op}`} data-mini-handle=${op}
    style=${`left:${x}px;top:${y}px`} disabled=${!!why} title=${why || text} aria-label=${`${text} ${view.template.nodes[id]?.name || id}`}
    onPointerDown=${(event) => { if (op === 'after' && !why) { const at = pointAt(event); setDrag({ id, ...at, x0: at.x, y0: at.y, moved: false }); } }}
    onClick=${(event) => {
      if (dragged.current) { dragged.current = false; return; }
      const box = event.currentTarget?.getBoundingClientRect?.() || { left: 0, bottom: 0 };
      if (!why) setPicker({ op, id, cx: box.left, cy: box.bottom + 4 });
    }}>${glyph}<span>${text}</span></button>`;
  const OPS = { after: insertAfter, branch, alongside: runAlongside, before: insertBefore };
  const dropOp = (id) => (draft.template.nodes[id]?.type === 'decision' && !canBranch(draft, id) ? 'branch' : 'after');
  // overlay draws, over the laid-out graph: the hovered (or selected) step's
  // handles, a drag's ghost, the kind picker and the structure menu.
  const overlay = (layout) => {
    const id = hover || selected;
    const node = layout.nodes.find((n) => n.id === id);
    const data = draft.template.nodes[id];
    const handles = node && data && data.type !== 'end' && miniKind(data) !== 'advanced';
    const right = handles ? node.x + node.width / 2 + 4 : 0;
    return html`${handles && html`<span class="gm-handles" data-mini-handles=${id}>
      ${handle('after', id, right, node.y - 9, '+', 'after', canInsertAfter(draft, id))}
      ${isStep(data) && handle('branch', id, node.x - 9, node.y + node.height / 2 + 2, '⑂', 'branch', canBranch(draft, id))}
      ${isStep(data) && handle('alongside', id, node.x - 9, node.y - node.height / 2 - 20, '⫴', 'alongside', canRunAlongside(draft, id))}
    </span>`}
      ${drag?.moved && html`<span class="gm-ghost" style=${`left:${drag.x}px;top:${drag.y}px`}>+ new step</span>`}
      ${picker && html`<div class="gm-picker" data-mini-picker=${picker.op} style=${floating(picker, 300, 40)}>
        <${KindChips} label=${`${{ after: 'Add after', branch: 'Branch to', alongside: 'Run alongside', before: 'Insert before' }[picker.op]}`}
          onPick=${(kind) => run(() => OPS[picker.op](draft, picker.id, kind))} />
        <button type="button" class="gq-link" aria-label="Close" onClick=${() => setPicker(null)}>✕</button>
      </div>`}
      ${menu && html`<${StructureMenu} view=${draft} menu=${menu} onRun=${run} onClose=${() => setMenu(null)}
        onWrap=${() => { setMenu(null); void fullEditor(); }} />`}`;
  };
  const projection = draftProjection(draft, { whoOf: (id, node) => whoLine(node), name });
  return html`<section class="gm-card" aria-label=${`Edit ${name} of ${group.name}`} data-mini-editor=${process.templateId}
    onKeyDown=${(event) => {
      if (event.key === 'Escape' && (menu || picker)) { event.stopPropagation(); setMenu(null); setPicker(null); }
      if ((event.ctrlKey || event.metaKey) && event.key === 'z' && !/INPUT|TEXTAREA|SELECT/.test(event.target?.tagName || '')) { event.preventDefault(); undo(); }
    }}>
    <div class="gm-head">
      <b class="gm-name">${name}</b>
      <span class="gq-dim">${dirty ? 'draft' : 'saved'}</span>
      <span class="gm-toggle" role="group" aria-label="View">
        <button type="button" data-mini-steps disabled=${!stepsList.ok} title=${stepsList.ok ? 'Show the same draft as a step list' : `The step list cannot show it: ${stepsList.reason}`}
          onClick=${() => onSteps({ list: stepsList.list, changed: dirty })}>☰ Steps</button>
        <button type="button" aria-pressed="true" class="on">◈ Graph</button>
      </span>
      ${dirty && html`<span class="gm-changes" data-mini-changes=${summary.length}>● ${summary.length || 1} unsaved change${summary.length === 1 ? '' : 's'}${summary.length ? `: ${summary.slice(0, 3).join(', ')}${summary.length > 3 ? '…' : ''}` : ''}</span>`}
      <span class="gq-spacer"></span>
      <button type="button" class="gq-btn" data-mini-undo disabled=${!model.current.canUndo} title="Undo (Ctrl+Z)" onClick=${undo}>↶ undo</button>
      <button type="button" class="gq-btn" data-mini-full disabled=${!!busy} title=${dirty ? 'Save, then open it in the full editor' : 'Open it in the full editor'} onClick=${fullEditor}>⤢ full editor</button>
      ${discarding
    ? html`<span class="gq-confirm">Discard ${summary.length || 'the'} change${summary.length === 1 ? '' : 's'}?
        <button type="button" class="gq-btn" data-mini-discard onClick=${onClose}>discard</button>
        <button type="button" class="gq-link" onClick=${() => setDiscarding(false)}>keep</button></span>`
    : html`<button type="button" class="gq-btn" data-mini-cancel onClick=${() => (dirty ? setDiscarding(true) : onClose())}>cancel</button>`}
      <button type="button" class="gq-btn primary" data-mini-save disabled=${!!busy || !dirty} title=${dirty ? 'Save as a new version' : 'Nothing changed'}
        onClick=${async () => { if (await save()) onClose(); }}>${busy === 'save' ? 'saving…' : 'Save new version'}</button>
    </div>
    <div class="gm-canvas" ref=${canvas} data-mini-canvas
      onMouseLeave=${() => { if (!drag) setHover(''); }}
      onPointerMove=${(event) => { if (drag) { const at = pointAt(event); setDrag({ ...drag, ...at, moved: drag.moved || Math.hypot(at.x - drag.x0, at.y - drag.y0) > DRAG_SLOP }); } }}
      onPointerUp=${(event) => {
        if (!drag) return;
        const onNode = event.target?.closest?.('[data-process-node]');
        if (drag.moved && !onNode) setPicker({ op: dropOp(drag.id), id: drag.id, cx: event.clientX || 0, cy: event.clientY || 0 });
        dragged.current = drag.moved;
        setDrag(null);
      }}
      onClick=${(event) => {
        if (event.target === canvas.current || event.target?.classList?.contains?.('pk-mini-graph')) { setSelected(''); setMenu(null); setPicker(null); }
      }}>
      <${ProcessMiniGraph} run=${projection} wizard=${wizard} selectedId=${selected}
        onNodeClick=${(node) => { setMenu(null); setPicker(null); setSelected(node.id === selected ? '' : node.id); }}
        onNodeContextMenu=${openMenu} onNodeHover=${setHover} overlay=${overlay} />
    </div>
    ${selected && html`<${Inspector} view=${draft} id=${selected} roster=${roster} roles=${roles}
      onApply=${(result) => apply(result)} onClose=${() => setSelected('')} onFullEditor=${fullEditor}
      onKind=${(kind, who) => run(() => {
        const result = changeKind(draft, selected, kind);
        return who ? updateStep(result.view, selected, (s) => setStepWho(s, who)) : result;
      })} />`}
    <div class="gm-foot">
      <span class="gq-dim">Saving makes a new version; runs already going keep theirs.</span>
      ${errors.length > 0 && html`<span class="gq-error" role="alert" data-mini-problems=${errors.length}>${errors.length} problem${errors.length === 1 ? '' : 's'}: ${errors[0].message}</span>`}
      <span class="gq-spacer"></span>
      ${!process.listed && html`<label class="gq-dim"><input type="checkbox" data-mini-publish checked=${publish}
        onChange=${(event) => setPublish(event.currentTarget.checked)} /> also publish to the library</label>`}
    </div>
    ${error && html`<div class="gq-error" role="alert">${error}</div>`}
  </section>`;
}

