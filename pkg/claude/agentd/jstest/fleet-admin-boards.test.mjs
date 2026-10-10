import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const view = {
  self: { id: 'inst_self', name: 'desk', hubURL: 'wss://hub.example' },
  trusted: [{ id: 'inst_forge', label: 'forge' }],
  waiting: [],
};

const invite = (hub) => `board1_${Buffer.from(JSON.stringify({ hub, board: 'brd_1', secret: 's', key: 'k' })).toString('base64url')}`;

const ITEMS = [
  { id: 'itm_1', version: 'v2bbbbbbbbbbbb', name: 'review roles', kind: 'config', publisher: 'inst_forge', republisher: 'inst_forge', digest: 'ab'.repeat(32), bytes: 2048, latest_version: 'v2bbbbbbbbbbbb', pinned_version: 'v1aaaaaaaaaaaa', update_available: true },
  { id: 'itm_bad', version: 'vx', name: 'Invalid item metadata', invalid: true, error: 'publisher metadata could not be verified' },
];

function boardActions(log, { joinError = null } = {}) {
  let boards = [{ id: 'brd_ops', name: 'ops notes', role: 'owner', epoch: 2, frozen: false, quota_bytes: 256 << 20, max_members: 100, max_versions: 10 }];
  return {
    boards: async () => { log.push(['boards']); return boards; },
    joinBoard: async (token) => {
      log.push(['join', token]);
      if (joinError) { const e = new Error(joinError.error); e.code = joinError.code; e.status = 409; throw e; }
      boards = [...boards, { id: 'brd_new', name: 'designs', role: 'reader' }];
      return boards.at(-1);
    },
    createBoard: async (name) => { log.push(['create', name]); return { id: 'brd_c', name, role: 'owner', epoch: 1 }; },
    leaveBoard: async (id) => { log.push(['leave', id]); return true; },
    boardMembers: async (id) => { log.push(['members', id]); return [{ instance: 'inst_self', role: 'owner', pubkey: 'AAAA', key_proof: 'BBBB' }, { instance: 'inst_forge', role: 'reader' }, { instance: 'inst_zed', role: 'publisher' }]; },
    setBoardMember: async (b, i, r) => { log.push(['role', b, i, r]); return true; },
    removeBoardMember: async (b, i) => { log.push(['remove', b, i]); return true; },
    createBoardInvite: async (b, role, ttl) => { log.push(['invite', b, role, ttl]); return { token: 'board1_SECRET', token_id: 'tid_9', expires_at: '2026-10-10T15:00:00Z' }; },
    revokeBoardInvite: async (b, id) => { log.push(['revoke', b, id]); return true; },
    rotateBoardKey: async (b) => { log.push(['rotate', b]); return { epoch: 3 }; },
    boardItems: async (b) => { log.push(['items', b]); return b === 'brd_ops' ? ITEMS : []; },
    boardItemVersions: async (b, i) => { log.push(['versions', b, i]); return [{ ...ITEMS[0], version: 'v2bbbbbbbbbbbb' }, { ...ITEMS[0], version: 'v1aaaaaaaaaaaa' }]; },
    publishBoardItem: async (b, body) => { log.push(['publish', b, body]); return { board: b, item: 'itm_2', version: 'v1', blob: 'x', signature: 'sig' }; },
    pinBoardItem: async (b, i, v) => { log.push(['pin', b, i, v]); return true; },
    fetchBoardItem: async (b, i, v) => { log.push(['fetch', b, i, v]); return { state: 'ready', version: v }; },
    boardItemContents: async () => ({ type: 'config', entries: [{ path: 'roles/reviewer.json', size: 120, kind: 'json' }] }),
    boardItemEntry: async (b, i, v, path) => { log.push(['entry', path]); return { kind: 'json', size: 20, text: '{"name":"<b>reviewer</b>"}', truncated: false }; },
    downloadBoardItem: async (b, i, v) => { log.push(['download', b, i, v]); },
    previewBoardItem: async (b, i, v, c) => { log.push(['preview', c]); return { changes: [{ item: 'roles/reviewer', action: 'replace', security: true }, { item: 'roles/writer', action: 'create' }], unresolved: [], applied: [], preview_token: `tok${log.length}` }; },
    importBoardItem: async (b, i, v, tok, c) => { log.push(['import', b, i, v, tok, c]); return { applied: ['roles/writer'] }; },
  };
}

async function mount(t, opts = {}) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/fleet-admin-boards.js');
  const log = []; const toasts = []; const confirms = [];
  const confirm = async (o) => { confirms.push(o); return o.action(); };
  const actions = boardActions(log, opts);
  const mounted = await harness.mount(harness.html`<${mod.BoardsPage} view=${view} actions=${actions} confirm=${confirm} toast=${(m, e) => toasts.push([m, e])} copy=${async () => {}} />`);
  const q = (sel) => mounted.container.querySelector(sel);
  const settle = () => harness.act(() => new Promise((r) => setTimeout(r, 10)));
  const click = async (el) => { await harness.act(() => el.click()); await settle(); };
  const type = async (el, value) => { el.value = value; await harness.act(() => harness.fireEvent(el, 'input')); };
  await settle();
  return { harness, mod, mounted, q, log, toasts, confirms, click, type, settle };
}

test('joining is pasting an invite; boards show plain roles and the hub-trust sentence', async (t) => {
  const s = await mount(t);
  assert.match(s.q('#fleet-boards').textContent, /cannot read anything or fake a post\. Use a hub you trust\./);
  assert.match(s.q('#fleet-boards-list').textContent, /ops notes.*owner/);
  assert.equal(s.q('#fleet-board-join-btn').disabled, true);
  await s.type(s.q('#fleet-board-token'), `  ${invite('wss://hub.example')}  `);
  await s.click(s.q('#fleet-board-join-btn'));
  assert.deepEqual(s.log.find((l) => l[0] === 'join'), ['join', invite('wss://hub.example')]);
  assert.match(s.toasts.at(-1)[0], /Joined designs \(can read\)/);
  assert.match(s.q('#fleet-boards-list').textContent, /designs.*can read/);
  assert.equal(s.q('#fleet-board-detail').dataset.board, 'brd_new', 'the joined board opens');
  assert.equal(s.q('#fleet-board-invite'), null, 'a reader cannot invite');
  assert.equal(s.mounted.container.textContent.includes('AAAA'), false, 'protocol keys are never shown');
});

test('a refused join says what to do, naming the invite\'s hub', async (t) => {
  const s = await mount(t, { joinError: { code: 'hub_mismatch', error: 'invitation belongs to a different hub' } });
  await s.type(s.q('#fleet-board-token'), invite('wss://other.example'));
  await s.click(s.q('#fleet-board-join-btn'));
  assert.match(s.q('#fleet-board-join-error').textContent, /hub at wss:\/\/other\.example, but this node uses wss:\/\/hub\.example/);
  assert.equal(s.mod.inviteHub('garbage'), '');
  assert.match(s.mod.joinError({ code: 'board_invite' }), /expired, already used/);
  assert.match(s.mod.joinError({ code: 'hub_required' }), /Set this node's hub first/);
});

test('an owner invites (token once, cancellable), changes roles, removes members and changes the key', async (t) => {
  const s = await mount(t);
  await s.click(s.q('[data-board-id="brd_ops"] [data-board="open"]'));
  const rows = s.q('#fleet-board-members').textContent;
  assert.match(rows, /this node inst_self/); assert.match(rows, /forge inst_forge/); assert.match(rows, /inst_zed/);
  assert.equal(s.q('[data-member="inst_self"] [data-board="remove"]'), null, 'leaving is how this node goes');
  const role = s.q('#fleet-board-invite-role');
  for (const o of role.querySelectorAll('option')) { if (o.value === 'publisher') o.setAttribute('selected', ''); else o.removeAttribute('selected'); }
  await s.harness.act(() => s.harness.fireEvent(role, 'change'));
  await s.click(s.q('#fleet-board-invite-create'));
  assert.match(s.confirms.at(-1).body, /join ops notes once, within 1 hour, and post to it.*no access to this node.*shown only once/);
  assert.deepEqual(s.log.find((l) => l[0] === 'invite'), ['invite', 'brd_ops', 'publisher', 3600]);
  assert.match(s.q('#fleet-board-new-invite').textContent, /board1_SECRET/);
  await s.click(s.q('#fleet-board-invite-cancel'));
  assert.deepEqual(s.log.find((l) => l[0] === 'revoke'), ['revoke', 'brd_ops', 'tid_9']);
  assert.equal(s.q('#fleet-board-new-invite'), null);
  const sel = s.q('[data-member="inst_forge"] select');
  for (const o of sel.querySelectorAll('option')) { if (o.value === 'owner') o.setAttribute('selected', ''); else o.removeAttribute('selected'); }
  await s.harness.act(() => s.harness.fireEvent(sel, 'change'));
  await s.settle();
  assert.match(s.confirms.at(-1).body, /becomes an owner.*including you/);
  assert.deepEqual(s.log.find((l) => l[0] === 'role'), ['role', 'brd_ops', 'inst_forge', 'owner']);
  await s.click(s.q('[data-member="inst_zed"] [data-board="remove"]'));
  assert.match(s.confirms.at(-1).body, /loses access to ops notes now.*change the key afterwards/);
  await s.click(s.q('#fleet-board-rotate'));
  assert.match(s.confirms.at(-1).body, /only the current members get.*Unused invites stop working/);
  assert.deepEqual(s.log.find((l) => l[0] === 'rotate'), ['rotate', 'brd_ops']);
  await s.click(s.q('[data-board-id="brd_ops"] [data-board="leave"]'));
  assert.match(s.confirms.at(-1).body, /last owner cannot leave/);
});

test('hub moderation freezes, limits and deletes boards with the consequence spelled out', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/fleet-admin-boards.js');
  const log = []; const confirms = [];
  const actions = {
    hubBoards: async () => [{ id: 'brd_ops', name: 'ops notes', role: '', frozen: false, quota_bytes: 256 << 20, max_members: 100, max_versions: 10 }],
    patchHubBoard: async (b, p) => { log.push(['patch', b, p]); return true; },
    deleteHubBoard: async (b) => { log.push(['delete', b]); return true; },
  };
  const confirm = async (o) => { confirms.push(o); return o.action(); };
  const mounted = await harness.mount(harness.html`<${mod.HubBoards} actions=${actions} confirm=${confirm} toast=${() => {}} />`);
  const settle = () => harness.act(() => new Promise((r) => setTimeout(r, 10)));
  await settle();
  const q = (sel) => mounted.container.querySelector(sel);
  assert.match(q('#fleet-hub-boards').textContent, /ops notes.*brd_ops.*100 members · 256 MiB · 10 versions/);
  await harness.act(() => q('[data-hub-board-act="freeze"]').click()); await settle();
  assert.match(confirms.at(-1).body, /Posting to and joining ops notes stop.*Nothing is deleted/);
  assert.deepEqual(log.at(-1), ['patch', 'brd_ops', { frozen: true }]);
  await harness.act(() => q('[data-hub-board-act="limits"]').click()); await settle();
  const members = q('input[aria-label="members"]'); members.value = '20';
  await harness.act(() => harness.fireEvent(members, 'input'));
  await harness.act(() => q('[data-hub-board-act="save"]').click()); await settle();
  assert.deepEqual(log.at(-1), ['patch', 'brd_ops', { quota_bytes: 256 << 20, max_members: 20, max_versions: 10 }]);
  assert.match(confirms.at(-1).body, /never removes members or content/);
  await harness.act(() => q('[data-hub-board-act="delete"]').click()); await settle();
  assert.match(confirms.at(-1).body, /deleted for every member, and cannot be brought back/);
  assert.deepEqual(log.at(-1), ['delete', 'brd_ops']);
});

test('the board client sends the PR A routes', async (t) => {
  const harness = await createPreactHarness(t);
  const { createFleetAdminActions } = await harness.importDashboardModule('js/fleet-admin-actions.js');
  const calls = [];
  const fetchImpl = async (url, init) => {
    calls.push([init.method, url, init.body ? JSON.parse(init.body) : null]);
    if (url === '/api/federation/boards') return { ok: true, status: 200, json: async () => ({ boards: [{ id: 'a' }], next_cursor: 'c2' }) };
    if (url === '/api/federation/boards?cursor=c2') return { ok: true, status: 200, json: async () => ({ boards: [{ id: 'b' }], next_cursor: '' }) };
    return { ok: true, status: 200, json: async () => ({ ok: true }) };
  };
  const a = createFleetAdminActions({ fetchImpl });
  assert.deepEqual((await a.boards()).map((b) => b.id), ['a', 'b'], 'pages are read whole');
  await a.joinBoard('board1_x');
  await a.createBoardInvite('brd 1', 'reader', 3600);
  await a.setBoardMember('brd', 'inst_f', 'publisher');
  await a.rotateBoardKey('brd');
  await a.patchHubBoard('brd', { frozen: true });
  await a.publishBoardItem('brd', { name: 'n', only: ['roles'] });
  await a.fetchBoardItem('brd', 'itm', 'v1');
  await a.boardItemEntry('brd', 'itm', 'v1', 'roles/x.json', 0, 100);
  await a.previewBoardItem('brd', 'itm', 'v1', { skip: ['roles/y'] });
  await a.importBoardItem('brd', 'itm', 'v1', 'tok', { skip: ['roles/y'] });
  await a.pinBoardItem('brd', 'itm', 'v1');
  assert.deepEqual(calls.slice(2), [
    ['POST', '/api/federation/boards/join', { token: 'board1_x' }],
    ['POST', '/api/federation/boards/brd%201/invites', { role: 'reader', ttl_seconds: 3600 }],
    ['PUT', '/api/federation/boards/brd/members/inst_f', { role: 'publisher' }],
    ['POST', '/api/federation/boards/brd/rotate-key', {}],
    ['PATCH', '/api/federation/hub/boards/brd', { frozen: true }],
    ['POST', '/api/federation/boards/brd/items', { name: 'n', only: ['roles'] }],
    ['POST', '/api/federation/boards/brd/items/itm/versions/v1/fetch', {}],
    ['GET', '/api/federation/boards/brd/items/itm/versions/v1/contents?path=roles%2Fx.json&offset=0&max_bytes=100', null],
    ['POST', '/api/federation/boards/brd/items/itm/versions/v1/preview', { skip: ['roles/y'] }],
    ['POST', '/api/federation/boards/brd/items/itm/versions/v1/import', { skip: ['roles/y'], preview_token: 'tok' }],
    ['PUT', '/api/federation/boards/brd/items/itm/pin', { version: 'v1' }],
  ]);
});

test('shared config: open verifies and shows text, import previews then applies with the token; invalid rows stay inert', async (t) => {
  const s = await mount(t);
  await s.click(s.q('[data-board-id="brd_ops"] [data-board="open"]'));
  const list = s.q('#fleet-board-item-list');
  assert.match(list.textContent, /review roles.*update.*kept v1aaaaaaaa.*forge/);
  const bad = s.q('[data-item-id="itm_bad"]');
  assert.match(bad.textContent, /publisher metadata could not be verified.*not importable/);
  assert.equal(bad.querySelector('button'), null, 'no fetch or import on an invalid row');
  await s.click(s.q('[data-item-id="itm_1"] [data-item-act="open"]'));
  assert.deepEqual(s.log.find((l) => l[0] === 'fetch'), ['fetch', 'brd_ops', 'itm_1', 'v2bbbbbbbbbbbb']);
  assert.match(s.q('#fleet-bundle-inspect-title').textContent, /review roles \(version v2bbbbbbbb\)/);
  assert.match(s.q('#fleet-bundle-inspect').textContent, /posted by forge · sha256 abab/);
  await s.click(s.q('#fleet-bundle-entries [data-path="roles/reviewer.json"]'));
  assert.match(s.q('#fleet-bundle-text').textContent, /<b>reviewer<\/b>/, 'shown as text');
  assert.equal(s.q('#fleet-bundle-text b'), null);
  assert.equal([...s.q('#fleet-bundle-inspect').querySelectorAll('button')].some((b) => /Decline/.test(b.textContent)), false);
  await s.click(s.q('#fleet-bundle-import'));
  assert.equal(s.q('#fleet-board-import-apply').disabled, true, 'a conflict needs overwrite or untick');
  const box = s.q('[data-item="roles/reviewer"] input');
  box.checked = false;
  await s.harness.act(() => s.harness.fireEvent(box, 'change'));
  assert.equal(s.q('#fleet-board-import-apply').disabled, true, 'changed choices need a fresh preview');
  await s.click(s.q('#fleet-board-import-preview'));
  assert.deepEqual(s.log.filter((l) => l[0] === 'preview').at(-1), ['preview', { skip: ['roles/reviewer'] }]);
  await s.click(s.q('#fleet-board-import-apply'));
  assert.match(s.confirms.at(-1).body, /Applies 1 item from review roles \(board ops notes, posted by forge\).*1 new/);
  const imp = s.log.find((l) => l[0] === 'import');
  assert.deepEqual(imp.slice(1, 4), ['brd_ops', 'itm_1', 'v2bbbbbbbbbbbb']);
  assert.match(imp[4], /^tok/); assert.deepEqual(imp[5], { skip: ['roles/reviewer'] });
  assert.match(s.toasts.at(-1)[0], /Imported 1 items from review roles/);
});

test('posting config: sections or named items, new versions carry the parent; versions can be kept', async (t) => {
  const s = await mount(t);
  await s.click(s.q('[data-board-id="brd_ops"] [data-board="open"]'));
  await s.click(s.q('#fleet-board-post'));
  assert.equal(s.q('#fleet-board-publish-post').disabled, true);
  await s.type(s.q('#fleet-board-publish-name'), 'team roles');
  const roles = s.q('#fleet-board-publish [data-section="roles"]');
  roles.checked = true;
  await s.harness.act(() => s.harness.fireEvent(roles, 'change'));
  await s.click(s.q('#fleet-board-publish-post'));
  assert.match(s.confirms.at(-1).body, /Shares roles from this node's config with every member of ops notes.*nothing changes on their nodes until they do/);
  assert.deepEqual(s.log.find((l) => l[0] === 'publish'), ['publish', 'brd_ops', { name: 'team roles', only: ['roles'] }]);
  await s.click(s.q('[data-item-id="itm_1"] [data-item-act="update"]'));
  await s.type(s.q('#fleet-board-publish-only'), 'roles/reviewer, profiles/fast');
  await s.click(s.q('#fleet-board-publish-post'));
  assert.deepEqual(s.log.filter((l) => l[0] === 'publish').at(-1), ['publish', 'brd_ops', { name: 'review roles', only: ['roles/reviewer', 'profiles/fast'], item: 'itm_1', parent: 'v2bbbbbbbbbbbb' }]);
  await s.click(s.q('[data-item-id="itm_1"] [data-item-act="versions"]'));
  assert.equal(s.q('[data-version="v1aaaaaaaaaaaa"] [data-version-act="pin"]'), null, 'the kept version is marked, not offered');
  await s.click(s.q('[data-version="v2bbbbbbbbbbbb"] [data-version-act="pin"]'));
  assert.match(s.confirms.at(-1).body, /It is not imported/);
  assert.deepEqual(s.log.find((l) => l[0] === 'pin'), ['pin', 'brd_ops', 'itm_1', 'v2bbbbbbbbbbbb']);
});
