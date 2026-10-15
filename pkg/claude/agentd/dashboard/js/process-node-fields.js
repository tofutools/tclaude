// process-node-fields.js -- the node editor's field controls, shared by the
// docked node inspector's blocks (process-node-inspector.js). The pure
// mutation vocabulary stays in process-node-form.js; these components only
// render a value and hand a raw edit to `apply(fieldKey, raw)`.
//
// `apply` returns `{ blocked: true }` when the edit was refused (a mutator
// threw, or the node is read-only); the field then snaps back to the model
// value instead of keeping text the model never accepted.

import { h } from 'preact';
import { useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  PERFORMER_KINDS, performerFieldsFor, setPerformerKind, setPerformerField,
  setChoiceOutcome, setContactField, setFallbackField, formatLines, FALLBACK_ACTIONS,
} from './process-node-form.js';

const html = htm.bind(h);

export function NodeField({
  fieldKey, label, value = '', apply, readOnly, multiline = false,
  hint = '', placeholder = '', invalid = false, autofocus = false,
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
    // A no-op commit is not free: mutators normalize (trim) as they apply, so
    // re-applying an untouched value can rewrite the node and read as dirty.
    if (current === String(value ?? '')) return;
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
    <${Tag} ref=${input} class=${multiline ? 'process-node-textarea process-scroll-surface' : 'process-node-input'}
      type=${multiline ? undefined : 'text'} rows=${multiline ? 3 : undefined}
      spellcheck="false" placeholder=${placeholder} disabled=${readOnly} autofocus=${autofocus}
      value=${raw} aria-invalid=${invalid ? 'true' : undefined} data-node-field=${fieldKey}
      onCompositionStart=${() => { composing.current = true; }}
      onCompositionEnd=${(event) => { composing.current = false; setRaw(event.currentTarget.value); }}
      onInput=${(event) => setRaw(event.currentTarget.value)} onChange=${commit} />
  </label>`;
}

export function NodeSelect({ fieldKey, label, options, value, apply, readOnly, blankLabel = null, invalid = false }) {
  const selected = options.includes(value) ? value : (blankLabel !== null ? '' : options[0]);
  return html`<label class="field process-node-field"><span class="process-node-field-label">${label}</span>
    <select class="process-node-select" aria-label=${label} disabled=${readOnly} data-node-field=${fieldKey}
      value=${selected} aria-invalid=${invalid ? 'true' : undefined}
      onChange=${(event) => apply?.(fieldKey, event.currentTarget.value)}>
      ${blankLabel !== null && html`<option value="">${blankLabel}</option>`}
      ${options.map((option) => html`<option key=${option} value=${option}>${option}</option>`)}
    </select>
  </label>`;
}

// PerformerEditor is the ONE shared performer editor (the uniform performer
// contract in process-node-form.js): kind plus the descriptor-declared fields
// of that kind. `only` / `omit` slice the field list so blocks can split one
// performer across Performer, Prompt and Bounds; `contact` renders the
// contact schedule.
export function PerformerEditor({
  performer, locate, path, commit, readOnly, choiceRouting = true,
  only = null, omit = [], showKind = true, contact = false,
}) {
  const kind = typeof performer?.kind === 'string' && performer.kind ? performer.kind : 'agent';
  const known = PERFORMER_KINDS.includes(kind);
  const mutate = (key, value, operation) => commit(`${path}.${key}`, (draft) => operation(locate(draft), value));
  const skip = new Set(omit);
  const fields = (known ? performerFieldsFor(kind) : [])
    .filter((field) => (only ? only.includes(field.key) : !skip.has(field.key)));
  const schedule = performer?.contact || {};
  return html`<div class="process-performer-editor">
    ${showKind && html`<${NodeSelect} fieldKey=${`${path}.kind`} label="kind"
      options=${known ? PERFORMER_KINDS : [...PERFORMER_KINDS, kind]} value=${kind} readOnly=${readOnly}
      apply=${(key, value) => mutate('kind', value, setPerformerKind)} />`}
    ${showKind && !known && html`<p class="process-node-empty">Unknown performer kind ${kind}: validation rejects it — pick a supported kind.</p>`}
    ${fields.map((field) => {
      if (field.outcomeMap) {
        if (!choiceRouting || !(performer?.choices || []).length) return null;
        return html`<div key=${field.key} class="process-choice-outcomes" title=${field.hint}>
          <span class="process-node-field-label">${field.label}</span>
          ${performer.choices.map((choice) => html`<${NodeSelect}
            key=${choice} fieldKey=${`${path}.choice.${choice}`} label=${choice}
            options=${['pass', 'fail']} value=${performer.choiceOutcomes?.[choice] || 'pass'} readOnly=${readOnly}
            apply=${(key, value) => mutate(`choice.${choice}`, value, (slot, next) => setChoiceOutcome(slot, choice, next))} />`)}
        </div>`;
      }
      if (field.flag) {
        return html`<div key=${field.key} title=${field.hint}><${NodeSelect} fieldKey=${`${path}.${field.key}`} label=${field.label}
          options=${['no', 'yes']} value=${performer?.[field.key] ? 'yes' : 'no'} readOnly=${readOnly}
          apply=${(key, next) => mutate(field.key, next, (slot, raw) => setPerformerField(slot, field.key, raw))} /></div>`;
      }
      if (field.fallback) {
        const fallback = performer?.fallback || {};
        return html`<div key=${field.key} class="process-node-fallback" title=${field.hint}>
          <${NodeSelect} fieldKey=${`${path}.fallback.action`} label=${field.label} options=${FALLBACK_ACTIONS}
            value=${fallback.action || ''} blankLabel="process default" readOnly=${readOnly}
            apply=${(key, next) => mutate('fallback.action', next, (slot, raw) => setFallbackField(slot, 'action', raw))} />
          ${fallback.action === 'spawn' && html`<${NodeField} fieldKey=${`${path}.fallback.profile`} label="spawn profile"
            value=${fallback.profile || ''} placeholder="group default" readOnly=${readOnly}
            apply=${(key, next) => mutate('fallback.profile', next, (slot, raw) => setFallbackField(slot, 'profile', raw))} />`}
        </div>`;
      }
      const value = field.list ? formatLines(performer?.[field.key]) : performer?.[field.key];
      return html`<div key=${field.key}>
        <${NodeField} fieldKey=${`${path}.${field.key}`} label=${field.label} value=${value || ''}
          multiline=${!!(field.multiline || field.list)} hint=${field.hint} readOnly=${readOnly}
          apply=${(key, next) => mutate(field.key, next, (slot, raw) => setPerformerField(slot, field.key, raw, { choiceRouting }))} />
        ${field.key === 'run' && html`<p class="process-node-security-note">⚠ Program performers are command execution: this command runs on the host when the node activates.</p>`}
      </div>`;
    })}
    ${contact && html`<div class="process-node-contact" title="Contact schedule for this slot: nudge cadence, budget, escalation target">
      <span class="process-node-field-label">contact schedule</span>
      ${[
        ['cadence', schedule.cadence || '', '30m'],
        ['budget', schedule.budget === undefined ? '' : String(schedule.budget), '5'],
        ['escalationTarget', schedule.escalationTarget || '', 'human:operator'],
      ].map(([key, value, placeholder]) => html`<${NodeField} key=${key}
        fieldKey=${`${path}.contact.${key}`} label=${key === 'escalationTarget' ? 'escalate to' : key}
        value=${value} placeholder=${placeholder} readOnly=${readOnly}
        apply=${(fieldKey, next) => mutate(`contact.${key}`, next, (slot, raw) => setContactField(slot, key, raw))} />`)}
    </div>`}
  </div>`;
}
