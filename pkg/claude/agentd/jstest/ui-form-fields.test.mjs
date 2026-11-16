// Every form field on the dashboard has a name or an id. Chrome's Issues panel
// flags a field with neither ("A form field element should have an id or
// name attribute"): it cannot be autofilled, and nothing tells it apart from
// its neighbours. The shared Input / Select / Textarea controls (and the
// config form's routed controls) name a field themselves from its label, so
// this holds the line on the raw <input>, <select> and <textarea> tags in the
// page modules and dashboard.html. A tag that spreads props (...${rest}) is a
// component passing a name through, and is left to that component.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { createPreactHarness } from './preact-harness.mjs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const read = (file) => readFileSync(new URL(file, DASHBOARD), 'utf8');
const lineOf = (source, index) => source.slice(0, index).split('\n').length;

// tagEnd is the index just past the `>` closing the tag opened at start,
// stepping over quoted attribute values and ${…} expressions (which hold
// arrows and nested templates).
function tagEnd(source, start) {
  let depth = 0;
  let quote = '';
  for (let i = start; i < source.length; i += 1) {
    const c = source[i];
    if (quote) {
      if (c === quote) quote = '';
      continue;
    }
    if (depth === 0 && (c === '"' || c === "'")) quote = c;
    else if (c === '$' && source[i + 1] === '{') { depth += 1; i += 1; }
    else if (depth > 0 && c === '{') depth += 1;
    else if (depth > 0 && c === '}') depth -= 1;
    else if (depth === 0 && c === '>') return i + 1;
  }
  return source.length;
}

// unnamedFields lists the lines of raw form-field tags with neither a name nor
// an id, skipping ones in a // line comment or an HTML comment.
export function unnamedFields(source) {
  const found = [];
  for (const match of source.matchAll(/<(input|select|textarea)\b/g)) {
    const lineStart = source.lastIndexOf('\n', match.index) + 1;
    const before = source.slice(lineStart, match.index);
    if (before.includes('//') || /^\s*\*/.test(before)) continue;
    const open = source.lastIndexOf('<!--', match.index);
    if (open >= 0 && source.indexOf('-->', open) > match.index) continue;
    const attrs = source.slice(match.index, tagEnd(source, match.index));
    if (/(?<![\w.-])(name|id)=/.test(attrs) || attrs.includes('...${') || /type="hidden"/.test(attrs)) continue;
    found.push(lineOf(source, match.index));
  }
  return found;
}

test('the scanner finds a form field with neither a name nor an id', () => {
  const source = [
    'html`<input type="text" aria-label="Find" />`;',
    'html`<input name="find" onKeyDown=${(e) => { if (e.key === "a") { go(); } }} />`;',
    'html`<select id="pick">${options}</select>`;',
    'html`<input ...${rest} />`;',
    'html`<input data-name="x" onInput=${(e) => set(e.target.value)} />`;',
    '// a <select> in prose',
    '<!-- the two <input>s stay adjacent -->',
    'html`<textarea class="note" />`;',
  ].join('\n');
  assert.deepEqual(unnamedFields(source), [1, 5, 8]);
});

test('every raw form field in the page modules and dashboard.html has a name or an id', () => {
  const offenders = [];
  const files = ['dashboard.html', ...readdirSync(new URL('js/', DASHBOARD)).filter((f) => f.endsWith('.js')).map((f) => `js/${f}`)];
  for (const file of files) {
    for (const line of unnamedFields(read(file))) offenders.push(`${file}:${line}`);
  }
  assert.deepEqual(offenders, [], 'give these fields a name (or an id): browsers flag a field with neither');
});

test('the shared controls and the config form name a field given neither', async (t) => {
  const harness = await createPreactHarness(t);
  const { Input, Select, Textarea } = await harness.importDashboardModule('js/ui-components.js');
  const view = await harness.mount(harness.html`<div>
    <${Input} aria-label="Filter the library" />
    <${Input} placeholder="from state" />
    <${Input} name="kept" aria-label="Ignored" />
    <${Input} id="by-id" />
    <${Select} />
    <${Textarea} aria-label="Why" />
  </div>`);
  const fields = [...view.container.querySelectorAll('input, select, textarea')];
  assert.deepEqual(fields.map((f) => f.getAttribute('name')), ['filter-the-library', 'from-state', 'kept', null, 'ui-select', 'why']);
  assert.equal(fields[3].id, 'by-id', 'an id is enough');
});
