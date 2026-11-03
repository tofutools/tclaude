import test from 'node:test';
import assert from 'node:assert/strict';
import { assertSameNode } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

test('component harness covers events, keyed focus, controlled state, cleanup, and queries', async (t) => {
  const harness = await createPreactHarness(t);
  const { useEffect, useState } = harness.hooks;
  let pings = 0;

  assert.equal(globalThis.HTMLDetailsElement, harness.window.HTMLDetailsElement);
  assert.ok(harness.document.createElement('details') instanceof HTMLDetailsElement);
  assert.ok(!(harness.document.createElement('div') instanceof HTMLDetailsElement));

  function Fixture({ items }) {
    const [name, setName] = useState('');
    useEffect(() => {
      const onPing = () => { pings += 1; };
      harness.window.addEventListener('fixture-ping', onPing);
      return () => harness.window.removeEventListener('fixture-ping', onPing);
    }, []);
    return harness.html`
      <section aria-label="Component harness fixture">
        <label for="fixture-name">Name</label>
        <input
          id="fixture-name"
          value=${name}
          onInput=${(event) => setName(event.currentTarget.value)}
        />
        <output role="status" aria-label="Current name">${name}</output>
        <ul>
          ${items.map((item) => harness.html`
            <li key=${item.id}>
              <button data-id=${item.id}>${item.label}</button>
            </li>
          `)}
        </ul>
      </section>
    `;
  }

  const initial = [
    { id: 'alpha', label: 'Alpha' },
    { id: 'beta', label: 'Beta' },
  ];
  const view = await harness.mount(harness.html`<${Fixture} items=${initial} />`);

  const nameInput = harness.getByLabelText(view.container, 'Name');
  await harness.input(nameInput, 'Ada');
  assert.equal(nameInput.value, 'Ada');
  assert.equal(harness.getByRole(view.container, 'status', { name: 'Current name' }).textContent, 'Ada');

  const beta = harness.getByRole(view.container, 'button', { name: 'Beta' });
  beta.focus();
  assertSameNode(harness.document.activeElement, beta);
  let betaBlurred = 0;
  beta.addEventListener('blur', () => { betaBlurred += 1; });
  harness.getByRole(view.container, 'button', { name: 'Alpha' }).focus();
  assert.equal(betaBlurred, 1, 'moving focus blurs the previous element');
  beta.focus();
  await view.rerender(harness.html`<${Fixture} items=${[initial[1], initial[0]]} />`);
  assertSameNode(harness.getByRole(view.container, 'button', { name: 'Beta' }), beta);
  assertSameNode(harness.document.activeElement, beta);

  harness.window.dispatchEvent(new harness.window.Event('fixture-ping'));
  assert.equal(pings, 1);
  await view.unmount();
  assert.equal(view.container.isConnected, false, 'harness-owned root was removed');
  assertSameNode(harness.document.activeElement, harness.document.body, 'detached focus falls back to body');
  harness.window.dispatchEvent(new harness.window.Event('fixture-ping'));
  assert.equal(pings, 1, 'effect listener was removed on unmount');

  const callerRoot = harness.document.body.appendChild(harness.document.createElement('div'));
  const callerView = await harness.mount(harness.html`<span>caller owned</span>`, callerRoot);
  await callerView.unmount();
  assert.equal(callerRoot.isConnected, true, 'caller-owned root is preserved');
});

// Every harness of a test file shares one rewritten copy of the dashboard
// tree through hard links. That sharing must stay invisible: each harness
// still gets its own module instances, and a module one harness replaces
// never reaches another harness or the shared tree.
test('harnesses get their own module instances, and a replaced module stays in its harness', async (t) => {
  // Each harness's directory goes at the end of its own subtest, so import
  // inside it; the module objects outlive the files.
  const load = async (st) => {
    const harness = await createPreactHarness(st);
    return { harness, model: await harness.importDashboardModule('js/access-model.js') };
  };
  let first;
  let second;
  await t.test('first', async (st) => { first = await load(st); });
  await t.test('second', async (st) => { second = await load(st); });
  const a = first.model;
  assert.notEqual(a, second.model, 'two harnesses must not share a module instance');
  assert.notEqual(first.harness.preact, second.harness.preact, 'nor share Preact');

  await t.test('replace', async (st) => {
    const harness = await createPreactHarness(st);
    await harness.replaceDashboardModule('js/access-model.js', 'export const ACCESS_SUBTABS = ["replaced"];\n');
    const replaced = await harness.importDashboardModule('js/access-model.js');
    assert.deepEqual(replaced.ACCESS_SUBTABS, ['replaced']);
  });
  await t.test('after', async (st) => {
    const harness = await createPreactHarness(st);
    const fresh = await harness.importDashboardModule('js/access-model.js');
    assert.deepEqual(fresh.ACCESS_SUBTABS, a.ACCESS_SUBTABS, 'a later harness sees the original module');
    assert.notDeepEqual(fresh.ACCESS_SUBTABS, ['replaced']);
  });
});
