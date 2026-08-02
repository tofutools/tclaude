// nav-history.test.mjs — tests for the DOM/History ADAPTER (js/nav-history.js),
// as opposed to nav-history-core.test.mjs which covers the pure location-stack
// reducer underneath it.
//
// The adapter is where a location decision turns into a real history mutation,
// so it is where the push-vs-replace distinction actually matters: pushing for
// something the user did not choose truncates the forward tail (destroying
// entries Forward could still reach) and appends a new entry every time it
// repeats. That cannot be observed from the pure core, hence this suite.
//
// The vendored LinkeDOM runtime implements no browsing context, so `history`
// and `window.location` are installed here as recording fakes. Everything else
// — the module graph, the event wiring, the location model — is production code.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// installBrowsingContext gives the adapter the two browser APIs LinkeDOM lacks,
// recording every mutation so a test can assert on the exact call sequence.
function installBrowsingContext(t, harness, startPath = '/') {
  const calls = [];
  let current = startPath;
  const location = {
    get pathname() { return current.split('?')[0]; },
    get search() { const q = current.split('?')[1]; return q ? `?${q}` : ''; },
  };
  const history = {
    state: null,
    pushState(state, _title, url) { this.state = state; current = url; calls.push({ kind: 'push', url }); },
    replaceState(state, _title, url) { this.state = state; current = url; calls.push({ kind: 'replace', url }); },
  };
  const previous = ['history', 'location'].map((name) => [
    name, Object.getOwnPropertyDescriptor(globalThis, name),
  ]);
  for (const [name, value] of [['history', history], ['location', location]]) {
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
    Object.defineProperty(harness.window, name, { configurable: true, writable: true, value });
  }
  t.after(() => {
    for (const [name, descriptor] of previous) {
      if (descriptor === undefined) delete globalThis[name];
      else Object.defineProperty(globalThis, name, descriptor);
    }
  });
  return { calls, history, at: () => current };
}

// The adapter reads the active tab out of a real <nav>, and treats a tab as
// unavailable when its button is missing.
function installShell(harness) {
  harness.document.body.innerHTML = `
    <nav>
      <button data-tab="groups" class="active"></button>
      <button data-tab="processes"></button>
      <button data-tab="jobs"></button>
    </nav>
    <main><section id="tab-processes"></section></main>`;
}

test('a correction replaces the current history entry; a navigation pushes', async (t) => {
  const harness = await createPreactHarness(t);
  installShell(harness);
  const browsing = installBrowsingContext(t, harness, '/processes/templates');

  // The Processes island publishes its location through the feature registry;
  // the adapter reads it there rather than from the DOM.
  const [{ registerFeatureState }, { initNavHistory }] = await Promise.all([
    harness.importDashboardModule('js/feature-state-registry.js'),
    harness.importDashboardModule('js/nav-history.js'),
  ]);
  const islandLocation = harness.signals.signal({ tab: 'processes', subtab: 'templates' });
  registerFeatureState('processes', { location: islandLocation });

  initNavHistory();
  // Boot canonicalises the current entry in place — never a push.
  assert.deepEqual(browsing.calls, [{ kind: 'replace', url: '/processes/templates' }]);
  browsing.calls.length = 0;

  // Opening the editor is a real navigation: it must PUSH, so Back returns to
  // the list.
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:navigated', {
    detail: { location: { tab: 'processes', subtab: 'templates', selection: 'release-train' } },
  }));
  assert.deepEqual(browsing.calls, [{ kind: 'push', url: '/processes/templates/release-train' }]);
  browsing.calls.length = 0;

  // A correction is NOT a user navigation. It must replace, because the browser
  // has already moved — pushing here is what destroys the forward tail.
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:navigated', {
    detail: {
      location: { tab: 'processes', subtab: 'templates', selection: 'release-train' },
      correction: true,
    },
  }));
  assert.deepEqual(browsing.calls, [
    { kind: 'replace', url: '/processes/templates/release-train' },
  ], 'a correction never pushes');
});

test('a repeated refusal does not grow history without bound', async (t) => {
  const harness = await createPreactHarness(t);
  installShell(harness);
  const browsing = installBrowsingContext(t, harness, '/processes/templates/release-train');

  const [{ registerFeatureState }, { initNavHistory }] = await Promise.all([
    harness.importDashboardModule('js/feature-state-registry.js'),
    harness.importDashboardModule('js/nav-history.js'),
  ]);
  registerFeatureState('processes', {
    location: harness.signals.signal({
      tab: 'processes', subtab: 'templates', selection: 'release-train',
    }),
  });
  initNavHistory();
  browsing.calls.length = 0;

  // An operator holding an unsaved editor may press Back many times; each
  // refusal answers with a correction. Every one of them must be a replace, so
  // the entry count is stable no matter how often it repeats.
  for (let i = 0; i < 5; i++) {
    harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:navigated', {
      detail: {
        location: { tab: 'processes', subtab: 'templates', selection: 'release-train' },
        correction: true,
      },
    }));
  }
  assert.equal(browsing.calls.length, 5);
  assert.equal(browsing.calls.every((call) => call.kind === 'replace'), true,
    'refusals never accumulate history entries');
  assert.equal(browsing.at(), '/processes/templates/release-train');
});

test('a terminals deep link survives boot with the tab still hidden', async (t) => {
  // The ordering hazard behind /terminals/<agent-id>: panes do not survive a
  // reload, so at the instant the router boots there are none, the Terminals
  // tab is CSS-hidden (no nav button in this shell) and the ordinary
  // stale-target guard would bounce the URL to the default tab — killing the
  // reattach before it could run. A deep link must be let through instead, and
  // handed to the island to attach.
  const harness = await createPreactHarness(t);
  installShell(harness);
  const browsing = installBrowsingContext(t, harness, '/terminals/agt_abc123');
  const { initNavHistory } = await harness.importDashboardModule('js/nav-history.js');

  const restores = [];
  harness.document.addEventListener('tclaude:restore-location',
    (event) => restores.push(event.detail.location));

  initNavHistory();

  assert.equal(browsing.at(), '/terminals/agt_abc123', 'the deep link must not be bounced to /');
  assert.deepEqual(browsing.calls, [{ kind: 'replace', url: '/terminals/agt_abc123' }]);
  assert.deepEqual(restores, [{ tab: 'terminals', selection: 'agt_abc123' }],
    'the island is asked to reattach the agent named in the URL');
});

test('a bare /terminals with no terminals open still falls back', async (t) => {
  // Only a deep link earns the exemption above. A bare /terminals names no
  // agent, so there is nothing to reattach and nothing to show — the original
  // stale-target fallback must still apply.
  const harness = await createPreactHarness(t);
  installShell(harness);
  const browsing = installBrowsingContext(t, harness, '/terminals');
  const { initNavHistory } = await harness.importDashboardModule('js/nav-history.js');

  const restores = [];
  harness.document.addEventListener('tclaude:restore-location',
    (event) => restores.push(event.detail.location));

  initNavHistory();

  assert.equal(browsing.at(), '/', 'a bare hidden tab still falls back to the default');
  assert.deepEqual(restores, []);
});

test('an explicit Groups Route map deep link restores without changing Members fallback semantics', async (t) => {
  const harness = await createPreactHarness(t);
  installShell(harness);
  const browsing = installBrowsingContext(t, harness, '/groups/routes/rte_123');
  const [{ registerFeatureState }, { initNavHistory }] = await Promise.all([
    harness.importDashboardModule('js/feature-state-registry.js'),
    harness.importDashboardModule('js/nav-history.js'),
  ]);
  const groupsSubview = harness.signals.signal('members');
  const routeSelection = harness.signals.signal('');
  const unregister = registerFeatureState('groups', {
    subview: groupsSubview, routeSelection,
  });
  t.after(unregister);
  const restores = [];
  harness.document.addEventListener('tclaude:restore-location',
    (event) => restores.push(event.detail.location));

  initNavHistory();

  assert.equal(browsing.at(), '/groups/routes/rte_123');
  assert.deepEqual(restores, [{ tab: 'groups', subtab: 'routes', selection: 'rte_123' }],
    'an explicit Route map deep link still asks Groups to restore the route detail');

  // Once the Route map is active, a browser Back to bare Groups must ask the
  // island to return to Members. This is distinct from boot fallback, where
  // Members is already the default and no restore event should be synthesized.
  groupsSubview.value = 'routes';
  routeSelection.value = 'rte_123';
  browsing.history.replaceState(browsing.history.state, '', '/');
  browsing.calls.length = 0;
  restores.length = 0;
  harness.window.dispatchEvent(new harness.window.Event('popstate'));
  assert.deepEqual(restores, [{ tab: 'groups' }],
    'a Route map to Members traversal remains an explicit restore');
});

test('switching between two terminals pushes one entry each', async (t) => {
  const harness = await createPreactHarness(t);
  installShell(harness);
  const browsing = installBrowsingContext(t, harness, '/terminals/agt_one');
  const [{ registerFeatureState }, { initNavHistory }] = await Promise.all([
    harness.importDashboardModule('js/feature-state-registry.js'),
    harness.importDashboardModule('js/nav-history.js'),
  ]);
  const islandLocation = harness.signals.signal({ tab: 'terminals', selection: 'agt_one' });
  registerFeatureState('terminals', { location: islandLocation });

  initNavHistory();
  browsing.calls.length = 0;

  // Clicking another terminal's tab is a real navigation, so Back returns to
  // the terminal you were on before.
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:navigated', {
    detail: { location: { tab: 'terminals', selection: 'agt_two' } },
  }));
  assert.deepEqual(browsing.calls, [{ kind: 'push', url: '/terminals/agt_two' }]);
  browsing.calls.length = 0;

  // Re-announcing the terminal already being viewed is a duplicate and must not
  // grow history (AC #4).
  islandLocation.value = { tab: 'terminals', selection: 'agt_two' };
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:navigated', {
    detail: { location: { tab: 'terminals', selection: 'agt_two' } },
  }));
  assert.deepEqual(browsing.calls, []);
});

test('the adapter reads the open editor id from the island, not the DOM', async (t) => {
  const harness = await createPreactHarness(t);
  installShell(harness);
  const browsing = installBrowsingContext(t, harness, '/');

  const [{ registerFeatureState }, { initNavHistory }] = await Promise.all([
    harness.importDashboardModule('js/feature-state-registry.js'),
    harness.importDashboardModule('js/nav-history.js'),
  ]);
  const islandLocation = harness.signals.signal({
    tab: 'processes', subtab: 'templates', selection: 'release-train',
  });
  registerFeatureState('processes', { location: islandLocation });
  initNavHistory();
  browsing.calls.length = 0;

  // A plain nav click carries no location detail, so the adapter has to derive
  // one. The template id exists only in island state — a DOM read would silently
  // drop it and leave the URL describing the list while the editor is showing.
  harness.document.querySelector('nav [data-tab="processes"]').classList.add('active');
  harness.document.querySelector('nav [data-tab="groups"]').classList.remove('active');
  harness.document.querySelector('nav [data-tab="processes"]').click();

  assert.deepEqual(browsing.calls, [
    { kind: 'push', url: '/processes/templates/release-train' },
  ]);
});
