// process-rule-cases.js: a rule decision's cases as rows, shared by the
// inline editor's inspector (groups-mini-editor.js) and the full editor's
// (process-node-inspector.js). Each case is rows of what it reads, a
// comparison and a value, joined by and, and the outcome it picks; then
// else, and the whole rule in plain words. A condition the rows cannot show
// (or, not, parentheses) shows as written; the full editor offers it as text.

import { h } from 'preact';
import htm from 'htm';
import { OP_WORDS, blankRow, fromRows, ruleWords, toRows } from './process-rules.js';
import { TypedField } from './typed-field.js';
import { Button, Select } from './ui-components.js';

const html = htm.bind(h);

// A value the grammar cannot quote: it has no escapes.
const unquotable = (value) => value.includes("'") && value.includes('"');

// RuleCases renders `rule` ({cases, else}) and calls onChange({cases, else})
// with every edit. operands come from ruleOperands; loopsBack(outcome) marks
// an outcome that loops back (↺); stepName names a step id in the words.
// With `editText`, a case also shows its condition as text to edit, which is
// how a condition with or, not or parentheses is written.
export function RuleCases({ rule, outcomes, operands, loopsBack = () => false, stepName = (id) => id, onChange, readOnly = false, editText = false, label = 'Decides', fieldClass = 'gm-field' }) {
  const cases = rule.cases || [];
  const apply = (nextCases, otherwise = rule.else) => onChange({ cases: nextCases, else: otherwise });
  const setCase = (index, change) => apply(cases.map((c, i) => (i === index ? { ...c, ...change } : c)));
  const outcomeSelect = (value, aria, pick) => html`<${Select} size="sm" aria-label=${aria} value=${value} disabled=${readOnly} onChange=${(event) => pick(event.currentTarget.value)}>
    ${!outcomes.includes(value) && html`<option value=${value}>${value || 'pick one'}</option>`}
    ${outcomes.map((outcome) => html`<option key=${outcome} value=${outcome}>${outcome}${loopsBack(outcome) ? ' ↺' : ''}</option>`)}
  <//>`;
  const remove = (index) => cases.length > 1 && !readOnly && html`<${Button} size="sm" icon=${true} variant="ghost danger" aria-label="Remove this case"
    onClick=${() => apply(cases.filter((_, j) => j !== index))}>✕<//>`;
  const row = (index, rows, r, i) => {
    const operand = operands.find((o) => o.value === r.operand);
    const setRow = (change) => setCase(index, { when: fromRows(rows.map((other, j) => (j === i ? { ...other, ...change } : other))) });
    return html`<div key=${i} class="gm-rule-row" data-mini-rule-row=${`${index}.${i}`}>
      <span class="gm-rule-and">${i === 0 ? 'if' : 'and'}</span>
      <${Select} size="sm" aria-label="What the rule reads" value=${r.operand} disabled=${readOnly} onChange=${(event) => {
        const picked = operands.find((o) => o.value === event.currentTarget.value);
        setRow({ operand: event.currentTarget.value, ...(picked?.values ? { value: picked.values[0], cmp: '==' } : {}) });
      }}>
        ${!operand && html`<option value=${r.operand}>${r.operand}</option>`}
        ${operands.map((o) => html`<option key=${o.value} value=${o.value}>${o.label}</option>`)}
      <//>
      <${Select} size="sm" aria-label="How it compares" value=${r.cmp} disabled=${readOnly} onChange=${(event) => setRow({ cmp: event.currentTarget.value })}>
        ${Object.entries(OP_WORDS).filter(([cmp]) => !operand?.values || cmp === '==' || cmp === '!=').map(([cmp, words]) => html`<option key=${cmp} value=${cmp}>${words}</option>`)}
      <//>
      ${operand?.values
    ? html`<${Select} size="sm" aria-label="The value" value=${r.value} disabled=${readOnly} onChange=${(event) => setRow({ value: event.currentTarget.value })}>
          ${!operand.values.includes(r.value) && html`<option value=${r.value}>${r.value}</option>`}
          ${operand.values.map((value) => html`<option key=${value} value=${value}>${value}</option>`)}
        <//>`
    : html`<${TypedField} name="gm-rule-value" type="text" class="gm-rule-value" aria-label="The value" value=${r.value} disabled=${readOnly}
          title="A value cannot hold both ' and \""
          onChange=${(event) => { const value = event.currentTarget.value.trim(); setRow({ value: value && !unquotable(value) ? value : r.value }); }} />`}
      ${i === 0 && html`<span class="gm-rule-arrow">→</span>${outcomeSelect(cases[index].then, `Case ${index + 1} picks`, (then) => setCase(index, { then }))}`}
      ${!readOnly && (rows.length > 1 || cases.length > 1) && html`<${Button} size="sm" icon=${true} variant="ghost danger" aria-label=${rows.length > 1 ? 'Remove this condition' : 'Remove this case'}
        onClick=${() => (rows.length > 1 ? setCase(index, { when: fromRows(rows.filter((_, j) => j !== i)) }) : apply(cases.filter((_, j) => j !== index)))}>✕<//>`}
    </div>`;
  };
  const whenField = (c, index) => html`<${TypedField} name="gm-rule-when" type="text" class="gm-rule-when" aria-label=${`Case ${index + 1} condition`}
    value=${c.when} disabled=${readOnly} data-mini-rule-when=${index} onChange=${(event) => setCase(index, { when: event.currentTarget.value.trim() || c.when })} />`;
  const asText = (c, index) => html`<div class="gm-rule-row">
    <span class="gm-rule-and">if</span>
    ${editText ? whenField(c, index) : html`<code class="gm-rule-text" title="Uses or, not or parentheses: edit it in the full editor">${c.when}</code>`}
    <span class="gm-rule-arrow">→</span>${outcomeSelect(c.then, `Case ${index + 1} picks`, (then) => setCase(index, { then }))}
    ${remove(index)}</div>`;
  return html`<div class=${fieldClass} data-mini-rule><span>${label}</span><div class="gm-rule">
    ${cases.map((c, index) => {
    const rows = toRows(c.when);
    return html`<div key=${index} class="gm-rule-case" data-mini-rule-case=${index}>
      ${rows
    ? html`${rows.map((r, i) => row(index, rows, r, i))}
          ${!readOnly && html`<${Button} size="sm" variant="link" class="gq-link" data-mini-rule-and=${index} onClick=${() => setCase(index, { when: fromRows([...rows, blankRow(operands, rows)]) })}>+ and<//>`}
          ${editText && html`<div class="gm-rule-row gm-rule-as-text"><span class="gm-rule-and">as text</span>${whenField(c, index)}</div>`}`
    : asText(c, index)}
    </div>`;
  })}
    ${!readOnly && html`<${Button} size="sm" variant="link" class="gq-link" data-mini-rule-add onClick=${() => apply([...cases, { when: fromRows([blankRow(operands)]), then: outcomes[0] }])}>+ another case<//>`}
    <div class="gm-rule-row" data-mini-rule-else><span class="gm-rule-and">else</span><span class="gm-rule-arrow">→</span>${outcomeSelect(rule.else, 'Otherwise', (otherwise) => apply(cases, otherwise))}</div>
    <div class="gm-rule-words" data-mini-rule-words>${ruleWords(rule, stepName)}</div>
  </div></div>`;
}
