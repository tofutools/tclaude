// js/phone-ui.js, phone-search.js, phone-node-picker.js and phone-input.js:
// the phone layout's shared building blocks, the search model, the node
// chip's model and the touch/keyboard mark.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function load(t, path) {
  const harness = await createPreactHarness(t);
  return { harness, mod: await harness.importDashboardModule(path) };
}

test('the primitives render the mock\'s shapes', async (t) => {
  const { harness, mod } = await load(t, 'js/phone-ui.js');
  const { html } = harness;
  const picked = [];
  const { container } = await harness.mount(html`<div>
    <${mod.Avatar} name="steward" />
    <${mod.Avatar} bot glyph="🤖" badge="🔑" dim />
    <${mod.StatePill} state="asking" />
    <${mod.StatePill} state="crashed">down<//>
    <${mod.Card} accent="key">Approve?<//>
    <${mod.Card} accent="bogus">plain<//>
    <${mod.ScopeBadge} kind="simp">a few<//>
    <${mod.ChipRow} label="Group" value="b" onSelect=${(key) => picked.push(key)}
      items=${[{ key: 'a', label: 'All', count: 9 }, { key: 'b', label: 'webshop', count: 6, badge: 2 }]} />
    <${mod.IconButton} icon="🔍" label="Search" onClick=${() => picked.push('search')} />
    <${mod.ButtonRow} buttons=${[{ label: 'Deny', tone: 'no' }, { label: 'Approve', tone: 'ok' }, { label: '⋯', grow: false }]} />
  </div>`);
  const [person, bot] = container.querySelectorAll('.phone-av');
  assert.equal(person.textContent, 'S');
  assert.equal(person.dataset.hue, String(mod.avatarHue('steward')));
  assert.equal(mod.avatarHue('steward'), mod.avatarHue('steward'), 'stable');
  assert.ok(bot.classList.contains('phone-av-bot') && bot.classList.contains('phone-av-dim'));
  assert.equal(bot.querySelector('.phone-av-badge').textContent, '🔑');
  const pills = [...container.querySelectorAll('.phone-pill')];
  assert.deepEqual(pills.map((p) => [p.className, p.textContent]), [['phone-pill phone-pill-ask', 'asking'], ['phone-pill phone-pill-err', 'down']]);
  const cards = [...container.querySelectorAll('.phone-box')];
  assert.deepEqual(cards.map((c) => c.className), ['phone-box phone-box-key', 'phone-box']);
  assert.equal(container.querySelector('.phone-scope').className, 'phone-scope phone-scope-simp');
  const chips = harness.getByRole(container, 'tablist', { name: 'Group' });
  assert.ok(chips.classList.contains('phone-chips'));
  const webshop = chips.querySelector('[aria-selected="true"]');
  assert.match(webshop.textContent, /webshop 6 2/);
  await harness.act(() => chips.querySelector('[data-tab-key="a"]').click());
  await harness.act(() => harness.getByRole(container, 'button', { name: 'Search' }).click());
  assert.deepEqual(picked, ['a', 'search']);
  const buttons = [...container.querySelectorAll('.phone-btns .ui-btn')];
  assert.deepEqual(buttons.map((b) => b.className.split(' ').filter((c) => c.startsWith('phone-'))),
    [['phone-btn', 'phone-btn-no', 'phone-btn-grow'], ['phone-btn', 'phone-btn-ok', 'phone-btn-grow'], ['phone-btn']]);
});

test('pillTone maps agent states onto the mock\'s pill colours', async (t) => {
  const { mod } = await load(t, 'js/phone-ui.js');
  assert.deepEqual(['asking', 'working', 'idle', 'offline', 'error', 'approval', 'key', 'mystery'].map(mod.pillTone),
    ['ask', 'work', 'idle', 'off', 'err', 'key', 'key', 'idle']);
});

test('searchHits ranks each kind by the palette\'s scoring and caps it', async (t) => {
  const { mod } = await load(t, 'js/phone-search.js');
  const members = Array.from({ length: 7 }, (_, i) => ({ agent_id: `agt_${String(i).padStart(32, '0')}`, title: `worker-${i}`, online: true }));
  const snapshot = {
    groups: [{ name: 'webshop', members: [{ agent_id: `agt_${'9'.repeat(32)}`, title: 'shop-lead', online: true }, ...members] }],
    messages: [{ id: 1, from_title: 'shop-lead', subject: 'Deploy?', body: 'The shop is ready' }],
  };
  const board = { groups: [{ group: 'webshop', tasks: [{ id: 'tk_1', title: 'Shop banner' }] }] };
  assert.deepEqual(mod.searchHits('  ', { snapshot, board }).agents, { total: 0, items: [] }, 'an empty query finds nothing');
  const shop = mod.searchHits('Shop', { snapshot, board });
  assert.deepEqual(shop.agents.items.map((a) => a.name), ['shop-lead']);
  assert.deepEqual(shop.groups.items.map((g) => [g.name, g.online, g.members]), [['webshop', 8, 8]]);
  assert.equal(shop.mail.total, 1, 'a body match finds the message');
  assert.deepEqual(shop.tasks.items.map((task) => [task.id, task.group]), [['tk_1', 'webshop']]);
  assert.deepEqual([
    { online: true, status: 'awaiting_permission' }, { online: true, status: 'awaiting_input' },
    { online: true, status: 'working' }, { online: true, status: '' }, { online: false, status: 'working' },
  ].map(mod.agentState), ['approval', 'asking', 'working', 'idle', 'offline']);
  const workers = mod.searchHits('worker', { snapshot });
  assert.equal(workers.agents.total, 7);
  assert.equal(workers.agents.items.length, mod.SEARCH_LIMIT);
});

test('the node chip names the node on screen, and the picker lists the fleet', async (t) => {
  const { mod } = await load(t, 'js/phone-node-picker.js');
  const self = { id: 'inst_self', name: 'dev@box', color: '#fff', local: true };
  assert.equal(mod.nodeChipView({ fleet: null, node: null }, ''), null, 'nothing before the first status read');
  assert.deepEqual(mod.nodeChipView({ fleet: null, node: { self } }, ''), { name: 'dev@box', color: '#fff', local: true, current: 'inst_self', pickable: false });
  const peers = [{ id: 'inst_p', name: 'laptop', color: '#f0f', online: false, level: 'unrestricted' }];
  const view = { fleet: { self, peers }, node: { self } };
  assert.equal(mod.nodeChipView(view, 'inst_p').name, 'laptop', 'a peer\'s page names the peer');
  const rows = mod.nodeRows(view, 'inst_p');
  assert.deepEqual(rows.map((r) => [r.name, r.current, r.disabled, r.meta]),
    [['dev@box', false, false, 'this node'], ['laptop', true, false, 'offline']], 'the peer on screen stays pickable');
  const opened = [];
  mod.openNode('inst_p', { pathname: '/messages', search: '?nodes=inst_self,inst_p', assign: (url) => opened.push(url) });
  mod.openNode('', { pathname: '/messages', search: '?node=inst_p', assign: (url) => opened.push(url) });
  assert.deepEqual(opened, ['/messages?node=inst_p', '/messages']);
});

test('the input mark follows the last touch or key press', async (t) => {
  const { harness, mod } = await load(t, 'js/phone-input.js');
  const root = harness.document.documentElement;
  const stop = mod.installInputModality(harness.document);
  harness.fireEvent(harness.document.body, 'pointerdown', { pointerType: 'touch' });
  assert.equal(root.dataset.phoneInput, 'touch');
  harness.fireEvent(harness.document.body, 'keydown', { key: 'Shift' });
  assert.equal(root.dataset.phoneInput, 'touch', 'a modifier alone is not keyboard use');
  harness.fireEvent(harness.document.body, 'keydown', { key: 'Tab' });
  assert.equal(root.dataset.phoneInput, 'key');
  harness.fireEvent(harness.document.body, 'pointerdown', { pointerType: 'mouse' });
  assert.equal(root.dataset.phoneInput, 'pointer');
  stop();
  assert.equal(root.dataset.phoneInput, undefined);
});
