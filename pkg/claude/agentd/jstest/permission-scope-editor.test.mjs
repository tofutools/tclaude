import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

// The permission editor's scope chips and dimension pickers (TCL-1072).
//
// Everything these tests assert about WHICH dimensions appear comes from the
// snapshot the daemon sent — scope_dims on the slug, scope_dim_options for the
// pickers. Nothing in the frontend enumerates dimensions, which is what lets a
// dimension added by a later phase become editable with no change here; the
// "unknown dimension" case below is the standing proof of that.

function scopeSnapshot({ slugs = [], scopes = {}, dimOptions = {}, unreadable = [] } = {}) {
  return {
    agents: [{ agent_id: 'agt_sender', conv_id: 'conv-s', title: 'sender', online: true, groups: [] }],
    groups: [],
    permissions: {
      defaults: [],
      overrides: { 'conv-s': Object.fromEntries(slugs.map((slug) => [slug.slug, 'grant'])) },
      scopes: { 'conv-s': scopes },
      scope_dim_options: dimOptions,
      unreadable_scopes: { 'conv-s': unreadable },
    },
    slugs,
    sudo: [],
  };
}

async function openEditor(harness, snapshot, actions) {
  const [{ createMessageAccessDialogState }, { MessageAccessDialogApp }] = await Promise.all([
    harness.importDashboardModule('js/message-access-dialog-state.js'),
    harness.importDashboardModule('js/message-access-dialog-island.js'),
  ]);
  const state = createMessageAccessDialogState();
  state.openAgentPermissions({ conv: 'conv-s', label: 'sender' });
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${MessageAccessDialogApp} state=${state} actions=${actions}
    snapshot=${snapshot} confirmDiscard=${async () => true}/>`, host);
  return { host, mounted, state };
}

// The test DOM exposes select.value as a getter only, so a choice is made the
// way a browser records one: by moving the selected attribute.
function pickOption(harness, select, value) {
  [...select.options].forEach((option) => option.removeAttribute('selected'));
  select.querySelector(`option[value="${value}"]`).setAttribute('selected', '');
  return select.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
}

const noopActions = {
  sendMessage: async () => {}, replyHuman: async () => {},
  grantSudo: async () => {}, savePermissions: async () => {},
};

const SPAWN = { slug: 'groups.spawn', description: 'spawn', owner_implied: false, scope_dims: ['group', 'spawn_profile'] };
const CLIPBOARD = { slug: 'human.clipboard', description: 'clipboard', owner_implied: false };

test('a scoped grant renders its chips and an unscoped one says so', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN, CLIPBOARD],
    scopes: { 'groups.spawn': { group: ['alpha', 'beta'] } },
  }), noopActions);

  const chips = host.querySelectorAll('[data-slug="groups.spawn"] .perm-scope-chip');
  assert.equal(chips.length, 1, 'one chip per constrained dimension');
  assert.equal(chips[0].textContent.replace(/\s+/g, ' ').trim(), 'group=alpha, beta');

  // A slug that declares no dimensions gets no scope affordance at all, so a
  // row that cannot be narrowed reads exactly as it did before scopes existed.
  assertAbsent(host.querySelector('[data-slug="human.clipboard"] .perm-scope-chips'));
  await mounted.unmount();
});

test('an unscoped grant is labelled unscoped rather than left blank', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, mounted } = await openEditor(harness, scopeSnapshot({ slugs: [SPAWN] }), noopActions);
  const chip = host.querySelector('[data-slug="groups.spawn"] .perm-scope-chip.unscoped');
  assert.ok(chip, 'a granted, dimensioned, unnarrowed slug must SAY it applies everywhere');
  assert.match(chip.textContent, /unscoped/);
  await mounted.unmount();
});

test('the drawer offers one editor per advertised dimension and saves what was picked', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN],
    dimOptions: {
      group: { values: ['alpha', 'beta'] },
      spawn_profile: { values: ['p1'] },
    },
  }), { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push({ selection, scopes }); } });

  await harness.act(() => { host.querySelector('[data-slug="groups.spawn"] .perm-scope-toggle').click(); });
  const drawer = host.querySelector('.perm-scope-drawer[data-slug="groups.spawn"]');
  assert.ok(drawer);
  assert.deepEqual(
    Array.from(drawer.querySelectorAll('.perm-scope-dim')).map((row) => row.dataset.dim),
    ['group', 'spawn_profile'],
    'the drawer renders exactly the dimensions the slug advertised, in that order',
  );

  const groupSelect = drawer.querySelector('.perm-scope-dim[data-dim="group"] .perm-scope-add');
  await harness.act(() => pickOption(harness, groupSelect, 'beta'));
  assert.match(host.querySelector('[data-slug="groups.spawn"] .perm-scope-chip').textContent, /group=beta/,
    'the row chips track the drawer edit live');

  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0].scopes, { 'groups.spawn': { group: ['beta'] } });
  await mounted.unmount();
});

const TEMPLATES = {
  slug: 'process.runs.manage', description: 'runs', owner_implied: false, scope_dims: ['process_template'],
};

test('a value can be typed for a dimension the daemon has no catalogue for', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [TEMPLATES],
    dimOptions: { process_template: {} },
  }), { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push(scopes); } });

  await harness.act(() => { host.querySelector('[data-slug="process.runs.manage"] .perm-scope-toggle').click(); });
  // No catalogue means no picker to offer — free text is the whole control,
  // and it is the same input the CLI's --scope takes.
  assertAbsent(host.querySelector('.perm-scope-dim[data-dim="process_template"] .perm-scope-add'));
  const free = host.querySelector('.perm-scope-dim[data-dim="process_template"] .perm-scope-free');
  await harness.input(free, 'release-train');
  await harness.act(() => harness.fireEvent(free, 'keydown', { key: 'Enter' }));
  assert.match(host.querySelector('[data-slug="process.runs.manage"] .perm-scope-chip').textContent,
    /process_template=release-train/);

  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0], { 'process.runs.manage': { process_template: ['release-train'] } });
  await mounted.unmount();
});

test('removing the last value of a dimension returns the grant to unscoped', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [TEMPLATES],
    scopes: { 'process.runs.manage': { process_template: ['release-train'] } },
  }), { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push(scopes); } });

  await harness.act(() => { host.querySelector('[data-slug="process.runs.manage"] .perm-scope-toggle').click(); });
  await harness.act(() => { host.querySelector('.perm-scope-dim[data-dim="process_template"] .perm-scope-chip .x').click(); });
  assert.ok(host.querySelector('[data-slug="process.runs.manage"] .perm-scope-chip.unscoped'),
    'an emptied dimension is absent, not an empty list the daemon would reject');
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0], {});
  await mounted.unmount();
});

test('a dimension this build has never heard of is still editable', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const FUTURE = {
    slug: 'git.push', description: 'push', owner_implied: false, scope_dims: ['git_remote'],
  };
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [FUTURE],
    dimOptions: { git_remote: { values: ['origin'], selectors: ['@launch-repo'] } },
  }), { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push(scopes); } });

  await harness.act(() => { host.querySelector('[data-slug="git.push"] .perm-scope-toggle').click(); });
  const select = host.querySelector('.perm-scope-dim[data-dim="git_remote"] .perm-scope-add');
  assert.deepEqual(Array.from(select.options).map((option) => option.value), ['', 'origin', '@launch-repo'],
    'advertised values and selectors are both offered, with no frontend knowledge of the dimension');
  await harness.act(() => pickOption(harness, select, '@launch-repo'));
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0], { 'git.push': { git_remote: ['@launch-repo'] } });
  await mounted.unmount();
});

test('scopes are dropped for a slug moved off Grant', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN],
    scopes: { 'groups.spawn': { group: ['alpha'] } },
  }), { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push({ selection, scopes }); } });

  await harness.act(() => { host.querySelector('[data-slug="groups.spawn"] [data-effect="deny"]').click(); });
  assertAbsent(host.querySelector('[data-slug="groups.spawn"] .perm-scope-chips'),
    'a deny is unconditional, so it shows no scope to edit');
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0].selection, { 'groups.spawn': 'deny' });
  assert.deepEqual(saved[0].scopes, {}, 'no scope rides a deny — the daemon would refuse it');
  await mounted.unmount();
});

test('a grant whose stored scope cannot be decoded is never shown as unscoped', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN],
    unreadable: ['groups.spawn'],
  }), { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push(scopes); } });

  const chip = host.querySelector('[data-slug="groups.spawn"] .perm-scope-chip');
  assert.ok(chip, 'the row must say something about the scope it cannot read');
  assert.match(chip.textContent, /unreadable scope/);
  assert.equal(chip.classList.contains('unscoped'), false,
    'a grant that authorizes nothing must never read as one that authorizes everything');
  // No editor either: the operator cannot narrow what the build cannot show,
  // and the daemon refuses to overwrite the row from here.
  assertAbsent(host.querySelector('[data-slug="groups.spawn"] .perm-scope-toggle'));
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0], {}, 'saving sends no scope for it, and the daemon keeps the stored one');
  await mounted.unmount();
});

test('a stored dimension the slug no longer accepts can still be removed', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  // groups.spawn declares group + spawn_profile; the stored scope also carries
  // a dimension a past build allowed. Without an editor for it, every save
  // would be refused by the daemon and the operator could not fix it here.
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN],
    scopes: { 'groups.spawn': { group: ['alpha'], legacy_dim: ['x'] } },
  }), { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push(scopes); } });

  await harness.act(() => { host.querySelector('[data-slug="groups.spawn"] .perm-scope-toggle').click(); });
  const stale = host.querySelector('.perm-scope-dim[data-dim="legacy_dim"]');
  assert.ok(stale, 'a stored dimension gets an editor even when the slug stopped declaring it');
  assert.ok(stale.querySelector('.perm-scope-stale'), 'and is flagged as no longer accepted');
  await harness.act(() => { stale.querySelector('.perm-scope-chip .x').click(); });
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0], { 'groups.spawn': { group: ['alpha'] } });
  await mounted.unmount();
});

test('a scope value cannot smuggle markup into the row chips', async (t) => {
  const harness = await createPreactHarness(t);
  const evil = '<img src=x onerror="globalThis.__pwned = true">';
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN],
    scopes: { 'groups.spawn': { group: [evil] } },
  }), noopActions);
  const chip = host.querySelector('[data-slug="groups.spawn"] .perm-scope-chip');
  assert.equal(chip.querySelector('img'), null, 'a scope value is text, never markup');
  assert.ok(chip.textContent.includes(evil), 'and it is shown verbatim so the operator sees what is stored');
  assert.equal(globalThis.__pwned, undefined);
  await mounted.unmount();
});
