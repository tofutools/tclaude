// Tests for the dock's process library (tcl-rzik1e): the one-line summary
// on each card, the preview graph built from a stored template, and the pin
// action that tells every library view and the group chips to reload.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function load(t) {
  const harness = await createPreactHarness(t);
  return harness.importDashboardModule('js/dock-processes.js');
}

test('processSummaryText reads steps, roles, commands and asks', async (t) => {
  const { processSummaryText } = await load(t);
  assert.equal(processSummaryText({ summary: { steps: 5, roles: ['dev', 'reviewer'], commands: 1 } }),
    '5 steps · dev, reviewer · 1 command');
  assert.equal(processSummaryText({ summary: { steps: 1, asks: 1 } }), '1 step · asks you once');
  assert.equal(processSummaryText({ summary: { steps: 4, commands: 2, asks: 3 } }), '4 steps · 2 commands · asks you 3×');
  assert.equal(processSummaryText({}), '0 steps');
});

test('templateAsRun plans every step and labels edges with their outcome', async (t) => {
  const { templateAsRun } = await load(t);
  const run = templateAsRun({
    template: { nodes: { build: { type: 'task', name: 'Build', performer: { kind: 'agent', role: 'dev' } }, done: { type: 'end' } } },
    edges: [{ from: '', outcome: 'start', to: 'build' }, { from: 'build', outcome: 'pass', to: 'done' }],
  });
  assert.deepEqual(run.nodes, [
    { id: 'build', kind: 'task', state: 'planned', label: 'Build', performer: { kind: 'agent' } },
    { id: 'done', kind: 'end', state: 'planned', label: 'done', performer: undefined },
  ]);
  assert.deepEqual(run.edges, [{ from: 'build', to: 'done', label: 'pass' }]);
});

test('pinToGroup pins, then asks library views and the process context to reload', async (t) => {
  const { pinToGroup, PROCESS_LIBRARY_EVENT } = await load(t);
  const calls = [];
  const events = [];
  const doc = { dispatchEvent: (event) => events.push(event.type) };
  await pinToGroup('webshop', 'ship-feature', {
    api: { pinProcess: async (group, id) => calls.push([group, id]) },
    doc,
    context: { refresh: async () => calls.push(['refresh']) },
  });
  assert.deepEqual(calls, [['webshop', 'ship-feature'], ['refresh']]);
  assert.deepEqual(events, [PROCESS_LIBRARY_EVENT]);
});

test('unpinFromGroup unpins, then asks library views and the process context to reload', async (t) => {
  const { unpinFromGroup, PROCESS_LIBRARY_EVENT } = await load(t);
  const calls = [];
  const events = [];
  const doc = { dispatchEvent: (event) => events.push(event.type) };
  await unpinFromGroup('tclaude', 'ship-feature', {
    api: { unpinProcess: async (group, id) => calls.push([group, id]) },
    doc,
    context: { refresh: async () => calls.push(['refresh']) },
  });
  assert.deepEqual(calls, [['tclaude', 'ship-feature'], ['refresh']]);
  assert.deepEqual(events, [PROCESS_LIBRARY_EVENT]);
});

test('a Start in request expands the group and expires if never taken', async (t) => {
  const harness = await createPreactHarness(t);
  const { requestProcessStart, takeProcessStart } = await harness.importDashboardModule('js/process-start-handoff.js');
  const prefs = new Map();
  let rerenders = 0;
  const doc = { querySelector: () => null, dispatchEvent: () => true };
  const opts = { doc, prefs: { setItem: (k, v) => prefs.set(k, v) }, setFocus: () => {}, rerender: () => rerenders++, now: () => 1000 };
  requestProcessStart({ group: 'webshop', templateId: 'ship' }, opts);
  assert.equal(prefs.get('tclaude.dash.group.webshop'), '1');
  assert.equal(rerenders, 1);
  assert.equal(takeProcessStart('infra', { now: () => 1000 }), null);
  assert.deepEqual(takeProcessStart('webshop', { now: () => 2000 }), { group: 'webshop', templateId: 'ship' });
  assert.equal(takeProcessStart('webshop', { now: () => 2000 }), null, 'taken once');
  requestProcessStart({ group: 'webshop', templateId: 'ship' }, opts);
  assert.equal(takeProcessStart('webshop', { now: () => 60_000 }), null, 'stale request dropped');
});

test('a Start in request for a nested group opens each ancestor and its Subgroups section', async (t) => {
  const harness = await createPreactHarness(t);
  const { requestProcessStart } = await harness.importDashboardModule('js/process-start-handoff.js');
  const prefs = new Map();
  const doc = { querySelector: () => null, dispatchEvent: () => true };
  const groups = [{ name: 'org' }, { name: 'team', parent: 'org' }, { name: 'squad', parent: 'team' }];
  requestProcessStart({ group: 'squad', templateId: 'ship' }, {
    doc, groups, prefs: { getItem: (k) => prefs.get(k) ?? null, setItem: (k, v) => prefs.set(k, v) }, now: () => 1000,
  });
  for (const name of ['org', 'team']) {
    assert.equal(prefs.get(`tclaude.dash.group.${name}`), '1', `${name} row opens`);
    assert.equal(prefs.get(`tclaude.dash.groups.fused.subgroups.${name}`), '1', `${name} subgroups open`);
  }
  assert.equal(prefs.has('tclaude.dash.groups.fused.subgroups.squad'), false, 'the target keeps its own Subgroups fold');
});

test('deleteFromLibrary asks first with the shared copy, then deletes and reloads', async (t) => {
  const { deleteFromLibrary, PROCESS_LIBRARY_EVENT } = await load(t);
  const row = { id: 'ship', name: 'Ship it', versionCount: 3 };
  const asked = [];
  const calls = [];
  const events = [];
  const doc = { body: { classList: { contains: () => false } }, dispatchEvent: (event) => events.push(event.type) };
  const fetchImpl = async (url, init) => { calls.push([url, init.method]); return { ok: true, status: 200, json: async () => ({}) }; };

  assert.equal(await deleteFromLibrary(row, { confirm: async (request) => { asked.push(request); return false; }, fetchImpl, doc, notify: null, context: null }), false);
  assert.deepEqual(calls, [], 'declined: nothing deleted');
  assert.equal(asked[0].title, 'Delete this process template?');
  assert.match(asked[0].body, /Ship it and all 3 versions/);
  assert.equal(asked[0].meta, 'ship');

  const refreshed = [];
  assert.equal(await deleteFromLibrary(row, { confirm: async () => true, fetchImpl, doc, notify: null, context: { refresh: async () => refreshed.push(1) } }), true);
  assert.deepEqual(calls, [['/v1/process/templates/ship', 'DELETE']]);
  assert.deepEqual(events, [PROCESS_LIBRARY_EVENT]);
  assert.equal(refreshed.length, 1);
});

test('deleteFromLibrary reports a refused delete', async (t) => {
  const { deleteFromLibrary } = await load(t);
  const doc = { body: null, dispatchEvent: () => assert.fail('no reload on failure') };
  await assert.rejects(deleteFromLibrary({ id: 'ship' }, {
    confirm: async () => true, doc, notify: null, context: null,
    fetchImpl: async () => ({ ok: false, status: 403, statusText: 'Forbidden', json: async () => ({ message: 'not allowed' }) }),
  }), /not allowed/);
});

test('the Processes heading carries the same ＋ ⧉ ⋯ tools as the other sections', async (t) => {
  const harness = await createPreactHarness(t);
  const { DockProcesses } = await harness.importDashboardModule('js/dock-processes.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  let openMenu = null;
  const setOpenMenu = (value) => { openMenu = value; };
  await harness.mount(harness.html`<${DockProcesses} isSectionOpen=${() => false} setSectionOpen=${() => {}}
    openMenu=${'section:processes'} setOpenMenu=${setOpenMenu} />`, host);
  const head = host.querySelector('summary.dock-section-head');
  assert.deepEqual([...head.querySelectorAll('.dock-section-tools > .dock-section-manage, .dock-section-tools .dock-card-actions > .dock-section-manage')]
    .map((button) => button.dataset.dockAct), ['new-item', 'manage-all', 'section-menu']);
  assert.deepEqual([...head.querySelectorAll('.dock-card-menu.open [role="menuitem"]')].map((item) => item.textContent),
    ['Import .pwf…', 'Export all']);
  assert.ok(host.querySelector('[data-pwf-file]'), 'the .pwf importer stays mounted for the menu to drive');
  await harness.act(() => harness.fireEvent(head.querySelector('[data-dock-act="section-menu"]'), 'click'));
  assert.equal(openMenu, null, 'a second click closes the open menu');
});
