import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent, assertSameNode } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

function deferred() {
  let resolve;
  const promise = new Promise((res) => { resolve = res; });
  return { promise, resolve };
}

async function flush(harness, turns = 6) {
  await harness.act(async () => {
    for (let turn = 0; turn < turns; turn += 1) await Promise.resolve();
  });
}

test('guarded overlay Cancel shares busy, dirty-confirmation, stack, and focus contracts', async (t) => {
  const harness = await createPreactHarness(t);
  const { ManagementOverlay, useGuardedOverlayClose } =
    await harness.importDashboardModule('js/management-overlay.js');
  const { useState } = harness.hooks;
  const firstConfirmation = deferred();
  let confirmResult = firstConfirmation.promise;
  let confirmations = 0;
  let closes = 0;

  function Dialog({ blocked, close }) {
    const { requestClose, registerClose } = useGuardedOverlayClose();
    return harness.html`<${ManagementOverlay}
      id="guarded-overlay-test"
      labelledby="guarded-overlay-title"
      onClose=${close}
      dirty=${true}
      blocked=${blocked}
      confirmDiscard=${() => {
        confirmations += 1;
        return confirmResult;
      }}
      registerClose=${registerClose}
    >
      <h3 id="guarded-overlay-title">Guarded draft</h3>
      <input id="guarded-overlay-input" autofocus />
      <button id="guarded-overlay-cancel" type="button" disabled=${blocked}
        onClick=${() => { void requestClose(); }}>Cancel</button>
    </${ManagementOverlay}>`;
  }

  function App({ blocked }) {
    const [open, setOpen] = useState(true);
    return open ? harness.html`<${Dialog} blocked=${blocked} close=${() => {
      closes += 1;
      setOpen(false);
    }} />` : null;
  }

  const invoker = harness.document.body.appendChild(harness.document.createElement('button'));
  invoker.id = 'guarded-overlay-invoker';
  invoker.focus();
  const mounted = await harness.mount(harness.html`<${App} blocked=${true} />`);
  const overlay = () => harness.document.querySelector('#guarded-overlay-test');
  const cancel = () => harness.document.querySelector('#guarded-overlay-cancel');

  cancel().click();
  harness.fireEvent(harness.document, 'keydown', { key: 'Escape' });
  const blockedBackdrop = harness.fireEvent(overlay(), 'mousedown');
  await flush(harness);
  assert.equal(blockedBackdrop.defaultPrevented, false,
    'a blocked backdrop retains its native pointer default');
  assert.equal(confirmations, 0, 'busy blocks every close entry point');
  assert.equal(closes, 0);

  await mounted.rerender(harness.html`<${App} blocked=${false} />`);
  cancel().click();
  await Promise.resolve();
  assert.equal(confirmations, 1);
  assert.equal(overlay().hasAttribute('inert'), true,
    'the draft is suspended while the stacked confirmation owns interaction');
  assert.equal(overlay().getAttribute('aria-hidden'), 'true');
  assert.equal(overlay().querySelector('[role="dialog"]').getAttribute('aria-modal'), 'false');

  cancel().click();
  harness.fireEvent(harness.document, 'keydown', { key: 'Escape' });
  assert.equal(confirmations, 1, 'a pending confirmation serializes repeated dismissal');
  firstConfirmation.resolve(false);
  await flush(harness);
  assert.ok(overlay(), 'rejected confirmation preserves the draft');
  assert.equal(overlay().hasAttribute('inert'), false);
  assert.equal(overlay().querySelector('[role="dialog"]').getAttribute('aria-modal'), 'true');

  confirmResult = Promise.resolve(true);
  cancel().click();
  await flush(harness);
  assert.equal(confirmations, 2);
  assert.equal(closes, 1);
  assertAbsent(overlay());
  assertSameNode(harness.document.activeElement, invoker,
    'accepted close restores focus to the invoker');

  await mounted.unmount();
  invoker.remove();
});

test('one-button dialogs accept Enter shortcuts without duplicating handled events or submitting drafts', async (t) => {
  const harness = await createPreactHarness(t);
  const { ManagementOverlay } = await harness.importDashboardModule('js/management-overlay.js');
  let clicks = 0;
  const mounted = await harness.mount(harness.html`<${ManagementOverlay}
    id="single-button-test" onClose=${() => {}} onSubmitHotkey=${() => { clicks += 1; }}
  >
    <textarea></textarea>
    <button onClick=${() => { clicks += 1; }}>Done</button>
  </${ManagementOverlay}>`);
  const button = harness.document.querySelector('#single-button-test button');
  const field = harness.document.querySelector('#single-button-test textarea');
  harness.fireEvent(field, 'keydown', { key: 'Enter' });
  harness.fireEvent(button, 'keydown', { key: 'Enter', isComposing: true });
  harness.fireEvent(button, 'keydown', { key: 'Enter', keyCode: 229 });
  assert.equal(clicks, 0);
  harness.fireEvent(button, 'keydown', { key: 'Enter', ctrlKey: true });
  assert.equal(clicks, 1, 'the overlay hotkey must not also click the button');
  harness.fireEvent(harness.document.body, 'keydown', { key: 'Enter' });
  assert.equal(clicks, 2);
  button.disabled = true;
  harness.fireEvent(harness.document.body, 'keydown', { key: 'Enter' });
  assert.equal(clicks, 2, 'disabled sole buttons stay inactive');
  button.disabled = false;
  const second = button.parentElement.appendChild(harness.document.createElement('button'));
  second.disabled = true;
  harness.fireEvent(harness.document.body, 'keydown', { key: 'Enter' });
  assert.equal(clicks, 2, 'a disabled second button still counts as a choice');
  second.hidden = true;
  harness.fireEvent(harness.document.body, 'keydown', { key: 'Enter', metaKey: true });
  assert.equal(clicks, 3, 'hidden buttons do not count');
  await mounted.unmount();
});
