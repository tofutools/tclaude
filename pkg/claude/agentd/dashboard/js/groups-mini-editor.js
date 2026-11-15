// groups-mini-editor.js -- the inline mini editor (mock M1, tcl-jqmrj5): a
// saved process opened in place in its group as a small graph. It is a
// constrained front end on the full editor's pieces, not a second editor:
// the draft lives in a ProcessEditModel (undo, redo, dirty, the CAS base),
// the graph is the kit's ProcessMiniGraph, and every structural edit is a
// pure process-mini-edit.js operation landed with model.applyDraft. Hover a
// step for + handles, click it for a compact inspector (the step list's
// fields), right-click it for structure, or drag a handle into empty space.
// ☰ Steps shows the same draft as the step list, ⤢ full editor opens it in
// Automations › Processes, and Save writes a new version. A new process
// (`draft`, from New process) opens here too: its name is a field, it saves
// itself as it changes, and while it has no steps the canvas offers the first
// one and a Start from strip (a starter, a description for the scribe, a
// .pwf, or a library process to copy).

import { h } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { useDismissablePopover } from './hover-popover.js';
import htm from 'htm';
import { ProcessMiniGraph } from './process-kit.js';
import { ProcessEditModel } from './process-edit-model.js';
import {
  addOutcome, branch, canBranch, canDelete, canInsertAfter, canInsertBefore, canRunAlongside, changeKind, changeSummary,
  deleteOrphans, deleteStep, draftProjection, duplicate, insertAfter, insertBefore, isStep, miniKind, outcomesOf,
  canRemoveOutcome, removeOutcome, renameOutcome, runAlongside, setLoopRounds, setOutcomeTarget, targetsFor, updateStep,
} from './process-mini-edit.js';
import { LOOP_MAX_ROUNDS, LOOP_MIN_ROUNDS, loopBound, normalizeRounds } from './process-loops.js';
import {
  agentStepIDs, collapseParamShorthand, constraintFromValue, constraintValue, outputRef, setStepConstraint, setStepName, setStepOutputs,
  setStepText, setStepWho, stepConstraint, stepListToView, stepOutputs, stepText, stepWho, viewToStepList,
} from './process-step-list.js';
import { openProcessDraft } from './process-draft-handoff.js';
import {
  AUTOSAVE_DELAY_MS, DRAFT_DESCRIPTION_MAX, FallbackPicker, UNTITLED_PROCESS, WhoPicker, defaultNotify, inheritLabel, useSpawnProfiles,
} from './groups-quickstart.js';
import { STARTERS, fallbackNote, setStepFallback, stepFallbackValue } from './process-quickstart.js';
import { withShortcut } from './keybindings.js';
import { Button, Select } from './ui-components.js';

const html = htm.bind(h);

const KIND_CHOICES = Object.freeze([['agent', '🤖', 'agent'], ['you', '👤', 'you'], ['command', '⌘', 'command']]);
const VALIDATE_DELAY_MS = 400;
// A pointer that moves this far from a handle is a drag, not a click.
const DRAG_SLOP = 8;

// whoLine is a step's sub line in the graph: who does it, and its fallback
// when that is not to wait.
function whoLine(node, fallbackInfo) {
  const who = stepWho({ node });
  const note = fallbackNote({ node }, fallbackInfo);
  const agent = (label) => (note ? `${label} ${note}` : label);
  if (who.mode === 'agent') return agent(who.agent);
  if (who.mode === 'role') return agent(`any ${who.role}`);
  if (who.mode === 'anyone') return agent('anyone free');
  if (who.mode === 'lead') return agent('the lead');
  if (who.mode === 'you') return 'you';
  if (who.mode === 'command') return stepText({ node }) || 'a command';
  return agent('pick who');
}

function viewOf(model) {
  const { template, edges, layout } = model.saveBody();
  return { template, edges, layout };
}

// FIRST_STEP is the placeholder an empty process shows between start and
// what follows it: a step-shaped "＋ Add first step" that the layout spaces
// like any step. It is drawn only; it is never part of the process.
const FIRST_STEP = 'mini-first-step';

// withFirstStep puts the placeholder after `startId` in a projection, taking
// over start's own edges, when startId is given.
function withFirstStep(projection, startId) {
  if (!startId) return projection;
  const after = projection.edges.filter((edge) => edge.from === startId);
  return {
    ...projection,
    nodes: [...projection.nodes, { id: FIRST_STEP, label: '＋ Add first step', state: 'planned', kind: 'agent', sub: 'or start from one below' }],
    edges: [
      ...projection.edges.filter((edge) => edge.from !== startId),
      { from: startId, to: FIRST_STEP, label: '' },
      ...after.map((edge) => ({ ...edge, from: FIRST_STEP, label: '' })),
    ],
  };
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
    ${KIND_CHOICES.map(([kind, glyph, text]) => html`<button data-ui-exempt key=${kind} type="button" class="gm-kind" data-mini-kind=${kind}
      disabled=${!!disabled} title=${disabled || `${label ? `${label}: ` : ''}${text}`} onClick=${() => onPick(kind)}>${glyph} ${text}</button>`)}
  </span>`;
}

// Inspector is the compact editor of one step: the step list's fields.
function Inspector({ view, id, roster, roles, fallbackInfo, onApply, onKind, onClose, onFullEditor }) {
  const node = view.template.nodes[id];
  if (!node) return null;
  const kind = miniKind(node);
  if (!isStep(node)) {
    return html`<div class="gm-inspector" data-mini-inspector=${id} role="dialog" aria-label=${`${node.name || id}`}>
      <div class="gm-row"><b>${node.name || id}</b><span class="gq-dim">${node.type}</span><span class="gq-spacer"></span>
        <${Button} size="sm" variant="link" class="gq-link" aria-label="Close" onClick=${onClose}>✕<//></div>
      ${kind === 'advanced'
    ? html`<p class="gq-dim">${node.type} nodes are edited in the full editor.</p>
        <${Button} size="sm" data-mini-full-node onClick=${onFullEditor}>⤢ edit in full editor<//>`
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
  const label = { agent: 'What', you: 'Asks', command: 'Runs' }[kind];
  const stepName = (stepID) => view.template.nodes[stepID]?.name || stepID;
  // Which agent: the same as, or not the one of, another agent step.
  const others = kind === 'agent' && stepWho(step).mode !== 'agent' ? agentStepIDs(view.template, id) : [];
  const constraint = constraintValue(stepConstraint(step));
  const constraintKnown = !constraint || others.some((other) => constraint === `same:${other}` || constraint === `different:${other}`);
  const outputs = stepOutputs(step);
  // What this step's text can read: the other steps' outputs.
  const readable = Object.entries(view.template.nodes).filter(([other]) => other !== id)
    .flatMap(([, other]) => other.captures || []);
  return html`<div class="gm-inspector" data-mini-inspector=${id} role="dialog" aria-label=${`Step ${node.name || id}`}
    onKeyDown=${(event) => { if (event.key === 'Escape') { event.stopPropagation(); onClose(); } }}>
    <label class="gm-field"><span>Step</span><input type="text" data-mini-name value=${node.name || ''} placeholder=${id}
      onChange=${(event) => onApply(updateStep(view, id, (s) => setStepName(s, event.currentTarget.value)))} /></label>
    <label class="gm-field"><span>${label}</span><textarea rows="3" data-mini-text value=${text}
      onChange=${(event) => onApply(updateStep(view, id, (s) => setStepText(s, event.currentTarget.value)))}></textarea></label>
    ${readable.length > 0 && kind !== 'command' && html`<p class="gq-dim gm-readable" data-mini-readable>Can use: ${readable.map((name, i) => html`${i > 0 && ', '}<code key=${name}>${outputRef(name)}</code>`)}</p>`}
    <div class="gm-field"><span>Who</span><${WhoPicker} step=${step} roster=${roster} roles=${roles} onChange=${setWho} /></div>
    ${others.length > 0 && html`<div class="gm-field" data-mini-constraint=${id}><span>Which agent</span><${Select} size="sm" aria-label="Which agent does this step"
      value=${constraint} onChange=${(event) => onApply(updateStep(view, id, (s) => setStepConstraint(s, constraintFromValue(event.currentTarget.value))))}>
      <option value="">whoever fits</option>
      ${!constraintKnown && html`<option value=${constraint}>${constraint.startsWith('same:') ? `same as ${stepName(constraint.slice(5))}` : `not the agent of ${constraint.slice(10).split(',').map(stepName).join(', ')}`}</option>`}
      <optgroup label="The same agent as">${others.map((other) => html`<option key=${`same:${other}`} value=${`same:${other}`}>${stepName(other)}</option>`)}</optgroup>
      <optgroup label="Not the agent of">${others.map((other) => html`<option key=${`different:${other}`} value=${`different:${other}`}>${stepName(other)}</option>`)}</optgroup>
    <//></div>`}
    ${kind === 'agent' && html`<div class="gm-field" data-mini-fallback=${id}><span>If nobody</span><${FallbackPicker} value=${stepFallbackValue(step)}
      label="If no member can take this step" inherit=${inheritLabel(fallbackInfo.fallback, fallbackInfo.defaultSpawnProfile)}
      defaultSpawnProfile=${fallbackInfo.defaultSpawnProfile} profiles=${fallbackInfo.profiles}
      onChange=${(next) => onApply(updateStep(view, id, (s) => setStepFallback(s, next)))} /></div>`}
    ${kind === 'agent' && node.type === 'task' && html`<label class="gm-field" data-mini-outputs=${id}><span>Outputs</span><input type="text"
      value=${outputs.join(', ')} placeholder="none (e.g. pr)" title="Names this step reports, which later steps read as {{ outputs.<name> }}"
      onChange=${(event) => onApply(updateStep(view, id, (s) => setStepOutputs(s, event.currentTarget.value)))} /></label>`}
    <div class="gm-field"><span>${node.type === 'decision' ? 'Outcomes' : 'Then'}</span><div class="gm-outcomes">
      ${outcomes.map(({ outcome, to }) => html`<span key=${outcome} class="gm-outcome" data-mini-outcome=${outcome}>
        ${node.type === 'decision'
    ? html`<input type="text" class="gm-outcome-name" aria-label=${`Rename ${outcome}`} value=${outcome}
          onChange=${(event) => onApply(renameOutcome(view, id, outcome, event.currentTarget.value))} />`
    : ''} →
        <${Select} size="sm" aria-label=${`After ${outcome}`} value=${to} onChange=${(event) => onApply(setOutcomeTarget(view, id, outcome, event.currentTarget.value))}>
          ${targetsFor(view, id, outcome).map((target) => html`<option key=${target.id} value=${target.id} data-mini-back=${target.back ? '' : undefined}>${target.back ? `↺ ${target.label} (again)` : target.label}</option>`)}
        <//>
        ${loopBound(node, outcome) && html`<label class="ui-tag ui-tag-warn gm-loop" data-mini-loop=${outcome} title="Going back runs that work again; past the last round this outcome is refused">
          ≤<input type="number" min=${LOOP_MIN_ROUNDS} max=${LOOP_MAX_ROUNDS} aria-label=${`Rounds for ${outcome}`} value=${loopBound(node, outcome).maxIterations}
            onChange=${(event) => { const rounds = normalizeRounds(event.currentTarget.value); if (rounds) onApply(setLoopRounds(view, id, outcome, rounds)); else event.currentTarget.value = String(loopBound(node, outcome).maxIterations); }} /> rounds</label>`}
        ${node.type === 'decision' && !canRemoveOutcome(view, id, outcome) && html`<${Button} size="sm" icon=${true} variant="ghost danger" aria-label=${`Remove ${outcome}`}
          onClick=${() => onApply(removeOutcome(view, id, outcome))}>✕<//>`}
      </span>`)}
      ${node.type === 'decision' && html`<${Button} size="sm" icon=${true} class="gm-add-outcome" data-mini-add-outcome title="Add an outcome"
        onClick=${() => onApply(addOutcome(view, id, outcomes.find((o) => !loopBound(node, o.outcome))?.to || targets.find((t) => view.template.nodes[t.id]?.type === 'end')?.id || targets[0]?.id))}>+<//>`}
      ${!outcomes.length && html`<span class="gq-dim">nothing yet: hover the step for + after</span>`}
    </div></div>
    <div class="gm-row"><${Button} size="sm" variant="link" class="gq-link gm-more" data-mini-more onClick=${onFullEditor}
      title="Retry, model and effort, plan/check/review stages and the rest are edited in the full editor">More options in ⤢ Full editor<//>
      <span class="gq-spacer"></span><${Button} size="sm" variant="link" class="gq-link" onClick=${onClose}>done<//></div>
  </div>`;
}

// StartFrom is the strip under a new process's empty canvas: what it can
// start from instead of a first step.
function StartFrom({ roster, api, wizard, busy, onView, onDescribe, onImport }) {
  const [open, setOpen] = useState('');
  const [text, setText] = useState('');
  const [templates, setTemplates] = useState(null);
  useEffect(() => {
    if (open !== 'library' || templates) return undefined;
    let live = true;
    api.listTemplates().then((rows) => { if (live) setTemplates(rows); }).catch(() => { if (live) setTemplates([]); });
    return () => { live = false; };
  }, [open]);
  const starter = (item) => {
    const built = item.build(roster);
    onView(built.view || stepListToView(built.list), item.title);
  };
  const describe = (event) => {
    event.preventDefault();
    if (text.trim() && !busy) onDescribe(text.trim());
  };
  const chip = (attrs, label, title, onClick, on = false) => html`<button data-ui-exempt type="button" class=${`gm-from${on ? ' on' : ''}`} ...${attrs}
    disabled=${!!busy} title=${title} onClick=${onClick}>${label}</button>`;
  return html`<div class="gm-start-from" data-mini-start-from>
    <div class="gm-start-row">
      <span class="gq-dim">Start from</span>
      ${STARTERS.map((item) => chip({ key: item.key, 'data-mini-starter': item.key }, item.chain.map(([, label]) => label).join(' → '),
    `${item.title}: ${item.note}`, () => starter(item)))}
      ${chip({ 'data-mini-describe': '' }, '✨ Describe it…', wizard ? 'Tell a scribe the rite in plain words' : 'Describe it in plain words and a process scribe drafts the steps', () => setOpen(open === 'describe' ? '' : 'describe'), open === 'describe')}
      ${onImport && chip({ 'data-mini-import': '' }, '↥ Import .pwf', 'Import a shared .pwf workflow file: you review what it runs first', onImport)}
      ${chip({ 'data-mini-library': '' }, wizard ? 'More written rites ›' : 'More ways of working ›', 'Copy a process from the template library', () => setOpen(open === 'library' ? '' : 'library'), open === 'library')}
    </div>
    ${open === 'describe' && html`<form class="gq-ask gm-describe" data-mini-describe-form onSubmit=${describe}>
      <input type="text" class="gq-ask-in" aria-label="Describe the process in plain words" maxlength=${DRAFT_DESCRIPTION_MAX} value=${text} disabled=${!!busy}
        placeholder="e.g. implement the issue, have the reviewer check it, I approve, then merge" onInput=${(event) => setText(event.currentTarget.value)} />
      <${Button} size="sm" variant="primary" type="submit" disabled=${!!busy || !text.trim()}
        title="A process scribe drafts the steps in the background. It uses a model run; you approve its access first, and nothing runs until you start it.">✨ Draft steps<//>
    </form>`}
    ${open === 'library' && html`<div class="gm-start-row gm-library" data-mini-library-list>
      ${templates === null && html`<span class="gq-dim">Loading the template library…</span>`}
      ${templates?.length === 0 && html`<span class="gq-dim">The template library is empty.</span>`}
      ${(templates || []).map((template) => chip({ key: template.id, 'data-mini-library-pick': template.id }, `▣ ${template.name || template.id}`,
    template.description || `Copy ${template.name || template.id} into this process`,
    () => void api.getTemplate(template.id).then((view) => onView(view, view?.template?.name || template.name)).catch(() => {})))}
    </div>`}
  </div>`;
}

// StructureMenu is the right-click menu of one node.
function StructureMenu({ view, menu, onRun, onWrap, onClose }) {
  const rootRef = useRef(null);
  useDismissablePopover(rootRef, true, onClose);
  const { id } = menu;
  const node = view.template.nodes[id];
  if (!node) return null;
  const step = isStep(node);
  const orphans = step || node.type === 'end' ? deleteOrphans(view, id) : [];
  const item = (key, text, why, onClick) => html`<button data-ui-exempt type="button" role="menuitem" class="gm-menu-item" data-mini-menu=${key}
    disabled=${!!why} title=${why} onClick=${onClick}>${text}</button>`;
  const kinds = (key, text, why, op) => html`<div class="gm-menu-kinds" data-mini-menu=${key}><span class=${why ? 'gm-off' : ''} title=${why}>${text}</span>
    <${KindChips} label=${text} disabled=${why} onPick=${(kind) => onRun(() => op(view, id, kind))} /></div>`;
  return html`<div ref=${rootRef} class="gm-menu" role="menu" aria-label=${`Structure of ${node.name || id}`} style=${floating(menu, 400, 330)}>
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
  notify = defaultNotify, onClose, onSteps, draft: isDraft = false, untouched = false, onRun = null, onDiscard = null,
  onImport = null, onDescribe = null,
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
  // autosave is how a draft's own saving goes: '', 'saving', 'saved' or 'failed'.
  const [autosave, setAutosave] = useState('');
  // touched: the operator has changed this draft since it was made.
  const touched = useRef(!untouched);
  const inflight = useRef(null);
  const failedAt = useRef(null);
  const nameRef = useRef(null);
  const canvas = useRef(null);
  // dragged swallows the click that ends a handle drag.
  const dragged = useRef(false);
  const draft = useMemo(() => viewOf(model.current), [rev]);
  const draftKey = JSON.stringify(draft);
  const dirty = model.current.dirty;
  const summary = dirty ? changeSummary(stored, draft) : [];
  const name = draft.template.name || process.name;
  const [title, setTitle] = useState(name);
  // A new process opens with its name ready to type over.
  useEffect(() => {
    if (!isDraft || !untouched) return;
    nameRef.current?.focus?.();
    nameRef.current?.select?.();
  }, []);
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
  const profiles = useSpawnProfiles(api);
  const fallbackInfo = { fallback: draft.template.fallback || null, defaultSpawnProfile: String(groupInfo?.defaultSpawnProfile || ''), profiles };
  // apply lands one operation's result as one undoable edit.
  const apply = (result, focus = undefined) => {
    touched.current = true;
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
  const save = async ({ quiet = false } = {}) => {
    // A click during an autosave waits for it, rather than being dropped.
    if (inflight.current) {
      const ok = await inflight.current;
      if (!ok || !model.current.dirty) return ok;
    }
    if (inflight.current) return false;
    let done;
    inflight.current = new Promise((resolve) => { done = resolve; });
    setBusy(quiet ? 'auto' : 'save');
    setError('');
    const savedAt = model.current.rev;
    try {
      const body = await api.saveTemplate(process.templateId, draft, { sourceHash: model.current.sourceHash, ...(publish ? { listed: true } : {}) });
      model.current.markSaved(body, savedAt);
      if (!quiet) notify(isDraft ? `Saved ${name}` : `Saved ${name} as a new version`);
      void source?.refresh?.();
      return true;
      done(true);
      return true;
    } catch (cause) {
      setError(/head changed/.test(cause.message)
        ? `${name} was saved elsewhere since you opened it. ${isDraft ? 'Close it without saving' : 'Cancel'} and reopen it to edit the newest version.` : cause.message);
      done(false);
      return false;
    } finally {
      inflight.current = null;
      setBusy('');
    }
  };
  // A draft saves itself a moment after each change; a change made while a
  // save is under way is saved after it.
  // One that fails is not retried until the next change.
  useEffect(() => {
    if (!isDraft || !dirty || busy || failedAt.current === draftKey) return undefined;
    const timer = setTimeout(() => {
      setAutosave('saving');
      void save({ quiet: true }).then((ok) => {
        failedAt.current = ok ? null : draftKey;
        setAutosave(ok ? 'saved' : 'failed');
      });
    }, AUTOSAVE_DELAY_MS);
    return () => clearTimeout(timer);
  }, [isDraft, draftKey, dirty, busy]);
  // The full editor opens over this tab, pinned to the group still: its
  // ← Back inline brings the process back here.
  const fullEditor = async () => {
    if (dirty && !(await save({ quiet: isDraft }))) return;
    await openProcessDraft({ id: process.templateId, name, group: group.name });
    onClose();
  };
  const commitTitle = (value) => {
    const next = String(value || '').trim() || UNTITLED_PROCESS;
    setTitle(next);
    if (next !== draft.template.name) apply({ view: { ...draft, template: { ...draft.template, name: next } } });
  };
  const empty = !Object.values(draft.template.nodes || {}).some(isStep);
  // startFrom fills an empty draft with a starter's or a library process's
  // graph, under this draft's id, named after it while still untitled.
  const startFrom = (from, label) => {
    if (!from?.template) return;
    const keep = draft.template.name && draft.template.name !== UNTITLED_PROCESS;
    const next = keep ? draft.template.name : (label || from.template.name || UNTITLED_PROCESS);
    setTitle(next);
    apply({ view: { template: { ...from.template, id: draft.template.id, name: next }, edges: from.edges || [], layout: from.layout || {} } });
  };
  // Closing a draft keeps it (saved first); one never touched is deleted.
  const closeDraft = async () => {
    if (!touched.current && empty && onDiscard) { onDiscard(process.templateId); return; }
    if (dirty && !(await save({ quiet: true }))) return;
    onClose();
  };
  const runDraft = async () => {
    if (dirty && !(await save({ quiet: true }))) return;
    onRun?.(process.templateId, name);
  };
  const describe = async (text) => {
    if (dirty && !(await save({ quiet: true }))) return;
    onDescribe?.(text, { id: process.templateId, sourceHash: model.current.sourceHash, ref: model.current.currentRef, view: draft });
  };
  const importPwf = () => {
    onImport?.();
    if (!touched.current && empty && onDiscard) onDiscard(process.templateId);
    else onClose();
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
  const handle = (op, id, x, y, glyph, text, why) => html`<button type="button" data-ui-exempt class=${`gm-handle gm-handle-${op}`} data-mini-handle=${op}
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
        <${Button} size="sm" variant="link" class="gq-link" aria-label="Close" onClick=${() => setPicker(null)}>✕<//>
      </div>`}
      ${menu && html`<${StructureMenu} view=${draft} menu=${menu} onRun=${run} onClose=${() => setMenu(null)}
        onWrap=${() => { setMenu(null); void fullEditor(); }} />`}`;
  };
  const startId = Object.keys(draft.template.nodes || {}).find((id) => draft.template.nodes[id]?.type === 'start') || '';
  const projection = withFirstStep(draftProjection(draft, { whoOf: (id, node) => whoLine(node, fallbackInfo), noteOf: (id, node) => fallbackNote({ node }, fallbackInfo), name }), empty ? startId : '');
  // The placeholder opens the kind picker for a step after start.
  const addFirst = () => {
    const box = canvas.current?.querySelector?.(`[data-process-node="${FIRST_STEP}"]`)?.getBoundingClientRect?.() || { left: 0, bottom: 0 };
    setMenu(null);
    setPicker({ op: 'after', id: startId, cx: box.left, cy: box.bottom + 4 });
  };
  return html`<section class="gm-card ui-card ui-inset" aria-label=${`Edit ${name} of ${group.name}`} data-mini-editor=${process.templateId}
    onKeyDown=${(event) => {
      if (event.key === 'Escape' && (menu || picker)) { event.stopPropagation(); setMenu(null); setPicker(null); }
      if ((event.ctrlKey || event.metaKey) && event.key === 'z' && !/INPUT|TEXTAREA|SELECT/.test(event.target?.tagName || '')) { event.preventDefault(); undo(); }
    }}>
    <div class="gm-head">
      ${isDraft
    ? html`<input type="text" class="gm-title" data-mini-title ref=${nameRef} aria-label="Process name" maxlength="120" value=${title}
        onInput=${(event) => setTitle(event.currentTarget.value)} onChange=${(event) => commitTitle(event.currentTarget.value)}
        onKeyDown=${(event) => { if (event.key === 'Enter') { event.preventDefault(); event.currentTarget.blur(); } }} />
      <span class="ui-tag gm-autosave" data-mini-autosave=${autosave || (dirty ? 'pending' : 'saved')} title=${autosave === 'failed' ? error : ''}>${
        autosave === 'saving' || busy === 'auto' ? 'draft · saving…' : autosave === 'failed' ? 'draft · not saved' : `draft · autosaved to ${group.name}`}</span>`
    : html`<b class="gm-name">${name}</b>
      <span class="gq-dim">${dirty ? 'draft' : 'saved'}</span>`}
      <span class="gm-toggle" role="group" aria-label="View">
        <button data-ui-exempt type="button" aria-pressed="true" class="on">◈ Graph</button>
        <button type="button" data-ui-exempt data-mini-steps disabled=${!stepsList.ok} title=${stepsList.ok ? 'Show the same draft as a step list' : `The step list cannot show it: ${stepsList.reason}`}
          onClick=${() => onSteps({ list: stepsList.list, changed: dirty, sourceHash: model.current.sourceHash, ref: model.current.currentRef })}>☰ Steps</button>
      </span>
      ${dirty && !isDraft && html`<span class="gm-changes" data-mini-changes=${summary.length}>● ${summary.length || 1} unsaved change${summary.length === 1 ? '' : 's'}${summary.length ? `: ${summary.slice(0, 3).join(', ')}${summary.length > 3 ? '…' : ''}` : ''}</span>`}
      <span class="gq-spacer"></span>
      <${Button} size="sm" data-mini-undo disabled=${!model.current.canUndo} title=${withShortcut('Undo', 'mini-undo')} onClick=${undo}>↶ undo<//>
      <${Button} size="sm" data-mini-full disabled=${!!busy && busy !== 'auto'} title=${dirty ? 'Save, then open it in the full editor' : 'Open it in the full editor'} onClick=${fullEditor}>⤢ ${isDraft ? 'Full editor' : 'full editor'}<//>
      ${isDraft
    ? html`<${Button} size="sm" variant="primary" data-mini-save disabled=${!!busy && busy !== 'auto'} title="Save it now and close the editor"
        onClick=${() => void closeDraft()}>Save<//>
      ${onRun && html`<${Button} size="sm" data-mini-run disabled=${(!!busy && busy !== 'auto') || empty} title=${empty ? 'Add a step first' : `Save it, then pick how to run it in ${group.name}`}
        onClick=${() => void runDraft()}>▶ Run…<//>`}
      <${Button} size="sm" icon=${true} variant="ghost" data-mini-close aria-label="Close" title=${!touched.current && empty ? 'Close: an untouched new process is not kept' : 'Close: it stays saved in the group'}
        onClick=${() => void closeDraft()}>✕<//>
      ${autosave === 'failed' && html`<${Button} size="sm" variant="link" class="gq-link" data-mini-drop title="Close it; the changes since the last save are lost"
        onClick=${onClose}>close without saving<//>`}`
    : html`${discarding
    ? html`<span class="gq-confirm">Discard ${summary.length || 'the'} change${summary.length === 1 ? '' : 's'}?
        <${Button} size="sm" variant="danger" data-mini-discard onClick=${onClose}>discard<//>
        <${Button} size="sm" variant="link" class="gq-link" onClick=${() => setDiscarding(false)}>keep<//></span>`
    : html`<${Button} size="sm" data-mini-cancel onClick=${() => (dirty ? setDiscarding(true) : onClose())}>cancel<//>`}
      <${Button} size="sm" variant="primary" data-mini-save disabled=${!!busy || !dirty} title=${dirty ? 'Save as a new version' : 'Nothing changed'}
        onClick=${async () => { if (await save()) onClose(); }}>${busy === 'save' ? 'saving…' : 'Save new version'}<//>`}
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
        onNodeClick=${(node) => { if (node.id === FIRST_STEP) { addFirst(); return; } setMenu(null); setPicker(null); setSelected(node.id === selected ? '' : node.id); }}
        onNodeContextMenu=${(node, event) => (node.id === FIRST_STEP ? event.preventDefault() : openMenu(node, event))}
        onNodeHover=${(id) => setHover(id === FIRST_STEP ? '' : id)} overlay=${overlay} />
    </div>
    ${isDraft && empty && html`<${StartFrom} roster=${roster} api=${api} wizard=${wizard} busy=${busy && busy !== 'auto' ? busy : ''}
      onView=${startFrom} onDescribe=${onDescribe ? (text) => void describe(text) : null} onImport=${onImport ? importPwf : null} />`}
    ${selected && html`<${Inspector} view=${draft} id=${selected} roster=${roster} roles=${roles} fallbackInfo=${fallbackInfo}
      onApply=${(result) => apply(result)} onClose=${() => setSelected('')} onFullEditor=${fullEditor}
      onKind=${(kind, who) => run(() => {
        const result = changeKind(draft, selected, kind);
        return who ? updateStep(result.view, selected, (s) => setStepWho(s, who)) : result;
      })} />`}
    <div class="gm-foot">
      <span class="gq-dim">${isDraft ? 'It saves itself as you go; runs already going keep their version.' : 'Saving makes a new version; runs already going keep theirs.'}</span>
      ${errors.length > 0 && html`<span class="gq-error ui-error" role="alert" data-mini-problems=${errors.length}>${errors.length} problem${errors.length === 1 ? '' : 's'}: ${errors[0].message}</span>`}
      <span class="gq-spacer"></span>
      ${!process.listed && html`<label class="gq-dim"><input type="checkbox" data-mini-publish checked=${publish}
        onChange=${(event) => setPublish(event.currentTarget.checked)} /> also publish to the library</label>`}
    </div>
    ${error && html`<div class="gq-error ui-error" role="alert">${error}</div>`}
  </section>`;
}

