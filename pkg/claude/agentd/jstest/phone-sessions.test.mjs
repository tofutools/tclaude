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

async function setup(t, sections) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/phone-sessions.js');
  const opened = [];
  const mounted = await harness.mount(harness.html`<${mod.PhoneSessions} snapshot=${{ value: {} }} actions=${{}}
    listed=${() => sections} open=${(item, ctx) => opened.push([item.key, !!ctx.snapshot])} />`);
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
