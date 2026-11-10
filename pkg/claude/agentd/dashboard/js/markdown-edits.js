// markdown-edits.js -- what the Markdown editor's toolbar buttons do, as pure
// functions of the text and the selection (ported from awb's
// markdown-toolbar.ts). Each returns an edit, { changes: [{ from, to?, insert }],
// selection?: { anchor, head } }, that the editor applies: CodeMirror through
// a transaction, the plain-textarea fallback through applyEdit below. Keeping
// them free of any editor is what lets jstest/markdown-edits.test.mjs pin them.
//
// The dialect is what markdown-model.js renders: CommonMark with tables,
// strikethrough, autolinks and task lists. Raw HTML is shown as text.

// An ATX heading: up to three spaces, one to six `#`, then whitespace or the
// end of the line.
const HEADING = /^( {0,3})(#{1,6})(?:[ \t]+|$)/;

function lineStart(doc, pos) {
  return doc.lastIndexOf('\n', pos - 1) + 1;
}

function lineEnd(doc, pos) {
  const next = doc.indexOf('\n', pos);
  return next === -1 ? doc.length : next;
}

// lineStarts lists the start of every line from..to touches. A selection
// ending at a line start stops on the line before it.
function lineStarts(doc, from, to) {
  const first = lineStart(doc, from);
  const last = lineEnd(doc, to > from ? to - 1 : to);
  const starts = [first];
  for (let i = first; i < last; i += 1) if (doc[i] === '\n') starts.push(i + 1);
  return starts;
}

// blockPadding is the blank lines a block (fence, table, rule) needs around it
// to parse as one.
function blockPadding(doc, from, to) {
  const before = doc.slice(0, from);
  const after = doc.slice(to);
  let prefix = '';
  if (before.length > 0) prefix = before.endsWith('\n\n') ? '' : before.endsWith('\n') ? '\n' : '\n\n';
  let suffix = '\n\n';
  if (after.length === 0) suffix = '\n';
  else if (after.startsWith('\n\n')) suffix = '';
  else if (after.startsWith('\n')) suffix = '\n';
  return { prefix, suffix };
}

const cursor = (at) => ({ anchor: at, head: at });
const range = (from, to) => ({ anchor: from, head: to });

// wrapEdit surrounds the selection with an inline marker, or unwraps it when
// it is already wrapped; with nothing selected it opens an empty pair.
export function wrapEdit(doc, from, to, marker) {
  const n = marker.length;
  if (from === to) {
    return { changes: [{ from, insert: `${marker}${marker}` }], selection: cursor(from + n) };
  }
  const selected = doc.slice(from, to);
  if (selected.length >= 2 * n && selected.startsWith(marker) && selected.endsWith(marker)) {
    const inner = selected.slice(n, selected.length - n);
    return { changes: [{ from, to, insert: inner }], selection: range(from, from + inner.length) };
  }
  if (doc.slice(from - n, from) === marker && doc.slice(to, to + n) === marker) {
    return { changes: [{ from: from - n, to: from, insert: '' }, { from: to, to: to + n, insert: '' }], selection: range(from - n, to - n) };
  }
  const insert = `${marker}${selected}${marker}`;
  return { changes: [{ from, to, insert }], selection: range(from + n, from + n + selected.length) };
}

// LIST_MARKER is a bullet, numbered or task-list marker at a line's start.
const LIST_MARKER = /^(\s*)(?:[-*+]|\d{1,9}[.)])(?:\s+\[[ xX]\])?\s+/;

// linePrefixEdit puts a marker in front of every line the selection touches
// (bullet list, task list, blockquote). When every touched line already has
// it, it takes it off again. A list marker replaces another list marker, so
// a bullet becomes a task without stacking.
export function linePrefixEdit(doc, from, to, prefix) {
  const starts = lineStarts(doc, from, to);
  const lines = starts.map((start) => doc.slice(start, lineEnd(doc, start)));
  const nonBlank = starts.filter((_, i) => lines[i].trim() !== '');
  // A task line does not count as a bullet: the bullet button turns it into
  // one rather than stripping its dash.
  const has = (line) => {
    const body = line.trimStart();
    return body.startsWith(prefix) && (prefix !== '- ' || !/^- \[[ xX]\]\s/.test(body));
  };
  if (nonBlank.length && starts.every((start, i) => lines[i].trim() === '' || has(lines[i]))) {
    return {
      changes: starts.flatMap((start, i) => {
        if (!has(lines[i])) return [];
        const indent = lines[i].length - lines[i].trimStart().length;
        return [{ from: start + indent, to: start + indent + prefix.length, insert: '' }];
      }),
    };
  }
  const list = /^[-*+]|^- \[/.test(prefix);
  return {
    changes: starts.flatMap((start, i) => {
      const line = lines[i];
      if (starts.length > 1 && line.trim() === '') return [];
      const marker = list ? LIST_MARKER.exec(line) : null;
      if (marker) return [{ from: start + marker[1].length, to: start + marker[0].length, insert: prefix }];
      return [{ from: start, insert: prefix }];
    }),
  };
}

// numberedListEdit numbers the lines the selection touches from one.
export function numberedListEdit(doc, from, to) {
  const starts = lineStarts(doc, from, to);
  let n = 0;
  return {
    changes: starts.flatMap((start) => {
      const line = doc.slice(start, lineEnd(doc, start));
      if (starts.length > 1 && line.trim() === '') return [];
      n += 1;
      const marker = LIST_MARKER.exec(line);
      if (marker) return [{ from: start + marker[1].length, to: start + marker[0].length, insert: `${n}. ` }];
      return [{ from: start, insert: `${n}. ` }];
    }),
  };
}

// headingEdit sets (or with level 0 clears) the heading marker on the lines
// the selection touches, replacing one that is there.
export function headingEdit(doc, from, to, level) {
  const insert = level > 0 ? `${'#'.repeat(level)} ` : '';
  return {
    changes: lineStarts(doc, from, to).map((start) => {
      const existing = HEADING.exec(doc.slice(start, lineEnd(doc, start)));
      if (existing === null) return { from: start, to: start, insert };
      return { from: start + existing[1].length, to: start + existing[0].length, insert };
    }),
  };
}

// headingLevel is a line's heading level, 0 when it is not a heading.
export function headingLevel(line) {
  const existing = HEADING.exec(line);
  return existing === null ? 0 : existing[2].length;
}

// tableEdit seeds a table and selects its first header so it can be typed over.
export function tableEdit(doc, from, to) {
  const { prefix, suffix } = blockPadding(doc, from, to);
  const header = 'Column 1';
  const body = `| ${header} | Column 2 |\n| --- | --- |\n| Cell | Cell |`;
  const headerStart = from + prefix.length + 2;
  return { changes: [{ from, to, insert: `${prefix}${body}${suffix}` }], selection: range(headerStart, headerStart + header.length) };
}

// closingFenceLength is the longest backtick fence line in text.
function closingFenceLength(text) {
  let longest = 0;
  for (const line of text.split('\n')) {
    const fence = /^ {0,3}(`+)[ \t]*$/.exec(line);
    if (fence !== null) longest = Math.max(longest, fence[1].length);
  }
  return longest;
}

// codeBlockEdit fences the selection (with a fence longer than any inside
// it), or opens an empty fence with the cursor inside.
export function codeBlockEdit(doc, from, to) {
  const selected = doc.slice(from, to);
  const { prefix, suffix } = blockPadding(doc, from, to);
  const fence = '`'.repeat(Math.max(3, closingFenceLength(selected) + 1));
  const insert = `${prefix}${fence}\n${selected}\n${fence}${suffix}`;
  const selection = selected === '' ? cursor(from + prefix.length + fence.length + 1) : cursor(from + insert.length);
  return { changes: [{ from, to, insert }], selection };
}

// ruleEdit inserts a horizontal rule on a line of its own.
export function ruleEdit(doc, from, to) {
  const { prefix, suffix } = blockPadding(doc, from, to);
  const insert = `${prefix}---${suffix}`;
  return { changes: [{ from, to, insert }], selection: cursor(from + insert.length) };
}

function escapeLinkLabel(text) {
  return text.replace(/[\\[\]]/g, '\\$&');
}

const URLISH = /^(?:https?:\/\/|mailto:)\S+$/i;

// linkEdit writes a link. The selection becomes its text and the URL
// placeholder is selected to type over; a selected URL becomes the target
// instead, with the text selected.
export function linkEdit(doc, from, to) {
  const selected = doc.slice(from, to);
  if (URLISH.test(selected.trim())) {
    const insert = `[text](${selected.trim()})`;
    return { changes: [{ from, to, insert }], selection: range(from + 1, from + 5) };
  }
  const text = selected === '' ? 'text' : escapeLinkLabel(selected);
  const destination = 'https://';
  const insert = `[${text}](${destination})`;
  const textStart = from + 1;
  const selection = selected === ''
    ? range(textStart, textStart + text.length)
    : cursor(textStart + text.length + 2 + destination.length);
  return { changes: [{ from, to, insert }], selection };
}

// fileDestination is a file name as a link destination: in angle brackets
// when it holds a space or a bracket, which markdown-it reads as one name.
export function fileDestination(name) {
  const value = String(name || '');
  return /[\s()<>]/.test(value) ? `<${value.replace(/[<>]/g, '')}>` : value;
}

const IMAGE_NAME = /\.(?:png|jpe?g|gif|webp|avif)$/i;

// fileLinksEdit inserts a reference to each file added to the task: an image
// shows inline (`![name](name)`), anything else is a link to its name. The
// renderer resolves the name against the task's files.
export function fileLinksEdit(doc, from, to, names) {
  const parts = (names || []).map((name) => {
    const label = escapeLinkLabel(String(name).replace(/\.[^.]+$/, ''));
    return IMAGE_NAME.test(name) ? `![${label}](${fileDestination(name)})` : `[${escapeLinkLabel(String(name))}](${fileDestination(name)})`;
  });
  if (!parts.length) return { changes: [] };
  const before = doc.slice(0, from);
  const lead = before === '' || before.endsWith('\n') ? '' : '\n';
  const insert = `${lead}${parts.join('\n')}\n`;
  return { changes: [{ from, to, insert }], selection: cursor(from + insert.length) };
}

// applyEdit applies an edit to a string: the textarea fallback's editor and
// the tests' oracle. Changes refer to the original text, as in CodeMirror.
export function applyEdit(doc, edit) {
  const changes = [...edit.changes].map((c) => ({ from: c.from, to: c.to ?? c.from, insert: c.insert }))
    .sort((a, b) => b.from - a.from);
  let out = doc;
  for (const c of changes) out = out.slice(0, c.from) + c.insert + out.slice(c.to);
  return out;
}

// mapPos maps a position in the original text through an edit's changes.
export function mapPos(pos, edit) {
  let delta = 0;
  for (const c of edit.changes) {
    const to = c.to ?? c.from;
    if (to <= pos) delta += c.insert.length - (to - c.from);
  }
  return pos + delta;
}

// TOOLS are the toolbar's buttons, in order; null is a separator. `compact`
// ones also show on a comment's smaller toolbar. `key` is the shortcut the
// editor binds (Mod is Ctrl, or Cmd on a Mac).
export const TOOLS = [
  { id: 'bold', label: 'Bold', key: 'Mod-b', compact: true, edit: (d, f, t) => wrapEdit(d, f, t, '**') },
  { id: 'italic', label: 'Italic', key: 'Mod-i', compact: true, edit: (d, f, t) => wrapEdit(d, f, t, '*') },
  { id: 'code', label: 'Inline code', key: 'Mod-e', compact: true, edit: (d, f, t) => wrapEdit(d, f, t, '`') },
  { id: 'strike', label: 'Strikethrough', edit: (d, f, t) => wrapEdit(d, f, t, '~~') },
  null,
  { id: 'bullets', label: 'Bullet list', compact: true, edit: (d, f, t) => linePrefixEdit(d, f, t, '- ') },
  { id: 'numbers', label: 'Numbered list', edit: numberedListEdit },
  { id: 'tasks', label: 'Task list', compact: true, edit: (d, f, t) => linePrefixEdit(d, f, t, '- [ ] ') },
  null,
  { id: 'quote', label: 'Quote', compact: true, edit: (d, f, t) => linePrefixEdit(d, f, t, '> ') },
  { id: 'codeblock', label: 'Code block', compact: true, edit: codeBlockEdit },
  { id: 'table', label: 'Table', edit: tableEdit },
  { id: 'rule', label: 'Horizontal rule', edit: ruleEdit },
  null,
  { id: 'link', label: 'Link', key: 'Mod-k', compact: true, edit: linkEdit },
];

// ICONS are the toolbar's icons on a 24-unit stroked grid, as element specs
// ([tag, attrs]) the editor builds as vnodes: no markup string.
export const ICONS = {
  bold: [['path', { d: 'M7 5h6a3.5 3.5 0 0 1 0 7H7z' }], ['path', { d: 'M7 12h7a3.5 3.5 0 0 1 0 7H7z' }]],
  italic: [['path', { d: 'M15 5h-5M14 19H9M13 5l-2 14' }]],
  code: [['path', { d: 'm15 8 4 4-4 4M9 8l-4 4 4 4' }]],
  strike: [['path', { d: 'M16 6a4 4 0 0 0-4-2c-2.2 0-4 1.2-4 3 0 1.3.9 2.2 2.4 2.8' }], ['path', { d: 'M13.5 14c1.5.6 2.5 1.4 2.5 2.8 0 1.8-1.8 3.2-4 3.2a4 4 0 0 1-4-2' }], ['path', { d: 'M4 12h16' }]],
  bullets: [['path', { d: 'M9 6h11M9 12h11M9 18h11' }], ['path', { d: 'M5 6h.01M5 12h.01M5 18h.01' }]],
  numbers: [['path', { d: 'M10 6h10M10 12h10M10 18h10' }], ['path', { d: 'M4 4.5 5.5 4v4' }], ['path', { d: 'M4 11h2l-2 3h2' }], ['path', { d: 'M4 17h2v3H4' }]],
  tasks: [['path', { d: 'M11 6h9M11 12h9M11 18h9' }], ['path', { d: 'm4 6 1.5 1.5L8 5' }], ['path', { d: 'm4 17 1.5 1.5L8 15' }]],
  quote: [['path', { d: 'M6 5v14' }], ['path', { d: 'M11 8h9M11 12h9M11 16h6' }]],
  codeblock: [['rect', { x: '3', y: '4', width: '18', height: '16', rx: '2' }], ['path', { d: 'm10 10-2 2 2 2M14 14l2-2-2-2' }]],
  table: [['rect', { x: '3', y: '4', width: '18', height: '16', rx: '2' }], ['path', { d: 'M3 10h18M9 10v10' }]],
  rule: [['path', { d: 'M4 12h16' }]],
  link: [['path', { d: 'M10 13a5 5 0 0 0 7.1 0l2-2A5 5 0 0 0 12 3.9L10.9 5' }], ['path', { d: 'M14 11a5 5 0 0 0-7.1 0l-2 2A5 5 0 0 0 12 20.1l1.1-1.1' }]],
  attach: [['path', { d: 'm21 11-8.5 8.5a5 5 0 0 1-7-7L14 4a3.5 3.5 0 0 1 5 5l-8.5 8.5a2 2 0 0 1-3-3L15 7' }]],
  help: [['circle', { cx: '12', cy: '12', r: '9' }], ['path', { d: 'M9.6 9.5a2.5 2.5 0 1 1 3.4 2.3c-.6.3-1 .8-1 1.4v.3' }], ['path', { d: 'M12 17h.01' }]],
};

// HELP is the syntax reference behind the toolbar's ? button.
export const HELP = [
  ['# Heading', 'Heading; ## to ###### for smaller ones'],
  ['**bold**  *italic*  ~~struck~~', 'Emphasis'],
  ['`code`', 'Inline code'],
  ['- item  ·  1. item', 'Lists; indent two spaces (or Tab) to nest'],
  ['- [ ] to do  ·  - [x] done', 'Task list'],
  ['> quoted', 'Quote'],
  ['```\ncode\n```', 'Code block'],
  ['| A | B |\n| --- | --- |', 'Table'],
  ['[text](https://…)', 'Link; a bare URL links itself'],
  ['![alt](file.png)', 'An image among the task\'s files, by its name'],
];
