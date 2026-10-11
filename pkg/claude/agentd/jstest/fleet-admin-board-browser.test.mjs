import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const view = {
  self: { id: 'inst_self', name: 'desk' },
  trusted: [{ id: 'inst_forge', label: 'forge' }],
  waiting: [],
};

const BOARDS = [
  { id: 'brd_ops', name: 'ops notes', role: 'publisher', epoch: 2 },
  { id: 'brd_oss', name: 'oss friends', role: 'reader', epoch: 1 },
];
const item = (id, name, kind, publisher) => ({ id, version: `v_${id}`, latest_version: `v_${id}`, name, kind, publisher, republisher: publisher, digest: 'ab'.repeat(32), bytes: 2048, provenance: { pubkey: 'PUBKEYPUBKEYPUBKEY0123456789' } });
const ITEMS = {
  brd_ops: [item('itm_roles', 'review roles', 'config', 'inst_forge'), item('itm_mine', 'sandbox defaults', 'config', 'inst_self')],
  brd_oss: [item('itm_hunter', 'flaky-test-hunter', 'skill', 'inst_mx')],
};
const SKILL_FILES = [
  { path: 'SKILL.md', bytes: 300, text: '# hunter' },
  { path: 'bin/rerun.sh', bytes: 120, executable: true, security: true, text: '#!/bin/sh\necho <b>hi</b>' },
];
const ROOTS = [
  { id: 'claude', label: 'Claude Code', path: '/home/u/.claude/skills', kind: 'harness' },
  { id: 'codex', label: 'Codex home', path: '/home/u/.codex/skills', kind: 'harness' },
  { id: 'group:tclaude', label: 'Group tclaude', path: '/home/u/git/tclaude/.claude/skills', kind: 'group' },
];

function browserActions(log) {
  return {
    boardItems: async (b) => { log.push(['items', b]); return ITEMS[b] || []; },
    fetchBoardItem: async (b, i, v) => { log.push(['fetch', b, i, v]); return { state: 'ready' }; },
    boardItemContents: async () => ({ entries: [{ path: 'sections/roles.json', size: 80, kind: 'json' }] }),
    boardItemEntry: async (b, i, v, path) => { log.push(['entry', path]); return { text: '[{"name":"<b>reviewer</b>"}]', truncated: false }; },
    previewBoardItem: async (b, i, v, c) => {
      log.push(['preview', c]);
      if (c.custom_dir === '/bad') { const e = new Error('custom_dir: /bad is a symlink'); e.code = 'custom_dir'; throw e; }
      const targets = c.skill_targets || [];
      const roots = c.custom_dir ? [...ROOTS, { id: 'custom', label: 'Custom directory', path: c.custom_dir, kind: 'custom' }] : ROOTS;
      return { kind: 'skill', skill: 'flaky-test-hunter', skill_roots: roots, files: SKILL_FILES, preview_token: `tok${log.length}`,
        changes: targets.map((t) => ({ item: `${t}/flaky-test-hunter`, action: t === 'codex' ? 'replace' : 'create', path: `${roots.find((r) => r.id === t)?.path}/flaky-test-hunter` })) };
    },
    importBoardItem: async (b, i, v, tok, c) => { log.push(['import', b, i, v, tok, c]); return { applied: ['x'] }; },
    downloadBoardItemArchive: async (b, i, v, o) => { log.push(['archive', b, i, v, o]); },
    publishBoardItem: async (b, body) => { log.push(['publish', b, body]); return {}; },
  };
}

async function mount(t, { pref = null } = {}) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/fleet-admin-board-browser.js');
  const log = []; const toasts = []; const confirms = []; const prefs = { v: pref, getItem() { return this.v; }, setItem(k, v) { log.push(['pref', k, v]); this.v = v; } };
  const confirm = async (o) => { confirms.push(o); return o.action(); };
  const actions = browserActions(log);
  const mounted = await harness.mount(harness.html`<${mod.ContentBrowser} boards=${BOARDS} view=${view} actions=${actions} confirm=${confirm} toast=${(m, e) => toasts.push([m, e])} prefs=${prefs} />`);
  const q = (sel) => mounted.container.querySelector(sel);
  const qa = (sel) => [...mounted.container.querySelectorAll(sel)];
  const settle = (ms = 10) => harness.act(() => new Promise((r) => setTimeout(r, ms)));
  const click = async (el) => { await harness.act(() => el.click()); await settle(); };
  const type = async (el, value) => { el.value = value; await harness.act(() => harness.fireEvent(el, 'input')); await settle(); };
  const check = async (el, on = true) => { el.checked = on; await harness.act(() => harness.fireEvent(el, 'change')); await settle(); };
  await settle();
  return { harness, mod, q, qa, log, toasts, confirms, prefs, click, type, check, settle };
}

test('search filters: kind:, publisher: and board: tokens, toggled by facets and tabs', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/fleet-admin-board-browser.js');
  assert.deepEqual(m.parseQuery('kind:Skill hunter publisher:trusted x:y'), { kind: 'skill', publisher: 'trusted', board: '', text: ['hunter', 'x:y'] });
  assert.equal(m.withToken('hunter', 'kind', 'skill'), 'kind:skill hunter');
  assert.equal(m.withToken('kind:skill hunter', 'kind', 'skill'), 'hunter', 'the same facet again clears it');
  assert.equal(m.withToken('kind:skill hunter', 'kind', 'config'), 'kind:config hunter');
  assert.equal(m.withToken('x', 'board', 'oss friends'), 'board:"oss friends" x', 'a name with spaces is quoted');
  assert.deepEqual(m.parseQuery('board:"oss friends" x'), { kind: '', publisher: '', board: 'oss friends', text: ['x'] });
  assert.equal(m.withToken('board:"oss friends" x', 'board', 'oss friends'), 'x', 'and clears again');
  assert.equal(m.withToken('board:"oss friends" x', 'board', 'ops notes'), 'board:"ops notes" x');
  assert.equal(m.trustOf('inst_forge', view), 'trusted');
  assert.equal(m.trustOf('inst_self', view), 'mine');
  assert.equal(m.trustOf('inst_mx', view), 'unknown');
});

test('the list spans every board, marks trust per row, and defaults to List', async (t) => {
  const s = await mount(t);
  assert.equal(s.q('#fleet-content-browser').dataset.view, 'list');
  const rows = s.qa('#fleet-cb-table tbody tr');
  assert.equal(rows.length, 3);
  assert.equal(s.q('[data-row="brd_ops/itm_roles"] [data-trust]').dataset.trust, 'trusted');
  assert.equal(s.q('[data-row="brd_oss/itm_hunter"] [data-trust]').dataset.trust, 'unknown');
  assert.equal(s.q('[data-row="brd_ops/itm_mine"] [data-trust]').dataset.trust, 'mine');
  await s.type(s.q('#fleet-cb-search'), 'publisher:unknown');
  assert.deepEqual(s.qa('#fleet-cb-table tbody tr').map((r) => r.dataset.row), ['brd_oss/itm_hunter']);
  await s.type(s.q('#fleet-cb-search'), '');
  await s.click(s.q('[data-kind-tab="skill"]'));
  assert.equal(s.q('#fleet-cb-search').value, 'kind:skill', 'tabs write the same search');
  assert.equal(s.qa('#fleet-cb-table tbody tr').length, 1);
});

test('Cards remembers the choice; facets filter; a card opens the detail', async (t) => {
  const s = await mount(t);
  await s.click(s.q('[data-view-pick="cards"]'));
  assert.deepEqual(s.log.find((l) => l[0] === 'pref'), ['pref', 'tclaude.dash.boards.contentView', 'cards']);
  assert.equal(s.qa('[data-card]').length, 3);
  await s.click(s.q('[data-facet="board:oss friends"]'));
  assert.deepEqual(s.qa('[data-card]').map((c) => c.dataset.card), ['brd_oss/itm_hunter']);
  assert.ok(s.q('[data-facet="board:oss friends"]').classList.contains('on'), 'the facet shows as active');
  await s.click(s.q('[data-facet="board:ops notes"]'));
  assert.deepEqual(s.qa('[data-card]').map((c) => c.dataset.card), ['brd_ops/itm_roles', 'brd_ops/itm_mine'], 'another board replaces it');
  await s.click(s.q('[data-facet="board:ops notes"]'));
  assert.equal(s.q('#fleet-cb-search').value, '', 'and clicking it again clears the search');
  await s.click(s.q('[data-facet="board:oss friends"]'));
  await s.click(s.q('[data-card="brd_oss/itm_hunter"]'));
  await s.settle(300);
  assert.ok(document.querySelector('#fleet-cb-detail [data-detail="brd_oss/itm_hunter"]'));
});

test('a stored Cards choice is the starting view', async (t) => {
  const s = await mount(t, { pref: 'cards' });
  assert.equal(s.q('#fleet-content-browser').dataset.view, 'cards');
});

test('detail: an unknown publisher gets the banner; scripts are flagged and read as text', async (t) => {
  const s = await mount(t);
  await s.click(s.q('[data-row="brd_oss/itm_hunter"]'));
  await s.settle(300);
  const d = s.q('[data-detail="brd_oss/itm_hunter"]');
  assert.match(d.textContent, /Unknown publisher\..*inst_mx/);
  assert.deepEqual(s.log.filter((l) => l[0] === 'fetch').map((l) => l[2]), ['itm_hunter'], 'one fetch for the selected row');
  assert.deepEqual(s.log.find((l) => l[0] === 'preview'), ['preview', {}], 'a target-less preview lists the files and writes nothing');
  const script = d.querySelector('[data-file="bin/rerun.sh"]');
  assert.match(script.textContent, /executable · security-relevant/);
  await s.click(script.querySelector('button'));
  assert.equal(script.querySelector('pre').textContent, '#!/bin/sh\necho <b>hi</b>');
  assert.equal(script.querySelector('pre b'), null, 'file text is never HTML');
  await s.click(s.q('[data-row="brd_ops/itm_roles"]'));
  await s.settle(300);
  assert.equal(s.q('[data-detail="brd_ops/itm_roles"]').textContent.includes('Unknown publisher'), false, 'a trusted peer gets no banner');
});

test('skill install: target, preview with resolved paths, then install with the same choices', async (t) => {
  const s = await mount(t);
  await s.click(s.q('[data-row="brd_oss/itm_hunter"]'));
  await s.settle(300);
  await s.click(s.q('[data-cb-act="install"]'));
  const dlg = () => document.querySelector('#fleet-cb-install');
  assert.equal(dlg().querySelector('#fleet-cb-preview-btn').disabled, true, 'pick a destination first');
  await s.check(dlg().querySelector('[data-skill-target="codex"]'));
  await s.click(dlg().querySelector('#fleet-cb-preview-btn'));
  assert.deepEqual(s.log.filter((l) => l[0] === 'preview').at(-1)[1], { skill_targets: ['codex'] });
  assert.match(dlg().querySelector('#fleet-cb-preview').textContent, /\/home\/u\/\.codex\/skills\/flaky-test-hunter.*overwrites existing/);
  assert.equal(dlg().querySelector('#fleet-cb-install-btn').disabled, true, 'an overwrite needs the opt-in');
  await s.check(dlg().querySelector('#fleet-cb-replace'));
  assert.equal(dlg().querySelector('#fleet-cb-install-btn').disabled, true, 'a changed option needs a new preview');
  await s.click(dlg().querySelector('#fleet-cb-preview-btn'));
  const token = s.log.length;
  await s.click(dlg().querySelector('#fleet-cb-install-btn'));
  const imp = s.log.find((l) => l[0] === 'import');
  assert.deepEqual(imp.slice(1, 4), ['brd_oss', 'itm_hunter', 'v_itm_hunter']);
  assert.equal(imp[4], `tok${token}`);
  assert.deepEqual(imp[5], { skill_targets: ['codex'], replace: true });
  assert.match(s.confirms.at(-1).body, /a publisher this node has not trusted/);
  assert.match(s.confirms.at(-1).body, /Executable bits are removed/);
});

test('skill install: a group target and a custom dir whose error shows inline', async (t) => {
  const s = await mount(t);
  await s.click(s.q('[data-row="brd_oss/itm_hunter"]'));
  await s.settle(300);
  await s.click(s.q('[data-cb-act="install"]'));
  const dlg = () => document.querySelector('#fleet-cb-install');
  await s.check(dlg().querySelector('[data-target="group"] input[type="radio"]'));
  const sel = dlg().querySelector('#fleet-cb-group');
  [...sel.options].forEach((o) => o.removeAttribute('selected'));
  sel.querySelector('option[value="group:tclaude"]').setAttribute('selected', '');
  await s.harness.act(() => s.harness.fireEvent(sel, 'change'));
  await s.settle();
  assert.match(dlg().textContent, /\/home\/u\/git\/tclaude\/\.claude\/skills/);
  await s.click(dlg().querySelector('#fleet-cb-preview-btn'));
  assert.deepEqual(s.log.filter((l) => l[0] === 'preview').at(-1)[1], { skill_targets: ['group:tclaude'] });
  await s.check(dlg().querySelector('[data-target="custom"] input[type="radio"]'));
  await s.type(dlg().querySelector('#fleet-cb-custom'), '/bad');
  await s.click(dlg().querySelector('#fleet-cb-preview-btn'));
  assert.match(dlg().querySelector('[data-target="custom"]').textContent, /\/bad is a symlink/);
  await s.type(dlg().querySelector('#fleet-cb-custom'), '/srv/skills');
  await s.click(dlg().querySelector('#fleet-cb-preview-btn'));
  assert.deepEqual(s.log.filter((l) => l[0] === 'preview').at(-1)[1], { skill_targets: ['custom'], custom_dir: '/srv/skills' });
  assert.match(dlg().querySelector('#fleet-cb-preview').textContent, /\/srv\/skills\/flaky-test-hunter.*new/);
  assert.equal(dlg().querySelector('#fleet-cb-install-btn').disabled, false);
});

test('download fetches, then saves the archive; executable bits only on opt-in', async (t) => {
  const s = await mount(t);
  await s.click(s.q('[data-row="brd_oss/itm_hunter"]'));
  await s.settle(300);
  await s.click(s.q('[data-cb-act="install"]'));
  const dlg = () => document.querySelector('#fleet-cb-install');
  await s.check(dlg().querySelector('[data-target="download"] input[type="radio"]'));
  await s.click(dlg().querySelector('#fleet-cb-download'));
  assert.deepEqual(s.log.filter((l) => l[0] === 'archive').at(-1), ['archive', 'brd_oss', 'itm_hunter', 'v_itm_hunter', { preserveExecutable: false }]);
  assert.equal(s.log.some((l) => l[0] === 'import'), false, 'a download imports nothing');
});

test('config items install through the board import preview; upload goes to the one postable board', async (t) => {
  const s = await mount(t);
  await s.click(s.q('[data-row="brd_ops/itm_roles"]'));
  await s.settle(300);
  await s.click(s.q('[data-cb-act="install"]'));
  assert.ok(document.querySelector('#fleet-board-import'), 'the existing import preview dialog');
  document.querySelector('#fleet-board-import button:last-child').click();
  await s.settle();
  await s.click(s.q('#fleet-cb-upload-btn'));
  assert.match(document.querySelector('#fleet-board-publish').textContent, /Post config to ops notes/);
});

test('the archive download asks for executable bits only when opted in', async (t) => {
  const harness = await createPreactHarness(t);
  const { createFleetAdminActions } = await harness.importDashboardModule('js/fleet-admin-actions.js');
  const heads = [];
  const fetchImpl = async (url, init) => { heads.push([init.method, url]); return { ok: true, status: 200 }; };
  const a = createFleetAdminActions({ fetchImpl });
  await a.downloadBoardItemArchive('b 1', 'i', 'v', {});
  await a.downloadBoardItemArchive('b 1', 'i', 'v', { preserveExecutable: true });
  assert.deepEqual(heads, [
    ['HEAD', '/api/federation/boards/b%201/items/i/versions/v/archive'],
    ['HEAD', '/api/federation/boards/b%201/items/i/versions/v/archive?preserve_executable=1'],
  ]);
});
