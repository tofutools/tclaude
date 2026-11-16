// No control inside a <summary>. Interactive content there is invalid HTML
// and Chrome's Issues panel flags every one ("Interactive element inside of a
// <summary> element"); a screen reader announces the summary as one button
// with the controls folded into it. A foldable section whose header carries
// controls is a disclosure.js section instead (a header row with a real toggle
// button, the controls beside it), as the dock sections, a group's Triggers
// and the fleet node sections are.
//
// The group boxes of the Groups tree (and the sandbox profile editor's
// sections, whose help button sits in the legend) still hold controls in their
// <summary>: converting them reaches into drag and drop, reorder and the saved
// fold state, and is its own piece of work. KNOWN holds them at today's count
// per file, so no new <summary> takes a control and the count only goes down.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const read = (file) => readFileSync(new URL(file, DASHBOARD), 'utf8');
const lineOf = (source, index) => source.slice(0, index).split('\n').length;

const KNOWN = {
  'js/groups-list.js': 1, // RealGroupSummary: the group header
  'js/management-island.js': 2, // sandbox profile sections: the legend's help button
};

const CONTROL = new RegExp([
  String.raw`<(?:button|input|select|textarea)\b`,
  String.raw`<a\b[^>]*\bhref=`,
  String.raw`<\$\{(?:Button|MenuItem|Select|Input|Textarea|Check|Switch|ActionMenu|InlineEditor|HelpDisclosure|DisclosureToggle)\}`,
].join('|'));

// summariesWithControls lists the lines of <summary> elements whose markup
// (up to its </summary>) holds a control, skipping ones named in a comment.
export function summariesWithControls(source) {
  const found = [];
  for (const match of source.matchAll(/<summary\b/g)) {
    const lineStart = source.lastIndexOf('\n', match.index) + 1;
    const before = source.slice(lineStart, match.index);
    if (before.includes('//') || /^\s*\*/.test(before)) continue;
    const end = source.indexOf('</summary>', match.index);
    if (end < 0) continue;
    if (CONTROL.test(source.slice(match.index, end))) found.push(lineOf(source, match.index));
  }
  return found;
}

test('the scanner finds a control inside a <summary>', () => {
  const source = [
    'html`<details><summary>Plain <strong>title</strong></summary></details>`;',
    'html`<details><summary>Title <${Button} onClick=${go}>x<//></summary></details>`;',
    'html`<details><summary>Title',
    '  <a href="/x">more</a></summary></details>`;',
    '// a <summary> holding a <button> in prose',
    'html`<details><summary>Count <span class="muted">3</span></summary></details>`;',
  ].join('\n');
  assert.deepEqual(summariesWithControls(source), [2, 3]);
});

test('no new control goes inside a <summary>', () => {
  const counts = {};
  const files = ['dashboard.html', ...readdirSync(new URL('js/', DASHBOARD)).filter((f) => f.endsWith('.js')).map((f) => `js/${f}`)];
  for (const file of files) {
    const lines = summariesWithControls(read(file));
    if (lines.length) counts[file] = lines.length;
  }
  for (const [file, count] of Object.entries(counts)) {
    assert.ok(count <= (KNOWN[file] || 0),
      `${file} has ${count} <summary> holding a control (allowed ${KNOWN[file] || 0}): make it a disclosure.js section`);
  }
  for (const [file, count] of Object.entries(KNOWN)) {
    assert.equal(counts[file] || 0, count, `${file} is down to ${counts[file] || 0}: lower KNOWN to match`);
  }
});
