import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function mountDialogs(
  t,
  kind,
  descriptor,
  overrides = {},
  confirmDiscard = async () => false,
) {
  const harness = await createPreactHarness(t);
  const [{ createActionDialogState }, { ActionDialogApp }] = await Promise.all([
    harness.importDashboardModule('js/action-dialog-state.js'),
    harness.importDashboardModule('js/action-dialog-island.js'),
  ]);
  const state = createActionDialogState();
  state.dialog.value = { kind, ...descriptor };
  const calls = [];
  const actions = {
    openClone: state.openClone,
    openReincarnate: state.openReincarnate,
    openNest: state.openNest,
    finishChoice: state.finishChoice,
    close: state.close,
    nestModel: () => ({ currentParent: '', candidates: ['alpha', 'beta'] }),
    loadWorktrees: async () => ({
      is_repo: true,
      repo_root: '/repo',
      has_commits: true,
      default_branch: 'main',
      branches: ['main'],
      worktrees: [{ path: '/repo-wt', branch: 'feature', is_main: false }],
    }),
    createWorktree: async (value) => { calls.push(['create-worktree', value]); return { path: '/new-wt' }; },
    cloneAgent: async (value) => { calls.push(['clone', value]); },
    reincarnateAgent: async (value) => { calls.push(['reincarnate', value]); },
    nestGroup: async (value) => { calls.push(['nest', value]); },
    clonePreset: async (value) => { calls.push(['clone-preset', value]); },
    loadExportHistory: async () => [],
    startExport: async () => ({ id: 7 }),
    watchExport: () => () => {},
    downloadExport: (id) => calls.push(['download-export', id]),
    exportReady: (label) => calls.push(['export-ready', label]),
    exportStillRunning: () => calls.push(['export-running']),
    deleteExport: async () => {},
    clearExports: async () => {},
    ...overrides,
  };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`
    <${ActionDialogApp} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} />
  `, host);
  return { harness, host, state, actions, calls, cleanup: () => mounted.unmount() };
}

test('dirty clone Cancel rejects and accepts through the overlay close transaction', async (t) => {
  let allowDiscard = false;
  let confirmations = 0;
  const mounted = await mountDialogs(
    t,
    'clone-agent',
    { conv: 'abcdefgh-1234', label: 'worker', cwd: '/repo' },
    {},
    async () => {
      confirmations += 1;
      return allowDiscard;
    },
  );
  const { harness, host } = mounted;
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  await harness.input(host.querySelector('#clone-agent-followup'), 'keep this draft');

  host.querySelector('#clone-agent-cancel').click();
  await harness.act(() => Promise.resolve());
  assert.equal(confirmations, 1);
  assert.ok(host.querySelector('#clone-agent-modal'));

  allowDiscard = true;
  host.querySelector('#clone-agent-cancel').click();
  await harness.act(() => Promise.resolve());
  assert.equal(confirmations, 2);
  assert.equal(host.querySelector('#clone-agent-modal'), null);
  await mounted.cleanup();
});

test('clone dialog owns worktree state, dirty discard, and exact submit payload', async (t) => {
  const mounted = await mountDialogs(t, 'clone-agent', { conv: 'abcdefgh-1234', label: 'worker', cwd: '/repo' });
  const { harness, host, calls } = mounted;
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.equal(host.querySelector('#clone-agent-meta').textContent.trim(), 'source: worker  ·  /repo');
  assert.equal(host.querySelector('#clone-agent-error').childNodes.length, 0,
    'the shared :empty rule must collapse an error banner before an error exists');
  const worktree = host.querySelector('#clone-agent-worktree');
  assert.ok([...worktree.options].some((option) => option.value === 'wt:/repo-wt'));

  const followUp = host.querySelector('#clone-agent-followup');
  followUp.value = 'continue\ncarefully';
  followUp.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  const escape = new harness.window.Event('keydown', { bubbles: true });
  Object.defineProperty(escape, 'key', { value: 'Escape' });
  harness.document.dispatchEvent(escape);
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#clone-agent-modal'), 'discard rejection keeps a dirty dialog open');

  [...worktree.options].forEach((option) => option.removeAttribute('selected'));
  worktree.querySelector('option[value="wt:/repo-wt"]').setAttribute('selected', '');
  await harness.act(() => worktree.dispatchEvent(new harness.window.Event('change', { bubbles: true })));
  host.querySelector('#clone-agent-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(calls[0], ['clone', {
    conv: 'abcdefgh-1234', label: 'worker', followUp: 'continue\ncarefully',
    copyConversation: true, cwd: '/repo-wt',
  }]);
  await mounted.cleanup();
});

test('reincarnate dialog gates force mode and preserves plain DOM hooks', async (t) => {
  const mounted = await mountDialogs(t, 'reincarnate-agent', { conv: 'abcdefgh-1234', label: 'worker' });
  const { harness, host, calls } = mounted;
  assert.ok(host.querySelector('#reincarnate-self-fields'));
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.equal(harness.document.activeElement.id, 'reincarnate-agent-focus');
  const force = host.querySelector('input[value="force"]');
  force.checked = true;
  force.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#reincarnate-self-fields'), null);
  assert.match(host.querySelector('#reincarnate-agent-modal').textContent, /<prev>-r-<N>/,
    'HTM help copy must render angle brackets as text rather than encoded entities');
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.equal(harness.document.activeElement.id, 'reincarnate-agent-followup');
  const submit = host.querySelector('#reincarnate-agent-submit');
  assert.equal(submit.disabled, true);
  const followUp = host.querySelector('#reincarnate-agent-followup');
  followUp.value = 'pick up here';
  followUp.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.equal(submit.disabled, false);
  submit.click();
  await harness.act(() => Promise.resolve());
  assert.equal(calls[0][0], 'reincarnate');
  assert.equal(calls[0][1].mode, 'force');
  assert.equal(calls[0][1].followUp, 'pick up here');
  await mounted.cleanup();
});

test('nest dialog uses an explicit parent model and controlled selection', async (t) => {
  const mounted = await mountDialogs(t, 'nest-group', { group: 'child' }, {
    nestModel: () => ({ currentParent: 'alpha', candidates: ['alpha', 'beta'] }),
  });
  const { harness, host, calls } = mounted;
  const parent = host.querySelector('#group-nest-parent');
  assert.ok(parent.querySelector('option[value="alpha"]'));
  [...parent.options].forEach((option) => option.removeAttribute('selected'));
  parent.querySelector('option[value="beta"]').setAttribute('selected', '');
  await harness.act(() => parent.dispatchEvent(new harness.window.Event('change', { bubbles: true })));
  const enter = new harness.window.Event('keydown', { bubbles: true });
  Object.defineProperty(enter, 'key', { value: 'Enter' });
  host.querySelector('#group-nest-modal [role="dialog"]').dispatchEvent(enter);
  await harness.act(() => Promise.resolve());
  assert.deepEqual(calls[0], ['nest', { group: 'child', parent: 'beta' }]);
  await mounted.cleanup();
});

test('action model normalizes handoffs and excludes descendants from nesting', async (t) => {
  const harness = await createPreactHarness(t);
  const { descendantsOf, normaliseFollowUp } = await harness.importDashboardModule('js/action-dialog-actions.js');
  assert.equal(normaliseFollowUp(' one\n\ttwo  three '), 'one two three');
  assert.deepEqual([...descendantsOf('a', [
    { name: 'a' }, { name: 'b', parent: 'a' }, { name: 'c', parent: 'b' }, { name: 'x' },
  ])].sort(), ['a', 'b', 'c']);
});

test('action mutations preserve endpoint payloads, notifications, and refresh boundaries', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createActionDialogState }, { createActionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/action-dialog-state.js'),
    harness.importDashboardModule('js/action-dialog-actions.js'),
  ]);
  const state = createActionDialogState();
  const requests = [];
  const notices = [];
  const refreshes = [];
  const fetchImpl = async (url, options) => {
    requests.push([url, options]);
    const payload = url.includes('/clone') ? { new_conv: '12345678-rest', warning: 'late status' }
      : url.includes('/reincarnate') ? { new_title: 'worker-r-2' } : {};
    return new Response(JSON.stringify(payload), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  const actions = createActionDialogActions({
    state, fetchImpl,
    notify: (...args) => notices.push(args),
    refresh: async (options) => refreshes.push(options || null),
    getSnapshot: () => ({ groups: [{ name: 'parent' }, { name: 'child', parent: 'parent' }] }),
  });

  state.openClone({ conv: 'source', label: 'worker' });
  await actions.cloneAgent({ conv: 'source', label: 'worker', followUp: 'one\ntwo', copyConversation: false, cwd: '/wt' });
  assert.equal(requests[0][0], '/api/agents/source/clone');
  assert.deepEqual(JSON.parse(requests[0][1].body), { follow_up: 'one two', no_copy_conv: true, cwd: '/wt' });
  assert.deepEqual(notices[0], ['cloned worker → 12345678 (warning: late status)', true]);
  assert.equal(state.dialog.value, null);

  state.openReincarnate({ conv: 'source', label: 'worker' });
  await actions.reincarnateAgent({ conv: 'source', label: 'worker', mode: 'force', focusHint: '', followUp: 'resume now' });
  assert.equal(requests[1][0], '/api/agents/source/reincarnate');
  assert.deepEqual(JSON.parse(requests[1][1].body), { mode: 'force', follow_up: 'resume now' });
  assert.deepEqual(notices[1], ['reincarnated worker → worker-r-2']);

  state.openNest({ group: 'child' });
  await actions.nestGroup({ group: 'child', parent: '' });
  assert.equal(requests[2][0], '/api/groups/child/parent');
  assert.deepEqual(JSON.parse(requests[2][1].body), { parent: '' });
  assert.deepEqual(refreshes, [null, null, null]);
});

test('preset clone dialog submits controlled names and surfaces create errors', async (t) => {
  const source = { name: 'writer', aliases: ['scribe'], model: 'opus' };
  const mounted = await mountDialogs(t, 'preset-clone', {
    presetKind: 'profile', kindWizard: 'pattern', source, create: async () => {},
  });
  const { harness, host, calls } = mounted;
  await harness.act(() => Promise.resolve());
  assert.equal(harness.document.activeElement.id, 'clone-modal-name');
  const input = host.querySelector('#clone-modal-name');
  input.value = 'writer-copy-2';
  input.dispatchEvent(new harness.window.Event('input', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  host.querySelector('#clone-modal-submit').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(calls[0], ['clone-preset', {
    source, create: mounted.state.dialog.value.create, name: 'writer-copy-2',
  }]);
  await mounted.cleanup();

  const failed = await mountDialogs(t, 'preset-clone', {
    presetKind: 'role', kindWizard: 'class', source, create: async () => {},
  }, { clonePreset: async () => { throw new Error('name already exists'); } });
  failed.host.querySelector('#clone-modal-submit').click();
  await failed.harness.act(() => Promise.resolve());
  assert.equal(failed.host.querySelector('#clone-modal-error').textContent, 'name already exists');
  assert.ok(failed.host.querySelector('#clone-modal'), 'failed clone stays open for correction/retry');
  await failed.cleanup();
});

test('terminal-directory state resolves every result and cancellation path', async (t) => {
  const harness = await createPreactHarness(t);
  const { createActionDialogState } = await harness.importDashboardModule('js/action-dialog-state.js');
  const state = createActionDialogState();

  const worktree = state.openTerminalDirectory({ label: 'worker' });
  assert.equal(state.dialog.value.kind, 'terminal-directory');
  state.finishChoice('worktree');
  assert.equal(await worktree, 'worktree');

  const canceled = state.openTerminalDirectory({ label: 'worker' });
  state.close();
  assert.equal(await canceled, null);

  const unmounted = state.openTerminalDirectory({ label: 'worker' });
  state.dispose();
  assert.equal(await unmounted, null);
});

test('terminal-directory component publishes choice and cancel without DOM listeners', async (t) => {
  const chosen = await mountDialogs(t, 'terminal-directory', { label: 'worker' });
  chosen.host.querySelector('#term-current').click();
  await chosen.harness.act(() => Promise.resolve());
  assert.equal(chosen.state.dialog.value, null);
  await chosen.cleanup();

  const canceled = await mountDialogs(t, 'terminal-directory', { label: 'worker' });
  canceled.host.querySelector('#term-cancel').click();
  await canceled.harness.act(() => Promise.resolve());
  assert.equal(canceled.state.dialog.value, null);
  await canceled.cleanup();
});

test('export dialog owns submit/error/retry UI and cancels its watcher on close', async (t) => {
  let watcherCleanup = 0;
  let callbacks;
  const mounted = await mountDialogs(t, 'agent-export', { conv: 'abcdefgh-rest', label: 'worker' }, {
    watchExport: (_id, value) => { callbacks = value; return () => { watcherCleanup++; }; },
  });
  const { harness, host, calls } = mounted;
  await harness.act(() => Promise.resolve());
  host.querySelector('#export-agent-submit').click();
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#export-agent-status'));
  callbacks.onSlow();
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#export-agent-status-note').textContent, /Still working/);
  assert.match(host.querySelector('#export-agent-status-note').textContent, /Still inscribing/);
  callbacks.onFailed({});
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#export-agent-status-note').textContent, /agent did not deliver/);
  assert.match(host.querySelector('#export-agent-status-note').textContent, /familiar did not deliver/);
  host.querySelector('#export-agent-retry').click();
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#export-agent-form'));
  assert.equal(watcherCleanup, 1, 'leaving the working phase cancels the poll watcher');

  host.querySelector('#export-agent-submit').click();
  await harness.act(() => Promise.resolve());
  host.querySelector('#export-agent-cancel').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(calls.at(-1), ['export-running']);
  assert.equal(watcherCleanup, 2, 'closing an active export cancels the watcher');
  await mounted.cleanup();
});

test('export polling owns timers and aborts an in-flight request on cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createActionDialogState }, { createActionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/action-dialog-state.js'),
    harness.importDashboardModule('js/action-dialog-actions.js'),
  ]);
  const timers = [];
  const cleared = [];
  let aborted = false;
  const actions = createActionDialogActions({
    state: createActionDialogState(),
    refresh: async () => {}, notify: () => {},
    setTimer(fn) { timers.push(fn); return timers.length; },
    clearTimer(id) { cleared.push(id); },
    fetchImpl: (_url, options) => new Promise((_resolve, reject) => {
      options.signal.addEventListener('abort', () => {
        aborted = true;
        reject(Object.assign(new Error('aborted'), { name: 'AbortError' }));
      });
    }),
  });
  const cancel = actions.watchExport(9, {
    onStatus() {}, onReady() {}, onFailed() {}, onSlow() {},
  });
  assert.equal(timers.length, 1);
  const pending = timers[0]();
  cancel();
  await pending;
  assert.equal(aborted, true, 'cleanup aborts the request currently in flight');
  assert.ok(cleared.length >= 1, 'cleanup clears the scheduled poll timer');
});

test('export request errors prefer friendly JSON error fields and retain plain text', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createActionDialogState }, { createActionDialogActions }] = await Promise.all([
    harness.importDashboardModule('js/action-dialog-state.js'),
    harness.importDashboardModule('js/action-dialog-actions.js'),
  ]);
  const responses = [
    new Response(JSON.stringify({ error: 'friendly collision' }), { status: 409 }),
    new Response('plain failure', { status: 400 }),
  ];
  const actions = createActionDialogActions({
    state: createActionDialogState(), refresh: async () => {}, notify: () => {},
    fetchImpl: async () => responses.shift(),
  });
  await assert.rejects(
    actions.startExport({ conv: 'agent', preset: 'summary' }),
    /friendly collision/,
  );
  await assert.rejects(
    actions.startExport({ conv: 'agent', preset: 'summary' }),
    /plain failure/,
  );
});
