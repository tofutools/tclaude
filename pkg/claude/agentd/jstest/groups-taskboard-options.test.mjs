// A board's Board options drawer (groups-taskboard-options.js and its
// model): the ways in from the Task board section, provenance and reset,
// the draft and its Save (a PUT of the whole document at the version read,
// carried onto a newer version when that is refused as stale), the
// read-only drawer of a board this viewer may not change, and the deep
// link's route.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const EVENT_NAMES = ['assigned', 'started', 'unassigned', 'reopened', 'moved_away', 'comment', 'report_done', 'report_failed', 'unblocked', 'epic_close'];
const DEFAULT_TO = { comment: ['assignee', 'creator'], report_done: ['assigner'], report_failed: ['assigner'], epic_close: ['creator'] };
const COLUMNS = [{ slug: 'in-review', label: 'In review', count: 0 }, { slug: 'in-ci', label: 'In CI', count: 1 }];

// resolve mirrors boardopts.Resolve for the fields the drawer reads.
function resolve(options = {}, globals = {}) {
  const idle = options.idle || {};
  const v = (value, source) => ({ value, source });
  const enabled = idle.enabled !== undefined ? v(idle.enabled, 'board')
    : globals.idle_check !== undefined ? v(globals.idle_check, 'global') : v(false, 'default');
  const grace = idle.grace ? v(idle.grace, 'board') : globals.idle_grace && globals.idle_grace !== '0' ? v(globals.idle_grace, 'global') : v('5m', 'default');
  const globallyOff = globals.idle_grace === '0';
  return {
    idle: {
      active: enabled.value && !globallyOff, globally_off: globallyOff || undefined, enabled, grace,
      nudge: idle.nudge !== undefined ? v(idle.nudge, 'board') : v(true, 'default'),
      tell: idle.tell ? v(idle.tell, 'board') : v(['assigner'], 'default'),
      check_columns: idle.check_columns ? v(idle.check_columns, 'board') : v([], 'default'),
    },
    events: EVENT_NAMES.map((name) => {
      const set = options.events?.[name] || {};
      return {
        name, label: `the ${name} event`,
        enabled: set.enabled !== undefined ? v(set.enabled, 'board') : v(true, 'default'),
        to: set.to ? v(set.to, 'board') : v(DEFAULT_TO[name] || ['assignee'], 'default'),
      };
    }),
  };
}

function optionsView(options = {}, { globals = {}, version = 0 } = {}) {
  return { group: 'crew', options, resolved: resolve(options, globals), inherited: resolve({}, globals), globals, version, keys: [] };
}

function fakeApi(view, { stale = null } = {}) {
  const calls = [];
  let current = view;
  return {
    calls,
    options: async (group) => { calls.push(['options', group]); return current; },
    saveOptions: async (group, options, version) => {
      calls.push(['saveOptions', group, options, version]);
      if (stale) {
        const err = new Error('stale');
        err.status = 409;
        err.body = { code: 'stale', current: stale };
        current = stale;
        stale = null;
        throw err;
      }
      current = optionsView(options, { version: version + 1 });
      return current;
    },
    addColumn: async () => ({}), changeColumn: async () => ({}), removeColumn: async () => ({}), setColumnState: async () => ({}),
  };
}

function entryOf(options = { version: 0, idle_active: false, idle_source: 'default', set: [] }) {
  return {
    group: 'crew', group_id: 1,
    counts: { live: 1, needs_you: 0, next: 1 },
    columns: COLUMNS,
    tasks: [{ id: 'tk_00000001', group: 'crew', title: 'one', stage: 'next', status: 'open', depends_on: [], created_at: new Date().toISOString() }],
    closed: [],
    options,
  };
}

const tick = () => new Promise((r) => setTimeout(r, 0));
// LinkeDOM reflects a rendered checkbox's state as its attribute until a
// script sets the property.
const isOn = (el) => (typeof el.checked === 'boolean' ? el.checked : el.hasAttribute('checked'));

async function mountSection(t, { view = 'list', by = 'agent', api, readApi, entry = entryOf(), open = true, group = { name: 'crew', members: [] } } = {}) {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const route = await harness.importDashboardModule('js/groups-taskboard-options-route.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${group.name}`, view);
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.lanes-by.${group.name}`, by);
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${group.name}`, open ? '1' : '0');
  const context = harness.signals.signal({ groups: [entry] });
  const source = { context, status: harness.signals.signal('ready'), attach: () => () => {}, refresh: async () => {} };
  const boards = () => ({ source, api: readApi || api });
  const mounted = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} boards=${boards} />`);
  t.after(() => { route.boardOptionsRoute.value = null; });
  const drawer = () => harness.document.querySelector('#taskboard-options-modal');
  const settle = async () => { await harness.act(async () => { await tick(); await tick(); }); };
  const click = async (el) => { await harness.act(async () => { el.click(); await tick(); }); await settle(); };
  const toggle = async (el) => { await harness.act(async () => { el.checked = !isOn(el); harness.fireEvent(el, 'change'); await tick(); }); };
  return { harness, c: mounted.container, route, context, drawer, settle, click, toggle, unmount: mounted.unmount };
}

test('the model: drafts, provenance, unpinning, changes and rebasing', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/groups-taskboard-options-model.js');
  const doc = { idle: { enabled: true, grace: '10m', tell: ['owners'] }, events: { comment: { enabled: false }, report_failed: { to: ['assigner', 'operator'] } } };
  const flat = m.flatOptions(doc);
  assert.deepEqual(flat, { 'idle.enabled': true, 'idle.grace': '10m', 'idle.tell': ['owners'], 'events.comment.enabled': false, 'events.report_failed.to': ['assigner', 'operator'] });
  assert.deepEqual(m.documentOf(flat), doc, 'a draft writes back the document it came from');
  assert.deepEqual(m.documentOf({}), {}, 'a board that sets nothing is {}');

  const view = optionsView(doc, { globals: { idle_grace: '2m' } });
  assert.deepEqual(m.inheritedOf(view, 'idle.grace'), { value: '2m', source: 'global' });
  assert.deepEqual(m.inheritedOf(view, 'events.report_failed.to'), { value: ['assigner'], source: 'default' });
  assert.deepEqual(m.effectiveOf(view, flat, 'idle.grace'), { value: '10m', source: 'board' });
  // An older node sends no `inherited`: the idle values come from its
  // globals and the built-in defaults, an event's from the built-in table.
  const old = { ...view, inherited: undefined };
  assert.deepEqual(m.inheritedOf(old, 'idle.grace'), { value: '2m', source: 'global' });
  assert.deepEqual(m.inheritedOf(old, 'events.report_failed.to'), { value: ['assigner'], source: 'default' });
  assert.deepEqual(m.inheritedOf(old, 'events.assigned.to'), { value: ['assignee'], source: 'default' }, 'unset there, resolved is the inherited value');

  // Setting a value the board does not set back to what it inherits leaves
  // it unset: toggling there and back is no change.
  let draft = m.setOption(view, flat, flat, 'idle.nudge', false);
  assert.deepEqual(m.changedKeys(flat, draft), ['idle.nudge']);
  draft = m.setOption(view, flat, draft, 'idle.nudge', true);
  assert.deepEqual(m.changedKeys(flat, draft), [], 'back to the inherited value is no change');
  // A value the board sets stays set, even at the inherited value.
  draft = m.setOption(view, flat, flat, 'idle.grace', '2m');
  assert.equal(draft['idle.grace'], '2m');
  // Lists keep the daemon's order.
  draft = m.setOption(view, flat, flat, 'idle.tell', ['operator', 'assigner']);
  assert.deepEqual(draft['idle.tell'], ['assigner', 'operator']);
  draft = m.setOption(view, flat, flat, 'idle.check_columns', ['in-ci', 'in-review'], ['in-review', 'in-ci']);
  assert.deepEqual(draft['idle.check_columns'], ['in-review', 'in-ci']);
  // Reset one key, or a whole prefix.
  assert.equal(Object.hasOwn(m.resetOption(flat, 'idle.grace'), 'idle.grace'), false);
  assert.deepEqual(Object.keys(m.resetOption(flat, 'events.')).filter((k) => k.startsWith('events.')), []);

  // A stale save: the newer document, then this draft's own edits.
  const mine = m.setOption(view, flat, m.resetOption(flat, 'idle.tell'), 'idle.nudge', false);
  const newer = { ...flat, 'idle.grace': '20m', 'events.assigned.enabled': false };
  assert.deepEqual(m.rebaseDraft(flat, mine, newer),
    { 'idle.enabled': true, 'idle.grace': '20m', 'events.comment.enabled': false, 'events.report_failed.to': ['assigner', 'operator'], 'events.assigned.enabled': false, 'idle.nudge': false });

  assert.equal(m.graceError('10m'), '');
  assert.equal(m.graceError('1h30m'), '');
  assert.match(m.graceError('soon'), /duration/);
  assert.match(m.graceError('30s'), /Between 1m and 168h/);
  assert.match(m.graceError('200h'), /Between/);
  // The node's own event list is the authority: a newer event shows under
  // its label, an older node's list stays as it is.
  const nextNode = optionsView();
  nextNode.resolved.events.push({ name: 'future_event', label: 'Something new happens', enabled: { value: true, source: 'default' }, to: { value: [], source: 'default' } });
  assert.deepEqual(m.eventRows(nextNode).at(-1), { name: 'future_event', label: 'Something new happens', title: 'Something new happens' });
  assert.equal(m.eventRows(nextNode).length, 11);
  assert.equal(m.eventRows({}).length, 10);
  assert.deepEqual(m.draftErrors({ 'idle.grace': '0' }), { 'idle.grace': 'Between 1m and 168h; turn the check off with its switch' });
});

test('the section line summarises what a board changes, and the route names a board', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/groups-taskboard-options-model.js');
  assert.deepEqual(m.optionsSummary({ idle_active: false, idle_source: 'default', set: [] }), [], 'nothing changed: nothing said');
  assert.deepEqual(m.optionsSummary({ idle_active: true, idle_source: 'board', set: ['idle.enabled', 'events.comment.enabled', 'events.comment.to', 'events.report_failed.to'] })
    .map((p) => [p.section, p.text]), [['idle', 'idle check on'], ['events', '2 messages changed']]);
  assert.deepEqual(m.optionsSummary({ idle_active: true, idle_source: 'global', set: [] }).map((p) => p.text), ['idle check on'], 'on everywhere is said too');
  assert.deepEqual(m.optionsSummary({ idle_active: false, idle_globally_off: true, idle_source: 'board', set: ['idle.enabled'] }).map((p) => p.text), ['idle check off globally']);
  assert.deepEqual(m.optionsSummary({ idle_active: false, idle_source: 'default', set: ['idle.grace'] }).map((p) => p.text), ['idle check off']);

  assert.equal(m.formatBoardOptionsRoute({ group: 'crew' }), 'crew');
  assert.equal(m.formatBoardOptionsRoute({ group: 'crew', node: 'inst_ab12' }), 'inst_ab12:crew');
  assert.equal(m.formatBoardOptionsRoute({ group: 'inst_x:odd' }), ':inst_x:odd', "a local name that reads as a node's is escaped");
  for (const target of [{ group: 'crew', node: '' }, { group: 'a:b@c', node: 'inst_ab12' }, { group: 'inst_x:odd', node: '' }, { group: 'a:b', node: '' }]) {
    assert.deepEqual(m.parseBoardOptionsRoute(m.formatBoardOptionsRoute(target)), target);
  }
  assert.equal(m.parseBoardOptionsRoute(''), null);
  assert.equal(m.parseBoardOptionsRoute(':'), null);
  assert.equal(m.sectionOfHash('#idle'), 'idle');
  assert.equal(m.sectionOfHash('#columns'), 'columns');
  assert.equal(m.sectionOfHash('#nope'), '');
});

test('⚙ Board options opens the drawer from the toolbar; values say where they come from; Save sends the document at its version', async (t) => {
  const view = optionsView({ idle: { grace: '10m' } }, { globals: { idle_grace: '2m' }, version: 3 });
  const api = fakeApi(view);
  const s = await mountSection(t, { api, entry: entryOf({ version: 3, idle_active: false, idle_source: 'default', set: ['idle.grace'] }) });
  const button = [...s.c.querySelectorAll('.tb-toolbar .tb-options-btn')];
  assert.equal(button.length, 1, 'the toolbar has one ⚙ Board options');
  assert.match(button[0].textContent, /Board options/);
  assert.equal(s.drawer(), null);
  await s.click(button[0]);
  const d = s.drawer();
  assert.ok(d, 'the drawer opens');
  assert.match(d.querySelector('h3').textContent, /Board options · crew/);
  assert.match(d.querySelector('h3').textContent, /this node/);
  assert.deepEqual(api.calls[0], ['options', 'crew']);
  assert.equal(s.route.boardOptionsRoute.value?.group, 'crew');

  // Provenance: the grace is set here (the global 2m beside it), the rest inherited.
  const src = (key) => d.querySelector(`[data-tb-opt-src="${key}"]`);
  assert.match(src('idle.grace').textContent, /set on this board · global: 2m/);
  assert.match(src('idle.enabled').textContent, /default/);
  assert.equal(d.querySelector('#tb-opt-grace').value, '10m');
  assert.equal(d.querySelector('#taskboard-options-save').disabled, true, 'nothing to save yet');

  // Turn the idle check on, put the grace back to inherited, and send the
  // comment message to the operator too.
  await s.toggle(d.querySelector('input[aria-label="Idle check"]'));
  await s.click(src('idle.grace').querySelector('.tb-opt-reset'));
  assert.equal(d.querySelector('#tb-opt-grace').value, '2m', 'the inherited value shows before Save');
  assert.match(src('idle.grace').textContent, /global/);
  await s.toggle(d.querySelector('input[aria-label="Comment added: Operator"]'));
  assert.match(d.querySelector('[data-tb-opt-unsaved]').textContent, /3 unsaved changes/);
  await s.click(d.querySelector('#taskboard-options-save'));
  const put = api.calls.find((c) => c[0] === 'saveOptions');
  assert.deepEqual(put, ['saveOptions', 'crew', { idle: { enabled: true }, events: { comment: { to: ['assignee', 'creator', 'operator'] } } }, 3]);
  assert.equal(s.drawer(), null, 'a save closes the drawer');
  assert.equal(s.route.boardOptionsRoute.value, null);
});

test('a bad grace is refused before Save, and the event table turns messages off', async (t) => {
  const api = fakeApi(optionsView());
  const s = await mountSection(t, { api });
  await s.click(s.c.querySelector('.tb-options-btn'));
  const d = s.drawer();
  await s.harness.act(async () => { await s.harness.input(d.querySelector('#tb-opt-grace'), '30s'); });
  assert.match(d.querySelector('#tb-opt-grace-error').textContent, /Between 1m and 168h/);
  assert.equal(d.querySelector('#tb-opt-grace').getAttribute('aria-invalid'), 'true');
  assert.equal(d.querySelector('#taskboard-options-save').disabled, true);
  await s.harness.act(async () => { await s.harness.input(d.querySelector('#tb-opt-grace'), '15m'); });
  assert.equal(d.querySelector('#tb-opt-grace-error'), null);

  await s.click(d.querySelector('.tb-opt-all-off'));
  const rows = [...d.querySelectorAll('[data-tb-opt-event]')];
  assert.equal(rows.length, 10, 'one row per event');
  assert.ok(rows.every((row) => row.classList.contains('off')), 'all off turns every message off');
  await s.click(d.querySelector('.tb-opt-defaults'));
  assert.ok(rows.every((row) => !row.classList.contains('off')), '↺ defaults puts them back');
  assert.match(d.querySelector('[data-tb-opt-unsaved]').textContent, /1 unsaved change\b/, 'only the grace is left changed');
});

test('a save refused as stale carries the edits onto the newer options', async (t) => {
  const newer = optionsView({ idle: { grace: '20m' } }, { version: 5 });
  const api = fakeApi(optionsView({}, { version: 4 }), { stale: newer });
  const s = await mountSection(t, { api, entry: entryOf({ version: 4, idle_active: false, idle_source: 'default', set: [] }) });
  await s.click(s.c.querySelector('.tb-options-btn'));
  const d = s.drawer();
  await s.toggle(d.querySelector('input[aria-label="Idle check"]'));
  await s.click(d.querySelector('#taskboard-options-save'));
  assert.ok(s.drawer(), 'the drawer stays open');
  assert.match(d.querySelector('[role="alert"]').textContent, /changed elsewhere/);
  assert.equal(d.querySelector('#tb-opt-grace').value, '20m', 'the newer value shows');
  assert.equal(isOn(d.querySelector('input[aria-label="Idle check"]')), true, 'the edit is kept');
  await s.click(d.querySelector('#taskboard-options-save'));
  const puts = api.calls.filter((c) => c[0] === 'saveOptions');
  assert.deepEqual(puts[1], ['saveOptions', 'crew', { idle: { enabled: true, grace: '20m' } }, 5], 'saved over the newer version');
});

test('Cancel with unsaved changes asks first', async (t) => {
  const s = await mountSection(t, { api: fakeApi(optionsView()) });
  await s.click(s.c.querySelector('.tb-options-btn'));
  const d = s.drawer();
  await s.toggle(d.querySelector('input[aria-label="Idle check"]'));
  const { shellState } = await s.harness.importDashboardModule('js/shell-state.js');
  await s.click(d.querySelector('#taskboard-options-cancel'));
  assert.equal(shellState.confirmation.value?.title, 'Discard input?');
  await s.harness.act(async () => { shellState.resolveConfirmation(false); await tick(); });
  assert.ok(s.drawer(), 'kept');
  await s.click(d.querySelector('.tb-opt-close'));
  await s.harness.act(async () => { shellState.resolveConfirmation(true); await tick(); });
  await s.settle();
  assert.equal(s.drawer(), null, 'discarded and closed');
});

test('without taskboard.write the drawer is read-only and says why; a peer board reads through its node', async (t) => {
  const reader = fakeApi(optionsView({ idle: { enabled: true } }));
  const peer = { name: 'crew@node-b', fleet_node: { id: 'inst_b', name: 'node-b', local: false }, members: [] };
  const s = await mountSection(t, { api: null, readApi: reader, group: peer });
  await s.click(s.c.querySelector('.tb-options-btn'));
  const d = s.drawer();
  assert.ok(d);
  assert.deepEqual(s.route.boardOptionsRoute.value, { group: 'crew', node: 'inst_b', section: '', column: '' }, 'the route names the board as its node does');
  assert.deepEqual(reader.calls[0], ['options', 'crew'], "read under the group's own name on that node");
  assert.match(d.querySelector('h3').textContent, /node-b/);
  assert.match(d.querySelector('[data-tb-opt-readonly]').textContent, /no taskboard.write on node-b/);
  assert.equal(d.querySelector('#taskboard-options-save'), null, 'no Save');
  assert.equal(d.querySelector('.tb-opt-reset'), null, 'no ↺');
  assert.ok([...d.querySelectorAll('input')].every((input) => input.disabled), 'every control is read-only');
  assert.match(d.querySelector('#taskboard-options-cancel').textContent, /Close/);
});

test('taskboard.idle_grace "0" greys the idle check and says it is off everywhere', async (t) => {
  const s = await mountSection(t, { api: fakeApi(optionsView({ idle: { enabled: true } }, { globals: { idle_grace: '0' } })) });
  await s.click(s.c.querySelector('.tb-options-btn'));
  const d = s.drawer();
  const sw = d.querySelector('input[aria-label="Idle check"]');
  assert.equal(sw.disabled, true);
  assert.equal(isOn(sw), false);
  assert.ok(d.querySelector('[data-tb-opt-globally-off]'));
  assert.match(d.querySelector('.tb-opt-state').textContent, /off on every board/);
});

test('folded, the section line has an icon-only ⚙ and the summary', async (t) => {
  const s = await mountSection(t, { api: fakeApi(optionsView()), open: false,
    entry: entryOf({ version: 2, idle_active: true, idle_source: 'board', set: ['idle.enabled', 'events.comment.enabled'] }) });
  const icon = s.c.querySelector('.gf-section-actions .tb-options-btn');
  assert.ok(icon, 'the folded line has the ⚙');
  assert.equal(icon.textContent.trim(), '⚙');
  assert.equal(icon.getAttribute('aria-label'), 'Board options');
  assert.deepEqual([...s.c.querySelectorAll('[data-tb-options-summary]')].map((chip) => chip.textContent.trim()), ['⏱ idle check on', '✉ 1 message changed']);
  await s.click(icon);
  assert.ok(s.drawer(), 'the folded ⚙ opens the drawer');
});

test('a board whose node serves no options has no ⚙ and no summary', async (t) => {
  const old = await mountSection(t, { api: fakeApi(optionsView()), entry: { ...entryOf(), options: undefined } });
  assert.equal(old.c.querySelector('.tb-options-btn'), null, 'no options served: no ⚙');
  assert.equal(old.c.querySelector('[data-tb-options-summary]'), null);
});

test('in the triage lanes too, a custom column has its ⋯ with Column in Board options', async (t) => {
  const s = await mountSection(t, { api: fakeApi(optionsView()), view: 'lanes', by: 'triage' });
  const head = [...s.c.querySelectorAll('.tb-col-more')].find((b) => /In review/.test(b.getAttribute('aria-label') || ''));
  assert.ok(head, 'the In review head has a ⋯');
  await s.click(head);
  const items = [...s.harness.document.querySelectorAll('[data-column-menu]')].map((item) => item.dataset.columnMenu);
  assert.ok(items.includes('rename') && items.includes('options'), `rename and the options: ${items}`);
  await s.click(s.harness.document.querySelector('[data-column-menu="options"]'));
  assert.equal(s.route.boardOptionsRoute.value.column, 'in-review');
});

test("a custom column's ⋯ opens the drawer at the Columns tab on that column; skip saves with the options", async (t) => {
  const api = fakeApi(optionsView());
  const s = await mountSection(t, { api, view: 'lanes' });
  const head = [...s.c.querySelectorAll('.tb-col-more')].find((b) => /In CI/.test(b.getAttribute('aria-label') || ''));
  assert.ok(head, 'the In CI head has a ⋯');
  await s.click(head);
  const item = s.harness.document.querySelector('[data-column-menu="options"]');
  assert.ok(item, '⋯ has Column in Board options');
  await s.click(item);
  const d = s.drawer();
  assert.ok(d);
  assert.equal(d.querySelector('[data-tb-opt-tab="columns"]').getAttribute('aria-selected'), 'true', 'at the Columns tab');
  assert.ok(d.querySelector('tr[data-tb-opt-column="in-ci"]').classList.contains('focus'), 'on the column');
  const skip = d.querySelector('tr[data-tb-opt-column="in-ci"] input[type="checkbox"]');
  assert.equal(isOn(skip), true, 'a custom column is skipped by default');
  await s.toggle(skip);
  await s.click(d.querySelector('#taskboard-options-save'));
  assert.deepEqual(api.calls.find((c) => c[0] === 'saveOptions')[2], { idle: { check_columns: ['in-ci'] } });
});

test('a link opens the drawer at its section; Back closes it', async (t) => {
  const s = await mountSection(t, { api: fakeApi(optionsView()) });
  const { restoreOverlays, overlayParams } = await s.harness.importDashboardModule('js/overlay-routes.js');
  const was = Object.getOwnPropertyDescriptor(globalThis, 'location');
  Object.defineProperty(globalThis, 'location', { value: { hash: '#events' }, configurable: true, writable: true });
  t.after(() => { if (was) Object.defineProperty(globalThis, 'location', was); else delete globalThis.location; });
  await s.harness.act(async () => { restoreOverlays({ 'board-options': 'crew' }); await tick(); });
  await s.settle();
  assert.ok(s.drawer(), 'the link opens it');
  assert.equal(s.route.boardOptionsRoute.value.section, 'events');
  assert.equal(overlayParams()['board-options'], 'crew');
  await s.harness.act(async () => { restoreOverlays({}); await tick(); });
  assert.equal(s.drawer(), null, 'a location without it closes it');
});
