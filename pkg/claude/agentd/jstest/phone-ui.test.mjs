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

test('Section heads a block with its count; SheetHead is a sheet\'s sub-head', async (t) => {
  const { harness, mod } = await load(t, 'js/phone-ui.js');
  const { html } = harness;
  const { container } = await harness.mount(html`<div>
    <${mod.Section} title="Trusted peers" count=${2}><p>a peer</p><//>
    <${mod.SheetHead} class="extra">Move to<//>
  </div>`);
  const section = harness.getByRole(container, 'group', { name: 'Trusted peers' });
  assert.equal(section.querySelector('h2.phone-section-head').textContent, 'Trusted peers · 2');
  assert.equal(section.querySelector('p').textContent, 'a peer');
  const head = container.querySelector('h3');
  assert.equal(head.className, 'ui-eyebrow phone-sheet-subhead extra');
  assert.equal(head.textContent, 'Move to');
});

test('a Card can be the list itself; any other tag stays a div', async (t) => {
  const { harness, mod } = await load(t, 'js/phone-ui.js');
  const { html } = harness;
  const { container } = await harness.mount(html`<div>
    <${mod.Card} as="ul" class="rows"><li>one</li><//>
    <${mod.Card} as="script">safe<//>
  </div>`);
  const [list, other] = container.querySelectorAll('.phone-box');
  assert.equal(list.tagName, 'UL');
  assert.equal(list.className, 'phone-box rows');
  assert.equal(other.tagName, 'DIV');
});

test("RowMore is a row's muted ⋯: a sheet's popup by default, none when it goes to a page", async (t) => {
  const { harness, mod } = await load(t, 'js/phone-ui.js');
  const { html } = harness;
  const taps = [];
  const { container } = await harness.mount(html`<div>
    <${mod.RowMore} label="lead: more" class="phone-agent-more" onClick=${() => taps.push('lead')} />
    <${mod.RowMore} label="Group webshop: more" popup=${null} onClick=${() => taps.push('group')} />
  </div>`);
  const sheet = harness.getByRole(container, 'button', { name: 'lead: more' });
  assert.equal(sheet.getAttribute('aria-haspopup'), 'dialog');
  assert.equal(sheet.className, 'phone-ib phone-row-more phone-agent-more');
  assert.equal(sheet.textContent, '⋯');
  const page = harness.getByRole(container, 'button', { name: 'Group webshop: more' });
  assert.equal(page.hasAttribute('aria-haspopup'), false);
  await harness.act(() => sheet.click());
  await harness.act(() => page.click());
  assert.deepEqual(taps, ['lead', 'group']);
});

test('ChipRow: tabs that keep a link\'s href and hooks, or toggles that several may have on', async (t) => {
  const { harness, mod } = await load(t, 'js/phone-ui.js');
  const { html } = harness;
  const toggled = [];
  const { container } = await harness.mount(html`<div>
    <${mod.ChipRow} label="Views" value="b" items=${[{ key: 'a', label: 'A', href: '/a' }, { key: 'b', label: 'B', class: 'hook', attrs: { 'data-subtab': 'b' } }]} />
    <${mod.ChipRow} toggle label="Groups" onSelect=${(key) => toggled.push(key)}
      items=${[{ key: 'x', label: 'X', count: 3, pressed: true }, { key: 'y', label: 'Y', pressed: true }, { key: 'z', label: 'Z' }]} />
  </div>`);
  const [tabs, toggles] = container.querySelectorAll('.phone-chips');
  assert.equal(tabs.getAttribute('role'), 'tablist');
  assert.equal(tabs.querySelector('a.ui-tab').getAttribute('href'), '/a', 'a link tab keeps its href');
  const b = tabs.querySelector('[data-subtab="b"]');
  assert.ok(b.classList.contains('hook') && b.classList.contains('active'), 'and a tab its class and hooks');
  assert.equal(toggles.getAttribute('role'), 'group');
  assert.equal(toggles.getAttribute('aria-label'), 'Groups');
  const buttons = [...toggles.querySelectorAll('button.ui-tab')];
  assert.deepEqual(buttons.map((el) => el.getAttribute('aria-pressed')), ['true', 'true', 'false'], 'several on at once');
  assert.equal(buttons[0].querySelector('.phone-chip-count').textContent, '3');
  await harness.act(() => buttons[2].click());
  assert.deepEqual(toggled, ['z']);
});

test('Empty and ErrorNote are the one quiet state and the one error of every phone page', async (t) => {
  const { harness, mod } = await load(t, 'js/phone-ui.js');
  const { html } = harness;
  let retried = 0;
  const { container } = await harness.mount(html`<div>
    <${mod.Empty}>Nothing here yet.<//>
    <${mod.ErrorNote} onRetry=${() => { retried += 1; }}>${mod.errorText('the runs', new Error('HTTP 500'))}<//>
    <${mod.ErrorNote} class="phone-compose-error">Could not send.<//>
  </div>`);
  const empty = container.querySelector('p.ui-empty.phone-empty');
  assert.equal(empty.textContent, 'Nothing here yet.');
  const [read, send] = container.querySelectorAll('[role=alert]');
  assert.ok(read.classList.contains('ui-error-box') && read.classList.contains('phone-error'), 'the banner, padded on the phone');
  assert.equal(read.querySelector('.phone-error-text').textContent, 'Could not read the runs: HTTP 500');
  await harness.act(() => harness.getByRole(read, 'button', { name: 'Retry' }).click());
  assert.equal(retried, 1);
  assert.ok(send.classList.contains('phone-compose-error'));
  assert.equal(send.querySelector('button'), null, 'no Retry without a way to try again');
  assert.equal(mod.errorText('the task', ''), 'Could not read the task.');
  assert.equal(mod.errorText('the task', '  not found '), 'Could not read the task: not found');
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
    messages: [{ id: 1, from_agent: `agt_${'9'.repeat(32)}`, from_title: 'shop-lead', subject: 'Deploy?', body: 'The shop is ready' }],
  };
  const board = { groups: [{ group: 'webshop', tasks: [{ id: 'tk_1', title: 'Shop banner' }] }] };
  assert.deepEqual(mod.searchHits('  ', { snapshot, board }).agents, { total: 0, items: [] }, 'an empty query finds nothing');
  const shop = mod.searchHits('Shop', { snapshot, board });
  assert.deepEqual(shop.agents.items.map((a) => a.name), ['shop-lead']);
  assert.deepEqual(shop.groups.items.map((g) => [g.name, g.online, g.members]), [['webshop', 8, 8]]);
  assert.equal(shop.mail.total, 1, 'a body match finds the message');
  const anonymous = { messages: [{ id: 2, subject: 'Shop notice' }] };
  assert.equal(mod.searchHits('shop', { snapshot: anonymous }).mail.total, 0, 'a message with no sender cannot be opened, so it is not offered');
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
  const field = harness.document.body.appendChild(harness.document.createElement('input'));
  harness.fireEvent(field, 'keydown', { key: 'a' });
  assert.equal(root.dataset.phoneInput, 'touch', 'typing in a field (an on-screen keyboard) is not keyboard use');
  harness.fireEvent(field, 'keydown', { key: 'Tab' });
  assert.equal(root.dataset.phoneInput, 'key', 'tabbing out of it is');
  harness.fireEvent(harness.document.body, 'pointerdown', { pointerType: 'touch' });
  harness.fireEvent(harness.document.body, 'keydown', { key: 'Tab' });
  assert.equal(root.dataset.phoneInput, 'key');
  harness.fireEvent(harness.document.body, 'pointerdown', { pointerType: 'mouse' });
  assert.equal(root.dataset.phoneInput, 'pointer');
  stop();
  assert.equal(root.dataset.phoneInput, undefined);
});

test('usePhoneBar shows a view\'s bar while it is active, with its latest handlers, and clears only its own', async (t) => {
  const { harness, mod } = await load(t, 'js/phone-page.js');
  const { html } = harness;
  const calls = [];
  function View({ active, n }) {
    mod.usePhoneBar({ tab: 'messages', menu: { sub: 'Human', label: 'Pick', onClick: () => calls.push(`menu${n}`) },
      actions: [{ key: 'read', icon: '✓✓', label: 'Mark all read', onClick: () => calls.push(`read${n}`) }] }, active);
    return null;
  }
  const mounted = await harness.mount(html`<${View} active=${true} n=${1} />`);
  const shown = mod.phoneBar.value;
  assert.equal(shown.tab, 'messages');
  assert.equal(shown.menu.sub, 'Human');
  await mounted.rerender(html`<${View} active=${true} n=${2} />`);
  assert.equal(mod.phoneBar.value, shown, 'a re-render with the same look keeps the bar');
  shown.actions[0].onClick();
  shown.menu.onClick();
  assert.deepEqual(calls, ['read2', 'menu2'], 'its handlers are the latest');
  await mounted.rerender(html`<${View} active=${false} n=${2} />`);
  assert.equal(mod.phoneBar.value, null, 'an inactive view takes it down');
  await mounted.rerender(html`<${View} active=${true} n=${2} />`);
  const other = { tab: 'groups', actions: [] };
  await harness.act(() => { mod.phoneBar.value = other; });
  await mounted.unmount();
  assert.equal(mod.phoneBar.value, other, 'another view\'s bar is left alone');
});

test('usePhoneBar: a tab under another still finds its own bar, its latest look, and loses it when taken down', async (t) => {
  const { harness, mod } = await load(t, 'js/phone-page.js');
  const { html } = harness;
  function View({ tab, label, on = true }) {
    mod.usePhoneBar({ tab, actions: [{ key: 'a', icon: '＋', label }] }, on);
    return null;
  }
  // Plugins sets its bar, then Agents (mounted out of sight) sets its own.
  const plugins = await harness.mount(html`<${View} tab="plugins" label="Check all" />`);
  const agents = await harness.mount(html`<${View} tab="groups" label="New agent" />`);
  assert.equal(mod.barFor(mod.phoneBar.value, 'plugins').actions[0].label, 'Check all', 'the plugins bar, under Agents\'');
  assert.equal(mod.barFor(mod.phoneBar.value, 'groups').actions[0].label, 'New agent');
  assert.equal(mod.barFor(mod.phoneBar.value, 'messages'), null);

  // A new look of the covered one replaces its old entry, not adds one.
  await plugins.rerender(html`<${View} tab="plugins" label="Checking…" />`);
  await agents.rerender(html`<${View} tab="groups" label="New agent!" />`);
  const tabs = [];
  for (let at = mod.phoneBar.value; at; at = at.covered) tabs.push(at.tab);
  assert.deepEqual(tabs, ['groups', 'plugins'], 'one entry per tab');
  assert.equal(mod.barFor(mod.phoneBar.value, 'plugins').actions[0].label, 'Checking…');
  // Its handlers still reach the view through the copy the chain holds.
  assert.equal(typeof mod.barFor(mod.phoneBar.value, 'plugins').actions[0].onClick, 'function');

  // Taken down while covered, it leaves the chain.
  await plugins.rerender(html`<${View} tab="plugins" label="Checking…" on=${false} />`);
  assert.equal(mod.barFor(mod.phoneBar.value, 'plugins'), null);
  assert.equal(mod.barFor(mod.phoneBar.value, 'groups').actions[0].label, 'New agent!');
  await agents.unmount();
  assert.equal(mod.phoneBar.value, null);
});
