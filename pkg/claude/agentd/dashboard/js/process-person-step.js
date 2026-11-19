// A step a person performs, as the run view lists it among the awaited
// decisions (kind "task"): its ask and instructions, the choices it is
// finished with, and the outputs a passing choice fills in. The desktop run
// view, the phone and the Terminals popover all present it from here.
import { h } from 'preact';
import htm from 'htm';
import { Input } from './ui-components.js';

const html = htm.bind(h);

export function isPersonStep(decision) {
  return decision?.kind === 'task';
}

// personChoicePasses reports whether a choice finishes the step as done.
export function personChoicePasses(decision, choice) {
  return (decision?.passing || []).includes(choice);
}

// personChoiceLabel is a choice as a button says it. The default pair reads
// as plain words; a template's own choices are said as written.
export function personChoiceLabel(choice) {
  const raw = String(choice);
  if (raw === 'done') return 'Done';
  if (raw === 'failed') return "Couldn't do it";
  return raw;
}

// missingOutputs are the outputs a passing choice still needs filled in.
export function missingOutputs(decision, values) {
  return (decision?.outputs || []).filter((name) => !String(values?.[name] || '').trim());
}

// filledOutputs is the outputs to send with a choice: all of them for a
// passing one, none for a failing one (the daemon refuses none either way).
export function filledOutputs(decision, choice, values) {
  if (!personChoicePasses(decision, choice)) return undefined;
  const names = decision?.outputs || [];
  if (!names.length) return undefined;
  return Object.fromEntries(names.map((name) => [name, String(values?.[name] || '').trim()]));
}

// PersonStepText is what the person is asked and told.
export function PersonStepText({ decision, class: cls = 'pps-text' }) {
  if (!decision?.ask && !decision?.prompt) return null;
  return html`<div class=${cls} data-person-step-text>
    ${decision.ask && html`<p class="pps-ask">${decision.ask}</p>`}
    ${decision.prompt && html`<p class="pps-prompt">${decision.prompt}</p>`}
  </div>`;
}

// PersonStepOutputs is a field per output the step asks for.
export function PersonStepOutputs({ decision, values, onChange, disabled = false, idPrefix = 'pps' }) {
  const names = decision?.outputs || [];
  if (!names.length) return null;
  return html`<div class="pps-outputs" data-person-step-outputs>
    ${names.map((name) => {
      const id = `${idPrefix}-${decision.nodeId}-${name}`;
      return html`<label key=${name} class="pps-output" for=${id}>
        <span class="pps-output-name">${name}</span>
        <${Input} size="sm" id=${id} name=${id} maxlength="16384" value=${values?.[name] || ''} disabled=${disabled}
          data-person-output=${name} onInput=${(event) => onChange({ ...values, [name]: event.currentTarget.value })} />
      </label>`;
    })}
  </div>`;
}
