// js/phone-shell-model.js: the phone shell's slots, titles, Now view and
// Board groups, as plain data.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// The model reads taskboard-context.js, which imports Preact signals: load it
// through the harness's import map.
const load = async (t) => (await createPreactHarness(t)).importDashboardModule('js/phone-shell-model.js');

const member = (agent_id, title, status, extra = {}) => ({
  agent_id, conv_id: `conv-${agent_id}`, title, online: true, state: { status }, ...extra,
});

test('the bottom nav is Now · Agents · Mail · Board · More', async (t) => {
  const { PHONE_SLOTS } = await load(t);
  assert.deepEqual(PHONE_SLOTS.map((slot) => slot.label), ['Now', 'Agents', 'Mail', 'Board', 'More']);
});

test('the highlighted slot follows the own view, else the active tab', async (t) => {
  const { slotFor, titleFor } = await load(t);
  assert.equal(slotFor('now', 'messages'), 'now', 'an own view wins over the tab under it');
  assert.equal(slotFor('board', 'groups'), 'board');
  assert.equal(slotFor(null, 'groups'), 'agents');
  assert.equal(slotFor(null, 'messages'), 'mail');
  assert.equal(slotFor(null, 'config'), 'more', 'every other tab is under More');
  assert.equal(titleFor('now', 'groups'), 'Now');
  assert.equal(titleFor(null, 'groups'), 'Agents');
  assert.equal(titleFor(null, 'config'), 'Settings');
  assert.equal(titleFor(null, 'nonesuch'), 'tclaude');
});

test('Now lists what needs the operator, then errors and work', async (t) => {
  const { nowView } = await load(t);
  const snapshot = {
    groups: [{
      name: 'webshop',
      members: [
        member('agt_00000000000000000000000000000001', 'payments-fix', 'awaiting_permission'),
        member('agt_00000000000000000000000000000002', 'checkout-ui', 'awaiting_input', { state: { status: 'awaiting_input', status_detail: 'Which button?' } }),
        member('agt_00000000000000000000000000000003', 'lead', 'working'),
        member('agt_00000000000000000000000000000004', 'release-bot', 'error'),
        member('agt_00000000000000000000000000000005', 'tester', 'working', { online: false }),
      ],
    }],
    access_requests: [
      { id: 1, agent_id: 'agt_00000000000000000000000000000001', perm: 'groups.read', status: 'pending' },
      { id: 2, agent_id: 'agt_00000000000000000000000000000003', perm: 'models.proxy', status: 'pending' },
      { id: 3, agent_id: 'agt_00000000000000000000000000000003', perm: 'groups.read', status: 'approved' },
    ],
    messages: [
      { id: 7, subject: 'Which carrier?', options: ['DHL', 'UPS'], from_agent: 'agt_00000000000000000000000000000003' },
      { id: 8, subject: 'answered', options: ['a', 'b'], from_agent: 'agt_00000000000000000000000000000003', decision: 1 },
    ],
  };
  const view = nowView(snapshot);
  assert.deepEqual(view.approvals.map((r) => r.id), [1], 'only pending requests a card can answer');
  assert.equal(view.elsewhere, 1, 'the models.proxy request needs Messages');
  assert.deepEqual(view.decisions.map((d) => d.id), [7], 'open decisions only');
  assert.deepEqual(view.asking.map((a) => a.name), ['checkout-ui'], 'an agent with an approval card is not listed twice');
  assert.equal(view.asking[0].detail, 'Which button?');
  assert.deepEqual(view.errors.map((a) => a.name), ['release-bot']);
  assert.deepEqual(view.working.map((a) => a.name), ['lead'], 'offline agents are not working');
  assert.equal(view.needs, 4, 'one approval, one elsewhere, one decision, one asking agent');
  assert.equal(nowView(null).needs, 0);
});

test('Now\'s rows: every online agent when there are few, else the busy ones until Show all', async (t) => {
  const { nowView, agentRows, FEW_ONLINE } = await load(t);
  const id = (n) => `agt_${String(n).padStart(32, '0')}`;
  const snapshot = (idle) => ({ groups: [
    { name: 'webshop', members: [member(id(1), 'lead', 'working'), member(id(2), 'bot', 'error'), member(id(3), 'asker', 'awaiting_input')] },
    { name: 'tclaude', members: Array.from({ length: idle }, (_, i) => member(id(10 + i), `idle-${i}`, 'idle')) },
    { name: 'alpha', members: [member(id(99), 'zed', 'idle'), member(id(98), 'off', 'idle', { online: false })] },
  ] });
  const few = agentRows(nowView(snapshot(2)));
  assert.equal(few.all, true);
  assert.deepEqual(few.rows.map((r) => [r.agent.name, r.state]),
    [['bot', 'error'], ['lead', 'working'], ['zed', 'idle'], ['idle-0', 'idle'], ['idle-1', 'idle']],
    'busy first, then the rest by group; never the asker (a card) or an offline agent');
  const many = nowView(snapshot(FEW_ONLINE));
  const busy = agentRows(many);
  assert.equal(busy.all, false);
  assert.equal(busy.total, FEW_ONLINE + 3);
  assert.deepEqual(busy.rows.map((r) => r.agent.name), ['bot', 'lead']);
  assert.equal(agentRows(many, { showAll: true }).rows.length, FEW_ONLINE + 3);
});

test('Now\'s chips: Idle by group, Recent latest first, Stuck? quiet the longest first, and All above few keeps the recent', async (t) => {
  const { nowView, agentRows, idleGroups, recentRows, stuckRows, FEW_ONLINE, STUCK_AFTER_MS, RECENT_MS } = await load(t);
  const now = Date.parse('2026-11-15T12:00:00Z');
  const ago = (min) => new Date(now - min * 60_000).toISOString();
  const id = (n) => `agt_${String(n).padStart(32, '0')}`;
  const m = (n, title, status, hook, extra = {}) => member(id(n), title, status, { state: { status, last_hook: hook }, ...extra });
  const view = nowView({ groups: [
    { name: 'webshop', members: [m(1, 'lead', 'working', ago(1)), m(2, 'migrations', 'working', ago(47)), m(3, 'probe', 'working', ago(22)), m(4, 'nohook', 'working', '')] },
    { name: 'tclaude', members: [m(5, 'steward', 'idle', ago(4)), m(6, 'old', 'idle', ago(60)), m(7, 'asker', 'awaiting_input', ago(0))] },
    { name: 'alpha', members: [m(8, 'zed', 'idle', ago(9)), m(9, 'gone', 'idle', ago(0), { online: false })] },
  ] });
  assert.deepEqual(idleGroups(view).map(({ group, agents }) => [group, agents.map((a) => a.name)]),
    [['alpha', ['zed']], ['tclaude', ['steward', 'old']]], 'by group, the latest heard from first');
  assert.deepEqual(recentRows(view).map((r) => r.agent.name),
    ['asker', 'lead', 'steward', 'zed', 'probe', 'migrations', 'old', 'nohook'], 'every online agent, latest first, never-heard last');
  assert.equal(recentRows(view)[0].state, 'asking');
  const stuck = stuckRows(view, now);
  assert.deepEqual(stuck.rows.map((r) => [r.agent.name, r.quiet]),
    [['migrations', true], ['probe', true], ['lead', false], ['nohook', false]], 'quiet the longest first; every one past the threshold is marked');
  assert.equal(stuck.quiet, 2, 'the badge counts the marked rows');
  assert.equal(STUCK_AFTER_MS, 15 * 60_000);

  // Many online: All keeps the busy ones and the idle ones heard from lately.
  const crowd = nowView({ groups: [
    { name: 'webshop', members: [m(1, 'lead', 'working', ago(1))] },
    { name: 'tclaude', members: Array.from({ length: FEW_ONLINE }, (_, i) => m(20 + i, `idle-${i}`, 'idle', ago(i === 0 ? 3 : RECENT_MS / 60_000 + 5))) },
  ] });
  const shown = agentRows(crowd, { now });
  assert.equal(shown.all, false);
  assert.deepEqual(shown.rows.map((r) => r.agent.name), ['lead', 'idle-0'], 'idle-0 called a hook 3m ago; the rest are older than RECENT_MS');
});

test('Problems: each online agent once under its worst problem', async (t) => {
  const { nowView, problemRows, CONTEXT_HIGH_PCT } = await load(t);
  const now = Date.parse('2026-11-15T12:00:00Z');
  const ago = (min) => new Date(now - min * 60_000).toISOString();
  const id = (n) => `agt_${String(n).padStart(32, '0')}`;
  const pr = (n, state, checks) => ({ branch_pr_number: n, branch_pr_url: `https://x/pull/${n}`, branch_pr_state: state, branch_checks: checks });
  const m = (n, title, status, extra = {}) => member(id(n), title, status, { ...extra, state: { status, ...(extra.state || {}) } });
  const snapshot = { groups: [{ name: 'g', members: [
    m(1, 'broken', 'error', { state: { context_pct: 95 } }),
    m(2, 'red', 'idle', pr(10, 'open', { failed: 2 })),
    m(3, 'merged-red', 'idle', pr(11, 'merged', { failed: 1 })),
    m(4, 'full', 'working', { state: { context_pct: CONTEXT_HIGH_PCT, last_hook: ago(1) } }),
    m(5, 'almost', 'working', { state: { context_pct: CONTEXT_HIGH_PCT - 1, last_hook: ago(1) } }),
    m(6, 'quiet', 'working', { state: { last_hook: ago(30) } }),
    m(7, 'away', 'working', { online: false, state: { context_pct: 99, last_hook: ago(60) } }),
  ] }] };
  const view = nowView(snapshot);
  assert.deepEqual(problemRows(view, now).map((r) => [r.kind, r.agent.name]),
    [['error', 'broken'], ['ci', 'red'], ['quiet', 'quiet']],
    'broken once; a merged PR\'s red checks are not a problem; a full context is not one either (the harness compacts); offline is not');

});

test('Board lists groups with live tasks, those waiting on you first', async (t) => {
  const { boardGroups } = await load(t);
  const context = { groups: [
    { group: 'quiet', counts: { live: 0 } },
    { group: 'b-team', counts: { live: 3 } },
    { group: 'a-team', counts: { live: 2 } },
    { group: 'z-team', counts: { live: 1, waiting_on_you: 1 } },
  ] };
  assert.deepEqual(boardGroups(context).map((g) => g.group), ['z-team', 'a-team', 'b-team']);
  assert.deepEqual(boardGroups(null), []);
});

