// Dialog boxes use the standard .ui-dialog (dashboard/ui.css): one background,
// edge, corner, shadow and text colour per skin, from the --ui-dialog-*
// tokens. ManagementOverlay puts it on every dialog it renders. Dialogs used to
// paint their own box: each *-modal class carried the same dark panel, and the
// wizard skin repeated its violet panel once per dialog id, more than forty
// times. An area class keeps size, padding and layout; a stylesheet rule whose
// subject is a *-modal or *-dialog class may not paint a box itself, meaning a
// background and a border in one rule.
//
// Existing ones are a ratchet: ui-dialog-baseline.json lists, by selector, the
// boxes not yet migrated (previews and deliberately distinct dialogs such as
// the standing-order panels). Keying on the selector, not the class, keeps a
// new per-dialog copy on a shared class such as .cron-create-modal failing.
// A new one fails, and a migrated one must leave the baseline. Regenerate with
//   UPDATE_UI_DIALOG_BASELINE=1 node --test jstest/ui-dialog.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const BASELINE = new URL('./ui-dialog-baseline.json', import.meta.url);
const BOX = [/(?:^|[;\s])background(?:-color)?\s*:/, /(?:^|[;\s])border(?:-color)?\s*:/];

// handRolledBoxes lists the selectors (whitespace-normalised) whose subject is a
// single *-modal / *-dialog class that the rule paints as a box. A state
// (`:hover`, `.danger`) is not a box.
export function handRolledBoxes(css) {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const found = new Set();
  for (const [, selector, body] of source.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    if (!BOX.every((prop) => prop.test(body))) continue;
    for (const part of selector.split(/,(?![^()]*\))/)) {
      const subject = part.trim().split(/\s*[\s>+~]\s*/).pop();
      if (/[:[]/.test(subject)) continue;
      const classes = [...subject.matchAll(/\.([\w-]+)/g)].map((m) => m[1]);
      if (classes.length !== 1) continue;
      if (/^(?:[\w-]+-)?(?:modal|dialog)$/.test(classes[0])) found.add(part.trim().replace(/\s+/g, ' '));
    }
  }
  return found;
}

test('the scanner finds hand-painted dialog boxes', () => {
  const css = [
    '.a-modal { padding: 18px; width: 640px; }',
    '.b-modal { background: #161b22; border: 1px solid #2a3340; box-shadow: 0 8px 24px #0008; }',
    'body.wizard #x .c-modal, .other { background: violet; border-color: gold; }',
    '.d-modal.danger { background: red; border-color: red; box-shadow: none; }',
    '.e-modal:hover { background: red; border: 0; box-shadow: none; }',
    '/* .f-modal { background: red; border: 0; box-shadow: none; } */',
    '.modal { background: #161b22; border: 1px solid #2a3340; box-shadow: 0 8px 24px #0008; }',
  ].join('\n');
  assert.deepEqual([...handRolledBoxes(css)].sort(), ['.b-modal', '.modal', 'body.wizard #x .c-modal']);
});

test('ui.css defines the standard dialog box and ManagementOverlay applies it', () => {
  const css = readFileSync(new URL('ui.css', DASHBOARD), 'utf8');
  assert.match(css, /\.ui-dialog \{[^}]*background: var\(--ui-dialog-bg\)/);
  assert.match(css, /body\.wizard \{[^}]*--ui-dialog-bg:/, 'the wizard skin has its own dialog box');
  const overlay = readFileSync(new URL('js/management-overlay.js', DASHBOARD), 'utf8');
  assert.match(overlay, / ui-dialog`/, 'ManagementOverlay renders every dialog as a .ui-dialog');
  // The dialogs built without ManagementOverlay take the box themselves.
  for (const [file, needle] of [
    ['shell-island.js', '<div class="modal ui-dialog" role="dialog"'],
    ['plugins-island.js', '<div class="cron-create-modal ui-dialog" role="dialog"'],
    ['profiles-island.js', '<form class="cron-create-modal ui-dialog" role="dialog"'],
  ]) assert.ok(readFileSync(new URL(`js/${file}`, DASHBOARD), 'utf8').includes(needle), `${file} renders its dialog as a .ui-dialog`);
});

test('no stylesheet hand-paints a new dialog box', () => {
  const found = new Set();
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css') && f !== 'ui.css')) {
    for (const cls of handRolledBoxes(readFileSync(new URL(file, DASHBOARD), 'utf8'))) found.add(cls);
  }
  if (process.env.UPDATE_UI_DIALOG_BASELINE) writeFileSync(BASELINE, `${JSON.stringify([...found].sort(), null, 2)}\n`);
  const baseline = new Set(JSON.parse(readFileSync(BASELINE, 'utf8')));
  assert.deepEqual([...found].filter((c) => !baseline.has(c)).sort(), [],
    'paint dialog boxes with .ui-dialog (skin it through the --ui-dialog-* tokens); keep an area class for size and layout only');
  assert.deepEqual([...baseline].filter((c) => !found.has(c)).sort(), [],
    'these are migrated: remove them from jstest/ui-dialog-baseline.json');
});

// Dialog headings take the skin's title face from `:where(.ui-dialog) h3`
// (ui.css). The wizard skin used to copy its gilded serif onto each dialog's
// h3 by id; a new copy fails here. The process editors render outside
// .ui-dialog, and so does the config diff dialog (its own panel), so their
// rules stay.
const TITLE_EXEMPT = new Set([
  'body.wizard .process-editor-modal .modal h3',
  'body.wizard :is(#config-diff-modal, #sandbox-profile-diff-modal) .config-diff-modal h3',
]);

export function handRolledTitles(css) {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const found = new Set();
  for (const [, selector, body] of source.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    if (!/font-family\s*:[^;]*Papyrus/i.test(body)) continue;
    for (const part of selector.split(/,(?![^()]*\))/)) {
      const tokens = part.trim().split(/\s*[\s>+~]\s*/);
      if (tokens.at(-1) !== 'h3') continue;
      if (!tokens.slice(0, -1).some((t) => /\.[\w-]*(?:modal|dialog)\b/.test(t))) continue;
      const key = part.trim().replace(/\s+/g, ' ');
      if (!TITLE_EXEMPT.has(key)) found.add(key);
    }
  }
  return found;
}

test('dialog titles take the skin title face from ui.css, not per-dialog copies', () => {
  assert.deepEqual([...handRolledTitles("body.wizard #x .cron-create-modal h3, .y { font-family: Papyrus, serif; }")], ['body.wizard #x .cron-create-modal h3']);
  const ui = readFileSync(new URL('ui.css', DASHBOARD), 'utf8');
  assert.match(ui, /:where\(\.ui-dialog\) h3 \{[^}]*font-family: var\(--ui-dialog-title-font\)/);
  assert.match(ui, /body\.wizard \{[^}]*--ui-dialog-title-font: Papyrus/);
  const found = [];
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css') && f !== 'ui.css')) {
    for (const sel of handRolledTitles(readFileSync(new URL(file, DASHBOARD), 'utf8'))) found.push(`${file}: ${sel}`);
  }
  assert.deepEqual(found, [], 'dialog headings get the wizard face from the --ui-dialog-title-* tokens');
});
