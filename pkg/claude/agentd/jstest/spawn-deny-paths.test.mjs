import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// The spawn dialog's "Deny paths" control (tcl-074v5a): the dashboard's
// --sandbox-deny. The daemon is the authority on every refusal; the dialog
// sends what the operator chose, flags what the daemon would refuse, and
// never quietly drops a deny.

const harnesses = [{
  name: 'claude', display_name: 'Claude Code', models: ['sonnet'],
  can_tclaude_layer: true, can_builtin_os_sandbox: true,
  can_sandbox: true, sandbox_modes: ['inherit', 'on', 'off'], default_sandbox: 'inherit',
}];
const sandboxImpl = {
  options: [{ value: 'tclaude-layer', label: 'tclaude layer' }, { value: 'off', label: 'Off' }],
  default: 'harness-builtin', host_available: true,
};

test('the request carries the chips and a valid path still being typed', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/agent-spawn-model.js');
  const context = { harnesses, sandboxImpl, groups: [{ name: 'crew' }] };
  const base = { group: 'crew', harness: 'claude', name: 'w', cwd: '/repo', wtRepo: '/repo', sandboxImpl: 'tclaude-layer' };

  assert.equal('sandbox_deny' in model.buildSpawnRequest({ ...base, sandboxDeny: [] }, context, null).body, false,
    'no deny, no key');
  const sent = model.buildSpawnRequest({ ...base, sandboxDeny: ['~/secrets'], sandboxDenyDraft: '/srv/keys/' }, context, null);
  assert.deepEqual(sent.body.sandbox_deny, ['~/secrets', '/srv/keys'], 'a typed path is not lost to an early Spawn');
  const relative = model.buildSpawnRequest({ ...base, sandboxDeny: ['~/secrets'], sandboxDenyDraft: 'keys' }, context, null);
  assert.deepEqual(relative.body.sandbox_deny, ['~/secrets'], 'an invalid typed path is not sent');

  const off = model.buildSpawnRequest({ ...base, sandboxImpl: 'off', sandboxDeny: ['~/secrets'] }, context, null);
  assert.deepEqual(off.body.sandbox_deny, ['~/secrets'],
    'a launch that cannot enforce it still sends it, so the daemon refuses rather than launching without it');
  const remote = model.buildSpawnRequest({ ...base, sandboxDeny: ['~/secrets'] }, { ...context, remoteSpawn: true }, null);
  assert.equal('sandbox_deny' in remote.body, false, 'a remote spawn does not offer it');

  assert.equal(model.spawnCapabilityView({ ...base }, context).sandboxDenyBlockedReason, '');
  assert.match(model.spawnCapabilityView({ ...base, sandboxImpl: 'off' }, context).sandboxDenyBlockedReason, /cannot enforce/);
  assert.match(model.spawnCapabilityView({ ...base, sandboxProfile: model.SANDBOX_PROFILE_NONE }, context).sandboxDenyBlockedReason,
    /none/);
  assert.equal(model.spawnDraftIsDirty({ ...base, sandboxDeny: ['~/x'] }, { ...base, sandboxDeny: [] }), true);
});

test('the rules accept absolute and ~/ paths and flag a deny over the working directory', async (t) => {
  const harness = await createPreactHarness(t);
  const rules = await harness.importDashboardModule('js/spawn-deny-paths-model.js');
  assert.equal(rules.normalizeDenyPath(' /srv/keys// '), '/srv/keys');
  assert.equal(rules.normalizeDenyPath('~/secrets'), '~/secrets');
  assert.equal(rules.normalizeDenyPath('secrets'), '');
  assert.equal(rules.normalizeDenyPath('~bob/x'), '', 'another user’s home is not expanded by the daemon');
  assert.match(rules.denyPathProblem('secrets'), /absolute path or ~\//);
  assert.equal(rules.denyCoversCwd('/work', '/work/repo'), true);
  assert.equal(rules.denyCoversCwd('/work', '/workshop'), false, 'a sibling with a shared prefix is not covered');
  assert.equal(rules.denyCoversCwd('~/work', '/home/me/work'), false, 'left to the daemon, which knows ~');
});

async function mountChips(harness, props) {
  const { DenyPathChips } = await harness.importDashboardModule('js/spawn-deny-paths.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const state = { value: props.value || [], draft: '' };
  function Holder() {
    const [value, setValue] = harness.hooks.useState(state.value);
    const [draft, setDraft] = harness.hooks.useState('');
    state.value = value; state.draft = draft;
    return harness.html`<${DenyPathChips} id="deny" ...${props} value=${value} draftText=${draft}
      onChange=${setValue} onDraft=${setDraft} />`;
  }
  await harness.mount(harness.html`<${Holder} />`, host);
  return { host, state, input: () => host.querySelector('input[role="combobox"]') };
}

test('Enter adds a path as a chip, × removes it, and a bad path is flagged', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, state, input } = await mountChips(harness, { cwd: '/work/repo' });
  assert.equal(input().id, 'deny-input', 'a <label for> can name the input');
  await harness.act(() => harness.fireEvent(input(), 'focus'));
  await harness.input(input(), '~/secrets');
  await harness.act(() => harness.fireEvent(input(), 'keydown', { key: 'Enter' }));
  assert.deepEqual(state.value, ['~/secrets']);
  assert.equal(input().value, '', 'adding clears the box');

  await harness.input(input(), 'relative');
  await harness.act(() => harness.fireEvent(input(), 'keydown', { key: 'Enter' }));
  assert.deepEqual(state.value, ['~/secrets'], 'a relative path is not added');
  assert.match(host.querySelector('[role="alert"]').textContent, /absolute path or ~\//);

  await harness.input(input(), '/work');
  await harness.act(() => harness.fireEvent(input(), 'keydown', { key: 'Enter' }));
  assert.match(host.querySelector('.covers-cwd').textContent, /\/work/);
  assert.match(host.textContent, /covers the working directory \/work\/repo/);

  await harness.act(() => harness.fireEvent(host.querySelector('[aria-label="Remove denied path ~/secrets"]'), 'click'));
  assert.deepEqual(state.value, ['/work']);
});

test('a launch that cannot enforce a deny says the spawn will be refused and keeps every path removable', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, input } = await mountChips(harness, { value: ['~/secrets'], blockedReason: 'Sandbox off cannot enforce a deny.' });
  assert.equal(input().disabled, false, 'a pending path can still be cleared');
  assert.ok(host.querySelector('.spawn-deny-chip'), 'the chosen path stays on screen');
  assert.match(host.textContent, /daemon will refuse this spawn/);
});

// The uncommitted text is sent when valid, so it must stay on screen through
// anything that remounts the box: a queued spawn reopened, a blocked launch
// switched back. Only the draft carries it across.
test('a path still being typed stays on screen across a remount and a blocked launch', async (t) => {
  const harness = await createPreactHarness(t);
  const { DenyPathChips } = await harness.importDashboardModule('js/spawn-deny-paths.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const render = (props) => harness.mount(harness.html`<${DenyPathChips} id="deny" value=${[]} draftText="/srv/keys"
    onChange=${() => {}} onDraft=${() => {}} ...${props} />`, host);
  let mounted = await render({});
  assert.equal(host.querySelector('#deny-input').value, '/srv/keys', 'a reopened draft shows its pending path');
  await mounted.unmount();
  mounted = await render({ blockedReason: 'Sandbox off cannot enforce a deny.' });
  assert.equal(host.querySelector('#deny-input').value, '/srv/keys', 'blocked, the box keeps it, still editable');
  assert.match(host.textContent, /daemon will refuse this spawn/, 'and the refusal is foretold for a pending path too');
  await mounted.unmount();
});
