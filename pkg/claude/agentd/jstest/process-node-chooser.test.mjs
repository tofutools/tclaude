import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

function key(window, target, value) {
  const event = new window.Event('keydown', { bubbles: true, cancelable: true });
  Object.defineProperty(event, 'key', { value });
  target.dispatchEvent(event);
}

test('anchored node chooser exposes the full vocabulary and supports search, keyboard, and focus', async (t) => {
  const harness = await createPreactHarness(t);
  const { openProcessNodeTypeChooser } = await harness.importDashboardModule('js/process-node-chooser.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.getBoundingClientRect = () => ({ left: 10, top: 20, width: 700, height: 500 });
  const chosen = [];
  const dispose = openProcessNodeTypeChooser({
    host, anchor: { x: 125, y: 90 }, onChoose: (type) => chosen.push(type),
    documentRef: harness.document, wizard: false,
  });
  await Promise.resolve();

  const dialog = getByRole(host, 'dialog', { name: 'Create connected node' });
  const input = getByRole(dialog, 'combobox', { name: 'Choose a node type to connect' });
  assert.equal(harness.document.activeElement, input, 'the searchable control receives initial focus');
  assert.equal(dialog.style.left, '125px');
  assert.equal(dialog.style.top, '98px', 'the chooser sits just below the drop point');
  assert.equal(dialog.querySelectorAll('[role="option"]').length, 6, 'all canonical node types are offered');
  assert.ok(dialog.querySelector('[data-command-id="process.create.parallel"]'));
  assert.ok(input.getAttribute('aria-activedescendant'));
  key(harness.window, input, 'ArrowDown');
  assert.match(input.getAttribute('aria-activedescendant'), /-1$/, 'arrow navigation advances the active option');

  input.value = 'decision';
  input.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  assert.equal(dialog.querySelectorAll('[role="option"]').length, 1);
  assert.match(dialog.querySelector('[role="option"]').textContent, /Create decision node/);
  key(harness.window, input, 'Enter');
  assert.deepEqual(chosen, ['decision']);
  assert.equal(dialog.isConnected, false, 'selection closes before its action runs');
  dispose(); // idempotent after selection
});

test('node chooser pointer choice, Escape, Cancel, and click-away never double-run', async (t) => {
  const harness = await createPreactHarness(t);
  const { openProcessNodeTypeChooser } = await harness.importDashboardModule('js/process-node-chooser.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const outside = harness.document.body.appendChild(harness.document.createElement('button'));
  const chosen = [];
  let restored = 0;
  const open = () => openProcessNodeTypeChooser({
    host, anchor: { x: 10, y: 10 }, onChoose: (type) => chosen.push(type),
    restoreFocus: () => { restored += 1; }, documentRef: harness.document, wizard: false,
  });

  let dispose = open();
  await Promise.resolve();
  host.querySelector('[data-command-id="process.create.wait"]').click();
  assert.deepEqual(chosen, ['wait'], 'pointer selection invokes one canonical command');
  assert.equal(restored, 0, 'the selected action owns the next focus target');

  dispose = open();
  await Promise.resolve();
  key(harness.window, dispose.input, 'Escape');
  assert.deepEqual(chosen, ['wait']);
  assert.equal(restored, 1);

  dispose = open();
  await Promise.resolve();
  host.querySelector('.process-node-chooser-cancel').click();
  assert.deepEqual(chosen, ['wait']);
  assert.equal(restored, 2);

  open();
  await Promise.resolve();
  outside.dispatchEvent(new harness.window.Event('pointerdown', { bubbles: true }));
  assert.equal(host.querySelector('.process-node-chooser'), null);
  assert.deepEqual(chosen, ['wait']);
  assert.equal(restored, 3);

  dispose = open();
  await Promise.resolve();
  dispose();
  dispose();
  assert.equal(host.querySelector('.process-node-chooser'), null);
  assert.equal(restored, 3, 'unmount disposal does not move focus into a retiring graph host');
  outside.dispatchEvent(new harness.window.Event('pointerdown', { bubbles: true }));
  assert.equal(restored, 3, 'explicit disposal removes the document click-away listener');
});

test('wizard chooser changes presentation while plain search terms remain usable', async (t) => {
  const harness = await createPreactHarness(t);
  const { openProcessNodeTypeChooser } = await harness.importDashboardModule('js/process-node-chooser.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const chosen = [];
  const dispose = openProcessNodeTypeChooser({
    host, anchor: { x: 20, y: 20 }, onChoose: (type) => chosen.push(type),
    documentRef: harness.document, wizard: true,
  });
  await Promise.resolve();
  assert.equal(dispose.input.placeholder, 'Search runes…');
  assert.match(host.querySelector('[role="option"]').textContent, /Conjure task rune/);
  dispose.input.value = 'wait';
  dispose.input.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  assert.equal(host.querySelectorAll('[role="option"]').length, 1);
  key(harness.window, dispose.input, 'ArrowUp');
  key(harness.window, dispose.input, 'Enter');
  assert.deepEqual(chosen, ['wait']);
});

test('directionally unavailable types stay visible, disabled, and keep the chooser open', async (t) => {
  const harness = await createPreactHarness(t);
  const { openProcessNodeTypeChooser } = await harness.importDashboardModule('js/process-node-chooser.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const chosen = [];
  const dispose = openProcessNodeTypeChooser({
    host, anchor: { x: 20, y: 20 }, onChoose: (type) => chosen.push(type),
    availability: (type) => type === 'end' ? {
      enabled: false, disabledReason: 'End nodes cannot be sources because they cannot have outgoing edges.',
    } : null,
    documentRef: harness.document, wizard: false,
  });
  await Promise.resolve();

  dispose.input.value = 'end';
  dispose.input.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  const end = host.querySelector('[data-command-id="process.create.end"]');
  assert.equal(end.getAttribute('aria-disabled'), 'true');
  assert.match(end.textContent, /cannot have outgoing edges/);
  end.click();
  key(harness.window, dispose.input, 'Enter');
  assert.deepEqual(chosen, []);
  assert.equal(dispose.element.isConnected, true, 'a rejected directional choice preserves its reason');
});

test('Escape from the Cancel button closes and restores focus', async (t) => {
  const harness = await createPreactHarness(t);
  const { openProcessNodeTypeChooser } = await harness.importDashboardModule('js/process-node-chooser.js');
  const trigger = harness.document.body.appendChild(harness.document.createElement('button'));
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const dispose = openProcessNodeTypeChooser({
    host, anchor: { x: 20, y: 20 }, onChoose() {}, restoreFocus: () => trigger.focus(),
    documentRef: harness.document, wizard: false,
  });
  await Promise.resolve();
  const cancel = host.querySelector('.process-node-chooser-cancel');
  cancel.focus();
  key(harness.window, cancel, 'Escape');
  assert.equal(dispose.element.isConnected, false);
  assert.equal(harness.document.activeElement, trigger);
});
