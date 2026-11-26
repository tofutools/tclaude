// js/phone-links-manager.js: the phone's Links manager (the desktop's
// "🔗 links…"): every link, filtered and ordered, a new link with its FROM
// picked, and the peer proposal composer, posting the desktop's routes.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const LINKS = [
  { id: 1, from: 'webshop', to: 'docs', mode: 'members->members', created_at: '2026-11-01T10:00:00Z' },
  { id: 2, from: 'ops', to: 'webshop', mode: 'owners->members', created_at: '2026-11-03T10:00:00Z' },
  { id: 3, from: 'docs', to: 'ops', mode: 'members->members', created_at: '2026-11-02T10:00:00Z' },
  { id: 'xlink_4', peer: 'peer-1', peer_label: 'laptop', direction: 'out', from: 'webshop', to: 'shared', mode: 'members->members',
    local_state: 'active', remote_state: 'pending', created_at: '2026-11-04T10:00:00Z' },
];
const SNAPSHOT = { groups: [{ name: 'webshop', members: [{}, {}] }, { name: 'docs', members: [] }, { name: 'ops', members: [{}] }], links: LINKS };
const TARGETS = { hub_connected: true, peers: [{ peer: 'peer-1', label: 'laptop', link_capable: true, groups: [{ id: 'g9', name: 'shared', auto: false }, { id: 'g8', name: 'infra' }] }] };

async function until(harness, check, what) {
  for (let round = 0; round < 50; round += 1) {
    if (check()) return;
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  }
  assert.fail(`never happened: ${what}`);
}

// pick chooses value in a <select> as LinkeDOM can: by its selected option.
function pick(harness, select, value) {
  [...select.options].forEach((option) => option.removeAttribute('selected'));
  select.querySelector(`option[value="${value}"]`).setAttribute('selected', '');
  return harness.act(() => select.dispatchEvent(new harness.window.Event('change', { bubbles: true })));
}

function tick(harness, box, on = true) {
  return harness.act(() => { box.checked = on; harness.fireEvent(box, 'change'); });
}

async function mount(t, { group = '', targets = TARGETS, remotePage = false } = {}) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/phone-links-manager.js');
  const sheets = await harness.importDashboardModule('js/phone-sheet.js');
  t.after(() => sheets.closePhoneSheet());
  const requests = [];
  const fetchImpl = async (url, init = {}) => {
    requests.push([init.method || 'GET', url, init.body ? JSON.parse(init.body) : null]);
    if (url === '/api/federation/link-targets') return { ok: true, status: 200, json: async () => targets };
    return { ok: true, status: 200, json: async () => ({}) };
  };
  const notes = [];
  const deps = { fetchImpl, refresh: async () => {}, notify: (text) => notes.push(text) };
  const mounted = await harness.mount(harness.html`<${mod.PhoneLinksManager} snapshot=${SNAPSHOT} group=${group} remotePage=${remotePage} ...${deps} /><${sheets.PhoneSheetHost} />`);
  const c = mounted.container;
  const listed = () => [...c.querySelectorAll('[data-links-manager] [data-link-id]')].map((b) => b.dataset.linkId);
  const dialog = () => c.querySelector('[role="dialog"]');
  const button = (label) => [...(dialog()?.querySelectorAll('button') || [])].find((b) => b.textContent.trim() === label);
  const writes = () => requests.filter(([method]) => method !== 'GET');
  return { harness, mod, c, listed, dialog, button, writes, notes };
}

test('every link, newest first; the filter and the order chips narrow and reorder it', async (t) => {
  const { harness, c, listed } = await mount(t);
  assert.deepEqual(listed(), ['xlink_4', '2', '3', '1']);
  assert.match(c.querySelector('[data-link-id="xlink_4"]').textContent, /webshop → laptop:shared/);
  assert.match(c.querySelector('[data-link-id="xlink_4"]').textContent, /pending/);
  const filter = c.querySelector('[name="filter-links"]');
  await harness.act(() => { filter.value = 'owners'; harness.fireEvent(filter, 'input'); });
  assert.deepEqual(listed(), ['2']);
  assert.equal(c.querySelector('.phone-links-count').textContent, '1 of 4');
  await harness.act(() => { filter.value = ''; harness.fireEvent(filter, 'input'); });
  await harness.act(() => [...c.querySelectorAll('.phone-chips [role="tab"]')].find((b) => b.textContent === 'From').click());
  assert.deepEqual(listed(), ['3', '2', '1', 'xlink_4']);
});

test('scoped to a group until its chip is cleared', async (t) => {
  const { harness, c, listed } = await mount(t, { group: 'ops' });
  assert.deepEqual(listed(), ['2', '3']);
  await harness.act(() => c.querySelector('.phone-links-scope').click());
  assert.equal(listed().length, 4);
});

test('＋ New link picks FROM too, and links it to another group', async (t) => {
  const { harness, c, dialog, button, writes } = await mount(t);
  await harness.act(() => c.querySelector('[data-link-new]').click());
  await until(harness, () => dialog()?.querySelector('[name="link-from"]'), 'the add sheet with FROM');
  await pick(harness, dialog().querySelector('[name="link-from"]'), 'ops');
  assert.deepEqual([...dialog().querySelector('[name="link-to"]').options].map((o) => o.value), ['webshop', 'docs'], 'not to itself');
  await harness.act(() => button('Link').click());
  await until(harness, () => !dialog(), 'the sheet closes');
  assert.deepEqual(writes(), [['POST', '/api/groups/ops/links', { to: 'docs', mode: 'members->members', bidir: false }]], 'the TO picked before stays');
});

test('Propose…: groups to share, one with its member list, a ⇄ pair and a note, sent as the desktop composes it', async (t) => {
  const { harness, c, dialog, button, writes, notes } = await mount(t);
  await harness.act(() => c.querySelector('[data-links-manager] [data-link-propose]').click());
  await until(harness, () => dialog()?.querySelector('[data-share="webshop"]'), 'the composer');
  assert.equal(button('Send proposal').disabled, true, 'nothing to send yet');
  await tick(harness, dialog().querySelector('[data-share="webshop"] [name="proposal-share"]'));
  await tick(harness, dialog().querySelector('[data-share="webshop"] [name="proposal-share-members"]'));
  await harness.act(() => dialog().querySelector('[data-pair-add]').click());
  await pick(harness, dialog().querySelector('[data-pair="0"] [name="proposal-pair-theirs"]'), 'g8');
  assert.equal(dialog().querySelector('[data-pair="0"] [name="proposal-pair-dir"]').value, 'both');
  const note = dialog().querySelector('[name="proposal-note"]');
  await harness.act(() => { note.value = 'for the release'; harness.fireEvent(note, 'input'); });
  assert.match(dialog().querySelector('[data-propose-preview]').textContent, /link webshop ⇄ laptop:infra/);
  await harness.act(() => button('Send proposal').click());
  await until(harness, () => !dialog(), 'the sheet closes');
  assert.deepEqual(writes(), [['POST', '/api/federation/proposals', { peer: 'peer-1', note: 'for the release', items: [
    { kind: 'share_visibility', host: 'us', group_id: 'webshop', include_members: true },
    { kind: 'group_link', host: 'them', group_id: 'g8', source: 'us', source_group_id: 'webshop', mode: 'members->members' },
    { kind: 'group_link', host: 'us', group_id: 'webshop', source: 'them', source_group_id: 'g8', mode: 'members->members' },
  ] }]]);
  assert.match(notes[0], /Proposal sent to laptop/);
});

test('a drafted proposal asks before Cancel drops it', async (t) => {
  const { harness, c, dialog, button } = await mount(t);
  await harness.act(() => c.querySelector('[data-links-manager] [data-link-propose]').click());
  await until(harness, () => dialog()?.querySelector('[data-share="docs"]'), 'the composer');
  await tick(harness, dialog().querySelector('[data-share="docs"] [name="proposal-share"]'));
  await harness.act(() => button('Cancel').click());
  assert.ok(dialog().querySelector('.phone-draft-ask'), 'Discard this proposal?');
});

test('the add sheet hands a peer over to the composer', async (t) => {
  const { harness, c, dialog } = await mount(t);
  await harness.act(() => c.querySelector('[data-link-new]').click());
  await until(harness, () => dialog()?.querySelector('.phone-agents-seg'), 'the peer choice');
  await harness.act(() => [...dialog().querySelectorAll('.phone-agents-seg-btn')].find((b) => /Peer/.test(b.textContent)).click());
  await harness.act(() => dialog().querySelector('[data-link-propose]').click());
  await until(harness, () => dialog()?.querySelector('[data-propose]') && dialog().querySelector('[name="proposal-peer"]'), 'the composer');
  assert.equal(dialog().querySelector('[name="proposal-peer"]').value, 'peer-1');
});

test('off the hub, the composer says why', async (t) => {
  const off = await mount(t, { targets: { hub_connected: false, reason: 'hub unreachable', peers: [] } });
  await off.harness.act(() => off.c.querySelector('[data-links-manager] [data-link-propose]').click());
  await until(off.harness, () => /hub/.test(off.dialog()?.querySelector('[data-propose-blocked]')?.textContent || ''), 'why');
  assert.match(off.dialog().querySelector('[data-propose-blocked]').textContent, /need a hub connection\. hub unreachable/);
});

test('on a peer\'s page there is no proposing (its federation is its own)', async (t) => {
  const { c } = await mount(t, { remotePage: true });
  assert.equal(c.querySelector('[data-links-manager] [data-link-propose]'), null);
});
