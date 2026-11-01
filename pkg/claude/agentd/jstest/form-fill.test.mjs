// Tests for form fills (tcl-3ta7bl). The promises are the point: a fill goes
// through the field's own handler, never submits, never overwrites the operator,
// never touches a marked field, and can be undone.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { applyFill, claimField, FILL_FORMS, filledIn, fillSummary, undoFill, watchForm } from '../dashboard/js/form-fill.js';
import { parseHTML } from './third_party/linkedom.mjs';

const ROW = `
  <input id="agent-spawn-name" value="">
  <input id="agent-spawn-wt-branch" value="">
  <input id="agent-spawn-wt-sync" type="checkbox">
  <textarea id="agent-spawn-init-msg"></textarea>
  <select id="agent-spawn-harness"><option value="claude">Claude Code</option><option value="codex">Codex CLI</option></select>
  <select id="agent-spawn-usage-profile"><option value="">inherited</option><option value="/new" data-no-fill="">new</option></select>
  <select id="agent-spawn-load-profile"><option value="">none</option><option value="p">p</option></select>
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
  root.getClientRects = () => [{}];
  for (const el of root.querySelectorAll('input, select, textarea')) {
    el.getClientRects = () => (el.classList.contains('unrendered') ? [] : [{}]);
  }
  // linkedom has no settable select value and no checkbox click.
  for (const select of root.querySelectorAll('select')) {
    let current = select.querySelector('option')?.getAttribute('value') ?? '';
    Object.defineProperty(select, 'value', { get: () => current, set: (value) => { current = value; } });
    Object.defineProperty(select, 'options', { configurable: true, get: () => [...select.querySelectorAll('option')].map((option) => ({ value: option.getAttribute('value'), label: option.textContent, textContent: option.textContent, hasAttribute: (name) => option.hasAttribute(name) })) });
  }
  for (const box of root.querySelectorAll('input[type="checkbox"]')) {
    let checked = false;
    Object.defineProperty(box, 'checked', { get: () => checked, set: (value) => { checked = value; } });
    // A script's click() arrives trusted in a browser.
    box.click = () => {
      checked = !checked;
      const event = new window.Event('change', { bubbles: true });
      Object.defineProperty(event, 'isTrusted', { value: true });
      box.dispatchEvent(event);
    };
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

const value = (p, id) => p.document.getElementById(`agent-spawn-${id}`).value;
const resetting = (p, moves) => p.document.getElementById('agent-spawn-harness').addEventListener('change', () => {
  for (const [id, to] of Object.entries(moves)) p.document.getElementById(`agent-spawn-${id}`).value = to;
});

test('what a fill moves by cascade is warned about, reported and undone with it', async () => {
  const p = page();
  resetting(p, { sandbox: 'off', approval: 'yolo' });
  const result = await fill(p, { 'agent-spawn-harness': 'codex' });
  assert.deepEqual(result.fields[0].moved, ['agent-spawn-sandbox', 'agent-spawn-approval']);
  assert.equal(result.fields[0].widens, 'harness, sandbox, approval');
  assert.deepEqual(fillSummary(filledIn('spawn')).widened, ['Stewie widened: harness, sandbox, approval']);
  // Going back resets the cascade to the new harness's values, as a real one does.
  await undoFill('spawn', { win: p.window, tick: p.tick });
  assert.equal(value(p, 'harness'), 'claude');
  assert.equal(value(p, 'sandbox'), 'inherit');
  assert.equal(value(p, 'approval'), 'auto');
  assert.deepEqual(filledIn('spawn'), []);
  p.stop();
});

test('a fill that would reset the operator\'s field is refused before it writes', async () => {
  const p = page();
  typed(p, 'agent-spawn-sandbox', 'off');
  resetting(p, { sandbox: 'inherit' });
  assert.deepEqual(statuses(await fill(p, { 'agent-spawn-harness': 'codex' })), { 'agent-spawn-harness': 'operator_edited' });
  assert.equal(value(p, 'harness'), 'claude');
  p.stop();
});

test('a cascade nobody declared still leaves the operator\'s field alone', async () => {
  const p = page();
  typed(p, 'agent-spawn-name', 'mine');
  p.document.getElementById('agent-spawn-model').addEventListener('change', () => { p.document.getElementById('agent-spawn-name').value = ''; });
  await fill(p, { 'agent-spawn-model': 'opus' });
  assert.equal(value(p, 'name'), 'mine');
  p.stop();
});

test('undoing a fill keeps the operator\'s field that the undo moved', async () => {
  const p = page();
  await fill(p, { 'agent-spawn-harness': 'codex' });
  typed(p, 'agent-spawn-sandbox', 'off');
  resetting(p, { sandbox: 'inherit' });
  await undoFill('spawn', { win: p.window, tick: p.tick });
  assert.equal(value(p, 'harness'), 'claude');
  assert.equal(value(p, 'sandbox'), 'off');
  p.stop();
});

test('a field undo cannot restore stays listed, with its warning', async () => {
  const p = page();
  await fill(p, { 'agent-spawn-sandbox': 'off' });
  p.document.getElementById('agent-spawn-sandbox').addEventListener('change', (event) => { event.target.value = 'off'; });
  await undoFill('spawn', { win: p.window, tick: p.tick });
  assert.deepEqual(fillSummary(filledIn('spawn')).widened, ['Stewie widened: sandbox']);
  p.stop();
});

test('leaving a field for Undo does not make what Undo writes the operator\'s', async () => {
  const p = page();
  typed(p, 'agent-spawn-name', 'mine');
  await fill(p, { 'agent-spawn-init-msg': 'hi' });
  // The click on Undo blurs the field, which reports a change just before it.
  const event = new p.window.Event('change', { bubbles: true });
  Object.defineProperty(event, 'isTrusted', { value: true });
  p.document.getElementById('agent-spawn-name').dispatchEvent(event);
  await undoFill('spawn', { win: p.window, tick: async () => p.settle() });
  assert.equal(value(p, 'init-msg'), '');
  assert.deepEqual(filledIn('spawn'), []);
  p.stop();
});

test('undo leaves alone what the operator types while it runs', async () => {
  const p = page();
  await fill(p, { 'agent-spawn-init-msg': 'a', 'agent-spawn-name': 'b' });
  let once = () => typed(p, 'agent-spawn-init-msg', 'mine');
  await undoFill('spawn', { win: p.window, tick: async () => { once(); once = () => {}; } });
  assert.equal(value(p, 'init-msg'), 'mine');
  assert.equal(value(p, 'name'), '');
  p.stop();
});

test('a browse result during a fill stays', async () => {
  const p = page();
  typed(p, 'agent-spawn-name', 'old');
  const tick = async () => { p.document.getElementById('agent-spawn-name').value = 'browsed'; claimField('spawn', 'agent-spawn-name'); };
  await applyFill({ win: p.window, tick, request: { form: 'spawn', agent: 'a', fields: [{ field: 'agent-spawn-model', value: 'opus' }] } });
  assert.equal(value(p, 'name'), 'browsed');
  p.stop();
});

test('undo leaves a synced worktree for the form to work out again', async () => {
  const p = page();
  await fill(p, { 'agent-spawn-wt-sync': 'true' });
  const harness = p.document.getElementById('agent-spawn-harness');
  const once = () => { p.document.getElementById('agent-spawn-wt-branch').value = 'moved'; harness.removeEventListener('change', once); };
  harness.addEventListener('change', once);
  await fill(p, { 'agent-spawn-harness': 'codex' });
  const writes = [];
  p.document.getElementById('agent-spawn-wt-branch').addEventListener('input', (event) => writes.push(event.target.value));
  await undoFill('spawn', { win: p.window, tick: p.tick });
  assert.deepEqual(writes, []);
  assert.deepEqual(filledIn('spawn').filter((field) => field.id === 'agent-spawn-wt-branch'), []);
  p.stop();
});

test('an operator value the undone harness took away is listed, not blanked', async () => {
  const p = page();
  const approval = p.document.getElementById('agent-spawn-approval');
  await fill(p, { 'agent-spawn-harness': 'codex' });
  typed(p, 'agent-spawn-approval', 'yolo');
  // Undoing the harness resets approval and no longer offers yolo.
  const options = Object.getOwnPropertyDescriptor(approval, 'options').get;
  p.document.getElementById('agent-spawn-harness').addEventListener('change', () => {
    approval.value = 'auto';
    Object.defineProperty(approval, 'options', { configurable: true, get: () => options().filter((option) => option.value !== 'yolo') });
  });
  await undoFill('spawn', { win: p.window, tick: p.tick });
  assert.equal(value(p, 'approval'), 'auto');
  assert.deepEqual(filledIn('spawn').map((field) => field.id), ['agent-spawn-approval']);
  p.stop();
});

test('filling a checkbox is not the operator editing it', async () => {
  const p = page();
  await fill(p, { 'agent-spawn-owner': 'true' });
  assert.equal(filledIn('spawn').length, 1);
  assert.deepEqual(statuses(await fill(p, { 'agent-spawn-owner': 'false' })), { 'agent-spawn-owner': 'filled' });
  assert.deepEqual(filledIn('spawn'), [], 'back to where it started');
  p.stop();
});

test('an edit during the wait after a write wins', async () => {
  const p = page();
  let once = () => typed(p, 'agent-spawn-name', 'mine');
  const tick = async () => { once(); once = () => {}; };
  const result = await applyFill({ win: p.window, tick, request: { form: 'spawn', agent: 'a', fields: [{ field: 'agent-spawn-name', value: 'theirs' }] } });
  assert.deepEqual(statuses(result), { 'agent-spawn-name': 'operator_edited' });
  assert.equal(value(p, 'name'), 'mine');
  assert.deepEqual(filledIn('spawn'), []);
  p.stop();
});

test('what the operator types in their own field during a fill or undo stays', async () => {
  const p = page();
  typed(p, 'agent-spawn-name', 'mi');
  let once = () => typed(p, 'agent-spawn-name', 'mine');
  const tick = async () => { once(); once = () => {}; };
  await applyFill({ win: p.window, tick, request: { form: 'spawn', agent: 'a', fields: [{ field: 'agent-spawn-model', value: 'opus' }] } });
  assert.equal(value(p, 'name'), 'mine');
  once = () => typed(p, 'agent-spawn-name', 'mine too');
  await undoFill('spawn', { win: p.window, tick });
  assert.equal(value(p, 'name'), 'mine too');
  assert.equal(value(p, 'model'), '');
  p.stop();
});

test('an Undo pressed during a fill undoes all of it', async () => {
  const p = page();
  const filling = fill(p, { 'agent-spawn-name': 'a', 'agent-spawn-sandbox': 'off' });
  await Promise.all([filling, undoFill('spawn', { win: p.window, tick: p.tick })]);
  assert.equal(value(p, 'sandbox'), 'inherit');
  assert.deepEqual(filledIn('spawn'), []);
  p.stop();
});

test('a form closed during a fill takes no more of it', async () => {
  const p = page();
  const result = await applyFill({
    win: p.window, tick: async () => p.stop(),
    request: { form: 'spawn', agent: 'a', fields: [{ field: 'agent-spawn-name', value: 'x' }, { field: 'agent-spawn-model', value: 'opus' }] },
  });
  assert.deepEqual(statuses(result), { 'agent-spawn-name': 'form_closed' });
  assert.equal(value(p, 'model'), '');
});

test('a directory the operator browsed to is theirs', async () => {
  const p = page();
  await fill(p, { 'agent-spawn-name': 'a' });
  claimField('spawn', 'agent-spawn-name');
  assert.deepEqual(filledIn('spawn'), []);
  assert.deepEqual(statuses(await fill(p, { 'agent-spawn-name': 'b' })), { 'agent-spawn-name': 'operator_edited' });
  p.stop();
});

test('a profile and an option that opens something cannot be filled', async () => {
  const p = page();
  assert.deepEqual(statuses(await fill(p, { 'agent-spawn-load-profile': 'p', 'agent-spawn-usage-profile': '/new' })), {
    'agent-spawn-load-profile': 'not_fillable', 'agent-spawn-usage-profile': 'not_an_option',
  });
  p.stop();
});

test('a form that is not on screen is refused whole', async () => {
  const p = page();
  p.root.getClientRects = () => [];
  assert.match((await fill(p, { 'agent-spawn-name': 'x' })).error, /not open/);
  p.stop();
});

test('Config warns unless a section is known to be cosmetic', () => {
  const { document } = parseHTML(`<!doctype html><html><body><div class="config-form">
    <section data-cfg-panel="appearance"><h2>Appearance</h2><input id="cfg-theme"></section>
    <section data-cfg-panel="voice"><h2>Voice</h2><input id="cfg-voice-x"></section>
    <section data-cfg-panel="steward"><div class="cfg-panel-head"><h2><span aria-hidden="true">★</span> Steward</h2></div><input id="cfg-steward-model"></section>
    <section data-cfg-panel="agents"><h2>Agents</h2><input id="cfg-agent-spawnrestrict"><input id="cfg-agent-spawnmax"></section>
    <section data-cfg-panel="experimental"><h2>Experimental</h2><input id="cfg-feature-agent-dirs-mount-parent"></section>
    <section data-cfg-panel="daemon"><h2>Daemon</h2><input id="cfg-processes-refuse-unsigned"><input id="cfg-broker-enforce"></section>
    <input id="cfg-outside"></div></body></html>`);
  const widens = (id) => FILL_FORMS.config.widens(document.getElementById(id));
  assert.equal(widens('cfg-theme'), '');
  assert.equal(widens('cfg-voice-x'), 'Voice settings');
  assert.equal(widens('cfg-steward-model'), 'Steward settings');
  for (const id of ['cfg-agent-spawnrestrict', 'cfg-agent-spawnmax', 'cfg-feature-agent-dirs-mount-parent', 'cfg-processes-refuse-unsigned', 'cfg-broker-enforce']) assert.ok(widens(id), id);
  assert.equal(widens('cfg-outside'), 'settings');
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
  root.getClientRects = () => [{}];
  for (const el of root.querySelectorAll('input')) el.getClientRects = () => [{}];
  const stop = watchForm('spawn', root, { win: window, keepValues: true });
  const result = await applyFill({ win: window, tick: async () => {}, request: { form: 'spawn', agent: 'a', fields: [{ field: 'agent-spawn-name', value: 'x' }, { field: 'agent-spawn-descr', value: 'y' }] } });
  assert.deepEqual(statuses(result), { 'agent-spawn-name': 'operator_edited', 'agent-spawn-descr': 'filled' });
  stop();
});
