// js/phone-agents-model.js groupsTree / groupPage and js/phone-agents-island.js
// Active | All groups and a group's page: every group as the desktop's tree
// nests them, and a group's own page, driven through what the operator sees
// and taps.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const id = (n) => `agt_${String(n).padStart(32, '0')}`;
const on = (n, title, status = 'idle', extra = {}) => ({ agent_id: id(n), conv_id: `c${n}`, title, online: true, state: { status, harness: 'claude' }, ...extra });
const off = (n, title, extra = {}) => ({ agent_id: id(n), conv_id: `c${n}`, title, online: false, state: {}, ...extra });

const SNAPSHOT = {
  groups: [
    { name: 'webshop', members: [on(1, 'lead', 'idle', { role: 'lead' }), on(2, 'db-fix', 'error', { spawned_by: id(1) })] },
    { name: 'checkout', parent: 'webshop', members: [on(3, 'checkout-ui', 'working')] },
    { name: 'payments', parent: 'webshop', members: [off(4, 'pay-lead'), off(5, 'stripe-adapter', { spawned_by: id(4) })] },
    { name: 'infra', members: [on(6, 'ops', 'awaiting_input')] },
    { name: 'research', members: [] },
    { name: 'release', archived: true, members: [off(7, 'release-bot')] },
    { name: 'hotfix', parent: 'release', members: [on(8, 'patcher')] },
  ],
  ungrouped: [off(9, 'loner')],
};

const names = (nodes) => nodes.map((n) => (n.children.length ? [n.name, names(n.children)] : n.name));

test('the model: every group as the desktop nests them, archived last and hidden until asked', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/phone-agents-model.js');
  const hidden = model.groupsTree(SNAPSHOT);
  // hotfix is not archived: it takes its archived parent's place.
  // Someone online first, then only offline agents, then empty, each in the
  // operator's order.
  assert.deepEqual(names(hidden.nodes), [['webshop', ['checkout', 'payments']], 'infra', 'hotfix', 'Ungrouped', 'research']);
  assert.equal(hidden.count, 6);
  assert.equal(hidden.archived, 1);
  const byName = new Map(model.treeNodes(hidden.nodes).map((n) => [n.name, n]));
  assert.equal(byName.get('payments').dim, true, 'nothing online');
  assert.equal(byName.get('webshop').dim, false);
  assert.equal(byName.get('research').empty, true);
  assert.equal(byName.get('research').dim, true);
  assert.deepEqual(byName.get('webshop').summary.map((s) => `${s.count} ${s.text}`), ['1 error', '1 idle']);
  assert.deepEqual(byName.get('webshop').tree.map(({ row, kids }) => [row.name, kids.map((k) => k.name)]), [['lead', ['db-fix']]]);

  const shown = model.groupsTree(SNAPSHOT, (g) => g, { showArchived: true });
  assert.deepEqual(names(shown.nodes), [['webshop', ['checkout', 'payments']], 'infra', 'Ungrouped', 'research', ['release', ['hotfix']]]);
  const release = shown.nodes.at(-1);
  assert.equal(release.archived, true);
  assert.equal(release.dim, false, 'a subgroup has someone online');

  // The same order at every level: an empty subgroup after an offline one
  // after a live one, whatever the operator's order.
  const nested = model.groupsTree({ groups: [
    { name: 'top', members: [] },
    { name: 'a-empty', parent: 'top', members: [] },
    { name: 'b-off', parent: 'top', members: [off(20, 'x')] },
    { name: 'c-live', parent: 'top', members: [on(21, 'y')] },
  ] });
  assert.deepEqual(names(nested.nodes), [['top', ['c-live', 'b-off', 'a-empty']]]);

  // A crashed agent needs a look, as in Active: its group is not dimmed or
  // folded. A hidden archived subgroup's agents do not light its parent.
  const crashed = model.groupsTree({ groups: [
    { name: 'quiet', members: [off(30, 'q')] },
    { name: 'broke', members: [{ ...off(31, 'b'), state: { exit_reason: 'unexpected' } }] },
    { name: 'parent', members: [off(32, 'p')] },
    { name: 'gone', parent: 'parent', archived: true, members: [on(33, 'g')] },
  ] });
  assert.deepEqual(crashed.nodes.map((n) => [n.name, n.dim]), [['broke', false], ['quiet', true], ['parent', true]]);

  // The filter: a picked group with its subgroups; one under a picked one once.
  const key = (name) => model.groupKey(name);
  assert.deepEqual(names(model.shownTree(hidden.nodes, [key('payments'), key('infra')])), ['payments', 'infra']);
  assert.deepEqual(names(model.shownTree(hidden.nodes, [key('webshop'), key('checkout')])), [['webshop', ['checkout', 'payments']]]);
  assert.deepEqual(names(model.shownTree(hidden.nodes, [key('gone')])), names(hidden.nodes));
});

test('the model: a group\'s page has its crumbs, subgroups and members', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/phone-agents-model.js');
  const payments = model.groupPage(SNAPSHOT, 'payments');
  assert.deepEqual(payments.crumbs, ['webshop', 'payments']);
  assert.deepEqual(payments.children, []);
  assert.deepEqual(payments.tree.map(({ row, kids }) => [row.name, kids.map((k) => k.name)]), [['pay-lead', ['stripe-adapter']]]);
  assert.equal(model.groupSummaryLine(payments), 'Everything offline');
  assert.deepEqual(model.groupPage(SNAPSHOT, 'webshop').children.map((n) => n.name), ['checkout', 'payments']);
  assert.deepEqual(model.groupPage(SNAPSHOT, 'release').children.map((n) => n.name), ['hotfix'], 'an archived group\'s page still lists its subgroups');
  assert.equal(model.groupPage(SNAPSHOT, 'gone'), null);
  assert.equal(model.groupSummaryLine(model.groupPage(SNAPSHOT, 'research')), 'No agents yet');
  assert.equal(model.groupSummaryLine({ live: 1, total: 2 }), '1 of 2 online');
  // A parent loop (corrupt) ends the crumbs instead of spinning.
  const loop = { groups: [{ name: 'a', parent: 'b', members: [] }, { name: 'b', parent: 'a', members: [] }] };
  assert.deepEqual(model.groupPage(loop, 'a').crumbs, ['b', 'a']);
});

function memPrefs(init = {}) {
  const map = new Map(Object.entries(init));
  return { getItem: (k) => (map.has(k) ? map.get(k) : null), setItem: (k, v) => map.set(k, String(v)), removeItem: (k) => map.delete(k), map };
}

async function setup(t, { prefs = memPrefs() } = {}) {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, island, sheet, page, board] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/phone-agents-island.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
    harness.importDashboardModule('js/phone-page.js'),
    harness.importDashboardModule('js/phone-board.js'),
  ]);
  const model = await harness.importDashboardModule('js/phone-agents-model.js');
  t.after(() => { model.agentsFilter.value = []; });
  const state = createDashboardState();
  state.beginRequest();
  state.commitRequest(1, SNAPSHOT);
  const calls = [];
  const actions = {
    openTerminal: (row) => calls.push(['openTerminal', row.name]),
    message: () => {}, rename: () => {}, spawn: (g) => calls.push(['spawn', g]),
    rowAction: (act, row) => calls.push(['rowAction', act, row.name]),
    groupAction: (act, group) => calls.push(['groupAction', act, group]),
  };
  const mounted = await harness.mount(harness.html`<main><${island.PhoneAgents} state=${state} order=${(g) => g} actions=${actions} prefs=${prefs} /></main>
    <${sheet.PhoneSheetHost} />`);
  const c = mounted.container;
  const tap = (element) => harness.act(() => element.click());
  const node = (name) => c.querySelector(`.phone-agents-node[aria-label="${name}"]`);
  const caret = (name) => node(name).querySelector(':scope > h2 .phone-agents-caret-btn');
  const nameButton = (name) => node(name).querySelector(':scope > h2 .phone-agents-fold');
  const more = (name) => node(name).querySelector(`:scope > h2 [aria-label="${name}: more"]`);
  const seg = (label) => [...c.querySelectorAll('.phone-agents-seg button')].find((b) => b.textContent.startsWith(label));
  const button = (label) => [...c.querySelectorAll('button')].find((b) => b.textContent.trim() === label);
  const titles = () => [...c.querySelectorAll('.phone-agent-title')].map((n) => n.textContent);
  const shownGroups = () => [...c.querySelectorAll('.phone-agents-node')].map((n) => n.getAttribute('aria-label'));
  // The View sheet, opened from the sliders in the top bar, and its controls.
  const viewAction = () => page.phoneBar.value.actions.find((act) => act.key === 'view');
  const openView = async () => { await harness.act(() => viewAction().onClick()); return c.querySelector('.phone-sheet [data-agents-view]'); };
  const sw = (key) => c.querySelector(`[data-agents-view] input[name="agents-view-${key}"]`);
  const orderBy = (key) => c.querySelector(`[data-agents-view] input[name="agents-view-order"][value="${key}"]`);
  const layout = (label) => [...c.querySelectorAll('[data-agents-view] .phone-agents-seg-btn')].find((b) => b.textContent.trim() === label);
  const reset = () => c.querySelector('[data-agents-view] .phone-agents-view-reset');
  // The test DOM does not toggle an input on a click: set it, then change.
  const on = (box) => box.checked ?? box.hasAttribute('checked');
  const flip = (box) => harness.act(() => { box.checked = !on(box); harness.fireEvent(box, 'change'); });
  const pick = (radio) => harness.act(() => { radio.checked = true; harness.fireEvent(radio, 'change'); });
  return { harness, c, tap, node, caret, nameButton, more, button, titles, shownGroups, calls, prefs, page, board, model, viewAction, openView, sw, orderBy, layout, reset, on, flip, pick };
}

test('one view: the sliders open View; Layout, Show, Order and Reset apply at once, are remembered, and dot the sliders off the defaults', async (t) => {
  const prefs = memPrefs();
  const { c, tap, shownGroups, titles, viewAction, openView, sw, orderBy, layout, reset, on, flip, pick } = await setup(t, { prefs });
  assert.equal(c.querySelector('.phone-agents-seg'), null, 'no Active | All groups toggle on the page');
  assert.equal(viewAction().dot, false, 'at the defaults: no dot');
  // The defaults: the tree, offline shown, no empty or archived groups, the
  // operator's group order (the agents in no group last).
  assert.deepEqual(shownGroups(), ['webshop', 'checkout', 'payments', 'infra', 'hotfix', 'Ungrouped']);
  assert.ok(c.querySelector('.phone-agents-sub > .phone-agents-node[aria-label="checkout"]'), 'subgroups nest');
  const sheet = await openView();
  assert.ok(sheet, 'the View sheet');
  assert.equal(reset().disabled, true, 'Reset is off at the defaults');
  assert.equal(layout('Tree').getAttribute('aria-pressed'), 'true');
  assert.deepEqual([on(sw('offline')), on(sw('empty')), on(sw('archived')), on(orderBy('yours'))], [true, false, false, true]);

  // List: one level, every subgroup its own section, every worker its own row.
  await tap(layout('List'));
  assert.equal(prefs.getItem('tclaude.dash.phone.agents.layout'), 'list');
  assert.equal(c.querySelector('.phone-agents-sub'), null, 'nothing nests');
  assert.deepEqual(shownGroups(), ['webshop', 'checkout', 'payments', 'infra', 'hotfix', 'Ungrouped']);
  assert.equal(viewAction().dot, true, 'off the defaults: the sliders carry a dot');
  assert.equal(reset().disabled, false);
  await flip(sw('empty'));
  assert.equal(prefs.getItem('tclaude.dash.phone.agents.empty'), '1');
  assert.ok(shownGroups().includes('research'), 'an empty group shows');
  await flip(sw('archived'));
  assert.equal(shownGroups().at(-1), 'release', 'archived ones last');
  await pick(orderBy('name'));
  assert.equal(prefs.getItem('tclaude.dash.phone.agents.order'), 'name');
  assert.deepEqual(shownGroups(), ['checkout', 'hotfix', 'infra', 'payments', 'research', 'webshop', 'Ungrouped', 'release']);
  // Needs you first: an error or a question first, then who is online.
  await pick(orderBy('needs'));
  assert.deepEqual(shownGroups(), ['webshop', 'infra', 'checkout', 'hotfix', 'payments', 'Ungrouped', 'research', 'release']);
  await pick(orderBy('active'));
  assert.deepEqual(shownGroups().slice(0, 4).sort(), ['checkout', 'hotfix', 'infra', 'webshop'], 'someone online first');
  await flip(sw('offline'));
  assert.equal(prefs.getItem('tclaude.dash.phone.agents.offline'), '0');
  assert.ok(!titles().includes('pay-lead'));

  // Reset: every setting back, forgotten, and the dot gone.
  await tap(reset());
  assert.deepEqual([...prefs.map.keys()].filter((key) => key.startsWith('tclaude.dash.phone.agents.')), []);
  assert.equal(viewAction().dot, false);
  assert.equal(reset().disabled, true);
  assert.deepEqual(shownGroups(), ['webshop', 'checkout', 'payments', 'infra', 'hotfix', 'Ungrouped']);
});

test('the tree: subgroups nest, nothing-online groups dim and fold, folds remembered', async (t) => {
  const { c, tap, node, caret, titles, prefs } = await setup(t, { prefs: memPrefs({ 'tclaude.dash.phone.agents.empty': '1' }) });
  assert.ok(node('webshop').querySelector('.phone-agents-sub > .phone-agents-node[aria-label="checkout"]'), 'checkout under webshop');
  assert.ok(node('payments').classList.contains('phone-agents-dim'));
  assert.equal(caret('payments').getAttribute('aria-expanded'), 'false');
  assert.deepEqual([...node('payments').querySelectorAll('.phone-agents-sum')].map((n) => n.textContent.trim()), ['2 offline'], 'folded, its summary shows');
  await tap(caret('payments'));
  assert.equal(prefs.getItem('tclaude.dash.phone.tree.fold.payments'), '0');
  assert.ok(titles().includes('pay-lead'));
  assert.ok(node('payments').querySelector('.phone-agent-worker .phone-agent-title').textContent === 'stripe-adapter', 'workers under their lead');
  await tap(caret('webshop'));
  assert.equal(prefs.getItem('tclaude.dash.phone.tree.fold.webshop'), '1');
  assert.equal(node('checkout'), null, 'a folded group hides its subgroups');
  // An empty group (Show empty groups) says so, and has nothing to fold.
  assert.equal(node('research').querySelector('.phone-agents-tag').textContent, 'empty');
  assert.equal(node('research').querySelector('button.phone-agents-caret-btn'), null);
  // Ungrouped has no page: its name folds it.
  const loose = node('Ungrouped').querySelector('.phone-agents-fold');
  assert.equal(loose.getAttribute('aria-expanded'), 'false');
  await tap(loose);
  assert.ok(titles().includes('loner'));
  assert.equal(c.querySelector('.phone-group-page'), null);
});

test('archived groups behind Show archived, last; the chips filter', async (t) => {
  const { tap, node, shownGroups, prefs, c, model, openView, sw, flip } = await setup(t, { prefs: memPrefs({ 'tclaude.dash.phone.agents.empty': '1' }) });
  assert.equal(node('release'), null);
  await openView();
  await flip(sw('archived'));
  assert.equal(prefs.getItem('tclaude.dash.phone.agents.archived'), '1');
  assert.deepEqual(shownGroups().slice(-2), ['release', 'hotfix'], 'last, its subgroup under it');
  assert.equal(node('release').querySelector('.phone-agents-tag').textContent, 'archived');
  assert.equal(node('release').querySelector(':scope > h2 .phone-agents-board'), null, 'no board for an archived group');
  await flip(sw('archived'));
  assert.equal(node('release'), null);
  const chip = (label) => [...c.querySelectorAll('.phone-agents-chips button')].find((b) => b.firstChild.textContent.trim() === label);
  assert.ok(chip('research'), 'an empty group shown has its chip');
  await tap(chip('webshop'));
  assert.deepEqual(shownGroups(), ['webshop', 'checkout', 'payments']);
  assert.deepEqual(model.agentsFilter.value, [model.groupKey('webshop')]);
});

test('a group header\'s Board › and ＋ Agent stay links: no fold, no page', async (t) => {
  const { c, tap, node, caret, page, board } = await setup(t);
  const open = () => caret('webshop').getAttribute('aria-expanded');
  const was = open();
  const asked = board.boardRequest.value;
  await tap(node('webshop').querySelector(':scope > h2 .phone-agents-board'));
  assert.equal(board.boardRequest.value, asked + 1, 'Board › opens the group\'s board');
  await tap(node('webshop').querySelector(':scope > h2 [data-agent-add]'));
  assert.ok(c.querySelector('.phone-sheet[role=dialog]'), '＋ Agent opens the spawn sheet');
  assert.equal(open(), was, 'neither folds');
  assert.equal(page.phonePage.value, null, 'neither opens the page');
});

test('a group\'s ⋯ opens its page: crumbs, its actions through the desktop\'s handlers, subgroups, members', async (t) => {
  const { c, tap, more, button, calls, page, harness } = await setup(t);
  await tap(more('webshop'));
  const view = () => c.querySelector('.phone-group-page');
  assert.ok(view());
  assert.equal(page.phonePage.value.title, 'webshop');
  assert.equal(view().querySelector('.phone-agent-sheet-meta').textContent, 'Everything online');
  assert.equal(view().querySelector('.phone-agents-crumbs'), null, 'a top-level group: its title says it all');
  assert.deepEqual([...view().querySelectorAll('.phone-agents-subcard .phone-agents-name')].map((n) => n.textContent), ['checkout', 'payments']);
  await tap(button('⏻ Shut down all'));
  assert.equal(button('⏻ Power on all'), undefined, 'nothing offline to power on');
  await tap(button('Mail ›'));
  assert.deepEqual(calls.splice(0), [['groupAction', 'shutdown-group', 'webshop'], ['groupAction', 'view-group-messages', 'webshop']]);

  // A subgroup's card opens its page; a crumb goes back up.
  await tap(view().querySelector('.phone-agents-subcard[aria-label^="payments"]'));
  assert.equal(page.phonePage.value.title, 'payments');
  assert.deepEqual([...view().querySelectorAll('.phone-agents-crumbs > *')].map((n) => n.textContent.trim()), ['webshop ›', 'payments']);
  assert.equal(view().querySelector('.phone-agents-note').textContent, 'None. Nest one from the desktop.');
  await tap(button('⏻ Power on all'));
  assert.deepEqual(calls.splice(0), [['groupAction', 'power-on-group', 'payments']]);
  // Tapping an offline member resumes it.
  await tap([...view().querySelectorAll('.phone-agent-row')].find((b) => b.textContent.includes('pay-lead')));
  assert.deepEqual(calls.splice(0), [['rowAction', 'dot-toggle', 'pay-lead']]);
  // Back from a page opened from another goes back to that one.
  await harness.act(() => page.phonePage.value.onBack());
  assert.equal(page.phonePage.value.title, 'webshop');
  await tap(view().querySelector('.phone-agents-subcard[aria-label^="payments"]'));
  await tap(button('webshop'));
  assert.equal(page.phonePage.value.title, 'webshop');

  // Back steps through the pages opened, then closes the last.
  for (const title of ['payments', 'webshop']) {
    await harness.act(() => page.phonePage.value.onBack());
    assert.equal(page.phonePage.value.title, title);
  }
  await harness.act(() => page.phonePage.value.onBack());
  assert.equal(view(), null);
  assert.equal(page.phonePage.value, null);
  assert.ok(c.querySelector('.phone-agents-chips'));
});

test('an archived group\'s page offers only its mail', async (t) => {
  const prefs = memPrefs({ 'tclaude.dash.phone.agents.archived': '1' });
  const { c, tap, more, button } = await setup(t, { prefs });
  await tap(more('release'));
  const view = c.querySelector('.phone-group-page');
  assert.deepEqual([...view.querySelectorAll('.phone-group-card button')].map((b) => b.textContent.trim()), ['Mail ›']);
  assert.match(view.querySelector('.phone-agent-sheet-meta').textContent, /archived/);
  assert.equal(button('＋ Spawn'), undefined);
});

test('the model without offline: offline agents and nobody-online groups left out, crashed kept, counts follow', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/phone-agents-model.js');
  const crash = { groups: [
    { name: 'broke', members: [{ ...off(40, 'b'), state: { exit_reason: 'unexpected' } }, off(41, 'quiet')] },
    { name: 'waking', members: [{ ...off(42, 'w'), waking: true }] },
    { name: 'old', members: [off(43, 'x')] },
    { name: 'new', parent: 'old', members: [on(44, 'y')] },
  ] };
  assert.deepEqual(model.agentGroups(crash, (g) => g, { offline: false }).map((g) => [g.name, g.tree.map(({ row }) => row.name)]),
    [['broke', ['b']], ['waking', ['w']], ['new', ['y']]], 'a crashed agent needs a look and a waking one is coming up: both stay');
  const tree = model.groupsTree(crash, (g) => g, { offline: false });
  assert.deepEqual(names(tree.nodes), ['broke', ['old', ['new']], 'waking'], 'in All groups too; old stays for its online subgroup');
  assert.equal(tree.nodes[1].empty, false, 'old has agents, only hidden: not tagged empty');
  const hidden = model.groupsTree(SNAPSHOT, (g) => g, { offline: false });
  assert.deepEqual(names(hidden.nodes), [['webshop', ['checkout']], 'infra', 'hotfix'], 'payments, research and the loner gone');
  assert.equal(hidden.count, 4, 'webshop, checkout, infra, hotfix');
  assert.deepEqual(model.groupsTree(SNAPSHOT).count, 6, 'with offline: every group but the archived one');
  assert.deepEqual(model.agentGroups(SNAPSHOT, (g) => g, { offline: false }).map((g) => [g.name, g.total]),
    [['webshop', 2], ['checkout', 1], ['infra', 1], ['hotfix', 1]]);
});

test('Show offline: off hides offline agents and nobody-online groups, remembered; the chips count what shows', async (t) => {
  const prefs = memPrefs();
  const { c, shownGroups, titles, openView, sw, on, flip } = await setup(t, { prefs });
  await openView();
  assert.ok(on(sw('offline')), 'on by default: as before');
  assert.equal(sw('offline').closest('label').textContent.trim().startsWith('Offline agents'), true);
  await flip(sw('offline'));
  assert.equal(prefs.getItem('tclaude.dash.phone.agents.offline'), '0');
  assert.ok(!titles().includes('pay-lead') && !titles().includes('loner'), 'no offline agents');
  const chips = () => [...c.querySelectorAll('.phone-agents-chips button')].map((b) => b.textContent.replace(/\s+/g, ' ').trim());
  assert.deepEqual(chips(), ['All 5', 'webshop 2', 'checkout 1', 'infra 1', 'hotfix 1'], 'the chips count what shows');
  assert.deepEqual(shownGroups(), ['webshop', 'checkout', 'infra', 'hotfix']);
  await flip(sw('offline'));
  assert.equal(prefs.getItem('tclaude.dash.phone.agents.offline'), null, 'on again: forgotten');
  assert.deepEqual(shownGroups(), ['webshop', 'checkout', 'payments', 'infra', 'hotfix', 'Ungrouped']);
});

// A link to an agent unfolds every group above it through the tree's fold
// (TREE_FOLD), and shows the archived groups when it is in one.
test('a link to an agent unfolds its group and every one above it, archived too', async (t) => {
  const prefs = memPrefs({ 'tclaude.dash.phone.agents.offline': '0', 'tclaude.dash.phone.tree.fold.webshop': '1' });
  const { harness, c, titles } = await setup(t, { prefs });
  const routes = await harness.importDashboardModule('js/overlay-routes.js');
  assert.ok(!titles().includes('stripe-adapter'), 'folded, offline hidden');
  const found = () => c.querySelector('.phone-agent-found .phone-agent-title')?.textContent;
  await harness.act(() => routes.restoreOverlays({ agent: id(5) }));
  await harness.act(() => {});
  assert.equal(found(), 'stripe-adapter', 'its row glows');
  assert.equal(prefs.getItem('tclaude.dash.phone.tree.fold.webshop'), '0', 'its parent unfolded');
  assert.equal(prefs.getItem('tclaude.dash.phone.tree.fold.payments'), '0', 'its own group, folded for nobody online, unfolded');
  assert.equal(prefs.getItem('tclaude.dash.phone.agents.archived'), null);
  await harness.act(() => routes.restoreOverlays({ agent: id(7) }));
  await harness.act(() => {});
  assert.equal(prefs.getItem('tclaude.dash.phone.agents.archived'), '1', 'Show archived on');
  assert.equal(found(), 'release-bot');
});

// List without offline: a parent the tree kept only for an online subgroup
// has nobody listed of its own, so it goes (the subgroup is its own section).
test('List without offline drops a group with nobody listed', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/phone-agents-model.js');
  const snap = { groups: [{ name: 'parent', members: [off(50, 'p')] }, { name: 'child', parent: 'parent', members: [on(51, 'c')] }] };
  const list = model.agentsLayout(snap, (g) => g, { ...model.VIEW_DEFAULTS, layout: 'list', offline: false });
  assert.deepEqual(list.map((n) => n.name), ['child']);
  const tree = model.agentsLayout(snap, (g) => g, { ...model.VIEW_DEFAULTS, offline: false });
  assert.deepEqual(names(tree), [['parent', ['child']]], 'the tree keeps it for its subgroup');
});
