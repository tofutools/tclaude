import assert from 'node:assert/strict';
import test from 'node:test';

import { createPreactHarness } from './preact-harness.mjs';

const REGULAR_FAVICON =
  'data:image/svg+xml,<svg xmlns="http://www.w3.org/2000/svg"><text>🤝</text></svg>';

test('theme changes replace and uniquely version the favicon link', async (t) => {
  const harness = await createPreactHarness(t);
  const { document, window } = harness;
  document.head.innerHTML = `<title>tclaude agent dashboard</title><link rel="icon">`;
  document.querySelector('link[rel="icon"]').setAttribute('href', REGULAR_FAVICON);
  document.body.innerHTML = '<header><h1>🤝 tclaude agent dashboard</h1></header>';

  Object.defineProperty(window, 'location', {
    configurable: true,
    value: new URL('http://dashboard.test/'),
  });
  Object.defineProperty(window, 'history', {
    configurable: true,
    value: {
      state: null,
      replaceState(state, _unused, url) {
        this.state = state;
        this.url = url;
      },
    },
  });

  const theme = await harness.importDashboardModule('js/slop.js');

  const initialLink = document.querySelector('link[rel="icon"]');
  theme.applySlopThemeIfRequested();
  const regularLink = document.querySelector('link[rel="icon"]');
  assert.notEqual(regularLink, initialLink, 'startup replaces the tab-cached favicon node');
  assert.match(regularLink.getAttribute('href'), /#tclaude-regular-[^-]+-[^-]+-1$/);

  theme.toggleWizard();
  const wizardLink = document.querySelector('link[rel="icon"]');
  assert.notEqual(wizardLink, regularLink, 'entering wizard mode replaces the favicon node');
  assert.match(wizardLink.getAttribute('href'), /🧙/);
  assert.match(wizardLink.getAttribute('href'), /#tclaude-wizard-[^-]+-[^-]+-2$/);
  assert.equal(document.title, "The Wizard's Tower");

  theme.toggleWizard();
  const restoredLink = document.querySelector('link[rel="icon"]');
  assert.notEqual(restoredLink, wizardLink, 'leaving wizard mode replaces the favicon node again');
  assert.match(restoredLink.getAttribute('href'), /🤝/);
  assert.match(restoredLink.getAttribute('href'), /#tclaude-regular-[^-]+-[^-]+-3$/);
  assert.equal(document.title, 'tclaude agent dashboard');
});
