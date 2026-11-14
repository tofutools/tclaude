import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();

function row(id, title, extra = {}) {
  return { id, group: 'crew', title, stage: 'next', status: 'open', rank: id, depends_on: [], created_by: 'human:operator', created_at: ago(5), updated_at: ago(5), ...extra };
}

function board(group, extra = {}) {
  return {
    group_id: 1, group,
    counts: { live: 5, needs_you: 1, waiting_on_you: 1, in_progress: 1, next: 2, ideas: 1, blocked: 1, queued: 1, done_this_week: 3, dropped: 1 },
    tasks: [
      row('tk_00000001', 'Steward input row', { ref: { kind: 'github', url: 'https://github.com/tofutools/tclaude/pull/3432', label: '#3432' },
        assignee: { agent_id: 'agt_builder' }, assigned_by: 'human:operator', assigned_at: ago(41) }),
      row('tk_00000002', 'Engine: dynamic nodes', { ref: { kind: 'awb', label: 'tcl-n7x41z' } }),
      row('tk_00000003', 'Wait-for-work node', { depends_on: ['tk_00000002'], unmet: ['tk_00000002'], queued: true }),
      row('tk_00000004', 'Lighthouse regressions', { status: 'blocked', block_reason: 'wait for the CSS move' }),
      row('tk_00000005', 'Remote claim via peer grants', { stage: 'idea' }),
    ],
    closed: [row('tk_00000006', 'Docs first draft', { status: 'done', closed_at: ago(60) })],
    ...extra,
  };
}

function fetchWith(body, status = 200) {
  const calls = [];
  const fetchImpl = async (path) => {
    calls.push(path);
    return { ok: status === 200, status, json: async () => body };
  };
  return { calls, fetchImpl };
}

async function mountSection(harness, { group, context, status = 200 }) {
  const [{ TaskboardSection }, { createTaskboardContextSource }] = await Promise.all([
    harness.importDashboardModule('js/groups-taskboard.js'),
    harness.importDashboardModule('js/taskboard-context.js'),
  ]);
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  // These cover the List; Lanes is the default view.
  if (dashPrefs.getItem(`tclaude.dash.groups.taskboard.view.${group.name}`) === null) dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${group.name}`, 'list');
  const { calls, fetchImpl } = fetchWith(context, status);
  const source = createTaskboardContextSource({ fetchImpl });
  const snapshot = { groups: [group] };
  const mounted = await harness.mount(harness.html`<div>
    <${TaskboardSection} group=${group} snapshot=${snapshot} source=${source} />
  </div>`);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  return { harness, container: mounted.container, calls, source };
}

const crew = (name) => ({ name, members: [{ agent_id: 'agt_builder', title: 'builder' }] });

test('a board that needs the operator opens, lists the queue then the ideas, and folds to one line', async (t) => {
  const group = crew('tb-open');
  const harness = await createPreactHarness(t);
  const { container, calls } = await mountSection(harness, { group, context: { groups: [board('tb-open')] } });
  assert.deepEqual(calls, ['/v1/taskboard/context']);
  const section = container.querySelector('[data-fused-taskboard="tb-open"]');
  assert.ok(section, 'the section is drawn');
  assert.match(section.querySelector('.gf-label').textContent, /Task board/);
  assert.equal(section.querySelector('.tb-counts .gf-count').textContent, '5', 'the head shows the total');
  assert.equal(section.querySelector('.tb-waiting-tag').textContent, '1 waiting on you', 'the head shows the total and what waits on the operator');
  assert.match(section.querySelector('.tb-counts').getAttribute('title'), /1 waiting on you · 1 blocked/, 'the breakdown is in its tooltip');
  assert.equal(section.querySelector('.gf-section-actions'), null, 'open, the title line carries only the label and the total');
  const toolbar = [...section.querySelector('.gf-title-row').querySelectorAll('[data-taskboard-new], .tb-view')];
  assert.ok(toolbar.length >= 2 && toolbar[0].matches('[data-taskboard-new]'), 'the toolbar row: the add button, then the view switch');

  // Auto-open: something waits on the operator.
  const titles = [...section.querySelectorAll('tr.tb-row .tb-title')].map((n) => n.textContent);
  assert.deepEqual(titles, ['Steward input row', 'Engine: dynamic nodes', 'Wait-for-work node', 'Lighthouse regressions', 'Remote claim via peer grants']);
  const rows = [...section.querySelectorAll('tr.tb-row')];
  assert.ok(section.querySelector('tr.tb-ideas-divider').textContent.includes('Ideas · 1'));
  // Ref chips: a URL links out, a bare tracker id does not.
  const link = rows[0].querySelector('a.tb-ref');
  assert.equal(link.getAttribute('href'), 'https://github.com/tofutools/tclaude/pull/3432');
  assert.equal(link.getAttribute('rel'), 'noopener noreferrer');
  assert.equal(rows[1].querySelector('a.tb-ref'), null);
  assert.match(rows[1].querySelector('.tb-ref').textContent, /tcl-n7x41z/);
  // Who and state.
  assert.equal(rows[0].querySelector('.tb-who-name').textContent, 'builder');
  assert.match(rows[0].querySelector('.tb-who').getAttribute('title'), /^builder · assigned .* by operator$/, 'who assigned it is the tooltip');
  // The pill is the bare state; its tooltip has the rest, as the Agents
  // table's state pill does, and never a time (Last and Age have those).
  const tip = (row) => row.querySelector('.tb-state-wrap').getAttribute('title');
  assert.equal(rows[0].querySelector('.tb-state').textContent, 'in progress');
  assert.equal(tip(rows[0]), 'in progress · with builder');
  assert.equal(rows[0].querySelector('.tb-state-wrap').getAttribute('tabindex'), '0', 'the tooltip is reachable by keyboard');
  assert.ok(rows[0].querySelector('.tb-state-wrap').classList.contains('state-pill-wrap'), 'and shown on focus, as the Agents pill\'s is');
  assert.equal(rows[0].querySelector('.tb-state-wrap').dataset.fullStatus, tip(rows[0]));
  assert.match(rows[0].querySelector('.tb-state-wrap').getAttribute('aria-label'), /^state: in progress · with builder/);
  assert.equal(rows[1].querySelector('.tb-unassigned').textContent, 'unassigned');
  assert.ok(rows[1].querySelector('.tb-who-mark.tb-marker-unassigned'), 'the List has no Unassigned lane: the yellow ! says it');
  assert.equal(rows[0].querySelector('.tb-who-mark'), null, 'an agent\'s task: its avatar, no !');
  assert.equal(rows[1].querySelector('.tb-state').textContent, 'open');
  // Rows carry no number: a dependency is named by its title (clipped), and
  // its whole title and id are the tooltip.
  assert.equal(rows[2].querySelector('.tb-dep.unmet').textContent, 'after Engine: dynamic nodes');
  assert.equal(rows[2].querySelector('.tb-dep.unmet').getAttribute('title'), 'Waits on “Engine: dynamic nodes” (tk_00000002)');
  assert.equal(rows[2].querySelector('.tb-state').textContent, 'queued');
  assert.equal(tip(rows[2]), 'queued · waits on “Engine: dynamic nodes”');
  assert.equal(rows[3].querySelector('.tb-state').textContent, 'blocked');
  assert.equal(tip(rows[3]), 'blocked · “wait for the CSS move”');
  assert.match(section.querySelector('.tb-reason').textContent, /wait for the CSS move/);
  assert.equal(rows[4].querySelector('.tb-state').textContent, 'idea');
  assert.equal(tip(rows[4]), 'idea · captured, not yet in the queue');
  assert.equal(tip(rows[1]), 'open · next in the queue', 'nothing more to say: what the state means');
  for (const row of rows) assert.doesNotMatch(tip(row), /\bago\b|\d+[smhd]\b/, 'no time in a state\'s tooltip');

  // History: the counts, and the closed rows on demand.
  assert.match(section.querySelector('.tb-history').textContent, /3 done this week · 1 dropped/);
  assert.equal(section.querySelector('.tb-closed'), null);
  const toggle = section.querySelector('.tb-history-toggle');
  await harness.act(() => harness.fireEvent(toggle, 'click'));
  assert.deepEqual([...section.querySelectorAll('.tb-closed .tb-title')].map((n) => n.textContent), ['Docs first draft']);

  // Fold: one line, remembered.
  await harness.act(() => harness.fireEvent(section.querySelector('.gf-section-toggle'), 'click'));
  assert.equal(section.querySelector('.tb-table'), null, 'folded, the rows are gone');
  assert.ok(section.querySelector('.tb-counts'), 'the counts stay on the line');
  assert.equal(section.querySelector('.gf-title-row'), null, 'folded, no toolbar row');
  assert.ok(section.querySelector('.gf-section-actions [data-taskboard-new]'), 'folded, the add button rides the section line');
  assert.equal(section.querySelector('.gf-section-actions .tb-view'), null, 'but not the view switch');
});

test('a quiet board starts folded; the wizard calls it the Quest board', async (t) => {
  const harness = await createPreactHarness(t);
  const group = crew('tb-quiet');
  document.body.classList.add('wizard');
  const quiet = board('tb-quiet', {
    counts: { live: 1, needs_you: 0, in_progress: 0, next: 1, ideas: 0, blocked: 0, queued: 0, done_this_week: 0, dropped: 0 },
    tasks: [row('tk_00000009', 'only one')], closed: [],
  });
  const { container } = await mountSection(harness, { group, context: { groups: [quiet] } });
  const section = container.querySelector('[data-fused-taskboard="tb-quiet"]');
  assert.match(section.querySelector('.gf-label').textContent, /Quest board/);
  assert.equal(section.querySelector('.tb-table'), null, 'nothing needs the operator: folded');
  await harness.act(() => harness.fireEvent(section.querySelector('.gf-section-toggle'), 'click'));
  assert.deepEqual([...section.querySelectorAll('.tb-title')].map((n) => n.textContent), ['only one']);
  document.body.classList.remove('wizard');
});

test('the feature off or a hidden group draws nothing', async (t) => {
  const harness = await createPreactHarness(t);
  const off = await mountSection(harness, { group: crew('tb-off'), context: { error: 'feature_disabled' }, status: 404 });
  assert.equal(off.container.querySelector('[data-fused-taskboard]'), null);
  assert.equal(off.source.status.value, 'unavailable');

  const hidden = await mountSection(harness, { group: crew('tb-hidden'), context: { groups: [board('someone-else')] } });
  assert.equal(hidden.container.querySelector('[data-fused-taskboard]'), null);

});

test('a linked node\'s group reads its board from that node, read-only without taskboard.write', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ TaskboardSection, peerWrites }, { createTaskboardContextSource }] = await Promise.all([
    harness.importDashboardModule('js/groups-taskboard.js'),
    harness.importDashboardModule('js/taskboard-context.js'),
  ]);
  const local = fetchWith({ groups: [] });
  const remote = fetchWith({ groups: [board('tb-peer')] });
  const asked = [];
  const boards = (node) => { asked.push(node); return { source: remoteSource, api: { capture: async () => ({}) } }; };
  const remoteSource = createTaskboardContextSource({ fetchImpl: remote.fetchImpl });
  const peer = { ...crew('tb-peer'), fleet_node: { id: 'inst_forge', name: 'forge', local: false } };
  const mounted = await harness.mount(harness.html`<div>
    <${TaskboardSection} group=${peer} snapshot=${{ groups: [peer] }} source=${createTaskboardContextSource({ fetchImpl: local.fetchImpl })} boards=${boards} />
  </div>`);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.ok(asked.every((node) => node === 'inst_forge') && asked.length > 0, 'the board is read from the group\'s node');
  assert.deepEqual(local.calls, [], 'this node\'s context is not read for a peer group');
  assert.ok(mounted.container.querySelector('[data-fused-taskboard="tb-peer"]'), 'the peer group draws its board');
  assert.equal(mounted.container.querySelector('[data-taskboard-new]'), null, 'no peer view with taskboard.write: read-only');

  assert.equal(peerWrites(null, 'crew'), false);
  assert.equal(peerWrites({ level: 'restricted', groups: { crew: ['taskboard.read'] } }, 'crew'), false);
  assert.equal(peerWrites({ level: 'restricted', groups: { crew: ['taskboard.write'] } }, 'crew'), true);
  assert.equal(peerWrites({ level: 'unrestricted' }, 'crew'), true);
  const { peerHolds } = await harness.importDashboardModule('js/groups-taskboard.js');
  assert.equal(peerHolds({ level: 'restricted', groups: { crew: ['taskboard.write'] } }, 'crew', 'taskboard.assign'), false);
  assert.equal(peerHolds({ level: 'restricted', groups: { crew: ['taskboard.assign'] } }, 'crew', 'taskboard.assign'), true);
});

test('while the feature is off the source looks again only now and then', async (t) => {
  const harness = await createPreactHarness(t);
  const { createTaskboardContextSource, taskboardCountsText } = await harness.importDashboardModule('js/taskboard-context.js');
  let clock = 0;
  const { calls, fetchImpl } = fetchWith({}, 404);
  const source = createTaskboardContextSource({ fetchImpl, now: () => clock });
  await source.refresh();
  await source.refresh();
  assert.equal(calls.length, 1);
  clock = 31_000;
  await source.refresh();
  assert.equal(calls.length, 2);

  assert.equal(taskboardCountsText({ live: 0 }), 'empty');
  assert.equal(taskboardCountsText({ live: 9, needs_you: 1, waiting_on_you: 1, blocked: 1, stalled: 1, in_progress: 2, next: 6, ideas: 3 }),
    '9 · 1 waiting on you · 1 blocked · 1 stalled · 2 in progress · 6 next · 3 ideas', 'only what is assigned to the operator waits on it');
});

test('a stalled assignment shows amber and says its agent went idle', async (t) => {
  const group = crew('tb-stalled');
  const harness = await createPreactHarness(t);
  const context = { groups: [{
    group_id: 2, group: 'tb-stalled',
    counts: { live: 1, needs_you: 0, in_progress: 0, next: 0, ideas: 0, blocked: 0, stalled: 1, queued: 0, done_this_week: 0, dropped: 0 },
    tasks: [row('tk_00000011', 'Port the sweep', { assignee: { agent_id: 'agt_builder' }, assigned_by: 'human:operator',
      assigned_at: ago(20), stalled: true, idle_since: ago(7) })],
    closed: [],
  }] };
  const { container } = await mountSection(harness, { group, context });
  const section = container.querySelector('[data-fused-taskboard="tb-stalled"]');
  // A stalled task is not the operator's (tk_3cb51ce0): the section stays folded.
  assert.equal(section.querySelector('.tb-table'), null, 'a stalled task does not open the section');
  assert.match(section.querySelector('.tb-counts').getAttribute('title'), /1 stalled/);
  await harness.act(() => harness.fireEvent(section.querySelector('.gf-section-toggle'), 'click'));
  const state = section.querySelector('tr.tb-row-stalled .tb-state');
  assert.ok(state, 'the row is drawn as stalled');
  assert.ok(state.classList.contains('tb-state-stalled'));
  assert.equal(state.textContent, 'stalled');
  assert.match(state.closest('.tb-state-wrap').getAttribute('title'), /^stalled · with .* · idle, no report$/);
});

test('the list packs its columns left: Name is content-sized between a minimum and a maximum, never the column that takes the rest', async (t) => {
  const { readFileSync } = await import('node:fs');
  const read = (path) => readFileSync(new URL(path, import.meta.url), 'utf8');
  const ui = read('../dashboard/ui.css');
  const css = read('../dashboard/dashboard.css');
  const rule = (src, sel) => src.match(new RegExp(`${sel.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\s*\\{([^}]*)\\}`))?.[1] || '';
  // The shared convention (ui.css): a fitted column shrinks to its content,
  // a fitted cell sits between its minimum and maximum and wraps past it.
  assert.match(rule(ui, 'table.ui-table-auto > colgroup > col.ui-col-fit'), /width:\s*1%/);
  const fit = rule(ui, '.ui-fit');
  assert.match(fit, /max-width:\s*var\(--ui-fit-max/);
  assert.match(fit, /width:\s*max-content/);
  assert.doesNotMatch(fit, /nowrap|ellipsis/, 'past the maximum, text wraps');
  assert.match(rule(css, '.tb-title-box'), /--ui-fit-min:\s*12em;\s*--ui-fit-max:\s*30em/, 'Name: 12em to 30em');
  // The task list uses it: Name is fitted, and only Files takes the rest.
  const harness = await createPreactHarness(t);
  const group = crew('tb-cols');
  const { container } = await mountSection(harness, { group, context: { groups: [board('tb-cols')] } });
  const table = container.querySelector('table.tb-table');
  assert.ok(table.classList.contains('ui-table-auto'));
  const cols = [...table.querySelectorAll('colgroup col')];
  assert.deepEqual(cols.filter((c) => !c.classList.contains('ui-col-fit')).map((c) => c.className), ['ui-col-rest tb-col-files']);
  assert.ok(table.querySelector('td.tb-title-cell > .ui-fit.tb-title-box'), 'the title sits in a fitted box');
});

test('history drawn as its own table (no live tasks) is a bounded auto-width table too', async (t) => {
  const harness = await createPreactHarness(t);
  const group = crew('tb-hist');
  const quiet = board('tb-hist', { tasks: [], counts: { live: 0, done_this_week: 1 } });
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem('tclaude.dash.groups.fused.taskboard.tb-hist', '1');
  const { container } = await mountSection(harness, { group, context: { groups: [quiet] } });
  const toggle = container.querySelector('.tb-history-toggle');
  assert.ok(toggle, 'the history line');
  await harness.act(() => harness.fireEvent(toggle, 'click'));
  const table = container.querySelector('table.tb-closed-table');
  assert.ok(table, 'the closed tasks, in a table of their own');
  assert.ok(table.classList.contains('ui-table-auto'));
  assert.deepEqual([...table.querySelectorAll('colgroup col')].filter((c) => !c.classList.contains('ui-col-fit')).map((c) => c.className), ['ui-col-rest tb-col-files']);
});

test('the List\'s Filter shows done on demand and hides states, remembered per group', async (t) => {
  const group = crew('tb-filter');
  const harness = await createPreactHarness(t);
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  const { container } = await mountSection(harness, { group, context: { groups: [board('tb-filter')] } });
  const section = container.querySelector('[data-fused-taskboard="tb-filter"]');
  const titles = () => [...section.querySelectorAll('tr.tb-row .tb-title')].map((n) => n.textContent);
  const button = section.querySelector('.tb-toolbar .tb-list-filter');
  assert.ok(button, 'the List has a Filter in its toolbar');
  assert.match(button.textContent, /Filter/);
  const badge = () => section.querySelector('#tb-filter-tb-filter-badge');
  assert.equal(badge().hidden, true, 'the default: no count');
  await harness.act(() => harness.fireEvent(button, 'click'));
  const box = (state) => section.querySelector(`#tb-filter-tb-filter-${state}`);
  const ticked = (state) => box(state).hasAttribute('checked');
  const toggle = async (state) => {
    box(state).checked = !ticked(state);
    await harness.act(() => harness.fireEvent(box(state), 'change'));
  };
  assert.equal(ticked('done'), false, 'done is hidden by default, as before');
  assert.equal(ticked('open'), true);
  assert.equal(section.querySelector('.tb-closed'), null);

  await toggle('done');
  assert.deepEqual([...section.querySelectorAll('.tb-closed .tb-title')].map((n) => n.textContent), ['Docs first draft'], 'show done');
  assert.equal(section.querySelector('.tb-history-toggle').getAttribute('aria-expanded'), 'true', 'the history line follows');
  assert.equal(badge().textContent, '1');
  assert.deepEqual(JSON.parse(dashPrefs.getItem('tclaude.dash.groups.taskboard.list-hidden.tb-filter')), ['dropped'], 'remembered per group');

  await toggle('idea');
  await toggle('blocked');
  assert.deepEqual(titles(), ['Steward input row', 'Engine: dynamic nodes', 'Wait-for-work node', 'Docs first draft']);
  assert.equal(section.querySelector('tr.tb-ideas-divider'), null, 'no Ideas band while ideas are hidden');
  assert.equal(section.querySelector('[data-tb-filtered]').dataset.tbFiltered, '2', 'the list says what the filter hides');
  assert.equal(badge().textContent, '3');

  // show all brings back every hidden live state and keeps done shown.
  await harness.act(() => harness.fireEvent(section.querySelector('.tb-filter-reset'), 'click'));
  assert.deepEqual(JSON.parse(dashPrefs.getItem('tclaude.dash.groups.taskboard.list-hidden.tb-filter')), ['dropped']);
  assert.equal(titles().length, 6, 'the five live tasks and the done one');
  await toggle('idea');

  await harness.act(() => harness.fireEvent(section.querySelector('[data-tb-filter-reset]'), 'click'));
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.taskboard.list-hidden.tb-filter'), null, 'Reset puts the default back');
  assert.equal(titles().length, 5);
  assert.equal(section.querySelector('.tb-closed'), null);
  assert.equal(badge().hidden, true);
});

test('the history line\'s toggle shows and hides done and dropped in the Filter', async (t) => {
  const group = crew('tb-filter-history');
  const harness = await createPreactHarness(t);
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem('tclaude.dash.groups.taskboard.list-hidden.tb-filter-history', JSON.stringify(['done', 'dropped', 'idea', 'bogus']));
  const { container } = await mountSection(harness, { group, context: { groups: [board('tb-filter-history')] } });
  const section = container.querySelector('[data-fused-taskboard="tb-filter-history"]');
  assert.equal([...section.querySelectorAll('tr.tb-row')].some((r) => r.dataset.taskId === 'tk_00000005'), false, 'a stored hidden state holds');
  await harness.act(() => harness.fireEvent(section.querySelector('.tb-history-toggle'), 'click'));
  assert.deepEqual(JSON.parse(dashPrefs.getItem('tclaude.dash.groups.taskboard.list-hidden.tb-filter-history')), ['idea'],
    'history opens done and dropped together; an unknown stored state is dropped');
  await harness.act(() => harness.fireEvent(section.querySelector('.tb-history-toggle'), 'click'));
  assert.deepEqual(JSON.parse(dashPrefs.getItem('tclaude.dash.groups.taskboard.list-hidden.tb-filter-history')), ['idea', 'done', 'dropped']);
});

test('with ideas unticked, an idea in another state keeps the Ideas band above it', async (t) => {
  const group = crew('tb-filter-band');
  const harness = await createPreactHarness(t);
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem('tclaude.dash.groups.taskboard.list-hidden.tb-filter-band', JSON.stringify(['done', 'dropped', 'idea']));
  const b = board('tb-filter-band');
  b.tasks.push(row('tk_00000007', 'Blocked idea', { stage: 'idea', status: 'blocked', block_reason: 'later' }));
  const { container } = await mountSection(harness, { group, context: { groups: [b] } });
  const section = container.querySelector('[data-fused-taskboard="tb-filter-band"]');
  const order = [...section.querySelectorAll('tr.tb-row, tr.tb-ideas-divider')].map((r) => r.dataset.taskId || 'band');
  assert.deepEqual(order.slice(-2), ['band', 'tk_00000007'], 'the band still separates it from the queue');
  assert.equal(section.querySelector('[data-task-id="tk_00000005"]'), null, 'the open idea is hidden');
});

test('the Lanes view, which has no Filter, shows its history whole', async (t) => {
  const group = crew('tb-filter-lanes');
  const harness = await createPreactHarness(t);
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem('tclaude.dash.groups.taskboard.view.tb-filter-lanes', 'lanes');
  dashPrefs.setItem('tclaude.dash.groups.taskboard.list-hidden.tb-filter-lanes', JSON.stringify(['dropped']));
  const b = board('tb-filter-lanes');
  b.closed.push(row('tk_00000008', 'Dropped one', { status: 'dropped', closed_at: ago(70) }));
  const { container } = await mountSection(harness, { group, context: { groups: [b] } });
  const section = container.querySelector('[data-fused-taskboard="tb-filter-lanes"]');
  assert.equal(section.querySelector('.tb-list-filter'), null, 'no Filter in Lanes');
  assert.deepEqual([...section.querySelectorAll('.tb-closed .tb-title')].map((n) => n.textContent), ['Docs first draft', 'Dropped one'],
    'history open shows done and dropped alike');
});
