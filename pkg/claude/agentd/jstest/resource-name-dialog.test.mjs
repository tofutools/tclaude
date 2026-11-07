// The name dialog a new drawing board or live page asks first
// (resource-name-dialog.js): prefilled and selected, Enter or Ctrl/Cmd+Enter
// creates with the typed name, Escape cancels, an empty name is blocked, a
// refusal stays in the dialog, and the 🧙 theme words it as the palette does.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';
import { mountNameHost, nameDialog, waitFor, waitForNameDialog } from './resource-name-helpers.mjs';

async function open(t, { kind = 'drawing_board', group = 'crew', value = 'crew drawing board', create, wizard = false } = {}) {
  const harness = await createPreactHarness(t);
  if (wizard) harness.document.body.classList.add('wizard');
  // The test DOM has no text selection: record that the field asked for one.
  const selected = [];
  harness.window.HTMLElement.prototype.select = function select() { selected.push(this); };
  const host = await mountNameHost(harness);
  const made = [];
  let result;
  await harness.act(() => {
    result = host.askResourceName({ kind, group, value, create: create || (async (name) => { made.push(name); return { id: 'new', title: name }; }) });
  });
  const { dialog, input } = await waitForNameDialog(harness);
  return { harness, host, dialog, input, made, result, selected };
}

const key = (harness, target, init) => harness.act(() => harness.fireEvent(target, 'keydown', init));

test('the dialog names what is made and where, with the default name prefilled and selected', async (t) => {
  const { harness, dialog, input, selected } = await open(t);
  assert.equal(dialog.querySelector('h3').textContent, 'New drawing board in crew');
  assert.equal(input.value, 'crew drawing board');
  await waitFor(harness, () => harness.document.activeElement === input, 'the Name field takes focus');
  assert.ok(selected.includes(input), 'the name is selected, so typing replaces it');
  assert.equal(dialog.querySelector('#resource-name-submit').textContent.trim(), 'Create');
  assert.equal(dialog.querySelector('#resource-name-cancel').textContent.trim(), 'Cancel');
});

test('Enter in the field creates with the typed name', async (t) => {
  const { harness, input, made, result } = await open(t);
  await harness.input(input, '  Sprint map  ');
  await key(harness, input, { key: 'Enter' });
  await waitFor(harness, () => !nameDialog(harness), 'the dialog closes once made');
  assert.deepEqual(made, ['Sprint map'], 'the name is trimmed');
  assert.deepEqual(await result, { id: 'new', title: 'Sprint map' });
});

for (const [name, mod] of [['Ctrl+Enter', { ctrlKey: true }], ['Cmd+Enter', { metaKey: true }]]) {
  test(`${name} creates too`, async (t) => {
    const { harness, input, made } = await open(t, { kind: 'page', value: 'sketch' });
    await harness.input(input, 'Landing');
    await key(harness, input, { key: 'Enter', ...mod });
    await waitFor(harness, () => !nameDialog(harness), 'the dialog closes once made');
    assert.deepEqual(made, ['Landing']);
  });
}

test('Escape closes it and makes nothing', async (t) => {
  const { harness, input, made, result } = await open(t);
  await key(harness, input, { key: 'Escape' });
  await waitFor(harness, () => !nameDialog(harness), 'Escape closes the dialog');
  assert.deepEqual(made, []);
  assert.equal(await result, null, 'a cancel resolves to null');
});

test('Cancel closes it and makes nothing', async (t) => {
  const { harness, dialog, made, result } = await open(t);
  await harness.act(() => dialog.querySelector('#resource-name-cancel').click());
  await waitFor(harness, () => !nameDialog(harness), 'Cancel closes the dialog');
  assert.deepEqual(made, []);
  assert.equal(await result, null);
});

test('an empty or blank name blocks Create and says so', async (t) => {
  const { harness, dialog, input, made } = await open(t);
  await harness.input(input, '   ');
  const submit = dialog.querySelector('#resource-name-submit');
  assert.equal(submit.disabled, true, 'Create is disabled');
  assert.equal(input.getAttribute('aria-invalid'), 'true');
  assert.equal(dialog.querySelector('#resource-name-invalid').textContent, 'Give it a name.');
  await key(harness, input, { key: 'Enter' });
  await key(harness, input, { key: 'Enter', ctrlKey: true });
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.ok(nameDialog(harness), 'the dialog stays');
  assert.deepEqual(made, [], 'nothing is made');
  await harness.input(input, 'ok');
  assert.equal(dialog.querySelector('#resource-name-invalid'), null, 'a name clears the message');
});

test('a refusal shows in the dialog, which stays open to try again', async (t) => {
  let tries = 0;
  const { harness, dialog, input } = await open(t, {
    create: async (name) => {
      tries += 1;
      if (tries === 1) throw new Error('the title must be printable');
      return { id: 'x', title: name };
    },
  });
  await key(harness, input, { key: 'Enter' });
  await waitFor(harness, () => dialog.querySelector('#resource-name-error').textContent === 'the title must be printable', 'the refusal shows in the error slot');
  assert.ok(nameDialog(harness), 'the dialog stays open');
  await key(harness, input, { key: 'Enter' });
  await waitFor(harness, () => !nameDialog(harness), 'a second try that works closes it');
  assert.equal(tries, 2);
});

test('the wizard theme words a drawing board as the palette does', async (t) => {
  const { dialog } = await open(t, { wizard: true });
  assert.equal(dialog.querySelector('h3').textContent, 'Unroll a scrying slate in crew');
  assert.equal(dialog.querySelector('#resource-name-submit').textContent.trim(), 'Unroll the slate');
});

test('the wizard theme words a live page as the palette does', async (t) => {
  const { dialog } = await open(t, { kind: 'page', value: 'sketch', wizard: true });
  assert.equal(dialog.querySelector('h3').textContent, 'Raise a looking glass in crew');
  assert.equal(dialog.querySelector('#resource-name-submit').textContent.trim(), 'Raise the glass');
  assert.match(dialog.querySelector('.modal-hint').textContent, /a hero of crew fills with its vision/);
});
