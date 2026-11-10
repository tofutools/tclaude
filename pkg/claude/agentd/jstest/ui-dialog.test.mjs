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
  // The dialog built without ManagementOverlay takes the box itself.
  for (const [file, needle] of [
    ['shell-island.js', '<div class="modal ui-dialog" role="dialog"'],
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
// h3 by id; a new copy fails here. The standing-orders heading is a
// deliberate variant (smaller, softer glow).
const TITLE_EXEMPT = new Set([
  'body.wizard #group-standing-orders-modal .group-standing-orders-heading h3',
]);

export function handRolledTitles(css) {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const found = new Set();
  for (const [, selector, body] of source.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    if (!/font-family\s*:[^;]*Papyrus/i.test(body)) continue;
    for (const part of selector.split(/,(?![^()]*\))/)) {
      const tokens = part.trim().split(/\s*[\s>+~]\s*/);
      if (tokens.at(-1) !== 'h3') continue;
      if (!tokens.slice(0, -1).some((t) => /[.#][\w-]*(?:modal|dialog)\b/.test(t))) continue;
      const key = part.trim().replace(/\s+/g, ' ');
      if (!TITLE_EXEMPT.has(key)) found.add(key);
    }
  }
  return found;
}

test('dialog titles take the skin title face from ui.css, not per-dialog copies', () => {
  assert.deepEqual([...handRolledTitles("body.wizard #x .cron-create-modal h3, .y { font-family: Papyrus, serif; }")], ['body.wizard #x .cron-create-modal h3']);
  assert.deepEqual([...handRolledTitles("body.wizard #foo-modal h3 { font-family: Papyrus, serif; }")], ['body.wizard #foo-modal h3']);
  const ui = readFileSync(new URL('ui.css', DASHBOARD), 'utf8');
  assert.match(ui, /:where\(\.ui-dialog\) h3 \{[^}]*font-family: var\(--ui-dialog-title-font\)/);
  assert.match(ui, /body\.wizard \{[^}]*--ui-dialog-title-font: Papyrus/);
  const found = [];
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css') && f !== 'ui.css')) {
    for (const sel of handRolledTitles(readFileSync(new URL(file, DASHBOARD), 'utf8'))) found.push(`${file}: ${sel}`);
  }
  assert.deepEqual(found, [], 'dialog headings get the wizard face from the --ui-dialog-title-* tokens');
});

// Raw fields inside a wizard dialog, filter bar or config form take the
// shared wizard field paint from ui.css (`body.wizard :is(.ui-dialog,
// .filter-bar, …) :is(input…, select, textarea)`), and checkboxes and radios
// take the wizard accent from the global zero-specificity rule at
// the top of dashboard.css. The wizard skin used to repeat both once per
// dialog id; a rule that only repeats them fails here. A dialog that wants a
// different field look still writes its own rule with its own values.
const SHARED_FIELD_DECLS = new Set([
  'background: #140f28',
  'border-color: rgba(122, 93, 176, 0.5)',
  'color: #f3e6c0',
  'border-color: #a97bd6',
  'accent-color: #a97bd6',
]);
// Kept on purpose: #launch-target is not a dialog; the profile import and
// export dialogs paint every input, file pickers and checkboxes included,
// which the shared rule leaves alone; and three accents override a base
// stylesheet tint on that control, which the zero-specificity rule cannot.
// The spawn-harness focus edge must out-rank that dialog's own id-scoped
// field paint.
const FIELD_COPY_EXEMPT = new Set([
  'body.wizard #spawn-harness-policy-modal .spawn-harness-cell select:where(:not(.ui-select)):focus',
  'body.wizard #spawn-harness-policy-modal .spawn-harness-cell textarea:focus',
  'body.wizard :is(#paf-export-modal, #group-export-modal) .paf-check input',
  'body.wizard #trigger-modal .trigger-scope-option input[type=radio]',
  'body.wizard .git-repositories-modal input[type=checkbox]',
  'body.wizard :is(#clone-agent-modal, #reincarnate-agent-modal, #group-nest-modal, #launch-target) :is(.cron-create-row, .lt-row) :is(input[type=text], input:not([type]), select:where(:not(.ui-select)), textarea)',
  'body.wizard :is(#clone-agent-modal, #reincarnate-agent-modal, #group-nest-modal, #launch-target) :is(.cron-create-row, .lt-row) :is(input[type=text], input:not([type]), select:where(:not(.ui-select)), textarea):focus',
  'body.wizard #group-import-modal .cron-create-row input',
  'body.wizard #group-import-modal .cron-create-row input:focus',
  'body.wizard #template-import-modal .cron-create-row input',
  'body.wizard #template-import-modal .cron-create-row input:focus',
  'body.wizard :is(#profile-export-modal, #profile-import-modal) input',
  'body.wizard :is(#profile-export-modal, #profile-import-modal) input:focus',
  'body.wizard :is(#sandbox-profile-export-modal, #sandbox-profile-import-modal) input',
  'body.wizard :is(#sandbox-profile-export-modal, #sandbox-profile-import-modal) input:focus',
]);

// splitTop splits on `sep` outside any parentheses.
function splitTop(text, sep) {
  const out = [];
  let depth = 0, start = 0;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (c === '(') depth++;
    else if (c === ')') depth--;
    else if (depth === 0 && sep.test(c)) { out.push(text.slice(start, i)); start = i + 1; }
  }
  out.push(text.slice(start));
  return out.map((t) => t.trim()).filter(Boolean);
}

export function copiedFieldPaints(css) {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const found = new Set();
  for (const [, selector, body] of source.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    const decls = body.split(';').map((d) => d.trim().replace(/\s+/g, ' ').replace(/([ (,])\.(\d)/g, '$10.$2')).filter(Boolean);
    if (!decls.length || !decls.every((d) => SHARED_FIELD_DECLS.has(d))) continue;
    for (const part of splitTop(selector.replace(/\s+/g, ' '), /,/)) {
      const key = part;
      if (!key.startsWith('body.wizard ')) continue;
      const tokens = splitTop(key, /\s/);
      if (!tokens.slice(1, -1).some((t) => /[.#][\w-]*(?:modal|dialog)\b|\.(?:filter-bar|cfg-field|cfg-row-ctl)\b/.test(t))) continue;
      if (!/(?:^|[(,\s])(?:input|select|textarea)\b|\.add-member-search/.test(tokens.at(-1))) continue;
      if (!FIELD_COPY_EXEMPT.has(key)) found.add(key);
    }
  }
  return found;
}

test('wizard dialog fields take the shared paint, not per-dialog copies', () => {
  assert.deepEqual([...copiedFieldPaints('body.wizard #x-modal .row input[type=text], .y { background: #140f28; color: #f3e6c0; }')], ['body.wizard #x-modal .row input[type=text]']);
  assert.deepEqual([...copiedFieldPaints('body.wizard #x-modal :is(input[type=checkbox], input[type=radio]) { accent-color: #a97bd6; }')], ['body.wizard #x-modal :is(input[type=checkbox], input[type=radio])']);
  assert.deepEqual([...copiedFieldPaints('body.wizard #x-modal input { background: #241b3d; }')], []);
  assert.deepEqual([...copiedFieldPaints('body.wizard .filter-bar input { border-color: rgba(122, 93, 176, .5); }')], ['body.wizard .filter-bar input']);
  const ui = readFileSync(new URL('ui.css', DASHBOARD), 'utf8');
  assert.match(ui, /body\.wizard :is\(\.ui-dialog, \.filter-bar, [^)]*\) :is\(input:not\(\[type\]\), input\[type=text\][^{]*\{\s*background: #140f28/);
  const found = [];
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css') && f !== 'ui.css')) {
    for (const sel of copiedFieldPaints(readFileSync(new URL(file, DASHBOARD), 'utf8'))) found.push(`${file}: ${sel}`);
  }
  assert.deepEqual(found, [], 'wizard dialog fields get their paint from ui.css');
});
