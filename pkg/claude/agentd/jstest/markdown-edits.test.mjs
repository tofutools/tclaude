import test from 'node:test';
import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';

// markdown-edits.js imports nothing, so it loads straight from the dashboard
// tree. Each case applies the edit to the text the way the editor does and
// checks the text and the selection it leaves.
const edits = await import(pathToFileURL(new URL('../dashboard/js/markdown-edits.js', import.meta.url).pathname).href);
const { applyEdit } = edits;

// run applies fn to doc with the selection marked by [ and ] (or a lone |),
// and returns the result with the new selection marked the same way.
function run(marked, fn) {
  let doc = marked;
  let from;
  let to;
  if (doc.includes('|')) {
    from = to = doc.indexOf('|');
    doc = doc.replace('|', '');
  } else {
    from = doc.indexOf('[[');
    doc = doc.replace('[[', '');
    to = doc.indexOf(']]');
    doc = doc.replace(']]', '');
  }
  const edit = fn(doc, from, to);
  const out = applyEdit(doc, edit);
  // An edit with no selection leaves the editor to map the old one through it.
  const sel = edit.selection || { anchor: edits.mapPos(from, edit), head: edits.mapPos(to, edit) };
  const a = Math.min(sel.anchor, sel.head);
  const b = Math.max(sel.anchor, sel.head);
  return a === b ? `${out.slice(0, a)}|${out.slice(a)}` : `${out.slice(0, a)}[[${out.slice(a, b)}]]${out.slice(b)}`;
}

// text is run without the selection: for line edits only the text matters.
const text = (marked, fn) => run(marked, fn).replace(/\[\[|\]\]|\|/g, '');

test('bold wraps the selection, opens an empty pair, and unwraps again', () => {
  const bold = (d, f, t) => edits.wrapEdit(d, f, t, '**');
  assert.equal(run('a [[word]] b', bold), 'a **[[word]]** b');
  assert.equal(run('a | b', bold), 'a **|** b');
  assert.equal(run('a **[[word]]** b', bold), 'a [[word]] b', 'markers around the selection come off');
  assert.equal(run('a [[**word**]] b', bold), 'a [[word]] b', 'markers inside the selection come off');
});

test('list buttons prefix every touched line, swap one list kind for another, and toggle off', () => {
  const bullets = (d, f, t) => edits.linePrefixEdit(d, f, t, '- ');
  const tasks = (d, f, t) => edits.linePrefixEdit(d, f, t, '- [ ] ');
  assert.equal(text('[[one\ntwo]]', bullets), '- one\n- two');
  assert.equal(text('[[- one\n- two]]', bullets), 'one\ntwo', 'all bulleted: the bullets come off');
  assert.equal(text('[[- one\n- two]]', tasks), '- [ ] one\n- [ ] two', 'a bullet becomes a task, not "- [ ] - one"');
  assert.equal(text('[[- [ ] one]]', bullets), '- one', 'a task line becomes a bullet rather than losing its dash');
  assert.equal(text('[[one\n\ntwo]]', bullets), '- one\n\n- two', 'blank lines between are left blank');
  assert.equal(text('[[1. one\n- two]]', edits.numberedListEdit), '1. one\n2. two');
  assert.equal(text('[[a\nb]]', (d, f, t) => edits.linePrefixEdit(d, f, t, '> ')), '> a\n> b');
});

test('headings replace an existing marker instead of stacking one', () => {
  assert.equal(run('Ti|tle', (d, f, t) => edits.headingEdit(d, f, t, 2)), '## Ti|tle');
  assert.equal(run('# Ti|tle', (d, f, t) => edits.headingEdit(d, f, t, 3)), '### Ti|tle');
  assert.equal(run('### Ti|tle', (d, f, t) => edits.headingEdit(d, f, t, 0)), 'Ti|tle');
  assert.equal(edits.headingLevel('## x'), 2);
  assert.equal(edits.headingLevel('#x'), 0);
});

test('blocks get the blank lines they need to parse as blocks', () => {
  assert.equal(run('para|', edits.ruleEdit), 'para\n\n---\n|');
  assert.equal(run('para|', edits.codeBlockEdit), 'para\n\n```\n|\n```\n');
  assert.equal(run('[[has ``` inside]]', edits.codeBlockEdit).startsWith('```\nhas'), true);
  assert.equal(run('[[```\nx\n```]]', edits.codeBlockEdit).startsWith('````\n'), true, 'a longer fence holds a fence');
  assert.equal(run('|', edits.tableEdit), '| [[Column 1]] | Column 2 |\n| --- | --- |\n| Cell | Cell |\n');
});

test('a link takes the selection as its text, or a selected URL as its target', () => {
  assert.equal(run('see [[the docs]]', edits.linkEdit), 'see [the docs](https://|)');
  assert.equal(run('see [[https://x.io/a]]', edits.linkEdit), 'see [[[text]]](https://x.io/a)');
  assert.equal(run('|', edits.linkEdit), '[[[text]]](https://)');
  assert.equal(run('[[a]b]]', edits.linkEdit), '[a\\]b](https://|)', 'a bracket in the text is escaped');
});

test('files are linked by name: images inline, others as links, odd names in angle brackets', () => {
  assert.equal(run('Look:|', (d, f, t) => edits.fileLinksEdit(d, f, t, ['shot.png', 'my log.txt'])),
    'Look:\n![shot](shot.png)\n[my log.txt](<my log.txt>)\n|');
  assert.equal(run('|', (d, f, t) => edits.fileLinksEdit(d, f, t, ['a.PNG'])), '![a](a.PNG)\n|');
});

test('applyEdit and mapPos treat changes as positions in the original text', () => {
  const edit = { changes: [{ from: 0, insert: '- ' }, { from: 4, insert: '- ' }] };
  assert.equal(applyEdit('one\ntwo', edit), '- one\n- two');
  assert.equal(edits.mapPos(5, edit), 9);
});
