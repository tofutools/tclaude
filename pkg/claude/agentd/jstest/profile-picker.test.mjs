import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';
import { comboPick } from './combobox-helpers.mjs';

// The profile choice (profile-picker.js, tcl-986r2g): a node's profile
// libraries grow to tens of entries, so profiles are type-to-filter boxes.

test('profileItems say a spawn profile\'s launch and state, and list its aliases as rows of their own', async (t) => {
  const harness = await createPreactHarness(t);
  const { profileItems } = await harness.importDashboardModule('js/profile-picker.js');
  const items = profileItems([
    { name: 'fast', harness: 'codex', model: 'gpt-5', effort: 'high', aliases: ['f'] },
    { name: 'off', disabled: true, disabled_reason: 'retired\n  for now' },
    { name: 'mine', operator_only: true },
    '',
    'plain',
  ], { aliases: true });
  assert.deepEqual(items.map((item) => [item.key, item.label, item.hint]), [
    ['fast', 'fast', 'codex · gpt-5 · effort high'],
    ['f', 'f → fast', 'codex · gpt-5 · effort high'],
    ['off', 'off', '🚫 disabled: retired for now'],
    ['mine', 'mine', '👤 operator only'],
    ['plain', 'plain', ''],
  ]);
  assert.equal(profileItems([{ name: 'fast', aliases: ['f'] }]).length, 1, 'aliases are rows only when asked for');
  assert.equal(profileItems([{ name: 'n', trust: 'full' }], { hint: (p) => `trust ${p.trust}` })[0].hint, 'trust full');
  assert.match(items[0].keywords, /codex/, 'a profile is found by its launch');
});

test('a profile is found by typing and picked; a deleted one stays, marked', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProfileCombobox } = await harness.importDashboardModule('js/profile-picker.js');
  const profiles = Array.from({ length: 60 }, (_, i) => ({ name: `profile-${i}`, harness: i % 2 ? 'codex' : 'claude' }));
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const changes = [];
  function Holder({ initial }) {
    const [value, setValue] = harness.hooks.useState(initial);
    return harness.html`<${ProfileCombobox} id="prof" value=${value} profiles=${profiles} noneLabel="(none)"
      onChange=${(next) => { changes.push(next); setValue(next); }} />`;
  }
  const mounted = await harness.mount(harness.html`<${Holder} initial="gone" />`, host);
  const box = host.querySelector('#prof');
  assert.ok(box.classList.contains('missing'), 'a profile the library no longer has is kept and marked');
  assert.equal(box.querySelector('input').value, 'gone (missing)');
  await harness.input(box.querySelector('input[role="combobox"]'), 'profile-47');
  assert.equal(box.querySelector('[role="option"]').dataset.key, 'profile-47');
  await comboPick(harness, box, 'profile-47');
  assert.deepEqual(changes, ['profile-47']);
  assert.equal(box.dataset.value, 'profile-47');
  await mounted.unmount();
});
