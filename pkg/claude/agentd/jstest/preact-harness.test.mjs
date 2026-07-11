import test from 'node:test';
import assert from 'node:assert/strict';
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
  assert.equal(harness.document.activeElement, beta);
  let betaBlurred = 0;
  beta.addEventListener('blur', () => { betaBlurred += 1; });
  harness.getByRole(view.container, 'button', { name: 'Alpha' }).focus();
  assert.equal(betaBlurred, 1, 'moving focus blurs the previous element');
  beta.focus();
  await view.rerender(harness.html`<${Fixture} items=${[initial[1], initial[0]]} />`);
  assert.equal(harness.getByRole(view.container, 'button', { name: 'Beta' }), beta);
  assert.equal(harness.document.activeElement, beta);

  harness.window.dispatchEvent(new harness.window.Event('fixture-ping'));
  assert.equal(pings, 1);
  await view.unmount();
  assert.equal(view.container.isConnected, false, 'harness-owned root was removed');
  assert.equal(harness.document.activeElement, harness.document.body, 'detached focus falls back to body');
  harness.window.dispatchEvent(new harness.window.Event('fixture-ping'));
  assert.equal(pings, 1, 'effect listener was removed on unmount');

  const callerRoot = harness.document.body.appendChild(harness.document.createElement('div'));
  const callerView = await harness.mount(harness.html`<span>caller owned</span>`, callerRoot);
  await callerView.unmount();
  assert.equal(callerRoot.isConnected, true, 'caller-owned root is preserved');
});
