// js/phone-group-links.js: a group's Links on its phone page, the sheet that
// adds one and the sheet that changes or removes one, posting the desktop's
// link routes.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const LINKS = [
  { id: 1, from: 'webshop', to: 'docs', mode: 'members->members' },
  { id: 2, from: 'ops', to: 'webshop', mode: 'owners->members' },
  { id: 3, from: 'docs', to: 'ops', mode: 'members->members' },
  { id: 4, peer: 'peer-abcdef0123456', peer_label: 'laptop', direction: 'out', from: 'webshop', to: 'shared', mode: 'members->members',
    local_state: 'active', remote_state: 'pending' },
  { id: 5, peer: 'peer-abcdef0123456', peer_label: 'laptop', direction: 'in', from: 'theirs', to: 'webshop', mode: 'members->members',
    local_state: 'active', remote_state: 'active' },
];
const SNAPSHOT = { groups: [{ name: 'webshop' }, { name: 'docs' }, { name: 'ops' }], links: LINKS };

// until flushes the harness until check holds, up to a bound of rounds: a
// save awaits its fetch and the refresh before the sheet closes.
async function until(harness, check, what) {
  for (let round = 0; round < 50; round += 1) {
    if (check()) return;
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  }
  assert.fail(`never happened: ${what}`);
}

function recorder(targets = null, refuse = '') {
  const requests = [];
  const fetchImpl = async (url, init = {}) => {
    requests.push([init.method || 'GET', url, init.body ? JSON.parse(init.body) : null]);
    if (url === '/api/federation/link-targets') {
      return targets ? { ok: true, status: 200, json: async () => targets } : { ok: false, status: 404, text: async () => '' };
    }
    if (refuse && init.method && init.method !== 'GET') return { ok: false, status: 403, text: async () => refuse };
    return { ok: true, status: 200, json: async () => ({}) };
  };
  return { requests, fetchImpl, writes: () => requests.filter(([method]) => method !== 'GET') };
}

// pick chooses value in a <select> as LinkeDOM can: by its selected option.
function pick(harness, select, value) {
  [...select.options].forEach((option) => option.removeAttribute('selected'));
  select.querySelector(`option[value="${value}"]`).setAttribute('selected', '');
  return harness.act(() => select.dispatchEvent(new harness.window.Event('change', { bubbles: true })));
}

async function mount(t, { snapshot = SNAPSHOT, readOnly = false, targets = null, refuse = '' } = {}) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/phone-group-links.js');
  const sheets = await harness.importDashboardModule('js/phone-sheet.js');
  t.after(() => sheets.closePhoneSheet());
  const net = recorder(targets, refuse);
  const notes = [];
  let refreshed = 0;
  const deps = { fetchImpl: net.fetchImpl, refresh: async () => { refreshed += 1; }, notify: (text, error) => notes.push([text, !!error]) };
  const mounted = await harness.mount(harness.html`<${mod.GroupLinks} group="webshop" snapshot=${snapshot} readOnly=${readOnly} ...${deps} /><${sheets.PhoneSheetHost} />`);
  const q = (sel) => mounted.container.querySelector(sel);
  const dialog = () => q('[role="dialog"]');
  const button = (label) => [...(dialog()?.querySelectorAll('button') || [])].find((b) => b.textContent.trim() === label);
  return { harness, mod, mounted, q, dialog, button, net, notes, refreshed: () => refreshed };
}

test('the rows: links leaving the group first, then coming in; across nodes named peer:group with their state', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/phone-group-links.js');
  const rows = mod.groupLinks(SNAPSHOT, 'webshop');
  assert.deepEqual(rows.map((r) => [r.link.id, r.out, r.other, r.state]), [
    [1, true, 'docs', ''],
    [4, true, 'laptop:shared', 'pending'],
    [2, false, 'ops', ''],
    [5, false, 'laptop:theirs', 'active'],
  ]);
  assert.equal(mod.deleteFields(LINKS[4]).scope, 'webshop', 'an incoming cross-node link is removed on its TO');
  assert.equal(mod.deleteFields(LINKS[3]).pending, true, 'an unanswered request is withdrawn');
  assert.deepEqual(mod.groupLinks({ links: [] }, 'webshop'), []);
});

test('＋ Link adds a link from the group to another here, with the reverse when asked', async (t) => {
  const { harness, q, dialog, button, net, notes } = await mount(t);
  assert.equal(q('.phone-links .phone-section-head').textContent.includes('Links · 4'), true);
  await harness.act(() => q('.phone-links-add').click());
  await until(harness, () => dialog()?.querySelector('[name="link-to"]'), 'the add sheet');
  assert.equal(dialog().querySelector('.phone-agents-seg'), null, 'no peer choice on a daemon without cross-node links');
  const to = dialog().querySelector('[name="link-to"]');
  assert.deepEqual([...to.options].map((o) => o.value), ['docs', 'ops'], 'not to itself');
  await pick(harness, to, 'ops');
  const mode = dialog().querySelector('[name="link-mode"]');
  await pick(harness, mode, 'owners->members');
  const back = dialog().querySelector('[name="link-bidir"]');
  await harness.act(() => { back.checked = true; harness.fireEvent(back, 'change'); });
  await harness.act(() => button('Link').click());
  await until(harness, () => !dialog(), 'the sheet closes');
  assert.deepEqual(net.writes(), [['POST', '/api/groups/webshop/links', { to: 'ops', mode: 'owners->members', bidir: true }]]);
  assert.match(notes[0][0], /linked: webshop → ops \(\+reverse\)/);
});

test('a refused add stays open with the server\'s reason', async (t) => {
  const { harness, q, dialog, button } = await mount(t, { refuse: 'permission denied: groups.links' });
  await harness.act(() => q('.phone-links-add').click());
  await until(harness, () => button('Link'), 'the add sheet');
  await harness.act(() => button('Link').click());
  await until(harness, () => dialog()?.querySelector('[role="alert"]'), 'the reason');
  assert.match(dialog().querySelector('[role="alert"]').textContent, /permission denied: groups.links/);
  assert.equal(button('Link').disabled, false, 'it can be tried again');
});

test('across nodes: a peer\'s shared group, its approval said before sending', async (t) => {
  const targets = { hub_connected: true, peers: [{ peer: 'peer-1', label: 'laptop', link_capable: true, groups: [{ id: 'g9', name: 'shared', auto: false }] }] };
  const { harness, q, dialog, button, net } = await mount(t, { targets });
  await harness.act(() => q('.phone-links-add').click());
  await until(harness, () => dialog()?.querySelector('.phone-agents-seg'), 'the peer choice');
  await harness.act(() => [...dialog().querySelectorAll('.phone-agents-seg-btn')].find((b) => /Peer/.test(b.textContent)).click());
  assert.match(dialog().querySelector('[data-peer-note]').textContent, /Needs laptop's approval/);
  await harness.act(() => button('Send request').click());
  await until(harness, () => !dialog(), 'the sheet closes');
  assert.deepEqual(net.writes(), [['POST', '/api/groups/webshop/links', { to_peer: 'peer-1', to_group_id: 'g9', mode: 'members->members', bidir: false }]]);
});

test('a link\'s sheet changes its mode, and removes it after the desktop\'s question', async (t) => {
  const { harness, q, dialog, button, net, refreshed } = await mount(t);
  await harness.act(() => q('[data-link-id="1"]').click());
  assert.equal(button('Save').disabled, true, 'nothing to save yet');
  const mode = dialog().querySelector('[name="link-mode"]');
  await pick(harness, mode, 'owners->members');
  await harness.act(() => button('Save').click());
  await until(harness, () => !dialog(), 'the sheet closes');
  assert.deepEqual(net.writes(), [['PATCH', '/api/groups/webshop/links/1', { mode: 'owners->members' }]]);
  assert.equal(refreshed(), 1);

  await harness.act(() => q('[data-link-id="2"]').click());
  await harness.act(() => button('Remove link').click());
  await until(harness, () => dialog()?.querySelector('[role="alertdialog"]'), 'the question');
  assert.match(dialog().querySelector('[role="alertdialog"]').textContent, /Remove this link\?/);
  await harness.act(() => button('Keep').click());
  await until(harness, () => button('Remove link'), 'back to the sheet');
  assert.equal(net.writes().length, 1, 'Keep removes nothing');
  await harness.act(() => button('Remove link').click());
  await until(harness, () => button('Keep'), 'asked again');
  await harness.act(() => [...dialog().querySelectorAll('[role="alertdialog"] button')].at(-1).click());
  await until(harness, () => !dialog(), 'the sheet closes');
  assert.deepEqual(net.writes().at(-1), ['DELETE', '/api/groups/ops/links/2', null]);
});

test('a cross-node link has no mode to change; a pending one is withdrawn on its own side', async (t) => {
  const { harness, q, dialog, button, net } = await mount(t);
  await harness.act(() => q('[data-link-id="4"]').click());
  assert.equal(dialog().querySelector('[name="link-mode"]'), null);
  assert.equal(button('Save'), undefined);
  await harness.act(() => button('Cancel request').click());
  await until(harness, () => dialog()?.querySelector('[role="alertdialog"]'), 'the question');
  assert.match(dialog().textContent, /Cancel this link request\?/);
  await harness.act(() => [...dialog().querySelectorAll('[role="alertdialog"] button')].at(-1).click());
  await until(harness, () => !dialog(), 'the sheet closes');
  assert.deepEqual(net.writes(), [['DELETE', '/api/groups/webshop/links/4', null]]);
});

test('an archived group\'s links can be looked at, not changed', async (t) => {
  const { harness, q, dialog } = await mount(t, { readOnly: true });
  assert.equal(q('.phone-links-add'), null);
  await harness.act(() => q('[data-link-id="1"]').click());
  assert.ok(dialog());
  assert.deepEqual([...dialog().querySelectorAll('button')].map((b) => b.textContent.trim()), ['Close'], 'only a way out');
});

test('a save in flight holds the sheet: Back does not drop it, and the refusal shows', async (t) => {
  const { harness, q, dialog, button, net } = await mount(t, { refuse: 'permission denied: groups.links' });
  const sheets = await harness.importDashboardModule('js/phone-sheet.js');
  await harness.act(() => q('[data-link-id="1"]').click());
  await pick(harness, dialog().querySelector('[name="link-mode"]'), 'owners->members');
  await harness.act(() => button('Save').click());
  await harness.act(() => sheets.requestClosePhoneSheet({ back: true }));
  await until(harness, () => dialog()?.querySelector('[role="alert"]'), 'the refusal');
  assert.match(dialog().querySelector('[role="alert"]').textContent, /permission denied/);
  assert.equal(net.writes().length, 1);
});
