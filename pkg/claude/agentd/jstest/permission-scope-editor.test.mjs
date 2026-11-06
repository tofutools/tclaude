import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';
import { comboKeys, comboPick } from './combobox-helpers.mjs';

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

async function openEditor(harness, snapshot, actions, descriptor = null) {
  const [{ createMessageAccessDialogState }, { MessageAccessDialogApp }] = await Promise.all([
    harness.importDashboardModule('js/message-access-dialog-state.js'),
    harness.importDashboardModule('js/message-access-dialog-island.js'),
  ]);
  const state = createMessageAccessDialogState();
  if (descriptor?.mode === 'group') state.openGroupPermissions(descriptor);
  else if (descriptor?.mode === 'buffer') state.openBufferedPermissions(descriptor);
  else state.openAgentPermissions({ conv: 'conv-s', label: 'sender' });
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${MessageAccessDialogApp} state=${state} actions=${actions}
    snapshot=${snapshot} confirmDiscard=${async () => true}/>`, host);
  return { host, mounted, state };
}

// The test DOM exposes select.value as a getter only, so a choice is made the
// way a browser records one: by moving the selected attribute.
// dimBox is one dimension's type-to-filter box (its .perm-scope-dim row);
// dimInput the box's input.
const dimBox = (host, dim) => host.querySelector(`.perm-scope-dim[data-dim="${dim}"]`);
const dimInput = (host, dim) => dimBox(host, dim).querySelector('input[role="combobox"]');

const noopActions = {
  sendMessage: async () => {}, replyHuman: async () => {},
  grantSudo: async () => {}, savePermissions: async () => {},
};

const SPAWN = { slug: 'groups.members.spawn', description: 'spawn', owner_implied: false, scope_dims: ['group', 'spawn_profile'] };
const CLIPBOARD = { slug: 'human.clipboard', description: 'clipboard', owner_implied: false };

test('group and buffered profile editors use the same scoped permission controls', async (t) => {
  const harness = await createPreactHarness(t);
  const snapshot = scopeSnapshot({
    slugs: [SPAWN],
    dimOptions: { group: { values: ['alpha', 'beta'] }, spawn_profile: { values: ['reviewer'] } },
  });

  const groupSaved = [];
  let opened = await openEditor(harness, snapshot, {
    ...noopActions,
    savePermissions: async (descriptor, selection, scopes, ownerScopes) => {
      groupSaved.push({ descriptor, selection, scopes, ownerScopes });
    },
  }, { mode: 'group', group: 'alpha', grants: ['groups.members.spawn'], scopes: {
    'groups.members.spawn': { group: ['alpha'] },
  } });
  assert.match(opened.host.querySelector('[data-slug="groups.members.spawn"] .perm-scope-chip').textContent, /group=alpha/);
  await harness.act(() => opened.host.querySelector('[data-slug="groups.members.spawn"] button.perm-scope-twisty').click());
  await comboPick(harness, dimBox(opened.host, 'group'), 'beta');
  await harness.act(async () => { opened.host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(groupSaved[0].scopes, { 'groups.members.spawn': { group: ['alpha', 'beta'] } });
  assert.equal(groupSaved[0].ownerScopes, null, 'grant scopes are separate from owner-bypass narrowing');
  await opened.mounted.unmount();

  const bufferSaved = [];
  opened = await openEditor(harness, snapshot, {
    ...noopActions,
    savePermissions: async (descriptor, selection, scopes) => bufferSaved.push({ selection, scopes }),
  }, { mode: 'buffer', overrides: {
    'groups.members.spawn': { effect: 'grant', scope: { spawn_profile: ['reviewer'] } },
  } });
  assert.match(opened.host.querySelector('[data-slug="groups.members.spawn"] .perm-scope-chip').textContent,
    /spawn_profile=reviewer/);
  await harness.act(async () => { opened.host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(bufferSaved[0], {
    selection: { 'groups.members.spawn': 'grant' },
    scopes: { 'groups.members.spawn': { spawn_profile: ['reviewer'] } },
  });
  await opened.mounted.unmount();
});

test('role grants use the shared scoped editor in grant-only regular and wizard modes', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const opened = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN, CLIPBOARD],
    dimOptions: { group: { values: ['dev', 'ops'] }, spawn_profile: { values: [] } },
  }), {
    ...noopActions,
    savePermissions: async (descriptor, selection, scopes) => saved.push({ descriptor, selection, scopes }),
  }, {
    mode: 'buffer', grantOnly: true, subject: 'role', label: 'lead',
    overrides: { 'groups.members.spawn': { effect: 'grant', scope: { group: ['dev'] } } },
  });

  assert.match(opened.host.querySelector('.perm-edit-title-regular').textContent, /role permissions/i);
  assert.match(opened.host.querySelector('.perm-edit-title-wizard').textContent, /Class Boons/i,
    'the shared editor retains its wizard-mode title');
  assert.match(opened.host.querySelector('.perm-edit-banner .perm-edit-banner-wizard')?.textContent
    || opened.host.querySelector('.perm-edit-banner').textContent, /CLASS BOONS/i);
  assertAbsent(opened.host.querySelector('[data-effect="deny"]'));
  assert.match(opened.host.querySelector('[data-slug="groups.members.spawn"] .perm-scope-chip').textContent,
    /group=dev/);

  await harness.act(() => opened.host.querySelector('[data-slug="groups.members.spawn"] button.perm-scope-twisty').click());
  await comboPick(harness, dimBox(opened.host, 'group'), 'ops');
  await harness.act(async () => { opened.host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.equal(saved.length, 1);
  assert.equal(saved[0].selection['groups.members.spawn'], 'grant');
  assert.deepEqual(saved[0].scopes, { 'groups.members.spawn': { group: ['dev', 'ops'] } });
  await opened.mounted.unmount();
});

test('a buffered role save preserves an unknown slug and scope fail-closed', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const opened = await openEditor(harness, scopeSnapshot({ slugs: [CLIPBOARD] }), {
    ...noopActions,
    savePermissions: async (descriptor, selection, scopes) => saved.push({ selection, scopes }),
  }, {
    mode: 'buffer', grantOnly: true, subject: 'role', label: 'future-role',
    overrides: {
      'future.permission': { effect: 'grant', scope: { future_dimension: ['narrow'] } },
    },
  });

  assertAbsent(opened.host.querySelector('[data-slug="future.permission"]'),
    'unknown slugs have no invented registry metadata or editable row');
  await harness.act(async () => { opened.host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.equal(saved[0].selection['future.permission'], 'grant');
  assert.deepEqual(saved[0].scopes, {
    'future.permission': { future_dimension: ['narrow'] },
  }, 'an unrelated save cannot flatten the unknown scoped grant');
  await opened.mounted.unmount();
});

test('canceling and reopening role permissions does not leak the abandoned draft', async (t) => {
  const harness = await createPreactHarness(t);
  const opened = await openEditor(harness, scopeSnapshot({ slugs: [CLIPBOARD] }), noopActions, {
    mode: 'buffer', grantOnly: true, subject: 'role', label: 'first', overrides: {},
  });
  await harness.act(() => opened.host.querySelector('[data-slug="human.clipboard"] [data-effect="grant"]').click());
  await harness.act(async () => { opened.host.querySelector('#perm-edit-cancel').click(); await Promise.resolve(); });
  await harness.act(() => opened.state.openBufferedPermissions({
    grantOnly: true, subject: 'role', label: 'second', overrides: {},
  }));
  assert.match(opened.host.querySelector('#perm-edit-subtitle').textContent, /second/);
  assert.ok(opened.host.querySelector('[data-slug="human.clipboard"] [data-effect="default"].active'),
    'the new launch starts from its own baseline');
  await opened.mounted.unmount();
});

test('an unreadable group scope cannot be widened by saving the editor', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const { host, mounted } = await openEditor(harness, scopeSnapshot({ slugs: [SPAWN] }), {
    ...noopActions,
    savePermissions: async (...args) => saved.push(args),
  }, {
    mode: 'group', group: 'alpha', grants: ['groups.members.spawn'], unreadable: ['groups.members.spawn'],
  });

  assert.match(host.querySelector('[data-slug="groups.members.spawn"] .perm-scope-chip').textContent,
    /unreadable scope/);
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.equal(saved.length, 0, 'saving must not rewrite the fail-closed scope as an unscoped grant');
  assert.match(host.querySelector('#perm-edit-error').textContent, /Cannot save.*unreadable scopes/);

  await harness.act(() => host.querySelector('[data-slug="groups.members.spawn"] [data-effect="default"]').click());
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.equal(saved.length, 1, 'removing the unreadable grant remains an explicit safe escape hatch');
  await mounted.unmount();
});

test('a scoped grant renders its chips and an unscoped one says so', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN, CLIPBOARD],
    scopes: { 'groups.members.spawn': { group: ['alpha', 'beta'] } },
  }), noopActions);

  const chips = host.querySelectorAll('[data-slug="groups.members.spawn"] .perm-scope-chip');
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
  const chip = host.querySelector('[data-slug="groups.members.spawn"] .perm-scope-chip.unscoped');
  assert.ok(chip, 'a granted, dimensioned, unnarrowed slug must SAY it applies everywhere');
  assert.match(chip.textContent, /unscoped/);
  await mounted.unmount();
});

// The human picked layout "variant C proper": chips ride on the tristate line
// as a direct row child, and the effective-source line moves into the drawer
// because the row cannot carry both at this dialog's width. Pinning the shape
// keeps a later tidy-up from quietly reinstating the two-column row that
// truncated one of them.
test('a scopable row puts its chips in-row and its effective source in the drawer', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN],
    scopes: { 'groups.members.spawn': { group: ['alpha'] } },
  }), noopActions);

  const row = host.querySelector('.perm-row[data-slug="groups.members.spawn"]');
  assert.equal(row.querySelector(':scope > .perm-scope-chips .perm-scope-chip').textContent.replace(/\s+/g, ''),
    'group=alpha', 'the chips are a row item, not stacked under the slug');
  // The column is emptied, not removed: it still reserves its width so the
  // tristate buttons line up with the rows that have no scope.
  assert.equal(row.querySelector('.perm-row-eff').textContent, '');
  assert.ok(row.querySelector('.perm-row-eff.empty'));

  await harness.act(() => { row.querySelector('button.perm-scope-twisty').click(); });
  const eff = host.querySelector('.perm-scope-drawer[data-slug="groups.members.spawn"] .perm-row-eff');
  assert.ok(eff, 'the effective source is still reachable — it moved, it was not dropped');
  assert.match(eff.textContent, /✓/);
  await mounted.unmount();
});

// A row that cannot be scoped keeps the effective-source column, and still
// gets the (inert) gutter so the list stays aligned.
test('a non-scopable row keeps its effective source and an inert gutter', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN, { slug: 'human.notify', description: 'notify' }],
  }), noopActions);

  const row = host.querySelector('.perm-row[data-slug="human.notify"]');
  assert.ok(row.querySelector('.perm-row-eff'), 'nothing displaced it, so it stays');
  assertAbsent(row.querySelector('button.perm-scope-twisty'));
  assert.ok(row.querySelector('span.perm-scope-twisty.empty'), 'but the gutter is reserved');
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

  await harness.act(() => { host.querySelector('[data-slug="groups.members.spawn"] button.perm-scope-twisty').click(); });
  const drawer = host.querySelector('.perm-scope-drawer[data-slug="groups.members.spawn"]');
  assert.ok(drawer);
  assert.deepEqual(
    Array.from(drawer.querySelectorAll('.perm-scope-dim')).map((row) => row.dataset.dim),
    ['group', 'spawn_profile'],
    'the drawer renders exactly the dimensions the slug advertised, in that order',
  );

  await comboPick(harness, dimBox(drawer, 'group'), 'beta');
  assert.match(host.querySelector('[data-slug="groups.members.spawn"] .perm-scope-chip').textContent, /group=beta/,
    'the row chips track the drawer edit live');

  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0].scopes, { 'groups.members.spawn': { group: ['beta'] } });
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

  await harness.act(() => { host.querySelector('[data-slug="process.runs.manage"] button.perm-scope-twisty').click(); });
  // No catalogue means no rows to offer — what is typed is the value, the
  // same input the CLI's --scope takes.
  assert.deepEqual(await comboKeys(harness, dimBox(host, 'process_template')), []);
  const free = dimInput(host, 'process_template');
  await harness.input(free, 'release-train');
  await harness.act(() => harness.fireEvent(free, 'keydown', { key: 'Enter' }));
  assert.match(host.querySelector('[data-slug="process.runs.manage"] .perm-scope-chip').textContent,
    /process_template=release-train/);

  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0], { 'process.runs.manage': { process_template: ['release-train'] } });
  await mounted.unmount();
});

// A dimension with no catalogue is typed into and nothing else, so the
// gestures an operator actually makes after typing — Enter, blur, or
// straight to Save — must all keep the value. Dropping it
// posts the grant back as {}, which is not "no change" but an explicit
// widening to unscoped: the exact way a narrowing appears to have "no effect".
test('a typed matcher survives Enter, a blur, and a straight Save', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const actions = { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push(scopes); } };
  const snapshot = scopeSnapshot({ slugs: [TEMPLATES], dimOptions: { process_template: {} } });

  let opened = await openEditor(harness, snapshot, actions);
  await harness.act(() => { opened.host.querySelector('[data-slug="process.runs.manage"] button.perm-scope-twisty').click(); });
  let free = dimInput(opened.host, 'process_template');
  await harness.input(free, 'release-train');
  await harness.act(() => harness.fireEvent(free, 'keydown', { key: 'Enter' }));
  assert.match(opened.host.querySelector('[data-slug="process.runs.manage"] .perm-scope-chip').textContent,
    /process_template=release-train/, 'Enter commits the typed value');
  await opened.mounted.unmount();

  opened = await openEditor(harness, snapshot, actions);
  await harness.act(() => { opened.host.querySelector('[data-slug="process.runs.manage"] button.perm-scope-twisty').click(); });
  free = dimInput(opened.host, 'process_template');
  await harness.input(free, 'release-train');
  await harness.act(() => harness.fireEvent(free, 'blur'));
  assert.match(opened.host.querySelector('[data-slug="process.runs.manage"] .perm-scope-chip').textContent,
    /process_template=release-train/, 'leaving the box commits what is in it');
  await opened.mounted.unmount();

  // The unforgiving one: type, then click Save without ever leaving the box.
  opened = await openEditor(harness, snapshot, actions);
  await harness.act(() => { opened.host.querySelector('[data-slug="process.runs.manage"] button.perm-scope-twisty').click(); });
  free = dimInput(opened.host, 'process_template');
  await harness.input(free, 'release-train');
  await harness.act(async () => { opened.host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved.at(-1), { 'process.runs.manage': { process_template: ['release-train'] } },
    'Save must flush the typed matcher, never post the grant back as unscoped');
  await opened.mounted.unmount();
});

// A box that leaves the screen WITHOUT committing withdraws its draft, so what
// Save writes is what the dialog was showing. In a browser a pointer gesture
// blurs the box first and the value commits as a chip on the way out — these
// cover the paths where no blur happens (keyboard, programmatic re-render, and
// the browsers that do not move focus on mousedown), which is where an armed
// draft would otherwise be resurrected by a later Save.
//
// Each case takes the box off screen a different way: the twisty, the slug
// leaving Grant, and the stale dimension the draft belongs to being removed.
test('a typed matcher withdrawn from the screen is not resurrected by Save', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const actions = { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push(scopes); } };
  const snapshot = scopeSnapshot({ slugs: [TEMPLATES], dimOptions: { process_template: {} } });
  const typeInto = async (host, dim, value) => harness.input(dimInput(host, dim), value);

  let opened = await openEditor(harness, snapshot, actions);
  let twisty = opened.host.querySelector('[data-slug="process.runs.manage"] button.perm-scope-twisty');
  await harness.act(() => { twisty.click(); });
  await typeInto(opened.host, 'process_template', 'oops');
  await harness.act(() => { twisty.click(); });
  await harness.act(async () => { opened.host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved.at(-1), { 'process.runs.manage': {} }, 'collapsing the drawer withdraws the draft');
  await opened.mounted.unmount();

  // Deny unmounts the drawer (a deny is unconditional, so it carries no
  // scope). Coming back to Grant must not resurrect what was typed.
  opened = await openEditor(harness, snapshot, actions);
  await harness.act(() => { opened.host.querySelector('[data-slug="process.runs.manage"] button.perm-scope-twisty').click(); });
  await typeInto(opened.host, 'process_template', 'ghost');
  await harness.act(() => { opened.host.querySelector('[data-slug="process.runs.manage"] [data-effect="deny"]').click(); });
  await harness.act(() => { opened.host.querySelector('[data-slug="process.runs.manage"] [data-effect="grant"]').click(); });
  assert.ok(opened.host.querySelector('[data-slug="process.runs.manage"] .perm-scope-chip.unscoped'),
    'the row is back to unscoped, and that is what Save must write');
  await harness.act(async () => { opened.host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved.at(-1), { 'process.runs.manage': {} });
  await opened.mounted.unmount();

  // A dimension the slug no longer accepts is offered only while the stored
  // scope still carries it. Emptying it removes the editor; a draft typed into
  // that editor must go with it, or Save posts a dimension the daemon rejects
  // and the operator can no longer see to fix.
  opened = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN], dimOptions: { group: { values: ['alpha'] } },
    scopes: { 'groups.members.spawn': { group: ['alpha'], legacy_dim: ['x'] } },
  }), actions);
  await harness.act(() => { opened.host.querySelector('[data-slug="groups.members.spawn"] button.perm-scope-twisty').click(); });
  await typeInto(opened.host, 'legacy_dim', 'y');
  await harness.act(() => {
    opened.host.querySelector('.perm-scope-dim[data-dim="legacy_dim"] .perm-scope-chip .x').click();
  });
  assertAbsent(opened.host.querySelector('.perm-scope-dim[data-dim="legacy_dim"]'));
  await harness.act(async () => { opened.host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved.at(-1), { 'groups.members.spawn': { group: ['alpha'] } },
    'the removed dimension stays removed');
  await opened.mounted.unmount();
});

// A rejected save leaves the dialog open. The flushed value must be a chip and
// nothing else — the box still holding the same text reads as a second matcher
// waiting to be added.
test('a flushed matcher clears the box it was typed into', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [TEMPLATES], dimOptions: { process_template: {} },
  }), { ...noopActions, savePermissions: async () => { throw new Error('daemon said no'); } });

  await harness.act(() => { host.querySelector('[data-slug="process.runs.manage"] button.perm-scope-twisty').click(); });
  await harness.input(dimInput(host, 'process_template'), 'release-train');
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });

  assert.match(host.querySelector('[data-slug="process.runs.manage"] .perm-scope-chip').textContent,
    /process_template=release-train/);
  assert.equal(dimInput(host, 'process_template').value, '',
    'the box is empty; the value lives in the chip now');
  assert.match(host.querySelector('#perm-edit-error').textContent, /daemon said no/);
  await mounted.unmount();
});

test('removing the last value of a dimension returns the grant to unscoped', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [TEMPLATES],
    scopes: { 'process.runs.manage': { process_template: ['release-train'] } },
  }), { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push(scopes); } });

  await harness.act(() => { host.querySelector('[data-slug="process.runs.manage"] button.perm-scope-twisty').click(); });
  await harness.act(() => { host.querySelector('.perm-scope-dim[data-dim="process_template"] .perm-scope-chip .x').click(); });
  assert.ok(host.querySelector('[data-slug="process.runs.manage"] .perm-scope-chip.unscoped'),
    'an emptied dimension is absent, not an empty list the daemon would reject');
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  // Sent EXPLICITLY as {}, not omitted: the daemon reads a missing key as
  // "keep the stored scope", so an omission here would silently fail to clear.
  assert.deepEqual(saved[0], { 'process.runs.manage': {} });
  await mounted.unmount();
});

test('a dimension this build has never heard of is still editable', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const FUTURE = {
    slug: 'proxy.git.push', description: 'push', owner_implied: false, scope_dims: ['git_remote'],
  };
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [FUTURE],
    dimOptions: { git_remote: { values: ['origin'], selectors: ['@launch-repo'] } },
  }), { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push(scopes); } });

  await harness.act(() => { host.querySelector('[data-slug="proxy.git.push"] button.perm-scope-twisty').click(); });
  assert.deepEqual((await comboKeys(harness, dimBox(host, 'git_remote'))).sort(), ['@launch-repo', 'origin'],
    'advertised values and selectors are both offered, with no frontend knowledge of the dimension');
  await comboPick(harness, dimBox(host, 'git_remote'), '@launch-repo');
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0], { 'proxy.git.push': { git_remote: ['@launch-repo'] } });
  await mounted.unmount();
});

test('scopes are dropped for a slug moved off Grant', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN],
    scopes: { 'groups.members.spawn': { group: ['alpha'] } },
  }), { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push({ selection, scopes }); } });

  await harness.act(() => { host.querySelector('[data-slug="groups.members.spawn"] [data-effect="deny"]').click(); });
  assertAbsent(host.querySelector('[data-slug="groups.members.spawn"] .perm-scope-chips'),
    'a deny is unconditional, so it shows no scope to edit');
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0].selection, { 'groups.members.spawn': 'deny' });
  assert.deepEqual(saved[0].scopes, {}, 'no scope rides a deny — the daemon would refuse it');
  await mounted.unmount();
});

test('a grant whose stored scope cannot be decoded is never shown as unscoped', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN],
    unreadable: ['groups.members.spawn'],
  }), { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push(scopes); } });

  const chip = host.querySelector('[data-slug="groups.members.spawn"] .perm-scope-chip');
  assert.ok(chip, 'the row must say something about the scope it cannot read');
  assert.match(chip.textContent, /unreadable scope/);
  assert.equal(chip.classList.contains('unscoped'), false,
    'a grant that authorizes nothing must never read as one that authorizes everything');
  // No editor either: the operator cannot narrow what the build cannot show,
  // and the daemon refuses to overwrite the row from here.
  assertAbsent(host.querySelector('[data-slug="groups.members.spawn"] button.perm-scope-twisty'));
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0], {}, 'saving sends no scope for it, and the daemon keeps the stored one');
  await mounted.unmount();
});

test('a stored dimension the slug no longer accepts can still be removed', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  // groups.members.spawn declares group + spawn_profile; the stored scope also carries
  // a dimension a past build allowed. Without an editor for it, every save
  // would be refused by the daemon and the operator could not fix it here.
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN],
    scopes: { 'groups.members.spawn': { group: ['alpha'], legacy_dim: ['x'] } },
  }), { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push(scopes); } });

  await harness.act(() => { host.querySelector('[data-slug="groups.members.spawn"] button.perm-scope-twisty').click(); });
  const stale = host.querySelector('.perm-scope-dim[data-dim="legacy_dim"]');
  assert.ok(stale, 'a stored dimension gets an editor even when the slug stopped declaring it');
  assert.ok(stale.querySelector('.perm-scope-stale'), 'and is flagged as no longer accepted');
  await harness.act(() => { stale.querySelector('.perm-scope-chip .x').click(); });
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0], { 'groups.members.spawn': { group: ['alpha'] } });
  await mounted.unmount();
});

test('a scope value cannot smuggle markup into the row chips', async (t) => {
  const harness = await createPreactHarness(t);
  const evil = '<img src=x onerror="globalThis.__pwned = true">';
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN],
    scopes: { 'groups.members.spawn': { group: [evil] } },
  }), noopActions);
  const chip = host.querySelector('[data-slug="groups.members.spawn"] .perm-scope-chip');
  assert.equal(chip.querySelector('img'), null, 'a scope value is text, never markup');
  assert.ok(chip.textContent.includes(evil), 'and it is shown verbatim so the operator sees what is stored');
  assert.equal(globalThis.__pwned, undefined);
  await mounted.unmount();
});

test('HTTP proxy permission can be granted and scoped to a configured instance', async (t) => {
  const harness = await createPreactHarness(t);
  const snapshot = scopeSnapshot({
    slugs: [{ slug: 'proxy.http', description: 'HTTP proxy', scope_dims: ['http_proxy'] }],
    dimOptions: { http_proxy: { values: ['billing', 'inventory'] } },
  });
  snapshot.permissions.overrides['conv-s'] = {};
  const saved = [];
  const { host } = await openEditor(harness, snapshot, {
    ...noopActions,
    savePermissions: async (descriptor, selection, scopes) => saved.push({ selection, scopes }),
  });
  await harness.act(() => host.querySelector('[data-slug="proxy.http"] [data-effect="grant"]').click());
  await harness.act(() => host.querySelector('[data-slug="proxy.http"] button.perm-scope-twisty').click());
  await comboPick(harness, dimBox(host, 'http_proxy'), 'inventory');
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.equal(saved[0].selection['proxy.http'], 'grant');
  assert.deepEqual(saved[0].scopes, { 'proxy.http': { http_proxy: ['inventory'] } });
});

test('the scope control says it adds scopes and appears only on a grant that has them', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, mounted } = await openEditor(harness, scopeSnapshot({ slugs: [SPAWN, CLIPBOARD] }), noopActions);
  const twisty = () => host.querySelector('[data-slug="groups.members.spawn"] button.perm-scope-twisty');
  assert.equal(twisty().getAttribute('aria-expanded'), 'false');
  assert.equal(twisty().getAttribute('aria-label'), 'Add or edit scopes for groups.members.spawn');
  assert.match(twisty().title, /^Add or edit scopes/);
  assertAbsent(host.querySelector('[data-slug="human.clipboard"] button.perm-scope-twisty'));
  await harness.act(() => { twisty().click(); });
  assert.equal(twisty().getAttribute('aria-expanded'), 'true');
  assert.equal(twisty().getAttribute('aria-label'), 'Hide scopes for groups.members.spawn');
  await mounted.unmount();
});

test('the filter\'s clear keeps selections, and the reset keeps the filter', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const { host, mounted } = await openEditor(harness, scopeSnapshot({ slugs: [SPAWN, CLIPBOARD] }), {
    ...noopActions,
    savePermissions: async (descriptor, selection) => { saved.push(selection); },
  });
  const filter = () => host.querySelector('#perm-edit-filter');
  const reset = () => host.querySelector('#perm-edit-reset');
  const slugs = () => [...host.querySelectorAll('.perm-row')].map((row) => row.dataset.slug);
  const granted = (slug) => host.querySelector(`[data-slug="${slug}"] [data-effect="grant"]`).classList.contains('active');

  assertAbsent(host.querySelector('#perm-edit-filter-clear'), 'nothing to clear yet');
  assert.equal(reset().querySelector('.pe-btn-regular').textContent, 'Reset all to Default');
  assert.equal(reset().querySelector('.pe-btn-wizard').textContent, 'unbind all');
  assertAbsent(host.querySelector('.perm-edit-toolbar #perm-edit-reset'), 'not beside the search box');

  await harness.input(filter(), 'clip');
  assert.deepEqual(slugs(), ['human.clipboard']);
  await harness.act(() => { host.querySelector('#perm-edit-filter-clear').click(); });
  assert.equal(filter().value, '');
  assert.deepEqual(slugs(), ['groups.members.spawn', 'human.clipboard']);
  assert.ok(granted('groups.members.spawn') && granted('human.clipboard'), 'clearing the filter keeps every selection');

  await harness.input(filter(), 'clip');
  assert.equal(reset().disabled, false);
  await harness.act(() => { reset().click(); });
  assert.equal(filter().value, 'clip', 'resetting keeps the filter');
  assert.equal(reset().disabled, true, 'nothing left to clear, hidden rows included');
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.equal(saved[0]['groups.members.spawn'], 'default');
  assert.equal(saved[0]['human.clipboard'], 'default');
  await mounted.unmount();
});

test('Escape empties a non-empty filter first and closes the editor only after that', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, mounted } = await openEditor(harness, scopeSnapshot({ slugs: [SPAWN, CLIPBOARD] }), noopActions);
  const filter = () => host.querySelector('#perm-edit-filter');
  await harness.input(filter(), 'clip');
  await harness.act(() => harness.fireEvent(filter(), 'keydown', { key: 'Escape' }));
  assert.equal(filter().value, '');
  assert.ok(host.querySelector('#perm-edit-modal'), 'the first Escape leaves the editor open');
  await harness.act(async () => { harness.fireEvent(filter(), 'keydown', { key: 'Escape' }); await Promise.resolve(); });
  assertAbsent(host.querySelector('#perm-edit-modal'), 'the next Escape closes it');
  await mounted.unmount();
});

test('the reset clears a buffered draft\'s unknown slug, which has no row of its own', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const { host, mounted } = await openEditor(harness, scopeSnapshot({ slugs: [CLIPBOARD] }), {
    ...noopActions,
    savePermissions: async (descriptor, selection, scopes) => saved.push({ selection, scopes }),
  }, {
    mode: 'buffer', grantOnly: true, subject: 'role', label: 'future-role',
    overrides: { 'future.permission': { effect: 'grant', scope: { future_dimension: ['narrow'] } } },
  });
  const reset = host.querySelector('#perm-edit-reset');
  assert.equal(reset.querySelector('.pe-btn-regular').textContent, 'Clear all grants');
  assert.equal(reset.disabled, false, 'the unknown grant is something to clear');
  await harness.act(() => { reset.click(); });
  assert.equal(host.querySelector('#perm-edit-reset').disabled, true);
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.notEqual(saved[0].selection['future.permission'], 'grant');
  assert.equal(saved[0].scopes?.['future.permission'], undefined);
  await mounted.unmount();
});

// Resource umbrellas (tcl-h3zaen). resources.read / resources.write take a
// resource=kind:id scope over a catalogue that can run to hundreds of boards
// and templates, so the drawer searches it rather than listing it.
const RES_READ = { slug: 'resources.read', description: 'read', scope_dims: ['group', 'resource'], umbrella: 'resources.write' };
const RES_WRITE = { slug: 'resources.write', description: 'write', scope_dims: ['group', 'resource'] };
const BOARDS_READ = { slug: 'boards.read', description: 'boards', umbrella: 'resources.read' };

function resourceCatalogue(count) {
  return Array.from({ length: count }, (_, i) => ({
    kind: i % 3 ? 'board' : 'process_template', id: `res-${i}`,
    title: i === 7 ? 'Q4 plan' : `Roadmap ${i}`, groups: i % 4 ? ['crew'] : [],
  }));
}

test('the resource= drawer searches the catalogue it loads once, and saves kind:id', async (t) => {
  const harness = await createPreactHarness(t);
  const loads = [];
  const saved = [];
  const { host } = await openEditor(harness, scopeSnapshot({
    slugs: [RES_READ], dimOptions: { group: { values: ['crew'] }, resource: {} },
  }), {
    ...noopActions,
    loadScopeValues: async (descriptor, dim) => { loads.push(dim); return resourceCatalogue(300); },
    savePermissions: async (descriptor, selection, scopes) => saved.push(scopes),
  });
  await harness.act(() => host.querySelector('[data-slug="resources.read"] button.perm-scope-twisty').click());
  await harness.act(async () => { await Promise.resolve(); await Promise.resolve(); });
  assert.deepEqual(loads, ['resource'], 'the catalogue loads when the drawer opens, once');
  const dim = host.querySelector('.perm-scope-dim[data-dim="resource"]');
  assertAbsent(dim.querySelector('select'), 'no flat select of 300 resources');
  const box = dim.querySelector('input[role="combobox"]');
  assert.match(box.getAttribute('placeholder'), /300/);
  await harness.act(() => harness.fireEvent(box, 'focus'));
  await harness.input(box, 'q4');
  const option = dim.querySelector('[role="option"]');
  assert.equal(option.dataset.key, 'board:res-7');
  await harness.act(() => harness.fireEvent(option, 'mousedown'));
  assert.match(dim.querySelector('.perm-scope-chip').textContent, /board\s+Q4 plan/, 'the chip reads as the title');
  await harness.input(box, 'board:roadmap');
  assert.equal(dim.querySelectorAll('[role="option"]').length, 8);
  assert.match(dim.querySelector('.filter-combobox-foot').textContent, /Refine your search/);
  await harness.input(box, '');

  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0], { 'resources.read': { resource: ['board:res-7'] } }, 'the chosen kind:id is saved');
});

// Text left in the search box is an uncommitted value, as in the plain box:
// Save folds it in (as the catalogue key it names, or as typed for the daemon
// to judge), and never writes the grant unscoped because Enter was not pressed.
test('Save folds what is left in the resource box in, never saving the grant unscoped', async (t) => {
  const harness = await createPreactHarness(t);
  for (const [typed, want] of [['Q4 plan', 'board:res-7'], ['board:not-listed', 'board:not-listed']]) {
    const saved = [];
    const { host, mounted } = await openEditor(harness, scopeSnapshot({ slugs: [RES_READ], dimOptions: { resource: {} } }), {
      ...noopActions,
      loadScopeValues: async () => resourceCatalogue(20),
      savePermissions: async (descriptor, selection, scopes) => saved.push(scopes),
    });
    await harness.act(() => host.querySelector('[data-slug="resources.read"] button.perm-scope-twisty').click());
    await harness.act(async () => { await Promise.resolve(); await Promise.resolve(); });
    const box = host.querySelector('.perm-scope-dim[data-dim="resource"] input[role="combobox"]');
    await harness.act(() => harness.fireEvent(box, 'focus'));
    await harness.input(box, typed);
    await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
    assert.deepEqual(saved[0], { 'resources.read': { resource: [want] } }, `typed ${typed}`);
    await mounted.unmount();
  }
});

test('many selected resources collapse, in the drawer and in the row', async (t) => {
  const harness = await createPreactHarness(t);
  const values = Array.from({ length: 41 }, (_, i) => `board:res-${String(i).padStart(2, '0')}`);
  const saved = [];
  const { host } = await openEditor(harness, scopeSnapshot({
    slugs: [RES_READ], scopes: { 'resources.read': { resource: values } }, dimOptions: { resource: {} },
  }), {
    ...noopActions,
    loadScopeValues: async () => [],
    savePermissions: async (descriptor, selection, scopes) => saved.push(scopes),
  });
  assert.match(host.querySelector('[data-slug="resources.read"] .perm-scope-chip').textContent,
    /resource=board:res-00, board:res-01 \+39 more/);
  await harness.act(() => host.querySelector('[data-slug="resources.read"] button.perm-scope-twisty').click());
  const dim = host.querySelector('.perm-scope-dim[data-dim="resource"]');
  assert.equal(dim.querySelectorAll('.perm-scope-chips > .perm-scope-chip:not(.perm-scope-more)').length, 6);
  const more = dim.querySelector('.perm-scope-more');
  assert.equal(more.textContent, '+35 more');
  await harness.act(() => more.click());
  const sheet = dim.querySelector('.perm-scope-sheet');
  assert.equal(sheet.querySelectorAll('.perm-scope-sheet-row').length, 8, 'the sheet pages');
  assert.match(sheet.querySelector('.perm-scope-sheet-pager').textContent, /1–8 of 35/);
  await harness.input(sheet.querySelector('.perm-scope-sheet-filter'), 'res-40');
  assert.equal(sheet.querySelectorAll('.perm-scope-sheet-row').length, 1);
  await harness.act(() => sheet.querySelector('.perm-scope-sheet-row button.x').click());
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.equal(saved[0]['resources.read'].resource.length, 40);
  assert.ok(!saved[0]['resources.read'].resource.includes('board:res-40'), 'removing from the sheet removes the matcher');
});

test('a per-kind row an umbrella covers says so, in counts', async (t) => {
  const harness = await createPreactHarness(t);
  const snapshot = scopeSnapshot({
    slugs: [RES_READ, RES_WRITE, BOARDS_READ],
    scopes: { 'resources.read': { group: ['crew', 'web'], resource: ['board:a', 'board:b', 'board:c'] } },
    dimOptions: { group: { values: ['crew', 'web'] }, resource: {} },
  });
  snapshot.permissions.overrides['conv-s'] = { 'resources.read': 'grant' };
  let opened = await openEditor(harness, snapshot, noopActions);
  const covered = () => opened.host.querySelector('[data-slug="boards.read"] .perm-row-covered')?.textContent || '';
  assert.match(covered(), /covered by resources\.read, scoped to 2 groups and 3 resources/);
  assert.doesNotMatch(covered(), /board:a/, 'counts, never names');
  const eff = () => opened.host.querySelector('[data-slug="boards.read"] .perm-row-eff').textContent;
  assert.equal(eff(), '✓ via resources.read (scoped)', 'a covered row is not "denied (no source)"');
  await harness.act(() => opened.host.querySelector('[data-slug="boards.read"] [data-effect="deny"]').click());
  assert.equal(covered(), '', 'a deny of the row wins over its umbrella');
  assert.match(eff(), /denied \(explicit veto\)/);
  await opened.mounted.unmount();

  snapshot.permissions.overrides['conv-s'] = { 'resources.write': 'grant' };
  snapshot.permissions.scopes['conv-s'] = {};
  opened = await openEditor(harness, snapshot, noopActions);
  assert.match(covered(), /covered by resources\.write, unscoped/, 'the chain follows resources.write → resources.read');
  await harness.act(() => opened.host.querySelector('[data-slug="resources.read"] [data-effect="deny"]').click());
  assert.equal(covered(), '', 'a deny of resources.read stops the chain');
  await opened.mounted.unmount();
});

test('a long snapshot catalogue switches the dimension to the searchable box', async (t) => {
  const harness = await createPreactHarness(t);
  const groups = Array.from({ length: 30 }, (_, i) => `group-${i}`);
  const saved = [];
  const { host } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN], dimOptions: { group: { values: groups }, spawn_profile: { values: ['reviewer'] } },
  }), { ...noopActions, savePermissions: async (descriptor, selection, scopes) => saved.push(scopes) });
  await harness.act(() => host.querySelector('[data-slug="groups.members.spawn"] button.perm-scope-twisty').click());
  assert.deepEqual(await comboKeys(harness, dimBox(host, 'spawn_profile')), ['reviewer'],
    'a short catalogue is the type-to-filter box too');
  const box = host.querySelector('.perm-scope-dim[data-dim="group"] input[role="combobox"]');
  await harness.act(() => harness.fireEvent(box, 'focus'));
  await harness.input(box, 'group-17');
  await harness.act(() => harness.fireEvent(box, 'keydown', { key: 'Enter' }));
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved[0], { 'groups.members.spawn': { group: ['group-17'] } });
});

// The sudo editor reads its picks through effectOf, while the agent's stored
// denies stay on row.effect. The resolver returns a dedicated deny before it
// consults an umbrella, so the hint must not claim coverage past one.
test('the covered-by hint honours stored denies the sudo picks do not override', async (t) => {
  const harness = await createPreactHarness(t);
  const { umbrellaCover } = await harness.importDashboardModule('js/message-access-dialog-model.js');
  const rows = [
    { slug: 'boards.read', umbrella: 'resources.read', effect: 'deny', granted: false, sources: [] },
    { slug: 'resources.read', umbrella: 'resources.write', effect: 'default', granted: false, sources: [] },
    { slug: 'resources.write', effect: 'default', granted: false, sources: [] },
  ];
  const by = (list) => new Map(list.map((row) => [row.slug, row]));
  const picks = (map) => (slug) => map[slug] || 'default';
  const scopeOf = () => ({});
  assert.equal(umbrellaCover(rows[0], by(rows), picks({ 'resources.read': 'grant' }), scopeOf), null,
    'a stored deny of the row wins over a sudo of its umbrella');
  const stepDenied = rows.map((row) => (row.slug === 'resources.read' ? { ...row, effect: 'deny' } : { ...row, effect: 'default' }));
  assert.equal(umbrellaCover(stepDenied[0], by(stepDenied), picks({ 'resources.write': 'grant' }), scopeOf), null,
    'a stored deny of a step stops the chain');
  assert.equal(umbrellaCover(stepDenied[0], by(stepDenied), picks({ 'resources.read': 'grant' }), scopeOf)?.slug, 'resources.read',
    'a sudo of the denied step itself wins over its deny');
  const viaDefault = rows.map((row) => ({ ...row, effect: 'default',
    ...(row.slug === 'resources.read' ? { granted: true, sources: ['global default'] } : {}) }));
  assert.equal(umbrellaCover(viaDefault[0], by(viaDefault), picks({}), scopeOf), null,
    'a config default of an umbrella is not a source its step reads');
});
