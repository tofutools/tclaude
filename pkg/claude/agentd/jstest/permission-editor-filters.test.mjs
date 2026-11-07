import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

// The permission editor's composable filters: Set, Holds, Kind and the flag
// toggles AND together, and with the slug text.

const FLEET = { slug: 'fleet.read', description: 'see the fleet', read_only: true };
const ROSTER = { slug: 'groups.roster.read', description: 'roster', read_only: true };
const DIRECT = { slug: 'message.direct', description: 'message any agent' };
const SPAWN = { slug: 'agent.spawn', description: 'spawn agents', risky: true };
const RENAME = { slug: 'groups.rename', description: 'rename a group', owner_implied: true };

function snapshot() {
  return {
    agents: [{ agent_id: 'agt_sender', conv_id: 'conv-s', title: 'sender', online: true, groups: [] }],
    groups: [],
    permissions: {
      defaults: ['fleet.read'],
      overrides: { 'conv-s': { 'message.direct': 'grant', 'groups.roster.read': 'deny' } },
      scopes: {}, scope_dim_options: {}, unreadable_scopes: {},
    },
    slugs: [FLEET, ROSTER, DIRECT, SPAWN, RENAME],
    sudo: [],
  };
}

async function openEditor(harness) {
  const [{ createMessageAccessDialogState }, { MessageAccessDialogApp }] = await Promise.all([
    harness.importDashboardModule('js/message-access-dialog-state.js'),
    harness.importDashboardModule('js/message-access-dialog-island.js'),
  ]);
  const state = createMessageAccessDialogState();
  state.openAgentPermissions({ conv: 'conv-s', label: 'sender' });
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const actions = { sendMessage: async () => {}, replyHuman: async () => {}, grantSudo: async () => {}, savePermissions: async () => {} };
  const mounted = await harness.mount(harness.html`<${MessageAccessDialogApp} state=${state} actions=${actions}
    snapshot=${snapshot()} confirmDiscard=${async () => true}/>`, host);
  return { host, mounted };
}

test('facets compose: set here, inherited, holds, kind, flags and the text filter', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, mounted } = await openEditor(harness);
  const slugs = () => [...host.querySelectorAll('.perm-row')].map((row) => row.dataset.slug);
  const pick = (facet, option) => harness.act(() => {
    host.querySelector(`.perm-facet[data-facet="${facet}"] [data-option="${option}"]`).click();
  });
  const flag = (key) => harness.act(() => { host.querySelector(`[data-flag="${key}"]`).click(); });
  const count = () => host.querySelector('#perm-edit-facet-count').textContent;

  assert.equal(slugs().length, 5);
  assert.equal(count(), '5 slugs');
  assertAbsent(host.querySelector('#perm-edit-facets-clear'), 'nothing to clear yet');

  await pick('set', 'here');
  assert.deepEqual(slugs(), ['groups.roster.read', 'message.direct'], 'Grant and Deny set in this editor');
  await pick('set', 'here-or-inherited');
  assert.deepEqual(slugs(), ['fleet.read', 'groups.roster.read', 'message.direct'], 'plus what a default gives');

  await pick('kind', 'write');
  assert.deepEqual(slugs(), ['message.direct']);
  await pick('kind', 'read');
  assert.deepEqual(slugs(), ['fleet.read', 'groups.roster.read']);
  await pick('access', 'yes');
  assert.deepEqual(slugs(), ['fleet.read'], 'the denied read is not held');
  assert.equal(count(), '1 of 5');

  await harness.act(() => { host.querySelector('#perm-edit-facets-clear').click(); });
  assert.equal(slugs().length, 5);

  await pick('set', 'unsaved');
  assert.deepEqual(slugs(), []);
  await pick('set', 'any');
  await harness.act(() => { host.querySelector('[data-slug="agent.spawn"] [data-effect="grant"]').click(); });
  await pick('set', 'unsaved');
  assert.deepEqual(slugs(), ['agent.spawn'], 'a change made here shows as unsaved');
  await pick('set', 'any');

  await flag('risky');
  assert.deepEqual(slugs(), ['agent.spawn']);
  await flag('risky');
  await flag('owner');
  assert.deepEqual(slugs(), ['groups.rename']);
  await flag('owner');

  await pick('kind', 'read');
  await harness.input(host.querySelector('#perm-edit-filter'), 'roster');
  assert.deepEqual(slugs(), ['groups.roster.read'], 'the text filter ANDs with the facets');
  await mounted.unmount();
});
