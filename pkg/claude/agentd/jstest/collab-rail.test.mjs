import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const wait = () => new Promise((resolve) => setTimeout(resolve, 0));

const SNAPSHOT = {
  groups: [{ id: 1, name: 'tclaude' }, { id: 2, name: 'home-lab' }, { id: 3, name: 'quiet' }],
  agents: [
    { agent_id: 'agt_x', conv_id: 'cx', title: 'launch-uniform', groups: ['home-lab', 'tclaude'], spawn_group: 'tclaude', online: true },
    { agent_id: 'agt_y', conv_id: 'cy', title: 'relaunch-table', groups: ['home-lab', 'tclaude'], online: true },
    { agent_id: 'agt_z', conv_id: 'cz', title: 'loner', groups: [], online: true },
  ],
  group_resources: [
    { kind: 'board', id: 'launch', group_id: 1, title: 'Launch-resolution map', state: 'closed' },
    { kind: 'page', id: 'pg_dash', group_id: 1, title: 'Dashboard preview', state: 'running' },
    { kind: 'process', id: 'pr-review', group_id: 1, title: 'pr-review', state: 'running', locked: true },
    { kind: 'shell', id: 'tclaude', group_id: 1, title: 'Group shell', state: 'running', session: 's' },
    { kind: 'board', id: 'release', group_id: 1, title: 'Release planning', state: 'closed' },
    { kind: 'board', id: 'net', group_id: 2, title: 'Network diagram', state: 'closed' },
    { kind: 'page', id: 'pg_status', group_id: 2, title: 'Status page', state: 'closed' },
    { kind: 'shell', id: 'home-lab', group_id: 2, title: 'Group shell', state: 'closed' },
    { kind: 'shell', id: 'quiet', group_id: 3, title: 'Group shell', state: 'closed' },
  ],
};

// statusOf stands in for terminalTabStatus: each terminal's agent, by title.
const statusOf = (item) => {
  const agent = SNAPSHOT.agents.find((a) => a.title === item.title);
  return agent ? { agent, key: 'idle', label: 'idle', symbol: '○', className: 'idle' } : null;
};

test('rail model: agents under their group first, live before closed, never-started group shells left out', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/collab-rail-model.js');
  const model = await harness.importDashboardModule('js/resource-model.js');
  const places = model.browserPlaces({
    strip: ['board:launch', 'agent:x'],
    overlays: [{ kind: 'page', page: 'pg_dash', minimized: true }, { kind: 'board', board: 'net', minimized: true }],
  });
  const all = m.railItems({
    resources: SNAPSHOT.group_resources, groups: SNAPSHOT.groups, places, statusOf,
    panes: [{ key: 'board:launch', seed: { board: 'launch' }, label: 'Launch' }, { key: 'agent:x', seed: { agent: 'x' }, label: 'launch-uniform' }],
    overlays: [{ id: 'term:y', kind: 'terminal', paneKey: 'agent:y', agent: 'agt_y', label: 'relaunch-table', minimized: false }],
    background: [{ kind: 'agent', key: 'agent:z', tmux_session: 'tc-z', label: 'loner' }, { kind: 'group_shell', key: 'groupterm:tclaude', tmux_session: 'g' },
      { kind: 'shell', key: 'agent:y', tmux_session: 'held', label: 'held by a window' },
      { kind: 'agent', tmux_session: 'old-gen', label: 'superseded' }],
  });
  assert.deepEqual(all.sections.map((s) => s.name), ['tclaude', 'home-lab', 'Not in a group'],
    'groups with something open here first; a group with only a closed shell is not there');
  const tclaude = all.sections[0].items.map((i) => i.key);
  assert.equal(tclaude[0], 'pane:agent:x', 'the agent spawned into tclaude is listed there, first');
  assert.ok(tclaude.includes('shell:tclaude'), 'a running group shell is listed');
  const flat = all.sections.flatMap((s) => s.items);
  assert.ok(!flat.some((i) => i.key === 'shell:home-lab' || i.key === 'shell:quiet'), 'a group shell nobody started is not');
  assert.equal(tclaude.at(-1), 'board:release', 'closed items come last');
  assert.equal(all.sections[1].items[0].key, 'overlay:term:y', 'an agent with no spawn group goes under its first group');
  assert.deepEqual(all.sections[2].items.map((i) => [i.key, i.bucket]), [['bg::tc-z', 'background']], 'an agent in no group is under Not in a group');
  assert.ok(!flat.some((i) => i.title === 'held by a window'), 'a session an overlay window holds is listed once');
  assert.ok(!flat.some((i) => i.title === 'superseded'), 'a session that cannot be attached is not offered');

  const active = m.filterRail(all);
  assert.equal(active.hidden, 2, 'the default leaves the two closed items out');
  assert.ok(!active.sections.flatMap((s) => s.items).some((i) => i.bucket === 'closed'), 'the default shows nothing closed');
  const closed = m.filterRail(all, { filter: { states: ['closed'], kinds: m.DEFAULT_FILTER.kinds } });
  assert.deepEqual(closed.sections.flatMap((s) => s.items).map((i) => i.key), ['board:release', 'page:pg_status']);
  const found = m.filterRail(all, { query: 'status' });
  assert.deepEqual(found.sections.flatMap((s) => s.items).map((i) => i.key), ['page:pg_status'], 'a search finds a closed item whatever the chip');
  assert.deepEqual(m.filterRail(all, { query: 'lab' }).sections.map((s) => s.name), ['home-lab'], 'search matches the group name too');

});

test('agents sort by what they are doing, then by name: needs you, working, idle', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/collab-rail-model.js');
  const states = { zed: 'idle', amy: 'working', bob: 'awaiting_permission', cal: 'idle', dan: 'working' };
  const agents = Object.keys(states).map((title) => ({ agent_id: title, title, groups: ['g'] }));
  const all = m.railItems({
    groups: [{ id: 1, name: 'g' }],
    background: Object.keys(states).map((title) => ({ kind: 'agent', key: `agent:${title}`, tmux_session: title, label: title })),
    statusOf: (item) => ({ agent: agents.find((a) => a.title === item.title), key: states[item.title], label: states[item.title] }),
    used: new Map([['bg::zed', 9]]),
  });
  assert.deepEqual(all.sections[0].items.map((i) => i.title), ['bob', 'amy', 'dan', 'cal', 'zed'], 'recent use does not move an agent');
  assert.ok(m.needsYou(all.sections[0].items[0]));
  assert.equal(m.itemState(all.sections[0].items[0]), 'needs you');
  assert.equal(m.itemState(all.sections[0].items[1]), '', 'the light says working; no word');
});

test('filter: place and kind for every item, what it is doing for an agent; a count of changes; a stored filter is read safely', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/collab-rail-model.js');
  const agent = (key, bucket) => ({ kind: 'terminal', bucket, status: { agent: {}, key } });
  const all = { states: m.FILTER_STATES.map(([k]) => k), kinds: m.FILTER_KINDS.map(([k]) => k) };
  assert.ok(m.matchesFilter(agent('working', 'background')));
  assert.ok(!m.matchesFilter(agent('working', 'background'), { ...all, states: all.states.filter((k) => k !== 'working') }),
    'unticking Working hides a working agent wherever it is');
  assert.ok(!m.matchesFilter(agent('idle', 'background'), { ...all, states: all.states.filter((k) => k !== 'background') }),
    'unticking Background hides what runs out of sight');
  assert.ok(m.matchesFilter(agent('awaiting_input', 'minimized'), { ...all, states: ['needs_you', 'open'] }), 'a docked window counts as open');
  assert.ok(!m.matchesFilter({ kind: 'board', bucket: 'closed' }), 'the default leaves closed items out');
  assert.ok(!m.matchesFilter({ kind: 'shell', bucket: 'background' }, { ...all, kinds: ['agents'] }));
  assert.ok(m.matchesFilter({ kind: 'terminal', bucket: 'open' }, { ...all, kinds: ['shells'] }), 'a terminal with no agent is a shell');
  assert.equal(m.filterChanges(m.DEFAULT_FILTER), 0);
  assert.equal(m.filterChanges(all), 1);
  assert.deepEqual(m.readFilter('not json'), m.DEFAULT_FILTER);
  assert.deepEqual(m.readFilter('closed'), m.DEFAULT_FILTER, 'an old chip value reads as the default');
  assert.deepEqual(m.readFilter(JSON.stringify({ states: ['closed', 'bogus'], kinds: ['pages'] })), { states: ['closed'], kinds: ['pages'] });
});

test('the home: nothing open, a button to the panel, and the agents that need you', async (t) => {
  const harness = await createPreactHarness(t);
  const doc = harness.document;
  doc.body.appendChild(doc.createElement('section')).id = 'tab-terminals';
  doc.getElementById('tab-terminals').classList.add('active');
  const shell = doc.body.appendChild(doc.createElement('aside'));
  shell.id = 'agent-dock';
  shell.appendChild(doc.createElement('div')).id = 'dock-collab-body';
  const [home, rail, side, places, bg] = await Promise.all([
    harness.importDashboardModule('js/collab-home.js'),
    harness.importDashboardModule('js/collab-rail.js'),
    harness.importDashboardModule('js/side-panel.js'),
    harness.importDashboardModule('js/resource-places.js'),
    harness.importDashboardModule('js/background-terminals.js'),
  ]);
  const waiting = {
    ...SNAPSHOT,
    agents: SNAPSHOT.agents.map((a) => (a.title === 'relaunch-table' ? { ...a, state: { status: 'awaiting_permission' } } : { ...a, state: { status: 'working' } })),
  };
  await harness.act(() => {
    places.stripPaneKeys.value = [];
    bg.backgroundTerminals.value = [
      { kind: 'agent', key: 'agent:agt_y', agent_id: 'agt_y', tmux_session: 'tc-y', label: 'relaunch-table' },
      { kind: 'agent', key: 'agent:agt_x', agent_id: 'agt_x', tmux_session: 'tc-x', label: 'launch-uniform' },
    ];
  });
  const { container } = await harness.mount(harness.html`<div>
    <${rail.CollabRail} panes=${[]} actions=${{}} snapshot=${{ value: waiting }} />
    <${home.CollabHome} panes=${[]} actions=${{}} snapshot=${{ value: waiting }} />
  </div>`);
  const root = container.querySelector('[data-collab-home]');
  assert.equal(root.querySelector('.ch-title').textContent, 'Nothing open');
  const open = root.querySelector('[data-home-open-panel]');
  assert.ok(open, 'the folded panel is offered');
  await harness.act(() => harness.fireEvent(open, 'click'));
  assert.equal(side.isSidePanelOpen('collab'), true);
  assert.equal(container.querySelector('[data-home-open-panel]'), null, 'an open panel needs no button');
  assert.deepEqual([...root.querySelectorAll('[data-home-agent]')].map((el) => el.dataset.homeAgent), ['bg::tc-y'],
    'only the agent waiting on approval is listed');
  assert.equal(root.querySelector('.ch-quiet'), null);

  // A click focuses the agent: its background session attaches as a tab, as a
  // click on it in the panel does (openRailItem).
  const used = await harness.importDashboardModule('js/collab-rail-actions.js');
  await harness.act(async () => { harness.fireEvent(root.querySelector('[data-home-agent="bg::tc-y"]'), 'click'); await wait(); });
  assert.ok(used.railUsed.value?.has('bg::tc-y'), 'the click opened that agent\'s item');

  await harness.act(() => { bg.backgroundTerminals.value = [bg.backgroundTerminals.value[1]]; });
  assert.equal(container.querySelector('[data-collab-home] .ch-quiet').textContent, 'No agents need you.');
});

test('rail: the Filter lists all but closed by default, ticks pick any mix, Reset goes back, a group header folds', async (t) => {
  const harness = await createPreactHarness(t);
  const [rail, places] = await Promise.all([
    harness.importDashboardModule('js/collab-rail.js'),
    harness.importDashboardModule('js/resource-places.js'),
  ]);
  await harness.act(() => { places.stripPaneKeys.value = ['board:launch']; });
  const activated = [];
  const actions = { activatePane: (key) => activated.push(key), closePane: () => {}, popOutPane: () => {} };
  const { container } = await harness.mount(harness.html`<${rail.CollabRail}
    panes=${[{ key: 'board:launch', seed: { board: 'launch' }, label: 'Launch' }]} actions=${actions} snapshot=${{ value: SNAPSHOT }} />`);
  const root = container.querySelector('[data-collab-rail]');
  const items = () => [...root.querySelectorAll('[data-rail-item]')].map((el) => el.dataset.railItem);
  assert.deepEqual([...root.querySelectorAll('[data-rail-group]')].map((el) => el.dataset.railGroup), ['tclaude']);
  assert.deepEqual(items(), ['board:launch', 'page:pg_dash', 'process:pr-review', 'shell:tclaude']);
  assert.equal(root.querySelector('#collab-filter-badge').hidden, true, 'the default filter shows no count');
  assert.equal(root.querySelector('[data-rail-item="page:pg_dash"] .cr-state').textContent, 'live', 'a page out of sight is live');
  assert.equal(root.querySelector('[data-rail-item="board:launch"] .cr-state'), null, 'an item open here needs no words');
  assert.ok(root.querySelector('[data-rail-item="process:pr-review"] [data-lock-badge="process"]'), 'a locked process wears the lock icon');
  assert.equal(root.querySelector('[data-rail-item="board:launch"] [data-lock-badge]'), null, 'an unlocked one does not');

  // A strip tab comes forward on click, and offers pop out and close.
  const launch = root.querySelector('[data-rail-item="board:launch"]');
  assert.equal(launch.querySelectorAll('.cr-tool').length, 2);
  await harness.act(async () => { harness.fireEvent(launch.querySelector('.cr-name'), 'click'); await wait(); });
  assert.deepEqual(activated, ['board:launch']);

  await harness.act(() => harness.fireEvent(root.querySelector('#collab-filter-btn'), 'click'));
  assert.ok(root.querySelector('#collab-filter-menu').classList.contains('open'));
  const tick = async (key, on) => {
    const box = root.querySelector(`#collab-filter-${key}`);
    box.checked = on;
    await harness.act(() => harness.fireEvent(box, 'change'));
  };
  await tick('closed', true);
  for (const key of ['open', 'background']) await tick(key, false);
  assert.equal(root.querySelector('#collab-filter-badge').textContent, '3', 'three boxes differ from the default');
  assert.deepEqual(items(), ['board:release', 'board:net', 'page:pg_status']);
  const search = root.querySelector('.cr-search');
  await harness.act(() => { search.value = 'preview'; harness.fireEvent(search, 'input'); });
  assert.deepEqual(items(), ['page:pg_dash'], 'a search finds a live item while Closed is picked');
  await harness.act(() => { search.value = ''; harness.fireEvent(search, 'input'); });

  await harness.act(() => harness.fireEvent(root.querySelector('[data-collab-filter-reset]'), 'click'));
  assert.equal(root.querySelector('#collab-filter-badge').hidden, true, 'Reset goes back to the default');
  assert.deepEqual(items(), ['board:launch', 'page:pg_dash', 'process:pr-review', 'shell:tclaude']);
  await harness.act(() => harness.fireEvent(root.querySelector('[data-rail-group="tclaude"] .cr-section-head'), 'click'));
  assert.deepEqual(items(), [], 'a folded group shows only its header');
  assert.equal(root.querySelector('[data-rail-group="tclaude"] .cr-count').textContent, '4');
});

test('side panel: each tab has its own panel and state; Collab is folded by default and offered only while mounted', async (t) => {
  const harness = await createPreactHarness(t);
  const doc = harness.document;
  for (const id of ['tab-groups', 'tab-terminals']) doc.body.appendChild(doc.createElement('section')).id = id;
  const shell = doc.body.appendChild(doc.createElement('aside'));
  shell.id = 'agent-dock';
  shell.appendChild(doc.createElement('button')).id = 'dock-toggle';
  shell.appendChild(doc.createElement('div')).id = 'dock-body';
  shell.appendChild(doc.createElement('div')).id = 'dock-collab-body';
  const side = await harness.importDashboardModule('js/side-panel.js');
  side.registerSidePanel('groups', { pane: 'tab-groups', pref: 'test.groups.open', open: true, body: '#dock-body', titles: [['Hide', 'Hide'], ['Show', 'Show']] });
  side.registerSidePanel('collab', { pane: 'tab-terminals', pref: 'test.collab.open', open: false, body: '#dock-collab-body', needsOffer: true,
    titles: [['Hide C', 'Hide C'], ['Show C', 'Show C']] });
  side.bindSidePanel(doc);
  const body = doc.body;

  doc.getElementById('tab-groups').classList.add('active');
  side.applySidePanel(doc);
  assert.ok(body.classList.contains('dock-tab') && body.classList.contains('dock-open'), 'the Groups palette opens by default');
  assert.equal(body.dataset.dockPanel, 'groups');
  assert.equal(doc.getElementById('dock-collab-body').hidden, true);

  doc.getElementById('tab-groups').classList.remove('active');
  doc.getElementById('tab-terminals').classList.add('active');
  side.applySidePanel(doc);
  assert.ok(!body.classList.contains('dock-tab'), 'Collab offers no panel until its view is mounted');
  side.offerSidePanel('collab', true);
  assert.ok(body.classList.contains('dock-tab') && !body.classList.contains('dock-open'), 'the Collab panel is folded by default');
  assert.equal(doc.getElementById('dock-toggle').title, 'Show C');
  harness.fireEvent(doc.getElementById('dock-toggle'), 'click');
  assert.ok(body.classList.contains('dock-open'));
  assert.equal(doc.getElementById('dock-body').hidden, true, 'the Collab body shows, not the palette');
  assert.equal(doc.getElementById('dock-collab-body').hidden, false);
  harness.fireEvent(doc.getElementById('dock-toggle'), 'click');
  assert.equal(side.isSidePanelOpen('collab'), false);
  assert.equal(side.isSidePanelOpen('groups'), true, 'each panel keeps its own state');
});

test('the Collab panel renders into the shared side panel while mounted, and the whole panel takes a drop as the rail', async (t) => {
  const harness = await createPreactHarness(t);
  const doc = harness.document;
  doc.body.appendChild(doc.createElement('section')).id = 'tab-terminals';
  doc.getElementById('tab-terminals').classList.add('active');
  const shell = doc.body.appendChild(doc.createElement('aside'));
  shell.id = 'agent-dock';
  shell.appendChild(doc.createElement('button')).id = 'dock-toggle';
  const host = shell.appendChild(doc.createElement('div'));
  host.id = 'dock-collab-body';
  const [rail, actions] = await Promise.all([
    harness.importDashboardModule('js/collab-rail.js'),
    harness.importDashboardModule('js/collab-rail-actions.js'),
  ]);
  const mounted = await harness.mount(harness.html`<${rail.CollabRail} panes=${[]} actions=${{}} snapshot=${{ value: SNAPSHOT }} />`);
  assert.equal(mounted.container.querySelector('[data-collab-rail]'), null, 'nothing renders in place');
  assert.ok(host.querySelector('[data-collab-rail]'), 'the list renders into the panel body');
  assert.ok(doc.body.classList.contains('dock-tab'), 'the Collab tab offers its panel');
  assert.ok(!doc.body.classList.contains('dock-open'), 'folded by default');
  assert.equal(actions.railDropZone(host), 'rail', 'the panel body around the list is the rail\'s');
  assert.equal(actions.railDropZone(doc.getElementById('dock-toggle')), 'rail', 'so is the edge handle');
  await mounted.unmount();
  assert.ok(!doc.body.classList.contains('dock-tab'), 'unmounted, the panel is withdrawn');
  assert.equal(host.childNodes.length, 0, 'and its body is emptied');
});

test('opening a live item does not reorder the panel', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/collab-rail-model.js');
  const resources = [
    { kind: 'page', id: 'a', group_id: 1, title: 'A', state: 'running' },
    { kind: 'page', id: 'b', group_id: 2, title: 'B', state: 'running' },
  ];
  const groups = [{ id: 1, name: 'one' }, { id: 2, name: 'two' }];
  const before = m.railItems({ resources, groups }).sections.map((s) => s.name);
  const after = m.railItems({ resources, groups, used: new Map([['page:b', 99]]) }).sections.map((s) => s.name);
  assert.deepEqual(after, before);
});
