import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ROWS = [
  { field: 'group_env', label: 'Group environment', mode: 're-resolve', default: 're-resolve', modes: ['re-resolve', 'reuse'], set: false, settable: true, why: 'Group settings are kept current.' },
  { field: 'harness_config', label: 'Harness config posture', mode: 'explicit-wins', default: 'explicit-wins', modes: ['explicit-wins', 'tighten-only', 'chain-wins'], set: false, settable: true, why: 'Least surprise.' },
];
const FIELDS = [
  { key: 'harness', label: 'Harness', layer: 'fixed', configurable: false, why: 'The conversation belongs to one harness.' },
  { key: 'approval', label: 'Approval policy', layer: 'spawn copy', configurable: false, why: 'Part of the spawn profile copy.' },
  { key: 'group_access', label: 'Group permissions and owners', layer: 'live', configurable: false, why: 'Applied to members at once.' },
  { key: 'group_env', label: 'Group environment', layer: 'group setting', configurable: true, why: 'x' },
  { key: 'harness_config', label: 'Harness config posture', layer: 'referenced profile', configurable: true, why: 'x' },
];

function daemon({ canManage = true } = {}) {
  const calls = [];
  let rows = ROWS.map((r) => ({ ...r }));
  const reply = (status, body) => ({ ok: status < 400, status, json: async () => body });
  const view = () => ({ scope: 'global', rows, fields: FIELDS, can_manage: canManage });
  const fetchImpl = async (url, init = {}) => {
    const body = init.body ? JSON.parse(init.body) : null;
    calls.push({ url, method: init.method || 'GET', body });
    if (init.method === 'PUT') {
      rows = rows.map((r) => (r.field === body.field
        ? { ...r, mode: body.mode || r.default, set: !!body.mode && body.mode !== r.default } : r));
    }
    return reply(200, view());
  };
  return { calls, fetchImpl };
}

async function mount(t, opts) {
  const harness = await createPreactHarness(t);
  const { RelaunchPolicyTable } = await harness.importDashboardModule('js/relaunch-policy-table.js');
  const fake = daemon(opts);
  const view = await harness.mount(harness.html`<${RelaunchPolicyTable} fetchImpl=${fake.fetchImpl} />`);
  const flush = async () => harness.act(async () => { for (let i = 0; i < 8; i += 1) await Promise.resolve(); });
  await flush();
  const row = (field) => view.container.querySelector(`[data-rlp-field="${field}"]`);
  const button = (scope, label) => [...scope.querySelectorAll('button')].find((b) => b.textContent.trim() === label);
  return { ...fake, harness, view, flush, row, button };
}

test('relaunch policy: configurable rows get controls, the rest pills with their Why', async (t) => {
  const { row, button, view } = await mount(t);
  const env = row('group_env');
  assert.equal(button(env, 'Re-resolve').getAttribute('aria-pressed'), 'true');
  assert.equal(button(env, 'Reuse').getAttribute('aria-pressed'), 'false');
  const select = row('harness_config').querySelector('select');
  assert.equal(select.getAttribute('value'), 'explicit-wins');
  assert.equal(select.querySelectorAll('option').length, 3);
  assert.match(row('harness').textContent, /Fixed/);
  assert.match(row('harness').textContent, /one harness/);
  assert.match(row('approval').textContent, /Copied at spawn/);
  assert.match(row('group_access').textContent, /Live/);
  assert.equal([...view.container.querySelectorAll('[data-rlp-field="group_env"]')].length, 1, 'a configurable row is listed once');
});

test('relaunch policy: Reuse saves at once, and Reset to defaults clears the set rows', async (t) => {
  const { calls, row, button, flush, view, harness } = await mount(t);
  const reset = button(view.container, 'Reset to defaults');
  assert.equal(reset.disabled, true, 'nothing to reset');
  await harness.act(async () => { button(row('group_env'), 'Reuse').click(); });
  await flush();
  assert.deepEqual(calls.at(-1), { url: '/api/relaunch-policy', method: 'PUT', body: { field: 'group_env', mode: 'reuse' } });
  assert.equal(button(row('group_env'), 'Reuse').getAttribute('aria-pressed'), 'true');
  assert.match(row('group_env').textContent, /changed/);

  await harness.act(async () => { button(view.container, 'Reset to defaults').click(); });
  await flush();
  assert.deepEqual(calls.at(-1).body, { field: 'group_env', mode: '' });
  assert.equal(button(row('group_env'), 'Re-resolve').getAttribute('aria-pressed'), 'true');
});

test('relaunch policy: without relaunch-policy.manage the table is read-only with a hint', async (t) => {
  const { row, button, view } = await mount(t, { canManage: false });
  assert.ok(view.container.querySelector('[data-rlp-readonly]'));
  assert.equal(button(row('group_env'), 'Reuse').disabled, true);
  assert.equal(row('harness_config').querySelector('select').disabled, true);
  assert.equal(button(view.container, 'Reset to defaults').disabled, true);
});
