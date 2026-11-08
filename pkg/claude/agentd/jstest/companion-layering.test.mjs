// The layering rule for resting companion surfaces (tcl-masauq): anything the
// operator opens paints above the chips, overlay windows included. The chips
// are a step over content on the one scale in ui.css (tcl-s7zf2k, held by
// ui-layers.test.mjs); this pins where the companion's own surfaces sit on it. A stylesheet scan rather than a browser
// test, so a new surface is held to the rule when it is added.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const sheets = ['dashboard.css', 'companion.css', 'drawing-board.css', 'mux.css', 'terminals.css', 'process-kit.css', 'process-terminal-overlay.css'];
const LAYERS = ['content', 'window', 'chrome', 'panel', 'dialog', 'popover', 'confirm', 'toast', 'system'];

// Popovers that sit inside something else's stacking context, so their own
// value says nothing about the page: the window hides the chips, the others
// are clipped to or carried by their host. A dialog's resize grip is part of it,
// and so are an overlay window's own controls, snapped edges and the frame that
// covers its content while it comes up (overlay-window-motion.js), a dock section
// head and the process editor's issues panel (inside its full-screen dialog).
const nested = /process-issues-panel|dock-section-head|dialog-resizer|overlay-window-(edge|controls)|overlay-live-frame|companion-(menu|decide-queue)|fleet-link-pop|mux-terminal-disconnect-overlay|pk-pane-overlay|open-prs-confirm-backdrop/;
const opened = /menu|pop|modal|overlay|drawer|panel|dialog/i;

// rank orders a z-index value on the scale: a layer plus its step, or a raw
// in-component number, which stays under every layer above content.
function rank(value) {
  const layer = value.match(/--z-(\w+)\)(?: \+ (\d+))?/);
  if (layer) return LAYERS.indexOf(layer[1]) * 1000 + Number(layer[2] || 0);
  return Number(value);
}

async function zRules() {
  const rules = [];
  for (const sheet of sheets) {
    const css = (await readFile(new URL(`../dashboard/${sheet}`, import.meta.url), 'utf8')).replace(/\/\*[\s\S]*?\*\//g, '');
    for (const [, selector, body] of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
      const z = body.match(/z-index:\s*([^;}]+?)\s*(?:;|$)/);
      if (z && z[1] !== 'auto') rules.push({ selector: selector.trim().replace(/\s+/g, ' '), z: rank(z[1].trim()), body });
    }
  }
  return rules;
}

test('everything the operator opens paints above the resting chips', async () => {
  const rules = await zRules();
  const zOf = (selector) => rules.find((r) => r.selector === selector)?.z;
  const chips = zOf('.companion-chips');
  assert.ok(chips > 9 && chips < LAYERS.indexOf('window') * 1000, 'over in-component stacking, under the windows');
  const map = zOf('.fleet-map:has(.fleet-edge-link.open)');
  assert.ok(map > chips && map < LAYERS.indexOf('window') * 1000, 'the map carries its link popover over the chips, and stays page content under the windows');
  const under = rules.filter((r) => opened.test(r.selector) && !nested.test(r.selector) && r.z <= chips);
  assert.deepEqual(under.map((r) => `${r.selector} (${r.z})`), []);
});

test('the docs offer stays above every window, a dialog above it', async () => {
  const rules = await zRules();
  const zOf = (selector) => rules.find((r) => r.selector === selector)?.z;
  const windowTop = LAYERS.indexOf('chrome') * 1000 - 1;
  assert.ok(zOf('.knowledge-source-card') > windowTop, 'a window or drawing board never hides its question');
  assert.ok(zOf('.modal-overlay') > zOf('.knowledge-source-card'), 'a dialog covers it');
  const css = await readFile(new URL('../dashboard/companion.css', import.meta.url), 'utf8');
  assert.match(css, /@media \(max-width: 660px\) \{\s*body\.dock-open \.knowledge-source-card \{ z-index: var\(--z-panel\); \}/, 'a narrow window keeps it off the dock\'s underside');
});

test('the chips step clear of the dock instead of covering its menus', async () => {
  const css = await readFile(new URL('../dashboard/companion.css', import.meta.url), 'utf8');
  assert.match(css, /body\.dock-open \{ --chip-right: calc\(var\(--dock-w\) \+ 16px\); \}/);
  assert.match(css, /\.companion-spot-bottom-right \{ right: var\(--chip-right\); \}/);
  assert.match(css, /@media \(max-width: 620px\) \{\s*body\.dock-open \{ --chip-right: 16px; \}\s*body\.dock-open \.companion-chips:not\(\.dragging\) \{ z-index: var\(--z-panel\); \}/, 'a narrow window keeps them on screen');
});

test('the footer\'s PR popover opens above overlay windows, and the footer chips stay on the bar (tcl-rsr7pi)', async () => {
  const rules = await zRules();
  const zOf = (selector) => rules.find((r) => r.selector === selector)?.z;
  const footer = zOf('footer:has(.open-prs.is-open)');
  assert.equal(footer, LAYERS.indexOf('popover') * 1000, 'the footer carries the popover on the popover layer');
  assert.ok(footer > zOf('.overlay-dock.floating') && footer > zOf('#agent-dock'), 'over the floating overlay dock and the agent dock');
  assert.ok(zOf('footer') >= LAYERS.indexOf('chrome') * 1000, 'resting, the bar and the window dock in it sit over every overlay window');
  assert.ok(zOf('body:has(.open-prs.is-open) .companion-chips.compact:not(.dragging)') > footer, 'chips in the footer stay above the lifted bar');
  assert.ok(zOf('.companion-chips.compact') > zOf('footer'), 'and above the resting bar, so they rise over windows with it');
});
