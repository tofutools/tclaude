// js/phone-keyboard.js: the on-screen keyboard's height as --phone-keyboard.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function fakeWindow({ innerHeight = 844, height = 844, offsetTop = 0, scale = 1 } = {}) {
  const listeners = {};
  const view = {
    height, offsetTop, scale,
    addEventListener: (type, fn) => { (listeners[type] ||= new Set()).add(fn); },
    removeEventListener: (type, fn) => listeners[type]?.delete(fn),
  };
  const props = new Map();
  const root = { style: { setProperty: (k, v) => props.set(k, v), removeProperty: (k) => props.delete(k) } };
  return { win: { innerHeight, visualViewport: view }, view, root, props, fire: (type) => listeners[type]?.forEach((fn) => fn()) };
}

test('keyboardInset is what the keyboard covers, not toolbar wiggle or zoom', async (t) => {
  const harness = await createPreactHarness(t);
  const { keyboardInset } = await harness.importDashboardModule('js/phone-keyboard.js');
  assert.equal(keyboardInset(fakeWindow().win), 0);
  assert.equal(keyboardInset(fakeWindow({ height: 508 }).win), 336);
  assert.equal(keyboardInset(fakeWindow({ height: 800 }).win), 0, 'a toolbar is not a keyboard');
  assert.equal(keyboardInset(fakeWindow({ height: 422, scale: 2 }).win), 0, 'pinch zoom is not a keyboard');
  assert.equal(keyboardInset({}), 0, 'no visualViewport');
});

test('installKeyboardInset keeps the variable current and removes it on uninstall', async (t) => {
  const harness = await createPreactHarness(t);
  const { installKeyboardInset } = await harness.importDashboardModule('js/phone-keyboard.js');
  const fake = fakeWindow();
  const uninstall = installKeyboardInset(fake.win, fake.root);
  assert.equal(fake.props.get('--phone-keyboard'), '0px');
  fake.view.height = 500;
  fake.fire('resize');
  assert.equal(fake.props.get('--phone-keyboard'), '344px');
  uninstall();
  assert.equal(fake.props.has('--phone-keyboard'), false);
});
