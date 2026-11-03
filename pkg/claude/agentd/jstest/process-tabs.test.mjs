// Tests for process templates as tabs of the Collab strip (tcl-tnbtwp): the ＋
// menu's Processes half, the one live editor moving between the strip, an
// overlay window and the Processes tab without losing its draft, closing with
// unsaved edits, and the /collab/processes routes. The shell is the real
// mounted feature; xterm, the daemon and the editor itself are faked.

import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

const wait = (ms = 0) => new Promise((r) => setTimeout(r, ms));
const ON = { processes_enabled: true, agents: [], groups: [] };
const TEMPLATES = [
  { id: 'release-train', name: 'Release train', versionCount: 3, pinnedGroups: ['tclaude'] },
  { id: 'triage', name: 'Triage', versionCount: 1 },
  { id: 'review', name: 'Code review', versionCount: 2, pinnedGroups: ['tclaude', 'web'] },
];

function installHosts(harness) {
  const doc = harness.document;
  const nav = doc.body.appendChild(doc.createElement('nav'));
  const main = doc.body.appendChild(doc.createElement('main'));
  for (const name of ['groups', 'terminals', 'processes']) {
    const button = nav.appendChild(doc.createElement('a'));
    button.dataset.tab = name;
    button.addEventListener('click', (event) => {
      event.preventDefault();
      for (const item of nav.querySelectorAll('[data-tab]')) item.classList.toggle('active', item === button);
      for (const section of main.querySelectorAll('section')) section.classList.toggle('active', section.id === `tab-${name}`);
    });
    main.appendChild(doc.createElement('section')).id = `tab-${name}`;
  }
  const host = doc.getElementById('tab-terminals').appendChild(doc.createElement('div'));
  host.id = 'terminals-root';
  const badgeHost = nav.appendChild(doc.createElement('span'));
  badgeHost.id = 'terminals-badge-root';
  return { host, badgeHost };
}

function fakeDaemon(t, templates = TEMPLATES) {
  const requests = [];
  const previous = globalThis.fetch;
  globalThis.fetch = async (url, init = {}) => {
    const path = String(url);
    requests.push(`${init.method || 'GET'} ${path}`);
    if (path === '/v1/process/templates' && init.method === 'POST') {
      const body = JSON.parse(init.body);
      return { ok: true, status: 201, json: async () => ({ id: 'night-shift', ref: 'r1', sourceHash: 's', semanticHash: 'h', name: body.template.name }) };
    }
    if (path === '/v1/process/templates') return { ok: true, status: 200, json: async () => ({ templates }) };
    if (path === '/v1/process/template-heads') return { ok: true, status: 200, json: async () => ({ heads: [] }) };
    return { ok: true, status: 200, json: async () => ({}) };
  };
  t.after(() => { globalThis.fetch = previous; });
  return requests;
}

// The Processes tab, as process-editor-place.js sees it: it opens editors, and
// may hold one docked.
function fakeProcessesHost(place) {
  const host = {
    opened: [], docked: [], held: null, discard: true, asked: 0,
    open: async (mount, id, _name, _handoff, seed = {}) => {
      const editor = { mount, seed, model: { dirty: false, template: { id } }, destroyed: 0, destroy() { this.destroyed += 1; } };
      mount.textContent = `editor ${id}`;
      host.opened.push(editor);
      return editor;
    },
    dock: (id, live) => host.docked.push({ id, live }),
    release: (id) => {
      const held = host.held;
      if (held?.editor.model.template.id !== id) return null;
      host.held = null;
      return held;
    },
    canLeave: async () => true,
    confirmDiscard: async () => { host.asked += 1; return host.discard; },
  };
  return host;
}

async function mountShell(t, snapshotValue = ON) {
  const harness = await createPreactHarness(t);
  const hosts = installHosts(harness);
  const widgets = [];
  const factory = (options) => {
    options.host.append(harness.document.createElement('textarea'));
    const widget = { options, connect: () => Promise.resolve(true), copy: () => Promise.resolve(), fit() {}, focus() {}, setActive() {}, status: () => 'connected', dispose() {} };
    widgets.push(widget);
    return widget;
  };
  const [{ mountTerminalsFeature }, controller, place, instances, { dashboardState }, { terminalShellState }, closeNav] = await Promise.all([
    harness.importDashboardModule('js/preact-loader.js'),
    harness.importDashboardModule('js/terminals-tab.js'),
    harness.importDashboardModule('js/process-editor-place.js'),
    harness.importDashboardModule('js/overlay-instances.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/terminal-shell-state.js'),
    harness.importDashboardModule('js/terminal-close-nav.js'),
  ]);
  dashboardState.publishLocalEdit(snapshotValue);
  closeNav.setStayOnTerminalsAfterLastClose(false);
  const processes = fakeProcessesHost(place);
  t.after(place.registerProcessEditorHost(processes));
  t.after(() => { instances.overlayWindows.value = []; });
  const cleanup = await mountTerminalsFeature({ widgetFactory: factory, fetchImpl: async () => ({ ok: true }) });
  await harness.act(() => wait());
  let done = false;
  const stop = () => { if (!done) { done = true; cleanup(); } };
  t.after(stop);
  const processTabs = () => instances.overlayTabs.peek().filter((tab) => tab.kind === place.PROCESS_WINDOW_KIND);
  return { harness, ...hosts, widgets, controller, place, instances, state: terminalShellState, processes, processTabs, stop, setSnapshot: (value) => dashboardState.publishLocalEdit(value) };
}

const tabLabels = (host) => [...host.querySelectorAll('.mux-tab .mux-tab-label')].map((node) => node.textContent);

test('the ＋ menu lists processes by pinned group, filters them, and opens one as a tab', async (t) => {
  const requests = fakeDaemon(t);
  const env = await mountShell(t);
  const { harness, host, widgets, processes } = env;
  await harness.act(async () => { harness.fireEvent(host.querySelector('.mux-board-add'), 'click'); await wait(); });
  const switcher = host.querySelector('.mux-board-menu .mux-add-sections');
  assert.ok(switcher, 'with Processes on, the menu offers Boards and Processes');
  await harness.act(async () => { harness.fireEvent([...switcher.querySelectorAll('[role="tab"]')].find((tab) => tab.textContent === 'Processes'), 'click'); await wait(); });
  await harness.act(() => wait());
  assert.ok(requests.includes('GET /v1/process/templates'));
  const sections = () => [...host.querySelectorAll('.process-menu-list .board-section')].map((section) => ({
    name: section.querySelector('.board-section-name span')?.textContent,
    rows: [...section.querySelectorAll('.board-row-title')].map((row) => row.textContent),
  }));
  assert.deepEqual(sections(), [
    { name: 'tclaude', rows: ['Code review', 'Release train'] },
    { name: 'web', rows: ['Code review'] },
    { name: 'Not pinned', rows: ['Triage'] },
  ]);
  const box = host.querySelector('.process-menu-list input');
  await harness.act(async () => { box.value = 'train'; harness.fireEvent(box, 'input'); await wait(); });
  assert.deepEqual(sections(), [{ name: 'tclaude', rows: ['Release train'] }]);

  await harness.act(async () => { harness.fireEvent(host.querySelector('[data-process-menu-row="release-train"] .board-row-open'), 'click'); await wait(); });
  await harness.act(() => wait());
  assert.deepEqual(tabLabels(host), ['Release train']);
  assertAbsent(host.querySelector('.mux-board-menu'), 'picking a process closes the menu');
  assert.equal(widgets.length, 0, 'a process tab mounts no terminal');
  assert.equal(host.querySelector('.mux-tab[data-agent-status="process"]')?.getAttribute('data-pane-key'), 'process:release-train');
  const pane = host.querySelector('.mux-pane-process.active');
  assert.ok(pane.querySelector('#process-editor-canvas'), 'the tab shows the editor');
  assert.equal(processes.opened.length, 1);
  assert.equal(env.place.floatingProcessEditor('release-train').editor, processes.opened[0]);
  assert.ok(harness.document.getElementById('tab-terminals').classList.contains('active'));
  env.stop();
});

test('a new process from the ＋ menu is stored and opens as a tab', async (t) => {
  const requests = fakeDaemon(t);
  const env = await mountShell(t);
  const { harness, host, state } = env;
  await harness.act(async () => { state.requestBoardMenu('processes'); await wait(); });
  await harness.act(() => wait());
  const box = host.querySelector('.process-menu-list input');
  await harness.act(async () => { box.value = 'Night shift'; harness.fireEvent(box, 'input'); await wait(); });
  const submit = host.querySelector('.process-menu-list button[type="submit"]');
  assert.equal(submit.textContent, '＋ New process');
  await harness.act(async () => { harness.fireEvent(host.querySelector('.process-menu-list form'), 'submit'); await wait(20); });
  await harness.act(() => wait());
  assert.ok(requests.includes('POST /v1/process/templates'));
  assert.deepEqual(tabLabels(host), ['Night shift']);
  assert.equal(state.activeKey.value, 'process:night-shift');
  env.stop();
});

test('the editor moves between the strip, an overlay window and the Processes tab with its draft', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t);
  const { harness, host, place, processes, processTabs, state } = env;
  // Docked in the Processes tab with unsaved edits, it moves to the strip as it is.
  const mount = place.processEditorMount(harness.document);
  const docked = { mount, model: { dirty: true, template: { id: 'release-train' } }, destroyed: 0, destroy() { this.destroyed += 1; } };
  processes.held = { mount, editor: docked, label: 'Release train' };
  await harness.act(async () => { await place.openProcessTab('release-train'); await wait(); });
  assert.deepEqual(tabLabels(host), ['Release train']);
  assert.equal(host.querySelector('.mux-pane-process #process-editor-canvas'), mount, 'the strip shows the docked editor itself');
  assert.equal(processes.opened.length, 0, 'no second editor opens');

  // To an overlay window, and back: one place at a time, the same editor.
  await harness.act(async () => { harness.fireEvent([...host.querySelectorAll('.mux-process-head button')].find((b) => b.textContent === 'Open in overlay'), 'click'); await wait(); });
  assert.deepEqual(tabLabels(host), [], 'the strip lets it go');
  assert.deepEqual(processTabs().map((tab) => tab.template), ['release-train']);
  assert.equal(place.floatingProcessEditor('release-train').editor, docked);
  assert.equal(docked.destroyed, 0);
  assert.equal((await place.openProcessTab('release-train'))?.floating, true, 'opening it again shows the overlay window');
  assert.equal(state.panes.value.length, 0);

  await harness.act(async () => { await place.processTabToCollab(processTabs()[0]); await wait(); });
  assert.deepEqual(processTabs(), []);
  assert.deepEqual(tabLabels(host), ['Release train']);
  assert.equal(host.querySelector('.mux-pane-process #process-editor-canvas'), mount);
  assert.equal(docked.destroyed, 0);

  // To the Processes tab: it docks there, unsaved edits and all.
  await harness.act(async () => { harness.fireEvent([...host.querySelectorAll('.mux-process-head button')].find((b) => b.textContent === 'Open in the Processes tab'), 'click'); await wait(); });
  assert.deepEqual(tabLabels(host), []);
  assert.equal(processes.docked.length, 1);
  assert.equal(processes.docked[0].live.editor, docked);
  assert.equal(docked.model.dirty, true);
  assert.equal(docked.destroyed, 0);
  assert.equal(processes.asked, 0, 'no move asks about the edits it carries');
  env.stop();
});

test('closing a process tab with unsaved edits asks first; closed for good, its editor goes', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t);
  const { harness, host, place, processes } = env;
  await harness.act(async () => { await place.openProcessTab('triage', 'Triage'); await wait(); });
  await harness.act(() => wait());
  const editor = processes.opened[0];
  editor.model.dirty = true;
  processes.discard = false;
  const close = () => harness.act(async () => { harness.fireEvent(host.querySelector('.mux-tab[data-agent-status="process"] .mux-tab-close'), 'click'); await wait(); });
  await close();
  assert.equal(processes.asked, 1);
  assert.deepEqual(tabLabels(host), ['Triage'], 'kept edits keep the tab');
  assert.equal(editor.destroyed, 0);
  processes.discard = true;
  await close();
  assert.deepEqual(tabLabels(host), []);
  assert.equal(editor.destroyed, 1);
  assert.equal(place.floatingProcessEditor('triage'), null);
  env.stop();
});

test('/collab/processes/<id> opens that tab; bare /collab/processes offers the list', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t);
  const { harness, host, state } = env;
  const restore = (location) => harness.act(async () => {
    harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:restore-location', { detail: { location } }));
    await wait();
  });
  await restore({ tab: 'collab-processes', selection: 'triage' });
  assert.equal(state.activeKey.value, 'process:triage');
  assert.deepEqual(state.location.value, { tab: 'collab-processes', selection: 'triage' });
  await restore({ tab: 'collab-processes', selection: '../etc' });
  assert.equal(state.panes.value.length, 1, 'an invalid id opens nothing');
  await restore({ tab: 'collab-processes' });
  await harness.act(() => wait());
  assert.ok(host.querySelector('.mux-board-menu .process-menu-list'), 'a bare route opens the Processes half of the menu');

  const core = await harness.importDashboardModule('js/nav-history-core.js');
  assert.deepEqual(core.fromPath('/collab/processes/triage', ''), { tab: 'collab-processes', selection: 'triage' });
  assert.equal(core.toPath({ tab: 'collab-processes', selection: 'triage' }), '/collab/processes/triage');
  assert.equal(core.isKnownPath('/collab/processes/triage'), true);
  env.stop();
});

test('a crafted seed cannot give a process tab a socket or a colliding key', async (t) => {
  const harness = await createPreactHarness(t);
  const { normalizeSeed } = await harness.importDashboardModule('js/terminals-core.js');
  assert.equal(normalizeSeed({ process: '../etc', ws: '/x' }), null);
  assert.equal(normalizeSeed({ process: 'triage', board: 'plan' }), null);
  const seed = normalizeSeed({ process: 'triage', ws: 'wss://evil', key: 'window:agt_x' });
  assert.equal(seed.ws, undefined);
  assert.equal(seed.key, 'process:triage');
});

test('with Processes off the ＋ menu is boards only', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t, { ...ON, processes_enabled: false });
  const { harness, host } = env;
  await harness.act(async () => { harness.fireEvent(host.querySelector('.mux-board-add'), 'click'); await wait(); });
  assertAbsent(host.querySelector('.mux-add-sections'), 'no Processes half');
  assertAbsent(host.querySelector('.process-menu-list'));
  env.stop();
});

test('the Groups full editor opens in an overlay window over the tab, on the version it was handed', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t);
  const { harness, place, processes, processTabs, instances } = env;
  const { openProcessDraft, takeProcessDraft } = await harness.importDashboardModule('js/process-draft-handoff.js');
  const nav = harness.document.querySelector('nav [data-tab="groups"]');
  harness.fireEvent(nav, 'click');
  // The companion's overlay host, as when Collab is on.
  instances.overlayHostUp.value = true;
  t.after(() => { instances.overlayHostUp.value = false; });
  const view = { template: { id: 'triage', name: 'Triage' }, currentRef: 'triage@sha256:1', sourceHash: 's1' };
  let where;
  await harness.act(async () => { where = await openProcessDraft({ id: 'triage', name: 'Triage', view }); await wait(10); });
  assert.equal(where, 'overlay');
  assert.deepEqual(processTabs().map((tab) => [tab.template, tab.label]), [['triage', 'Triage']]);
  // The overlay window, shown, opens its editor.
  const slot = harness.document.body.appendChild(harness.document.createElement('div'));
  await harness.act(async () => { await place.openFloatingEditor('triage', 'Triage', slot); });
  assert.equal(processes.opened.length, 1);
  assert.deepEqual(processes.opened[0].seed, { view, readable: true }, 'the editor opens on the handed-off version');
  assert.ok(nav.classList.contains('active'), 'the operator stays on Groups');
  assert.equal(takeProcessDraft(), null, 'nothing is queued for the Processes tab');

  // The one in the Processes tab moves to the overlay as it is.
  const mount = place.processEditorMount(harness.document);
  const docked = { mount, model: { dirty: true, template: { id: 'release-train' } }, destroyed: 0, destroy() { this.destroyed += 1; } };
  processes.held = { mount, editor: docked, label: 'Release train' };
  await harness.act(async () => { await openProcessDraft({ id: 'release-train', name: 'Release train' }); await wait(10); });
  assert.equal(place.floatingProcessEditor('release-train').editor, docked);
  assert.equal(processes.opened.length, 1, 'no second editor opens');

  // A clean one there is behind the version just saved on Groups: it goes, and
  // the overlay opens on that version.
  const cleanMount = place.processEditorMount(harness.document);
  const clean = { mount: cleanMount, model: { dirty: false, template: { id: 'review' } }, destroyed: 0, destroy() { this.destroyed += 1; } };
  processes.held = { mount: cleanMount, editor: clean, label: 'Code review' };
  const saved = { template: { id: 'review', name: 'Code review' }, currentRef: 'review@sha256:2', sourceHash: 's2' };
  await harness.act(async () => { await openProcessDraft({ id: 'review', name: 'Code review', view: saved }); await wait(10); });
  assert.equal(clean.destroyed, 1, 'the stale clean editor goes');
  const reviewSlot = harness.document.body.appendChild(harness.document.createElement('div'));
  await harness.act(async () => { await place.openFloatingEditor('review', 'Code review', reviewSlot); });
  assert.deepEqual(processes.opened.at(-1).seed, { view: saved, readable: true }, 'the overlay opens on the saved version');

  // Without overlay windows it goes to the Processes tab as before.
  instances.overlayHostUp.value = false;
  await harness.act(async () => { where = await openProcessDraft({ id: 'night-shift', name: 'Night shift', view }); await wait(); });
  assert.equal(where, 'processes');
  assert.deepEqual(takeProcessDraft(), { id: 'night-shift', name: 'Night shift', view });
  env.stop();
});

test('the dock\'s Edit opens an overlay over the tab the operator is on, but the Processes tab\'s own editor there', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t);
  const { harness, processTabs, instances } = env;
  const { openProcessDraft, takeProcessDraft } = await harness.importDashboardModule('js/process-draft-handoff.js');
  instances.overlayHostUp.value = true;
  t.after(() => { instances.overlayHostUp.value = false; });
  const groups = harness.document.querySelector('nav [data-tab="groups"]');
  const processesNav = harness.document.querySelector('nav [data-tab="processes"]');

  // From Groups (or any other tab) the editor opens over it.
  harness.fireEvent(groups, 'click');
  let where;
  await harness.act(async () => { where = await openProcessDraft({ id: 'triage', name: 'Triage' }); await wait(10); });
  assert.equal(where, 'overlay');
  assert.deepEqual(processTabs().map((tab) => tab.template), ['triage']);
  assert.ok(groups.classList.contains('active'), 'the operator stays where they are');

  // On the Processes tab it opens in that tab's editor, not floated out of it.
  harness.fireEvent(processesNav, 'click');
  await harness.act(async () => { where = await openProcessDraft({ id: 'review', name: 'Code review' }); await wait(); });
  assert.equal(where, 'processes');
  assert.deepEqual(takeProcessDraft(), { id: 'review', name: 'Code review' });
  assert.deepEqual(processTabs().map((tab) => tab.template), ['triage'], 'no overlay opens for it');
  env.stop();
});
