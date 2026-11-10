// An overlay window's title-bar icons sit in the middle of their buttons, the
// wizard skin's gilded boxes included. Each glyph is drawn in a fixed box and
// centred by grid; the wizard skin's page-level header-button padding would
// otherwise leave a fixed-size box a few pixels of content and push the glyph
// off its middle. A stylesheet scan, so a new icon button is held to it.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

async function rules() {
  const out = [];
  for (const sheet of ['dashboard.css', 'companion.css']) {
    const css = (await readFile(new URL(`../dashboard/${sheet}`, import.meta.url), 'utf8')).replace(/\/\*[\s\S]*?\*\//g, '');
    for (const [, selector, body] of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) out.push({ selector: selector.trim().replace(/\s+/g, ' '), body });
  }
  return out;
}

const centres = (body) => /display:\s*(inline-)?grid/.test(body) && /place-items:\s*center/.test(body);

test('the tab – × and the standard − ⇄ × centre a drawn glyph with no padding', async () => {
  const all = await rules();
  const tab = all.find(({ selector }) => selector === '.companion-tab-minimize, .companion-tab-close');
  assert.ok(tab && centres(tab.body) && /padding:\s*0\s*;/.test(tab.body), 'tab buttons');
  const standard = all.find(({ selector }) => selector === '.overlay-look-standard .overlay-window-controls button');
  assert.ok(standard && /padding:\s*0\s*;/.test(standard.body), 'standard buttons drop padding');
  const base = all.find(({ selector }) => selector === '.overlay-window-controls button');
  assert.ok(base && /display:\s*grid/.test(base.body) && /place-items:\s*center/.test(base.body), 'every look centres by grid');
});

test('the wizard skin keeps its gilded boxes on the title bar, centred', async () => {
  const all = await rules();
  const wizard = all.find(({ selector }) => selector.startsWith('body.wizard .drawing-board-overlay-titlebar :is('));
  assert.ok(wizard, 'a wizard rule for the overlay title bar');
  for (const cls of ['.overlay-window-controls button', '.companion-tab-minimize', '.companion-tab-close', '.companion-add']) {
    assert.ok(wizard.selector.includes(cls), `${cls} is covered`);
  }
  assert.ok(centres(wizard.body) && /padding:\s*0\s*;/.test(wizard.body), 'no page-level padding, centred');
  assert.ok(!/border|background/.test(wizard.body), 'the gilding stays the page rule’s');
});
