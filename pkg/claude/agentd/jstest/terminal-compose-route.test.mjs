import test from 'node:test';
import assert from 'node:assert/strict';
import { terminalComposeShortcutAction } from '../dashboard/js/terminal-compose-route.js';

const chord = (overrides = {}) => ({
  type: 'keydown', key: 'm', code: 'KeyM', ctrlKey: true,
  metaKey: false, altKey: false, shiftKey: false, ...overrides,
});

test('active eligible terminal opens the operator composer', () => {
  assert.equal(terminalComposeShortcutAction(chord(), {
    tabActive: true, eligiblePane: true,
  }), 'open');
});

test('held compose chord is consumed while its composer is already open', () => {
  assert.equal(terminalComposeShortcutAction(chord({ repeat: true }), {
    tabActive: true, eligiblePane: true, operatorModalOpen: true,
    blockingOverlayOpen: true,
  }), 'consume');
});

test('other overlays block opening without claiming their shortcut', () => {
  assert.equal(terminalComposeShortcutAction(chord(), {
    tabActive: true, eligiblePane: true, blockingOverlayOpen: true,
  }), 'ignore');
});

test('other tabs, group terminals, and unrelated keys remain untouched', () => {
  assert.equal(terminalComposeShortcutAction(chord(), {
    tabActive: false, eligiblePane: true,
  }), 'ignore');
  assert.equal(terminalComposeShortcutAction(chord(), {
    tabActive: true, eligiblePane: false,
  }), 'ignore');
  assert.equal(terminalComposeShortcutAction(chord({ code: 'KeyN', key: 'n' }), {
    tabActive: true, eligiblePane: true,
  }), 'ignore');
});
