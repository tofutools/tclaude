// Dialog meta lines (.modal-meta) come in two shapes: a boxed line in .modal
// dialogs and a left-edged note in .cron-create-modal dialogs. Both paint from
// tokens (ui.css): --ui-card-inset-bg and --ui-card-line for the surface,
// --ui-meta-text for the boxed text, --ui-note-edge for the note's edge. The
// wizard skin used to repeat that paint once per dialog id, and dialogs it
// missed stayed grey. A skin rule may not paint a .modal-meta surface again;
// the few deliberate variants below keep their own look.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const VARIANTS = new Set([
  'body.wizard #standing-order-modal .modal-meta',
  'body.wizard :is(#paf-export-modal, #group-export-modal) .modal-meta',
  'body.wizard #link-modal .modal-meta',
]);

// paintedMetaSurfaces lists skin selectors (whitespace-normalised) whose
// subject is .modal-meta and whose rule sets a background or border colour.
export function paintedMetaSurfaces(css) {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const found = new Set();
  for (const [, selector, body] of source.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    if (!/(?:^|[;\s])(?:background(?:-color)?|border(?:-[a-z]+)*-color|border(?:-(?:top|right|bottom|left))?)\s*:/.test(body)) continue;
    for (const raw of selector.split(/,(?![^()]*\))/)) {
      const part = raw.trim().replace(/\s+/g, ' ');
      if (!/^body\.(?:wizard|slop)\b/.test(part)) continue;
      if (/\.modal-meta$/.test(part.split(/\s*[>+~]\s*|\s(?![^()]*\))/).pop())) found.add(part);
    }
  }
  return found;
}

test('the scanner finds a skin repainting the meta line', () => {
  const css = [
    'body.wizard #x-modal .modal-meta { background: #140f28; color: #b39ddb; }',
    'body.wizard #y-modal .modal-meta { color: #b39ddb; }',
    'body.slop :is(#a, #b) .modal-meta, body.slop .other { border-left-color: red; }',
    '.modal .modal-meta { background: var(--ui-card-inset-bg); }',
  ].join('\n');
  assert.deepEqual([...paintedMetaSurfaces(css)].sort(), ['body.slop :is(#a, #b) .modal-meta', 'body.wizard #x-modal .modal-meta']);
});

test('both meta line shapes paint from the tokens', () => {
  const css = readFileSync(new URL('dashboard.css', DASHBOARD), 'utf8');
  const body = (selector) => css.match(new RegExp(`^${selector.replace(/[.]/g, '\\.')} \\{([^}]*)\\}`, 'm'))?.[1] ?? '';
  const boxed = body('.modal .modal-meta');
  for (const want of ['background: var(--ui-card-inset-bg)', 'border: 1px solid var(--ui-card-line)', 'color: var(--ui-meta-text)']) assert.ok(boxed.includes(want), `.modal .modal-meta: ${want}`);
  const note = body('.cron-create-modal .modal-meta');
  for (const want of ['background: var(--ui-card-inset-bg)', 'border-left: 2px solid var(--ui-note-edge)']) assert.ok(note.includes(want), `.cron-create-modal .modal-meta: ${want}`);
  const ui = readFileSync(new URL('ui.css', DASHBOARD), 'utf8');
  for (const token of ['--ui-meta-text', '--ui-note-edge']) {
    assert.match(ui, new RegExp(`:root \\{[^}]*${token}:`));
    assert.match(ui, new RegExp(`body\\.wizard \\{[^}]*${token}:`));
  }
});

test('skins do not repaint the meta line per dialog', () => {
  const found = [];
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css'))) {
    for (const sel of paintedMetaSurfaces(readFileSync(new URL(file, DASHBOARD), 'utf8'))) if (!VARIANTS.has(sel)) found.push(`${file}: ${sel}`);
  }
  assert.deepEqual(found, [], '.modal-meta paints from the --ui-card-* / --ui-meta-text / --ui-note-edge tokens; set a skin token once instead');
});
