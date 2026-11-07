// Every way of making a drawing board or a live page asks its name first
// (resource-name-dialog.js): the Collab panel's ＋ New and the command
// palette as well as the Groups tab's ＋ (groups-processes.test.mjs). Each is
// driven through its production wiring (defaultNewContext, buildCommands)
// against a fake daemon, and what reaches the daemon is the typed name.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';
import { mountNameHost, submitName, waitFor, waitForNameDialog } from './resource-name-helpers.mjs';

const GROUP = { id: 7, name: 'crew', default_cwd: '/src/crew', members: [] };

function fakeDaemon(t) {
  const calls = [];
  const previous = globalThis.fetch;
  globalThis.fetch = async (path, init = {}) => {
    const method = init.method || 'GET';
    const body = init.body ? JSON.parse(init.body) : null;
    calls.push({ method, path: String(path), body });
    const json = (value) => ({ ok: true, status: 200, json: async () => value });
    if (method === 'GET' && path === '/api/drawing-boards') return json({ drawingBoards: [{ id: 'crew-drawing-board' }] });
    if (method === 'POST' && path === '/api/drawing-boards') return json({ id: body.id, title: body.title });
    if (method === 'POST' && path === '/api/pages') return json({ id: 'pg_new', name: body.name, title: body.title });
    return json({});
  };
  t.after(() => { globalThis.fetch = previous; });
  const posts = (path) => calls.filter((c) => c.method === 'POST' && c.path === path);
  return { calls, posts };
}

async function setup(t) {
  const harness = await createPreactHarness(t);
  harness.document.body.innerHTML = '<nav><button data-tab="groups" class="active"></button></nav>';
  const daemon = fakeDaemon(t);
  await mountNameHost(harness);
  return { harness, ...daemon };
}

test('the Collab panel\'s ＋ New › Drawing board asks the name, then creates it under that name', async (t) => {
  const { harness, posts } = await setup(t);
  const { NewMenu } = await harness.importDashboardModule('js/collab-new-menu.js');
  const { container } = await harness.mount(harness.html`<${NewMenu} group=${GROUP} groups=${[GROUP]} />`);
  await harness.act(() => container.querySelector('[data-new-menu="crew"]').click());
  await harness.act(() => harness.fireEvent(container.querySelector('[data-new-kind="drawing_board"]'), 'click'));
  const { input } = await waitForNameDialog(harness, 'the Collab ＋ asks for the drawing board\'s name');
  assert.equal(input.value, 'crew drawing board');
  assert.equal(posts('/api/drawing-boards').length, 0, 'nothing is made before the name is given');
  await submitName(harness, 'Release plan');
  const [made] = posts('/api/drawing-boards');
  assert.equal(made.body.title, 'Release plan');
  assert.match(made.body.id, /^release-plan/, 'its id follows the typed name');
  assert.deepEqual(posts(`/api/drawing-boards/${made.body.id}/group`).map((c) => c.body), [{ group: 'crew' }]);
});

test('the Collab panel\'s ＋ New › Page asks the name, then creates the page with it as its title', async (t) => {
  const { harness, posts } = await setup(t);
  const { NewMenu } = await harness.importDashboardModule('js/collab-new-menu.js');
  const { container } = await harness.mount(harness.html`<${NewMenu} group=${GROUP} groups=${[GROUP]} />`);
  await harness.act(() => container.querySelector('[data-new-menu="crew"]').click());
  await harness.act(() => harness.fireEvent(container.querySelector('[data-new-kind="page"]'), 'click'));
  const { input } = await waitForNameDialog(harness, 'the Collab ＋ asks for the page\'s name');
  assert.equal(input.value, 'sketch');
  await submitName(harness, 'Checkout mock');
  assert.deepEqual(posts('/api/pages').map((c) => c.body), [{ group: 'crew', name: 'sketch', title: 'Checkout mock' }]);
});

async function paletteCommand(harness, label) {
  await harness.replaceDashboardModule('js/dashboard.js', 'export let lastSnapshot = null; export function setLastSnapshot(v) { lastSnapshot=v; } export function webTerminalDefault() { return false; }');
  const { buildCommands } = await harness.importDashboardModule('js/palette.js');
  const cmd = buildCommands({ groups: [GROUP], agents: [] }).find((c) => c.label === label);
  assert.ok(cmd, `the palette lists ${label}`);
  return cmd;
}

test('the palette\'s "Make a drawing board in X" asks the name first', async (t) => {
  const { harness, posts } = await setup(t);
  const cmd = await paletteCommand(harness, 'Make a drawing board in crew');
  await harness.act(() => { void cmd.run(); });
  const { input } = await waitForNameDialog(harness, 'the palette asks for the drawing board\'s name');
  assert.equal(input.value, 'crew drawing board');
  await submitName(harness, 'Ops map');
  await waitFor(harness, () => posts('/api/drawing-boards').length === 1, 'the drawing board is created');
  assert.equal(posts('/api/drawing-boards')[0].body.title, 'Ops map');
});

test('the palette\'s "Make a live page in X" asks the name first', async (t) => {
  const { harness, posts } = await setup(t);
  const cmd = await paletteCommand(harness, 'Make a live page in crew');
  await harness.act(() => { void cmd.run(); });
  const { input } = await waitForNameDialog(harness, 'the palette asks for the page\'s name');
  assert.equal(input.value, 'sketch');
  await submitName(harness, 'Pricing');
  assert.deepEqual(posts('/api/pages').map((c) => c.body), [{ group: 'crew', name: 'sketch', title: 'Pricing' }]);
});
