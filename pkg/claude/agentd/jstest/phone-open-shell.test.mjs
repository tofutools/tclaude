// phone-open-shell: the phone opens a shell where an agent is working (its
// sheet) and in a group's default directory (the group page), through the
// desktop's paths, and on a peer's page only where sessions.shell lets it.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AGENT = 'agt_shellhere0001';
const member = (extra = {}) => ({
  agent_id: AGENT, conv_id: 'c1', title: 'builder', online: true, state: { status: 'idle', harness: 'claude' },
  current_dir: '/home/dev/git/tclaude-wt', startup_dir: '/home/dev/git/tclaude', ...extra,
});
const snapshotWith = (m, extra = {}) => ({ groups: [{ name: 'tclaude', default_cwd: '/home/dev/git/tclaude', members: [m] }], ...extra });

async function load(t) {
  const harness = await createPreactHarness(t);
  const [shell, model, menu, island] = await Promise.all([
    harness.importDashboardModule('js/phone-open-shell.js'),
    harness.importDashboardModule('js/phone-agents-model.js'),
    harness.importDashboardModule('js/phone-agent-menu.js'),
    harness.importDashboardModule('js/phone-agents-island.js'),
  ]);
  return { harness, shell, model, menu, island };
}

test('agent sheet: Shell here in its worktree, and its launch directory when that differs', async (t) => {
  const { shell, model, menu } = await load(t);
  const m = member();
  const snapshot = snapshotWith(m);
  const row = model.agentRow(m, 'tclaude');
  const entries = shell.agentShellEntries(snapshot, row);
  assert.deepEqual(entries.map((e) => [e.key, e.which, e.label, e.sub]), [
    ['shell', 'worktree', 'Shell here', '/home/dev/git/tclaude-wt'],
    ['shell-start', 'start', 'Shell in its launch directory', '/home/dev/git/tclaude'],
  ]);
  assert.deepEqual(entries[0].target, { agent: AGENT, group: 'tclaude', dir: '/home/dev/git/tclaude-wt' });

  // The same directory twice is one entry.
  const same = member({ startup_dir: '/home/dev/git/tclaude-wt' });
  assert.deepEqual(shell.agentShellEntries(snapshotWith(same), model.agentRow(same, 'tclaude')).map((e) => e.key), ['shell']);

  // It leads the sheet's Agent section, offline too (a shell needs no agent running).
  const offline = member({ online: false });
  const offlineModel = menu.menuModel(model.agentRow(offline, 'tclaude'), { group: { name: 'tclaude' }, name: 'tclaude' }, snapshotWith(offline));
  assert.deepEqual(offlineModel.agent.slice(0, 2).map((e) => e.key), ['shell', 'shell-start']);

  // No directory known, or a shell's own row: no entry.
  const nowhere = member({ current_dir: '', startup_dir: '' });
  assert.deepEqual(shell.agentShellEntries(snapshotWith(nowhere), model.agentRow(nowhere, 'tclaude')), []);
  const aShell = member({ state: { harness: 'shell' } });
  assert.deepEqual(shell.agentShellEntries(snapshotWith(aShell), model.agentRow(aShell, 'tclaude')), []);
});

test('agent sheet on a peer: only where the peer grants sessions.shell on the group', async (t) => {
  const { shell, model } = await load(t);
  const m = member();
  const row = model.agentRow(m, 'tclaude');
  const denied = snapshotWith(m, { peer_view: { level: 'restricted', groups: { tclaude: ['sessions.watch'] } } });
  assert.deepEqual(shell.agentShellEntries(denied, row), []);
  const granted = snapshotWith(m, { peer_view: { level: 'restricted', groups: { tclaude: ['sessions.shell'] } } });
  assert.equal(shell.agentShellEntries(granted, row)[0].target.group, 'tclaude');
});

test('openAgentShell: this node opens its directory shell; a peer page opens the peer\'s', async (t) => {
  const { shell, model } = await load(t);
  const m = member();
  const [here, start] = shell.agentShellEntries(snapshotWith(m), model.agentRow(m, 'tclaude'));
  const local = [];
  const peer = [];
  const seams = { local: async (...args) => { local.push(args); return { key: 'k' }; }, peer: async (o) => { peer.push(o); return {}; }, notify: () => {} };
  await shell.openAgentShell(here, 'builder', { ...seams, node: null });
  await shell.openAgentShell(start, 'builder', { ...seams, node: null });
  assert.deepEqual(local, [[AGENT, 'shell · builder', 'worktree', 'tclaude'], [AGENT, 'shell · builder', 'start', 'tclaude']]);
  assert.equal(peer.length, 0);

  await shell.openAgentShell(here, 'builder', { ...seams, node: { id: 'inst_peer0001' } });
  assert.equal(peer.length, 1);
  assert.deepEqual({ ...peer[0], toast: undefined }, { instance: 'inst_peer0001', group: 'tclaude', shell: 'worktree', agent: AGENT, label: 'builder', toast: undefined });

  // A terminal that does not open says so.
  const said = [];
  await shell.openAgentShell(here, 'builder', { local: async () => null, notify: (text, error) => said.push([text, error]), node: null });
  assert.match(said[0][0], /could not open/);
  assert.equal(said[0][1], true);
});

test('group page: Shell in its default directory; greyed with why without one; hidden where it cannot be', async (t) => {
  const { shell, island, harness } = await load(t);
  assert.deepEqual(shell.groupShellOffer({}, { name: 'tclaude', default_cwd: ' /home/dev/git/tclaude ' }), { dir: '/home/dev/git/tclaude' });
  assert.match(shell.groupShellOffer({}, { name: 'tclaude', default_cwd: '' }).why, /no default directory/);
  assert.equal(shell.groupShellOffer({}, { name: 'tclaude', default_cwd: '/x', archived: true }), null);
  assert.equal(shell.groupShellOffer({ peer_view: { level: 'restricted', groups: { tclaude: [] } } }, { name: 'tclaude', default_cwd: '/x' }), null);
  assert.deepEqual(shell.groupShellOffer({ peer_view: { level: 'unrestricted' } }, { name: 'tclaude', default_cwd: '/x' }), { dir: '/x' });

  // The group page renders it, and a tap opens the group's shell.
  const opened = [];
  const page = (group) => ({ name: 'tclaude', group, archived: false, crumbs: ['tclaude'], children: [], total: 0, live: 0, shown: [], offline: [], summary: [] });
  const list = { isSteward: () => false, tap: () => () => {}, more: () => () => {} };
  const render = (group) => harness.mount(harness.preact.h(island.GroupPage, {
    page: page(group), actions: { groupAction: () => {} }, list, onOpen: () => {}, spawn: () => {},
    snapshot: {}, openShell: (name) => opened.push(name),
  }));
  const withDir = await render({ name: 'tclaude', default_cwd: '/home/dev/git/tclaude' });
  const button = [...withDir.container.querySelectorAll('.phone-group-shell button')][0];
  assert.equal(button.disabled, false);
  assert.match(withDir.container.querySelector('[data-group-shell]').textContent, /In \/home\/dev\/git\/tclaude/);
  await harness.act(() => button.click());
  assert.deepEqual(opened, ['tclaude']);
  await withDir.unmount();

  const noDir = await render({ name: 'tclaude', default_cwd: '' });
  assert.equal(noDir.container.querySelector('.phone-group-shell button').disabled, true);
  assert.equal(noDir.container.querySelector('[data-group-shell]').dataset.groupShell, 'none');
});

test('openGroupShell: this node\'s group shell, or the peer\'s', async (t) => {
  const { shell } = await load(t);
  const local = [];
  const peer = [];
  await shell.openGroupShell('tclaude', { local: (name) => local.push(name), peer: (o) => peer.push(o), node: null });
  await shell.openGroupShell('tclaude', { local: (name) => local.push(name), peer: (o) => peer.push(o), node: { id: 'inst_peer0001' } });
  assert.deepEqual(local, ['tclaude']);
  assert.deepEqual(peer.map((o) => [o.instance, o.group, o.shell]), [['inst_peer0001', 'tclaude', 'group']]);
});
