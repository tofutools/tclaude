import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const POLICY = {
  scope: 'global',
  can_manage: true,
  rows: [
    { field: 'group_env', label: 'Group environment', mode: 'reuse', default: 're-resolve', modes: ['re-resolve', 'reuse'] },
    { field: 'sandbox_profile_contents', label: 'Sandbox profile contents', mode: 're-resolve', default: 're-resolve', modes: ['re-resolve', 'reuse'] },
  ],
  fields: [
    { key: 'harness', label: 'Harness', layer: 'fixed', configurable: false },
    { key: 'model', label: 'Model', layer: 'fixed', configurable: false },
    { key: 'approval', label: 'Approval policy', layer: 'spawn copy', configurable: false },
    { key: 'identity', label: 'Agent name and description', layer: 'spawn only', configurable: false },
    { key: 'group_env', label: 'Group environment', layer: 'group setting', configurable: true },
  ],
};

async function mount(t, fetchImpl) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/spawn-on-resume.js');
  const view = await harness.mount(harness.html`<${mod.SpawnOnResumeRow} fetchImpl=${fetchImpl} />`);
  const flush = () => harness.act(async () => { for (let i = 0; i < 8; i += 1) await Promise.resolve(); });
  await flush();
  return { harness, mod, view, flush };
}

const ok = (body) => async () => ({ ok: true, status: 200, json: async () => body });

test('on resume: collapsed to a summary, Show expands the read-only table', async (t) => {
  const { view, harness, flush } = await mount(t, ok(POLICY));
  const c = view.container;
  assert.match(c.querySelector('[data-on-resume-summary]').textContent, /2 configurable rows; changed: Group environment → Reuse/);
  assert.equal(c.querySelector('#agent-spawn-on-resume-table'), null, 'collapsed by default');
  const show = [...c.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Show');
  await harness.act(async () => { show.click(); });
  await flush();
  assert.ok(c.querySelector('#agent-spawn-on-resume-table'));
  assert.match(c.querySelector('[data-on-resume-field="group_env"]').textContent, /Reuse/);
  assert.match(c.querySelector('[data-on-resume-layer="fixed"]').textContent, /Harness · Model/);
  assert.match(c.querySelector('[data-on-resume-layer="copy"]').textContent, /Copied at spawn/);
  assert.equal(c.querySelector('[data-on-resume-layer="once"]'), null, 'spawn-only inputs are not a resume question');
  assert.equal(c.querySelectorAll('select, input').length, 0, 'read-only');
});

test('on resume: says nothing when the policy cannot be read', async (t) => {
  const { view } = await mount(t, async () => ({ ok: false, status: 500, json: async () => ({}) }));
  assert.equal(view.container.querySelector('#agent-spawn-on-resume-row'), null);
});

test('on resume: the summary at defaults', async (t) => {
  const harness = await createPreactHarness(t);
  const { onResumeSummary } = await harness.importDashboardModule('js/spawn-on-resume.js');
  assert.equal(onResumeSummary([{ mode: 're-resolve', default: 're-resolve' }]), '1 configurable row, all at their defaults');
});
