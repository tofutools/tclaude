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
// one and a Start from strip (a built-in way of working, a description for the scribe, a
// .pwf, or a library process to copy).

import { h } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { shellConfirmDiscard } from './shell-state.js';
import { useDismissablePopover } from './hover-popover.js';
import htm from 'htm';
import { ProcessMiniGraph } from './process-kit.js';
import { ProcessEditModel } from './process-edit-model.js';
import {
  addOutcome, branch, branchKinds, canBranch, canDelete, canInsertAfter, canInsertBefore, canRunAlongside, changeKind, changeSummary,
  deleteOrphans, deleteStep, draftProjection, duplicate, insertAfter, insertBefore, isStep, miniKind, outcomesOf,
  canRemoveOutcome, removeOutcome, renameOutcome, runAlongside, REVIEWER_TRACK, canAddReviewerTrack, reviewerRoleOf, reviewLoop, setLoopRounds, setOutcomeTarget, targetsFor, updateStep,
  addDecider, canAddDecider, gateDeciders, removeDecider, setAgree, setDecider,
  exhaustionOptions, setDecisionWho, setExhaustion, declareParams, setParams, insertOnAnswer, ruleOperands, setRule,
} from './process-mini-edit.js';
import { LOOP_MAX_ROUNDS, LOOP_MIN_ROUNDS, loopBound, normalizeRounds } from './process-loops.js';
import { RuleCases } from './process-rule-cases.js';
import { FailureRow } from './process-failure-row.js';
import {
  agentStepIDs, collapseParamShorthand, expandParamShorthand, constraintFromValue, constraintValue, outputRef, setStepConstraint, setStepName, setStepOutputs,
  FAILURE_RETRIES_MAX, setStepFailure, setStepText, setStepWho, stepConstraint, stepFailure, stepOutputs, stepText, stepWho, viewToStepList,
} from './process-step-list.js';
import { openProcessDraft } from './process-draft-handoff.js';
import { followName } from './process-step-ids.js';
import {
  deciderOwnAsk, deciderSummary, gateAsk, setDeciderAsk, setDeciderFallback, setDeciderNot, setGateAsk, spawnIfNobody,
  addReviewer, stuckGate, withReviewerDefault,
} from './process-gate-deciders.js';
import {
  AUTOSAVE_DELAY_MS, DRAFT_DESCRIPTION_MAX, FallbackPicker, TypedField, UNTITLED_PROCESS, WhoPicker, defaultNotify, inheritLabel, useSpawnProfiles,
} from './groups-quickstart.js';
import { fallbackNote, groupRoles, setStepFallback, stepFallbackValue } from './process-quickstart.js';
import { withShortcut } from './keybindings.js';
import { Button, Select } from './ui-components.js';
import { GetHelpMenu } from './get-help-menu.js';
import { openHelperWindow } from './get-help.js';
import {
  PROCESS_SCRIBE_CHAT_SUBJECT, PROCESS_SCRIBE_GRANT_META, PROCESS_SCRIBE_NAME, PROCESS_SCRIBE_SCOPE_KIND, PROCESS_SCRIBE_SLUGS,
  processScribeBrief, processScribeGrantBody, processScribeHandoff, processScribeTaskRef,
} from './process-scribe.js';
import { templateHeadFromEditView } from './process-external-change.js';
import { processActorPresentation } from './processes-actions.js';
import { dashboardState } from './snapshot-store.js';
import {
  EDITING_IDLE_MS, FRESH_MS, LIVE_POLL_MS, changeList, freshNodeIds, inlineHelpNote, mergeIncoming, stopAgent,
} from './groups-mini-live.js';

const html = htm.bind(h);

// agentStatus is an agent's status ('working' while in a turn) as the
// snapshot has it, from its own row or a group's member row.
function agentStatus(snapshot, agentId) {
  if (!agentId) return '';
  const rows = [...(snapshot?.agents || []), ...(snapshot?.groups || []).flatMap((group) => group.members || [])];
  return String(rows.find((row) => row?.agent_id === agentId)?.state?.status || '');
}

const KIND_CHOICES = Object.freeze([['agent', '🤖', 'agent'], ['you', '👤', 'you'], ['command', '⌘', 'command']]);
// INSERT_CHOICES adds a decision (yes / no, an agent decides until you pick
// otherwise) to what a new step can be.
const INSERT_CHOICES = Object.freeze([...KIND_CHOICES, ['decide', '◇', 'decision']]);
// BRANCH_TASK_CHOICES are ⑂'s choices on a task, by kind (branchKinds says
// which apply): its own agent decides, or a decision goes after it.
const BRANCH_TASK_CHOICES = Object.freeze({
  review: ['↺', 'its agent decides (approved / changes)'],
  decide: ['🤖', 'an agent decides next'],
  'decide-you': ['👤', 'you decide next'],
});
// REVIEWER_CHOICE is the reviewer-track preset: a review an agent decides
// and a fix it sends the work back to, run alongside any other reviewer
// tracks of the same work and joined before what comes next. + after offers
// it on the step whose work is reviewed, ⫴ alongside on a review track.
const REVIEWER_CHOICE = Object.freeze([REVIEWER_TRACK, '↺', 'reviewer track']);

function afterChoices(view, id) {
  return canAddReviewerTrack(view, id) ? INSERT_CHOICES : [...INSERT_CHOICES, REVIEWER_CHOICE];
}

function alongsideChoices(view, id) {
  return reviewLoop(view, id) && !canAddReviewerTrack(view, id) ? [...KIND_CHOICES, REVIEWER_CHOICE] : KIND_CHOICES;
}

const VALIDATE_DELAY_MS = 400;
// MAX_REFUSED is how many saves in a row a newer version may refuse before
// the editor stops merging and saving again, and says so.
const MAX_REFUSED = 3;

// wholeChoice is the merge when the draft's base is unknown (it opened with
// unsaved edits of an older version): nothing is merged unasked, and the
// operator takes theirs or keeps theirs out as a whole.
function wholeChoice(ours, theirs, choice) {
  const whole = [{ id: 'whole', kind: 'template', op: 'changed', label: 'the whole process', conflict: true }];
  if (choice === 'theirs') return { changes: whole, conflicts: whole, merged: { template: theirs.template, edges: theirs.edges, layout: theirs.layout }, clean: true };
  if (choice === 'mine') return { changes: whole, conflicts: whole, merged: ours, clean: false };
  return { changes: whole, conflicts: whole, merged: null, clean: false };
}
// A pointer that moves this far from a handle is a drag, not a click.
const DRAG_SLOP = 8;

// whoLine is a step's sub line in the graph: who does it, and its fallback
// when that is not to wait.
function whoLine(node, fallbackInfo) {
  if (node.deciders?.length) return `${node.deciders.length} deciders · all ${node.agree || '…'}`;
  const who = stepWho({ node });
  const note = fallbackNote({ node }, fallbackInfo);
  const agent = (label) => (note ? `${label} ${note}` : label);
  if (who.mode === 'agent') return agent(who.agent);
  if (who.mode === 'role') return agent(`any ${who.role}`);
  if (who.mode === 'anyone') return agent('anyone free');
  if (who.mode === 'lead') return agent('the lead');
  if (who.mode === 'spawn') return `a new agent · ${who.profile || fallbackInfo?.defaultSpawnProfile || 'group default'}`;
  if (who.mode === 'you') return 'you';
  if (who.mode === 'rule') return '⚙ a rule';
  if (who.mode === 'command') return stepText({ node }) || 'a command';
  if (node.performer?.sameAs) return agent(`same agent as ${node.performer.sameAs}`);
  return agent('pick who');
}

// branchChoices are the chips ⑂ offers on id: on a task, how it branches
// (a decision after it, or its agent deciding); on a decision, what the new
// outcome leads to.
function branchChoices(view, id) {
  const kinds = branchKinds(view, id);
  return kinds ? kinds.map((kind) => [kind, ...BRANCH_TASK_CHOICES[kind]]) : INSERT_CHOICES;
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

// KindChips is a row of agent / you / command (/ decision) buttons.
function KindChips({ onPick, disabled = '', label = '', choices = KIND_CHOICES }) {
  return html`<span class="gm-kinds" role="group" aria-label=${label || 'Pick a kind'}>
    ${choices.map(([kind, glyph, text]) => html`<button data-ui-exempt key=${kind} type="button" class="gm-kind" data-mini-kind=${kind}
      disabled=${!!disabled} title=${disabled || `${label ? `${label}: ` : ''}${text}`} onClick=${() => onPick(kind)}>${glyph} ${text}</button>`)}
  </span>`;
}

// DeciderRow is one decider of a gate: who, and a ▸ that opens its own ask,
// "If nobody" and "Not" in place. Closed, it says what is set ("not
// implement").
function DeciderRow({ view, id, index, decider, shared, open, onToggle, roster, roles, fallbackInfo, onApply }) {
  const node = view.template.nodes[id];
  const agent = decider.kind === 'agent';
  const summary = deciderSummary(view, id, index, shared);
  const stepName = (stepID) => view.template.nodes[stepID]?.name || stepID;
  // A decider naming one agent takes no "not" (the validator refuses both).
  const others = agent && !decider.assignee ? agentStepIDs(view.template, id) : [];
  const not = decider.differentFrom || [];
  const notValue = decider.sameAs ? `same:${decider.sameAs}` : not.join(',');
  const fallbackValue = agent ? stepFallbackValue({ node: { performer: decider } }) : '';
  const panel = `gm-decider-${id}-${index}`;
  return html`<div class=${`gm-decider${open ? ' is-open' : ''}`} data-mini-decider=${index}>
    <div class="gm-decider-row">
      <${Button} size="sm" icon=${true} variant="ghost" class="gm-decider-toggle" data-mini-decider-toggle=${index}
        aria-expanded=${open ? 'true' : 'false'} aria-controls=${panel}
        aria-label=${`${open ? 'Close' : 'Open'} decider ${index + 1}'s settings`} onClick=${onToggle}>${open ? '▾' : '▸'}<//>
      <${WhoPicker} step=${{ id: `${id}.vote.${index + 1}`, node: { type: 'decision', performer: decider } }} roster=${roster} roles=${roles} spawn=${false}
        label=${`Who casts vote ${index + 1}`}
        onChange=${(who) => {
    if (who.mode === 'command') return;
    const changed = setDecider(view, id, index, who);
    onApply(decider.kind === 'agent' ? changed : withReviewerDefault(changed.view, id, index));
  }} />
      ${!open && summary && html`<span class="gq-dim gm-decider-summary" data-mini-decider-summary=${index}>${summary}</span>`}
      <${Button} size="sm" icon=${true} variant="ghost danger" aria-label=${`Remove decider ${index + 1}`}
        onClick=${() => onApply(removeDecider(view, id, index))}>✕<//>
    </div>
    ${open && html`<div class="gm-decider-panel" id=${panel} data-mini-decider-panel=${index}>
      <label class="gm-field"><span>Its own ask</span><${TypedField} as="textarea" name="gm-decider-ask" rows="2" data-mini-decider-ask=${index}
        value=${collapseParamShorthand(deciderOwnAsk(node, index, shared))} placeholder="the same as Asks"
        onChange=${(event) => onApply(setDeciderAsk(view, id, index, event.currentTarget.value, shared))} /></label>
      ${agent && html`<div class="gm-field" data-mini-decider-fallback=${index}><span>If nobody</span><${FallbackPicker} value=${fallbackValue}
        label=${`If no member can cast vote ${index + 1}`} inherit=${inheritLabel(fallbackInfo.fallback, fallbackInfo.defaultSpawnProfile)}
        defaultSpawnProfile=${fallbackInfo.defaultSpawnProfile} profiles=${fallbackInfo.profiles}
        onChange=${(next) => onApply(setDeciderFallback(view, id, index, next))} /></div>`}
      ${agent && others.length > 0 && html`<div class="gm-field" data-mini-decider-not=${index}><span>Not</span><${Select} size="sm"
        aria-label=${`Vote ${index + 1} is not cast by`} value=${notValue}
        onChange=${(event) => { const picked = event.currentTarget.value; if (!picked.startsWith('same:')) onApply(setDeciderNot(view, id, index, picked.split(','))); }}>
        <option value="">anyone who fits</option>
        ${decider.sameAs && html`<option value=${notValue}>the same agent as ${stepName(decider.sameAs)}</option>`}
        ${not.length > 1 && html`<option value=${notValue}>not the agents of ${not.map(stepName).join(', ')}</option>`}
        ${others.map((other) => html`<option key=${other} value=${other}>the agent who did ${stepName(other)}</option>`)}
      <//></div>`}
    </div>`}
  </div>`;
}

// GateDeciders lists a gate's deciders, one row each, and warns when the gate
// would wait forever: more agent deciders than members who can vote, and one
// of them does not spawn.
function GateDeciders({ view, id, shared, roster, roles, fallbackInfo, noMoreDeciders, onApply }) {
  const deciders = gateDeciders(view.template.nodes[id]);
  const [open, setOpen] = useState({ id, rows: {} });
  const rows = open.id === id ? open.rows : {};
  const toggle = (index) => setOpen({ id, rows: { ...rows, [index]: !rows[index] } });
  const stuck = stuckGate(view, id, roster || [], fallbackInfo.fallback, fallbackInfo.defaultSpawnProfile);
  const fix = stuck && spawnIfNobody(fallbackInfo);
  const ordinal = (n) => ['first', 'second', 'third', 'fourth', 'fifth', 'sixth', 'seventh', 'eighth'][n] || `vote ${n + 1}`;
  return html`<div class="gm-field" data-mini-deciders=${id}><span>Deciders</span><div class="gm-deciders">
    ${deciders.map((decider, index) => html`<${DeciderRow} key=${index} view=${view} id=${id} index=${index} decider=${decider} shared=${shared}
      open=${!!rows[index]} onToggle=${() => toggle(index)} roster=${roster} roles=${roles} fallbackInfo=${fallbackInfo}
      onApply=${(change) => { if (change.view !== view && deciders.length !== change.view.template.nodes[id]?.deciders?.length) setOpen({ id, rows: {} }); onApply(change); }} />`)}
    <div><${Button} size="sm" icon=${true} class="gm-add-outcome" data-mini-add-decider title=${noMoreDeciders || 'Add a decider'} disabled=${!!noMoreDeciders}
      onClick=${() => onApply(addReviewer(view, id))}>+<//></div>
    ${stuck && html`<p class="gm-stuck" role="status" data-mini-stuck=${stuck.index}>${`⚠ ${stuck.deciders} agent deciders but ${stuck.members} ${stuck.noun} in the group: the ${ordinal(stuck.index)} vote waits forever.`}${fix
    ? html` <${Button} size="sm" variant="link" class="gq-link" data-mini-stuck-fix
        onClick=${() => onApply(setDeciderFallback(view, id, stuck.index, fix))}>Spawn one if nobody<//> (${fix.profile ? `with ${fix.profile}, ` : ''}on decider ${stuck.index + 1})`
    : ' Add a member, or a spawn profile to spawn one.'}</p>`}
  </div></div>`;
}

// MiniRuleCases is the shared rule rows (process-rule-cases.js) on the
// inline editor's view.
function MiniRuleCases({ view, id, onApply }) {
  const node = view.template.nodes[id];
  return html`<${RuleCases} rule=${node.performer} outcomes=${view.edges.filter((edge) => edge.from === id).map((edge) => edge.outcome)}
    operands=${ruleOperands(view, id)} loopsBack=${(outcome) => !!loopBound(node, outcome)}
    stepName=${(stepID) => view.template.nodes[stepID]?.name || stepID}
    onChange=${(rule) => { try { onApply(setRule(view, id, rule)); } catch { /* an outcome went away under the picker */ } }} />`;
}

// ExhaustionPicker says where a loop goes once its rounds are used up, beside
// its rounds: another outcome, failing the run, or (a plain decision) asking
// the decider again. The agree outcome is listed only when already chosen,
// marked: it would pass work the deciders kept sending back.
function ExhaustionPicker({ view, id, outcome, value, onApply }) {
  const options = exhaustionOptions(view, id, outcome);
  return html`<label class="gm-exhausted" data-mini-exhausted=${outcome}><span class="gq-dim">then</span>
    <${Select} size="sm" aria-label=${`When ${outcome} runs out of rounds`} value=${value}
      onChange=${(event) => onApply(setExhaustion(view, id, outcome, event.currentTarget.value))}>
      ${options.map((option) => html`<option key=${option.value} value=${option.value}>${option.skipsReview ? `${option.label} (skips the review)` : option.label}</option>`)}
    <//></label>`;
}

// Inspector is the compact editor of one step: the step list's fields.
function Inspector({ view, id, roster, roles, fallbackInfo, onApply, onRename, onKind, onClose, onFullEditor, onAnswerStep, fresh = '', onFreshUsed }) {
  const node = view.template.nodes[id];
  // A step just added opens with its name ready to type: naming it is part
  // of adding it, and Tab goes on to what it does.
  const box = useRef(null);
  // fresh is the id of the step just added; once focused it is used up, so
  // reopening the inspector on any step later never takes focus.
  useEffect(() => {
    if (!fresh || fresh !== id) return;
    const input = box.current?.querySelector?.('[data-mini-name]');
    input?.focus?.();
    input?.select?.();
    onFreshUsed?.();
  }, [fresh, id]);
  // The gate's Asks as last shown, per gate: a tie keeps it, so giving one
  // decider its own ask never swaps which text is the shared one.
  const shownAsk = useRef({});
  const shared = node?.deciders?.length ? gateAsk(node, shownAsk.current[id] ?? null) : '';
  if (node?.deciders?.length) shownAsk.current[id] = shared;
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
    // A decision stays one whoever decides it: its outcomes and loops stay.
    if (node.type === 'decision' && who.mode !== 'command') { onApply(setDecisionWho(view, id, who)); return; }
    if (who.mode === 'you' || who.mode === 'command') { onKind(who.mode); return; }
    if (kind !== 'agent') { onKind('agent', who); return; }
    onApply(updateStep(view, id, (s) => setStepWho(s, who)));
  };
  const label = { agent: 'What', you: 'Asks', command: 'Runs', gate: 'Asks' }[kind];
  const rule = kind === 'rule';
  const gate = kind === 'gate';
  const noMoreDeciders = node.type === 'decision' ? canAddDecider(view, id) : 'only a decision';
  const stepName = (stepID) => view.template.nodes[stepID]?.name || stepID;
  // Which agent: the same as, or not the one of, another agent step.
  const spawns = kind === 'agent' && stepWho(step).mode === 'spawn';
  const others = kind === 'agent' && !spawns && stepWho(step).mode !== 'agent' ? agentStepIDs(view.template, id) : [];
  const constraint = constraintValue(stepConstraint(step));
  const constraintKnown = !constraint || others.some((other) => constraint === `same:${other}` || constraint === `different:${other}`);
  const outputs = stepOutputs(step);
  // What this step's text can read: the other steps' outputs.
  const readable = Object.entries(view.template.nodes).filter(([other]) => other !== id)
    .flatMap(([, other]) => other.captures || []);
  return html`<div ref=${box} class="gm-inspector" data-mini-inspector=${id} role="dialog" aria-label=${`Step ${node.name || id}`}
    onKeyDown=${(event) => { if (event.key === 'Escape') { event.stopPropagation(); onClose(); } }}>
    <label class="gm-field"><span>Step</span><${TypedField} name="groups-mini-editor-input" type="text" data-mini-name value=${node.name || ''} placeholder=${id}
      onChange=${(event) => onRename(event.currentTarget.value)} /></label>
    ${!rule && html`<label class="gm-field"><span>${label}</span><${TypedField} as="textarea" name="groups-mini-editor-textarea" rows="3" data-mini-text value=${gate ? collapseParamShorthand(shared) : text}
      title=${gate ? "Every decider is asked this, unless its ▸ gives it its own ask" : undefined}
      onChange=${(event) => onApply(gate ? ((typed) => { shownAsk.current[id] = expandParamShorthand(typed); return setGateAsk(view, id, typed, shared); })(event.currentTarget.value)
        : updateStep(view, id, (s) => setStepText(s, event.currentTarget.value)))} /></label>`}
    ${readable.length > 0 && kind !== 'command' && !rule && html`<p class="gq-dim gm-readable" data-mini-readable>Can use: ${readable.map((name, i) => html`${i > 0 && ', '}<code key=${name}>${outputRef(name)}</code>`)}</p>`}
    ${gate
    ? html`<${GateDeciders} view=${view} id=${id} shared=${shared} roster=${roster} roles=${roles} fallbackInfo=${fallbackInfo}
        noMoreDeciders=${noMoreDeciders} onApply=${onApply} />
      <div class="gm-field" data-mini-agree=${id}><span>All must choose</span><${Select} size="sm" aria-label="The outcome every decider must choose"
        value=${node.agree || ''} onChange=${(event) => onApply(setAgree(view, id, event.currentTarget.value))}>
        ${outcomes.filter(({ outcome }) => !loopBound(node, outcome)).map(({ outcome }) => html`<option key=${outcome} value=${outcome}>${outcome}</option>`)}
      <//></div>
      <p class="gq-dim">Otherwise the first decider, in this order, who chose something else decides; the work goes back with everyone's notes.</p>`
    : html`<div class="gm-field"><span>Who</span><${WhoPicker} step=${step} roster=${roster} roles=${roles} profiles=${fallbackInfo.profiles}
      defaultSpawnProfile=${fallbackInfo.defaultSpawnProfile} rule=${node.type === 'decision' && (kind === 'rule' || ruleOperands(view, id).length > 0)} onChange=${setWho} />
      ${node.type === 'decision' && kind !== 'command' && !rule && html`<${Button} size="sm" variant="link" class="gq-link" data-mini-add-decider
        disabled=${!!noMoreDeciders} title=${noMoreDeciders || 'Several decide together: all must agree'}
        onClick=${() => onApply(addReviewer(view, id))}>+ another decider<//>`}</div>
      ${node.type === 'decision' && kind === 'agent' && html`<div class="gm-field" data-mini-decided-by=${id}><span>Decided by</span>
        <span class="gq-dim">🤖 ${whoLine(node, fallbackInfo).replace(/^pick who$/, 'the agent you pick')} reports one outcome as its verdict. Nobody is asked.</span></div>`}
      ${rule && html`<${MiniRuleCases} view=${view} id=${id} onApply=${onApply} />`}`}
    ${others.length > 0 && html`<div class="gm-field" data-mini-constraint=${id}><span>Which agent</span><${Select} size="sm" aria-label="Which agent does this step"
      value=${constraint} onChange=${(event) => onApply(updateStep(view, id, (s) => setStepConstraint(s, constraintFromValue(event.currentTarget.value))))}>
      <option value="">whoever fits</option>
      ${!constraintKnown && html`<option value=${constraint}>${constraint.startsWith('same:') ? `same as ${stepName(constraint.slice(5))}` : `not the agent of ${constraint.slice(10).split(',').map(stepName).join(', ')}`}</option>`}
      <optgroup label="The same agent as">${others.filter((other) => !view.template.nodes[other]?.deciders?.length).map((other) => html`<option key=${`same:${other}`} value=${`same:${other}`}>same as ${stepName(other)}</option>`)}</optgroup>
      <optgroup label="Not the agent of">${others.map((other) => html`<option key=${`different:${other}`} value=${`different:${other}`}>not ${stepName(other)}</option>`)}</optgroup>
    <//></div>`}
    ${kind === 'agent' && !spawns && html`<div class="gm-field" data-mini-fallback=${id}><span>If nobody</span><${FallbackPicker} value=${stepFallbackValue(step)}
      label="If no member can take this step" inherit=${inheritLabel(fallbackInfo.fallback, fallbackInfo.defaultSpawnProfile)}
      defaultSpawnProfile=${fallbackInfo.defaultSpawnProfile} profiles=${fallbackInfo.profiles}
      onChange=${(next) => onApply(updateStep(view, id, (s) => setStepFallback(s, next)))} /></div>`}
    ${(kind === 'agent' || kind === 'command') && node.type === 'task' && html`<${FailureRow} id=${id} failure=${stepFailure(step)} staged=${!!(node.plan || node.checks?.length || node.review)}
      onChange=${(next) => onApply(updateStep(view, id, (s) => setStepFailure(s, next)))} />`}
    ${kind === 'agent' && node.type === 'task' && html`<label class="gm-field" data-mini-outputs=${id}><span>Outputs</span><${TypedField} name="none-e-g-pr" type="text"
      value=${outputs.join(', ')} placeholder="none (e.g. pr)" title="Names this step reports, which later steps read as {{ outputs.<name> }}"
      onChange=${(event) => onApply(updateStep(view, id, (s) => setStepOutputs(s, event.currentTarget.value)))} /></label>`}
    <div class="gm-field"><span>${node.type === 'decision' ? 'Outcomes' : 'Then'}</span><div class="gm-outcomes">
      ${outcomes.map(({ outcome, to }) => html`<span key=${outcome} class="gm-outcome" data-mini-outcome=${outcome}>
        ${node.type === 'decision'
    ? html`<${TypedField} name="gm-outcome-name" type="text" class="gm-outcome-name" aria-label=${`Rename ${outcome}`} value=${outcome}
          onChange=${(event) => onApply(renameOutcome(view, id, outcome, event.currentTarget.value))} />`
    : ''} →
        <${Select} size="sm" aria-label=${`After ${outcome}`} value=${to} onChange=${(event) => onApply(setOutcomeTarget(view, id, outcome, event.currentTarget.value))}>
          ${targetsFor(view, id, outcome).map((target) => html`<option key=${target.id} value=${target.id} data-mini-back=${target.back ? '' : undefined}>${target.back ? `↺ ${target.label} (again)` : target.label}</option>`)}
        <//>
        ${loopBound(node, outcome) && html`<label class="ui-tag ui-tag-warn gm-loop" data-mini-loop=${outcome} title="Going back runs that work again; past the last round this outcome is refused">
          ≤<${TypedField} name="groups-mini-editor-input" type="number" min=${LOOP_MIN_ROUNDS} max=${LOOP_MAX_ROUNDS} aria-label=${`Rounds for ${outcome}`} value=${loopBound(node, outcome).maxIterations}
            onChange=${(event) => { const rounds = normalizeRounds(event.currentTarget.value); if (rounds) onApply(setLoopRounds(view, id, outcome, rounds)); else event.currentTarget.value = String(loopBound(node, outcome).maxIterations); }} /> rounds</label>`}
        ${loopBound(node, outcome) && html`<${ExhaustionPicker} view=${view} id=${id} outcome=${outcome} value=${loopBound(node, outcome).onExhausted || ''} onApply=${onApply} />`}
        ${node.type === 'decision' && !loopBound(node, outcome) && onAnswerStep && html`<${Button} size="sm" icon=${true} variant="ghost" data-mini-answer-step=${outcome}
          aria-label=${`A new step on ${outcome}`} title=${`A new step on ${outcome}, before where it goes now`}
          onClick=${(event) => onAnswerStep(outcome, event)}>⑂<//>`}
        ${node.type === 'decision' && !canRemoveOutcome(view, id, outcome) && html`<${Button} size="sm" icon=${true} variant="ghost danger" aria-label=${`Remove ${outcome}`}
          onClick=${() => onApply(removeOutcome(view, id, outcome))}>✕<//>`}
      </span>`)}
      ${node.type === 'decision' && html`<${Button} size="sm" icon=${true} class="gm-add-outcome" data-mini-add-outcome title="Add an outcome"
        onClick=${() => onApply(addOutcome(view, id, outcomes.find((o) => !loopBound(node, o.outcome))?.to || targets.find((t) => view.template.nodes[t.id]?.type === 'end')?.id || targets[0]?.id))}>+<//>`}
      ${!outcomes.length && html`<span class="gq-dim">nothing yet: hover the step for + after</span>`}
    </div></div>
    <div class="gm-row"><${Button} size="sm" variant="link" class="gq-link gm-more" data-mini-more onClick=${onFullEditor}
      title="Model and effort, plan/check/review stages and the rest are edited in the full editor">More options in ⤢ Full editor<//>
      <span class="gq-spacer"></span><${Button} size="sm" variant="link" class="gq-link" onClick=${onClose}>done<//></div>
  </div>`;
}

// StartFrom is the strip under a new process's empty canvas: what it can
// start from instead of a first step. One row: the built-in ways of working,
// a description for the scribe, a .pwf file, and the library.
function StartFrom({ api, wizard, busy, onView, onDescribe, onImport }) {
  const [open, setOpen] = useState('');
  const [text, setText] = useState('');
  const [templates, setTemplates] = useState(null);
  // The built-in ways of working (tk_d02b0f4c), read from the daemon: [] when
  // it offers none.
  const [builtins, setBuiltins] = useState([]);
  useEffect(() => {
    if (typeof api?.listBuiltins !== 'function') return undefined;
    let live = true;
    api.listBuiltins().then((rows) => { if (live) setBuiltins(rows || []); }).catch(() => {});
    return () => { live = false; };
  }, [api]);
  const builtin = (item) => void api.getBuiltin(item.id).then((view) => onView(view, item.name)).catch(() => {});
  const builtinTitle = (item) => `${item.description || item.name}${item.steps?.length ? `\nSteps: ${item.steps.join(' → ')}` : ''}\nBuilt in: this process gets its own copy, yours to change.`;
  useEffect(() => {
    if (open !== 'library' || templates) return undefined;
    let live = true;
    api.listTemplates().then((rows) => { if (live) setTemplates(rows); }).catch(() => { if (live) setTemplates([]); });
    return () => { live = false; };
  }, [open]);
  const describe = (event) => {
    event.preventDefault();
    if (text.trim() && !busy) onDescribe(text.trim());
  };
  const chip = (attrs, label, title, onClick, on = false) => html`<button data-ui-exempt type="button" class=${`gm-from${on ? ' on' : ''}`} ...${attrs}
    disabled=${!!busy} title=${title} onClick=${onClick}>${label}</button>`;
  return html`<div class="gm-start-from" data-mini-start-from>
    <div class="gm-start-row" data-mini-builtins>
      <span class="gq-dim">Start from</span>
      ${builtins.map((item) => chip({ key: item.id, 'data-mini-builtin': item.id }, `◈ ${item.name}`, builtinTitle(item), () => builtin(item)))}
      ${chip({ 'data-mini-describe': '' }, '✨ Describe it…', wizard ? 'Tell a scribe the rite in plain words' : 'Describe it in plain words and a process scribe drafts the steps', () => setOpen(open === 'describe' ? '' : 'describe'), open === 'describe')}
      ${onImport && chip({ 'data-mini-import': '' }, '↥ Import .pwf', 'Import a shared .pwf workflow file: you review what it runs first', onImport)}
      ${chip({ 'data-mini-library': '' }, wizard ? 'More written rites ›' : 'More ways of working ›', 'Copy a process from the template library', () => setOpen(open === 'library' ? '' : 'library'), open === 'library')}
    </div>
    ${open === 'describe' && html`<form class="gq-ask gm-describe" data-mini-describe-form onSubmit=${describe}>
      <input name="describe-the-process-in-plain" type="text" class="gq-ask-in" aria-label="Describe the process in plain words" maxlength=${DRAFT_DESCRIPTION_MAX} value=${text} disabled=${!!busy}
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
  // A disabled item says why in visible text under it, not only on hover.
  const why = (text) => text && html`<span class="gm-menu-why" data-mini-why>${text}</span>`;
  const item = (key, text, reason, onClick) => html`<button data-ui-exempt type="button" role="menuitem" class="gm-menu-item" data-mini-menu=${key}
    disabled=${!!reason} title=${reason} onClick=${onClick}>${text}${why(reason)}</button>`;
  const kinds = (key, text, reason, op, choices = INSERT_CHOICES) => html`<div class="gm-menu-kinds" data-mini-menu=${key}><span class=${reason ? 'gm-off' : ''}>${text}${why(reason)}</span>
    <${KindChips} label=${text} disabled=${reason} choices=${choices} onPick=${(kind) => onRun(() => op(view, id, kind))} /></div>`;
  return html`<div ref=${rootRef} class="gm-menu" role="menu" aria-label=${`Structure of ${node.name || id}`} style=${floating(menu, 400, 330)}>
    ${kinds('before', 'Insert step before', canInsertBefore(view, id), insertBefore)}
    ${kinds('after', 'Insert step after', canInsertAfter(view, id), insertAfter, afterChoices(view, id))}
    ${kinds('branch', node.type === 'task' ? 'Branch' : 'Branch on outcome', step ? canBranch(view, id) : 'only a step branches', branch, branchChoices(view, id))}
    ${kinds('alongside', 'Run alongside (a track)', canRunAlongside(view, id), runAlongside, alongsideChoices(view, id))}
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
  notify = defaultNotify, onClose, onSteps, draft: isDraft = false, untouched = false, onRun = null, onDiscard = null, onAbandon = null, otherNames = [],
  onImport = null, onDescribe = null, scribes = [], onGone = null,
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
  // createdHere is a draft New process just made and opened here.
  const createdHere = useRef(isDraft && untouched);
  const inflight = useRef(null);
  const failedAt = useRef(null);
  // base is the saved version the draft rests on: the merge base when an
  // agent saves a newer one (groups-mini-live.js).
  // Opened with unsaved edits (from the step list), the view is not the
  // version its sourceHash names: then the base is the stored one when that is
  // the same version, and unknown otherwise, so nothing merges unasked.
  const base = useRef(!changed ? view : stored?.sourceHash && stored.sourceHash === view.sourceHash ? stored : null);
  // refused counts saves refused for a newer version in a row: after a few,
  // the merge is not getting anywhere, and the old error shows instead.
  const refused = useRef(0);
  const mounted = useRef(true);
  useEffect(() => () => { mounted.current = false; }, []);
  // goneRef: the template was deleted while open; nothing saves it again.
  const goneRef = useRef(false);
  const cardRef = useRef(null);
  // incoming is a newer version that changes what the operator is editing too,
  // waiting for their choice; notice is the last one merged, with its undo.
  const [incoming, setIncoming] = useState(null);
  const [notice, setNotice] = useState(null);
  const syncing = useRef(false);
  const resync = useRef(false);
  // helper is the agent asked from this editor's ✨ With an agent menu.
  const [helper, setHelper] = useState(null);
  // lit is the steps the last agent save added or changed, highlighted for
  // a moment; editing is the agent saving into this draft lately, shown with
  // a Stop; stopped is the agent the operator stopped, until it saves again.
  const [lit, setLit] = useState(null);
  const [editing, setEditing] = useState(null);
  const [stopped, setStopped] = useState('');
  useEffect(() => {
    if (!lit) return undefined;
    const timer = setTimeout(() => setLit(null), FRESH_MS);
    return () => clearTimeout(timer);
  }, [lit]);
  useEffect(() => {
    if (!editing) return undefined;
    const timer = setTimeout(() => setEditing(null), EDITING_IDLE_MS);
    return () => clearTimeout(timer);
  }, [editing]);
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
  // duplicate is another process of the group with the name being typed,
  // the placeholder name aside: a warning, not a refusal.
  const typedName = String(title || '').trim().toLowerCase();
  const duplicate = isDraft && typedName && typedName !== UNTITLED_PROCESS.toLowerCase()
    ? otherNames.find((other) => String(other || '').trim().toLowerCase() === typedName) || '' : '';
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
  // problemNode is the drawn node an error names: its own, the inline
  // subprocess holding it ("qa/unit" is drawn as qa), or the node an edge
  // error leaves ("review:changes"); '' for the process's.
  const problemNode = (diagnostic) => {
    const target = String(diagnostic?.targetId || '');
    const id = diagnostic?.scope === 'node' ? target.split('/')[0] : diagnostic?.scope === 'edge' ? target.split(':')[0].split('/')[0] : '';
    return id && draft.template.nodes[id] ? id : '';
  };
  const problemIds = new Set(errors.map(problemNode).filter(Boolean));
  const firstProblem = problemNode(errors[0]);
  const roles = groupInfo?.roles || null;
  const profiles = useSpawnProfiles(api);
  const fallbackInfo = { fallback: draft.template.fallback || null, defaultSpawnProfile: String(groupInfo?.defaultSpawnProfile || ''), profiles };
  // apply lands one operation's result as one undoable edit.
  // declare: false for the Params dialog's own edit, which may rename or
  // drop a param on purpose (validation then names the steps still using it).
  const apply = (result, focus = undefined, { declare = true } = {}) => {
    touched.current = true;
    setError('');
    setNotice(null);
    // A {{param}} a step now uses is declared with it (declareParams).
    model.current.applyDraft(declare ? declareParams(result.view) : result.view);
    setRev(model.current.rev);
    if (focus !== undefined) setSelected(focus);
  };
  // A process that has never run names its steps by their names: a renamed
  // step's id follows (process-step-ids.js), so runs, decisions and agent
  // messages read build-it, not new-step-2. Once it has run, ids stay.
  const idsFollowNames = isDraft && !process.ran;
  const renameStep = (id, value) => {
    const named = updateStep(draft, id, (s) => setStepName(s, value));
    const result = idsFollowNames ? followName(named.view, id) : named;
    apply(result, result.id);
  };
  // fresh is the step just added: its inspector opens on the name.
  const [fresh, setFresh] = useState('');
  const run = (op) => {
    setMenu(null);
    setPicker(null);
    try {
      const result = op();
      if (result.view === draft) return;
      apply(result, result.id && result.view.template.nodes[result.id] ? result.id : '');
      // What the edit also changed elsewhere, with its undo.
      if (result.note) setNotice({ note: result.note });
      if (result.id && !draft.template.nodes[result.id] && isStep(result.view.template.nodes[result.id])) setFresh(result.id);
    } catch (cause) {
      setError(cause.message);
    }
  };
  // editParams opens the full editor's Params dialog over this draft: what
  // it applies lands as one undoable edit (setParams), on the draft as it is
  // then, since a newer version may have been merged in meanwhile.
  const paramsDialog = useRef(null);
  useEffect(() => () => paramsDialog.current?.(), []);
  const editParams = async () => {
    if (paramsDialog.current) return;
    paramsDialog.current = () => {};
    const { openProcessParamsDialog } = await import('./process-params-dialog.js');
    if (!mounted.current) { paramsDialog.current = null; return; }
    paramsDialog.current = openProcessParamsDialog({
      model: {
        template: viewOf(model.current).template,
        setParams: (params) => {
          const now = viewOf(model.current);
          if (JSON.stringify(params || {}) === JSON.stringify(now.template.params || {})) return false;
          apply(setParams(now, params), undefined, { declare: false });
          return true;
        },
      },
      onClosed: () => { paramsDialog.current = null; },
      confirmDiscard: shellConfirmDiscard,
    });
  };
  const undo = () => { if (model.current.undo()) { setRev(model.current.rev); setSelected(''); setNotice(null); } };
  const stepsList = viewToStepList(draft);
  const save = async ({ quiet = false } = {}) => {
    if (goneRef.current) return false;
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
      base.current = { ...draft, sourceHash: model.current.sourceHash, currentRef: model.current.currentRef };
      refused.current = 0;
      if (!quiet) notify(isDraft ? `Saved ${name}` : `Saved ${name} as a new version`);
      void source?.refresh?.();
      done(true);
      return true;
    } catch (cause) {
      done(false);
      // Deleted while open: close, never save it back into being.
      if (cause?.status === 404) { gone(); return false; }
      // Someone (an agent, most often) saved a newer version: merge it in and
      // save again, rather than leave the operator's edit stuck.
      if (/head changed/.test(cause.message) && live && refused.current < MAX_REFUSED) {
        refused.current += 1;
        resync.current = true;
        return null;
      }
      setError(/head changed/.test(cause.message)
        ? `${name} was saved elsewhere since you opened it. ${isDraft ? 'Close it without saving' : 'Cancel'} and reopen it to edit the newest version.` : cause.message);
      return false;
    } finally {
      inflight.current = null;
      setBusy('');
      if (resync.current) { resync.current = false; void sync({ force: true }); }
    }
  };
  // live: this editor follows its template's head (an api that can read it).
  const live = typeof api.getTemplateHead === 'function';
  // gone: the template was deleted while open (its head reads 404). The
  // editor closes, and nothing saves it back into being.
  const gone = () => {
    if (!mounted.current) return;
    goneRef.current = true;
    if (onGone) onGone(process.templateId, name);
    else setError(`${name} was deleted. Close the editor.`);
  };
  // adopt merges theirs, a newer saved version, into the draft as one
  // undoable edit; a version that also changes what the operator changed
  // waits for their choice ('theirs' or 'mine').
  const adopt = (theirs, choice = '') => {
    if (!mounted.current) return;
    const ours = viewOf(model.current);
    const head = templateHeadFromEditView(theirs);
    const presented = head.actor === 'human:operator' ? null : processActorPresentation(dashboardState.snapshot.value, head.actor);
    const who = head.actor === 'human:operator' ? 'You (elsewhere)' : presented?.label || 'Someone';
    const result = base.current ? mergeIncoming({ base: base.current, ours, theirs, choice })
      : wholeChoice(ours, theirs, choice);
    if (!result.merged) { setIncoming({ theirs, who, conflicts: result.conflicts }); return; }
    model.current.adoptReviewedGeneration(result.merged,
      { ref: theirs.currentRef, sourceHash: theirs.sourceHash, semanticHash: theirs.semanticHash }, { clean: result.clean });
    base.current = theirs;
    touched.current = true;
    failedAt.current = null;
    setIncoming(null);
    setAutosave('');
    setError('');
    if (result.merged.template?.name !== ours.template?.name) setTitle(result.merged.template?.name || name);
    setNotice(result.changes.length ? { who, what: changeList(result.changes), kept: choice === 'mine' } : null);
    if (presented?.agentId) {
      const ids = freshNodeIds(result.changes);
      if (ids.size) setLit({ ids });
      setEditing({ agentId: presented.agentId, who: presented.label });
      setStopped('');
    }
    setRev(model.current.rev);
  };
  // sync reads the head and adopts it when it is a version this editor has
  // not seen. A save under way, or one that lands meanwhile, skips the read:
  // the save's own answer is the newer truth.
  // A field the operator is typing in (it commits on change) holds the read
  // back, so a merge never resets what they have not committed yet; a save's
  // own retry (force) goes ahead, as the field has just committed.
  const typing = () => {
    const active = cardRef.current?.ownerDocument?.activeElement;
    return !!active && cardRef.current.contains(active) && /^(INPUT|TEXTAREA)$/.test(active.tagName || '');
  };
  const sync = async ({ force = false } = {}) => {
    if (!live || !mounted.current || syncing.current || inflight.current || (!force && typing())) return;
    syncing.current = true;
    const at = model.current.sourceHash;
    try {
      const theirs = await api.getTemplateHead(process.templateId);
      if (!inflight.current && model.current.sourceHash === at && theirs?.sourceHash && theirs.sourceHash !== at) adopt(theirs);
    } catch (cause) {
      if (cause?.status === 404) { gone(); return; }
      // The next read tries again; a save waiting on this merge says why it stopped.
      if (force && mounted.current) setError(`${name} was saved elsewhere, and the newer version could not be read. Close it and reopen it.`);
    } finally {
      syncing.current = false;
    }
  };
  const syncRef = useRef(sync);
  syncRef.current = sync;
  useEffect(() => {
    if (!live) return undefined;
    let stopped = false;
    let timer = null;
    const tick = async () => {
      if (stopped) return;
      if (!incomingRef.current) await syncRef.current();
      if (!stopped) timer = setTimeout(tick, LIVE_POLL_MS);
    };
    timer = setTimeout(tick, LIVE_POLL_MS);
    return () => { stopped = true; clearTimeout(timer); };
  }, [process.templateId, live]);
  const incomingRef = useRef(null);
  incomingRef.current = incoming;
  // A draft saves itself a moment after each change; a change made while a
  // save is under way is saved after it.
  // One that fails is not retried until the next change.
  useEffect(() => {
    if (!isDraft || !dirty || busy || incoming || failedAt.current === draftKey) return undefined;
    const timer = setTimeout(() => {
      setAutosave('saving');
      void save({ quiet: true }).then((ok) => {
        // null: a newer version is being merged in, and saved after it.
        if (ok === null) { setAutosave(''); return; }
        failedAt.current = ok ? null : draftKey;
        setAutosave(ok ? 'saved' : 'failed');
      });
    }, AUTOSAVE_DELAY_MS);
    return () => clearTimeout(timer);
  }, [isDraft, draftKey, dirty, busy, !!incoming, model.current.sourceHash]);
  // The full editor opens over this tab, pinned to the group still: its
  // ← Back inline brings the process back here.
  // saveFirst saves before an action that needs the draft stored. One refused
  // for a newer version merges that in first: the operator checks it and
  // presses again, rather than the action going ahead on what they did not see.
  const saveFirst = async (quiet = true) => {
    const ok = await save({ quiet });
    if (ok === null) notify('A newer version was merged in first: check it, then press again.');
    return !!ok;
  };
  const fullEditor = async () => {
    if (dirty && !(await saveFirst(isDraft))) return;
    leaving.current = true;
    await openProcessDraft({ id: process.templateId, name, group: group.name });
    onClose();
  };
  const commitTitle = (value) => {
    const next = String(value || '').trim() || UNTITLED_PROCESS;
    setTitle(next);
    if (next !== draft.template.name) apply({ view: { ...draft, template: { ...draft.template, name: next } } });
  };
  const empty = !Object.values(draft.template.nodes || {}).some(isStep);
  // droppable is a draft that leaving may delete: one made by New process
  // in this editor, never run, with nothing but start and end (an advanced
  // node is content too), that no agent is helping with or scribe drafting.
  // A draft opened any other way (from the full editor, the step list, the
  // group's list) is never dropped. The delete names the version this editor
  // last saw, so steps an agent saved since are not taken with it.
  const scribing = scribes.some((session) => session.scope?.id === process.templateId);
  const droppable = isDraft && createdHere.current && !process.ran && !helper && !scribing
    && Object.values(draft.template.nodes || {}).every((node) => node?.type === 'start' || node?.type === 'end');
  const dropOptions = (options = {}) => (model.current.sourceHash ? { ...options, ifSourceHash: model.current.sourceHash } : options);
  // startFrom fills an empty draft with a starter's or a library process's
  // graph, under this draft's id, named after it while still untitled.
  const startFrom = (from, label) => {
    if (!from?.template) return;
    const keep = draft.template.name && draft.template.name !== UNTITLED_PROCESS;
    const next = keep ? draft.template.name : (label || from.template.name || UNTITLED_PROCESS);
    setTitle(next);
    apply({ view: { template: { ...from.template, id: draft.template.id, name: next }, edges: from.edges || [], layout: from.layout || {} } });
  };
  // Closing a draft keeps it (saved first). ✕ drops a droppable one, named
  // or not; Save keeps a named one, as asked.
  const closeDraft = async ({ keep = false } = {}) => {
    if (droppable && (!keep || !touched.current) && onDiscard) { leaving.current = true; onDiscard(process.templateId, dropOptions()); return; }
    if (dirty && !(await saveFirst())) return;
    leaving.current = true;
    onClose();
  };
  const runDraft = async () => {
    if (dirty && !(await saveFirst())) return;
    leaving.current = true;
    onRun?.(process.templateId, name);
  };
  const describe = async (text) => {
    if (dirty && !(await saveFirst())) return;
    leaving.current = true;
    onDescribe?.(text, { id: process.templateId, sourceHash: model.current.sourceHash, ref: model.current.currentRef, view: draft });
  };
  const importPwf = () => {
    onImport?.();
    leaving.current = true;
    if (droppable && onDiscard) onDiscard(process.templateId, dropOptions());
    else onClose();
  };
  // A draft left with no steps any other way (the page or the tab changes
  // under it, the browser goes) is dropped as ✕ drops it, so abandoned
  // "Untitled process · 0 steps" drafts do not pile up. Not one handed on
  // (to the step list, the full editor, a run, a scribe), nor one an agent
  // was asked to help with or a scribe is drafting.
  const leaving = useRef(false);
  const abandon = useRef(null);
  abandon.current = (options) => {
    if (leaving.current || goneRef.current || !droppable || !onAbandon) return;
    leaving.current = true;
    onAbandon(process.templateId, dropOptions(options));
  };
  useEffect(() => {
    const win = cardRef.current?.ownerDocument?.defaultView || globalThis;
    // A page going into the back/forward cache may come back: keep it.
    const gone = (event) => { if (!event?.persisted) abandon.current?.({ keepalive: true }); };
    win.addEventListener?.('pagehide', gone);
    return () => { win.removeEventListener?.('pagehide', gone); abandon.current?.(); };
  }, []);
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
  const handle = (op, id, x, y, glyph, text, why, hint = '') => html`<button type="button" data-ui-exempt class=${`gm-handle gm-handle-${op}`} data-mini-handle=${op}
    style=${`left:${x}px;top:${y}px`} disabled=${!!why} title=${why || hint || text} aria-label=${`${text} ${view.template.nodes[id]?.name || id}`}
    onPointerDown=${(event) => { if (op === 'after' && !why) { const at = pointAt(event); setDrag({ id, ...at, x0: at.x, y0: at.y, moved: false }); } }}
    onClick=${(event) => {
      if (dragged.current) { dragged.current = false; return; }
      const box = event.currentTarget?.getBoundingClientRect?.() || { left: 0, bottom: 0 };
      if (!why) setPicker({ op, id, cx: box.left, cy: box.bottom + 4 });
    }}>${glyph}<span>${text}</span></button>`;
  // A new reviewer track's review defaults to the group's reviewer role.
  const trackOptions = { reviewerRole: reviewerRoleOf(roles?.length ? roles : groupRoles(roster)) };
  const OPS = { after: (v, id, kind) => insertAfter(v, id, kind, trackOptions), branch, alongside: (v, id, kind) => runAlongside(v, id, kind, trackOptions), before: insertBefore, answer: (v, id, kind) => insertOnAnswer(v, id, picker.outcome, kind) };
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
      ${isStep(data) && handle('branch', id, node.x - 9, node.y + node.height / 2 + 2, '⑂', 'branch', canBranch(draft, id),
    draft.template.nodes[id]?.type === 'task' ? 'branch: a decision after this step, or its agent decides' : '')}
      ${isStep(data) && handle('alongside', id, node.x - 9, node.y - node.height / 2 - 20, '⫴', 'alongside', canRunAlongside(draft, id))}
    </span>`}
      ${drag?.moved && html`<span class="gm-ghost" style=${`left:${drag.x}px;top:${drag.y}px`}>+ new step</span>`}
      ${picker && html`<div class="gm-picker" data-mini-picker=${picker.op} style=${floating(picker, 300, 40)}>
        <${KindChips} label=${`${{ after: 'Add after', branch: 'Branch to', alongside: 'Run alongside', before: 'Insert before', answer: `On ${picker.outcome}` }[picker.op]}`}
          choices=${picker.op === 'branch' ? branchChoices(draft, picker.id) : picker.op === 'alongside' ? alongsideChoices(draft, picker.id)
    : picker.op === 'after' ? afterChoices(draft, picker.id) : INSERT_CHOICES}
          onPick=${(kind) => run(() => OPS[picker.op](draft, picker.id, kind))} />
        <${Button} size="sm" variant="link" class="gq-link" aria-label="Close" onClick=${() => setPicker(null)}>✕<//>
      </div>`}
      ${menu && html`<${StructureMenu} view=${draft} menu=${menu} onRun=${run} onClose=${() => setMenu(null)}
        onWrap=${() => { setMenu(null); void fullEditor(); }} />`}`;
  };
  // ✨ With an agent: the process scribe of this template (the one the full
  // editor's Get help also reaches), or an agent already running, such as
  // the steward, briefed on this draft. What it saves lands here as it goes.
  const brief = () => [
    processScribeBrief(processScribeHandoff({
      kind: 'template', id: process.templateId, currentRef: model.current.currentRef, sourceHash: model.current.sourceHash,
    })),
    inlineHelpNote(),
  ].join('\n\n');
  let taskRef = null;
  try { taskRef = processScribeTaskRef({ scope: { kind: PROCESS_SCRIBE_SCOPE_KIND, id: process.templateId } }); } catch { taskRef = null; }
  const help = {
    name: PROCESS_SCRIBE_NAME, slugs: PROCESS_SCRIBE_SLUGS, exclusive: true, taskRef,
    scope: { kind: PROCESS_SCRIBE_SCOPE_KIND, id: process.templateId }, subject: PROCESS_SCRIBE_CHAT_SUBJECT, brief,
    confirmation: {
      title: 'Grant process-scribe access?', body: processScribeGrantBody(`the process "${name}"`),
      meta: PROCESS_SCRIBE_GRANT_META, okLabel: 'Grant & start',
    },
  };
  const scribe = scribes.find((session) => session.scope?.id === process.templateId && session.online) || null;
  const agent = scribe ? { id: scribe.agentId, label: scribe.name, status: scribe.status } : helper;
  // The agent is editing while it saved here lately, or while this
  // process's own scribe is in a turn, until the operator stops it. A helper
  // such as the steward counts only by its saves: its turn may be about
  // something else.
  const working = scribe && agentStatus(dashboardState.snapshot.value, scribe.agentId) === 'working';
  const busyAgent = editing || (working ? { agentId: scribe.agentId, who: scribe.name } : null);
  const shownEditing = busyAgent && busyAgent.agentId !== stopped ? busyAgent : null;
  const stop = async () => {
    const { agentId, who } = shownEditing;
    try {
      await stopAgent(agentId);
      setStopped(agentId);
      setEditing(null);
      notify(`Stopped ${who}. Ask it in its chat window to carry on.`);
    } catch (cause) {
      notify(`Could not stop ${who}: ${cause.message}`, true);
    }
  };
  const agentChip = agent && html`<${Button} size="sm" variant="link" class="gq-link gm-agent" data-mini-agent=${agent.id}
    title=${`Open ${agent.label}'s chat window: ask it for changes there; what it saves shows here`}
    onClick=${() => void openHelperWindow(agent.id, agent.label)}>💬 ${agent.label}${agent.status === 'working' ? ' · working…' : ''}<//>`;
  const startId = Object.keys(draft.template.nodes || {}).find((id) => draft.template.nodes[id]?.type === 'start') || '';
  const projection = withFirstStep(draftProjection(draft, { whoOf: (id, node) => whoLine(node, fallbackInfo), noteOf: (id, node) => fallbackNote({ node }, fallbackInfo), name }), empty ? startId : '');
  // The placeholder opens the kind picker for a step after start.
  const addFirst = () => {
    const box = canvas.current?.querySelector?.(`[data-process-node="${FIRST_STEP}"]`)?.getBoundingClientRect?.() || { left: 0, bottom: 0 };
    setMenu(null);
    setPicker({ op: 'after', id: startId, cx: box.left, cy: box.bottom + 4 });
  };
  return html`<section ref=${cardRef} class="gm-card ui-card ui-inset" aria-label=${`Edit ${name} of ${group.name}`} data-mini-editor=${process.templateId}
    onKeyDownCapture=${(event) => {
      // An open picker or menu closes first, before the inspector's own
      // Escape (which would close the step instead).
      if (event.key === 'Escape' && (menu || picker)) { event.stopPropagation(); setMenu(null); setPicker(null); }
    }}
    onKeyDown=${(event) => {
      if ((event.ctrlKey || event.metaKey) && event.key === 'z' && !/INPUT|TEXTAREA|SELECT/.test(event.target?.tagName || '')) { event.preventDefault(); undo(); }
    }}>
    <div class="gm-head">
      ${isDraft
    ? html`<input name="process-name" type="text" class="gm-title" data-mini-title ref=${nameRef} aria-label="Process name" maxlength="120" value=${title}
        onInput=${(event) => setTitle(event.currentTarget.value)} onChange=${(event) => commitTitle(event.currentTarget.value)}
        onKeyDown=${(event) => { if (event.key === 'Enter') { event.preventDefault(); event.currentTarget.blur(); } }} />
      ${duplicate && html`<span class="ui-tag ui-tag-warn" role="status" data-mini-duplicate title="Two processes with one name are easy to mix up in Resources and when starting a run">Another process in ${group.name} is called ${duplicate}</span>`}
      <span class="ui-tag gm-autosave" data-mini-autosave=${autosave || (dirty ? 'pending' : 'saved')} title=${autosave === 'failed' ? error : ''}>${
        autosave === 'saving' || busy === 'auto' ? 'draft · saving…' : autosave === 'failed' ? 'draft · not saved' : `draft · autosaved to ${group.name}`}</span>`
    : html`<b class="gm-name">${name}</b>
      <span class="gq-dim">${dirty ? 'draft' : 'saved'}</span>`}
      <span class="gm-toggle" role="group" aria-label="View">
        <button data-ui-exempt type="button" aria-pressed="true" class="on">◈ Graph</button>
        <button type="button" data-ui-exempt data-mini-steps disabled=${!stepsList.ok} title=${stepsList.ok ? 'Show the same draft as a step list' : `The step list cannot show it: ${stepsList.reason}`}
          onClick=${() => { leaving.current = true; onSteps({ list: stepsList.list, changed: dirty, sourceHash: model.current.sourceHash, ref: model.current.currentRef }); }}>☰ Steps</button>
      </span>
      ${dirty && !isDraft && html`<span class="gm-changes" data-mini-changes=${summary.length}>● ${summary.length || 1} unsaved change${summary.length === 1 ? '' : 's'}${summary.length ? `: ${summary.slice(0, 3).join(', ')}${summary.length > 3 ? '…' : ''}` : ''}</span>`}
      <span class="gq-spacer"></span>
      ${agentChip}
      <${GetHelpMenu} class="gm-get-help" help=${help} label="With an agent" labelNode=${'✨ With an agent'}
        title="Build this with an agent: a process scribe, or one already running (the steward, a chat). It sees this draft, and what it saves shows here."
        before=${async () => !model.current.dirty || saveFirst()}
        onHelper=${(found) => setHelper({ id: found.agentId, label: found.name })} />
      <${Button} size="sm" data-mini-undo disabled=${!model.current.canUndo} title=${withShortcut('Undo', 'mini-undo')} onClick=${undo}>↶ undo<//>
      <${Button} size="sm" data-mini-full disabled=${!!busy && busy !== 'auto'} title=${dirty ? 'Save, then open it in the full editor' : 'Open it in the full editor'} onClick=${fullEditor}>⤢ ${isDraft ? 'Full editor' : 'full editor'}<//>
      ${isDraft
    ? html`<${Button} size="sm" variant="primary" data-mini-save disabled=${!!busy && busy !== 'auto'} title="Save it now and close the editor"
        onClick=${() => void closeDraft({ keep: true })}>Save<//>
      ${onRun && html`<${Button} size="sm" data-mini-run disabled=${(!!busy && busy !== 'auto') || empty} title=${empty ? 'Add a step first' : `Save it, then pick how to run it in ${group.name}`}
        onClick=${() => void runDraft()}>▶ Run…<//>`}
      <${Button} size="sm" icon=${true} variant="ghost" data-mini-close aria-label="Close" title=${droppable ? 'Close: a process with no steps is not kept' : 'Close: it stays saved in the group'}
        onClick=${() => void closeDraft()}>✕<//>
      ${autosave === 'failed' && html`<${Button} size="sm" variant="link" class="gq-link" data-mini-drop title="Close it; the changes since the last save are lost"
        onClick=${() => { leaving.current = true; onClose(); }}>close without saving<//>`}`
    : html`${discarding
    ? html`<span class="gq-confirm">Discard ${summary.length || 'the'} change${summary.length === 1 ? '' : 's'}?
        <${Button} size="sm" variant="danger" data-mini-discard onClick=${onClose}>discard<//>
        <${Button} size="sm" variant="link" class="gq-link" onClick=${() => setDiscarding(false)}>keep<//></span>`
    : html`<${Button} size="sm" data-mini-cancel onClick=${() => (dirty ? setDiscarding(true) : onClose())}>cancel<//>`}
      <${Button} size="sm" variant="primary" data-mini-save disabled=${!!busy || !dirty} title=${dirty ? 'Save as a new version' : 'Nothing changed'}
        onClick=${async () => { if (await saveFirst(false)) onClose(); }}>${busy === 'save' ? 'saving…' : 'Save new version'}<//>`}
    </div>
    ${shownEditing && html`<div class="gm-live gm-editing" role="status" data-mini-editing=${shownEditing.agentId}>
      <span class="gm-editing-dot" aria-hidden="true"></span>
      <span>✨ ${shownEditing.who} is editing this process. New and changed steps light up as they land.</span>
      <${Button} size="sm" variant="link" class="gq-link" data-mini-editing-chat title="Open its chat window" onClick=${() => void openHelperWindow(shownEditing.agentId, shownEditing.who)}>💬 chat<//>
      <${Button} size="sm" data-mini-stop title="Stop its current turn; your draft keeps what it saved so far" onClick=${() => void stop()}>■ Stop<//>
    </div>`}
    ${incoming && html`<div class="gm-live gm-live-conflict" role="alert" data-mini-incoming>
      <span>${incoming.who} saved a version that also changes what you changed: ${changeList(incoming.conflicts)}.</span>
      <${Button} size="sm" variant="primary" data-mini-take-theirs onClick=${() => adopt(incoming.theirs, 'theirs')}>Use theirs<//>
      <${Button} size="sm" data-mini-keep-mine onClick=${() => adopt(incoming.theirs, 'mine')}>Keep mine<//>
      <span class="gq-dim">Their other changes are merged either way.</span>
    </div>`}
    ${notice && !incoming && html`<div class="gm-live" role="status" data-mini-notice>
      <span>${notice.note || `✨ ${notice.who} changed ${notice.what}${notice.kept ? ' (your edits kept where you both changed)' : ''}.`}</span>
      <${Button} size="sm" variant="link" class="gq-link" data-mini-notice-undo title=${notice.note ? 'Undo this edit, clearing included' : 'Take their change back out; it saves again'} onClick=${undo}>undo<//>
      <${Button} size="sm" variant="link" class="gq-link" aria-label="Dismiss" onClick=${() => setNotice(null)}>✕<//>
    </div>`}
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
      <${ProcessMiniGraph} run=${projection} wizard=${wizard} selectedId=${selected} problemIds=${problemIds} freshIds=${lit?.ids || null}
        onNodeClick=${(node) => { if (node.id === FIRST_STEP) { addFirst(); return; } setMenu(null); setPicker(null); setSelected(node.id === selected ? '' : node.id); }}
        onNodeContextMenu=${(node, event) => (node.id === FIRST_STEP ? event.preventDefault() : openMenu(node, event))}
        onNodeHover=${(id) => setHover(id === FIRST_STEP ? '' : id)} overlay=${overlay} />
    </div>
    ${isDraft && empty && html`<${StartFrom} api=${api} wizard=${wizard} busy=${busy && busy !== 'auto' ? busy : ''}
      onView=${startFrom} onDescribe=${onDescribe ? (text) => void describe(text) : null} onImport=${onImport ? importPwf : null} />`}
    ${selected && html`<${Inspector} view=${draft} id=${selected} roster=${roster} roles=${roles} fallbackInfo=${fallbackInfo}
      onApply=${(result) => apply(result)} onRename=${(value) => renameStep(selected, value)} onClose=${() => setSelected('')} onFullEditor=${fullEditor}
      fresh=${fresh} onFreshUsed=${() => setFresh('')} onAnswerStep=${(outcome, event) => {
        const box = event.currentTarget?.getBoundingClientRect?.() || { left: 0, bottom: 0 };
        setMenu(null);
        setPicker({ op: 'answer', id: selected, outcome, cx: box.left, cy: box.bottom + 4 });
      }}
      onKind=${(kind, who) => run(() => {
        const result = changeKind(draft, selected, kind);
        // The Who lands on the changed step; its note (what it cleared) stays.
        return who ? { ...updateStep(result.view, selected, (s) => setStepWho(s, who)), ...(result.note ? { note: result.note } : {}) } : result;
      })} />`}
    <div class="gm-foot">
      <span class="gq-dim">${isDraft ? 'It saves itself as you go; runs already going keep their version.' : 'Saving makes a new version; runs already going keep theirs.'}</span>
      ${errors.length > 0 && html`<span class="gq-error ui-error" role="alert" data-mini-problems=${errors.length}>${`${errors.length} problem${errors.length === 1 ? '' : 's'}: `}${firstProblem && html`<${Button} size="sm" variant="link" class="gq-link gm-problem-step" data-mini-problem-step=${firstProblem}
          title="Select this step" onClick=${() => { setMenu(null); setPicker(null); setSelected(firstProblem); }}>${draft.template.nodes[firstProblem].name || firstProblem}<//>`}${firstProblem ? ': ' : ''}${errors[0].message}${problemIds.size > 1 ? ` (${problemIds.size} steps outlined)` : ''}</span>`}
      <span class="gq-spacer"></span>
      <span class="gm-params" data-mini-params=${Object.keys(draft.template.params || {}).join(',')}>
        ${Object.entries(draft.template.params || {}).map(([pname, param]) => html`<span key=${pname} class="ui-tag" data-mini-param=${pname}
          title=${param.description || ''}>${pname}${param.type && param.type !== 'string' ? `: ${param.type}` : ''}${param.default !== undefined ? ` = ${String(param.default)}` : ''}</span>`)}
        <${Button} size="sm" variant="link" class="gq-link" data-mini-params-edit title="Declare the process's parameters: {{name}} in a step's text uses one"
          onClick=${() => void editParams()}>${Object.keys(draft.template.params || {}).length ? 'edit params' : '+ params'}<//>
      </span>
      ${!process.listed && html`<label class="gq-dim"><input name="groups-mini-editor-input" type="checkbox" data-mini-publish checked=${publish}
        onChange=${(event) => setPublish(event.currentTarget.checked)} /> also publish to the library</label>`}
    </div>
    ${error && html`<div class="gq-error ui-error" role="alert">${error}</div>`}
  </section>`;
}

