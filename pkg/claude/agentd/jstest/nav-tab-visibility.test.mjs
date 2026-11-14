// js/nav-tab-visibility.js: a tab's nav anchor counts as shown on the desktop
// when it renders, and on a phone (whose tab bar is hidden) unless a gate
// hides the anchor itself.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('nav anchors: rendered on the desktop, not gated on a phone', async (t) => {
  const harness = await createPreactHarness(t);
  const { navTabShown } = await harness.importDashboardModule('js/nav-tab-visibility.js');
  const { document } = harness;
  const nav = document.body.appendChild(document.createElement('nav'));
  const shown = nav.appendChild(document.createElement('a'));
  const gated = nav.appendChild(document.createElement('a'));
  gated.style.display = 'none';
  const rendered = (el, value) => Object.defineProperty(el, 'offsetParent', { configurable: true, get: () => value });

  rendered(shown, nav);
  rendered(gated, null);
  assert.equal(navTabShown(shown, false), true);
  assert.equal(navTabShown(gated, false), false);
  assert.equal(navTabShown(null, false), false);

  // On a phone the whole tab bar is hidden, so nothing renders. LinkeDOM has
  // no computed style; the anchor's own style stands in for it.
  const styleOf = (el) => el.style;
  nav.style.display = 'none';
  rendered(shown, null);
  assert.equal(navTabShown(shown, true, styleOf), true, 'an ungated anchor is available');
  assert.equal(navTabShown(gated, true, styleOf), false, 'a gated anchor is not');
  assert.equal(navTabShown(null, true, styleOf), false);
  nav.remove();
});
