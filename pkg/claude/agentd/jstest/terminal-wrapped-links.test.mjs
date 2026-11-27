// js/terminal-interactions.js: a URL its harness wrapped over several rows
// links as the whole URL, on the phone (screenRows → screenLines →
// screenLinks) and on the desktop (hardWrappedLinkProvider).
//
// The fixtures are what Claude Code drew in a 44-column tmux pane when asked
// to echo a long URL (captured with `tmux capture-pane -p -e`):
//
//   - Its reply hard-wraps the URL with real newlines, indenting each row it
//     carries on by two, and wraps every piece in an OSC 8 hyperlink to the
//     whole URL. tmux passes that on to the dashboard's xterm.
//   - Its echo of the prompt hard-wraps the same URL as plain text, one
//     padding column short of the edge, with no hyperlink at all.
import test from 'node:test';
import assert from 'node:assert/strict';
import { hardWrappedLinkProvider, screenLinks, screenLines, screenRows } from '../dashboard/js/terminal-interactions.js';

const URL = 'https://github.com/gigurra/tclaude/pull/4541/files#diff-0123456789abcdef0123456789abcdef';
const COLS = 44;

// [text, [first col, end col) of the OSC 8 hyperlink to URL, or none]
const REPLY = [
  ['● See https://github.com/gigurra/tclaude/pul', [6, 44]],
  ['  l/4541/files#diff-0123456789abcdef01234567', [2, 44]],
  ['  89abcdef for details.', [2, 10]],
];
const PROMPT_ECHO = [
  ['❯ Reply with only this URL in a sentence,'],
  ['  nothing else, no tools: See https://githu'],
  ['  b.com/gigurra/tclaude/pull/4541/files#dif'],
  ['  f-0123456789abcdef0123456789abcdef for'],
  ['  details.'],
];

// fakeTerm is an xterm with rows drawn on it, the hyperlinks registered as
// xterm's OSC 8 link service holds them (a cell's extended.urlId).
function fakeTerm(rows, cols = COLS) {
  const lines = rows.map(([text, link, wrapped = false]) => {
    const cells = [...text.padEnd(cols)].map((ch, x) => ({
      getWidth: () => 1, getChars: () => ch, isInvisible: () => false,
      extended: { urlId: link && x >= link[0] && x < link[1] ? 7 : 0 },
    }));
    return { isWrapped: wrapped, getCell: (x) => cells[x] };
  });
  return {
    cols, rows: rows.length,
    buffer: { active: { viewportY: 0, length: lines.length, getLine: (y) => lines[y] } },
    _core: { _oscLinkService: { getLinkData: (id) => (id === 7 ? { id: 'x', uri: URL } : undefined) } },
  };
}

// linkAtCell is the link screenLinks puts under a cell, as the phone's tap
// finds it.
function linkAtCell(term, row, col) {
  const lines = screenLines(screenRows(term));
  const links = screenLinks(lines);
  const i = lines.findIndex((l) => l.rows.some((r) => r.row === row));
  const index = lines[i].rows.find((r) => r.row === row).offsets[col];
  return links[i].find((l) => index >= l.start && index < l.end) || null;
}

test('a hyperlinked URL Claude Code hard-wrapped opens the whole URL from every piece', () => {
  const term = fakeTerm(REPLY);
  for (const [row, col] of [[0, 6], [0, 43], [1, 2], [1, 30], [2, 9]]) {
    assert.equal(linkAtCell(term, row, col)?.text, URL, `row ${row} col ${col}`);
  }
  assert.equal(linkAtCell(term, 2, 12), null, '"for details." stays text');
  assert.equal(linkAtCell(term, 0, 2), null);
  // The Select view's spans: each piece shows its own text.
  const lines = screenLines(screenRows(term));
  assert.deepEqual(screenLinks(lines).map((ls) => ls.map((l) => [l.start, l.end])), [[[6, 44]], [[2, 44]], [[2, 10]]]);
  assert.deepEqual(links(term, 2), [], 'the desktop leaves hyperlinks to xterm\'s own');
});

test('a plain URL hard-wrapped with a padding column joins into one link, and no further', () => {
  const term = fakeTerm(PROMPT_ECHO);
  for (const [row, col] of [[1, 31], [1, 42], [2, 2], [2, 42], [3, 5]]) {
    assert.equal(linkAtCell(term, row, col)?.text, URL, `row ${row} col ${col}`);
  }
  assert.equal(linkAtCell(term, 2, 43), null, 'the padding column is not part of it');
  assert.equal(linkAtCell(term, 3, 38), null, '"for" after the URL is not glued on');
  assert.equal(linkAtCell(term, 4, 3), null);
});

// links is what the desktop's provider gives for buffer line y (1-based).
function links(term, y) {
  let out = null;
  hardWrappedLinkProvider(term, { activate() {} }).provideLinks(y, (l) => { out = l; });
  return out.map((l) => ({ text: l.text, range: l.range }));
}

test('the desktop highlights a hard-wrapped URL whole, from whichever row is hovered', () => {
  const term = fakeTerm(PROMPT_ECHO);
  const whole = [{ text: URL, range: { start: { x: 31, y: 2 }, end: { x: 36, y: 4 } } }];
  assert.deepEqual(links(term, 2), whole);
  assert.deepEqual(links(term, 3), whole);
  assert.deepEqual(links(term, 4), whole);
  assert.deepEqual(links(term, 1), []);
  assert.deepEqual(links(term, 5), []);
});

test('ordinary text is never glued onto a link', () => {
  const at = (rows) => {
    const lines = screenLines(rows.map(([text, wrapped = false], row) => ({ row, cells: [...text.padEnd(COLS)], wrapped })));
    return [...new Set(screenLinks(lines).flat().map((l) => l.text))];
  };
  const edge = (s) => s.padStart(COLS, 'x ');
  assert.equal(edge(' https://example.com/guide').length, COLS);
  assert.deepEqual(at([[edge(' https://example.com/guide')], ['  and more on the next row']]), ['https://example.com/guide'],
    'a URL short enough to fit a row was not broken by its wrapper: the next word is a word');
  assert.deepEqual(at([['see https://example.com/a/very/long/path/x'], ['  continues/here']]), ['https://example.com/a/very/long/path/x'],
    'two columns short of the edge is not a wrap');
  assert.deepEqual(at([['● See https://github.com/gigurra/tclaude/pul'], ['  https://example.com/other']]),
    ['https://github.com/gigurra/tclaude/pul', 'https://example.com/other'], 'a row that starts a URL of its own');
  assert.deepEqual(at([['● See https://github.com/gigurra/tclaude/pul'], [''], ['  l/4541/files']]),
    ['https://github.com/gigurra/tclaude/pul'], 'a blank row between');
  assert.deepEqual(at([['● See https://github.com/gigurra/tclaude/pul'], ['        l/4541/files']]),
    ['https://github.com/gigurra/tclaude/pul'], 'deeper than an indent');
  assert.deepEqual(at([['│ See https://github.com/gigurra/tclaude/p │'], ['│ ull/4541/files for it'.padEnd(COLS - 1) + '│']]),
    ['https://github.com/gigurra/tclaude/pull/4541/files'], 'a box\'s border and padding are allowed for, and stay out');
});
