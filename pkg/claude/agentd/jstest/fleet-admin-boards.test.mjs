import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const view = {
  self: { id: 'inst_self', name: 'desk', hubURL: 'wss://hub.example' },
  trusted: [{ id: 'inst_forge', label: 'forge' }],
  waiting: [],
};

const invite = (hub) => `board1_${Buffer.from(JSON.stringify({ hub, board: 'brd_1', secret: 's', key: 'k' })).toString('base64url')}`;

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
  assert.deepEqual(calls.slice(2), [
    ['POST', '/api/federation/boards/join', { token: 'board1_x' }],
    ['POST', '/api/federation/boards/brd%201/invites', { role: 'reader', ttl_seconds: 3600 }],
    ['PUT', '/api/federation/boards/brd/members/inst_f', { role: 'publisher' }],
    ['POST', '/api/federation/boards/brd/rotate-key', {}],
    ['PATCH', '/api/federation/hub/boards/brd', { frozen: true }],
  ]);
});
