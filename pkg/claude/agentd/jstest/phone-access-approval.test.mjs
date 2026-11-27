// The phone's access-request card and its helpers (js/phone-now-model.js
// approvalTiming / approvalAsk, js/phone-access-facts.js,
// js/phone-access-alerts.js): both clocks, "+5 min" only while it can still
// buy time, every permission shown, and the app badge.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const NOW = Date.parse('2026-11-27T17:15:00Z');
const at = (secs) => new Date(NOW + secs * 1000).toISOString();

test('approvalTiming: both clocks, and +5 min only while the deadline is short of the agent\'s wait', async (t) => {
  const harness = await createPreactHarness(t);
  const { approvalTiming } = await harness.importDashboardModule('js/phone-now-model.js');
  // A sudo request: the popup's 60s, the agent waits 10 min.
  assert.deepEqual(approvalTiming({ created_at: at(-20), deadline: at(40), wait_ceiling: at(580) }, NOW),
    { left: 40, waitLeft: 580, extendable: true, ceilingSecs: 600 });
  // An --ask-human gate: the deadline is the agent's limit, nothing to extend.
  assert.deepEqual(approvalTiming({ created_at: at(-35), deadline: at(85), wait_ceiling: at(85) }, NOW),
    { left: 85, waitLeft: 85, extendable: false, ceilingSecs: 120 });
  // An older agentd names no ceiling: extend as before.
  assert.deepEqual(approvalTiming({ deadline: at(40) }, NOW), { left: 40, waitLeft: null, extendable: true, ceilingSecs: null });
});

test('approvalAsk names a sudo plan and a one-off command in words', async (t) => {
  const harness = await createPreactHarness(t);
  const { approvalAsk } = await harness.importDashboardModule('js/phone-now-model.js');
  const plan = approvalAsk({ perm: 'sudo.groups.create,agent.spawn,groups.members.spawn',
    body: 'Sudo request:\n  slugs:\n    - groups.create\n    - agent.spawn\n    - groups.members.spawn\n  duration: 15m0s\n  expires_at: 2026-11-27T19:04:09+01:00\n  reason: bootstrap the task force' });
  assert.deepEqual(plan, { plan: true, slugs: ['groups.create', 'agent.spawn', 'groups.members.spawn'], meta: 'for 15 min of sudo',
    why: 'bootstrap the task force', title: 'asks for 3 permissions', fields: null, duration: '15 min' });
  const once = approvalAsk({ perm: 'git.push', body: 'push PR #4351\nmore' });
  assert.equal(once.title, 'asks for git.push');
  assert.equal(once.meta, 'once');
  assert.equal(once.why, 'push PR #4351');
  // A gated command's JSON body reads as labelled fields, not raw JSON.
  const gate = approvalAsk({ perm: 'groups.create', body: '{"name":"sudo-mobile","owner":"lead","tags":["a","b"],"note":""}' });
  assert.deepEqual(gate.fields, [['name', 'sudo-mobile'], ['owner', 'lead'], ['tags', 'a, b']]);
  assert.equal(gate.why, 'name: sudo-mobile · owner: lead · tags: a, b');
});

test('requestFields reads only a flat JSON object; anything else stays as it is', async (t) => {
  const harness = await createPreactHarness(t);
  const { requestFields } = await harness.importDashboardModule('js/phone-now-model.js');
  assert.deepEqual(requestFields('{"n":1,"ok":true}'), [['n', '1'], ['ok', 'true']]);
  assert.equal(requestFields('push PR #1'), null, 'plain text');
  assert.equal(requestFields('{"spec":{"deep":1}}'), null, 'a nested object is an unknown shape');
  assert.equal(requestFields('[1,2]'), null);
  assert.equal(requestFields('{}'), null);
  assert.equal(requestFields(''), null);
});

test('the reader\'s facts: the two clocks as clock times, and each slug\'s first sentence', async (t) => {
  const harness = await createPreactHarness(t);
  const facts = await harness.importDashboardModule('js/phone-access-facts.js');
  assert.match(facts.accessTiming({ deadline: at(40), wait_ceiling: at(580) }), /^Auto-denies [^·]+ · agent waits until [^·]+$/);
  assert.match(facts.accessTiming({ deadline: at(85), wait_ceiling: at(85) }), /^Auto-denies [^·]+ · the agent stops waiting then$/);
  assert.match(facts.accessTiming({ deadline: at(85) }), /^Auto-denies [^·]+$/, 'an older agentd names no ceiling');
  assert.match(facts.accessTiming({ deadline: at(40), wait_ceiling: at(580) }, NOW), /^Auto-denies [^·(]+ \(0:40\) · agent waits until [^·]+$/, 'the time left rides along');
  assert.equal(facts.accessTiming({}), '');
  const described = facts.slugDescriptions([{ slug: 'agent.spawn', description: 'Spawn a fresh agent into any group globally. Group-scoped authority uses groups.members.spawn.' }, { slug: 'x' }]);
  assert.equal(described.get('agent.spawn'), 'Spawn a fresh agent into any group globally.');
  assert.equal(described.has('x'), false);
});

test('the app badge counts the access requests that wait', async (t) => {
  const harness = await createPreactHarness(t);
  const alerts = await harness.importDashboardModule('js/phone-access-alerts.js');
  assert.equal(alerts.pendingAccessCount({ access_requests_pending: 2 }), 2);
  assert.equal(alerts.pendingAccessCount(null), 0);
  const badge = [];
  await alerts.syncAppBadge(3, { setAppBadge: (n) => badge.push(['set', n]), clearAppBadge: () => badge.push(['clear']) });
  await alerts.syncAppBadge(0, { setAppBadge: (n) => badge.push(['set', n]), clearAppBadge: () => badge.push(['clear']) });
  await alerts.syncAppBadge(1, {});
  assert.deepEqual(badge, [['set', 3], ['clear']]);
});

test('the card shows every permission, both clocks, and +5 min only when it can extend', async (t) => {
  const harness = await createPreactHarness(t);
  const { PhoneApprovalCard } = await harness.importDashboardModule('js/phone-approval.js');
  const now = Date.now();
  const iso = (secs) => new Date(now + secs * 1000).toISOString();
  const sudo = { id: 'ar_s', perm: 'sudo.groups.create,agent.spawn,groups.members.spawn', created_at: iso(-20), deadline: iso(40), wait_ceiling: iso(580),
    body: 'Sudo request:\n  slugs:\n    - groups.create\n    - agent.spawn\n    - groups.members.spawn\n  duration: 15m0s\n  expires_at: 2026-11-27T19:04:09+01:00\n  reason: bootstrap the task force' };
  const gate = { id: 'ar_g', perm: 'groups.create', created_at: iso(-35), deadline: iso(85), wait_ceiling: iso(85), body: 'tclaude agent groups create sudo-mobile' };
  const { container: root } = await harness.mount(harness.html`<div>
    <${PhoneApprovalCard} request=${sudo} from="Stewie" group="tclaude" notify=${() => {}} decide=${async () => {}} />
    <${PhoneApprovalCard} request=${gate} from="lead" notify=${() => {}} decide=${async () => {}} />
  </div>`);
  const [plan, once] = [...root.querySelectorAll('.phone-approval-card')];
  assert.deepEqual([...plan.querySelectorAll('.phone-approval-slug')].map((c) => c.textContent), ['groups.create', 'agent.spawn', 'groups.members.spawn']);
  assert.match(plan.querySelector('.phone-approval-meta').textContent, /^asks for 15 min of sudo · \d+s ago$/);
  assert.match(plan.querySelector('.phone-approval-timing').textContent, /Auto-denies in \d:\d\d\s*agent waits 9:\d\d/);
  assert.deepEqual([...plan.querySelectorAll('.phone-btns .ui-btn')].map((b) => b.textContent.trim()), ['Deny', '+5 min', 'Approve']);
  assert.equal(plan.querySelector('.phone-approval-note'), null);

  assert.deepEqual([...once.querySelectorAll('.phone-btns .ui-btn')].map((b) => b.textContent.trim()), ['Deny', 'Approve'], 'nothing to extend at the agent\'s limit');
  assert.match(once.querySelector('.phone-approval-timing').textContent, /agent stops waiting then/);
  assert.equal(once.querySelector('.phone-approval-note').textContent, "Can't be extended: the agent asked to wait 2 min.");
});

