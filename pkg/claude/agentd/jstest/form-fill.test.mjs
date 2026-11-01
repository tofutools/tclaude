// Tests for form fills (tcl-3ta7bl). The promises are the point: a fill goes
// through the field's own handler, never submits, never overwrites the operator,
// never touches a marked field, and can be undone.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { applyFill, filledIn, fillSummary, undoFill, watchForm } from '../dashboard/js/form-fill.js';
import { parseHTML } from './third_party/linkedom.mjs';

const ROW = `
  <input id="agent-spawn-name" value="">
  <textarea id="agent-spawn-init-msg"></textarea>
  <select id="agent-spawn-harness"><option value="claude">Claude Code</option><option value="codex">Codex CLI</option></select>
  <select id="agent-spawn-model"><option value="">Default</option><option value="opus">opus</option></select>
  <select id="agent-spawn-sandbox"><option value="inherit">inherit</option><option value="off">off</option></select>
  <select id="agent-spawn-approval"><option value="auto">auto</option><option value="yolo">yolo</option></select>
  <input id="agent-spawn-owner" type="checkbox">
  <input id="agent-spawn-descr" disabled value="">
  <input id="agent-spawn-role" data-capture-mask value="">
  <input id="agent-spawn-task" type="password" value="">
  <input id="agent-spawn-queue" value="">
  <input id="agent-spawn-cwd" class="unrendered" value="">
  <button id="agent-spawn-submit" type="submit">Spawn</button>`;

// page builds an open spawn form. stateful mimics a controlled field: its handler
// keeps what the form accepts, so a value only stays if the handler took it.
function page() {
  const { window } = parseHTML(`<!doctype html><html><body><div id="agent-spawn-modal">${ROW}</div></body></html>`);
  window.Event = window.Event || globalThis.Event;
  const root = window.document.getElementById('agent-spawn-modal');
  for (const el of root.querySelectorAll('input, select, textarea')) {
    el.getClientRects = () => (el.classList.contains('unrendered') ? [] : [{}]);
  }
  // linkedom has no settable select value and no checkbox click.
  for (const select of root.querySelectorAll('select')) {
    let current = select.querySelector('option')?.getAttribute('value') ?? '';
    Object.defineProperty(select, 'value', { get: () => current, set: (value) => { current = value; } });
    Object.defineProperty(select, 'options', { get: () => [...select.querySelectorAll('option')].map((option) => ({ value: option.getAttribute('value'), label: option.textContent, textContent: option.textContent })) });
  }
  for (const box of root.querySelectorAll('input[type="checkbox"]')) {
    let checked = false;
    Object.defineProperty(box, 'checked', { get: () => checked, set: (value) => { checked = value; } });
    box.click = () => { checked = !checked; box.dispatchEvent(new window.Event('change', { bubbles: true })); };
  }
  // A timer runs after the event's handlers, as in a browser.
  const later = [];
  const stop = watchForm('spawn', root, { win: window, setTimeoutImpl: (fn) => later.push(fn) });
  return { window, document: window.document, root, stop, tick: async () => {}, settle: () => { while (later.length) later.shift()(); } };
}

const fill = (p, fields, agent = 'Stewie') => applyFill({
  win: p.window, tick: p.tick, request: { form: 'spawn', agent, fields: Object.entries(fields).map(([field, value]) => ({ field, value })) },
});
const statuses = (result) => Object.fromEntries(result.fields.map((field) => [field.field, field.status]));

// typed is what the browser does for a person: the same events, marked trusted.
function typed(p, id, value) {
  const el = p.document.getElementById(id);
  el.value = value;
  const event = new p.window.Event('input', { bubbles: true });
  Object.defineProperty(event, 'isTrusted', { value: true });
  el.dispatchEvent(event);
}

test('a fill goes through the field\'s own handlers and is remembered', async () => {
  const p = page();
  const heard = [];
  p.document.getElementById('agent-spawn-name').addEventListener('input', (event) => heard.push(event.target.value));
  const result = await fill(p, { 'agent-spawn-name': 'reviewer' });
  assert.deepEqual(statuses(result), { 'agent-spawn-name': 'filled' });
  assert.deepEqual(heard, ['reviewer']);
  assert.equal(filledIn('spawn')[0].agent, 'Stewie');
  p.stop();
  assert.deepEqual(filledIn('spawn'), [], 'closing the form forgets it');
});

test('a select takes the label a capture prints, or its value, and nothing else', async () => {
  const p = page();
  const result = await fill(p, { 'agent-spawn-harness': 'Codex CLI', 'agent-spawn-model': 'opus', 'agent-spawn-effort': 'x' });
  assert.equal(p.document.getElementById('agent-spawn-harness').value, 'codex');
  assert.equal(statuses(result)['agent-spawn-model'], 'filled');
  const bad = await fill(p, { 'agent-spawn-approval': 'nonsense' });
  assert.deepEqual(statuses(bad), { 'agent-spawn-approval': 'not_an_option' });
  assert.equal(p.document.getElementById('agent-spawn-approval').value, 'auto');
  p.stop();
});

test('a checkbox takes true or false', async () => {
  const p = page();
  assert.deepEqual(statuses(await fill(p, { 'agent-spawn-owner': 'maybe' })), { 'agent-spawn-owner': 'not_a_boolean' });
  await fill(p, { 'agent-spawn-owner': 'true' });
  assert.equal(p.document.getElementById('agent-spawn-owner').checked, true);
  p.stop();
});

test('a fill refuses marked, password, disabled, hidden and unlisted fields', async () => {
  const p = page();
  const result = await fill(p, {
    'agent-spawn-role': 'x', 'agent-spawn-task': 'x', 'agent-spawn-descr': 'x', 'agent-spawn-cwd': 'x', 'agent-spawn-queue': 'x', 'agent-spawn-submit': 'x', 'agent-spawn-missing': 'x',
  });
  assert.deepEqual(statuses(result), {
    'agent-spawn-role': 'sensitive', 'agent-spawn-task': 'sensitive', 'agent-spawn-descr': 'disabled', 'agent-spawn-cwd': 'not_available',
    'agent-spawn-queue': 'not_fillable', 'agent-spawn-submit': 'not_fillable', 'agent-spawn-missing': 'not_fillable',
  });
  assert.deepEqual(filledIn('spawn'), []);
  p.stop();
});

test('a fill never presses or submits anything', async () => {
  const p = page();
  let pressed = 0;
  p.document.getElementById('agent-spawn-submit').addEventListener('click', () => { pressed += 1; });
  p.root.addEventListener('submit', () => { pressed += 1; });
  await fill(p, { 'agent-spawn-name': 'a', 'agent-spawn-owner': 'true', 'agent-spawn-submit': 'x' });
  await undoFill('spawn', { win: p.window, tick: p.tick });
  assert.equal(pressed, 0);
  const source = readFileSync(new URL('../dashboard/js/form-fill.js', import.meta.url), 'utf8');
  assert.ok(!/\.submit\(|requestSubmit|new win\.Event\('submit/.test(source));
  p.stop();
});

test('a fill never overwrites what the operator typed, before or after it', async () => {
  const p = page();
  typed(p, 'agent-spawn-name', 'mine');
  assert.deepEqual(statuses(await fill(p, { 'agent-spawn-name': 'theirs' })), { 'agent-spawn-name': 'operator_edited' });
  assert.equal(p.document.getElementById('agent-spawn-name').value, 'mine');

  await fill(p, { 'agent-spawn-init-msg': 'proposed' });
  typed(p, 'agent-spawn-init-msg', 'proposed, then edited');
  assert.deepEqual(filledIn('spawn'), [], 'an edited field is no longer the agent\'s');
  assert.deepEqual(statuses(await fill(p, { 'agent-spawn-init-msg': 'again' })), { 'agent-spawn-init-msg': 'operator_edited' });
  p.stop();
});

test('what the operator\'s own change moves is theirs too', async () => {
  const p = page();
  const profile = p.document.getElementById('agent-spawn-harness');
  // Set after the event, as a render does.
  profile.addEventListener('change', () => { queueMicrotask(() => { p.document.getElementById('agent-spawn-model').value = 'opus'; }); });
  profile.value = 'codex';
  const event = new p.window.Event('change', { bubbles: true });
  Object.defineProperty(event, 'isTrusted', { value: true });
  profile.dispatchEvent(event);
  await null;
  p.settle();
  assert.deepEqual(statuses(await fill(p, { 'agent-spawn-model': '' })), { 'agent-spawn-model': 'operator_edited' });
  p.stop();
});

test('a cascade from a fill does not count as the operator\'s', async () => {
  const p = page();
  p.document.getElementById('agent-spawn-harness').addEventListener('change', () => { p.document.getElementById('agent-spawn-model').value = 'opus'; });
  const result = await fill(p, { 'agent-spawn-harness': 'codex', 'agent-spawn-model': '' });
  assert.deepEqual(statuses(result), { 'agent-spawn-harness': 'filled', 'agent-spawn-model': 'filled' });
  p.stop();
});

test('a field its handler refuses reports that', async () => {
  const p = page();
  p.document.getElementById('agent-spawn-name').addEventListener('input', (event) => { event.target.value = ''; });
  assert.deepEqual(statuses(await fill(p, { 'agent-spawn-name': 'x' })), { 'agent-spawn-name': 'rejected' });
  assert.deepEqual(filledIn('spawn'), []);
  p.stop();
});

test('undo all restores what was there and keeps later edits', async () => {
  const p = page();
  p.document.getElementById('agent-spawn-init-msg').value = 'loaded';
  p.stop();
  const q = page();
  await fill(q, { 'agent-spawn-name': 'a', 'agent-spawn-init-msg': 'b' });
  await fill(q, { 'agent-spawn-name': 'a2' });
  typed(q, 'agent-spawn-init-msg', 'mine');
  await undoFill('spawn', { win: q.window, tick: q.tick });
  assert.equal(q.document.getElementById('agent-spawn-name').value, '', 'back to before the first fill');
  assert.equal(q.document.getElementById('agent-spawn-init-msg').value, 'mine');
  assert.deepEqual(filledIn('spawn'), []);
  q.stop();
});

test('a change that widens access is named, with the agent that made it', async () => {
  const p = page();
  await fill(p, { 'agent-spawn-name': 'a', 'agent-spawn-sandbox': 'off' }, 'Stewie');
  await fill(p, { 'agent-spawn-approval': 'yolo' }, 'reviewer');
  await fill(p, { 'agent-spawn-owner': 'true' }, 'reviewer');
  const { filled, widened } = fillSummary(filledIn('spawn'));
  assert.equal(filled, 'Stewie, reviewer filled 4 fields');
  assert.deepEqual(widened, ['Stewie widened: sandbox', 'reviewer widened: approval, owner']);
  p.stop();
});

test('a form that is not open is refused whole', async () => {
  const p = page();
  p.stop();
  assert.match((await fill(p, { 'agent-spawn-name': 'x' })).error, /not open/);
  assert.equal(p.document.getElementById('agent-spawn-name').value, '');
});

test('a form reopened with stored values keeps them', async () => {
  const { window } = parseHTML(`<!doctype html><html><body><div id="agent-spawn-modal"><input id="agent-spawn-name" value="stored"><input id="agent-spawn-descr" value=""></div></body></html>`);
  const root = window.document.getElementById('agent-spawn-modal');
  for (const el of root.querySelectorAll('input')) el.getClientRects = () => [{}];
  const stop = watchForm('spawn', root, { win: window, keepValues: true });
  const result = await applyFill({ win: window, tick: async () => {}, request: { form: 'spawn', agent: 'a', fields: [{ field: 'agent-spawn-name', value: 'x' }, { field: 'agent-spawn-descr', value: 'y' }] } });
  assert.deepEqual(statuses(result), { 'agent-spawn-name': 'operator_edited', 'agent-spawn-descr': 'filled' });
  stop();
});
