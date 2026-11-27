import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// A group's Links are one of its regular sections (FusedSection): the same
// fold, remembered per group, the blue ＋ on the section line, a 🗑 per row
// and the trash-bin drop.

function memoryPrefs(initial = {}) {
  const values = new Map(Object.entries(initial));
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
  };
}

const link = (id, from, to, mode = 'direct') => ({ id, from, to, mode, created_at: '2026-11-20T10:00:00Z' });

function snapshot() {
  return {
    groups: [
      { id: 1, name: 'alpha', members: [] },
      { id: 2, name: 'beta', members: [] },
      { id: 3, name: 'gamma', members: [] },
      { id: 4, name: 'delta', members: [] },
    ],
    pending: [], ungrouped: [], retired: [], conversations: [], replaced: [], paging: {},
    activity_bots: { regular: 'emoji', slop: 'off', wizard: 'off' },
    links: [
      link(1, 'alpha', 'beta'),
      link(2, 'beta', 'alpha', 'cc'),
      link(3, 'beta', 'x1'), link(4, 'beta', 'x2'), link(5, 'x3', 'beta'),
      { id: 8, peer: 'peer-abcdef0123456', peer_label: 'laptop', direction: 'out', from: 'delta', to: 'shared', mode: 'members->members',
        local_state: 'active', remote_state: 'pending' },
      { id: 9, peer: 'peer-abcdef0123456', peer_label: 'laptop', direction: 'in', from: 'theirs', to: 'delta', mode: 'members->members',
        local_state: 'active', remote_state: 'active' },
    ],
  };
}

async function mount(t) {
  const harness = await createPreactHarness(t);
  const [{ createGroupsState }, { GroupsList }, { dashPrefs }, controller, dnd] = await Promise.all([
    harness.importDashboardModule('js/groups-state.js'),
    harness.importDashboardModule('js/groups-island.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/links-controller.js'),
    harness.importDashboardModule('js/resource-dnd.js'),
  ]);
  const deleted = [];
  t.after(controller.registerLinksController({ deleteLink: async (value) => { deleted.push(value); return true; } }));
  const state = createGroupsState({
    prefs: memoryPrefs({ 'tclaude.dash.ungrouped.groups': '0', 'tclaude.dash.retired.groups': '0' }),
    resetOffsets: () => {},
    columns: { list: () => [{ key: 'id', label: 'ID' }], hidden: () => false, setHidden: () => {}, deviationCount: () => 0 },
    reorder: (groups) => groups,
  });
  state.initialize();
  state.publish(snapshot());
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  t.after(() => host.remove());
  await harness.mount(harness.html`<${GroupsList} host=${host} state=${state} actions=${{ sort: () => {}, page: () => {}, setPageSize: () => {} }} />`, host);
  const section = (name) => host.querySelector(`[data-group-links="${name}"]`);
  return { harness, state, host, dashPrefs, dnd, deleted, section };
}

const isOpen = (section) => section.classList.contains('open');

test('Links is a regular group section: open for a few links, folded for more, the head always in reach', async (t) => {
  const { harness, host, section } = await mount(t);
  const alpha = section('alpha');
  assert.equal(alpha.dataset.fusedSection, 'links', 'it is a FusedSection like Agents and the Task board');
  assert.ok(isOpen(alpha), 'two links start open');
  assert.match(alpha.querySelector('.gf-section-head').textContent, /Links\s*2\s*→ 1 · ← 1/);
  assert.equal(alpha.querySelectorAll('tbody tr').length, 2);
  assert.deepEqual([...alpha.querySelectorAll('thead th')].map((th) => th.textContent.replace(/[▾▴]/g, '').trim()).slice(1), ['Other group', 'Direction', 'State', 'Age', 'Mode']);
  const row = alpha.querySelector('tbody tr');
  assert.equal(row.querySelector('[data-act="link-edit"]').dataset.mode, 'direct', 'mode edit stays a row action');
  assert.equal(row.querySelector('[data-act="link-delete"]').dataset.group, 'alpha');

  const beta = section('beta');
  assert.equal(isOpen(beta), false, 'more than three links start folded');
  assert.match(beta.querySelector('.gf-section-head').textContent, /Links\s*5\s*→ 3 · ← 2/);
  const add = beta.querySelector('.gf-section-head [data-act="link-new"]');
  assert.ok(add, 'the blue ＋ rides the folded section line');
  assert.equal(add.dataset.from, 'beta');
  assert.ok(add.classList.contains('spawn-btn'), 'the same add button the other sections use');

  // The group header no longer repeats the links (as chips or a 🔗): the
  // section lists them, and the toolbar's 🔗 links… opens the manager.
  const header = host.querySelector('.group-box[data-group-key="beta"] > .group-summary');
  assert.ok(header);
  assert.equal(header.querySelector('[data-act="links-manage"]'), null);
  assert.doesNotMatch(header.textContent, /🔗|→ alpha/);

  const gamma = section('gamma');
  assert.ok(isOpen(gamma));
  assert.match(gamma.textContent, /No links involving this group\./, 'the empty state keeps its wording');
  assert.ok(gamma.querySelector('.gf-section-head [data-act="link-new"]'), 'open, the ＋ stays on the section line, as on the Task board');
  assert.equal(alpha.querySelectorAll('[data-act="link-new"]').length, 1, 'no extra row just for the ＋');
  assert.equal(alpha.querySelector('.gf-title-row'), null);
  void harness;
});

test('a Links fold is remembered per group and overrides the default', async (t) => {
  const { harness, state, dashPrefs, section } = await mount(t);
  await harness.act(() => harness.fireEvent(section('beta').querySelector('.gf-section-toggle'), 'click'));
  assert.ok(isOpen(section('beta')));
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.fused.links.beta'), '1');
  await harness.act(() => harness.fireEvent(section('alpha').querySelector('.gf-section-toggle'), 'click'));
  assert.equal(isOpen(section('alpha')), false);
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.fused.links.alpha'), '0');
  await harness.act(() => state.publish(snapshot()));
  assert.ok(isOpen(section('beta')), 'the choice survives a new snapshot');
  assert.equal(isOpen(section('alpha')), false);
});

test('a Links row dragged onto the trash bin is removed behind the usual confirmation', async (t) => {
  const { harness, dnd, deleted, section } = await mount(t);
  const doc = harness.document;
  const bin = doc.body.appendChild(doc.createElement('div'));
  bin.id = 'dnd-trash';
  t.after(dnd.bindResourceDnd(doc, { api: {}, notify: () => {}, snapshot: () => snapshot(), refresh: () => {}, confirm: async () => true }));
  const transfer = { setData: () => {}, types: ['application/x-tclaude-resource'], dropEffect: 'none', effectAllowed: 'all' };
  const fire = (type, target) => {
    const event = new harness.window.Event(type, { bubbles: true, cancelable: true });
    Object.defineProperty(event, 'dataTransfer', { value: transfer });
    target.dispatchEvent(event);
    return event;
  };
  // Link #2 is drawn at both ends: opening then folding beta's end
  // unregisters its row, and alpha's row must still reach the bin.
  const betaToggle = () => section('beta').querySelector('.gf-section-toggle');
  await harness.act(() => harness.fireEvent(betaToggle(), 'click'));
  assert.ok(section('beta').querySelector('tr[data-group-link="2"]'));
  await harness.act(() => harness.fireEvent(betaToggle(), 'click'));
  const row = section('alpha').querySelector('tr[data-group-link="2"]');
  assert.equal(JSON.parse(row.dataset.dndResource).kind, 'link');
  fire('dragstart', row);
  assert.ok(bin.classList.contains('show'), 'the bin shows for a link row');
  assert.equal(fire('dragover', section('gamma')).defaultPrevented, false, 'a link does not move to another group');
  assert.equal(fire('dragover', bin).defaultPrevented, true);
  fire('drop', bin);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.equal(deleted.length, 1, 'the 🗑 path: deleteLink confirms, then deletes');
  assert.deepEqual({ id: deleted[0].id, from: deleted[0].from, to: deleted[0].to, scope: deleted[0].scope }, { id: 2, from: 'beta', to: 'alpha', scope: 'beta' },
    'a local link is removed on its FROM, as the Links… manager does');
  assert.equal(bin.classList.contains('show'), false);
});

test('cross-node links show their peer and state, and remove as the Links… manager does', async (t) => {
  const { section } = await mount(t);
  const delta = section('delta');
  const row = (id) => delta.querySelector(`tr[data-group-link="${id}"]`);
  assert.match(row(8).querySelector('.rowname').textContent, /🌐 laptop:shared/);
  assert.equal(row(8).querySelector('[data-link-state]').textContent, 'pending', 'the state the peer reported');
  assert.equal(row(9).querySelector('[data-link-state]').textContent, 'active');
  assert.match(row(9).querySelector('.rowname').textContent, /🌐 laptop:theirs/);
  const edit = row(8).querySelector('.gl-edit');
  assert.ok(edit.disabled, 'a cross-node link keeps the mode both nodes agreed');
  assert.equal(edit.hasAttribute('data-act'), false);
  const withdraw = row(8).querySelector('[data-act="link-delete"]');
  assert.equal(withdraw.dataset.peerLabel, 'laptop');
  assert.equal(withdraw.dataset.pending, '1', 'an unanswered request is withdrawn');
  assert.equal(withdraw.getAttribute('title'), 'Withdraw this link request');
  const remove = row(9).querySelector('[data-act="link-delete"]');
  assert.equal(remove.dataset.group, 'delta', 'an incoming cross-node link is removed on its TO');
  assert.equal(remove.hasAttribute('data-pending'), false);
  assert.equal(section('alpha').querySelector('tr [data-link-state]'), null, 'a local link has no cross-node state');
});
