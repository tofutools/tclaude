// Tests for Update now / Check now / Test method: Config › Installation ›
// Updates' try-it row (UpdateNowPanel) and the Fleet node dialog's Test
// method button. Both act on the node's saved update method: the row holds
// Test and Update back while the form has unsaved method edits, a custom
// method updates now even with no newer release, and a dry run shows each
// check, what it would run and what it cannot check.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const PLAN = {
  method: 'custom', ready: false, current_version: 'v0.42.0',
  checks: [
    { name: 'Method', state: 'ok', detail: 'Run my own update command can run here.' },
    { name: 'Command', state: 'fail', detail: 'No custom command is set (updates.self.custom.command).' },
  ],
  steps: ['Ask GitHub for the latest release.', 'Restart agentd if a binary changed; agent sessions keep running.'],
  not_checked: ['What the custom command does: it is not run.'],
};

function fakeActions({ status = {}, plan = PLAN, preflightError = null } = {}) {
  const calls = [];
  return {
    calls,
    status: async () => ({ current_version: 'v0.42.0', latest_version: 'v0.42.0', update_available: false, chosen_method: 'custom', ...status }),
    start: async (node, action, version) => {
      calls.push({ action, version });
      return { id: 'j1', action, version: version || '', state: 'succeeded', phase: 'complete', warnings: ['the custom update command left tclaude unchanged: already up to date'] };
    },
    preflight: async () => { calls.push({ action: 'preflight' }); if (preflightError) throw preflightError; return structuredClone(plan); },
  };
}

async function mountPanel(t, props = {}, actionOpts = {}) {
  const harness = await createPreactHarness(t);
  const { UpdateNowPanel } = await harness.importDashboardModule('js/node-update.js');
  const actions = fakeActions(actionOpts);
  const confirms = [];
  const toasts = [];
  await harness.mount(harness.html`<${UpdateNowPanel} node=${{ id: 'self', label: 'this node', local: true }} actions=${actions}
    confirm=${async (o) => { confirms.push(o); return true; }} toast=${(m) => toasts.push(m)} timers=${globalThis} idp="cfg-" ...${props} />`);
  const q = (x) => harness.document.querySelector(x);
  const flush = () => harness.act(() => new Promise((r) => setTimeout(r, 10)));
  const deadline = Date.now() + 5000;
  while (Date.now() < deadline && /Reading/.test(q('#cfg-update-now-version')?.textContent || 'Reading')) await flush();
  return { harness, q, flush, actions, confirms, toasts };
}

test('Test method shows each check, what Update now would run and what it cannot check', async (t) => {
  const { q, flush, actions } = await mountPanel(t, { method: 'custom', methodLabel: 'Custom command' });
  assert.match(q('#cfg-update-now-version').textContent, /v0\.42\.0 · latest v0\.42\.0 · up to date/);
  assert.match(q('#cfg-update-now-note').textContent, /saved method \(Custom command\)/);
  assert.equal(q('#cfg-update-plan'), null, 'no result before a test');
  q('#cfg-update-now-test').click();
  await flush();
  assert.deepEqual(actions.calls, [{ action: 'preflight' }]);
  const plan = q('#cfg-update-plan');
  assert.equal(plan.getAttribute('data-ready'), 'false');
  assert.match(plan.textContent, /Would fail/);
  assert.match(plan.textContent, /nothing was installed/);
  assert.equal(plan.querySelector('li[data-state="fail"]').textContent.includes('No custom command is set'), true);
  assert.match(plan.querySelector('.update-plan-steps').textContent, /Restart agentd if a binary changed/);
  assert.match(plan.querySelector('.update-plan-unchecked').textContent, /it is not run/);
});

test('Update now with a custom method confirms in its own words and applies without pinning a release', async (t) => {
  const { q, flush, actions, confirms } = await mountPanel(t, { method: 'custom', methodLabel: 'Custom command' });
  assert.ok(!q('#cfg-update-now-apply').hasAttribute('disabled'), 'Update now works with no newer release');
  q('#cfg-update-now-apply').click();
  await flush();
  assert.equal(confirms.length, 1);
  assert.match(confirms[0].title, /Run the custom update command on this node/);
  assert.match(confirms[0].body, /even when it is already on the latest release/);
  assert.deepEqual(actions.calls, [{ action: 'apply', version: undefined }]);
  assert.match(q('#cfg-node-update-job').textContent, /apply.*succeeded/);
  assert.match(q('#cfg-node-update-job-warnings').textContent, /already up to date/);
});

test('unsaved method edits hold Test and Update back and say why; Check now still works', async (t) => {
  const { q, flush, actions } = await mountPanel(t, { method: 'custom', unsaved: true });
  assert.ok(q('#cfg-update-now-test').hasAttribute('disabled'));
  assert.ok(q('#cfg-update-now-apply').hasAttribute('disabled'));
  assert.match(q('#cfg-update-now-note').textContent, /use the saved settings: save changes first/);
  assert.ok(!q('#cfg-update-now-check').hasAttribute('disabled'));
  q('#cfg-update-now-check').click();
  await flush();
  assert.deepEqual(actions.calls.map((c) => c.action), ['check']);
});

test('a notify-only method cannot update now; a refused test says which grant it needs', async (t) => {
  const err = Object.assign(new Error('forbidden'), { status: 403 });
  const { q, flush } = await mountPanel(t, { method: 'manual', methodLabel: 'Notify me' }, { preflightError: err });
  assert.ok(q('#cfg-update-now-apply').hasAttribute('disabled'));
  assert.match(q('#cfg-update-now-apply').getAttribute('title'), /only notifies/);
  q('#cfg-update-now-test').click();
  await flush();
  assert.match(q('#cfg-update-plan').textContent, /needs node\.update/);
});

async function mountDialog(t, node, actionOpts = {}) {
  const harness = await createPreactHarness(t);
  const { NodeUpdateDialog } = await harness.importDashboardModule('js/node-update.js');
  const actions = fakeActions(actionOpts);
  await harness.mount(harness.html`<${NodeUpdateDialog} node=${node} actions=${actions} confirm=${async () => true} toast=${() => {}} timers=${globalThis} onClose=${() => {}} />`);
  const q = (x) => harness.document.querySelector(x);
  const flush = () => harness.act(() => new Promise((r) => setTimeout(r, 10)));
  const deadline = Date.now() + 5000;
  while (Date.now() < deadline && !q('#fleet-node-update-modal .fa-dl')) await flush();
  return { q, flush, actions };
}

test('the Fleet dialog tests a peer\'s method and offers Update now for a custom method', async (t) => {
  const { q, flush, actions } = await mountDialog(t, { id: 'peer-1', label: 'maria-laptop' });
  assert.match(q('#fleet-node-update-apply').textContent, /Update now/, 'a custom method updates with no newer release');
  q('#fleet-node-update-test').click();
  await flush();
  assert.deepEqual(actions.calls, [{ action: 'preflight' }]);
  assert.match(q('#fleet-update-plan').textContent, /No custom command is set/);
  assert.match(q('#fleet-node-update-modal').textContent, /--dry-run/);
});

test('the hub has no Test method, and a release method keeps Update to <latest>', async (t) => {
  const hub = await mountDialog(t, { id: 'hub', label: 'the hub', hub: true }, { status: { chosen_method: '', latest_version: 'v0.43.0', update_available: true } });
  assert.equal(hub.q('#fleet-node-update-test'), null);
  assert.match(hub.q('#fleet-node-update-apply').textContent, /Update to v0\.43\.0/);
});
