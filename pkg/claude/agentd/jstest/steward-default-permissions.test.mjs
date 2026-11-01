import assert from 'node:assert/strict';
import test from 'node:test';
import { createPreactHarness } from './preact-harness.mjs';

const access = { convs: [], config: ['fleet.read', 'dashboard.capture'], carried: { 'agent.spawn': 'grant', 'agent.task': 'grant', 'dashboard.capture': 'deny', 'fleet.read': 'grant' } };

test('the Config row counts what is stored and opens the editor on future Stewards', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ StewardDefaultPermissions }, { dashboardState }] = await Promise.all([
    harness.importDashboardModule('js/steward-default-permissions.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
  ]);
  dashboardState.snapshot.value = { steward_access: access };
  const calls = [];
  const fetchImpl = async (url, options = {}) => {
    calls.push({ url, ...options });
    return new Response(JSON.stringify({ stored: { 'agent.spawn': 'grant' }, levels: ['fleet.read'] }), { status: 200 });
  };
  let opened = null;
  const mounted = await harness.mount(harness.html`<${StewardDefaultPermissions} fetchImpl=${fetchImpl} openEditor=${(options) => { opened = options; }} />`);
  assert.equal(mounted.container.querySelector('#cfg-steward-permissions-summary').textContent, '2 added, 1 denied', 'a stored grant the levels give adds nothing');

  assert.equal(mounted.container.querySelector('#cfg-steward-permissions-edit').className, 'cfg-inline', 'styled like the other Config row buttons');
  await harness.act(async () => { mounted.container.querySelector('#cfg-steward-permissions-edit').click(); });
  for (let i = 0; i < 50 && !opened; i += 1) await new Promise((resolve) => { setTimeout(resolve, 5); });
  assert.deepEqual(opened.stewardLevels, ['fleet.read']);
  assert.deepEqual(opened.overrides, { 'agent.spawn': 'grant' });

  await opened.onSave({ 'node.exec': 'deny' });
  assert.equal(calls.at(-1).method, 'PUT');
  assert.deepEqual(JSON.parse(calls.at(-1).body), { overrides: { 'node.exec': 'deny' } });
  await mounted.unmount();
});

test('with Collab off the row says so instead of offering a closed route', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ StewardDefaultPermissions }, { dashboardState }] = await Promise.all([
    harness.importDashboardModule('js/steward-default-permissions.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
  ]);
  dashboardState.snapshot.value = {};
  const mounted = await harness.mount(harness.html`<${StewardDefaultPermissions} />`);
  assert.equal(mounted.container.querySelector('#cfg-steward-permissions-summary').textContent, 'Needs Collab');
  assert.equal(mounted.container.querySelector('#cfg-steward-permissions-edit'), null);
  await mounted.unmount();
});

test('a failed save surfaces to the editor', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ StewardDefaultPermissions }, { dashboardState }] = await Promise.all([
    harness.importDashboardModule('js/steward-default-permissions.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
  ]);
  dashboardState.snapshot.value = { steward_access: access };
  let first = true;
  const fetchImpl = async () => {
    if (first) { first = false; return new Response('{"stored":{},"levels":[]}', { status: 200 }); }
    return new Response('{"error":"nope"}', { status: 400 });
  };
  let opened = null;
  const mounted = await harness.mount(harness.html`<${StewardDefaultPermissions} fetchImpl=${fetchImpl} openEditor=${(options) => { opened = options; }} />`);
  await harness.act(async () => { mounted.container.querySelector('#cfg-steward-permissions-edit').click(); });
  for (let i = 0; i < 50 && !opened; i += 1) await new Promise((resolve) => { setTimeout(resolve, 5); });
  await assert.rejects(opened.onSave({}), /nope/);
  await mounted.unmount();
});

test('the view tick says when capture is off', async (t) => {
  const harness = await createPreactHarness(t);
  const { FloatingTools } = await harness.importDashboardModule('js/dictation-tools.js');
  const dictation = { listening: false, block: null, toggle: () => {}, continuing: false };
  const mounted = await harness.mount(harness.html`<${FloatingTools} dictation=${dictation} micLabel="m" micTitle="m" withContext=${true} contextNote="off for it" />`);
  const note = mounted.container.querySelector('.companion-context-off');
  assert.equal(note.textContent, 'capture off');
  assert.equal(note.getAttribute('title'), 'off for it');
  await mounted.unmount();
});
