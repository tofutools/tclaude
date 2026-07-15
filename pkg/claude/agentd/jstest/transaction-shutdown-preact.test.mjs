import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

async function openShutdown(t, action) {
  const harness = await createPreactHarness(t);
  const [{ createTransactionDialogState }, island] = await Promise.all([
    harness.importDashboardModule('js/transaction-dialog-state.js'),
    harness.importDashboardModule('js/transaction-dialog-island.js'),
  ]);
  const state = createTransactionDialogState();
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const opener = harness.document.body.appendChild(harness.document.createElement('button'));
  opener.id = 'shutdown-opener';
  opener.focus();
  const actions = {
    close: state.close,
    shutdownAgent: action,
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
      kind: 'shutdown-agent', agent: 'agt_stable-target', label: 'Stable target',
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

test('shutdown renderer preserves distinct actions, initial focus, and exact focus return', async (t) => {
  const mounted = await openShutdown(t, async () => {});
  const { harness, host, opener } = mounted;
  assert.equal(host.querySelector('#shutdown-title').textContent, 'Shut down agent?');
  assert.equal(host.querySelector('#shutdown-meta').textContent, 'Stable target');
  assert.match(host.querySelector('#shutdown-modal').textContent,
    /Soft exit injects \/exit into tmux pane/);
  assert.equal(host.querySelector('#shutdown-soft').textContent, 'Soft exit');
  assert.equal(host.querySelector('#shutdown-force').textContent, 'Force kill');
  assert.equal(host.querySelector('#shutdown-force').classList.contains('confirm-danger'), true);
  assert.equal(host.querySelector('#shutdown-soft').classList.contains('confirm-danger'), false);
  assert.equal(harness.document.activeElement.id, 'shutdown-soft');

  // A release on the backdrop whose press began in the dialog is ignored.
  host.querySelector('#shutdown-modal').dispatchEvent(
    new harness.window.Event('click', { bubbles: true }),
  );
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#shutdown-modal'));

  host.querySelector('#shutdown-modal').dispatchEvent(
    new harness.window.Event('mousedown', { bubbles: true }),
  );
  host.querySelector('#shutdown-modal').dispatchEvent(
    new harness.window.Event('click', { bubbles: true }),
  );
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#shutdown-modal'), null);
  assert.equal(harness.document.activeElement, opener);
  assert.equal(await mounted.pending, null);
  await mounted.mounted.unmount();
});

test('shutdown actions share one busy lock and retry the same frozen soft choice', async (t) => {
  const first = deferred();
  const second = deferred();
  const requests = [];
  const mounted = await openShutdown(t, (request) => {
    requests.push(request);
    return requests.length === 1 ? first.promise : second.promise;
  });
  const { harness, host } = mounted;
  const soft = host.querySelector('#shutdown-soft');
  const force = host.querySelector('#shutdown-force');

  // Two distinct buttons still funnel through one synchronous transaction
  // lock, before Preact has time to publish busy=true.
  soft.click();
  force.click();
  await harness.act(() => Promise.resolve());
  assert.equal(requests.length, 1);
  assert.deepEqual(requests[0], {
    agent: 'agt_stable-target', label: 'Stable target', force: false,
  });
  assert.ok(Object.isFrozen(requests[0]));
  assert.equal(host.querySelector('#shutdown-soft').disabled, true);
  assert.equal(host.querySelector('#shutdown-force').disabled, true);
  assert.equal(host.querySelector('#shutdown-cancel').disabled, true);
  assert.equal(host.querySelector('#shutdown-soft').getAttribute('aria-busy'), 'true');

  escape(harness);
  const overlay = host.querySelector('#shutdown-modal');
  overlay.dispatchEvent(new harness.window.Event('mousedown', { bubbles: true }));
  overlay.dispatchEvent(new harness.window.Event('click', { bubbles: true }));
  host.querySelector('#shutdown-cancel').click();
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#shutdown-modal'), 'busy blocks Escape, backdrop, and Cancel');

  first.reject(new Error('soft stop failed'));
  await harness.act(() => first.promise.catch(() => {}));
  assert.equal(host.querySelector('[role="alert"]').textContent, 'soft stop failed');
  assert.equal(host.querySelector('#shutdown-soft').textContent, 'Retry soft exit');
  assert.equal(host.querySelector('#shutdown-soft').disabled, false);
  assert.equal(host.querySelector('#shutdown-force').disabled, true,
    'the failed soft choice cannot be retargeted to force');
  host.querySelector('#shutdown-force').click();
  assert.equal(requests.length, 1);

  host.querySelector('#shutdown-soft').click();
  await harness.act(() => Promise.resolve());
  assert.equal(requests.length, 2);
  assert.equal(requests[1], requests[0], 'retry reuses the exact frozen request');
  second.reject(new Error('soft stop still failed'));
  await harness.act(() => second.promise.catch(() => {}));
  host.querySelector('#shutdown-cancel').click();
  await harness.act(() => Promise.resolve());
  await mounted.pending;
  await mounted.mounted.unmount();
});

test('shutdown force path freezes force=true and yields Escape to a higher overlay', async (t) => {
  const request = deferred();
  let submitted = null;
  const mounted = await openShutdown(t, (next) => {
    submitted = next;
    return request.promise;
  });
  const { harness, host, opener } = mounted;
  const higher = harness.document.body.appendChild(harness.document.createElement('div'));
  higher.className = 'modal-overlay show';
  higher.style.zIndex = '999';
  escape(harness);
  await harness.act(() => Promise.resolve());
  assert.ok(host.querySelector('#shutdown-modal'), 'a lower dialog ignores Escape');
  higher.remove();

  host.querySelector('#shutdown-force').click();
  await harness.act(() => Promise.resolve());
  assert.deepEqual(submitted, {
    agent: 'agt_stable-target', label: 'Stable target', force: true,
  });
  assert.ok(Object.isFrozen(submitted));
  assert.equal(host.querySelector('#shutdown-force').getAttribute('aria-busy'), 'true');
  assert.equal(host.querySelector('#shutdown-soft').hasAttribute('aria-busy'), false);
  request.reject(new Error('force failed'));
  await harness.act(() => request.promise.catch(() => {}));
  escape(harness);
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#shutdown-modal'), null);
  assert.equal(harness.document.activeElement, opener);
  assert.equal(await mounted.pending, null);
  await mounted.mounted.unmount();
});
