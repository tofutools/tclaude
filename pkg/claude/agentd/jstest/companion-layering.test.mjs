// The layering rule for resting companion surfaces (tcl-masauq): anything the
// operator opens paints above the chips. A stylesheet scan rather than a
// browser test, so a new menu or popover is held to the rule when it is added.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const sheets = ['dashboard.css', 'companion.css', 'board.css', 'mux.css', 'terminals.css', 'process-kit.css', 'process-terminal-overlay.css'];

// Popovers that sit inside something else's stacking context, so their own
// number says nothing about the page: the window hides the chips, the others
// are clipped to or carried by their host. A dialog's resize grip is part of it,
// and so are an overlay window's own controls and snapped edges.
// The scan reads the numbers only: it can't see a stacking context.
const nested = /dialog-resizer|overlay-window-(edge|controls)|companion-(menu|decide-queue)|fleet-link-pop|dock-card-menu|mux-terminal-disconnect-overlay|pk-pane-overlay|open-prs-confirm-backdrop/;
const opened = /menu|pop|modal|overlay|drawer|panel|dialog/i;

async function zRules() {
  const rules = [];
  for (const sheet of sheets) {
    const css = (await readFile(new URL(`../dashboard/${sheet}`, import.meta.url), 'utf8')).replace(/\/\*[\s\S]*?\*\//g, '');
    for (const [, selector, body] of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
      const z = body.match(/z-index:\s*(-?\d+)/);
      if (z) rules.push({ selector: selector.trim().replace(/\s+/g, ' '), z: Number(z[1]), body });
    }
  }
  return rules;
}

test('everything the operator opens paints above the resting chips', async () => {
  const rules = await zRules();
  const zOf = (selector) => rules.find((r) => r.selector === selector)?.z;
  const chips = zOf('.companion-chips');
  assert.ok(chips > 6, 'above page content');
  assert.equal(zOf('.knowledge-source-card'), chips, 'the docs offer rests with the chips');
  assert.ok(zOf('.fleet-map:has(.fleet-edge-link.open)') > chips, 'the map carries its link popover over the chips');
  const under = rules.filter((r) => opened.test(r.selector) && !nested.test(r.selector) && r.z <= chips);
  assert.deepEqual(under.map((r) => `${r.selector} (${r.z})`), []);
});

test('the docs offer stays above a window or board, but not above a dialog', async () => {
  const rule = (await zRules()).find((r) => /companion-window:not/.test(r.selector) && r.selector.endsWith('.knowledge-source-card'));
  assert.ok(rule.z > 951, 'above the companion window and a front board');
  assert.match(rule.selector, /:not\(:has\(\.modal-overlay\.show, \.manage-overlay\.show\)\)/);
  const css = await readFile(new URL('../dashboard/companion.css', import.meta.url), 'utf8');
  assert.match(css, /@media \(max-width: 660px\) \{\s*body\.dock-open \.knowledge-source-card \{ z-index: 45; \}/, 'a narrow window keeps it off the dock\'s underside');
});

test('the chips step clear of the dock instead of covering its menus', async () => {
  const css = await readFile(new URL('../dashboard/companion.css', import.meta.url), 'utf8');
  assert.match(css, /body\.dock-open \{ --chip-right: calc\(var\(--dock-w\) \+ 16px\); \}/);
  assert.match(css, /\.companion-spot-bottom-right \{ right: var\(--chip-right\); \}/);
  assert.match(css, /@media \(max-width: 620px\) \{\s*body\.dock-open \{ --chip-right: 16px; \}\s*body\.dock-open \.companion-chips:not\(\.dragging\) \{ z-index: 45; \}/, 'a narrow window keeps them on screen');
});
