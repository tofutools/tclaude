// The phone's access-request card and its helpers (js/phone-now-model.js
// approvalTiming / approvalAsk, js/phone-access-facts.js,
// js/phone-access-alerts.js): both clocks, "+5 min" only while it can still
// buy time, every permission shown, and the one-tap alert settings.
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
    why: 'bootstrap the task force', title: 'asks for 3 permissions', duration: '15 min' });
  const once = approvalAsk({ perm: 'git.push', body: 'push PR #4351\nmore' });
  assert.equal(once.title, 'asks for git.push');
  assert.equal(once.meta, 'once');
  assert.equal(once.why, 'push PR #4351');
});

test('the reader\'s facts: the two clocks as clock times, and each slug\'s first sentence', async (t) => {
  const harness = await createPreactHarness(t);
  const facts = await harness.importDashboardModule('js/phone-access-facts.js');
  assert.match(facts.accessTiming({ deadline: at(40), wait_ceiling: at(580) }), /^Auto-denies [^·]+ · agent waits until [^·]+$/);
  assert.match(facts.accessTiming({ deadline: at(85), wait_ceiling: at(85) }), /^Auto-denies [^·]+$/);
  assert.equal(facts.accessTiming({}), '');
  const described = facts.slugDescriptions([{ slug: 'agent.spawn', description: 'Spawn a fresh agent into any group globally. Group-scoped authority uses groups.members.spawn.' }, { slug: 'x' }]);
  assert.equal(described.get('agent.spawn'), 'Spawn a fresh agent into any group globally.');
  assert.equal(described.has('x'), false);
});

test('access alerts: on needs master, knob and a browser delivery; turning on keeps the desktop, off touches only the knob', async (t) => {
  const harness = await createPreactHarness(t);
  const alerts = await harness.importDashboardModule('js/phone-access-alerts.js');
  assert.equal(alerts.accessAlertsOn({ enabled: true, access_requests: true, delivery: 'os' }), false, 'the desktop never reaches a phone');
  assert.equal(alerts.accessAlertsOn({ enabled: true, access_requests: true, delivery: 'both' }), true);
  assert.equal(alerts.accessAlertsOn({ enabled: false, access_requests: true, delivery: 'browser' }), false);
  assert.deepEqual(alerts.accessAlertsChange({ delivery: 'os' }, true), { enabled: true, access_requests: true, delivery: 'both' });
  assert.deepEqual(alerts.accessAlertsChange({ delivery: 'browser' }, true), { enabled: true, access_requests: true, delivery: 'browser' });
  assert.deepEqual(alerts.accessAlertsChange({ delivery: 'both', enabled: true }, false), { access_requests: false });
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

test('More\'s "Alert me on access requests" turns on all three settings with one tap and asks the browser', async (t) => {
  const harness = await createPreactHarness(t);
  const { AccessAlertsEntry } = await harness.importDashboardModule('js/phone-more.js');
  let settings = { enabled: false, access_requests: false, delivery: 'os' };
  const posts = [];
  const fetchImpl = async (url, init = {}) => {
    if (init.method === 'POST') {
      const body = JSON.parse(init.body);
      posts.push(body);
      settings = { ...settings, ...body };
    }
    return { ok: true, json: async () => settings, text: async () => '' };
  };
  let asked = 0;
  const win = { isSecureContext: true, Notification: Object.assign(function Notification() {}, { requestPermission: async () => { asked++; return 'granted'; } }) };
  const toasts = [];
  const { container } = await harness.mount(harness.html`<ul><${AccessAlertsEntry} fetchImpl=${fetchImpl} win=${win} notify=${(m) => toasts.push(m)} /></ul>`);
  // Settle the settings read (a few promise hops), bounded.
  const settled = async (ok) => { for (let i = 0; i < 50 && !ok(); i++) await harness.act(async () => {}); };
  await settled(() => container.querySelector('button'));
  const row = container.querySelector('button');
  assert.match(row.textContent, /Alert me on access requests/);
  assert.equal(container.querySelector('.phone-menu-note').textContent, 'off');
  await harness.act(async () => { row.click(); });
  await settled(() => container.querySelector('.phone-menu-note')?.textContent === 'on');
  assert.deepEqual(posts, [{ enabled: true, access_requests: true, delivery: 'both' }]);
  assert.equal(asked, 1, 'the tap asks this browser for permission');
  assert.equal(container.querySelector('.phone-menu-note').textContent, 'on');
  assert.match(toasts.at(-1), /alert this phone/);
  await harness.act(async () => { container.querySelector('button').click(); });
  await settled(() => posts.length === 2);
  assert.deepEqual(posts.at(-1), { access_requests: false }, 'off leaves every other notification alone');
});
