import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

// Mount the Preact-owned action-dialog root with a task-link descriptor already
// open, an invoker focused so restoration is observable, and a recording
// setTaskLink mock. Domain/HTTP behavior is covered separately against the real
// action module below.
async function mountTaskLink(t, descriptor, { confirmDiscard = async () => false, ...overrides } = {}) {
  const harness = await createPreactHarness(t);
  const [{ createActionDialogState }, { ActionDialogApp }] = await Promise.all([
    harness.importDashboardModule('js/action-dialog-state.js'),
    harness.importDashboardModule('js/action-dialog-island.js'),
  ]);
  const state = createActionDialogState();
  const calls = [];
  const actions = {
    openTaskLink: state.openTaskLink,
    close: state.close,
    setTaskLink: async (value) => { calls.push(value); },
    ...overrides,
  };
  const invoker = harness.document.body.appendChild(harness.document.createElement('button'));
  invoker.id = 'task-invoker';
  invoker.focus();
  state.openTaskLink(descriptor);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`
    <${ActionDialogApp} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} />
  `, host);
  return { harness, host, state, actions, calls, invoker, cleanup: () => mounted.unmount() };
}

test('task cells separate navigation from editing and retain raw edit values', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/prefs.js', `
    export const dashPrefs = {
      getItem: (key) => key === 'tclaude.dash.group.tasks' ? '1' : null,
      setItem: () => {}, removeItem: () => {},
    };
  `);
  const [{ GroupsNativeList }, { GroupsInteractionProvider }] = await Promise.all([
    harness.importDashboardModule('js/groups-list.js'),
    harness.importDashboardModule('js/groups-interactions.js'),
  ]);
  const members = [{ conv_id: 'empty', agent_id: 'agt_empty', title: 'empty', online: true, state: {} }, {
    conv_id: 'set', agent_id: 'agt_set', title: 'set', online: true, state: {},
    task_ref_url: 'https://example.com/work/42?x=1&y=2',
    task_ref_label: 'Release blocker',
    task_ref_label_override: 'Release blocker',
  }, {
    conv_id: 'unsafe', agent_id: 'agt_unsafe', title: 'unsafe', online: true, state: {},
    task_ref_url: 'javascript:alert(1)', task_ref_label: 'bad',
  }];
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${GroupsInteractionProvider}>
    <${GroupsNativeList}
      groups=${[{ name: 'tasks', members, online: members.length }]}
      snapshot=${{ activity_bots: {}, links: [], sudo: [] }}
      actions=${{}}
    />
  <//>`, host);
  const taskCell = (key) => host.querySelector(`tr[data-key="${key}"] .task-cell`);

  const empty = taskCell('empty');
  assert.ok(empty.querySelector('.task-attach'));
  assert.equal(empty.querySelector('.task-attach').dataset.act, 'edit-task');
  assert.match(empty.textContent, /✧ bind quest/);
  assert.equal(empty.querySelector('a'), null);

  const set = taskCell('set');
  const link = set.querySelector('a.task-ref.task-link');
  assert.ok(link);
  assert.match(set.textContent, /Release blocker/);
  assert.equal(link.getAttribute('href'), 'https://example.com/work/42?x=1&y=2');
  const edit = set.querySelector('.task-edit.task-edit-icon');
  assert.ok(edit);
  assert.equal(edit.dataset.currentTaskLabel, 'Release blocker');
  assert.equal(edit.textContent, '✎');

  const unsafe = taskCell('unsafe');
  assert.equal(unsafe.querySelector('a'), null, 'stored unsafe values remain inert');
  assert.ok(unsafe.querySelector('.task-edit-icon'), 'an unsafe legacy value remains editable');
  await mounted.unmount();
});

test('task-link dialog prefills, selects the URL, and submits the changed pair', async (t) => {
  const mounted = await mountTaskLink(t, {
    conv: 'agt-alice', agentLabel: 'alice',
    url: 'https://linear.app/acme/issue/JOH-42/work', taskLabel: 'Launch task',
  });
  const { harness, host, calls } = mounted;
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));

  assert.equal(host.querySelector('#task-link-meta').textContent, 'alice');
  const url = host.querySelector('#task-link-url');
  assert.equal(url.value, 'https://linear.app/acme/issue/JOH-42/work');
  assert.equal(host.querySelector('#task-link-label').value, 'Launch task');
  // The shared dialog lifecycle focuses the first control and honours
  // data-select-on-focus, so the prefilled URL is focused for quick replacement.
  assert.equal(harness.document.activeElement, url);

  await harness.input(host.querySelector('#task-link-label'), 'Release task');
  host.querySelector('#task-link-save').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(calls[0], {
    conv: 'agt-alice', label: 'alice',
    url: 'https://linear.app/acme/issue/JOH-42/work', taskLabel: 'Release task', changed: true,
  });
  await mounted.cleanup();
});

test('a repeated open cannot overwrite the live draft or retarget save', async (t) => {
  const mounted = await mountTaskLink(t, {
    conv: 'agt-alice', agentLabel: 'alice', url: 'https://example.com/alice', taskLabel: '',
  });
  const { harness, host, state, calls } = mounted;
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));

  // Edit the draft, then a second open for a *different* agent arrives (a
  // repeated pencil click or a programmatic launch). The legacy controller
  // refused this so it could neither clobber the draft nor retarget Save.
  await harness.input(host.querySelector('#task-link-url'), 'https://example.com/alice-edited');
  state.openTaskLink({ conv: 'agt-bob', agentLabel: 'bob', url: 'https://example.com/bob', taskLabel: 'Bob' });
  await harness.act(() => Promise.resolve());

  assert.equal(state.dialog.value.conv, 'agt-alice');
  assert.equal(host.querySelector('#task-link-meta').textContent, 'alice');
  assert.equal(host.querySelector('#task-link-url').value, 'https://example.com/alice-edited');

  host.querySelector('#task-link-save').click();
  await harness.act(() => Promise.resolve());
  assert.equal(calls[0].conv, 'agt-alice');
  assert.equal(calls[0].url, 'https://example.com/alice-edited');
  await mounted.cleanup();
});

test('task-link dialog enforces url rules, no-ops, and clears', async (t) => {
  // A display name without a URL has nothing to attach to.
  {
    const m = await mountTaskLink(t, { conv: 'c', agentLabel: 'a', url: '', taskLabel: '' });
    await m.harness.act(() => new Promise((r) => setTimeout(r, 0)));
    await m.harness.input(m.host.querySelector('#task-link-label'), 'orphan label');
    m.host.querySelector('#task-link-save').click();
    await m.harness.act(() => Promise.resolve());
    assert.match(m.host.querySelector('#task-link-error').textContent, /Enter a URL/);
    assert.equal(m.calls.length, 0, 'an invalid submit is not routed to the action');
    await m.cleanup();
  }
  // Only http(s) URLs persist; a stored javascript: value cannot be re-saved.
  {
    const m = await mountTaskLink(t, { conv: 'c', agentLabel: 'a', url: '', taskLabel: '' });
    await m.harness.act(() => new Promise((r) => setTimeout(r, 0)));
    await m.harness.input(m.host.querySelector('#task-link-url'), 'javascript:alert(1)');
    m.host.querySelector('#task-link-save').click();
    await m.harness.act(() => Promise.resolve());
    assert.match(m.host.querySelector('#task-link-error').textContent, /http:\/\//);
    assert.equal(m.calls.length, 0);
    await m.cleanup();
  }
  // An unchanged submit routes as a no-op the action can short-circuit.
  {
    const m = await mountTaskLink(t, { conv: 'c', agentLabel: 'a', url: 'https://example.com/x', taskLabel: 'X' });
    await m.harness.act(() => new Promise((r) => setTimeout(r, 0)));
    m.host.querySelector('#task-link-save').click();
    await m.harness.act(() => Promise.resolve());
    assert.deepEqual(m.calls[0], { conv: 'c', label: 'a', url: 'https://example.com/x', taskLabel: 'X', changed: false });
    await m.cleanup();
  }
  // Emptying both fields clears the reference (changed, but no URL).
  {
    const m = await mountTaskLink(t, { conv: 'c', agentLabel: 'a', url: 'https://example.com/x', taskLabel: 'X' });
    await m.harness.act(() => new Promise((r) => setTimeout(r, 0)));
    await m.harness.input(m.host.querySelector('#task-link-url'), '');
    await m.harness.input(m.host.querySelector('#task-link-label'), '');
    m.host.querySelector('#task-link-save').click();
    await m.harness.act(() => Promise.resolve());
    assert.deepEqual(m.calls[0], { conv: 'c', label: 'a', url: '', taskLabel: '', changed: true });
    await m.cleanup();
  }
});

test('task-link submit is single-flight within one turn and releases after success', async (t) => {
  const requests = [];
  const mounted = await mountTaskLink(t, {
    conv: 'agt-alice', agentLabel: 'alice', url: '', taskLabel: '',
  }, {
    setTaskLink: () => {
      const request = deferred();
      requests.push(request);
      return request.promise;
    },
  });
  const { harness, host } = mounted;
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  // LinkeDOM does not implement this browser method used by ErrorBanner's
  // validation-error reveal effect.
  host.querySelector('#task-link-error').scrollIntoView = () => {};

  const submit = () => host.querySelector('#task-link-save').click();
  await harness.input(host.querySelector('#task-link-label'), 'Launch task');
  await harness.act(() => { submit(); submit(); });
  assert.equal(requests.length, 0, 'validation does not start a mutation');

  await harness.input(host.querySelector('#task-link-url'), 'https://example.com/task');
  await harness.act(() => { submit(); submit(); });
  assert.equal(requests.length, 1, 'same-turn valid submits set one task link');
  await harness.act(async () => {
    requests[0].resolve();
    await Promise.resolve();
  });
  await harness.act(() => { submit(); submit(); });
  assert.equal(requests.length, 2, 'validation and success both release the guard');
  await harness.act(async () => {
    requests[1].resolve();
    await Promise.resolve();
  });
  await mounted.cleanup();
});

test('dirty task-link dialog confirms discard and restores the invoker', async (t) => {
  const decisions = [false, true];
  let confirmations = 0;
  const mounted = await mountTaskLink(t, {
    conv: 'c', agentLabel: 'alice', url: 'https://example.com/old', taskLabel: '',
  }, { confirmDiscard: async () => { confirmations += 1; return decisions.shift(); } });
  const { harness, host, invoker } = mounted;
  await harness.act(() => new Promise((r) => setTimeout(r, 0)));

  await harness.input(host.querySelector('#task-link-url'), 'https://example.com/new');
  const escape = () => {
    const event = new harness.window.Event('keydown', { bubbles: true });
    Object.defineProperty(event, 'key', { value: 'Escape' });
    harness.document.dispatchEvent(event);
  };

  escape();
  await harness.act(() => new Promise((r) => setTimeout(r, 0)));
  assert.ok(host.querySelector('#task-link-modal'), 'a denied discard keeps the dirty dialog open');
  assert.equal(confirmations, 1);

  escape();
  await harness.act(() => new Promise((r) => setTimeout(r, 0)));
  assert.equal(host.querySelector('#task-link-modal'), null, 'a confirmed discard closes the dialog');
  assert.equal(confirmations, 2);
  assert.equal(harness.document.activeElement, invoker, 'closing restores the edit-pencil invoker');
});

test('Enter saves only from a field, never composing, never via a global hotkey', async (t) => {
  const mounted = await mountTaskLink(t, {
    conv: 'c', agentLabel: 'alice', url: 'https://example.com/x', taskLabel: 'X',
  });
  const { harness, host, calls } = mounted;
  await harness.act(() => new Promise((r) => setTimeout(r, 0)));
  await harness.input(host.querySelector('#task-link-label'), 'Renamed');
  const label = host.querySelector('#task-link-label');
  const fieldEnter = (init = {}) => {
    const event = new harness.window.Event('keydown', { bubbles: true, cancelable: true });
    Object.defineProperties(event, {
      key: { value: 'Enter' },
      isComposing: { value: !!init.isComposing },
      ctrlKey: { value: !!init.ctrlKey },
      metaKey: { value: !!init.metaKey },
    });
    return event;
  };

  // Plain and modified Enter while composing commit the IME candidate, not the
  // form — the composition guard must hold for Ctrl/⌘+Enter too.
  for (const modifier of [{}, { ctrlKey: true }, { metaKey: true }]) {
    const composing = fieldEnter({ isComposing: true, ...modifier });
    label.dispatchEvent(composing);
    await harness.act(() => Promise.resolve());
    assert.equal(composing.defaultPrevented, false);
  }
  assert.equal(calls.length, 0, 'no composing Enter submits');

  // Ctrl/⌘+Enter outside the fields must not submit: there is no global submit
  // hotkey that fires regardless of target. Dispatch from the heading rather
  // than a button so this assertion tests bubbling without LinkeDOM's synthetic
  // button-activation behavior getting involved.
  const nonFieldHotkey = fieldEnter({ ctrlKey: true });
  host.querySelector('#task-link-title').dispatchEvent(nonFieldHotkey);
  await harness.act(() => Promise.resolve());
  assert.equal(nonFieldHotkey.defaultPrevented, false);
  assert.equal(calls.length, 0, 'a hotkey outside the fields does not submit');

  // A committed Ctrl/⌘+Enter originating in a text field saves (matching the
  // legacy controller, which ignored modifiers on a field Enter).
  const enter = fieldEnter({ ctrlKey: true });
  label.dispatchEvent(enter);
  await harness.act(() => Promise.resolve());
  assert.equal(enter.defaultPrevented, true);
  assert.equal(calls[0].taskLabel, 'Renamed');
  assert.equal(calls[0].changed, true);
  await mounted.cleanup();
});

test('setTaskLink posts, clears, no-ops, notifies, and refreshes', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createActionDialogState }, { createActionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/action-dialog-state.js'),
    harness.importDashboardModule('js/action-dialog-actions.js'),
  ]);
  const state = createActionDialogState();
  const requests = [];
  const notices = [];
  let refreshes = 0;
  const fetchImpl = async (url, options) => {
    requests.push([url, options]);
    return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  const actions = createActionDialogActions({
    state, fetchImpl,
    notify: (...args) => notices.push(args),
    refresh: async () => { refreshes += 1; },
  });

  // Update: the daemon owns label derivation, so a blank label is sent blank.
  state.openTaskLink({ conv: 'agt-1', agentLabel: 'alice' });
  await actions.setTaskLink({ conv: 'agt-1', label: 'alice', url: 'https://example.com/x', taskLabel: 'X', changed: true }, state.dialog.value);
  assert.equal(requests[0][0], '/api/agents/agt-1/task');
  assert.equal(requests[0][1].method, 'POST');
  assert.deepEqual(JSON.parse(requests[0][1].body), { url: 'https://example.com/x', label: 'X' });
  assert.deepEqual(notices[0], ['task link updated: alice']);
  assert.equal(state.dialog.value, null);

  // Clear: an empty URL persists {clear:true}.
  state.openTaskLink({ conv: 'agt-1', agentLabel: 'alice' });
  await actions.setTaskLink({ conv: 'agt-1', label: 'alice', url: '', taskLabel: '', changed: true }, state.dialog.value);
  assert.deepEqual(JSON.parse(requests[1][1].body), { clear: true });
  assert.deepEqual(notices[1], ['task link cleared: alice']);

  // No-op: no request, and no refresh.
  state.openTaskLink({ conv: 'agt-1', agentLabel: 'alice' });
  await actions.setTaskLink({ conv: 'agt-1', label: 'alice', url: 'https://example.com/x', taskLabel: 'X', changed: false }, state.dialog.value);
  assert.equal(requests.length, 2, 'a no-op submit performs no request');
  assert.deepEqual(notices[2], ['no changes']);
  assert.equal(state.dialog.value, null);
  assert.equal(refreshes, 2, 'only real mutations refresh');
});
