// js/phone-now-shells.js: Now's Shells section on a phone, folded by
// default, the fold remembered, left out while no shell runs.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const NOW = 1_000_000;
const groupShell = { key: 'shell:webshop', kind: 'shell', title: 'webshop shell', view: 'background', bucket: 'background',
  resource: { kind: 'shell', id: 'webshop', group_id: 'g1' },
  shellRow: { kind: 'group_shell', tmux_session: 'tclaude-groupterm-webshop', group: 'webshop', last_activity: NOW - 120, created: NOW - 3 * 3600, killable: true } };
groupShell.shellSession = groupShell.shellRow;
const agentShell = { key: 'shell:agt_a:current', kind: 'shell', title: 'checkout-ui', view: 'background', bucket: 'background',
  resource: { kind: 'shell', id: 'agt_a', agent: 'agt_a', group_id: 'g1' },
  shellRow: { kind: 'shell', tmux_session: 'tclaude-term-a', last_activity: NOW - 40 * 60, created: NOW - 3600 } };
const SECTIONS = [
  { id: 'g1', name: 'webshop', items: [
    { key: 'bg:1', kind: 'terminal', title: 'checkout-ui', view: 'background', status: { agent: { agent_id: 'agt_a' } }, row: { kind: 'agent' } },
    groupShell, agentShell,
    { key: 'shell:closed', kind: 'shell', title: 'old', view: 'closed', bucket: 'closed', resource: { kind: 'shell', id: 'old' } },
  ] },
  { id: 'ungrouped', name: 'Not in a group', items: [
    { key: 'bg::stray', kind: 'terminal', title: 'stray', view: 'background', bucket: 'background', status: null,
      row: { kind: 'group_shell', tmux_session: 'stray', node: { name: 'node-2' } } },
  ] },
];

function memoryStorage(initial = {}) {
  const store = new Map(Object.entries(initial));
  return { store, getItem: (k) => store.get(k) ?? null, setItem: (k, v) => { store.set(k, String(v)); } };
}

async function setup(t, { sections = SECTIONS, storage = memoryStorage(), groups = [{ name: 'webshop' }] } = {}) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/phone-now-shells.js');
  const opened = [];
  const started = [];
  const snapshot = { value: { groups } };
  const mounted = await harness.mount(harness.html`<${mod.NowShells} snapshot=${snapshot} panes=${[]} storage=${storage}
    listed=${() => sections} open=${(item) => opened.push(item.key)} start=${(g) => started.push(g)} />`);
  return { harness, mod, opened, started, storage, c: mounted.container };
}

test('nowShells lists the running shells in the panel order, each saying whose it is, how quiet and how long up', async (t) => {
  const { mod } = await setup(t);
  const rows = mod.nowShells(SECTIONS, NOW);
  assert.deepEqual(rows.map((r) => [r.item.key, r.sub]), [
    ['shell:webshop', 'group shell · idle 2m · up 3h'],
    ['shell:agt_a:current', 'agent shell · webshop · idle 40m · up 1h'],
    ['bg::stray', 'group shell · node-2'],
  ], 'an agent is not a shell, and a closed shell is not running');
  assert.equal(mod.upText(0), '');
  assert.equal(mod.attachCommand(groupShell), "tmux -L tclaude attach -t 'tclaude-groupterm-webshop'");
  assert.equal(mod.attachCommand(SECTIONS[1].items[0]), '', "another node's session is not attached from here");
});

test('folded by default: the head and its count, a tap unfolds the rows and remembers it', async (t) => {
  const { harness, c, storage, opened } = await setup(t);
  const head = c.querySelector('.phone-now-shells .phone-section-fold');
  assert.match(head.textContent, /Shells · 3/);
  assert.equal(head.getAttribute('aria-expanded'), 'false');
  assert.equal(c.querySelectorAll('[data-shell-row]').length, 0, 'folded: no rows');
  await harness.act(() => head.click());
  assert.equal(c.querySelectorAll('[data-shell-row]').length, 3);
  assert.equal(storage.store.get('tclaude.phone.now.shells.open'), '1');
  await harness.act(() => c.querySelector('[data-shell-row] .phone-row').click());
  assert.deepEqual(opened, ['shell:webshop'], 'a tap opens the shell');
});

test('an unfolded section stays unfolded on the next visit', async (t) => {
  const { c } = await setup(t, { storage: memoryStorage({ 'tclaude.phone.now.shells.open': '1' }) });
  assert.equal(c.querySelectorAll('[data-shell-row]').length, 3);
});

test('no shell running: no section', async (t) => {
  const { c } = await setup(t, { sections: [{ id: 'g1', name: 'webshop', items: [SECTIONS[0].items[0]] }] });
  assert.equal(c.querySelector('.phone-now-shells'), null);
});

test('＋ Shell starts the only group\'s shell', async (t) => {
  const { harness, c, started } = await setup(t);
  await harness.act(() => c.querySelector('[data-shell-add]').click());
  assert.deepEqual(started, ['webshop']);
});

test('＋ Shell asks which group when there are more, live ones by name', async (t) => {
  const { harness, c, started } = await setup(t, { groups: [{ name: 'webshop' }, { name: 'docs' }, { name: 'gone', archived: true }] });
  const { phoneSheet } = await harness.importDashboardModule('js/phone-sheet.js');
  await harness.act(() => c.querySelector('[data-shell-add]').click());
  assert.deepEqual(started, []);
  assert.deepEqual(phoneSheet.value.props.groups, ['docs', 'webshop']);
});

test('a row\'s ⋯ menu: Attach, copy the attach command, Kill session… last', async (t) => {
  const { harness, mod } = await setup(t);
  const copied = [];
  const ran = [];
  const ctx = { open: (item) => ran.push(['open', item.key]), confirm: async () => false, notify: () => {} };
  const m = await harness.mount(harness.html`<${mod.ShellMenu} item=${groupShell} ctx=${ctx} copy=${async (text) => { copied.push(text); return true; }} close=${() => {}} />`);
  const actions = [...m.container.querySelectorAll('[data-shell-action]')].map((b) => b.getAttribute('data-shell-action'));
  assert.deepEqual(actions, ['open', 'copy-attach', 'kill']);
  await harness.act(() => m.container.querySelector('[data-shell-action="copy-attach"]').click());
  assert.deepEqual(copied, ["tmux -L tclaude attach -t 'tclaude-groupterm-webshop'"]);
  await harness.act(() => m.container.querySelector('[data-shell-action="open"]').click());
  assert.deepEqual(ran, [['open', 'shell:webshop']]);
});

test('a shell already open as a pane is brought forward; an unattachable one opens its menu', async (t) => {
  const openTab = { ...groupShell, view: 'open-tab', paneKey: 'groupterm:webshop' };
  const locked = { key: 'bg::locked', kind: 'terminal', title: 'locked shell', view: 'background', bucket: 'background', status: null,
    row: { kind: 'group_shell', tmux_session: 'locked' } };
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/phone-now-shells.js');
  const { phoneSheet } = await harness.importDashboardModule('js/phone-sheet.js');
  const calls = [];
  const m = await harness.mount(harness.html`<${mod.NowShells} snapshot=${{ value: { groups: [] } }} panes=${[]}
    storage=${memoryStorage({ 'tclaude.phone.now.shells.open': '1' })}
    listed=${() => [{ id: 'g1', name: 'webshop', items: [openTab, locked] }]}
    open=${(item, ctx) => calls.push([item.key, typeof ctx.actions?.activatePane])} />`);
  const rows = m.container.querySelectorAll('[data-shell-row] .phone-row');
  await harness.act(() => rows[0].click());
  assert.deepEqual(calls, [['shell:webshop', 'function']], 'an open pane has a way to be brought forward');
  await harness.act(() => rows[1].click());
  assert.equal(calls.length, 1, 'no attach for a session without a key');
  assert.equal(phoneSheet.value?.props?.item?.key, 'bg::locked', 'its menu opens instead');
});
