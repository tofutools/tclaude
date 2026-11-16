// js/phone-sessions.js: Collab on a phone, a plain list of the open sessions
// by group, each opening the way the Collab panel opens it.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const SECTIONS = [
  { id: 'g1', name: 'webshop', items: [
    { key: 'bg:1', kind: 'terminal', title: 'checkout-ui', view: 'background', status: { agent: 'a1', key: 'working', symbol: '●', label: 'working' }, row: { kind: 'agent' } },
    { key: 'bg:2', kind: 'terminal', title: 'payments-fix', view: 'background', status: { agent: 'a2', key: 'awaiting_input', symbol: '?', label: 'asking' }, row: { kind: 'agent' } },
    { key: 'db:1', kind: 'drawing_board', title: 'Checkout flow', view: 'closed', resource: { kind: 'drawing_board', id: 'db1' } },
    { key: 'pr:1', kind: 'process', title: 'release-train', view: 'open-tab', resource: { kind: 'process', id: 'p1' } },
  ] },
  { id: 'g2', name: 'docs', items: [
    { key: 'db:2', kind: 'drawing_board', title: 'Old sketch', view: 'closed', resource: { kind: 'drawing_board', id: 'db2' } },
  ] },
  { id: 'other', name: '', items: [
    { key: 'bg:3', kind: 'terminal', title: 'scratch', view: 'background', status: null, row: { kind: 'shell' } },
  ] },
];

async function setup(t, sections, { onMenu } = {}) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/phone-sessions.js');
  const opened = [];
  const mounted = await harness.mount(harness.html`<${mod.PhoneSessions} snapshot=${{ value: {} }} actions=${{}}
    listed=${() => sections} open=${(item, ctx) => opened.push([item.key, !!ctx.snapshot])} onMenu=${onMenu} />`);
  return { harness, mod, opened, c: mounted.container };
}

test('openSessions keeps the sessions running, in order, and drops empty groups', async (t) => {
  const { mod } = await setup(t, []);
  const kept = mod.openSessions(SECTIONS);
  assert.deepEqual(kept.map((s) => [s.id, s.items.map((i) => i.key)]), [['g1', ['bg:1', 'bg:2']], ['other', ['bg:3']]],
    'the sessions running; a drawing board or process is not a session, open or not');
  assert.deepEqual(mod.sessionKind(SECTIONS[0].items[0]), { glyph: '', word: 'agent' });
  assert.equal(mod.sessionKind(SECTIONS[2].items[0]).word, 'shell');
});

test('a running group or agent shell (a resource of its group) lists as a shell; a closed one does not', async (t) => {
  const { mod } = await setup(t, []);
  const shell = { key: 'shell:webshop', kind: 'shell', title: 'webshop shell', view: 'background', bucket: 'background', resource: { kind: 'shell', id: 'webshop' } };
  const closed = { ...shell, key: 'shell:old', bucket: 'closed', view: 'closed' };
  const kept = mod.openSessions([{ id: 'g', name: 'webshop', items: [shell, closed] }]);
  assert.deepEqual(kept.map((s) => s.items.map((i) => i.key)), [['shell:webshop']]);
  assert.deepEqual(mod.sessionKind(shell), { glyph: '›', word: 'shell' });
});

test('a group per section with its open sessions; a tap opens one', async (t) => {
  const { harness, opened, c } = await setup(t, SECTIONS);
  const groups = [...c.querySelectorAll('[role="group"]')].map((g) => g.getAttribute('aria-label'));
  assert.deepEqual(groups, ['webshop', 'Other'], 'a group with nothing open is left out; a nameless one is Other');
  const rows = [...c.querySelectorAll('.phone-session')];
  assert.deepEqual(rows.map((r) => r.querySelector('.phone-session-title').textContent), ['checkout-ui', 'payments-fix', 'scratch']);
  assert.equal(rows[0].querySelector('.phone-pill').textContent, 'working');
  assert.equal(rows[1].querySelector('.phone-pill').textContent, 'needs you', 'an agent waiting says so');
  assert.equal(rows[2].querySelector('.phone-pill'), null, 'a shell has no state');
  assert.equal(rows[2].querySelector('.phone-session-sub').textContent, 'shell');
  await harness.act(() => rows[1].click());
  assert.deepEqual(opened, [['bg:2', true]], 'the item opens with the panel\'s context');
});

test('nothing open says so', async (t) => {
  const { c } = await setup(t, [SECTIONS[1]]);
  assert.equal(c.querySelectorAll('.phone-session').length, 0);
  assert.match(c.querySelector('.phone-sessions-empty').textContent, /Nothing is open/);
});

test('the empty strip: a phone lists the open sessions, the desktop keeps its home or hint', async (t) => {
  const harness = await createPreactHarness(t);
  const { HomeOrHint } = await harness.importDashboardModule('js/terminal-shell-island.js');
  const snapshot = harness.signals.signal({ groups: [], group_resources: [] });
  const phone = await harness.mount(harness.html`<${HomeOrHint} phone=${true} rail=${true} snapshot=${snapshot} actions=${{}} panes=${[]}><p class="hint">hint</p><//>`);
  assert.ok(phone.container.querySelector('.phone-sessions'), 'a phone shows the list');
  assert.equal(phone.container.querySelector('.hint'), null);
  const desk = await harness.mount(harness.html`<${HomeOrHint} phone=${false} rail=${false} snapshot=${snapshot} actions=${{}} panes=${[]}><p class="hint">hint</p><//>`);
  assert.equal(desk.container.querySelector('.phone-sessions'), null, 'the desktop never shows it');
  assert.ok(desk.container.querySelector('.hint'), 'the desktop keeps the hint');
});

test('a local agent\'s row has a ⋯, and a hold, for its menu; another node\'s agent has neither', async (t) => {
  const agent = (key, title, extra = {}) => ({ key, kind: 'terminal', title, view: 'background',
    status: { agent: { agent_id: `agt_${key}`, conv_id: `c_${key}` }, key: 'idle', label: 'idle' }, row: { kind: 'agent', ...extra } });
  const menus = [];
  const { harness, c } = await setup(t, [{ id: 'g', name: 'webshop', items: [
    agent('a', 'checkout-ui'), agent('b', 'far-away', { node: { name: 'node-2' } }),
    { key: 'bg:3', kind: 'terminal', title: 'scratch', view: 'background', status: null, row: { kind: 'shell' } },
  ] }], { onMenu: (who) => menus.push(who) });
  assert.deepEqual([...c.querySelectorAll('.phone-session-more')].map((b) => b.getAttribute('aria-label')), ['checkout-ui: more', 'scratch: more']);
  await harness.act(() => c.querySelector('.phone-session-more').click());
  assert.deepEqual(menus, [{ agentId: 'agt_a', name: 'checkout-ui' }]);
  const { HOLD_MS } = await harness.importDashboardModule('js/phone-hold.js');
  t.mock.timers.enable({ apis: ['setTimeout'] });
  await harness.act(() => {
    const event = new harness.window.Event('pointerdown', { bubbles: true, cancelable: true });
    Object.assign(event, { button: 0, pointerId: 1, clientX: 5, clientY: 5 });
    c.querySelector('.phone-session').dispatchEvent(event);
  });
  await harness.act(() => t.mock.timers.tick(HOLD_MS));
  assert.equal(menus.length, 2, 'a hold opens it too');
});

test('a shell\'s row opens the shell menu (Now\'s Shells\') on a hold or its ⋯', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/phone-sessions.js');
  const shell = { key: 'bg:3', kind: 'terminal', title: 'scratch', view: 'background', status: null, row: { kind: 'shell', key: 'k3' } };
  const menus = [];
  const opened = [];
  const m = await harness.mount(harness.html`<${mod.PhoneSessions} snapshot=${{ value: {} }} actions=${{}}
    listed=${() => [{ id: 'g', name: 'webshop', items: [shell] }]} open=${(item) => opened.push(item.key)} onMenu=${() => {}}
    shellMenu=${(item, { open }) => { menus.push(item.key); open(item); }} />`);
  const c = m.container;
  await harness.act(() => c.querySelector('.phone-session-more').click());
  assert.deepEqual(menus, ['bg:3']);
  assert.deepEqual(opened, ['bg:3'], 'the menu\'s Attach opens it the way a tap does');
  opened.length = 0;
  const { HOLD_MS } = await harness.importDashboardModule('js/phone-hold.js');
  t.mock.timers.enable({ apis: ['setTimeout'] });
  await harness.act(() => {
    const event = new harness.window.Event('pointerdown', { bubbles: true, cancelable: true });
    Object.assign(event, { button: 0, pointerId: 1, clientX: 5, clientY: 5 });
    c.querySelector('.phone-session').dispatchEvent(event);
  });
  await harness.act(() => t.mock.timers.tick(HOLD_MS));
  assert.equal(menus.length, 2, 'a hold opens it too');
  assert.deepEqual(opened, []);
});
