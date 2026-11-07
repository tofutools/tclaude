// The tab strip's ＋ menu and its background counter moved into the Collab
// panel (tcl-ievs1w). Every action they offered keeps a home there, and
// each is pinned here from that home:
//
//   strip ＋ → Drawing boards: open, create, import, export, delete    → ＋ New › Drawing board… (the same drawing board list), Import drawing board…, a drawing board row's menu
//   strip ＋ → Processes: open, create                          → ＋ New › Process… (the same process list)
//   counter: count                                              → the Filter button's badge, Background (N), the folded panel's handle
//   counter: attach / open a row (title or button)              → a click on the row, or its menu's Attach / Open
//   counter: attach-locked row shown disabled with the reason   → its menu's Attach (locked), titled with the reason
//   counter: unattachable row with its tmux hint                → Other sessions, the hint in the row's title
//   counter: kill a session                                     → the row menu's Kill session… (asks first)
//   counter: end a drawing board's sessions                             → the drawing board row menu's End agents' sessions… (asks first)
//   counter: attach all                                         → Filter › Attach all background (N)
//
// Moving the background rows themselves (attach, peers, a drawing board row) is pinned
// in background-terminals.test.mjs; the bare /drawing-boards and /processes routes in
// drawing-board-tabs.test.mjs and process-tabs.test.mjs.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const wait = () => new Promise((resolve) => setTimeout(resolve, 0));

const GROUPS = [{ id: 1, name: 'tclaude', default_cwd: '/src/tclaude' }, { id: 2, name: 'notes' }];
const RESOURCES = [
  { kind: 'drawing_board', id: 'plan', group_id: 1, title: 'Plan', state: 'closed' },
  { kind: 'shell', id: 'tclaude', group_id: 1, title: 'Group shell', state: 'running', session: 's' },
];

// fakeContext records every seam a row menu or the ＋ New menu calls.
function fakeContext() {
  const calls = [];
  const record = (name) => (...args) => { calls.push([name, ...args]); return Promise.resolve(''); };
  return {
    calls,
    open: record('open'), kill: record('kill'), exportDrawingBoard: record('exportDrawingBoard'), askDeleteDrawingBoard: record('askDeleteDrawingBoard'),
    notify: record('notify'), fetchImpl: record('fetch'),
    // As shellConfirm: it resolves to what the action returns.
    confirm: async (options) => { calls.push(['confirm', options.title]); return options.action(); },
    spawn: record('spawn'), shell: record('shell'), drawingBoard: record('drawing_board'), page: record('page'),
    openDrawingBoard: record('openDrawingBoard'), openProcess: record('openProcess'), drawingBoardIds: async () => [],
  };
}

const ids = ({ open, end }) => [...open, ...end].map((action) => action.id);

test('a row menu offers what the counter did: attach, attach (locked), kill, and end a drawing board\'s sessions', async (t) => {
  const harness = await createPreactHarness(t);
  const { railItems, filterRail } = await harness.importDashboardModule('js/collab-rail-model.js');
  const { railRowActions, hasRowActions } = await harness.importDashboardModule('js/collab-rail-menu.js');
  const agent = { kind: 'agent', key: 'window:agt_a', tmux_session: 'tc-a', label: 'worker', killable: true };
  const locked = { kind: 'shell', tmux_session: 'tc-l', label: 'old shell', killable: true, attachLock: 'This shell predates group shells' };
  const drawingBoardRow = { kind: 'drawing_board', key: 'drawing_board:plan', drawingBoard: 'plan', label: 'Plan', killable: true };
  const shellRow = { kind: 'group_shell', key: 'groupterm:tclaude', tmux_session: 'tclaude-group-tclaude', group: 'tclaude', killable: true };
  const { sections } = railItems({ resources: RESOURCES, groups: GROUPS, background: [agent, locked, drawingBoardRow, shellRow], titleOf: (row) => row.label || row.group });
  const items = new Map(sections.flatMap((section) => section.items).map((item) => [item.key, item]));

  const ctx = fakeContext();
  const background = items.get('bg::tc-a');
  assert.deepEqual(ids(railRowActions(background)), ['open', 'kill']);
  const attach = railRowActions(background).open[0];
  assert.equal(attach.label, 'Attach');
  await attach.run(ctx);
  await railRowActions(background).end[0].run(ctx);
  assert.deepEqual(ctx.calls.splice(0), [['open', background], ['confirm', 'Kill this session?'], ['kill', agent], ['notify', 'Killed worker']],
    'Kill asks first, then ends the tmux session');

  const lockedItem = sections.find((section) => section.name === 'Other sessions').items[0];
  assert.equal(lockedItem.row, locked, 'a locked session lists under Other sessions');
  const [lockedAttach] = railRowActions(lockedItem).open;
  assert.deepEqual([lockedAttach.label, lockedAttach.disabled, lockedAttach.title], ['Attach (locked)', true, 'This shell predates group shells']);

  const drawingBoard = items.get('drawing_board:plan');
  assert.equal(drawingBoard.sessions, drawingBoardRow, "a drawing board's sessions ride on the drawing board's own item");
  assert.equal(drawingBoard.bucket, 'background', 'the snapshot calls the drawing board closed, but its sessions put it in the background');
  const listed = filterRail({ sections }).sections.flatMap((section) => section.items.map((item) => item.key));
  assert.ok(listed.includes('drawing_board:plan'), 'so the default filter lists it, and its End is reachable');
  assert.deepEqual(ids(railRowActions(drawingBoard)), ['open', 'download', 'end-sessions', 'delete-drawing-board']);
  for (const action of [...railRowActions(drawingBoard).open, ...railRowActions(drawingBoard).end]) await action.run(ctx);
  assert.deepEqual(ctx.calls.splice(0), [
    ['open', drawingBoard],
    ['exportDrawingBoard', 'plan', ctx.fetchImpl],
    ['confirm', 'End the agents’ sessions on this drawing board?'], ['kill', drawingBoardRow], ['notify', 'Ended the sessions on Plan'],
    ['askDeleteDrawingBoard', { id: 'plan', title: 'Plan' }],
  ], 'download, end the sessions (asking first) and delete, the drawing board list\'s ⤓ and 🗑 and the counter\'s End');

  const shell = items.get('shell:tclaude');
  assert.equal(shell.shellSession, shellRow, "a group shell's session rides on its group's shell item");
  assert.deepEqual(ids(railRowActions(shell)), ['open', 'kill']);
  await railRowActions(shell).end[0].run(ctx);
  assert.deepEqual(ctx.calls.splice(0), [['confirm', 'Kill this shell?'], ['kill', shellRow], ['notify', 'Killed tclaude shell']]);

  const quiet = { kind: 'agent', tmux_session: 'old', label: 'old gen', superseded: true };
  const [quietItem] = railItems({ background: [quiet], titleOf: (row) => row.label }).sections[0].items;
  assert.equal(hasRowActions(quietItem), false, 'a session with nothing to offer has no menu to open');

  const unlisted = railItems({ background: [{ kind: 'group_shell', key: 'groupterm:gone', tmux_session: 'g', group: 'gone', killable: true }], titleOf: (row) => row.group });
  assert.deepEqual(unlisted.sections.map((section) => [section.name, section.items.map((item) => item.key)]), [['Not in a group', ['bg::g']]],
    'a group shell whose group no longer lists it still shows, and attaches');
});

test('＋ New lists every kind there is a create path for, and a group\'s ＋ makes them in that group', async (t) => {
  const harness = await createPreactHarness(t);
  const { newMenuEntries } = await harness.importDashboardModule('js/collab-new-menu.js');
  const header = newMenuEntries({ groups: GROUPS, processes: true });
  assert.deepEqual(header.map((e) => [e.id, e.label, e.view || '', !!e.disabled]), [
    ['agent', 'Agent…', '', false], ['shell', 'Shell…', 'shell', false], ['drawing_board', 'Drawing board…', 'drawing-boards', false],
    ['process', 'Process…', 'processes', false], ['page', 'Page…', 'page', false], ['import', 'Import drawing board…', '', false],
  ]);
  assert.deepEqual(newMenuEntries({ groups: [GROUPS[1]] }).map((e) => [e.id, !!e.disabled]),
    [['agent', false], ['shell', true], ['drawing_board', false], ['page', false], ['import', false]],
    'without Processes there is no Process…; with no group directory there is no shell to open');
  assert.equal(newMenuEntries({ processes: true, wizard: true }).find((e) => e.id === 'process').label, 'Rite…');

  const group = GROUPS[0];
  const ctx = fakeContext();
  const preset = newMenuEntries({ group, groups: GROUPS, processes: true });
  assert.deepEqual(preset.map((e) => [e.id, e.label, e.view || '']), [
    ['agent', 'Agent…', ''], ['shell', 'Shell', ''], ['drawing_board', 'Drawing board', ''], ['process', 'Process…', 'processes'], ['page', 'Page…', ''], ['import', 'Import drawing board…', ''],
  ]);
  for (const entry of preset.filter((e) => e.run)) await entry.run({ ...ctx, pickImport: () => ctx.calls.push(['pickImport']) });
  assert.deepEqual(ctx.calls, [['spawn', { groupName: 'tclaude' }], ['shell', group], ['drawing_board', group], ['page', group], ['pickImport']]);
  assert.equal(newMenuEntries({ group: GROUPS[1] }).find((e) => e.id === 'shell').disabled, true);
});

async function mountRail(t, { background = [], groups = GROUPS } = {}) {
  const harness = await createPreactHarness(t);
  const { document } = harness;
  const handle = document.body.appendChild(document.createElement('button'));
  handle.id = 'dock-toggle';
  handle.appendChild(document.createElement('span')).id = 'dock-toggle-count';
  const host = document.body.appendChild(document.createElement('div'));
  const [{ CollabRail }, bg] = await Promise.all([
    harness.importDashboardModule('js/collab-rail.js'),
    harness.importDashboardModule('js/background-terminals.js'),
  ]);
  bg.backgroundTerminals.value = background;
  const snapshot = harness.signals.signal({ groups, group_resources: RESOURCES, agents: [] });
  const newContext = fakeContext();
  const actions = { activatePane: () => true };
  const mounted = await harness.mount(harness.html`<${CollabRail} panes=${[]} actions=${actions} snapshot=${snapshot} notify=${() => {}} newContext=${newContext} />`, host);
  t.after(() => mounted.unmount());
  return { harness, host, handle, bg, newContext };
}

test('the panel header\'s ＋ New opens the spawn dialog, and Shell… asks which group', async (t) => {
  const { harness, host, newContext } = await mountRail(t);
  const header = host.querySelector('.cr-head');
  assert.ok(header.querySelector('.cr-search') && header.querySelector('#collab-filter-btn') && header.querySelector('.cr-new'),
    'Find…, Filter and ＋ New share the header');
  await harness.act(() => header.querySelector('.cr-new').click());
  await harness.act(() => harness.fireEvent(host.querySelector('[data-new-kind="agent"]'), 'click'));
  assert.deepEqual(newContext.calls.splice(0), [['spawn', {}]]);
  assert.equal(host.querySelector('.cr-new-menu'), null, 'picking closes the menu');

  await harness.act(() => header.querySelector('.cr-new').click());
  await harness.act(() => harness.fireEvent(host.querySelector('[data-new-kind="shell"]'), 'click'));
  assert.deepEqual([...host.querySelectorAll('[data-new-group]')].map((node) => node.dataset.newGroup), ['tclaude'],
    'only groups with a directory have a shell to open');
  await harness.act(() => harness.fireEvent(host.querySelector('[data-new-group="tclaude"]'), 'click'));
  assert.deepEqual(newContext.calls.splice(0), [['shell', GROUPS[0]]]);

  await harness.act(() => header.querySelector('.cr-new').click());
  await harness.act(() => harness.fireEvent(host.querySelector('[data-new-kind="page"]'), 'click'));
  await harness.act(() => harness.fireEvent(host.querySelector('[data-new-back]'), 'click'));
  assert.ok(host.querySelector('[data-new-kind="agent"]'), '‹ Back returns to the kinds');
});

test('a group header\'s ＋ opens the same menu, made for that group', async (t) => {
  const { harness, host, newContext } = await mountRail(t);
  const add = host.querySelector('[data-rail-group="tclaude"] [data-new-menu="tclaude"]');
  assert.ok(add, 'the group header carries a ＋');
  await harness.act(() => add.click());
  assert.equal(host.querySelector('.cr-new-menu').getAttribute('aria-label'), 'New in tclaude');
  await harness.act(() => harness.fireEvent(host.querySelector('[data-new-kind="drawing_board"]'), 'click'));
  assert.deepEqual(newContext.calls.splice(0), [['drawing_board', GROUPS[0]]]);
  await harness.act(() => add.click());
  await harness.act(() => harness.fireEvent(host.querySelector('[data-new-kind="agent"]'), 'click'));
  assert.deepEqual(newContext.calls.splice(0), [['spawn', { groupName: 'tclaude' }]]);
});

test('the Filter counts the background and attaches all of it; the folded panel\'s handle carries the count', async (t) => {
  const rows = [
    { kind: 'agent', key: 'window:agt_a', tmux_session: 'tc-a', label: 'worker' },
    { kind: 'agent', key: 'window:agt_b', tmux_session: 'tc-b', label: 'helper' },
    { kind: 'other', tmux_session: 'stray', label: 'stray', killable: true },
  ];
  const { harness, host, handle, bg } = await mountRail(t, { background: rows });
  const button = host.querySelector('#collab-filter-btn');
  assert.equal(button.querySelector('[data-collab-bg-count]').textContent, '3');
  assert.equal(handle.querySelector('[data-dock-bg-count]').textContent, '3', 'the show-panel handle counts them too');
  await harness.act(() => button.click());
  const menu = host.querySelector('#collab-filter-menu');
  assert.match(menu.textContent, /Background \(3\)/, 'the Background box counts them');
  const attachAll = menu.querySelector('[data-collab-attach-all]');
  assert.equal(attachAll.textContent, 'Attach all background (2)', 'what can be attached, at the bottom, set apart from the boxes');
  assert.ok(attachAll.previousElementSibling?.matches('.view-menu-sep, [role="separator"], hr'), 'after a divider');

  await harness.act(async () => { bg.backgroundTerminals.value = [rows[2]]; await wait(); });
  assert.equal(host.querySelector('[data-collab-attach-all]'), null, 'hidden with nothing to attach');
  assert.equal(handle.querySelector('[data-dock-bg-count]').textContent, '1');
  await harness.act(async () => { bg.backgroundTerminals.value = []; await wait(); });
  assert.equal(button.querySelector('[data-collab-bg-count]'), null);
  assert.equal(handle.querySelector('[data-dock-bg-count]'), null, 'no badge with nothing in the background');
});

test('＋ New opens on its kinds again after another ＋ took over mid-step', async (t) => {
  const { harness, host } = await mountRail(t);
  await harness.act(() => host.querySelector('.cr-head .cr-new').click());
  await harness.act(() => harness.fireEvent(host.querySelector('[data-new-kind="shell"]'), 'click'));
  assert.ok(host.querySelector('[data-new-group]'), 'on the which-group step');
  await harness.act(() => host.querySelector('[data-new-menu="tclaude"]').click());
  assert.equal(host.querySelector('.cr-new-menu').getAttribute('aria-label'), 'New in tclaude', 'the group ＋ replaces it');
  await harness.act(() => host.querySelector('[data-new-menu="tclaude"]').click());
  await harness.act(() => host.querySelector('.cr-head .cr-new').click());
  assert.ok(host.querySelector('[data-new-kind="agent"]'), 'the header ＋ New opens on its kinds, not the old step');
});
