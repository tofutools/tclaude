import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('task cells separate navigation from editing and retain raw edit values', async (t) => {
  const harness = await createPreactHarness(t);
  const { taskCell } = await harness.importDashboardModule('js/helpers.js');

  const empty = taskCell({ conv_id: 'conv-1', agent_id: 'agt_1', title: 'alice' });
  assert.match(empty, /task-attach/);
  assert.match(empty, /data-act="edit-task"/);
  assert.match(empty, /✧ bind quest/);
  assert.doesNotMatch(empty, /<a /);

  const set = taskCell({
    conv_id: 'conv-1', agent_id: 'agt_1', title: 'alice',
    task_ref_url: 'https://example.com/work/42?x=1&y=2',
    task_ref_label: 'Release blocker',
    task_ref_label_override: 'Release blocker',
  });
  assert.match(set, /<a class="task-ref task-link"/);
  assert.match(set, /Release blocker/);
  assert.match(set, /href="https:\/\/example\.com\/work\/42\?x=1&amp;y=2"/);
  assert.match(set, /class="task-edit task-edit-icon"/);
  assert.match(set, /data-current-task-label="Release blocker"/);
  assert.match(set, />✎<\/span>/);

  const unsafe = taskCell({
    conv_id: 'conv-1', task_ref_url: 'javascript:alert(1)', task_ref_label: 'bad',
  });
  assert.doesNotMatch(unsafe, /href=/, 'stored unsafe values remain inert');
  assert.match(unsafe, /task-edit-icon/, 'an unsafe legacy value remains editable');
});

test('task-link dialog prefills URL and explicit label, then returns both', async (t) => {
  const harness = await createPreactHarness(t);
  harness.window.location = { search: '' };
  harness.document.body.innerHTML = `
    <button id="task-link-invoker"></button>
    <div id="task-link-modal">
      <div class="modal">
        <div id="task-link-meta"></div>
        <input id="task-link-url" data-select-on-focus />
        <input id="task-link-label" />
        <div id="task-link-error"></div>
        <button id="task-link-cancel"></button>
        <button id="task-link-save"></button>
      </div>
    </div>
  `;
  await harness.replaceDashboardModule('js/dashboard.js', `
    export let lastSnapshot = null;
    export function setLastSnapshot(value) { lastSnapshot = value; }
    export function webTerminalDefault() { return false; }
    export function sudoBadge() { return ''; }
  `);
  const { taskLinkModal } = await harness.importDashboardModule('js/refresh.js');

  const invoker = harness.document.querySelector('#task-link-invoker');
  invoker.focus();
  const result = taskLinkModal({
    agentLabel: 'alice',
    url: 'https://linear.app/acme/issue/JOH-42/work',
    taskLabel: 'Launch task',
  });
  await Promise.resolve(); // bindDialogFocus's initial-focus microtask
  assert.equal(harness.document.querySelector('#task-link-meta').textContent, 'alice');
  assert.equal(harness.document.querySelector('#task-link-url').value,
    'https://linear.app/acme/issue/JOH-42/work');
  assert.equal(harness.document.querySelector('#task-link-label').value, 'Launch task');
  assert.equal(harness.document.activeElement, harness.document.querySelector('#task-link-url'));

  // The shared form is single-instance. A programmatic/repeated open neither
  // overwrites the first agent's values nor leaves a second pending Promise.
  assert.equal(await taskLinkModal({
    agentLabel: 'bob', url: 'https://example.com/bob', taskLabel: 'Bob task',
  }), null);
  assert.equal(harness.document.querySelector('#task-link-meta').textContent, 'alice');

  // Tab from the last control wraps to the first instead of reaching the page
  // behind the aria-modal overlay.
  harness.document.querySelector('#task-link-save').focus();
  const tab = new harness.window.Event('keydown', { bubbles: true });
  Object.defineProperty(tab, 'key', { value: 'Tab' });
  harness.document.dispatchEvent(tab);
  assert.equal(harness.document.activeElement, harness.document.querySelector('#task-link-url'));

  harness.document.querySelector('#task-link-label').value = 'Release task';
  // Enter is a save shortcut only in the text fields. It must not override
  // native button activation or commit while an IME composition is active.
  const cancel = harness.document.querySelector('#task-link-cancel');
  cancel.focus();
  const cancelEnter = new harness.window.Event('keydown', { bubbles: true, cancelable: true });
  Object.defineProperty(cancelEnter, 'key', { value: 'Enter' });
  cancel.dispatchEvent(cancelEnter);
  assert.equal(cancelEnter.defaultPrevented, false);
  assert.ok(harness.document.querySelector('#task-link-modal').classList.contains('show'));

  const composingEnter = new harness.window.Event('keydown', { bubbles: true, cancelable: true });
  Object.defineProperties(composingEnter, {
    key: { value: 'Enter' },
    isComposing: { value: true },
  });
  harness.document.querySelector('#task-link-label').dispatchEvent(composingEnter);
  assert.equal(composingEnter.defaultPrevented, false);
  assert.ok(harness.document.querySelector('#task-link-modal').classList.contains('show'));

  harness.document.querySelector('#task-link-save').click();
  assert.deepEqual(await result, {
    url: 'https://linear.app/acme/issue/JOH-42/work',
    taskLabel: 'Release task',
  });
  assert.equal(harness.document.activeElement, invoker, 'close restores the edit-pencil invoker');

  const invalid = taskLinkModal({agentLabel: 'alice', url: '', taskLabel: ''});
  harness.document.querySelector('#task-link-url').value = 'javascript:alert(1)';
  harness.document.querySelector('#task-link-save').click();
  assert.match(harness.document.querySelector('#task-link-error').textContent, /http:\/\//);
  assert.ok(harness.document.querySelector('#task-link-modal').classList.contains('show'));
  harness.document.querySelector('#task-link-cancel').click();
  assert.equal(await invalid, null);
});

test('dirty task-link dialog contains accidental dismissal and confirms discard', async (t) => {
  const harness = await createPreactHarness(t);
  harness.window.location = { search: '' };
  harness.document.body.innerHTML = `
    <button id="invoker"></button>
    <div id="task-link-modal"><div class="modal">
      <div id="task-link-meta"></div><input id="task-link-url" />
      <input id="task-link-label" /><div id="task-link-error"></div>
      <button id="task-link-cancel"></button><button id="task-link-save"></button>
    </div></div>
  `;
  await harness.replaceDashboardModule('js/dashboard.js', `
    export let lastSnapshot = null;
    export function setLastSnapshot(value) { lastSnapshot = value; }
    export function webTerminalDefault() { return false; }
    export function sudoBadge() { return ''; }
  `);
  const { taskLinkModal } = await harness.importDashboardModule('js/refresh.js');
  const decisions = [false, true];
  let confirmations = 0;
  harness.document.querySelector('#invoker').focus();
  const result = taskLinkModal(
    {agentLabel: 'alice', url: 'https://example.com/old', taskLabel: ''},
    {confirmDiscardFn: async () => { confirmations += 1; return decisions.shift(); }},
  );
  await Promise.resolve();
  const overlay = harness.document.querySelector('#task-link-modal');
  const url = harness.document.querySelector('#task-link-url');
  url.value = 'https://example.com/new';

  // A selection drag beginning in the field and releasing on the backdrop is
  // not a backdrop click and must not lose the edit.
  url.dispatchEvent(new harness.window.Event('mousedown', { bubbles: true }));
  overlay.dispatchEvent(new harness.window.Event('click', { bubbles: true }));
  await Promise.resolve();
  assert.ok(overlay.classList.contains('show'));
  assert.equal(confirmations, 0);

  const escape = () => {
    const event = new harness.window.Event('keydown', { bubbles: true });
    Object.defineProperty(event, 'key', { value: 'Escape' });
    harness.document.dispatchEvent(event);
  };
  escape();
  await Promise.resolve();
  await Promise.resolve();
  assert.ok(overlay.classList.contains('show'), 'denied discard keeps the dirty dialog open');
  assert.equal(confirmations, 1);
  escape();
  assert.equal(await result, null);
  assert.equal(confirmations, 2);
  assert.equal(harness.document.activeElement, harness.document.querySelector('#invoker'));
});
