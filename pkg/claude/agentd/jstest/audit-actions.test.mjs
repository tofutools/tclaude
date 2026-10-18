import test from 'node:test'; import assert from 'node:assert/strict'; import { createPreactHarness } from './preact-harness.mjs';
test('Audit actions preserve API parameters and reject stale overlapping responses', async (t) => {
  const harness = await createPreactHarness(t); const [{ createAuditState }, { createAuditActions }] = await Promise.all([harness.importDashboardModule('js/audit-state.js'), harness.importDashboardModule('js/audit-actions.js')]);
  const state = createAuditState({ activeTab: harness.signals.signal('audit') }); state.setFilter('outcome', 'failure'); state.cycleSort('verb');
  const pending = []; const actions = createAuditActions({ state, fetchImpl: (url, options) => new Promise((resolve) => pending.push({ url, options, resolve })) });
  const old = actions.load(); const fresh = actions.load(); assert.match(pending[0].url, /^\/api\/audit\?page=1&page_size=100&sort=verb&dir=asc&outcome=failure$/); assert.deepEqual(pending[0].options, { credentials: 'same-origin' });
  pending[1].resolve({ ok: true, json: async () => ({ entries: [{ id: 2 }], total: 1, total_unfiltered: 1 }) }); assert.equal(await fresh, true);
  pending[0].resolve({ ok: true, json: async () => ({ entries: [{ id: 1 }], total: 1, total_unfiltered: 1 }) }); assert.equal(await old, false); assert.equal(state.view.value.rows[0].id, 2);
});
test('Audit actions expose HTTP failures', async (t) => {
  const harness = await createPreactHarness(t); const [{ createAuditState }, { createAuditActions }] = await Promise.all([harness.importDashboardModule('js/audit-state.js'), harness.importDashboardModule('js/audit-actions.js')]);
  const state = createAuditState({ activeTab: harness.signals.signal('audit') }); const actions = createAuditActions({ state, fetchImpl: async () => ({ ok: false, status: 503, text: async () => 'offline' }) });
  assert.equal(await actions.load(), false); assert.match(state.view.value.request.error, /offline/);
});
test('a fused audit reads one peer through its routes, or merges every node newest first', async (t) => {
  const harness = await createPreactHarness(t); const [{ createAuditState }, { createAuditActions }] = await Promise.all([harness.importDashboardModule('js/audit-state.js'), harness.importDashboardModule('js/audit-actions.js')]);
  const self = { id: 'inst_self', name: 'desk', local: true, color: '#58a6ff' }; const forge = { id: 'inst_forge', name: 'forge', color: '#d29922' }; const mac = { id: 'inst_mac0', name: 'mac', color: '#a371f7' };
  const page = (entries) => ({ ok: true, json: async () => ({ entries, total: entries.length, total_unfiltered: entries.length }) });
  const calls = [];
  const fetchFor = (id) => async (url) => { calls.push([id, url]); return id === 'inst_mac0' ? { ok: false, status: 403, text: async () => '' } : page([{ id: 7, at: '2026-10-18T10:02:00Z' }]); };
  const state = createAuditState({ activeTab: harness.signals.signal('audit') });
  const actions = createAuditActions({ state, nodes: () => [self, forge, mac], fetchFor, fetchImpl: async (url) => { calls.push(['self', url]); return page([{ id: 1, at: '2026-10-18T10:05:00Z' }, { id: 2, at: '2026-10-18T09:00:00Z' }]); } });
  state.setNode('inst_forge'); assert.equal(await actions.load(), true);
  assert.deepEqual(calls.map(([id]) => id), ['inst_forge'], "one peer's log, through its routes");
  state.setNode('inst_mac0'); assert.equal(await actions.load(), false);
  assert.match(state.view.value.request.error, /mac does not share its audit log with you \(needs federation\.audit\.read\)/);
  calls.length = 0; state.setNode('all'); assert.equal(await actions.load(), true);
  const view = state.view.value;
  assert.deepEqual(view.rows.map((e) => `${e.node.name}#${e.id}`), ['desk#1', 'forge#7', 'desk#2'], 'merged newest first, each tagged with its node');
  assert.equal(view.pages, 1, 'every node fits on the page');
  assert.deepEqual(view.response.refused.map((r) => r.node.name), ['mac']);
});
test('the fused "all nodes" audit pages every node with its keyset cursor, beside a node on an older build', async (t) => {
  const harness = await createPreactHarness(t); const [{ createAuditState }, { createAuditActions }] = await Promise.all([harness.importDashboardModule('js/audit-state.js'), harness.importDashboardModule('js/audit-actions.js')]);
  const self = { id: 'inst_self', name: 'desk', local: true }; const forge = { id: 'inst_forge', name: 'forge' }; const lab = { id: 'inst_lab', name: 'lab' };
  // desk 23, forge 17 and lab (an older build: no before=) 12 events, clocks interleaved.
  const counts = { desk: 23, forge: 17, lab: 12 };
  const event = (node, i) => ({ id: i, verb: `${node} ${i}`, at: new Date(Date.UTC(2026, 9, 18, 0, 0, i * 3 + { desk: 0, forge: 1, lab: 2 }[node])).toISOString() });
  const logs = Object.fromEntries(Object.entries(counts).map(([n, k]) => [n, Array.from({ length: k }, (_, i) => event(n, i + 1)).reverse()]));
  const calls = []; let down = '';
  const serve = (node) => async (url) => {
    calls.push([node, url]);
    if (node === down) return { ok: false, status: 502, text: async () => '' };
    const q = new URL(url, 'http://x').searchParams; const size = Number(q.get('page_size')); const page = Number(q.get('page'));
    const all = logs[node]; const total = all.length;
    if (node === 'lab') return { ok: true, json: async () => ({ entries: all.slice((page - 1) * size, page * size), total, total_unfiltered: total }) };
    const before = Number(q.get('before') || 0); const below = before ? all.filter((e) => e.id < before) : all;
    return { ok: true, json: async () => ({ entries: below.slice(0, size), total, total_unfiltered: total, has_more: below.length > size, cursor_paging: true }) };
  };
  const state = createAuditState({ activeTab: harness.signals.signal('audit') });
  const actions = createAuditActions({ state, nodes: () => [self, forge, lab], fetchImpl: serve('desk'), fetchFor: (id) => serve(id.slice(5)) });
  state.setNode('all'); state.setPageSize(10);
  const verbs = () => state.view.value.rows.map((e) => e.verb);
  assert.equal(await actions.load(), true);
  assert.equal(state.view.value.total, 52, 'counts are the nodes\' summed totals');
  assert.equal(state.view.value.pages, 6);
  const seen = [...verbs()];
  while (state.view.value.page < state.view.value.pages) {
    calls.length = 0; state.setPage(state.view.value.page + 1); await actions.load();
    assert.ok(calls.some(([n, u]) => n === 'forge' && /before=\d+/.test(u)) || !calls.some(([n]) => n === 'forge'), 'a cursor node reads below its cursor');
    assert.ok(!calls.some(([n, u]) => n !== 'lab' && !/page_size=10&/.test(u)), 'a cursor node never reads deeper than one page');
    seen.push(...verbs());
  }
  assert.equal(seen.length, 52, 'the walk shows every event'); assert.equal(new Set(seen).size, 52, 'none twice');
  const at = (v) => { const [n, i] = v.split(' '); return Date.parse(event(n, Number(i)).at); };
  assert.deepEqual(seen.map(at), seen.map(at).sort((a, b) => b - a), 'newest first across nodes');

  // Last from page 1 walks forward over the known vectors.
  state.setPage(1); await actions.load(); state.setPage(6); await actions.load();
  assert.equal(state.view.value.page, 6); assert.deepEqual(verbs(), seen.slice(50));

  // New events move page 1; Next starts right below it as it now reads.
  state.setPage(1); await actions.load();
  for (let i = 0; i < 3; i++) logs.forge.unshift(event('forge', 100 + i));
  await actions.load(); const first = verbs(); assert.equal(first[0], 'forge 102');
  state.setPage(2); await actions.load();
  const everything = [...logs.desk, ...logs.forge, ...logs.lab].map((e) => e.verb).sort((a, b) => at(b) - at(a));
  assert.deepEqual([...first, ...verbs()], everything.slice(0, 20), 'page 2 follows page 1 as it reads now');

  // A walk superseded by a newer load (here a filter change) stops, so it
  // cannot overwrite the newer walk's vectors.
  state.setFilter('source', 'hook'); state.setPage(6); const stale = actions.load();
  state.setFilter('source', 'cli'); state.setPage(4); const fresh = actions.load();
  assert.equal(await stale, false, 'the superseded walk commits nothing'); await fresh;
  assert.equal(state.view.value.page, 4, 'the newer walk reaches its page');
  const fourth = verbs(); state.setPage(1); await actions.load();
  assert.deepEqual(fourth.filter((v) => verbs().includes(v)), [], 'page 4 is not page 1 again');
  state.setFilter('source', '');

  // A node down while the pages were walked has no cursor in them; when it
  // comes back the walk restarts, so its events do not repeat.
  down = 'forge'; state.setPage(1); await actions.load(); state.setPage(2); await actions.load();
  assert.deepEqual(state.view.value.response.refused.map((r) => r.node.name), ['forge']);
  down = ''; await actions.load();
  const page2 = verbs(); state.setPage(1); await actions.load();
  assert.ok(page2.some((v) => v.startsWith('forge ')), 'forge events are back');
  assert.deepEqual(page2.filter((v) => verbs().includes(v)), [], 'no event on both pages');
});
test('a fused audit node that does not answer holds up neither the page nor the walk', async (t) => {
  const harness = await createPreactHarness(t); const [{ createAuditState }, { createAuditActions }] = await Promise.all([harness.importDashboardModule('js/audit-state.js'), harness.importDashboardModule('js/audit-actions.js')]);
  const self = { id: 'inst_self', name: 'desk', local: true }; const forge = { id: 'inst_forge', name: 'forge' };
  const event = (node, i) => ({ id: i, verb: `${node} ${i}`, at: new Date(Date.UTC(2026, 9, 18, 0, 0, i * 2 + (node === 'forge' ? 1 : 0))).toISOString() });
  const logs = { desk: Array.from({ length: 30 }, (_, i) => event('desk', 30 - i)), forge: Array.from({ length: 30 }, (_, i) => event('forge', 30 - i)) };
  const held = []; let hang = false; const forgeCalls = [];
  const serve = (node) => async (url) => {
    if (node === 'forge') { forgeCalls.push(url); if (hang) return new Promise((resolve) => held.push(resolve)); }
    const q = new URL(url, 'http://x').searchParams; const size = Number(q.get('page_size'));
    const before = Number(q.get('before') || 0); const below = before ? logs[node].filter((e) => e.id < before) : logs[node];
    return { ok: true, json: async () => ({ entries: below.slice(0, size), total: logs[node].length, total_unfiltered: logs[node].length, has_more: below.length > size, cursor_paging: true }) };
  };
  const state = createAuditState({ activeTab: harness.signals.signal('audit') });
  const actions = createAuditActions({ state, nodes: () => [self, forge], fetchImpl: serve('desk'), fetchFor: (id) => serve(id.slice(5)) });
  state.setNode('all'); state.setPageSize(10);
  const verbs = () => state.view.value.rows.map((e) => e.verb);
  const settle = () => new Promise((r) => setTimeout(r, 0));

  // forge is believed online but silent: desk's events show while it is awaited.
  hang = true; const first = actions.load(); await settle();
  assert.deepEqual(verbs().slice(0, 2), ['desk 30', 'desk 29'], "the answering node's page shows at once");
  assert.deepEqual(state.view.value.response.pending.map((n) => n.name), ['forge']);
  held.splice(0).forEach((resolve) => resolve({ ok: false, status: 504, text: async () => '' })); await first;
  assert.deepEqual(state.view.value.response.refused.map((r) => r.node.name), ['forge'], 'its timeout marks it refused');

  // Walking to page 3 steps past it: only the page shown asks it again, and
  // that page shows without waiting for it.
  forgeCalls.length = 0; state.setPage(3); const walkTo = actions.load(); await settle();
  assert.equal(forgeCalls.length, 1, 'one read of the silent node for the whole walk');
  assert.equal(state.view.value.page, 3); assert.deepEqual(verbs(), logs.desk.slice(20, 30).map((e) => e.verb));
  held.splice(0).forEach((resolve) => resolve({ ok: false, status: 504, text: async () => '' })); await walkTo;

  // It comes back: the walk restarts so its events slot in without repeats.
  hang = false; await actions.load();
  const third = verbs(); state.setPage(1); await actions.load();
  assert.ok(third.some((v) => v.startsWith('forge ')) && third.every((v) => !verbs().includes(v)), 'forge is back, nothing twice');
});
