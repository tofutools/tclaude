// Optional settings (inherit / on / off, or inherit / a value) use
// OptionalSelect from js/ui-components.js. Forms used to hand-write the same
// three options, each with its own spelling of the unset choice and its own
// '' / '1' / '0' or 'inherit' values: the profile editor's TRI_OPTIONS arrays,
// the spawn dialog's fast mode, group settings' remote policy, the sandbox
// editor's override selects and the hub's bool settings.
//
// A hand-written tri-state option (value "1", "0" or "inherit", in markup or as
// a ['1', …] option pair) outside ui-components.js is a ratchet:
// ui-optional-select-baseline.json counts the ones left per file, which are
// not optional-field selects (the network baseline, whose "No override" sits
// last, and its validator's list; the harness policy's per-rule decision; the
// logs "since" filter's 0 = all) or are values of an optional select rather
// than its unset choice (the filesystem root's 'inherit'). A new
// one fails, and a removed one must leave the baseline. Regenerate with
//   UPDATE_UI_OPTIONAL_SELECT_BASELINE=1 node --test jstest/ui-optional-select.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { createPreactHarness } from './preact-harness.mjs';

const JS = new URL('../dashboard/js/', import.meta.url);
const BASELINE = new URL('./ui-optional-select-baseline.json', import.meta.url);
const OWNER = 'ui-components.js';

// handWrittenTriOptions counts the tri-state options a source hand-writes.
export function handWrittenTriOptions(source) {
  const markup = source.match(/<option\s+value=(?:"(?:1|0|inherit)"|'(?:1|0|inherit)')\s*>/g)?.length ?? 0;
  const pairs = source.match(/\[\s*(?:'(?:1|0|inherit)'|"(?:1|0|inherit)")\s*,\s*['"`]/g)?.length ?? 0;
  return markup + pairs;
}

test('the scanner counts hand-written tri-state options', () => {
  assert.equal(handWrittenTriOptions('html`<option value="">Default</option><option value="1">On</option><option value="0">Off</option>`'), 2);
  assert.equal(handWrittenTriOptions("const X = [['', 'Default'], ['1', 'on'], ['0', 'off']];"), 2);
  assert.equal(handWrittenTriOptions('html`<option value="inherit">inherit profile</option>`'), 1);
  assert.equal(handWrittenTriOptions("if (['inherit', 'allow'].includes(x)) {}"), 1);
  assert.equal(handWrittenTriOptions("<${OptionalSelect} unset=\"Default\" on=\"On\" off=\"Off\"/>"), 0);
  assert.equal(handWrittenTriOptions("const n = [1, 0]; const o = { value: '1' };"), 0);
});

test('optional settings use OptionalSelect, not hand-written tri-state options', () => {
  const found = {};
  for (const name of readdirSync(JS).filter((f) => f.endsWith('.js') && f !== OWNER).sort()) {
    const count = handWrittenTriOptions(readFileSync(new URL(name, JS), 'utf8'));
    if (count) found[name] = count;
  }
  if (process.env.UPDATE_UI_OPTIONAL_SELECT_BASELINE) writeFileSync(BASELINE, `${JSON.stringify(found, null, 2)}\n`);
  const baseline = JSON.parse(readFileSync(BASELINE, 'utf8'));
  const grown = Object.entries(found).filter(([name, count]) => count > (baseline[name] ?? 0)).map(([name, count]) => `${name}: ${count}`);
  assert.deepEqual(grown, [], 'render an optional (inherit / on / off) setting with OptionalSelect from js/ui-components.js');
  const shrunk = Object.entries(baseline).filter(([name, count]) => (found[name] ?? 0) < count).map(([name]) => name);
  assert.deepEqual(shrunk, [], 'these files dropped hand-written tri-state options: lower jstest/ui-optional-select-baseline.json');
});

function options(select) {
  return [...select.options].map((option) => [option.value, option.textContent]);
}

function selectedValue(select) {
  return select.getAttribute('value')
    ?? Array.from(select.options).find((option) => option.selected)?.value
    ?? select.value
    ?? '';
}

function choose(select, value) {
  for (const option of select.options) {
    if (option.value === value) option.setAttribute('selected', '');
    else option.removeAttribute('selected');
  }
  Object.defineProperty(select, 'value', { configurable: true, writable: true, value });
}

test('OptionalSelect renders unset / on / off, or unset / values, and reports the value', async (t) => {
  const harness = await createPreactHarness(t);
  const ui = await harness.importDashboardModule('js/ui-components.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const changes = [];
  await harness.mount(harness.html`<div>
    <${ui.OptionalSelect} id="tri" value="0" unset="Default (off)" onChange=${(value) => changes.push(['tri', value])} disabled title="why"/>
    <${ui.OptionalSelect} id="relabel" value=${null} on="API" off="send-keys" size="sm"/>
    <${ui.OptionalSelect} id="policy" value="inherit" unsetValue="inherit" unset="inherit profile" options=${[['optin', 'opt in'], ['deny', 'deny']]}
      onChange=${(value) => changes.push(['policy', value])}/>
  </div>`, host);
  const tri = host.querySelector('#tri');
  assert.deepEqual(options(tri), [['', 'Default (off)'], ['1', 'on'], ['0', 'off']]);
  assert.equal(tri.className, 'ui-select', 'it is the standard select');
  assert.equal(tri.disabled, true);
  assert.equal(tri.getAttribute('title'), 'why');
  assert.ok(tri.hasAttribute('data-optional-select'));
  const relabel = host.querySelector('#relabel');
  assert.deepEqual(options(relabel), [['', 'Default'], ['1', 'API'], ['0', 'send-keys']]);
  assert.equal(relabel.className, 'ui-select ui-sm');
  assert.equal(selectedValue(relabel), '', 'a missing value shows the unset choice');
  assert.equal(selectedValue(tri), '0');
  const policy = host.querySelector('#policy');
  assert.deepEqual(options(policy), [['inherit', 'inherit profile'], ['optin', 'opt in'], ['deny', 'deny']]);
  choose(tri, '');
  await harness.act(() => harness.fireEvent(tri, 'change'));
  choose(policy, 'deny');
  await harness.act(() => harness.fireEvent(policy, 'change'));
  assert.deepEqual(changes, [['tri', ''], ['policy', 'deny']], 'onChange gets the value, not the event');
});
