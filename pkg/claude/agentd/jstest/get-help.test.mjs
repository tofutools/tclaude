// Tests for "Get help" (tcl-z32qq1): a new helper or an existing agent, opened in
// a regular overlay chat window (or a terminal tab without the companion), and
// nothing granted or started without the operator's confirmation.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const wait = (ms = 0) => new Promise((r) => setTimeout(r, ms));

// A daemon answering the scribe, permissions and operator-message calls.
function fakeDaemon({ scribe = { agent_id: 'agt_help1', conv_id: 'conv-help1', name: 'process-scribe-1', reused: false } } = {}) {
  const calls = [];
  const fetchImpl = async (url, init = {}) => {
    calls.push({ path: String(url), method: init.method || 'GET', body: init.body ? JSON.parse(init.body) : null });
    const body = String(url) === '/api/scribe' ? scribe : {};
    return { ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) };
  };
  return { calls, fetchImpl };
}

async function setup(t, { companion = true } = {}) {
  const harness = await createPreactHarness(t);
  const [help, facade, terminals] = await Promise.all([
    harness.importDashboardModule('js/get-help.js'),
    harness.importDashboardModule('js/companion-facade.js'),
    harness.importDashboardModule('js/terminals-tab.js'),
  ]);
  const opened = [];
  if (companion) t.after(facade.registerCompanionController({ open: (agentId, options) => { opened.push({ agentId, ...options }); return true; } }));
  const panes = [];
  t.after(terminals.registerTerminalShellController({ openPane: (seed) => { panes.push(seed); return { key: seed.key }; } }));
  const asked = [];
  const confirmWith = (answer) => async (options) => { asked.push(options); return answer; };
  return { harness, help, opened, panes, asked, confirmWith };
}

const HELP = {
  name: 'process-scribe', slugs: ['process.templates.read', 'process.templates.manage'], scope: { kind: 'process-template', id: 'release-train' },
  exclusive: true, brief: 'Help with Release train.', taskRef: { url: 'http://dash/automations/processes/templates/release-train', label: 'Release train' },
  confirmation: { title: 'Grant process-scribe access?', body: 'Grants process.templates.read and .manage.', okLabel: 'Grant & summon' },
};

test('a new helper starts in the background after the grant confirmation and opens in its chat window', async (t) => {
  const { help, opened, panes, asked, confirmWith } = await setup(t);
  const { calls, fetchImpl } = fakeDaemon();
  const result = await help.summonHelper({ ...HELP, confirm: confirmWith(true), fetchImpl });
  assert.equal(asked.length, 1);
  assert.equal(asked[0].title, 'Grant process-scribe access?');
  assert.deepEqual(calls.map((call) => `${call.method} ${call.path}`), ['POST /api/scribe']);
  const sent = calls[0].body;
  assert.equal(sent.background, true, 'no terminal opens on its own');
  assert.deepEqual(sent.slugs, HELP.slugs);
  assert.deepEqual(sent.scope, HELP.scope);
  assert.equal(sent.exclusive, true);
  assert.equal(sent.task_ref_url, HELP.taskRef.url);
  assert.deepEqual(opened, [{ agentId: 'agt_help1', starting: true }], 'a fresh helper opens in a regular chat window, starting');
  assert.deepEqual(panes, []);
  assert.deepEqual(result, { agentId: 'agt_help1', convId: 'conv-help1', name: 'process-scribe-1', reused: false, opened: 'chat' });
});

test('a reused helper opens as it is, not as starting', async (t) => {
  const { help, opened, confirmWith } = await setup(t);
  const { fetchImpl } = fakeDaemon({ scribe: { agent_id: 'agt_help1', conv_id: 'conv-help1', name: 'process-scribe-1', reused: true } });
  const result = await help.summonHelper({ ...HELP, confirm: confirmWith(true), fetchImpl });
  assert.equal(result.reused, true);
  assert.deepEqual(opened, [{ agentId: 'agt_help1', starting: false }]);
});

test('a page that names no confirmation still gets one; only an explicit confirmed skips it', async (t) => {
  const { help, asked, confirmWith } = await setup(t);
  const { calls, fetchImpl } = fakeDaemon();
  const { confirmation: _none, ...bare } = HELP;
  assert.equal(await help.summonHelper({ ...bare, confirm: confirmWith(false), fetchImpl }), null);
  assert.equal(asked.at(-1).title, 'Start a helper?');
  assert.deepEqual(calls, []);
  await help.summonHelper({ ...bare, confirmed: true, confirm: confirmWith(false), fetchImpl });
  assert.equal(calls.length, 1);
});

test('a server refusal in plain text reaches the operator as written', async (t) => {
  const { help, confirmWith } = await setup(t);
  const fetchImpl = async () => ({ ok: false, status: 409, statusText: 'Conflict', text: async () => 'an external Steward takes no grants here\n' });
  const helper = { id: 'agt_s', label: 'steward', effective: [] };
  await assert.rejects(help.askExistingAgent({ helper, slugs: ['process.templates.read'], brief: 'x', confirm: confirmWith(true), fetchImpl }), /an external Steward takes no grants here/);
});

test('a cancelled confirmation starts and grants nothing', async (t) => {
  const { help, opened, confirmWith } = await setup(t);
  const { calls, fetchImpl } = fakeDaemon();
  assert.equal(await help.summonHelper({ ...HELP, confirm: confirmWith(false), fetchImpl }), null);
  assert.deepEqual(calls, []);
  assert.deepEqual(opened, []);
});

test('without the companion a helper opens as a terminal tab', async (t) => {
  const { help, panes, confirmWith } = await setup(t, { companion: false });
  const { fetchImpl } = fakeDaemon();
  const result = await help.summonHelper({ ...HELP, confirm: confirmWith(true), fetchImpl });
  await wait();
  assert.equal(result.opened, 'terminal');
  assert.equal(panes.length, 1);
  assert.equal(panes[0].agent, 'agt_help1');
  assert.equal(panes[0].key, 'window:agt_help1');
});

test('asking an existing agent grants only what it lacks, after the same confirmation, then briefs it', async (t) => {
  const { help, opened, asked, confirmWith } = await setup(t);
  const helpers = help.liveHelpers({ agents: [
    { agent_id: 'agt_b', conv_id: 'conv-b', title: 'builder', online: true, groups: ['tclaude'], effective: ['process.templates.read'] },
    { agent_id: 'agt_a', conv_id: 'conv-a', title: 'architect', online: true, groups: [], effective: HELP.slugs },
    { agent_id: 'agt_z', conv_id: 'conv-z', title: 'sleeper', online: false },
  ] });
  assert.deepEqual(helpers.map((helper) => helper.label), ['architect', 'builder'], 'running agents only, by name');

  const { calls, fetchImpl } = fakeDaemon();
  const confirmation = (missing) => ({ title: 'Grant process-scribe access?', body: `Grants ${missing.join(', ')}` });
  const declined = await help.askExistingAgent({ helper: helpers[1], slugs: HELP.slugs, brief: 'Help with Release train.', confirmation, confirm: confirmWith(false), fetchImpl });
  assert.equal(declined, null);
  assert.deepEqual(calls, [], 'declined, nothing is granted or sent');
  assert.equal(asked[0].body, 'Grants process.templates.manage');

  const result = await help.askExistingAgent({ helper: helpers[1], slugs: HELP.slugs, subject: 'Process help', brief: 'Help with Release train.', confirmation, confirm: confirmWith(true), fetchImpl });
  assert.deepEqual(calls.map((call) => `${call.method} ${call.path}`), ['POST /api/permissions', 'POST /api/operator-message']);
  assert.deepEqual(calls[0].body, { conv: 'agt_b', overrides: { 'process.templates.manage': 'grant' } }, 'only the missing slug, unscoped wording untouched');
  assert.deepEqual(calls[1].body, { to: 'conv-b', subject: 'Process help', body: 'Help with Release train.' });
  assert.deepEqual(result.granted, ['process.templates.manage']);
  assert.deepEqual(opened.at(-1), { agentId: 'agt_b', starting: false });

  // One that holds everything is asked nothing, and granted nothing.
  calls.length = 0;
  const before = asked.length;
  await help.askExistingAgent({ helper: helpers[0], slugs: HELP.slugs, brief: 'Help.', confirmation, confirm: confirmWith(true), fetchImpl });
  assert.equal(asked.length, before);
  assert.deepEqual(calls.map((call) => call.path), ['/api/operator-message']);
});

test('the Get help menu offers a new helper, an existing agent, and the page actions', async (t) => {
  const { harness, opened, asked, confirmWith } = await setup(t);
  const { GetHelpMenu } = await harness.importDashboardModule('js/get-help-menu.js');
  const { calls, fetchImpl } = fakeDaemon();
  const snapshot = harness.signals.signal({ agents: [{ agent_id: 'agt_b', conv_id: 'conv-b', title: 'builder', online: true, groups: [], effective: [] }] });
  const helpers = [];
  let explained = 0;
  let guarded = true;
  const mounted = await harness.mount(harness.html`<${GetHelpMenu} help=${HELP} snapshot=${snapshot} fetchImpl=${fetchImpl} confirm=${confirmWith(true)}
    before=${async () => guarded} onHelper=${(helper) => helpers.push(helper)}
    actions=${[{ key: 'explain', label: 'Explain this node', run: () => { explained += 1; } }]} />`);
  const { container } = mounted;
  const toggle = () => harness.act(async () => { harness.fireEvent(container.querySelector('.get-help-toggle'), 'click'); await wait(); });
  const pick = (key) => harness.act(async () => { harness.fireEvent(container.querySelector(`[data-get-help="${key}"]`), 'click'); await wait(10); });
  await toggle();
  assert.deepEqual([...container.querySelectorAll('.get-help-menu [data-get-help]')].map((item) => item.textContent),
    ['New helper', 'Ask an existing agent…', 'Explain this node']);
  await pick('explain');
  assert.equal(explained, 1);

  // A page whose guard refuses (unsaved edits kept) starts nothing.
  guarded = false;
  await toggle();
  await pick('new');
  assert.deepEqual(calls, []);
  guarded = true;
  await toggle();
  await pick('new');
  assert.deepEqual(calls.map((call) => call.path), ['/api/scribe']);
  assert.equal(helpers[0].agentId, 'agt_help1', 'the page hears the helper it brought in');

  await toggle();
  await pick('ask');
  const row = harness.document.querySelector('[data-get-help-agent="agt_b"] button');
  assert.ok(row, 'the picker lists the running agents');
  await harness.act(async () => { harness.fireEvent(row, 'click'); await wait(10); });
  assert.deepEqual(calls.slice(1).map((call) => call.path), ['/api/permissions', '/api/operator-message']);
  assert.equal(asked.at(-1).title, 'Grant permissions?', 'a page with no askConfirmation still asks before granting');
  assert.equal(helpers[1].agentId, 'agt_b');
  assert.deepEqual(opened.at(-1), { agentId: 'agt_b', starting: false });
  await mounted.unmount();
});
