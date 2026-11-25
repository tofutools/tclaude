// js/phone-steward.js: the Steward's ✦ button on the phone's Now and Agents
// views. It counts what the Steward has for the operator, opens it full
// screen in the phone terminal, and starts one that is not running.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const STEWARD = 'agt_00000000000000000000000000000009';
const OTHER = 'agt_00000000000000000000000000000001';

function snapshotWith({ online = true, status = 'idle', messages = [], access = [], roster = [], disabled = false } = {}) {
  return {
    steward_active: STEWARD,
    stewards_disabled: disabled,
    steward_roster: roster,
    groups: [
      { name: 'steward', scribe: true, members: [{ agent_id: STEWARD, conv_id: 'cs', title: 'Steward', online, state: { status } }] },
      { name: 'webshop', members: [{ agent_id: OTHER, conv_id: 'c1', title: 'checkout-ui', online: true, state: { status: 'awaiting_input' } }] },
    ],
    messages,
    access_requests: access,
  };
}

// until flushes the harness until check holds, up to a bound of rounds: a
// start awaits a fetch and its body before the sheet closes.
async function until(harness, check, what) {
  for (let round = 0; round < 50; round += 1) {
    if (check()) return;
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  }
  assert.fail(`never happened: ${what}`);
}

const decision = (id, from) => ({ id, from_agent: from, subject: 'Merge now?', options: ['Yes', 'No'] });

async function load(t) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/phone-steward.js');
  return { harness, mod };
}

test('the badge counts the Steward\'s open decisions and the requests it waits on, no one else\'s', async (t) => {
  const { mod } = await load(t);
  const fab = mod.stewardFab(snapshotWith({
    messages: [decision(1, STEWARD), decision(2, OTHER), { ...decision(3, STEWARD), decision: 'Yes' }],
    access: [{ id: 'r1', agent_id: STEWARD, status: 'pending' }, { id: 'r2', agent_id: OTHER, status: 'pending' }],
  }));
  assert.equal(fab.count, 2);
  assert.equal(fab.attention, true);
  assert.match(mod.fabLabel(fab), /2 waiting for you/);
});

test('a Steward at a prompt asks for attention with nothing to count', async (t) => {
  const { mod } = await load(t);
  const fab = mod.stewardFab(snapshotWith({ status: 'awaiting_input' }));
  assert.equal(fab.count, 0);
  assert.equal(fab.attention, true);
  const calm = mod.stewardFab(snapshotWith({ status: 'working' }));
  assert.equal(calm.attention, false);
  assert.match(mod.fabLabel(calm), /working\. Open it/);
});

test('no button with Stewards turned off and none running; one to start it otherwise', async (t) => {
  const { mod } = await load(t);
  assert.equal(mod.stewardFab(snapshotWith({ online: false, disabled: true })), null);
  assert.ok(mod.stewardFab(snapshotWith({ online: true, disabled: true })), 'a running one keeps its button');
  const off = mod.stewardFab(snapshotWith({ online: false, roster: [{ agent_id: STEWARD, name: 'Steward', seat: 'active', online: false, last_ran: '' }] }));
  assert.equal(off.agent, null);
  assert.equal(off.returning.agentId, STEWARD);
  assert.match(mod.fabLabel(off), /not running/);
  assert.equal(mod.stewardFab(null), null);
});

test('nextShrunk: down shrinks, up restores, the top is always full size, jitter changes nothing', async (t) => {
  const { mod } = await load(t);
  assert.equal(mod.nextShrunk(false, 100, 140), true);
  assert.equal(mod.nextShrunk(true, 140, 100), false);
  assert.equal(mod.nextShrunk(false, 100, 105), false);
  assert.equal(mod.nextShrunk(true, 100, 95), true);
  assert.equal(mod.nextShrunk(true, 40, 5), false);
});

async function mountFab(t, snapshot, fetchImpl = async () => ({ ok: true, json: async () => ({}) })) {
  const { harness, mod } = await load(t);
  const sheets = await harness.importDashboardModule('js/phone-sheet.js');
  t.after(() => sheets.closePhoneSheet());
  const { signal } = harness.signals;
  const snap = signal(snapshot);
  const calls = [];
  const actions = { openTerminal: (agent) => calls.push(['open', agent.agentId]), notify: (text, error) => calls.push(['notify', text, !!error]) };
  const Host = () => harness.html`<${mod.StewardFab} snapshot=${snap.value} actions=${actions} fetchImpl=${fetchImpl} doc=${harness.document} /><${sheets.PhoneSheetHost} />`;
  const mounted = await harness.mount(harness.html`<${Host} />`);
  const button = () => mounted.container.querySelector('.phone-steward-fab');
  return { harness, mounted, snap, calls, button, root: harness.document.documentElement };
}

test('a tap opens a running Steward full screen in its terminal; the views keep room for the button', async (t) => {
  const { harness, calls, button, root } = await mountFab(t, snapshotWith({ messages: [decision(1, STEWARD)] }));
  assert.ok(button().classList.contains('attention'));
  assert.equal(button().querySelector('.phone-steward-fab-badge').textContent, '1');
  assert.ok('phoneFab' in root.dataset);
  await harness.act(() => button().click());
  assert.deepEqual(calls, [['open', STEWARD]]);
});

test('with no Steward running, a tap offers Resume and Start fresh; Resume opens it once it runs', async (t) => {
  const requests = [];
  const fetchImpl = async (url, init) => { requests.push([url, init?.method]); return { ok: true, json: async () => ({ action: 'resumed' }) }; };
  const roster = [{ agent_id: STEWARD, name: 'Steward', seat: 'active', online: false, last_ran: '' }];
  const { harness, mounted, snap, calls, button } = await mountFab(t, snapshotWith({ online: false, roster }), fetchImpl);
  assert.ok(button().classList.contains('offline'));
  await harness.act(() => button().click());
  const dialog = mounted.container.querySelector('[role="dialog"]');
  const labels = [...dialog.querySelectorAll('button')].map((b) => b.textContent.trim());
  assert.deepEqual(labels, ['Resume', 'Start fresh']);
  await harness.act(async () => { dialog.querySelector('button').click(); });
  await until(harness, () => !mounted.container.querySelector('[role="dialog"]'), 'the sheet closes');
  assert.deepEqual(requests, [[`/api/agents/${STEWARD}/resume`, 'POST']]);
  assert.ok(!calls.some(([kind]) => kind === 'open'), 'nothing to open until it runs');
  // While it comes up, a tap offers no second start.
  assert.match(button().getAttribute('aria-label'), /starting/);
  await harness.act(() => button().click());
  assert.equal(mounted.container.querySelector('[role="dialog"]'), null, 'no second start sheet');
  assert.equal(requests.length, 1);
  await harness.act(() => { snap.value = snapshotWith({ online: true, roster }); });
  assert.deepEqual(calls.filter(([kind]) => kind === 'open'), [['open', STEWARD]]);
});

test('with none to resume, Summon asks for a Steward', async (t) => {
  const requests = [];
  const fetchImpl = async (url, init) => { requests.push([url, JSON.parse(init.body).name]); return { ok: true, json: async () => ({ agent_id: STEWARD }) }; };
  const { harness, mounted, calls } = await mountFab(t, snapshotWith({ online: false }), fetchImpl);
  await harness.act(() => mounted.container.querySelector('.phone-steward-fab').click());
  const dialog = mounted.container.querySelector('[role="dialog"]');
  assert.deepEqual([...dialog.querySelectorAll('button')].map((b) => b.textContent.trim()), ['Summon']);
  await harness.act(async () => { dialog.querySelector('button').click(); });
  await until(harness, () => !mounted.container.querySelector('[role="dialog"]'), 'the sheet closes');
  assert.deepEqual(requests, [['/api/scribe', 'steward']]);
  assert.ok(calls.some(([kind, text]) => kind === 'notify' && /Summoning/.test(text)));
});
