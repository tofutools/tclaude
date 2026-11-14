// js/phone-shell-model.js: the phone shell's slots, titles, Now view and
// Board lanes, as plain data.
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

test('Board lanes split a group by row state, in board order, empty lanes left out', async (t) => {
  const { BOARD_LANES, boardLanes } = await load(t);
  const entry = { tasks: [
    { id: 'tk_1', title: 'mine', status: 'open', stage: 'next', assignee: { operator: true, agent_id: 'operator' } },
    { id: 'tk_2', title: 'stuck', status: 'blocked', stage: 'next' },
    { id: 'tk_3', title: 'doing', status: 'open', stage: 'next', assignee: { agent_id: 'agt_x' } },
    { id: 'tk_4', title: 'in review', status: 'open', stage: 'next', column: 'review' },
    { id: 'tk_5', title: 'queued', status: 'open', stage: 'next' },
    { id: 'tk_6', title: 'later', status: 'open', stage: 'idea' },
    { id: 'tk_7', title: 'parked', status: 'open', stage: 'backlog' },
  ] };
  const lanes = boardLanes(entry);
  assert.deepEqual(lanes.map((lane) => [lane.id, lane.tasks.map((t) => t.id)]), [
    ['waiting', ['tk_1']],
    ['blocked', ['tk_2']],
    ['progress', ['tk_3', 'tk_4']],
    ['next', ['tk_5']],
    ['ideas', ['tk_6']],
  ], 'a backlog task is in no lane');
  assert.equal(BOARD_LANES.length, 5);
  assert.deepEqual(boardLanes({ tasks: [] }), []);
});

test('a task opens through the same ?task= deep link as on the desktop', async (t) => {
  const { taskPath } = await load(t);
  assert.equal(taskPath({ id: 'tk_ab12' }, 'webshop'), '/?task=tk_ab12%3Awebshop');
});
