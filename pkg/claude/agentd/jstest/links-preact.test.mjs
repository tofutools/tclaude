import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

function memoryPrefs(initial = {}) {
  const values = new Map(Object.entries(initial));
  return {
    values,
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
  };
}

const sample = {
  groups: [{ name: 'alpha' }, { name: 'beta' }, { name: 'gamma' }],
  links: [
    { id: 1, from: 'alpha', to: 'beta', mode: 'members->members', created_at: '2026-07-13T12:00:00Z' },
    { id: 2, from: 'gamma', to: 'alpha', mode: 'owners->members', created_at: '2026-07-13T13:00:00Z' },
  ],
};

async function mountLinks(t, overrides = {}) {
  const harness = await createPreactHarness(t);
  const [{ createLinksState }, { mountLinksIsland }] = await Promise.all([
    harness.importDashboardModule('js/links-state.js'),
    harness.importDashboardModule('js/links-island.js'),
  ]);
  const state = createLinksState({ prefs: memoryPrefs(), readSort: () => null, writeSort: () => {} });
  const calls = [];
  const actions = {
    openManager: state.openManager,
    closeManager: state.closeManager,
    openCreate: state.openCreate,
    openEdit: state.openEdit,
    closeEditor: state.closeEditor,
    createLink: async (value) => { calls.push(['create', value]); },
    updateLink: async (value) => { calls.push(['update', value]); },
    deleteLink: async (value) => { calls.push(['delete', value]); },
    ...overrides.actions,
  };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const cleanups = [];
  await harness.act(() => mountLinksIsland({
    host, state, actions,
    confirmDiscard: overrides.confirmDiscard || (async () => true),
    registerCleanup: (cleanup) => cleanups.push(cleanup),
  }));
  await harness.act(() => state.publish(sample));
  return {
    harness, host, state, actions, calls,
    cleanup: async () => harness.act(() => cleanups.reverse().forEach((cleanup) => cleanup())),
  };
}

async function choose(harness, select, value) {
  [...select.options].forEach((option) => option.removeAttribute('selected'));
  const option = [...select.options].find((candidate) => candidate.value === value);
  assert.ok(option, `select contains option ${value}`);
  option.setAttribute('selected', '');
  await harness.act(() => harness.fireEvent(select, 'change'));
}

test('Links state owns persisted filtering, sorting and exclusive dialog descriptors', async (t) => {
  const harness = await createPreactHarness(t);
  const { createLinksState } = await harness.importDashboardModule('js/links-state.js');
  const prefs = memoryPrefs({ 'tclaude.dash.filter.links': 'alpha' });
  const writes = [];
  const state = createLinksState({
    prefs,
    readSort: () => ({ col: 'id', dir: 'desc' }),
    writeSort: (_table, value) => writes.push(value),
  });
  state.initialize();
  state.publish(sample);

  assert.equal(state.view.value.filtered, 2);
  assert.deepEqual(state.view.value.rows.map((row) => row.id), [2, 1]);
  assert.deepEqual(state.view.value.groups, ['alpha', 'beta', 'gamma']);
  state.setQuery('gamma');
  assert.equal(state.view.value.filtered, 1);
  assert.equal(prefs.values.get('tclaude.dash.filter.links'), 'gamma');

  state.cycleSort('from');
  state.cycleSort('from');
  state.cycleSort('from');
  assert.deepEqual(writes, [
    { col: 'from', dir: 'asc' },
    { col: 'from', dir: 'desc' },
    null,
  ]);

  assert.equal(state.openManager(), true);
  assert.equal(state.openManager(), false, 'one manager instance owns the listing');
  assert.equal(state.openCreate({ preset: { from: 'alpha' } }), true);
  assert.equal(state.openEdit({ id: 9, from: 'x', to: 'y' }), false, 'a repeated launcher cannot retarget a live draft');
  assert.equal(state.closeManager(), false, 'the manager cannot close underneath its child editor');
  state.closeEditor();
  assert.equal(state.closeManager(), true);
});

test('Links loader claims the one static feature host and registers its controller', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ mountLinksFeature }, controller] = await Promise.all([
    harness.importDashboardModule('js/preact-loader.js'),
    harness.importDashboardModule('js/links-controller.js'),
  ]);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.id = 'links-feature-root';
  const cleanup = await mountLinksFeature({
    refresh: async () => {}, confirm: async () => true,
    confirmDiscard: async () => true, notify: () => {},
  });
  assert.equal(host.dataset.islandOwner, 'links');
  assert.equal(controller.openLinksManager(), true);
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#links-manage-modal'));
  cleanup();
  assert.equal(host.childElementCount, 0);
});

test('Links manager owns keyed rows, filter/sort controls and direct actions', async (t) => {
  const mounted = await mountLinks(t);
  const { harness, host, state, calls } = mounted;
  await harness.act(() => state.openManager());
  await harness.act(() => Promise.resolve());

  const first = host.querySelector('tr[data-key="link-1"]');
  const edit = first.querySelector('button[title="Change this link\'s mode"]');
  assert.equal(edit.hasAttribute('data-act'), false, 'manager actions are direct component callbacks');
  edit.focus();
  await harness.act(() => state.publish({ ...sample, links: [sample.links[1], { ...sample.links[0], mode: 'owners->members' }] }));
  assert.equal(host.querySelector('tr[data-key="link-1"]'), first);
  assert.equal(harness.document.activeElement, edit);

  const filter = getByRole(host, 'textbox', { name: 'Filter inter-group links' });
  await harness.input(filter, 'gamma');
  assert.equal(host.querySelectorAll('#links-list tbody tr').length, 1);
  assert.equal(host.querySelector('#filter-links-count .theme-copy-regular').textContent, '1 / 2');
  const clear = getByRole(host, 'button', { name: 'Clear link filter' });
  await harness.act(() => harness.fireEvent(clear, 'click'));
  assert.equal(harness.document.activeElement, filter);
  assert.equal(host.querySelectorAll('#links-list tbody tr').length, 2);

  const fromHeader = host.querySelector('th[data-sort-col="from"]');
  await harness.act(() => harness.fireEvent(fromHeader, 'click'));
  assert.ok(fromHeader.classList.contains('sort-active'));

  await harness.act(() => harness.fireEvent(edit, 'click'));
  assert.equal(host.querySelector('#link-modal-meta').textContent, '#1 · alpha → beta');
  await harness.act(() => harness.fireEvent(host.querySelector('#link-modal-cancel'), 'click'));
  const remove = first.querySelector('button.danger');
  await harness.act(() => harness.fireEvent(remove, 'click'));
  assert.deepEqual(calls[0], ['delete', { ...sample.links[0], mode: 'owners->members', scope: 'alpha' }]);
  await mounted.cleanup();
});

test('Links create form validates, controls bidirectional payload and exposes busy/error state', async (t) => {
  let rejectRequest;
  const payloads = [];
  const mounted = await mountLinks(t, {
    actions: {
      createLink: (value) => {
        payloads.push(value);
        return new Promise((_resolve, reject) => { rejectRequest = reject; });
      },
    },
  });
  const { harness, host, state } = mounted;
  await harness.act(() => state.openCreate());
  await harness.act(() => Promise.resolve());

  const from = host.querySelector('#link-modal-from');
  const to = host.querySelector('#link-modal-to');
  const mode = host.querySelector('#link-modal-mode');
  const bidir = host.querySelector('#link-modal-bidir');
  assert.equal(harness.document.activeElement, from);
  await choose(harness, to, 'alpha');
  await harness.act(() => harness.fireEvent(host.querySelector('#link-modal-submit'), 'click'));
  assert.match(host.querySelector('#link-modal-error').textContent, /must differ/);
  assert.equal(payloads.length, 0);

  await choose(harness, to, 'beta');
  await choose(harness, mode, 'owners->members');
  bidir.checked = true;
  await harness.act(() => harness.fireEvent(bidir, 'change'));
  await harness.act(() => harness.fireEvent(host.querySelector('#link-modal-submit'), 'click'));
  assert.deepEqual(payloads[0], { from: 'alpha', to: 'beta', mode: 'owners->members', bidir: true });
  assert.equal(host.querySelector('#link-modal-submit').disabled, true);
  assert.match(host.querySelector('#link-modal-submit .theme-copy-regular').textContent, /Creating/);
  assert.equal(from.disabled, true);

  rejectRequest(new Error('permission denied'));
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#link-modal-error').textContent, /permission denied/);
  assert.equal(host.querySelector('#link-modal-submit').disabled, false);
  assert.ok(host.querySelector('#link-modal'), 'a failed create keeps the draft open');
  await mounted.cleanup();
});

test('Links edit form is immutable except for mode and submits the exact row identity', async (t) => {
  const mounted = await mountLinks(t);
  const { harness, host, state, calls } = mounted;
  await harness.act(() => state.openEdit({ id: 2, from: 'gamma', to: 'alpha', mode: 'owners->members' }));
  await harness.act(() => Promise.resolve());

  assert.equal(host.querySelector('#link-modal-meta').textContent, '#2 · gamma → alpha');
  assert.equal(host.querySelector('#link-modal-from').disabled, true);
  assert.equal(host.querySelector('#link-modal-to').disabled, true);
  assert.equal(host.querySelector('#link-modal-bidir'), null);
  assert.equal(harness.document.activeElement, host.querySelector('#link-modal-mode'));
  await choose(harness, host.querySelector('#link-modal-mode'), 'members->members');
  await harness.act(() => harness.fireEvent(host.querySelector('#link-modal-submit'), 'click'));
  assert.deepEqual(calls[0], ['update', { id: '2', from: 'gamma', to: 'alpha', mode: 'members->members' }]);
  await mounted.cleanup();
});

test('stacked Links focus, Escape and live snapshot refresh are deterministic', async (t) => {
  let discardChecks = 0;
  const mounted = await mountLinks(t, {
    confirmDiscard: async () => { discardChecks += 1; return true; },
  });
  const { harness, host, state } = mounted;
  const invoker = harness.document.body.appendChild(harness.document.createElement('button'));
  invoker.focus();
  await harness.act(() => state.openManager());
  await harness.act(() => Promise.resolve());
  assert.equal(harness.document.activeElement, host.querySelector('#filter-links'));

  const create = host.querySelector('#link-new-open');
  create.focus();
  await harness.act(() => harness.fireEvent(create, 'click'));
  await harness.act(() => Promise.resolve());
  const form = {
    from: host.querySelector('#link-modal-from'),
    to: host.querySelector('#link-modal-to'),
    mode: host.querySelector('#link-modal-mode'),
    bidir: host.querySelector('#link-modal-bidir'),
  };
  await choose(harness, form.mode, 'owners->members');
  form.mode.focus();
  await harness.act(() => state.publish({ ...sample, links: [{ ...sample.links[0], mode: 'owners->members' }] }));
  assert.equal(host.querySelector('#link-modal-mode'), form.mode, 'snapshot publish preserves the Preact-owned draft controls');
  assert.equal(form.mode.value, 'owners->members');
  assert.equal(harness.document.activeElement, form.mode);
  assert.equal(host.querySelectorAll('#links-list tbody tr').length, 1, 'the underlying manager updates live');

  const escape = () => {
    const event = new harness.window.Event('keydown', { bubbles: true });
    Object.defineProperty(event, 'key', { value: 'Escape' });
    harness.document.dispatchEvent(event);
  };
  await harness.act(() => escape());
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#link-modal'), null);
  assert.ok(host.querySelector('#links-manage-modal'), 'the first Escape closes only the top editor');
  assert.equal(harness.document.activeElement, create);
  assert.equal(discardChecks, 1);

  await harness.act(() => escape());
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#links-manage-modal'), null);
  assert.equal(harness.document.activeElement, invoker);
  invoker.remove();
  await mounted.cleanup();
});

test('Links create dirty baseline survives live group membership changes', async (t) => {
  let discardChecks = 0;
  let allowDiscard = false;
  const mounted = await mountLinks(t, {
    confirmDiscard: async () => {
      discardChecks += 1;
      return allowDiscard;
    },
  });
  const { harness, host, state } = mounted;
  const escape = () => {
    const event = new harness.window.Event('keydown', { bubbles: true });
    Object.defineProperty(event, 'key', { value: 'Escape' });
    harness.document.dispatchEvent(event);
  };

  await harness.act(() => state.openCreate());
  await harness.act(() => Promise.resolve());
  const from = host.querySelector('#link-modal-from');
  const to = host.querySelector('#link-modal-to');
  // The minimal DOM harness does not derive select.value from Preact's initial
  // controlled value until its first change event, so pin the default tuple by
  // the ordered options from which LinkEditor selects alpha and then beta.
  assert.equal(from.options[0].value, 'alpha');
  assert.equal(to.options[1].value, 'beta');
  await choose(harness, from, 'beta');
  await choose(harness, to, 'gamma');

  await harness.act(() => state.publish({ ...sample, groups: [{ name: 'beta' }, { name: 'gamma' }] }));
  await harness.act(() => escape());
  await harness.act(() => Promise.resolve());
  assert.equal(discardChecks, 1, 'a publish cannot make a changed draft appear clean');
  assert.ok(host.querySelector('#link-modal'), 'denied discard retains the editor');
  assert.equal(host.querySelector('#link-modal-from'), from, 'publish retains the controlled From element');
  assert.equal(host.querySelector('#link-modal-to'), to, 'publish retains the controlled To element');
  assert.equal(from.value, 'beta');
  assert.equal(to.value, 'gamma');

  allowDiscard = true;
  await harness.act(() => escape());
  await harness.act(() => Promise.resolve());
  assert.equal(discardChecks, 2);
  assert.equal(host.querySelector('#link-modal'), null);

  // The inverse remains clean: an untouched alpha→beta draft does not start
  // prompting just because a publish removes alpha from the live group list.
  await harness.act(() => state.publish(sample));
  await harness.act(() => state.openCreate());
  await harness.act(() => Promise.resolve());
  await harness.act(() => state.publish({ ...sample, groups: [{ name: 'beta' }, { name: 'gamma' }] }));
  await harness.act(() => escape());
  await harness.act(() => Promise.resolve());
  assert.equal(discardChecks, 2, 'a publish cannot make an untouched draft appear dirty');
  assert.equal(host.querySelector('#link-modal'), null);
  await mounted.cleanup();
});

test('plain Links actions preserve mutation payloads, refresh and deletion feedback', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createLinksState }, { createLinksActions }] = await Promise.all([
    harness.importDashboardModule('js/links-state.js'),
    harness.importDashboardModule('js/links-actions.js'),
  ]);
  const state = createLinksState({ prefs: memoryPrefs(), readSort: () => null, writeSort: () => {} });
  const requests = [];
  const notices = [];
  const confirms = [];
  let refreshes = 0;
  const actions = createLinksActions({
    state,
    fetchImpl: async (url, options) => {
      requests.push([url, options]);
      return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } });
    },
    refresh: async () => { refreshes += 1; },
    notify: (...args) => notices.push(args),
    confirm: async (descriptor) => { confirms.push(descriptor); return true; },
    words: (plain, wizard) => `wiz:${wizard || plain}`,
  });

  state.openCreate();
  await actions.createLink({ from: 'alpha', to: 'beta', mode: 'members->members', bidir: true });
  assert.equal(requests[0][0], '/api/groups/alpha/links');
  assert.deepEqual(JSON.parse(requests[0][1].body), { to: 'beta', mode: 'members->members', bidir: true });
  assert.equal(state.editor.value, null);

  state.openEdit({ id: 7, from: 'a/b', to: 'beta', mode: 'members->members' });
  await actions.updateLink({ id: 7, from: 'a/b', to: 'beta', mode: 'owners->members' });
  assert.equal(requests[1][0], '/api/groups/a%2Fb/links/7');
  assert.deepEqual(JSON.parse(requests[1][1].body), { mode: 'owners->members' });

  assert.equal(await actions.deleteLink({ id: 7, from: 'a/b', to: 'beta' }), true);
  assert.equal(requests[2][0], '/api/groups/a%2Fb/links/7');
  assert.equal(requests[2][1].method, 'DELETE');
  assert.equal(confirms[0].title, 'wiz:Sever this arcane channel?');
  assert.equal(refreshes, 3);
  assert.match(notices.at(-1)[0], /channel severed/);
});
