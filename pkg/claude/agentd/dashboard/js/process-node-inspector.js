// process-node-inspector.js -- the editor's docked node inspector (mock E3,
// tcl-oypy1p), replacing the modal node dialog. Selecting one node opens it
// beside the canvas as a stack of blocks (process-node-blocks.js): required
// blocks for the node kind, optional blocks that carry data, and blocks the
// author added. Blocks reorder by drag or ↑/↓ (a per-kind UI preference) and
// optional ones remove by clearing their slice.
//
// There is no draft: every edit commits straight through the editor's
// `editNode` gate (ProcessEditModel.updateNode inside the editor's mutate),
// so it is one undoable step and is validated live like any canvas edit. A
// refused edit leaves the model untouched and snaps the field back.

import { h } from 'preact';
import { useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { dashPrefs } from './prefs.js';
import {
  RETRY_ON_FAIL_MODES, PLAN_APPROVAL_MODES,
  defaultPerformer, setStageEnabled, setPlanApproval, addCheck, removeCheck, moveCheck, setCheckID,
  setRetryField, setCaptures, setWaitField, setNodeText, formatLines,
  MAX_GATE_DECIDERS, addGateDecider, removeGateDecider,
  DECISION_KINDS, RULE_KIND, TASK_KINDS, setDecisionKind, setNodeFailure, setRule,
} from './process-node-form.js';
import { ruleOperands } from './process-rules.js';
import { RuleCases } from './process-rule-cases.js';
import { loopBound } from './process-loops.js';
import { FailureRow } from './process-failure-row.js';
import { stepFailure } from './process-step-list.js';
import {
  nodeBlock, visibleBlocks, addableBlocks, removeBlock, moveKey, moveKeyBefore, mergeOrder,
  upstreamInputs, stageSummary,
  setSubprocessPort, setSubprocessRef,
} from './process-node-blocks.js';
import { NodeField, NodeSelect, PerformerEditor } from './process-node-fields.js';
import { Button, Select } from './ui-components.js';
import {
  CHECK_SOURCES, WAIT_MET, WAIT_TIMED_OUT, DEFAULT_BACKOFF_FACTOR, DEFAULT_MAX_FAILED_CHECKS,
  waitMode, setWaitMode, checkSource, setCheckSource, setCheckField, checkMoreSet,
  describeCheck, durationFeedback, waitRouteHint,
} from './process-wait-step.js';

const html = htm.bind(h);

// referencedTemplates lists the stored templates a node's diagnostics came
// through (subprocess templateRefs), each once.
export function referencedTemplates(diagnostics = []) {
  return [...new Set(diagnostics.map((entry) => entry.templateRef).filter(Boolean))];
}

export const BLOCK_ORDER_PREF = 'tclaude.dash.processInspector.blockOrder.';

// Prompt and Bounds own these performer fields; Performer shows the rest.
const PROMPT_FIELDS = ['ask', 'prompt', 'subject', 'text'];
const BOUNDS_FIELDS = ['timeout'];

export function readBlockOrder(kind) {
  try {
    const parsed = JSON.parse(dashPrefs.getItem(BLOCK_ORDER_PREF + kind) || '[]');
    return Array.isArray(parsed) ? parsed.filter((key) => typeof key === 'string') : [];
  } catch {
    return [];
  }
}

function writeBlockOrder(kind, order) {
  dashPrefs.setItem(BLOCK_ORDER_PREF + kind, JSON.stringify(order));
}

// The node's main performer slot: a task's work performer, a decision's
// decider. Reading it never mints one; editing through it does.
function mainPerformer(node) {
  return node.performer || defaultPerformer(node.type === 'decision' ? 'human' : 'agent');
}

function locateMain(type) {
  return (draft) => {
    if (!draft.performer) draft.performer = defaultPerformer(type === 'decision' ? 'human' : 'agent');
    return draft.performer;
  };
}

// DecidersBlock edits a gate: who decides, each asked their own question,
// and the outcome all of them must choose.
// A gate's votes are its outcomes, so a decider names no choices of its own.
const GATE_VOTE_OMIT = ['choices', 'choiceOutcomes'];
// A command cannot vote: deciders are agents or people.
const GATE_DECIDER_KINDS = ['agent', 'human'];

function DecidersBlock({ node, commit, readOnly, edges }) {
  const outcomes = edges.map((edge) => edge.outcome);
  return html`<div class="pni-deciders" data-node-deciders>
    <${NodeSelect} fieldKey="agree" label="all must choose" options=${outcomes.includes(node.agree) || !node.agree ? outcomes : [node.agree, ...outcomes]}
      value=${node.agree || ''} readOnly=${readOnly} apply=${(key, value) => commit(key, (draft) => { draft.agree = value; })} />
    <p class="process-node-edges-note">Otherwise the first decider below who chose something else decides, and every decider's notes go back with it.</p>
    ${node.deciders.map((decider, index) => html`<div key=${index} class="process-node-check" data-node-decider=${index}>
      <div class="process-node-check-head"><span class="process-node-field-label">decider ${index + 1}</span><span class="spacer"></span>
        ${!readOnly && html`<${Button} size="sm" variant="ghost danger" class="process-action" aria-label=${`Remove decider ${index + 1}`}
          onClick=${() => commit(`deciders.${index}.remove`, (draft) => removeGateDecider(draft, index, outcomes))}>remove<//>`}</div>
      <${PerformerEditor} path=${`deciders.${index}`} locate=${(draft) => draft.deciders[index]} performer=${decider}
        commit=${commit} readOnly=${readOnly} choiceRouting=${false} contact=${true} omit=${GATE_VOTE_OMIT} kinds=${GATE_DECIDER_KINDS} />
    </div>`)}
    ${!readOnly && node.deciders.length < MAX_GATE_DECIDERS && html`<${Button} size="sm" class="process-action process-node-add" data-node-add-decider
      onClick=${() => commit('deciders.add', (draft) => addGateDecider(draft, outcomes))}>+ add decider<//>`}
  </div>`;
}

// RuleBlock edits a rule decision's cases with the inline editor's rows,
// plus each case's condition as text, which is how one with or, not or
// parentheses is written.
function RuleBlock({ node, nodeId, model, commit, readOnly, edges }) {
  return html`<${RuleCases} rule=${node.performer} outcomes=${edges.map((edge) => edge.outcome)}
    operands=${ruleOperands(model, nodeId)} loopsBack=${(outcome) => !!loopBound(node, outcome)}
    stepName=${(id) => model.node(id)?.name || id} readOnly=${readOnly} editText=${true}
    label="cases" fieldClass="process-node-rule"
    onChange=${(rule) => commit('performer.rule', (draft) => setRule(draft.performer, rule))} />`;
}

function PerformerBlock({ node, nodeId, controller, commit, readOnly, edges }) {
  const type = node.type || 'task';
  const model = controller.model;
  const rule = type === 'decision' && node.performer?.kind === RULE_KIND;
  if (type === 'decision' && node.deciders?.length) return html`<${DecidersBlock} node=${node} commit=${commit} readOnly=${readOnly} edges=${edges} />`;
  if (type === 'task' && !node.performer && !readOnly) {
    return html`<${Button} size="sm" class="process-action process-node-add"
      onClick=${() => commit('work.add', (draft) => { draft.performer = defaultPerformer('agent'); })}>add work performer<//>`;
  }
  return html`<${PerformerEditor} path="performer" locate=${locateMain(type)} performer=${mainPerformer(node)}
    commit=${commit} readOnly=${readOnly} choiceRouting=${type !== 'decision'} omit=${[...PROMPT_FIELDS, ...BOUNDS_FIELDS]}
    ...${type === 'decision' ? { kinds: DECISION_KINDS, setKind: (slot, kind, draft) => setDecisionKind(model, nodeId, draft, slot, kind) }
    : type === 'task' && !(node.plan || node.checks?.length || node.review) ? { kinds: TASK_KINDS } : {}} />
    ${rule && html`<${RuleBlock} node=${node} nodeId=${nodeId} model=${model} commit=${commit} readOnly=${readOnly} edges=${edges} />`}
    ${type === 'decision' && !readOnly && node.performer?.kind !== 'program' && !rule && edges.length > 1 && html`<${Button} size="sm" class="process-action process-node-add" data-node-add-decider
      title="Several decide together, and all must agree" onClick=${() => commit('deciders.add', (draft) => addGateDecider(draft, edges.map((edge) => edge.outcome)))}>+ another decider<//>`}`;
}

function PromptBlock({ node, commit, readOnly }) {
  return html`<${PerformerEditor} path="performer" locate=${locateMain(node.type || 'task')} performer=${mainPerformer(node)}
    commit=${commit} readOnly=${readOnly} showKind=${false} only=${PROMPT_FIELDS} />`;
}

function BoundsBlock({ node, commit, readOnly }) {
  return html`<${PerformerEditor} path="performer" locate=${locateMain(node.type || 'task')} performer=${mainPerformer(node)}
    commit=${commit} readOnly=${readOnly} showKind=${false} only=${BOUNDS_FIELDS} contact=${true} />`;
}

function StagesBlock({ node, commit, readOnly }) {
  const field = (key, label, value, mutate, options = {}) => html`<${NodeField} fieldKey=${key} label=${label} value=${value || ''}
    readOnly=${readOnly} apply=${(fieldKey, raw) => commit(fieldKey, (draft) => mutate(draft, raw))} ...${options} />`;
  const toggle = (key, label, stage) => html`<label class="process-node-stage-toggle">
    <input name="process-node-toggle" type="checkbox" class="process-node-toggle" checked=${!!node[stage]} disabled=${readOnly} data-node-field=${key}
      onChange=${(event) => commit(key, (draft) => setStageEnabled(draft, stage, event.currentTarget.checked))} />
    <span>${label}</span>
  </label>`;
  return html`<div class="process-node-stages">
    <div class="pni-stage-chips" aria-label="Work pipeline">${stageSummary(node).map((stage, i) => html`${i > 0 && html`<span class="pni-stage-arrow" aria-hidden="true">→</span>`}<span key=${stage.key} class=${`ui-tag pni-stage-chip pni-stage-${stage.key.split(':')[0]}`} title=${stage.detail}>${stage.label}</span>`)}</div>
    ${toggle('plan.enabled', 'plan before work', 'plan')}
    ${node.plan && html`<${NodeSelect} fieldKey="plan.approval" label="plan approval" options=${PLAN_APPROVAL_MODES}
        value=${node.plan.approval || 'auto'} readOnly=${readOnly}
        apply=${(key, value) => commit(key, (draft) => setPlanApproval(draft, value))} />
      <${PerformerEditor} path="plan.performer" locate=${(draft) => draft.plan.performer} performer=${node.plan.performer}
        commit=${commit} readOnly=${readOnly} contact=${true} />`}
    <div class="process-node-checks">
      <span class="process-node-field-label">checks</span>
      ${(node.checks || []).map((check, index) => html`<div key=${index} class="process-node-check">
        <div class="process-node-check-head">
          ${field(`checks.${index}.id`, 'check id', check.id, (draft, value) => setCheckID(draft, index, value))}<span class="spacer"></span>
          ${!readOnly && html`<${Button} size="sm" icon=${true} class="process-action" aria-label=${`Move check ${check.id} up`} onClick=${() => commit(`checks.${index}.up`, (draft) => moveCheck(draft, index, -1))}>↑<//><${Button} size="sm" icon=${true} class="process-action" aria-label=${`Move check ${check.id} down`} onClick=${() => commit(`checks.${index}.down`, (draft) => moveCheck(draft, index, 1))}>↓<//><${Button} size="sm" variant="ghost danger" class="process-action" aria-label=${`Remove check ${check.id}`} onClick=${() => commit(`checks.${index}.remove`, (draft) => removeCheck(draft, index))}>remove<//>`}
        </div>
        <${PerformerEditor} path=${`checks.${index}.performer`} locate=${(draft) => draft.checks[index].performer} performer=${check.performer}
          commit=${commit} readOnly=${readOnly} contact=${true} />
      </div>`)}
      ${!readOnly && html`<${Button} size="sm" class="process-action process-node-add" onClick=${() => commit('checks.add', (draft) => addCheck(draft))}>+ add check<//>`}
    </div>
    ${toggle('review.enabled', 'review gate after checks', 'review')}
    ${node.review && html`<${PerformerEditor} path="review.performer" locate=${(draft) => draft.review.performer} performer=${node.review.performer}
      commit=${commit} readOnly=${readOnly} contact=${true} />`}
  </div>`;
}

function InputsBlock({ inputs }) {
  return html`<div class="pni-inputs">
    ${inputs.captures.length
    ? html`<ul class="pni-input-list">${inputs.captures.map((capture) => html`<li key=${`${capture.from}:${capture.name}`}><code>${`{{ outputs.${capture.name} }}`}</code> ← ${capture.fromName}</li>`)}</ul>`
    : html`<p class="process-node-empty ui-empty">No upstream captures reach this node.</p>`}
    ${inputs.params.length > 0 && html`<p class="pni-params">params: ${inputs.params.map((name, i) => html`${i > 0 && ', '}<code key=${name}>${name}</code>`)}</p>`}
  </div>`;
}

function RetryBlock({ node, nodeId, commit, readOnly }) {
  // "If it fails" is the inline editor's row, on the steps it is for there:
  // an agent's or a command's work.
  const failing = (node.performer?.kind || 'agent') !== 'human';
  return html`${failing && !readOnly && html`<${FailureRow} id=${nodeId} failure=${stepFailure({ node })} staged=${!!(node.plan || node.checks?.length || node.review)}
      onChange=${(failure) => commit('failure', (draft) => setNodeFailure(draft, failure))} />`}
    <div class="pni-row">
    <${NodeField} fieldKey="retry.maxAttempts" label="max attempts" placeholder="unset" readOnly=${readOnly}
      value=${node.retry?.maxAttempts === undefined ? '' : String(node.retry.maxAttempts)}
      apply=${(key, raw) => commit(key, (draft) => setRetryField(draft, 'maxAttempts', raw))} />
    <${NodeSelect} fieldKey="retry.onFail" label="on fail" options=${RETRY_ON_FAIL_MODES} value=${node.retry?.onFail}
      blankLabel="default (fresh-attempt)" readOnly=${readOnly}
      apply=${(key, raw) => commit(key, (draft) => setRetryField(draft, 'onFail', raw))} />
    <${NodeField} fieldKey="patience" label="patience" placeholder="template's" readOnly=${readOnly}
      value=${node.patience || ''}
      apply=${(key, raw) => commit(key, (draft) => setNodePatience(draft, raw))} />
  </div>`;
}

// setNodePatience sets how long the step may stay stuck before the engine
// raises a concern (a duration such as 45m); blank takes the template's.
export function setNodePatience(node, raw) {
  const text = String(raw ?? '').trim();
  if (text) node.patience = text;
  else delete node.patience;
}

function OutputsBlock({ node, commit, readOnly }) {
  // An agent reports each with --output <name>=<value>; a person fills each in
  // when they finish the step as done. Later prompts read {{ outputs.<name> }}.
  const person = node.performer?.kind === 'human';
  return html`<${NodeField} fieldKey="captures" label="published outputs" multiline=${true} placeholder=${person ? 'one name per line, e.g. weight' : 'one-name-per-line'}
    hint=${person ? 'The person fills each in when they finish this step as done' : ''}
    value=${formatLines(node.captures)} readOnly=${readOnly}
    apply=${(key, raw) => commit(key, (draft) => setCaptures(draft, raw))} />`;
}

// ChoiceSelect is a select whose options say what they mean.
function ChoiceSelect({ fieldKey, label, options, value, apply, readOnly, title = '' }) {
  return html`<label class="field process-node-field" title=${title || undefined}><span class="process-node-field-label">${label}</span>
    <select name="process-node-select" data-ui-exempt class="process-node-select" aria-label=${label} disabled=${readOnly} data-node-field=${fieldKey}
      value=${value} onChange=${(event) => apply(fieldKey, event.currentTarget.value)}>
      ${options.map(([key, text]) => html`<option key=${key} value=${key}>${text}</option>`)}
    </select>
  </label>`;
}

// WaitDuration is a duration field that says what it read: "= 3 days".
function WaitDuration({ fieldKey, label, value, placeholder, apply, readOnly, hint = '' }) {
  const feedback = durationFeedback(value);
  return html`<div class="pni-wait-duration">
    <${NodeField} fieldKey=${fieldKey} label=${label} value=${value || ''} placeholder=${placeholder} hint=${hint}
      invalid=${!!feedback.error} readOnly=${readOnly} apply=${apply} />
    ${feedback.text && html`<span class=${`pni-wait-feedback${feedback.error ? ' is-error' : ''}`} data-wait-feedback=${fieldKey}>${feedback.text}</span>`}
  </div>`;
}

const CHECK_PERFORMER_FIELDS = { program: ['profile', 'run', 'args'], agent: ['prompt', 'role', 'anyone', 'sameAs', 'differentFrom', 'fallback'] };

// WaitBlock edits a wait step: for a time, or until a criterion holds,
// checked again and again (process-wait-step.js).
function WaitBlock({ node, nodeId, controller, commit, readOnly, edges }) {
  const mode = waitMode(node);
  const [more, setMore] = useState(() => checkMoreSet(node.wait?.check));
  const check = node.wait?.check;
  // A bad value is refused with its reason, and the field snaps back.
  const field = (fieldKey, raw) => {
    const key = fieldKey.slice('wait.check.'.length);
    return commit(fieldKey, (draft) => { const problem = setCheckField(draft, key, raw); if (problem) throw new Error(problem); });
  };
  const toggle = html`<div class="pni-wait-mode" role="radiogroup" aria-label="How it waits">
    ${[['time', 'For a time'], ['check', 'Until a condition']].map(([key, text]) => html`<${Button} size="sm" key=${key}
      class=${`pni-wait-mode-option${mode === key ? ' is-selected' : ''}`} role="radio" aria-checked=${mode === key ? 'true' : 'false'}
      data-wait-mode=${key} disabled=${readOnly}
      onClick=${() => { if (mode !== key) commit('wait.mode', (draft) => setWaitMode(draft, key)); }}>${text}<//>`)}
  </div>`;
  if (mode === 'time') {
    return html`${toggle}<div class="pni-row">
      <${WaitDuration} fieldKey="wait.duration" label="wait for" value=${node.wait?.duration} placeholder="3 days, 2h, 90m" readOnly=${readOnly}
        hint="How long from when the step starts; {{ params.x }} or {{ outputs.x }} work too"
        apply=${(fieldKey, raw) => commit(fieldKey, (draft) => setWaitField(draft, 'duration', raw))} />
      <${NodeField} fieldKey="wait.until" label="or until" value=${node.wait?.until || ''} placeholder="2026-12-01T09:00:00Z or {{ params.at }}" readOnly=${readOnly}
        hint="An RFC3339 time; with both, the wait ends at whichever comes first"
        apply=${(fieldKey, raw) => commit(fieldKey, (draft) => setWaitField(draft, 'until', raw))} />
    </div>
    ${node.wait?.signal && html`<p class="process-node-empty ui-empty">This wait also names a signal (${node.wait.signal}), which does not run yet.</p>`}`;
  }
  const source = checkSource(check);
  const routeHint = waitRouteHint(node, (edges || []).map((edge) => edge.outcome));
  return html`${toggle}
    <p class="pni-wait-summary" data-wait-summary>${describeCheck(check)}</p>
    <div class="pni-row">
      <${ChoiceSelect} fieldKey="wait.check.source" label="check that" value=${source} readOnly=${readOnly}
        options=${CHECK_SOURCES.map((item) => [item.key, item.label])} title=${CHECK_SOURCES.find((item) => item.key === source)?.hint}
        apply=${(fieldKey, raw) => commit(fieldKey, (draft) => setCheckSource(draft, raw))} />
      <${WaitDuration} fieldKey="wait.check.every" label="every" value=${check.every} placeholder="5m" readOnly=${readOnly}
        hint="The first check is made straight away, then one every interval" apply=${field} />
    </div>
    ${source === 'condition' && html`<${NodeField} fieldKey="wait.check.when" label="condition" value=${check.when || ''} readOnly=${readOnly}
      placeholder='outputs.state == "ready"' hint="Like a rule case: params, outputs and steps.<id>.result; checked at each interval and as soon as the run changes"
      apply=${field} />`}
    ${source !== 'condition' && html`<${PerformerEditor} performer=${check.performer} path="wait.check.performer" readOnly=${readOnly}
      showKind=${false} only=${CHECK_PERFORMER_FIELDS[source]} choiceRouting=${false}
      locate=${(draft) => draft.wait.check.performer} commit=${commit} />`}
    <div class="pni-row">
      <${NodeField} fieldKey="wait.check.maxChecks" label="give up after (checks)" value=${check.maxChecks === undefined ? '' : String(check.maxChecks)}
        placeholder="12" readOnly=${readOnly} apply=${field} />
      <${WaitDuration} fieldKey="wait.check.within" label="or after (time)" value=${check.within} placeholder="1d" readOnly=${readOnly}
        hint="Counted from the first check" apply=${field} />
      <${ChoiceSelect} fieldKey="wait.check.onLimit" label="then" value=${check.onLimit || 'fail'} readOnly=${readOnly}
        options=${[['fail', 'fail the step'], [WAIT_TIMED_OUT, 'take the timed-out route']]}
        apply=${(fieldKey, raw) => {
          const result = commit(fieldKey, (draft) => { setCheckField(draft, 'onLimit', raw); });
          // Its one route becomes the met route; the timed-out one is drawn next.
          const only = edges?.length === 1 ? edges[0] : null;
          if (result.accepted && raw === WAIT_TIMED_OUT && only && only.outcome !== WAIT_MET) {
            controller.renameEdgeOutcome?.(nodeId, only.outcome, WAIT_MET);
          }
          return result;
        }} />
    </div>
    ${routeHint && html`<p class="ui-hint pni-wait-route-hint" role="status" data-wait-route-hint>${routeHint}</p>`}
    <${Button} size="sm" variant="link" class="pni-wait-more" aria-expanded=${more ? 'true' : 'false'} data-wait-more
      onClick=${() => setMore(!more)}>${more ? '▾' : '▸'} more: backoff, timeout, failed checks<//>
    ${more && html`<div class="pni-row" data-wait-more-fields>
      <${ChoiceSelect} fieldKey="wait.check.backoff" label="backoff" value=${check.backoff || 'fixed'} readOnly=${readOnly}
        options=${[['fixed', 'fixed interval'], ['exponential', 'back off']]}
        apply=${(fieldKey, raw) => commit(fieldKey, (draft) => { setCheckField(draft, 'backoff', raw); })} />
      ${check.backoff === 'exponential' && html`<${NodeField} fieldKey="wait.check.factor" label="×" value=${check.factor === undefined ? '' : String(check.factor)}
        placeholder=${String(DEFAULT_BACKOFF_FACTOR)} readOnly=${readOnly} hint="Each interval is the last one times this" apply=${field} />`}
      ${check.backoff === 'exponential' && html`<${WaitDuration} fieldKey="wait.check.maxEvery" label="up to" value=${check.maxEvery} placeholder="1h"
        readOnly=${readOnly} hint="The longest interval backing off reaches" apply=${field} />`}
      ${source === 'program' && html`<${WaitDuration} fieldKey="wait.check.timeout" label="each check at most" value=${check.timeout} placeholder="2m"
        readOnly=${readOnly} hint="A check that runs longer is a failed check" apply=${field} />`}
      <${NodeField} fieldKey="wait.check.maxFailedChecks" label="fail after failed checks in a row"
        value=${check.maxFailedChecks === undefined ? '' : String(check.maxFailedChecks)} placeholder=${String(DEFAULT_MAX_FAILED_CHECKS)}
        readOnly=${readOnly} hint="A check that errors (not a plain no); a no in between starts the count again" apply=${field} />
    </div>`}`;
}

function OutcomesBlock({ edges, nameOf }) {
  return html`<div class="process-node-edges">
    ${edges.map((edge) => html`<div key=${edge.outcome} class="process-node-edge-row"><span class="process-node-edge-outcome">${edge.outcome}</span><span class="process-node-edge-arrow">→</span><span class="process-node-edge-target">${nameOf(edge.to)}</span></div>`)}
    ${!edges.length && html`<p class="process-node-empty ui-empty">No outgoing edges yet.</p>`}
    <p class="process-node-edges-note">Edges are drawn and relabelled on the canvas.</p>
  </div>`;
}

function SubprocessBlock({ node, nodeId, controller, commit, readOnly, edges }) {
  const sub = node.subprocess || {};
  const isRef = !!sub.templateRef;
  const inlineNodes = Object.entries(sub.nodes || {});
  const params = isRef ? Object.keys(sub.inputs || {}) : Object.keys(sub.params || {});
  const ends = isRef ? Object.keys(sub.outputs || {}) : inlineNodes.filter(([, n]) => n?.type === 'end').map(([id]) => id);
  const outcomes = edges.map((edge) => edge.outcome);
  return html`<div class="pni-subprocess">
    ${isRef ? html`<div class="pni-row">
        <${NodeField} fieldKey="subprocess.templateRef" label="template" value=${sub.templateRef} readOnly=${readOnly}
          apply=${(key, raw) => commit(key, (draft) => setSubprocessRef(draft, 'templateRef', raw))} />
        <${NodeField} fieldKey="subprocess.version" label="version" placeholder="head at run start" value=${sub.version || ''} readOnly=${readOnly}
          apply=${(key, raw) => commit(key, (draft) => setSubprocessRef(draft, 'version', raw))} />
      </div>
      ${controller.options?.onOpenTemplate && html`<${Button} size="sm" class="process-action" data-subprocess-open-template
        onClick=${() => controller.options.onOpenTemplate(sub.templateRef)}>open template ↗<//>`}`
    : html`<div class="pni-subprocess-inline"><span>Inline body · ${inlineNodes.length} node${inlineNodes.length === 1 ? '' : 's'}</span>
        <${Button} size="sm" variant="primary" class="process-action" data-subprocess-enter disabled=${!controller.model.canEnterScope(nodeId)}
          title="Open the nested graph (double-click the node does the same)" onClick=${() => controller.enterSubprocess(nodeId)}>enter ▸<//>
        <${Button} size="sm" class="process-action" data-subprocess-expand disabled=${!controller.model.canExpand?.(nodeId)}
          title="Draw the body on this canvas as a framed group and edit it in context" onClick=${() => controller.expandSubprocess(nodeId)}>⊞ expand<//>
        <${Button} size="sm" class="process-action" data-subprocess-unwrap disabled=${readOnly || !controller.model.canExpand?.(nodeId)}
          title="Inline the body into this graph; the ▣ node's connectors attach to its start and ends" onClick=${() => controller.unwrapSubprocess(nodeId)}>⇲ unwrap<//></div>`}
    <div class="pni-ports">
      <span class="process-node-field-label">in-ports · bind body params</span>
      ${params.length ? params.map((param) => html`<${NodeField} key=${param} fieldKey=${`subprocess.inputs.${param}`} label=${`${param} ←`}
          placeholder="literal or {{ params.x }}" value=${sub.inputs?.[param] || ''} readOnly=${readOnly}
          apply=${(key, raw) => commit(key, (draft) => setSubprocessPort(draft, 'inputs', param, raw))} />`)
        : html`<p class="process-node-empty ui-empty">${isRef ? 'Bindings for the referenced template\'s params appear once set.' : 'The body declares no params.'}</p>`}
      <span class="process-node-field-label">out-ports · body end → outcome</span>
      ${ends.length ? ends.map((end) => html`<${NodeSelect} key=${end} fieldKey=${`subprocess.outputs.${end}`} label=${`${end} →`}
          options=${outcomes} value=${sub.outputs?.[end] || ''} blankLabel=${outcomes.length === 1 ? `default (${outcomes[0]})` : 'unmapped'} readOnly=${readOnly}
          apply=${(key, raw) => commit(key, (draft) => setSubprocessPort(draft, 'outputs', end, raw))} />`)
        : html`<p class="process-node-empty ui-empty">No end nodes to map.</p>`}
    </div>
  </div>`;
}

function AboutBlock({ node, commit, readOnly }) {
  const type = node.type || 'task';
  return html`<div class="pni-about">
    ${type !== 'start' && type !== 'end' && html`<${NodeField} fieldKey="meta.description" label="description" value=${node.description || ''}
      readOnly=${readOnly} apply=${(key, raw) => commit(key, (draft) => setNodeText(draft, 'description', raw))} />`}
    <${NodeField} fieldKey="meta.doc" label="doc" multiline=${true} value=${node.doc || ''} readOnly=${readOnly}
      apply=${(key, raw) => commit(key, (draft) => setNodeText(draft, 'doc', raw))} />
  </div>`;
}

const BLOCK_BODIES = {
  subprocess: SubprocessBlock,
  performer: PerformerBlock,
  prompt: PromptBlock,
  stages: StagesBlock,
  inputs: InputsBlock,
  retry: RetryBlock,
  captures: OutputsBlock,
  wait: WaitBlock,
  outcomes: OutcomesBlock,
  bounds: BoundsBlock,
  about: AboutBlock,
};

function Block({ blockKey, index, count, readOnly, removable = true, onMove, onRemove, onDragStart, onDrop, children }) {
  const block = nodeBlock(blockKey);
  const [over, setOver] = useState(false);
  // A div, not a <section>: dashboard.css hides bare sections (tab panes).
  return html`<div class=${`pni-block${over ? ' is-drop-target' : ''}`} data-node-block=${blockKey}
      role="group" aria-label=${block.label}
      onDragOver=${(event) => { if (event.dataTransfer?.types?.includes?.('application/x-tclaude-node-block')) { event.preventDefault(); setOver(true); } }}
      onDragLeave=${() => setOver(false)}
      onDrop=${(event) => { setOver(false); const key = event.dataTransfer?.getData?.('application/x-tclaude-node-block'); if (key) { event.preventDefault(); onDrop(key, blockKey); } }}>
    <div class="pni-block-head" title=${block.hint}>
      <span class="pni-grip" draggable="true" aria-hidden="true" title="Drag to reorder"
        onDragStart=${(event) => { event.dataTransfer?.setData?.('application/x-tclaude-node-block', blockKey); onDragStart?.(blockKey); }}>⠿</span>
      <span class="pni-block-icon" aria-hidden="true">${block.icon}</span>
      <span class="pni-block-title">${block.label}</span>
      <span class="spacer"></span>
      <${Button} size="sm" icon=${true} class="pni-block-move" aria-label=${`Move ${block.label} up`} disabled=${index === 0} onClick=${() => onMove(blockKey, -1)}>↑<//>
      <${Button} size="sm" icon=${true} class="pni-block-move" aria-label=${`Move ${block.label} down`} disabled=${index === count - 1} onClick=${() => onMove(blockKey, 1)}>↓<//>
      ${!block.required && !readOnly && removable && html`<${Button} size="sm" icon=${true} variant="ghost danger" class="pni-block-remove" aria-label=${`Remove ${block.label}`}
        title=${block.clear ? `Remove ${block.label} and clear its settings` : `Hide ${block.label}`} onClick=${() => onRemove(blockKey)}>✕<//>`}
    </div>
    <div class="pni-block-body">${children}</div>
  </div>`;
}

// Closing or deleting from the inspector unmounts the focused button; hand
// focus back to the canvas rather than dropping it on <body>.
function refocusCanvas(controller) {
  queueMicrotask(() => controller.graph?.focusKeyboardTarget?.());
}

export function NodeInspector({ controller, view }) {
  const nodeId = view.selection?.type === 'node' ? view.selection.id : '';
  const node = view.selectedNode;
  const root = useRef(null);
  const [opened, setOpened] = useState({});
  const [, setOrderRevision] = useState(0);
  useLayoutEffect(() => {
    if (!view.nodeFocusRequest) return;
    const label = root.current?.querySelector('[data-node-field="meta.name"]');
    (label && !label.disabled ? label : root.current?.querySelector('.pni-close'))?.focus?.();
  }, [view.nodeFocusRequest]);
  if (!nodeId || !node) return null;
  const model = controller.model;
  const type = node.type || 'task';
  const readOnly = !model.config.nodeEditable(nodeId);
  const pending = view.pending.externalDecision || view.pending.externalReload;
  const commit = (key, mutate) => (controller.editNode(nodeId, mutate) ? { accepted: true } : { blocked: true });
  const inputs = upstreamInputs(model, nodeId);
  const order = readBlockOrder(type);
  const nodeOpened = opened[nodeId] || [];
  const visible = visibleBlocks(node, { opened: nodeOpened, order, context: { inputs } });
  const addable = readOnly ? [] : addableBlocks(node, visible);
  const fromData = visibleBlocks(node, { order, context: { inputs } });
  const saveOrder = (next) => {
    writeBlockOrder(type, mergeOrder(type, order, next));
    setOrderRevision((value) => value + 1);
  };
  const open = (key) => setOpened((prev) => ({ ...prev, [nodeId]: [...(prev[nodeId] || []), key] }));
  const remove = (key) => {
    if (nodeBlock(key)?.clear && !controller.editNode(nodeId, (draft) => removeBlock(draft, key))) return;
    setOpened((prev) => ({ ...prev, [nodeId]: (prev[nodeId] || []).filter((candidate) => candidate !== key) }));
  };
  const nameOf = (id) => model.node(id)?.name || id;
  const diagnostics = (view.issues?.entries || []).filter((entry) => entry.node === nodeId);
  const extraActions = controller.options?.nodeInspectorActions?.({ nodeId, node, readOnly }) || [];
  const bodyProps = { node, nodeId, controller, commit, readOnly, inputs, edges: model.outgoingEdges(nodeId), nameOf };
  return html`<aside ref=${root} class="process-node-inspector process-node-detail${readOnly ? ' is-readonly' : ''}"
      aria-label=${`Node ${nodeId}`} data-node-inspector=${nodeId} inert=${pending}>
    <div class="pni-head">
      <div class="pni-head-row">
        <${NodeField} fieldKey="meta.name" label="label" value=${node.name || ''} readOnly=${readOnly}
          apply=${(key, raw) => (controller.renameNode(nodeId, raw) === undefined ? { blocked: true } : { accepted: true })} />
        <button data-ui-exempt type="button" class="pni-close process-node-close" title="Close the inspector" aria-label="Close node inspector"
          onClick=${() => { controller.setSelection(null); refocusCanvas(controller); }}>✕</button>
      </div>
      <div class="pni-meta"><span class="process-inspector-kind ui-eyebrow">${type} node</span> · <span class="process-inspector-id">${nodeId}</span>
        ${readOnly && html` <span class="process-node-readonly-badge ui-tag ui-tag-warn">read-only</span>`}</div>
      <div class="pni-actions">
        ${view.selectedNodeIncoming > 1 && html`<${Select} size="sm" class="process-inspector-select" aria-label="Join semantics" disabled=${readOnly}
          value=${node.join || ''} onChange=${(event) => controller.setJoin(nodeId, event.currentTarget.value || null)}>
          <option value="">join: unset</option><option value="all">join: all</option><option value="any">join: any</option><//>`}
        ${view.model.start !== nodeId && type !== 'end' && !readOnly && html`<${Button} size="sm" class="process-action" title="Make this node the process entry point" onClick=${() => controller.setStart(nodeId)}>set as start<//>`}
        <${Button} size="sm" class="process-action" disabled=${!view.snippets.canSaveSelection} title="Save this node as a reusable custom snippet" onClick=${() => controller.saveSelectionAsSnippet()}>save as snippet…<//>
        ${extraActions.map((action) => html`<${Button} size="sm" key=${action.key || action.label} class="process-action" disabled=${!!action.disabled} title=${action.title || ''} onClick=${action.run}>${action.label}<//>`)}
        ${!readOnly && html`<${Button} size="sm" variant="danger" class="process-action" onClick=${() => { controller.deleteSelection(); refocusCanvas(controller); }}>delete node<//>`}
      </div>
      ${diagnostics.length > 0 && html`<ul class="pni-diagnostics" aria-label="Issues on this node">${diagnostics.map((entry, i) => html`<li key=${i} class=${`pni-diag is-${entry.severity}`}>${entry.severity === 'error' ? '✕' : '⚠'} ${entry.message}</li>`)}</ul>`}
      ${referencedTemplates(diagnostics).map((ref) => controller.options?.onOpenTemplate && html`<${Button} size="sm" key=${`open:${ref}`}
        class="process-action pni-diag-open" data-diag-open-template=${ref} title="Problems in a stored template are fixed there, not in place"
        onClick=${() => controller.options.onOpenTemplate(ref)}>open ${ref} to fix ↗<//>`)}
    </div>
    ${visible.map((key, index) => {
      const Body = BLOCK_BODIES[key];
      // A block with nothing to clear that is present from data (Inputs with
      // upstream captures) cannot be hidden: its ✕ would do nothing.
      const removable = !!nodeBlock(key)?.clear || !fromData.includes(key);
      return html`<${Block} key=${key} blockKey=${key} index=${index} count=${visible.length} readOnly=${readOnly} removable=${removable}
        onMove=${(blockKey, delta) => saveOrder(moveKey(visible, blockKey, delta))}
        onDrop=${(dragged, before) => saveOrder(moveKeyBefore(visible, dragged, before))}
        onRemove=${remove}><${Body} ...${bodyProps} /></${Block}>`;
    })}
    ${addable.length > 0 && html`<div class="pni-add" role="group" aria-label="Add block">
      <span class="pni-add-label">+ add block:</span>
      ${addable.map((block) => html`<${Button} size="sm" key=${block.key} class="pni-add-block" data-add-block=${block.key}
        title=${block.hint} onClick=${() => open(block.key)}>${block.icon} ${block.label}<//>`)}
    </div>`}
  </aside>`;
}
