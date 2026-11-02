// One layering scale (tcl-s7zf2k): every z-index in the dashboard is a layer
// from ui.css (--z-content … --z-toast, and --z-system out of band), or calc()
// of one plus a step for order inside the layer. Raw 0-9 is the allowance for
// stacking inside one component (a sticky head over its rows); anything else
// fails here, so a new surface picks its layer instead of a number. A
// stylesheet scan: it can't see stacking contexts, so it holds the numbers
// and the layer each kind of surface sits on.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const read = (file) => readFileSync(new URL(file, DASHBOARD), 'utf8');
const SHEETS = readdirSync(DASHBOARD).filter((name) => name.endsWith('.css')).sort();
const LAYERS = ['content', 'window', 'chrome', 'panel', 'dialog', 'popover', 'confirm', 'toast', 'system'];

const LAYER = `var\\(--z-(${LAYERS.join('|')})\\)`;
const ALLOWED = [
  /^auto$/,
  /^[0-9]$/,
  new RegExp(`^${LAYER}$`),
  new RegExp(`^calc\\(${LAYER} [+-] \\d{1,3}\\)$`),
  /^var\(--overlay-level, var\(--z-window\)\)$/,
];

// Comments blanked to spaces, so offsets and line numbers still match.
const uncommented = (css) => css.replace(/\/\*[\s\S]*?\*\//g, (comment) => comment.replace(/[^\n]/g, ' '));
const lineOf = (source, index) => source.slice(0, index).split('\n').length;

function zRules() {
  const rules = [];
  for (const sheet of SHEETS) {
    const css = uncommented(read(sheet));
    for (const match of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
      const z = match[2].match(/z-index:\s*([^;}]+?)\s*(?:!important\s*)?(?:;|$)/);
      if (!z) continue;
      rules.push({ sheet, line: lineOf(css, match.index + match[0].indexOf(z[0])), selector: match[1].trim().replace(/\s+/g, ' '), value: z[1].trim() });
    }
  }
  return rules;
}

const rules = zRules();
const valueOf = (selector) => {
  const rule = rules.find((r) => r.selector === selector);
  assert.ok(rule, `a z-index rule for ${selector}`);
  return rule.value;
};
const layerOf = (selector) => valueOf(selector).match(/--z-(\w+)/)?.[1];

test('every z-index is a layer from ui.css or in-component 0-9', () => {
  const raw = rules.filter((rule) => !ALLOWED.some((pattern) => pattern.test(rule.value)));
  assert.deepEqual(raw.map((rule) => `${rule.sheet}:${rule.line} ${rule.selector} { z-index: ${rule.value} }`), [], [
    'Use a layer from dashboard/ui.css instead of a number:',
    '  var(--z-chrome | panel | dialog | popover | confirm | toast), or calc(var(--z-…) + n) for order inside it.',
    'Raw 0-9 is only for stacking inside one component.',
  ].join('\n'));
});

test('the layers rise in the order the scale lists them', () => {
  const css = read('ui.css');
  const levels = LAYERS.map((name) => Number(css.match(new RegExp(`--z-${name}: (\\d+);`))?.[1]));
  assert.ok(levels.every(Number.isFinite), 'ui.css defines every layer');
  for (let i = 1; i < levels.length; i++) {
    assert.ok(levels[i] - levels[i - 1] >= 1000, `--z-${LAYERS[i]} sits a full band over --z-${LAYERS[i - 1]}`);
  }
});

test('no script writes a z-index number', () => {
  const scripts = readdirSync(new URL('js/', DASHBOARD)).filter((name) => name.endsWith('.js'));
  const found = [];
  for (const name of scripts) {
    const source = read(`js/${name}`);
    for (const match of source.matchAll(/z-index:\s*\d|zIndex\s*=\s*['"`]?\d/g)) found.push(`js/${name}:${lineOf(source, match.index)}`);
  }
  for (const page of ['dashboard.html', 'terminals.html']) {
    const source = read(page);
    for (const match of source.matchAll(/z-index:\s*\d{2,}/g)) found.push(`${page}:${lineOf(source, match.index)}`);
  }
  assert.deepEqual(found, []);
});

test('a raised window stays in the window layer, under the snap guide and the chrome', () => {
  const windows = read('js/overlay-window.js');
  const bottom = Number(windows.match(/export const OVERLAY_BOTTOM_LEVEL = (\d+);/)[1]);
  // A window's level tops out at OVERLAY_TOP_LEVEL, and a split's lines sit two above it.
  const top = Number(windows.match(/export const OVERLAY_TOP_LEVEL = (\d+);/)[1]) + 2;
  const css = read('ui.css');
  const band = (name) => Number(css.match(new RegExp(`--z-${name}: (\\d+);`))[1]);
  assert.ok(bottom >= band('window'), `windows start in the window layer (${bottom})`);
  const guide = valueOf('.overlay-snap-guide').match(/calc\(var\(--z-window\) \+ (\d+)\)/);
  assert.ok(guide, 'the snap guide is a step in the window layer');
  assert.ok(top < band('window') + Number(guide[1]), `the front split's lines (${top}) stay under the snap guide`);
  assert.ok(band('window') + Number(guide[1]) < band('chrome'), 'and the guide under the chrome');
  for (const selector of ['.board-overlay', '.companion-window', '.window-resize-frame']) {
    assert.equal(valueOf(selector), 'var(--overlay-level, var(--z-window))', `${selector} takes its window's level`);
  }
});

test('opening a panel, dialog, popover or confirmation never moves a window', () => {
  // The old scheme lowered or hid windows while a dialog showed. Under one
  // scale that is never needed: whatever is opened paints over the window.
  const opened = /\.(modal-overlay|manage-overlay)\.show|-modal\.show|\.open-prs\.is-open|-pop\b|-menu\.open/;
  const windowish = /overlay-window|companion-window|board-overlay|window-resize-frame|companion-resize-frame|overlay-split/;
  const css = uncommented(SHEETS.map(read).join('\n'));
  const moved = [];
  for (const [, selector, body] of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    if (opened.test(selector) && windowish.test(selector) && /z-index|visibility|display|transform|translate|inset|left|top/.test(body)) {
      moved.push(selector.trim().replace(/\s+/g, ' '));
    }
  }
  assert.deepEqual(moved, []);
});

test('each kind of surface sits on its layer', () => {
  const expect = {
    chrome: ['footer', '.overlay-dock.floating', '.knowledge-source-card', '.companion-marks', '.companion-guide-ring'],
    panel: ['#agent-dock', '.human-notification-drawer'],
    dialog: ['.modal-overlay', '.manage-overlay', '#profile-editor-modal', '#perm-edit-modal', '#directory-picker-modal', '.overlay-expose'],
    popover: ['.action-menu', '.view-menu', '.open-prs-popover', '.modal-overlay.palette-overlay', '.board-context', '.mux-tab-menu', '.ui-menu.overlay-dock-actions', 'footer:has(.open-prs.is-open)'],
    confirm: ['#confirm-modal', '.access-banner', '#sandbox-profile-diff-modal'],
    toast: ['.toast', '.transfer-tray'],
    system: ['.disconnect-overlay', 'body.dashboard-booting > #boot-progress'],
  };
  for (const [layer, selectors] of Object.entries(expect)) {
    for (const selector of selectors) assert.equal(layerOf(selector), layer, `${selector} is on the ${layer} layer`);
  }
});
