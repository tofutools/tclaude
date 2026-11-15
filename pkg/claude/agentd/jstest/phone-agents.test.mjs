// js/phone-agents-model.js and js/phone-agents-island.js: the phone Agents
// view (group chips, one row per agent, an agent's sheet, the short spawn
// sheet) driven through what the operator sees and taps.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const id = (n) => `agt_${String(n).padStart(32, '0')}`;

const SNAPSHOT = {
  groups: [
    {
      name: 'webshop',
      members: [
        { agent_id: id(1), conv_id: 'c1', title: 'lead', role: 'lead', online: true, state: { status: 'idle', harness: 'claude', context_pct: 41.6 }, branch: 'main' },
        { agent_id: id(2), conv_id: 'c2', title: 'payments-fix', role: 'dev', online: true, state: { status: 'awaiting_permission', status_detail: 'Allow npm test?', harness: 'codex' },
          branch_pr_number: 1469, branch_pr_url: 'https://example.test/pr/1469', branch_checks: { passed: 3, failed: 1, pending: 1 } },
        { agent_id: id(3), conv_id: 'c3', title: 'old-worker', role: 'dev', online: false, state: {} },
      ],
    },
    { name: 'steward', default_profile: 'companion', members: [{ agent_id: id(4), conv_id: 'c4', title: 'steward', online: true, state: { status: 'working' } }] },
  ],
  profiles: [{ name: 'companion' }, { name: 'reviewer' }, { name: 'off', disabled: true }],
  spawn_profile_default: 'dev',
  steward_active: id(4),
  ungrouped: [{ agent_id: id(5), conv_id: 'c5', title: 'loner', online: false, state: {} }],
};

test('the model: groups in order, loudest first, offline apart, PR and checks', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/phone-agents-model.js');
  const groups = model.agentGroups(SNAPSHOT);
  assert.deepEqual(groups.map((g) => g.name), ['webshop', 'steward', 'Ungrouped']);
  const [webshop] = groups;
  assert.deepEqual(webshop.shown.map((r) => r.name), ['payments-fix', 'lead'], 'asking before idle');
  assert.deepEqual(webshop.offline.map((r) => r.name), ['old-worker']);
  const [asking, lead] = webshop.shown;
  assert.equal(asking.variant, 'asking');
  assert.equal(asking.detail, 'Allow npm test?');
  assert.deepEqual({ number: asking.pr.number, ci: asking.pr.ci }, { number: 1469, ci: 'failing' });
  assert.equal(lead.ctx, 42);
  assert.equal(lead.pr, null);
  assert.equal(lead.branch, 'main');
  assert.deepEqual(model.groupTabs(groups).map((tab) => `${tab.label} ${tab.online}/${tab.total}`),
    ['All 3/5', 'webshop 2/3', 'steward 1/1', 'Ungrouped 0/1']);
  assert.equal(model.shownGroups(groups, [model.groupKey('steward')]).length, 1);
  assert.deepEqual(model.shownGroups(groups, [model.UNGROUPED, model.groupKey('webshop')]).map((g) => g.name), ['webshop', 'Ungrouped'],
    'several groups, in the groups\' order');
  assert.equal(model.shownGroups(groups, ['gone']).length, 3, 'a group that went away shows All');
  assert.deepEqual(model.liveFilter(groups, ['gone', model.UNGROUPED]), [model.UNGROUPED]);
  assert.deepEqual(model.toggleFilter([], 'g:a'), ['g:a']);
  assert.deepEqual(model.toggleFilter(['g:a', 'g:b'], 'g:a'), ['g:b']);
  assert.deepEqual(model.toggleFilter(['g:a'], model.ALL), []);
  assert.deepEqual(webshop.summary.map((x) => `${x.count} ${x.text}`), ['1 approval', '1 idle', '1 offline'], 'loudest first');
  assert.deepEqual(webshop.tree.map((n) => [n.row.name, n.kids.length]), [['payments-fix', 0], ['lead', 0], ['old-worker', 0]],
    'no spawn parents: every agent at the top level');
  assert.equal(model.foldPref(webshop), 'tclaude.dash.phone.fold.webshop');
  assert.equal(model.foldPref(groups[2]), 'tclaude.dash.phone.fold.-ungrouped');
  assert.equal(model.findRow(groups, id(5)).group.key, model.UNGROUPED);
  assert.equal(model.ciState({ passed: 2, pending: 1 }), 'running');
  assert.equal(model.ciState({ passed: 2 }), 'passing');
  assert.equal(model.ciState(null), '');
  // The pill says what the agent waits on.
  assert.deepEqual(asking.pill, { text: 'approval', tone: 'key', badge: '🔑' });
  assert.deepEqual(model.agentPill({ online: true, state: { status: 'awaiting_input' } }), { text: 'asking', tone: 'ask', badge: '❓' });
  assert.deepEqual(model.agentPill({ online: true, state: { status: 'error' } }), { text: 'error', tone: 'err', badge: '💥' });
  assert.deepEqual(lead.pill, { text: 'idle', tone: 'idle', badge: '' });
  assert.deepEqual(webshop.offline[0].pill, { text: 'offline', tone: 'off', badge: '' });
  assert.deepEqual(model.metaParts(lead), ['lead', 'claude', 'main'], 'the context is the meter by the pill');
  assert.deepEqual(model.metaParts(webshop.offline[0]), ['dev', 'resume ›']);
});

test('the model: what the spawn sheet offers and sends', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/phone-agents-model.js');
  assert.deepEqual(model.spawnChoices(SNAPSHOT), {
    groups: [{ name: 'webshop', profile: 'dev' }, { name: 'steward', profile: 'companion' }],
    profiles: ['companion', 'reviewer'],
  });
  assert.deepEqual(model.spawnBody({ prompt: 'Fix the flaky checkout test, please' }),
    { initial_message: 'Fix the flaky checkout test, please' }, 'no name: the daemon and the profile name it');
  assert.deepEqual(model.spawnBody({ prompt: 'x', name: 'my agent', profile: 'reviewer' }),
    { initial_message: 'x', name: 'my-agent', profile: 'reviewer' });
});

test('the model: crashed agents stay in view, group names never collide, an agent in two groups', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/phone-agents-model.js');
  const both = { agent_id: id(6), conv_id: 'c6', title: 'shared', online: true, state: {} };
  const groups = model.agentGroups({ groups: [
    { name: 'all', members: [both, { agent_id: id(7), conv_id: 'c7', title: 'dead', online: false, state: { exit_reason: 'unexpected' } }] },
    { name: 'ungrouped', members: [both] },
  ], ungrouped: [{ agent_id: id(8), conv_id: 'c8', title: 'x', online: true, state: {} }] });
  const [all, named] = groups;
  assert.deepEqual(all.shown.map((r) => r.name), ['shared', 'dead'], 'crashed is not folded away');
  assert.deepEqual({ live: all.live, total: all.total }, { live: 1, total: 2 });
  assert.equal(new Set(groups.map((g) => g.key)).size, 3);
  assert.deepEqual(model.shownGroups(groups, [all.key]), [all], 'a group named all is its own chip');
  assert.equal(model.findRow(groups, id(6), named.key).group, named);
  assert.equal(model.findRow(groups, id(6)).group, all);
});

// settle lets an async submit (its fetch and what follows) finish.
async function settle(harness) {
  for (let i = 0; i < 5; i += 1) await harness.act(async () => { await Promise.resolve(); });
}

// memPrefs stands in for the daemon's dashboard prefs (prefs.js).
function memPrefs(init = {}) {
  const map = new Map(Object.entries(init));
  return { getItem: (k) => (map.has(k) ? map.get(k) : null), setItem: (k, v) => map.set(k, String(v)), removeItem: (k) => map.delete(k), map };
}

async function setup(t, { fetchImpl = async () => ({ ok: true, json: async () => ({}) }), prefs = memPrefs() } = {}) {
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
  const record = (name) => (...args) => { calls.push([name, ...args.map((a) => a?.name ?? a?.key ?? a)]); };
  const actions = {
    openTerminal: record('openTerminal'), message: record('message'), rename: record('rename'), spawn: record('spawn'),
    rowAction: (act, row) => calls.push(['rowAction', act, row.name]),
    grant: record('grant'), copy: (text, what) => calls.push(['copy', text, what]),
  };
  const mounted = await harness.mount(harness.html`<main><${island.PhoneAgents} state=${state} order=${(g) => g} actions=${actions} fetchImpl=${fetchImpl} prefs=${prefs} /></main>
    <${sheet.PhoneSheetHost} />`);
  const c = mounted.container;
  const tap = (element) => harness.act(() => element.click());
  const item = (name) => [...c.querySelectorAll('.phone-agent')].find((li) => li.querySelector('.phone-agent-title').textContent === name);
  const row = (name) => item(name).querySelector('.phone-agent-row');
  const more = (name) => item(name).querySelector('.phone-agent-more');
  // An entry's label is its first line (a second, smaller one may explain it).
  const label = (b) => (b.querySelector('.phone-agent-menu-text > span, .phone-menu-label') || b).textContent;
  const entries = () => [...c.querySelectorAll('.phone-sheet .phone-menu-item')].map(label);
  const tileLabel = (b) => b.querySelector('span:last-child').textContent;
  const tiles = () => [...c.querySelectorAll('.phone-sheet .phone-agent-tile')].map((b) => `${tileLabel(b)}${b.disabled ? ' (off)' : ''}`);
  const menu = (text) => [...c.querySelectorAll('.phone-sheet .phone-menu-item')].find((b) => label(b) === text)
    || [...c.querySelectorAll('.phone-sheet .phone-agent-tile')].find((b) => tileLabel(b) === text);
  const chip = (label) => [...c.querySelectorAll('.phone-agents-chips button')].find((b) => b.firstChild.textContent.trim() === label);
  const titles = () => [...c.querySelectorAll('.phone-agent-title')].map((n) => n.textContent);
  const sections = () => [...c.querySelectorAll('.phone-agents > .phone-section')].map((n) => n.getAttribute('aria-label'));
  return { harness, state, calls, c, tap, item, row, more, menu, entries, tiles, page, board, chip, titles, sections, prefs, model };
}

test('rows: an avatar, a pill, the meta line; offline agents inline; the chips filter', async (t) => {
  const { c, tap, item, row, chip, titles } = await setup(t);
  assert.equal(row('payments-fix').querySelector('.phone-pill').textContent, 'approval');
  assert.ok(row('payments-fix').querySelector('.phone-pill-key'));
  assert.equal(row('payments-fix').querySelector('.phone-av-badge').textContent, '🔑');
  assert.ok(row('payments-fix').textContent.includes('PR #1469 ✗'));
  assert.equal(row('lead').querySelector('.phone-agent-meta').textContent, 'lead · claude · main');
  // The desktop's context meter by the pill: 42% lights two of five, green.
  const meter = row('lead').querySelector('.phone-agent-line > .ctx-meter.ctx-regular');
  assert.equal(meter.querySelectorAll('.ctx-seg.lit-green').length, 2);
  assert.equal(meter.getAttribute('title'), 'context: 42% full');
  assert.ok(meter.nextElementSibling.nextElementSibling.classList.contains('phone-pill'), 'the meter (and its wizard twin) sit before the pill');
  assert.ok(row('old-worker').querySelector('.ctx-meter.ctx-unknown'), 'no usage reported: dimmed');
  assert.equal(row('steward').querySelector('.phone-av').textContent, '✦', 'the steward wears its own glyph');
  // Offline agents sit in their group's list, dimmed, after the online ones.
  const webshop = [...c.querySelector('[aria-label="webshop"]').querySelectorAll('.phone-agent-title')].map((n) => n.textContent);
  assert.deepEqual(webshop, ['payments-fix', 'lead', 'old-worker']);
  assert.ok(item('old-worker').classList.contains('phone-agent-off'));
  assert.ok(row('old-worker').querySelector('.phone-av-dim'));
  assert.equal(row('old-worker').querySelector('.phone-agent-meta').textContent, 'dev · resume ›');
  assert.equal(c.querySelector('details'), null, 'no Offline fold');
  const chips = [...c.querySelectorAll('.phone-agents-chips button')];
  assert.deepEqual(chips.map((b) => b.textContent.replace(/\s+/g, ' ').trim()), ['All 5', 'webshop 3', 'steward 1', 'Ungrouped 1']);
  await tap(chip('steward'));
  assert.deepEqual(titles(), ['steward']);
});

test('the chips filter by any number of groups; All shows every one', async (t) => {
  const { tap, chip, sections, model } = await setup(t);
  const pressed = () => ['All', 'webshop', 'steward', 'Ungrouped'].filter((l) => chip(l).getAttribute('aria-pressed') === 'true');
  assert.deepEqual(pressed(), ['All']);
  await tap(chip('Ungrouped'));
  await tap(chip('webshop'));
  assert.deepEqual(sections(), ['webshop', 'Ungrouped'], 'two groups, in the groups\' order');
  assert.deepEqual(pressed(), ['webshop', 'Ungrouped']);
  await tap(chip('webshop'));
  assert.deepEqual(sections(), ['Ungrouped'], 'a second tap takes a group out');
  await tap(chip('All'));
  assert.deepEqual(sections(), ['webshop', 'steward', 'Ungrouped']);
  assert.deepEqual(pressed(), ['All']);
  assert.deepEqual(model.agentsFilter.value, []);
  // The last picked group taken out shows every group again.
  await tap(chip('steward'));
  await tap(chip('steward'));
  assert.deepEqual(pressed(), ['All']);
});

test('a group\'s caret folds it, remembered in the prefs; its state summary shows folded too; its name opens its page', async (t) => {
  const prefs = memPrefs({ 'tclaude.dash.phone.fold.steward': '1' });
  const { c, tap, titles, prefs: kept, calls, page } = await setup(t, { prefs });
  const head = (name) => c.querySelector(`[aria-label="${name}"] .phone-agents-caret-btn`);
  const nameOf = (name) => c.querySelector(`[aria-label="${name}"] .phone-agents-fold`);
  assert.deepEqual(titles(), ['payments-fix', 'lead', 'old-worker', 'loner'], 'steward starts folded');
  assert.equal(head('steward').getAttribute('aria-expanded'), 'false');
  assert.equal(head('webshop').getAttribute('aria-expanded'), 'true');
  await tap(head('webshop'));
  assert.deepEqual(titles(), ['loner']);
  assert.equal(kept.getItem('tclaude.dash.phone.fold.webshop'), '1');
  const summary = (name) => [...c.querySelectorAll(`[aria-label="${name}"] .phone-agents-sum`)].map((n) => n.textContent.trim());
  assert.deepEqual(summary('webshop'), ['1 approval', '1 idle', '1 offline'], 'the approval still shows');
  assert.ok(c.querySelector('[aria-label="webshop"] .phone-agents-sum.tone-key'), 'in the approval pill\'s colour');
  assert.ok(nameOf('webshop').getAttribute('aria-label').includes('2 of 3 online: 1 approval, 1 idle, 1 offline'));
  assert.equal(nameOf('webshop').querySelector('.phone-agents-count').textContent, '2/3');
  assert.equal(head('webshop').getAttribute('aria-label'), 'Unfold webshop');
  // Board › stays its own button beside the header.
  assert.ok(c.querySelector('[aria-label="webshop\'s board"]'));
  await tap(head('steward'));
  assert.deepEqual(titles(), ['steward', 'loner']);
  assert.equal(kept.getItem('tclaude.dash.phone.fold.steward'), null, 'unfolding forgets it');
  assert.deepEqual(summary('steward'), ['1 working']);
  assert.equal(calls.length, 0, 'folding opens nothing');
  // The name opens the group's page, as in All groups.
  await tap(nameOf('steward'));
  assert.equal(page.phonePage.value.title, 'steward');
  assert.ok(c.querySelector('.phone-group-page'));
  await tap(c.querySelector('.phone-group-page') && { click: () => page.phonePage.value.onBack() });
  assert.equal(c.querySelector('.phone-group-page'), null);
  // The agents in no group have no page: their name folds them.
  assert.equal(nameOf('Ungrouped').getAttribute('aria-expanded'), 'true');
  await tap(nameOf('Ungrouped'));
  assert.ok(!titles().includes('loner'));
});

test('a tap opens an online agent\'s terminal and wakes an offline one; ⋯ opens its menu', async (t) => {
  const { harness, calls, c, tap, row, more, menu, entries, tiles } = await setup(t);
  await tap(row('lead'));
  assert.deepEqual(calls.at(-1), ['openTerminal', 'lead']);
  assert.equal(c.querySelector('.phone-sheet'), null);
  await tap(row('old-worker'));
  assert.deepEqual(calls.at(-1), ['rowAction', 'dot-toggle', 'old-worker']);

  await tap(more('lead'));
  const dialog = c.querySelector('.phone-sheet[role=dialog]');
  assert.equal(dialog.getAttribute('aria-label'), 'lead');
  assert.equal(dialog.querySelector('.phone-agent-sheet-meta').textContent, 'webshop · lead · claude · ctx 42%', 'the exact figure in the header');
  assert.equal(c.querySelector('main').inert, true, 'the page behind is inert');
  assert.deepEqual(tiles(), ['Terminal', 'Message', 'Interrupt (off)', 'Copy id'], 'Interrupt only while it is at work');
  assert.deepEqual(entries(), ['Compact…', 'Restart…', 'Shut down…', 'Rename…', 'Reincarnate…', 'Grant sudo…', 'Copy name', 'Retire…'],
    'Session, then Agent, then Retire');
  assert.ok(dialog.querySelector('.phone-menu-danger').textContent.startsWith('Retire…'));
  await tap(menu('Restart…'));
  assert.deepEqual(calls.at(-1), ['rowAction', 'restart', 'lead']);
  assert.equal(c.querySelector('.phone-sheet'), null, 'the sheet closed');
  assert.equal(c.querySelector('main').inert, false);

  await tap(more('lead'));
  await tap(menu('Rename…'));
  assert.deepEqual(calls.at(-1), ['rename', 'lead', 'webshop']);

  await tap(more('lead'));
  await tap(menu('Copy name'));
  assert.deepEqual(calls.at(-1), ['copy', 'lead', 'lead\'s name']);
  await tap(more('lead'));
  await tap(menu('Copy id'));
  assert.deepEqual(calls.at(-1), ['copy', id(1), 'lead\'s id']);
  await tap(more('lead'));
  await tap(menu('Grant sudo…'));
  assert.deepEqual(calls.at(-1), ['grant', 'lead']);

  // An offline agent: Power on where Terminal was, no Session group (a line
  // says why); Escape closes.
  await tap(more('loner'));
  assert.deepEqual(tiles(), ['Power on', 'Message', 'Interrupt (off)', 'Copy id']);
  assert.deepEqual(entries(), ['Grant sudo…', 'Copy name', 'Retire…'], 'no rename outside a group');
  assert.match(c.querySelector('.phone-sheet .phone-sheet-meta').textContent, /need it running/);
  await tap(menu('Power on'));
  assert.deepEqual(calls.at(-1), ['rowAction', 'dot-toggle', 'loner']);
  await tap(more('loner'));
  await harness.fireEvent(harness.document, 'keydown', { key: 'Escape' });
  assert.equal(c.querySelector('.phone-sheet'), null);
});

test('the sheet follows the snapshot', async (t) => {
  const { harness, state, c, tap, more } = await setup(t);
  await tap(more('lead'));
  await harness.act(() => {
    state.beginRequest();
    state.commitRequest(2, { groups: [], ungrouped: [] });
  });
  assert.ok(c.querySelector('.phone-sheet').textContent.includes('no longer listed'));
});

test('Board › opens the group\'s board', async (t) => {
  const { c, tap, board } = await setup(t);
  const before = board.boardRequest.value;
  await tap(c.querySelector('[aria-label="webshop\'s board"]'));
  assert.equal(board.boardRequest.value, before + 1);
  assert.equal(c.querySelector('[aria-label="Ungrouped\'s board"]'), null, 'no board outside a group');
});

test('＋ in the top bar spawns from a short sheet: group, profile, prompt', async (t) => {
  const sent = [];
  const fetchImpl = async (url, init) => { sent.push([url, JSON.parse(init.body)]); return { ok: true, json: async () => ({ conv_id: 'c9' }) }; };
  const { harness, calls, c, tap, page } = await setup(t, { fetchImpl });
  const bar = page.phoneBar.value;
  assert.equal(bar.tab, 'groups');
  assert.deepEqual(bar.actions.map((a) => a.icon), ['＋']);
  // With one group picked the sheet starts on that group.
  await tap([...c.querySelectorAll('.phone-agents-chips button')][2]);
  await harness.act(() => page.phoneBar.value.actions[0].onClick());
  const form = c.querySelector('.phone-sheet form');
  const [groupSel, profileSel] = form.querySelectorAll('select');
  assert.equal(groupSel.value, 'steward');
  assert.deepEqual([...profileSel.options].map((o) => o.textContent), ['Group default (companion)', 'reviewer']);
  assert.equal(form.querySelector('[type=submit]').disabled, true, 'no prompt yet');
  await harness.input(form.querySelector('textarea'), 'Review the open PRs');
  await harness.act(() => {
    for (const option of profileSel.options) option.toggleAttribute('selected', option.value === 'reviewer');
    profileSel.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  });
  await harness.fireEvent(form, 'submit');
  await settle(harness);
  assert.deepEqual(sent, [['/api/groups/steward/spawn', { initial_message: 'Review the open PRs', profile: 'reviewer' }]]);
  assert.equal(c.querySelector('.phone-sheet'), null, 'the sheet closed');

  // All options… is the desktop form, on the group picked.
  await harness.act(() => page.phoneBar.value.actions[0].onClick());
  await tap([...c.querySelectorAll('.phone-sheet button')].find((b) => b.textContent === 'All options…'));
  assert.deepEqual(calls.at(-1), ['spawn', 'steward']);
});

test('a refused spawn keeps the sheet open', async (t) => {
  const fetchImpl = async () => ({ ok: false, status: 403, text: async () => 'not allowed' });
  const { harness, c, page } = await setup(t, { fetchImpl });
  await harness.act(() => page.phoneBar.value.actions[0].onClick());
  const form = c.querySelector('.phone-sheet form');
  await harness.input(form.querySelector('textarea'), 'go');
  await harness.fireEvent(form, 'submit');
  await settle(harness);
  assert.ok(c.querySelector('.phone-sheet form'), 'still open');
  assert.equal(form.querySelector('[type=submit]').disabled, false);
});

test('the sheet opened from an agent\'s own terminal does not offer that terminal', async (t) => {
  const { harness, state, c, tap, more } = await setup(t);
  const [menuModule, sheet] = await Promise.all([
    harness.importDashboardModule('js/phone-agent-menu.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
  ]);
  t.after(() => sheet.closePhoneSheet());
  await tap(more('lead'));
  const props = { ...sheet.phoneSheet.value.props, here: true };
  await harness.act(() => sheet.openPhoneSheet({ label: 'lead (terminal)', Body: menuModule.AgentMenu, props: { ...props, state } }));
  const terminal = c.querySelector('.phone-sheet .phone-agent-tile[data-act="terminal"]');
  assert.equal(terminal.disabled, true, 'its own terminal is the one open');
});

test('a double tap on Spawn spawns once', async (t) => {
  const sent = [];
  let release;
  const fetchImpl = (url) => { sent.push(url); return new Promise((resolve) => { release = () => resolve({ ok: true, json: async () => ({}) }); }); };
  const { harness, c, page } = await setup(t, { fetchImpl });
  await harness.act(() => page.phoneBar.value.actions[0].onClick());
  const form = c.querySelector('.phone-sheet form');
  await harness.input(form.querySelector('textarea'), 'go');
  form.dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true }));
  form.dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true }));
  await settle(harness);
  assert.equal(sent.length, 1);
  release();
  await settle(harness);
});

test("on a peer's page ＋ is the desktop form, which spawns there", async (t) => {
  globalThis.__tclaudeRemoteNode = { id: 'inst_peer1' };
  t.after(() => { delete globalThis.__tclaudeRemoteNode; });
  const { harness, calls, c, page } = await setup(t);
  await harness.act(() => page.phoneBar.value.actions[0].onClick());
  assert.equal(c.querySelector('.phone-sheet'), null);
  assert.deepEqual(calls.at(-1), ['spawn', '']);
});

test('a row tells a screen reader what the agent waits on', async (t) => {
  const { row } = await setup(t);
  assert.equal(row('payments-fix').getAttribute('aria-label'), 'payments-fix, approval (Allow npm test?): open its terminal');
});

// TREE has a lead that spawned three workers, one of which spawned its own;
// a worker whose spawner is in another group, and one added by a human.
const TREE = {
  groups: [
    {
      name: 'webshop',
      members: [
        { agent_id: id(10), conv_id: 'c10', title: 'lead', role: 'lead', online: true, state: { status: 'idle' } },
        { agent_id: id(11), conv_id: 'c11', title: 'ui', role: 'dev', online: true, state: { status: 'working' }, spawned_by: id(10) },
        { agent_id: id(12), conv_id: 'c12', title: 'payments', role: 'dev', online: true, state: { status: 'error', status_detail: 'boom' }, spawned_by: id(10) },
        { agent_id: id(13), conv_id: 'c13', title: 'reviewer', role: 'reviewer', online: false, state: {}, spawned_by: id(10) },
        { agent_id: id(14), conv_id: 'c14', title: 'helper', role: 'dev', online: true, state: { status: 'working' }, spawned_by: id(11) },
        { agent_id: id(15), conv_id: 'c15', title: 'visitor', role: 'dev', online: true, state: { status: 'working' }, spawned_by: id(99) },
        { agent_id: id(16), conv_id: 'c16', title: 'solo', role: 'dev', online: true, state: { status: 'idle' } },
      ],
    },
  ],
  ungrouped: [],
};

test('the model: workers nest one level under the agent that spawned them', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/phone-agents-model.js');
  const [webshop] = model.agentGroups(TREE);
  assert.deepEqual(webshop.tree.map((n) => [n.row.name, n.kids.map((k) => k.name)]), [
    ['lead', ['payments', 'helper', 'ui', 'reviewer']],
    ['visitor', []],
    ['solo', []],
  ], 'a worker\'s worker sits flat under the same lead; a spawner from elsewhere leaves it at the top; kids loudest first, offline last;'
    + ' the idle lead ranks by its errored worker');
  // An offline lead does not take an asking worker down the list.
  const [quiet] = model.agentGroups({ groups: [{ name: 'g', members: [
    { agent_id: id(20), conv_id: 'c20', title: 'busy', online: true, state: { status: 'working' } },
    { agent_id: id(21), conv_id: 'c21', title: 'gone-lead', online: false, state: {} },
    { agent_id: id(22), conv_id: 'c22', title: 'asker', online: true, state: { status: 'awaiting_input' }, spawned_by: id(21) },
  ] }], ungrouped: [] });
  assert.deepEqual(quiet.tree.map((n) => n.row.name), ['gone-lead', 'busy']);
  // Ungrouped agents nest too.
  const ungrouped = model.agentGroups({ groups: [], ungrouped: [
    { agent_id: id(30), conv_id: 'c30', title: 'boss', online: true, state: { status: 'idle' } },
    { agent_id: id(31), conv_id: 'c31', title: 'minion', online: true, state: { status: 'idle' }, spawned_by: id(30) },
  ] });
  assert.deepEqual(ungrouped[0].tree.map((n) => [n.row.name, n.kids.map((k) => k.name)]), [['boss', ['minion']]]);
  assert.deepEqual(webshop.summary.map((x) => `${x.count} ${x.text}`), ['1 error', '3 working', '2 idle', '1 offline']);
  // A cycle (corrupt lineage) cannot spin: both stay listed.
  const loop = model.agentTree([{ agent: 'a', parent: 'b', name: 'a' }, { agent: 'b', parent: 'a', name: 'b' }]);
  assert.equal(loop.reduce((n, node) => n + 1 + node.kids.length, 0), 2);
});

test('rows: a worker sits under its lead, which says how many it has', async (t) => {
  const { harness, state, c } = await setup(t);
  await harness.act(() => {
    state.beginRequest();
    state.commitRequest(2, TREE);
  });
  const rows = [...c.querySelectorAll('[aria-label="webshop"] .phone-agent')];
  assert.deepEqual(rows.map((li) => `${li.classList.contains('phone-agent-worker') ? '  ' : ''}${li.querySelector('.phone-agent-title').textContent}`),
    ['lead', '  payments', '  helper', '  ui', '  reviewer', 'visitor', 'solo']);
  const lead = rows[0];
  assert.equal(lead.querySelector('.phone-agent-kids').textContent, '4 workers');
  assert.match(lead.querySelector('.phone-agent-meta').textContent, /^lead · 4 workers( · |$)/, 'right after the role, before an ellipsis can cut it');
  assert.ok(lead.querySelector('.phone-agent-row').getAttribute('aria-label').includes('idle, 4 workers'), 'a screen reader hears it too');
  assert.ok(rows[4].classList.contains('phone-agent-last'), 'the last worker ends the guide line');
  assert.equal(rows[1].classList.contains('phone-agent-last'), false);
  assert.ok(rows[1].querySelector('.phone-agent-row').getAttribute('aria-label').includes('spawned by lead'));
  assert.equal(rows[6].querySelector('.phone-agent-kids'), null);
});

test('a touch and hold on a row opens its menu; moving first, or a short tap, does not', async (t) => {
  const { harness, c, row, calls } = await setup(t);
  const { HOLD_MS } = await harness.importDashboardModule('js/phone-hold.js');
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const pointer = (el, type, x = 10, y = 10) => harness.act(() => {
    const event = new harness.window.Event(type, { bubbles: true, cancelable: true });
    Object.assign(event, { button: 0, pointerId: 1, clientX: x, clientY: y });
    el.dispatchEvent(event);
  });
  // A short tap: down and up before HOLD_MS is a tap (the terminal), no menu.
  await pointer(row('lead'), 'pointerdown');
  assert.ok(row('lead').closest('.phone-agent').classList.contains('phone-holding'), 'the cue while it builds');
  await harness.act(() => t.mock.timers.tick(HOLD_MS / 2));
  await pointer(row('lead'), 'pointerup');
  await harness.act(() => t.mock.timers.tick(HOLD_MS));
  assert.equal(c.querySelector('.phone-sheet'), null);
  assert.equal(row('lead').closest('.phone-agent').classList.contains('phone-holding'), false);

  // Moving (a scroll) cancels it.
  await pointer(row('lead'), 'pointerdown');
  await pointer(row('lead'), 'pointermove', 10, 40);
  await harness.act(() => t.mock.timers.tick(HOLD_MS * 2));
  assert.equal(c.querySelector('.phone-sheet'), null, 'a move is a scroll, not a hold');

  // Held still: the menu, and the click the lift makes is swallowed.
  await pointer(row('lead'), 'pointerdown');
  await harness.act(() => t.mock.timers.tick(HOLD_MS));
  assert.equal(c.querySelector('.phone-sheet[role=dialog]').getAttribute('aria-label'), 'lead');
  const before = calls.length;
  await pointer(row('lead'), 'pointerup');
  await harness.act(() => row('lead').click());
  assert.equal(calls.length, before, `the lift does not open the terminal too: ${JSON.stringify(calls.slice(before))}`);
  // A lift landing elsewhere (the sheet's scrim) is swallowed at the
  // document, in the capture phase linkedom does not run: the
  // phone-agent-menu smoke covers it in a browser.
});

test('menuModel: Interrupt only while at work, Compact where the harness can, Open PR when it has one', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ menuModel }, model] = await Promise.all([
    harness.importDashboardModule('js/phone-agent-menu.js'),
    harness.importDashboardModule('js/phone-agents-model.js'),
  ]);
  const group = { group: { name: 'webshop' }, name: 'webshop' };
  const snapshot = { harnesses: [{ name: 'codex', can_compact: true }] };
  const working = model.agentRow({ agent_id: id(7), conv_id: 'c7', title: 'w', online: true, state: { status: 'working', harness: 'codex', codex_app_server: true },
    branch_pr_number: 12, branch_pr_url: 'https://example.test/pr/12' }, 'webshop');
  const m = menuModel(working, group, snapshot);
  assert.equal(m.quick.find((q) => q.key === 'interrupt').disabled, false, 'a Codex turn on the app-server drive');
  const claude = model.agentRow({ ...working.member, state: { status: 'working', harness: 'claude' } }, 'webshop');
  assert.equal(menuModel(claude, group, snapshot).quick.find((q) => q.key === 'interrupt').disabled, true,
    'the daemon interrupts no other turn, so its tile is greyed as an idle one is');
  assert.deepEqual(m.agent.find((e) => e.key === 'pr'), { key: 'pr', label: 'Open PR #12 ›', href: 'https://example.test/pr/12' });
  const external = model.agentRow({ ...working.member, external_steward: true }, 'webshop');
  assert.equal(menuModel(external, group, snapshot).agent.some((e) => e.key === 'reincarnate'), false, 'an external Steward does not reincarnate here');
  const noAgentId = model.agentRow({ conv_id: 'c8', title: 'plain', online: true, state: {} }, '');
  assert.equal(menuModel(noAgentId, { group: null }, {}).agent.some((e) => e.key === 'grant'), false, 'sudo goes to an agent id');
});
