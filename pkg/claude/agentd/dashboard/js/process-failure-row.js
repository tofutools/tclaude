// process-failure-row.js: a step's "If it fails", shared by the inline
// editor's inspector (groups-mini-editor.js) and the full editor's
// (process-node-inspector.js). The modes are stepFailure's
// (process-step-list.js).

import { h } from 'preact';
import htm from 'htm';
import { FAILURE_RETRIES_MAX } from './process-step-list.js';
import { TypedField } from './typed-field.js';
import { Select } from './ui-components.js';

const html = htm.bind(h);

// FailureRow is a step's "If it fails" (stepFailure): fail the run, the
// default; hold it for the operator; retry N times, then hold it; or go on
// as failed (mayFail), for a rule decision after it to route. The N× box
// shows only for retry.
// On a step with plan, check or review stages the budget is its one rework
// budget: a stage sending the work back counts as a failure too.
export function FailureRow({ id, failure, staged = false, onChange }) {
  return html`<div class="gm-field" data-mini-failure=${id}><span>If it fails</span><div class="gm-failure">
      <div class="gq-pick-pair">
        <${Select} size="sm" aria-label="If this step fails" value=${failure.mode} data-failure=${failure.mode}
          onChange=${(event) => onChange({ mode: event.currentTarget.value, retries: failure.retries || FAILURE_RETRIES_DEFAULT })}>
          <option value="fail">fail the run</option>
          <option value="hold">hold it for me</option>
          <option value="retry">retry, then hold it for me</option>
          ${!staged && html`<option value="go-on">go on as failed, for a rule to see</option>`}
        <//>
        ${failure.mode === 'retry' && html`<label class="gm-retries" title="How many times it is tried again before it waits for you">
          <${TypedField} name="gm-retries" type="number" min="1" max=${FAILURE_RETRIES_MAX} aria-label="Retries" data-mini-retries value=${failure.retries}
            onChange=${(event) => onChange({ mode: 'retry', retries: event.currentTarget.value })} />×</label>`}
      </div>
      ${failure.mode === 'go-on' && html`<span class="gq-dim" data-mini-failure-note>The run goes on; a rule after it can read that it failed.</span>`}
      ${(failure.mode === 'hold' || failure.mode === 'retry') && html`<span class="gq-dim" data-mini-failure-note>Held, it waits for you to retry, skip or cancel it.${staged ? ' Its check or review sending it back counts as a failure too.' : ''}</span>`}
    </div></div>`;
}

// FAILURE_RETRIES_DEFAULT is the retries "retry, then hold it for me" starts at.
export const FAILURE_RETRIES_DEFAULT = 2;
