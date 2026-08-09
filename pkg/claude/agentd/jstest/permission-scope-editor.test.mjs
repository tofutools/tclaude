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
  }, { mode: 'group', group: 'alpha', grants: ['groups.spawn'], scopes: {
    'groups.spawn': { group: ['alpha'] },
  } });
  assert.match(opened.host.querySelector('[data-slug="groups.spawn"] .perm-scope-chip').textContent, /group=alpha/);
  await harness.act(() => opened.host.querySelector('[data-slug="groups.spawn"] button.perm-scope-twisty').click());
  await harness.act(() => pickOption(harness,
    opened.host.querySelector('.perm-scope-dim[data-dim="group"] .perm-scope-add'), 'beta'));
  await harness.act(async () => { opened.host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(groupSaved[0].scopes, { 'groups.spawn': { group: ['alpha', 'beta'] } });
  assert.equal(groupSaved[0].ownerScopes, null, 'grant scopes are separate from owner-bypass narrowing');
  await opened.mounted.unmount();

  const bufferSaved = [];
  opened = await openEditor(harness, snapshot, {
    ...noopActions,
    savePermissions: async (descriptor, selection, scopes) => bufferSaved.push({ selection, scopes }),
  }, { mode: 'buffer', overrides: {
    'groups.spawn': { effect: 'grant', scope: { spawn_profile: ['reviewer'] } },
  } });
  assert.match(opened.host.querySelector('[data-slug="groups.spawn"] .perm-scope-chip').textContent,
    /spawn_profile=reviewer/);
  await harness.act(async () => { opened.host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(bufferSaved[0], {
    selection: { 'groups.spawn': 'grant' },
    scopes: { 'groups.spawn': { spawn_profile: ['reviewer'] } },
  });
  await opened.mounted.unmount();
});

test('an unreadable group scope cannot be widened by saving the editor', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const { host, mounted } = await openEditor(harness, scopeSnapshot({ slugs: [SPAWN] }), {
    ...noopActions,
    savePermissions: async (...args) => saved.push(args),
  }, {
    mode: 'group', group: 'alpha', grants: ['groups.spawn'], unreadable: ['groups.spawn'],
  });

  assert.match(host.querySelector('[data-slug="groups.spawn"] .perm-scope-chip').textContent,
    /unreadable scope/);
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.equal(saved.length, 0, 'saving must not rewrite the fail-closed scope as an unscoped grant');
  assert.match(host.querySelector('#perm-edit-error').textContent, /Cannot save.*unreadable scopes/);

  await harness.act(() => host.querySelector('[data-slug="groups.spawn"] [data-effect="default"]').click());
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.equal(saved.length, 1, 'removing the unreadable grant remains an explicit safe escape hatch');
  await mounted.unmount();
});

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

// The human picked layout "variant C proper": chips ride on the tristate line
// as a direct row child, and the effective-source line moves into the drawer
// because the row cannot carry both at this dialog's width. Pinning the shape
// keeps a later tidy-up from quietly reinstating the two-column row that
// truncated one of them.
test('a scopable row puts its chips in-row and its effective source in the drawer', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [SPAWN],
    scopes: { 'groups.spawn': { group: ['alpha'] } },
  }), noopActions);

  const row = host.querySelector('.perm-row[data-slug="groups.spawn"]');
  assert.equal(row.querySelector(':scope > .perm-scope-chips .perm-scope-chip').textContent.replace(/\s+/g, ''),
    'group=alpha', 'the chips are a row item, not stacked under the slug');
  // The column is emptied, not removed: it still reserves its width so the
  // tristate buttons line up with the rows that have no scope.
  assert.equal(row.querySelector('.perm-row-eff').textContent, '');
  assert.ok(row.querySelector('.perm-row-eff.empty'));

  await harness.act(() => { row.querySelector('button.perm-scope-twisty').click(); });
  const eff = host.querySelector('.perm-scope-drawer[data-slug="groups.spawn"] .perm-row-eff');
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

  await harness.act(() => { host.querySelector('[data-slug="groups.spawn"] button.perm-scope-twisty').click(); });
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

  await harness.act(() => { host.querySelector('[data-slug="process.runs.manage"] button.perm-scope-twisty').click(); });
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

// A dimension with no catalogue is typed into and nothing else, so the three
// gestures an operator actually makes after typing — Enter (above), the +
// button, blur, or straight to Save — must all keep the value. Dropping it
// posts the grant back as {}, which is not "no change" but an explicit
// widening to unscoped: the exact way a narrowing appears to have "no effect".
test('a typed matcher survives the + button, a blur, and a straight Save', async (t) => {
  const harness = await createPreactHarness(t);
  const saved = [];
  const actions = { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push(scopes); } };
  const snapshot = scopeSnapshot({ slugs: [TEMPLATES], dimOptions: { process_template: {} } });

  let opened = await openEditor(harness, snapshot, actions);
  await harness.act(() => { opened.host.querySelector('[data-slug="process.runs.manage"] button.perm-scope-twisty').click(); });
  let free = opened.host.querySelector('.perm-scope-dim[data-dim="process_template"] .perm-scope-free');
  await harness.input(free, 'release-train');
  await harness.act(() => { opened.host.querySelector('.perm-scope-dim[data-dim="process_template"] .perm-scope-free-add').click(); });
  assert.match(opened.host.querySelector('[data-slug="process.runs.manage"] .perm-scope-chip').textContent,
    /process_template=release-train/, '+ commits the typed value');
  await opened.mounted.unmount();

  opened = await openEditor(harness, snapshot, actions);
  await harness.act(() => { opened.host.querySelector('[data-slug="process.runs.manage"] button.perm-scope-twisty').click(); });
  free = opened.host.querySelector('.perm-scope-dim[data-dim="process_template"] .perm-scope-free');
  await harness.input(free, 'release-train');
  await harness.act(() => harness.fireEvent(free, 'blur'));
  assert.match(opened.host.querySelector('[data-slug="process.runs.manage"] .perm-scope-chip').textContent,
    /process_template=release-train/, 'leaving the box commits what is in it');
  await opened.mounted.unmount();

  // The unforgiving one: type, then click Save without ever leaving the box.
  opened = await openEditor(harness, snapshot, actions);
  await harness.act(() => { opened.host.querySelector('[data-slug="process.runs.manage"] button.perm-scope-twisty').click(); });
  free = opened.host.querySelector('.perm-scope-dim[data-dim="process_template"] .perm-scope-free');
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
  const typeInto = async (host, dim, value) => harness.input(
    host.querySelector(`.perm-scope-dim[data-dim="${dim}"] .perm-scope-free`), value);

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
    scopes: { 'groups.spawn': { group: ['alpha'], legacy_dim: ['x'] } },
  }), actions);
  await harness.act(() => { opened.host.querySelector('[data-slug="groups.spawn"] button.perm-scope-twisty').click(); });
  await typeInto(opened.host, 'legacy_dim', 'y');
  await harness.act(() => {
    opened.host.querySelector('.perm-scope-dim[data-dim="legacy_dim"] .perm-scope-chip .x').click();
  });
  assertAbsent(opened.host.querySelector('.perm-scope-dim[data-dim="legacy_dim"]'));
  await harness.act(async () => { opened.host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });
  assert.deepEqual(saved.at(-1), { 'groups.spawn': { group: ['alpha'] } },
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
  await harness.input(host.querySelector('.perm-scope-dim[data-dim="process_template"] .perm-scope-free'), 'release-train');
  await harness.act(async () => { host.querySelector('#perm-edit-submit').click(); await Promise.resolve(); });

  assert.match(host.querySelector('[data-slug="process.runs.manage"] .perm-scope-chip').textContent,
    /process_template=release-train/);
  assert.equal(host.querySelector('.perm-scope-dim[data-dim="process_template"] .perm-scope-free').value, '',
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
    slug: 'git.push', description: 'push', owner_implied: false, scope_dims: ['git_remote'],
  };
  const { host, mounted } = await openEditor(harness, scopeSnapshot({
    slugs: [FUTURE],
    dimOptions: { git_remote: { values: ['origin'], selectors: ['@launch-repo'] } },
  }), { ...noopActions, savePermissions: async (descriptor, selection, scopes) => { saved.push(scopes); } });

  await harness.act(() => { host.querySelector('[data-slug="git.push"] button.perm-scope-twisty').click(); });
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
  assertAbsent(host.querySelector('[data-slug="groups.spawn"] button.perm-scope-twisty'));
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

  await harness.act(() => { host.querySelector('[data-slug="groups.spawn"] button.perm-scope-twisty').click(); });
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
