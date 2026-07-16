// Preact-owned structured node editor. The pure mutation vocabulary remains in
// process-node-form.js; this module owns only dialog-local draft state.

import { h, render } from 'preact';
import { useCallback, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import {
  PERFORMER_KINDS, RETRY_ON_FAIL_MODES, PLAN_APPROVAL_MODES,
  performerFieldsFor, defaultPerformer, setPerformerKind, setPerformerField,
  setChoiceOutcome, setContactField, setRetryField, setStageEnabled, setPlanApproval,
  addCheck, removeCheck, moveCheck, setCheckID,
  setCaptures, setWaitField, setNodeText, formatLines,
} from './process-node-form.js';

const html = htm.bind(h);
const NODE_DIALOG_SIZE_PREF = 'tclaude.dash.modalSize.process-node-editor';

function replaceNode(target, source) {
  for (const key of Object.keys(target)) delete target[key];
  Object.assign(target, structuredClone(source));
}

function NodeField({
  fieldKey, label, value = '', apply, readOnly, multiline = false,
  hint = '', placeholder = '', invalid = false,
}) {
  const [raw, setRaw] = useState(String(value ?? ''));
  const input = useRef(null);
  const composing = useRef(false);
  useLayoutEffect(() => {
    if (document.activeElement !== input.current && !composing.current) setRaw(String(value ?? ''));
  }, [value]);
  const commit = () => {
    if (!apply || readOnly || composing.current) return;
    const current = input.current?.value ?? raw;
    setRaw(current);
    const result = apply(fieldKey, current);
    if (result?.blocked) {
      const reset = String(value ?? '');
      if (input.current) input.current.value = reset;
      setRaw(reset);
    }
  };
  const Tag = multiline ? 'textarea' : 'input';
  return html`<label class="field process-node-field" title=${hint || undefined}>
    <span class="process-node-field-label">${label}</span>
    <${Tag} ref=${input} class=${multiline ? 'process-node-textarea' : 'process-node-input'}
      type=${multiline ? undefined : 'text'} rows=${multiline ? 3 : undefined}
      spellcheck="false" placeholder=${placeholder} disabled=${readOnly}
      value=${raw} aria-invalid=${invalid ? 'true' : undefined}
      onCompositionStart=${() => { composing.current = true; }}
      onCompositionEnd=${(event) => { composing.current = false; setRaw(event.currentTarget.value); }}
      onInput=${(event) => setRaw(event.currentTarget.value)} onChange=${commit} />
  </label>`;
}

function NodeSelect({ fieldKey, label, options, value, apply, readOnly, blankLabel = null, invalid = false }) {
  const selected = options.includes(value) ? value : (blankLabel !== null ? '' : options[0]);
  return html`<label class="field process-node-field"><span class="process-node-field-label">${label}</span>
    <select class="process-node-select" aria-label=${label} disabled=${readOnly}
      value=${selected} aria-invalid=${invalid ? 'true' : undefined}
      onChange=${(event) => apply?.(fieldKey, event.currentTarget.value)}>
      ${blankLabel !== null && html`<option value="">${blankLabel}</option>`}
      ${options.map((option) => html`<option key=${option} value=${option}>${option}</option>`)}
    </select>
  </label>`;
}

function Section({ title, children }) {
  return html`<section class="process-node-section"><h4 class="process-node-section-title">${title}</h4>${children}</section>`;
}

function PerformerEditor({ performer, locate, path, commit, invalid, readOnly, choiceRouting = true }) {
  const kind = typeof performer?.kind === 'string' && performer.kind ? performer.kind : 'agent';
  const known = PERFORMER_KINDS.includes(kind);
  const mutate = (key, value, operation) => commit(`${path}.${key}`, (draft) => operation(locate(draft), value));
  const fields = known ? performerFieldsFor(kind) : [];
  const contact = performer?.contact || {};
  return html`<div class="process-performer-editor">
    <${NodeSelect} fieldKey=${`${path}.kind`} label="kind"
      options=${known ? PERFORMER_KINDS : [...PERFORMER_KINDS, kind]} value=${kind}
      readOnly=${readOnly} invalid=${invalid.has(`${path}.kind`)}
      apply=${(key, value) => mutate('kind', value, setPerformerKind)} />
    ${!known && html`<p class="process-node-empty">Unknown performer kind ${kind}: validation rejects it — pick a supported kind.</p>`}
    ${fields.map((field) => {
      if (field.outcomeMap) {
        if (!choiceRouting || !(performer?.choices || []).length) return null;
        return html`<div key=${field.key} class="process-choice-outcomes" title=${field.hint}>
          <span class="process-node-field-label">${field.label}</span>
          ${performer.choices.map((choice) => html`<${NodeSelect}
            key=${choice} fieldKey=${`${path}.choice.${choice}`} label=${choice}
            options=${['pass', 'fail']} value=${performer.choiceOutcomes?.[choice] || 'pass'}
            readOnly=${readOnly} invalid=${invalid.has(`${path}.choice.${choice}`)}
            apply=${(key, value) => mutate(`choice.${choice}`, value, (slot, next) => setChoiceOutcome(slot, choice, next))} />`)}
        </div>`;
      }
      const value = field.list ? formatLines(performer?.[field.key]) : performer?.[field.key];
      return html`<div key=${field.key}>
        <${NodeField} fieldKey=${`${path}.${field.key}`} label=${field.label} value=${value || ''}
          multiline=${!!(field.multiline || field.list)} hint=${field.hint} readOnly=${readOnly}
          invalid=${invalid.has(`${path}.${field.key}`)}
          apply=${(key, next) => mutate(field.key, next, (slot, raw) => setPerformerField(slot, field.key, raw, { choiceRouting }))} />
        ${field.key === 'run' && html`<p class="process-node-security-note">⚠ Program performers are command execution: this command runs on the host when the node activates.</p>`}
      </div>`;
    })}
    <div class="process-node-contact" title="Contact schedule for this slot: nudge cadence, budget, escalation target">
      <span class="process-node-field-label">contact schedule</span>
      ${[
        ['cadence', contact.cadence || '', '30m'],
        ['budget', contact.budget === undefined ? '' : String(contact.budget), '5'],
        ['escalationTarget', contact.escalationTarget || '', 'human:operator'],
      ].map(([key, value, placeholder]) => html`<${NodeField} key=${key}
        fieldKey=${`${path}.contact.${key}`} label=${key === 'escalationTarget' ? 'escalate to' : key}
        value=${value} placeholder=${placeholder} readOnly=${readOnly}
        invalid=${invalid.has(`${path}.contact.${key}`)}
        apply=${(fieldKey, next) => mutate(`contact.${key}`, next, (slot, raw) => setContactField(slot, key, raw))} />`)}
    </div>
  </div>`;
}

export function NodeDetail({ model, nodeId, node, mode = 'edit', commit, invalid = new Set(), onClose }) {
  const readOnly = mode !== 'edit' || !commit;
  if (!node) return html`<div class="process-node-detail is-readonly"><p class="process-node-missing">Node ${nodeId} no longer exists.</p></div>`;
  const type = node.type || 'task';
  const field = (key, label, value, mutate, options = {}) => html`<${NodeField}
    fieldKey=${key} label=${label} value=${value || ''} apply=${commit && ((fieldKey, raw) => commit(fieldKey, (draft) => mutate(draft, raw)))}
    readOnly=${readOnly} invalid=${invalid.has(key)} ...${options} />`;
  const select = (key, label, options, value, mutate, props = {}) => html`<${NodeSelect}
    fieldKey=${key} label=${label} options=${options} value=${value}
    apply=${commit && ((fieldKey, raw) => commit(fieldKey, (draft) => mutate(draft, raw)))}
    readOnly=${readOnly} invalid=${invalid.has(key)} ...${props} />`;
  const performer = (slot, locate, value, options = {}) => html`<${PerformerEditor}
    path=${slot} locate=${locate} performer=${value} commit=${commit}
    invalid=${invalid} readOnly=${readOnly} ...${options} />`;
  const stageToggle = (key, label, enabled, stage) => html`<label class="process-node-stage-toggle">
    <input type="checkbox" class="process-node-toggle" checked=${enabled} disabled=${readOnly}
      onChange=${(event) => commit(key, (draft) => setStageEnabled(draft, stage, event.currentTarget.checked))} />
    <span>${label}</span>
  </label>`;
  const edges = model.outgoingEdges(nodeId);
  return html`<div class=${`process-node-detail${readOnly ? ' is-readonly' : ''}`}>
    <div class="process-node-detail-head">
      <span class="process-inspector-kind">${type} node</span><span class="process-inspector-id">${nodeId}</span>
      ${readOnly && html`<span class="process-node-readonly-badge">read-only</span>`}<span class="spacer"></span>
      ${onClose && html`<button class="process-node-close" type="button" title="Close" aria-label="Close node dialog"
        onMouseDown=${(event) => event.preventDefault()} onClick=${() => { void onClose(); }}>✕</button>`}
    </div>
    <${Section} title=${type === 'start' || type === 'end' ? 'label / doc' : 'node'}>
      ${field('meta.name', 'label', node.name, (draft, value) => setNodeText(draft, 'name', value))}
      ${type !== 'start' && type !== 'end' && field('meta.description', 'description', node.description, (draft, value) => setNodeText(draft, 'description', value))}
      ${field('meta.doc', 'doc', node.doc, (draft, value) => setNodeText(draft, 'doc', value), { multiline: true })}
    </${Section}>
    ${type === 'task' && html`
      <${Section} title="plan">
        ${stageToggle('plan.enabled', 'plan before work', !!node.plan, 'plan')}
        ${node.plan && html`${select('plan.approval', 'approval', PLAN_APPROVAL_MODES, node.plan.approval || 'auto', (draft, value) => setPlanApproval(draft, value))}${performer('plan.performer', (draft) => draft.plan.performer, node.plan.performer)}`}
      </${Section}>
      <${Section} title="work">
        ${!node.performer && !readOnly
          ? html`<button class="process-action process-node-add" type="button" onClick=${() => commit('work.add', (draft) => { draft.performer = defaultPerformer('agent'); })}>add work performer</button>`
          : performer('work.performer', (draft) => { if (!draft.performer) draft.performer = defaultPerformer('agent'); return draft.performer; }, node.performer || defaultPerformer('agent'))}
      </${Section}>
      <${Section} title="checks">
        ${(node.checks || []).map((check, index) => html`<div key=${index} class="process-node-check">
          <div class="process-node-check-head">
            ${field(`checks.${index}.id`, 'check id', check.id, (draft, value) => setCheckID(draft, index, value))}<span class="spacer"></span>
            ${!readOnly && html`<button class="process-action" type="button" title="Move check up" aria-label=${`Move check ${check.id} up`} onClick=${() => commit(`checks.${index}.up`, (draft) => moveCheck(draft, index, -1))}>↑</button><button class="process-action" type="button" title="Move check down" aria-label=${`Move check ${check.id} down`} onClick=${() => commit(`checks.${index}.down`, (draft) => moveCheck(draft, index, 1))}>↓</button><button class="process-action process-action-danger" type="button" aria-label=${`Remove check ${check.id}`} onClick=${() => commit(`checks.${index}.remove`, (draft) => removeCheck(draft, index))}>remove</button>`}
          </div>${performer(`checks.${index}.performer`, (draft) => draft.checks[index].performer, check.performer)}
        </div>`)}
        ${!(node.checks || []).length && html`<p class="process-node-empty">No checks: work settles without gate verdicts.</p>`}
        ${!readOnly && html`<button class="process-action process-node-add" type="button" onClick=${() => commit('checks.add', (draft) => addCheck(draft))}>+ add check</button>`}
      </${Section}>
      <${Section} title="review">${stageToggle('review.enabled', 'review gate after checks', !!node.review, 'review')}${node.review && performer('review.performer', (draft) => draft.review.performer, node.review.performer)}</${Section}>
      <${Section} title="retry policy">${field('retry.maxAttempts', 'max attempts', node.retry?.maxAttempts === undefined ? '' : String(node.retry.maxAttempts), (draft, value) => setRetryField(draft, 'maxAttempts', value), { placeholder: 'unset' })}${select('retry.onFail', 'on fail', RETRY_ON_FAIL_MODES, node.retry?.onFail, (draft, value) => setRetryField(draft, 'onFail', value), { blankLabel: 'default (fresh-attempt)' })}</${Section}>
      <${Section} title="captures">${field('captures', 'published outputs', formatLines(node.captures), (draft, value) => setCaptures(draft, value), { multiline: true, placeholder: 'one-name-per-line' })}</${Section}>
    `}
    ${type === 'decision' && html`<${Section} title="decider">${performer('decision.performer', (draft) => { if (!draft.performer) draft.performer = defaultPerformer('human'); return draft.performer; }, node.performer || defaultPerformer('human'), { choiceRouting: false })}</${Section}>`}
    ${type === 'wait' && html`<${Section} title="wait / timer">${field('wait.duration', 'duration', node.wait?.duration, (draft, value) => setWaitField(draft, 'duration', value), { placeholder: '30m' })}${field('wait.until', 'until', node.wait?.until, (draft, value) => setWaitField(draft, 'until', value))}${field('wait.signal', 'signal', node.wait?.signal, (draft, value) => setWaitField(draft, 'signal', value))}</${Section}>`}
    ${type !== 'end' && html`<${Section} title=${type === 'decision' ? 'choices → edges' : 'edges'}><div class="process-node-edges">${edges.map((edge) => html`<div key=${edge.outcome} class="process-node-edge-row"><span class="process-node-edge-outcome">${edge.outcome}</span><span class="process-node-edge-arrow">→</span><span class="process-node-edge-target">${edge.to}</span></div>`)}${!edges.length && html`<p class="process-node-empty">No outgoing edges yet.</p>`}</div><p class="process-node-edges-note">Edges are edited on the canvas, not in this dialog.</p></${Section}>`}
  </div>`;
}

export function NodeDialog({ model, nodeId, mode = 'edit', onMutated, complete, confirmDiscard, registerHandle }) {
  const original = useRef(structuredClone(model.node(nodeId)));
  const draftRef = useRef(structuredClone(original.current));
  const invalid = useRef(new Set());
  const dirty = useRef(false);
  const sharedClose = useRef(null);
  const [, redraw] = useState(0);
  const [status, setStatus] = useState('');
  const recomputeDirty = useCallback(() => {
    dirty.current = mode === 'edit' && (invalid.current.size > 0
      || JSON.stringify(draftRef.current) !== JSON.stringify(original.current));
  }, [mode]);
  recomputeDirty();
  const commit = (key, mutate) => {
    if (invalid.current.size && !invalid.current.has(key)) {
      setStatus('Correct the highlighted invalid field first; this change was not applied.');
      return { accepted: false, blocked: true };
    }
    try {
      const next = structuredClone(draftRef.current);
      mutate(next);
      draftRef.current = next;
      invalid.current.delete(key);
      setStatus('');
      recomputeDirty();
      redraw((value) => value + 1);
      return { accepted: true };
    } catch (error) {
      invalid.current.add(key);
      recomputeDirty();
      setStatus(error.message);
      return { accepted: false, invalid: true };
    }
  };
  const flushActive = useCallback(() => {
    const active = document.activeElement;
    if (active?.closest?.('.process-node-dialog') && active.matches?.('input, textarea, select')) {
      active.dispatchEvent(new Event('change', { bubbles: true, cancelable: true }));
    }
    return invalid.current.size === 0;
  }, []);
  const prepareClose = useCallback(() => {
    flushActive();
    recomputeDirty();
    return true;
  }, [flushActive, recomputeDirty]);
  const requestClose = useCallback(async () => {
    return sharedClose.current?.() ?? false;
  }, []);
  const save = () => {
    if (!flushActive() || mode !== 'edit') return false;
    try {
      const changed = model.updateNode(nodeId, (node) => replaceNode(node, draftRef.current));
      if (changed) onMutated?.();
      complete(true);
      return true;
    } catch (error) { setStatus(error.message); return false; }
  };
  useLayoutEffect(() => {
    const registered = registerHandle;
    const cleanup = registered?.({ isDirty: () => dirty.current, requestClose });
    return () => {
      if (typeof cleanup === 'function') cleanup();
      else registered?.(null);
    };
  }, [registerHandle, requestClose]);
  const registerSharedClose = useCallback((close) => {
    sharedClose.current = close;
    return () => { if (sharedClose.current === close) sharedClose.current = null; };
  }, []);
  return html`<${Overlay}
    id="process-node-modal" dialogClass="modal process-node-dialog" overlayClass="process-node-modal"
    ariaLabel=${`Node ${nodeId}`}
    onClose=${complete} beforeClose=${prepareClose} dirty=${() => dirty.current} blocked=${false}
    confirmDiscard=${confirmDiscard} onCloseError=${(error) => setStatus(`Discard confirmation failed: ${error?.message || String(error)}`)}
    registerClose=${registerSharedClose}
    resizeKey=${NODE_DIALOG_SIZE_PREF} fitContent=${false} onSubmitHotkey=${save}
  >
    <div class="process-node-dialog-body">
      <${NodeDetail} model=${model} nodeId=${nodeId} node=${draftRef.current} mode=${mode}
        commit=${mode === 'edit' ? commit : null} invalid=${invalid.current} onClose=${requestClose} />
      <p class=${`process-node-status${status ? ' is-error' : ''}`} role="status">${status}</p>
    </div>
    <div class="modal-buttons process-node-dialog-actions">
      <button class="process-node-cancel" type="button" onClick=${requestClose}>${mode === 'edit' ? 'Cancel' : 'Close'}</button>
      ${mode === 'edit' && html`<button class="primary process-node-save" type="button" onClick=${save}>Save</button>`}
    </div>
  </${Overlay}>`;
}

// Standalone compatibility for viewer/tests. Production editor dialogs are
// rendered by process-editor-island in the editor's Preact tree.
export function openNodeDialog({ model, nodeId, mode = 'edit', onMutated, onClosed, confirmDiscard = async () => false }) {
  const host = document.body.appendChild(document.createElement('div'));
  let handle = null;
  let closed = false;
  const complete = (result) => {
    if (closed) return;
    closed = true;
    render(null, host);
    host.remove();
    onClosed?.(result);
  };
  render(html`<${NodeDialog} model=${model} nodeId=${nodeId} mode=${mode}
    onMutated=${onMutated} complete=${complete} confirmDiscard=${confirmDiscard}
    registerHandle=${(value) => { handle = value; }} />`, host);
  const dispose = () => complete(null);
  dispose.isDirty = () => !!handle?.isDirty?.();
  dispose.requestClose = () => handle?.requestClose?.() || Promise.resolve(true);
  return dispose;
}
