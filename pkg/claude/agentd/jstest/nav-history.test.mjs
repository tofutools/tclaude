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
    origin: 'http://dashboard.test',
    get pathname() { return current.split('?')[0]; },
    get search() { const q = current.split('?')[1]; return q ? `?${q}` : ''; },
  };
  const history = {
    state: null,
    pushState(state, _title, url) { this.state = state; current = url; calls.push({ kind: 'push', url }); },
    replaceState(state, _title, url) { this.state = state; current = url; calls.push({ kind: 'replace', url }); },
    back() { calls.push({ kind: 'back' }); },
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

// The Activity shell: one nav anchor plus the static subnav/panels that
// activity-tab.js binds.
function installActivityShell(harness) {
  harness.document.body.innerHTML = `
    <nav>
      <button data-tab="groups"></button>
      <button data-tab="diagnostics" class="active"></button>
    </nav>
    <main><section id="tab-diagnostics" class="active">
      <a class="activity-subtab active" data-subtab="audit" href="/diagnostics/audit">Audit</a>
      <a class="activity-subtab" data-subtab="logs" href="/diagnostics/logs">Logs</a>
      <a class="activity-subtab" data-subtab="performance" href="/diagnostics/performance">Performance</a>
      <div class="activity-panel active" id="activity-audit"></div>
      <div class="activity-panel" id="activity-logs"></div>
      <div class="activity-panel" id="activity-performance"></div>
    </section></main>`;
}

test('a retired /logs deep link heals to /diagnostics/logs; subtabs push and Back restores', async (t) => {
  const harness = await createPreactHarness(t);
  installActivityShell(harness);
  const browsing = installBrowsingContext(t, harness, '/logs?wizard=1');
  const [{ initNavHistory }, { bindActivityTab }, { activitySubtab }] = await Promise.all([
    harness.importDashboardModule('js/nav-history.js'),
    harness.importDashboardModule('js/activity-tab.js'),
    harness.importDashboardModule('js/activity-state.js'),
  ]);
  const unbind = bindActivityTab({ doc: harness.document });
  t.after(unbind);
  const panel = (name) => harness.document.getElementById(`activity-${name}`);

  initNavHistory();
  assert.deepEqual(browsing.calls, [{ kind: 'replace', url: '/diagnostics/logs' }],
    'the old path is rewritten in place — no extra Back step');
  assert.equal(activitySubtab.value, 'logs');
  assert.equal(panel('logs').classList.contains('active'), true);
  assert.equal(panel('audit').classList.contains('active'), false);
  browsing.calls.length = 0;

  harness.document.querySelector('[data-subtab="performance"]').click();
  assert.deepEqual(browsing.calls, [{ kind: 'push', url: '/diagnostics/performance' }]);
  assert.equal(panel('performance').classList.contains('active'), true);
  const atPerformance = browsing.history.state;
  harness.document.querySelector('[data-subtab="performance"]').click();
  assert.equal(browsing.calls.length, 1, 're-selecting the shown subtab adds no entry');
  browsing.calls.length = 0;

  // Back: the browser has moved to the previous entry and fires popstate.
  browsing.history.replaceState({ ...atPerformance, navIndex: 0 }, '', '/diagnostics/logs');
  browsing.calls.length = 0;
  const back = new harness.window.Event('popstate');
  back.state = browsing.history.state;
  harness.window.dispatchEvent(back);
  assert.equal(activitySubtab.value, 'logs', 'Back restores the previous subtab');
  assert.deepEqual(browsing.calls.map((call) => call.kind), ['replace'], 'traversal never pushes');

  // A modified click is left to the browser (new tab), so nothing changes here.
  const modified = harness.fireEvent(harness.document.querySelector('[data-subtab="audit"]'), 'click', { ctrlKey: true });
  assert.equal(modified.defaultPrevented, false);
  assert.equal(activitySubtab.value, 'logs');
});

test('navigateToPath refuses an unknown tab instead of reporting Groups', async (t) => {
  const harness = await createPreactHarness(t);
  installShell(harness);
  const browsing = installBrowsingContext(t, harness, '/processes');
  const { initNavHistory, navigateToPath } = await harness.importDashboardModule('js/nav-history.js');
  initNavHistory();
  browsing.calls.length = 0;
  assert.equal(navigateToPath('/not-a-tab'), false);
  assert.deepEqual(browsing.calls, [], 'nothing moved');
  assert.equal(navigateToPath('/groups'), true);
  assert.equal(browsing.calls[0].kind, 'push');
});

test('a peer view keeps a Logs deep link until the peer answers (unrestricted trust serves it)', async (t) => {
  const harness = await createPreactHarness(t);
  installActivityShell(harness);
  const browsing = installBrowsingContext(t, harness, '/diagnostics/logs');
  const previous = globalThis.__tclaudeRemoteNode;
  globalThis.__tclaudeRemoteNode = { id: 'inst_forge7' };
  t.after(() => { globalThis.__tclaudeRemoteNode = previous; });
  const [{ initNavHistory }, { activitySubtab }] = await Promise.all([
    harness.importDashboardModule('js/nav-history.js'),
    harness.importDashboardModule('js/activity-state.js'),
  ]);
  initNavHistory();
  assert.equal(activitySubtab.value, 'logs', 'an unrestricted peer serves the daemon log');
  assert.equal(browsing.at(), '/diagnostics/logs?node=inst_forge7', 'boot keeps the location');
  assert.equal(browsing.calls.every((call) => call.kind === 'replace'), true);
});

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
  // Boot canonicalises the current entry in place — never a push. The legacy
  // top-level /processes route heals to its Automations-nested form.
  assert.deepEqual(browsing.calls, [{ kind: 'replace', url: '/automations/processes/templates' }]);
  browsing.calls.length = 0;

  // Opening the editor is a real navigation: it must PUSH, so Back returns to
  // the list.
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:navigated', {
    detail: { location: { tab: 'processes', subtab: 'templates', selection: 'release-train' } },
  }));
  assert.deepEqual(browsing.calls, [{ kind: 'push', url: '/automations/processes/templates/release-train' }]);
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
    { kind: 'replace', url: '/automations/processes/templates/release-train' },
  ], 'a correction never pushes');
});

test('a repeated refusal does not grow history without bound', async (t) => {
  const harness = await createPreactHarness(t);
  installShell(harness);
  const browsing = installBrowsingContext(t, harness, '/automations/processes/templates/release-train');

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
  assert.equal(browsing.at(), '/automations/processes/templates/release-train');
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

test('a Groups Route map deep link falls back when the opt-in flag is off', async (t) => {
  const harness = await createPreactHarness(t);
  installShell(harness);
  harness.document.body.classList.add('hide-groups-route-map');
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

  assert.equal(browsing.at(), '/', 'disabled Route map deep links fall back to Members');
  assert.deepEqual(restores, [], 'disabled fallback does not synthesize a route restore');
  assert.equal(groupsSubview.value, 'members');
  assert.equal(routeSelection.value, '');
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
    { kind: 'push', url: '/automations/processes/templates/release-train' },
  ]);
});

async function bootGroupsFocus(t, startPath, { focus = 'both', processesHidden = false } = {}) {
  const harness = await createPreactHarness(t);
  installShell(harness);
  if (processesHidden) harness.document.body.classList.add('hide-processes');
  const browsing = installBrowsingContext(t, harness, startPath);
  const [{ registerFeatureState }, { initNavHistory }] = await Promise.all([
    harness.importDashboardModule('js/feature-state-registry.js'),
    harness.importDashboardModule('js/nav-history.js'),
  ]);
  const groupsFocus = harness.signals.signal(focus);
  const focusedRun = harness.signals.signal(null);
  const unregister = registerFeatureState('groups', {
    subview: harness.signals.signal('members'), routeSelection: harness.signals.signal(''), focus: groupsFocus, focusedRun,
  });
  t.after(unregister);
  const restores = [];
  harness.document.addEventListener('tclaude:restore-location', (event) => {
    restores.push(event.detail.location);
    if (event.detail.location.tab === 'groups') {
      const { subtab } = event.detail.location;
      if (subtab === 'runs') {
        focusedRun.value = { group: event.detail.location.selection, runId: event.detail.location.item };
        return;
      }
      focusedRun.value = null;
      groupsFocus.value = subtab === 'processes' || subtab === 'agents' ? subtab : 'both';
    }
  });
  initNavHistory();
  return { harness, browsing, restores, groupsFocus, focusedRun };
}

test('a /groups/processes deep link restores the Processes focus', async (t) => {
  const { browsing, restores, groupsFocus } = await bootGroupsFocus(t, '/groups/processes');
  assert.equal(browsing.at(), '/groups/processes');
  assert.deepEqual(restores, [{ tab: 'groups', subtab: 'processes' }]);
  assert.equal(groupsFocus.value, 'processes');
});

test('a /groups/agents deep link restores the Agents focus; bare Groups is Both', async (t) => {
  const { browsing, restores, groupsFocus } = await bootGroupsFocus(t, '/groups/agents');
  assert.equal(browsing.at(), '/groups/agents');
  assert.deepEqual(restores, [{ tab: 'groups', subtab: 'agents' }]);
  assert.equal(groupsFocus.value, 'agents');
});

test('a fresh bare load in Both keeps the bare URL', async (t) => {
  const fused = await bootGroupsFocus(t, '/');
  assert.equal(fused.browsing.at(), '/');
  assert.equal(fused.groupsFocus.value, 'both');
});

test('a fresh bare load names the remembered Agents focus in the URL', async (t) => {
  const { browsing, groupsFocus } = await bootGroupsFocus(t, '/', { focus: 'agents' });
  assert.deepEqual(browsing.calls, [{ kind: 'replace', url: '/groups/agents' }]);
  assert.equal(groupsFocus.value, 'agents');
});

test('a /groups/processes deep link falls back to Groups while processes are off', async (t) => {
  const { browsing, restores, groupsFocus } = await bootGroupsFocus(t, '/groups/processes', { processesHidden: true });
  assert.equal(browsing.at(), '/');
  assert.deepEqual(restores, []);
  assert.equal(groupsFocus.value, 'both');
});

test('a fresh bare load names the remembered Processes focus in the URL', async (t) => {
  const { browsing, restores, groupsFocus } = await bootGroupsFocus(t, '/', { focus: 'processes' });
  assert.deepEqual(browsing.calls, [{ kind: 'replace', url: '/groups/processes' }],
    'the entry is rewritten in place, so a later Back onto it restores Processes');
  assert.deepEqual(restores, [{ tab: 'groups', subtab: 'processes' }]);
  assert.equal(groupsFocus.value, 'processes');
});

test('a remembered Processes focus stays out of the URL while processes are off, and survives', async (t) => {
  const { browsing, restores, groupsFocus } = await bootGroupsFocus(t, '/', { focus: 'processes', processesHidden: true });
  assert.equal(browsing.at(), '/');
  assert.deepEqual(restores, [], 'no synthetic restore clobbers the remembered lens');
  assert.equal(groupsFocus.value, 'processes');
});

test('a stale-target fallback lands on the remembered Processes lens', async (t) => {
  // /terminals with no panes (they do not survive a reload) is unavailable.
  const { browsing, restores, groupsFocus } = await bootGroupsFocus(t, '/terminals', { focus: 'processes' });
  assert.equal(browsing.at(), '/groups/processes');
  assert.deepEqual(restores, [{ tab: 'groups', subtab: 'processes' }]);
  assert.equal(groupsFocus.value, 'processes');
});

test('focus switches push entries and Back returns to the Both focus', async (t) => {
  const { harness, browsing, restores, groupsFocus } = await bootGroupsFocus(t, '/');
  browsing.calls.length = 0;
  groupsFocus.value = 'processes';
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:navigated', {
    detail: { location: { tab: 'groups', subtab: 'processes' } },
  }));
  assert.deepEqual(browsing.calls, [{ kind: 'push', url: '/groups/processes' }]);

  // Leaving and re-entering Groups through the nav reads the focus back.
  const nav = (tab) => {
    for (const b of harness.document.querySelectorAll('nav [data-tab]')) b.classList.toggle('active', b.dataset.tab === tab);
    harness.document.querySelector(`nav [data-tab="${tab}"]`).click();
  };
  nav('jobs');
  nav('groups');
  assert.equal(browsing.at(), '/groups/processes');

  browsing.history.replaceState({ ...browsing.history.state, navIndex: 0 }, '', '/');
  restores.length = 0;
  harness.window.dispatchEvent(new harness.window.Event('popstate'));
  assert.deepEqual(restores, [{ tab: 'groups' }], 'a bare Groups pop from Processes is an explicit restore');
  assert.equal(groupsFocus.value, 'both');
});

test('the Agents lens keeps its URL across tabs, and Back onto bare Groups returns to Both', async (t) => {
  const { harness, browsing, restores, groupsFocus } = await bootGroupsFocus(t, '/');
  browsing.calls.length = 0;
  groupsFocus.value = 'agents';
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:navigated', {
    detail: { location: { tab: 'groups', subtab: 'agents' } },
  }));
  assert.deepEqual(browsing.calls, [{ kind: 'push', url: '/groups/agents' }]);

  const nav = (tab) => {
    for (const b of harness.document.querySelectorAll('nav [data-tab]')) b.classList.toggle('active', b.dataset.tab === tab);
    harness.document.querySelector(`nav [data-tab="${tab}"]`).click();
  };
  nav('jobs');
  nav('groups');
  assert.equal(browsing.at(), '/groups/agents', 're-entering Groups names the Agents lens');

  browsing.history.replaceState({ ...browsing.history.state, navIndex: 0 }, '', '/');
  restores.length = 0;
  harness.window.dispatchEvent(new harness.window.Event('popstate'));
  assert.deepEqual(restores, [{ tab: 'groups' }], 'a bare Groups pop from Agents is an explicit restore');
  assert.equal(groupsFocus.value, 'both');
});

// installTabsShell is a nav whose buttons switch the active tab on click, the
// way bindTabs does, so a tab click reads back as a location.
function installTabsShell(harness, tabs) {
  harness.document.body.innerHTML = `<nav>${tabs.map((tab) => `<button data-tab="${tab}"></button>`).join('')}</nav>
    <main>${tabs.map((tab) => `<section id="tab-${tab}"></section>`).join('')}</main>`;
  const buttons = [...harness.document.querySelectorAll('nav [data-tab]')];
  for (const b of buttons) {
    b.addEventListener('click', () => {
      for (const o of buttons) o.classList.toggle('active', o === b);
      for (const s of harness.document.querySelectorAll('main section')) s.classList.toggle('active', s.id === `tab-${b.dataset.tab}`);
    });
  }
  buttons[0].classList.add('active');
  return buttons;
}

test('Config sections deep-link, push when picked, and a bare /config names the remembered one', async (t) => {
  const harness = await createPreactHarness(t);
  const [, config] = installTabsShell(harness, ['groups', 'config']);
  const browsing = installBrowsingContext(t, harness, '/config/daemon');
  const [{ initNavHistory }, panel] = await Promise.all([
    harness.importDashboardModule('js/nav-history.js'),
    harness.importDashboardModule('js/config-panel.js'),
  ]);
  initNavHistory();
  assert.equal(panel.configPanel.value, 'daemon');
  assert.equal(config.classList.contains('active'), true);
  assert.deepEqual(browsing.calls, [{ kind: 'replace', url: '/config/daemon' }]);
  browsing.calls.length = 0;

  // The rail picks a section: a navigation.
  panel.setConfigPanel('logs');
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:navigated', { detail: { location: panel.configLocation() } }));
  assert.deepEqual(browsing.calls, [{ kind: 'push', url: '/config/logs' }]);
  const atLogs = browsing.history.state;

  browsing.history.replaceState({ ...atLogs, navIndex: 0 }, '', '/config/daemon');
  const back = new harness.window.Event('popstate');
  back.state = browsing.history.state;
  harness.window.dispatchEvent(back);
  assert.equal(panel.configPanel.value, 'daemon', 'Back restores the previous section');

  // A bare /config (the nav anchor's href) keeps the open section and says so.
  browsing.history.replaceState({ ...atLogs, navIndex: 0 }, '', '/config');
  browsing.calls.length = 0;
  const bare = new harness.window.Event('popstate');
  bare.state = null;
  harness.window.dispatchEvent(bare);
  assert.equal(panel.configPanel.value, 'daemon');
  assert.equal(browsing.calls.at(-1).url, '/config/daemon');
});

test('a Messages deep link reaches the controller once it registers, and wins over the attention jump', async (t) => {
  const harness = await createPreactHarness(t);
  installTabsShell(harness, ['groups', 'messages']);
  installBrowsingContext(t, harness, '/messages/group:ops/42');
  const [{ initNavHistory }, bridge] = await Promise.all([
    harness.importDashboardModule('js/nav-history.js'),
    harness.importDashboardModule('js/mail-bridge.js'),
  ]);
  initNavHistory();
  const restored = []; const jumps = [];
  const unregister = bridge.registerMailController({
    restoreLocation: (loc) => restored.push(loc),
    focusNextAttention: () => jumps.push(1),
    mailLocation: () => ({ tab: 'messages', selection: 'group:ops', item: '42' }),
  });
  t.after(unregister);
  assert.deepEqual(restored, [{ tab: 'messages', selection: 'group:ops', item: '42' }]);
  assert.equal(bridge.focusNextMessagesAttention({}), undefined, 'the jump is suppressed for the deep link');
  assert.deepEqual(jumps, []);
});

test('Fleet config sections deep-link, push on the tab click, and restore on Back', async (t) => {
  const harness = await createPreactHarness(t);
  harness.document.body.innerHTML = `
    <nav>
      <button data-tab="groups"></button>
      <button data-tab="fleet-admin"></button>
    </nav>
    <main><section id="tab-fleet-admin"></section></main>`;
  const tabs = [...harness.document.querySelectorAll('nav [data-tab]')];
  for (const b of tabs) {
    b.addEventListener('click', () => { for (const o of tabs) o.classList.toggle('active', o === b); });
  }
  const browsing = installBrowsingContext(t, harness, '/fleet-admin/grants');
  const [{ initNavHistory }, page] = await Promise.all([
    harness.importDashboardModule('js/nav-history.js'),
    harness.importDashboardModule('js/fleet-admin-page.js'),
  ]);
  initNavHistory();
  assert.equal(page.fleetAdminPage.value, 'grants', 'the deep link is in place before the island mounts');
  assert.equal(tabs[1].classList.contains('active'), true);
  assert.deepEqual(browsing.calls, [{ kind: 'replace', url: '/fleet-admin/grants' }]);
  browsing.calls.length = 0;

  tabs[0].click();
  // A section asked for from elsewhere is recorded by the tab click.
  page.setFleetAdminPage('boards');
  tabs[1].click();
  assert.deepEqual(browsing.calls, [{ kind: 'push', url: '/' }, { kind: 'push', url: '/fleet-admin/boards' }]);
  // The island announces an on-screen section switch.
  page.setFleetAdminPage('hub');
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:navigated', { detail: { location: page.fleetAdminLocation() } }));
  assert.deepEqual(browsing.calls.at(-1), { kind: 'push', url: '/fleet-admin/hub' });
  const atHub = browsing.history.state;

  browsing.history.replaceState({ ...atHub, navIndex: 2 }, '', '/fleet-admin/boards');
  browsing.calls.length = 0;
  const back = new harness.window.Event('popstate');
  back.state = browsing.history.state;
  harness.window.dispatchEvent(back);
  assert.equal(page.fleetAdminPage.value, 'boards', 'Back restores the previous section');
  assert.deepEqual(browsing.calls.map((call) => call.kind), ['replace']);
});

test('a Messages search survives the deep link next to the page-global theme', async (t) => {
  const harness = await createPreactHarness(t);
  installTabsShell(harness, ['groups', 'messages']);
  harness.document.body.classList.add('slop');
  const browsing = installBrowsingContext(t, harness, '/messages/human?slop=1&q=deploy&page=2&stray=1');
  const [{ initNavHistory }, bridge] = await Promise.all([
    harness.importDashboardModule('js/nav-history.js'),
    harness.importDashboardModule('js/mail-bridge.js'),
  ]);
  initNavHistory();
  const restored = [];
  const unregister = bridge.registerMailController({ restoreLocation: (loc) => restored.push(loc) });
  t.after(unregister);
  const want = { tab: 'messages', selection: 'human', params: { q: 'deploy', page: '2' } };
  assert.deepEqual(restored, [want]);
  assert.equal(browsing.at(), '/messages/human?slop=1&q=deploy&page=2');
  // A later search edit is its own entry; Back returns to the earlier one.
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:navigated', {
    detail: { location: { tab: 'messages', selection: 'human', params: { q: 'rollback' } } },
  }));
  assert.equal(browsing.calls.at(-1).kind, 'push');
  assert.equal(browsing.at(), '/messages/human?slop=1&q=rollback');
});

test('Back from a run opened in Both closes it on bare Groups', async (t) => {
  const { harness, browsing, restores, focusedRun } = await bootGroupsFocus(t, '/');
  focusedRun.value = { group: 'ops', runId: 'run_1' };
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:navigated', {
    detail: { location: { tab: 'groups', subtab: 'runs', selection: 'ops', item: 'run_1' } },
  }));
  assert.equal(browsing.at(), '/groups/runs/ops/run_1');
  browsing.history.replaceState({ ...browsing.history.state, navIndex: 0 }, '', '/');
  restores.length = 0;
  harness.window.dispatchEvent(new harness.window.Event('popstate'));
  assert.deepEqual(restores, [{ tab: 'groups' }]);
  assert.equal(focusedRun.value, null);
});

test('an overlay over any tab is in the URL: opening pushes, closing steps back, Back closes it', async (t) => {
  const harness = await createPreactHarness(t);
  installShell(harness);
  const browsing = installBrowsingContext(t, harness, '/automations?manage=roles');
  const [{ initNavHistory }, overlays] = await Promise.all([
    harness.importDashboardModule('js/nav-history.js'),
    harness.importDashboardModule('js/overlay-routes.js'),
  ]);
  initNavHistory();
  assert.equal(browsing.at(), '/automations?manage=roles', 'kept while the manager island has not registered');
  // The island registers late and is handed the deep link's manager.
  let open = '';
  const restored = [];
  t.after(overlays.registerOverlayRoute('manage', { value: () => open, restore: (want) => { restored.push(want); open = want; } }));
  assert.deepEqual(restored, ['roles']);
  // Closing it right after the deep link has nothing to step back to: the entry is rewritten.
  browsing.calls.length = 0;
  open = '';
  overlays.announceOverlay({ closed: true, doc: harness.document });
  assert.deepEqual(browsing.calls.map((c) => c.kind + ' ' + (c.url || '')), ['replace /automations']);
  // Opening pushes, and the overlay stays in the URL across a tab switch under it.
  open = 'templates';
  overlays.announceOverlay({ doc: harness.document });
  assert.equal(browsing.calls.at(-1).kind, 'push');
  assert.equal(browsing.at(), '/automations?manage=templates');
  // Closing it right after opening is one gesture with Back.
  open = '';
  overlays.announceOverlay({ closed: true, doc: harness.document });
  assert.equal(browsing.calls.at(-1).kind, 'back');
  browsing.history.replaceState({ ...browsing.history.state, navIndex: 0 }, '', '/automations');
  harness.window.dispatchEvent(new harness.window.Event('popstate'));
  // Forward onto an entry with the overlay reopens it; Back off it closes it.
  restored.length = 0;
  browsing.history.replaceState({ ...browsing.history.state, navIndex: 1 }, '', '/automations?manage=templates');
  harness.window.dispatchEvent(new harness.window.Event('popstate'));
  browsing.history.replaceState({ ...browsing.history.state, navIndex: 0 }, '', '/automations');
  harness.window.dispatchEvent(new harness.window.Event('popstate'));
  assert.deepEqual(restored, ['templates', '']);
});

test('a navigation right after an overlay close waits for the step back to land', async (t) => {
  const harness = await createPreactHarness(t);
  installShell(harness);
  const browsing = installBrowsingContext(t, harness, '/automations');
  const [{ initNavHistory }, overlays] = await Promise.all([
    harness.importDashboardModule('js/nav-history.js'),
    harness.importDashboardModule('js/overlay-routes.js'),
  ]);
  initNavHistory();
  let open = '';
  t.after(overlays.registerOverlayRoute('notification', { value: () => open, restore: (want) => { open = want; } }));
  open = 'agt_a:5';
  overlays.announceOverlay({ doc: harness.document });
  assert.equal(browsing.at(), '/automations?notification=agt_a%3A5');
  // Another message in the reader rewrites the entry rather than adding one.
  open = 'agt_a:6';
  overlays.announceOverlay({ replace: true, doc: harness.document });
  assert.equal(browsing.calls.at(-1).kind, 'replace');
  // "Open in Messages": the reader closes, then a tab navigation follows at once.
  browsing.calls.length = 0;
  open = '';
  overlays.announceOverlay({ closed: true, doc: harness.document });
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:navigated', { detail: { location: { tab: 'groups', subtab: 'processes' } } }));
  assert.deepEqual(browsing.calls.map((c) => c.kind), ['back'], 'the push waits for the traversal');
  browsing.history.replaceState({ ...browsing.history.state, navIndex: 0 }, '', '/automations');
  harness.window.dispatchEvent(new harness.window.Event('popstate'));
  assert.equal(browsing.calls.at(-1).kind, 'push');
  assert.equal(browsing.at(), '/groups/processes');
});
