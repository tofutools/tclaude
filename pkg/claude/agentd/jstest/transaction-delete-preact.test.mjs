import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

async function openDelete(t, options = {}) {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, island] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-island.js'),
  ]);
  const state = createTransactionDialogState();
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const opener = harness.document.body.appendChild(harness.document.createElement('button'));
  opener.id = 'delete-agent-opener';
  opener.focus();
  const actions = {
    close: state.close,
    loadAgentWorktree: async () => ({ kind: 'none', path: '', removable: false }),
    deleteAgent: async () => {},
    ...options.actions,
  };
  const mounted = await harness.mount(harness.html`
    <${island.TransactionDialogApp}
      state=${state}
      actions=${actions}
      confirmDiscard=${async () => true}
    />
  `, host);
  let pending;
  await harness.act(() => {
    pending = state.open({
      kind: 'delete-agent',
      agent: options.agent || 'agt_stable-delete',
      label: options.label || 'Delete target',
    });
  });
  await harness.act(() => Promise.resolve());
  return { harness, state, host, opener, actions, mounted, pending };
}

function escape(harness) {
  const event = new harness.window.Event('keydown', { bubbles: true });
  Object.defineProperty(event, 'key', { value: 'Escape' });
  harness.document.dispatchEvent(event);
}

test('delete before probe completion freezes the safe no-worktree choice and aborts discovery', async (t) => {
  const probe = deferred();
  const mutation = deferred();
  let probeSignal = null;
  let submitted = null;
  const mounted = await openDelete(t, {
    actions: {
      loadAgentWorktree: (_agent, { signal }) => {
        probeSignal = signal;
        return probe.promise;
      },
      deleteAgent: (request) => {
        submitted = request;
        return mutation.promise;
      },
    },
  });
  const { harness, host } = mounted;
  assert.equal(host.querySelector('#delete-agent-wt-row'), null);
  assert.equal(harness.document.activeElement.id, 'delete-agent-ok');
  host.querySelector('#delete-agent-ok').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(submitted, {
    agent: 'agt_stable-delete', label: 'Delete target', deleteWorktree: false,
  });
  assert.ok(Object.isFrozen(submitted));
  assert.equal(probeSignal.aborted, true);
  assert.equal(host.querySelector('#delete-agent-ok').disabled, true);
  assert.equal(host.querySelector('#delete-agent-cancel').disabled, true);

  escape(harness);
  const overlay = host.querySelector('#delete-agent-modal');
  overlay.dispatchEvent(new harness.window.Event('mousedown', { bubbles: true }));
  overlay.dispatchEvent(new harness.window.Event('click', { bubbles: true }));
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#delete-agent-modal'), 'busy blocks Escape and backdrop');

  mutation.reject(new Error('delete failed'));
  await harness.act(() => mutation.promise.catch(() => {}));
  assert.equal(host.querySelector('[role="alert"]').textContent, 'delete failed');
  host.querySelector('#delete-agent-cancel').click();
  await harness.act(() => Promise.resolve());
  await mounted.pending;
  await mounted.mounted.unmount();
});

test('delete worktree probe renders absent, removable, main, and shared states exactly', async (t) => {
  for (const row of [
    {
      name: 'absent', worktree: { kind: 'none', path: '', removable: false },
      visible: false, checked: false, disabled: false, copy: '',
    },
    {
      name: 'removable',
      worktree: {
        kind: 'linked', path: '/repo/wt', branch: 'feature', shared: false, removable: true,
      },
      visible: true, checked: true, disabled: false,
      copy: 'Also delete the git worktree /repo/wt · feature — directory removed, branch kept',
    },
    {
      name: 'main',
      worktree: { kind: 'main', path: '/repo', branch: 'main', shared: false, removable: false },
      visible: true, checked: false, disabled: true,
      copy: 'Git worktree kept /repo · main — the repo’s main worktree, never removed',
    },
    {
      name: 'shared',
      worktree: {
        kind: 'linked', path: '/repo/shared', branch: 'shared', shared: true, removable: false,
      },
      visible: true, checked: false, disabled: true,
      copy: 'Git worktree kept /repo/shared · shared — shared with another agentshared with another familiar',
    },
  ]) {
    await t.test(row.name, async (t) => {
      const mounted = await openDelete(t, {
        actions: { loadAgentWorktree: async () => row.worktree },
      });
      await mounted.harness.act(() => Promise.resolve());
      const worktreeRow = mounted.host.querySelector('#delete-agent-wt-row');
      assert.equal(!!worktreeRow, row.visible);
      if (worktreeRow) {
        const checkbox = worktreeRow.querySelector('#delete-agent-wt');
        assert.equal(checkbox.hasAttribute('checked'), row.checked);
        assert.equal(checkbox.disabled, row.disabled);
        assert.equal(worktreeRow.textContent.replace(/\s+/g, ' ').trim(), row.copy);
        if (row.name === 'shared') {
          assert.equal(worktreeRow.querySelector('.theme-copy-regular').textContent,
            'shared with another agent');
          assert.equal(worktreeRow.querySelector('.theme-copy-wizard').textContent,
            'shared with another familiar');
        }
      }
      mounted.state.close();
      await mounted.harness.act(() => Promise.resolve());
      await mounted.pending;
      await mounted.mounted.unmount();
    });
  }
});

test('delete probe failure stays inline and explicit retry starts a fresh generation', async (t) => {
  const second = deferred();
  const signals = [];
  let probes = 0;
  const mounted = await openDelete(t, {
    actions: {
      loadAgentWorktree: (_agent, { signal }) => {
        signals.push(signal);
        probes += 1;
        if (probes === 1) return Promise.reject(new Error('probe unavailable'));
        return second.promise;
      },
    },
  });
  const { harness, host } = mounted;
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('[role="alert"]').textContent,
    'Worktree check failed: probe unavailable');
  assert.ok(host.querySelector('#delete-agent-wt-retry'));
  host.querySelector('#delete-agent-wt-retry').click();
  await harness.act(() => Promise.resolve());
  assert.equal(probes, 2);
  assert.equal(signals[0].aborted, true);
  assert.equal(host.querySelector('#delete-agent-error').textContent, '');
  second.resolve({
    kind: 'linked', path: '/repo/current', branch: 'current', shared: false, removable: true,
  });
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#delete-agent-wt-row').textContent, /\/repo\/current/);
  mounted.state.close();
  await harness.act(() => Promise.resolve());
  await mounted.pending;
  await mounted.mounted.unmount();
});

test('delete closes over stale probe generations and ignores their late responses', async (t) => {
  const first = deferred();
  const second = deferred();
  const signals = [];
  let calls = 0;
  const mounted = await openDelete(t, {
    actions: {
      loadAgentWorktree: (_agent, { signal }) => {
        signals.push(signal);
        calls += 1;
        return calls === 1 ? first.promise : second.promise;
      },
    },
  });
  mounted.state.close();
  await mounted.harness.act(() => Promise.resolve());
  assert.equal(signals[0].aborted, true);

  let reopened;
  await mounted.harness.act(() => {
    reopened = mounted.state.open({
      kind: 'delete-agent', agent: 'agt_second-delete', label: 'Second delete target',
    });
  });
  await mounted.harness.act(() => Promise.resolve());
  assert.equal(calls, 2);
  first.resolve({
    kind: 'linked', path: '/repo/stale', branch: 'stale', shared: false, removable: true,
  });
  await mounted.harness.act(() => Promise.resolve());
  assert.equal(mounted.host.querySelector('#delete-agent-wt-row'), null);
  second.resolve({
    kind: 'linked', path: '/repo/current', branch: 'current', shared: false, removable: true,
  });
  await mounted.harness.act(() => Promise.resolve());
  assert.match(mounted.host.querySelector('#delete-agent-wt-row').textContent, /\/repo\/current/);
  mounted.state.close();
  await mounted.harness.act(() => Promise.resolve());
  await reopened;
  await mounted.mounted.unmount();
});

test('delete mutation failure retries the same frozen worktree choice', async (t) => {
  const first = deferred();
  const second = deferred();
  const requests = [];
  const mounted = await openDelete(t, {
    actions: {
      loadAgentWorktree: async () => ({
        kind: 'linked', path: '/repo/wt', branch: 'feature', shared: false, removable: true,
      }),
      deleteAgent: (request) => {
        requests.push(request);
        return requests.length === 1 ? first.promise : second.promise;
      },
    },
  });
  const { harness, host } = mounted;
  await harness.act(() => Promise.resolve());
  host.querySelector('#delete-agent-ok').click();
  host.querySelector('#delete-agent-ok').click();
  await harness.act(() => Promise.resolve());
  assert.equal(requests.length, 1, 'the frame lock rejects same-render duplicate submits');
  assert.equal(requests[0].deleteWorktree, true);
  assert.equal(requests[0].expectedWorktree, '/repo/wt');
  assert.ok(Object.isFrozen(requests[0]));
  first.reject(new Error('server refused delete'));
  await harness.act(() => first.promise.catch(() => {}));
  assert.equal(host.querySelector('[role="alert"]').textContent, 'server refused delete');
  assert.equal(host.querySelector('#delete-agent-ok').textContent, 'Retry delete');
  assert.equal(host.querySelector('#delete-agent-wt').disabled, true);
  host.querySelector('#delete-agent-ok').click();
  await harness.act(() => Promise.resolve());
  assert.equal(requests[1], requests[0]);
  second.reject(new Error('server still refused delete'));
  await harness.act(() => second.promise.catch(() => {}));
  mounted.state.close();
  await harness.act(() => Promise.resolve());
  await mounted.pending;
  await mounted.mounted.unmount();
});

test('delete yields topmost Escape, guards backdrop drags, and restores its opener', async (t) => {
  const mounted = await openDelete(t);
  const { harness, host, opener } = mounted;
  const higher = harness.document.body.appendChild(harness.document.createElement('div'));
  higher.className = 'modal-overlay show';
  higher.style.zIndex = '999';
  escape(harness);
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#delete-agent-modal'));
  higher.remove();

  host.querySelector('#delete-agent-modal').dispatchEvent(
    new harness.window.Event('click', { bubbles: true }),
  );
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#delete-agent-modal'));
  escape(harness);
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#delete-agent-modal'), null);
  assert.equal(harness.document.activeElement, opener);
  assert.equal(await mounted.pending, null);
  await mounted.mounted.unmount();
});
