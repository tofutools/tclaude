// phone-terminal-text.js -- a phone terminal's text: links and paths a tap
// follows, and Select, the screen as plain text to select and copy.
//
// xterm.js has no touch selection, and its links want a mouse hover and a
// Ctrl/Cmd-click, which a finger has neither of. So a phone reads the rows on
// screen itself (widget.screen(), terminal-interactions.js screenRows) and
// runs the desktop's own link rules on them:
//
//   a tap     on a link or a file path opens a sheet that shows where it
//             goes, then Open / Download or Copy; a tap elsewhere stays a tap
//   a hold    (HOLD_MS, still) opens Select with the word under the finger
//             picked; Select is on the key strip and in ⋯ too
//   Select    the screen as it was, as text the phone's own handles select,
//             links tappable as on the terminal, Copy selection / Copy screen
//
// What a phone links is what the desktop links from visible text: an http(s)
// URL, and an absolute path or file:// URL (visibleLocalFileLinks); and it is
// followed under the desktop's rules (widget.link / followLink: a file only
// where this view may download it). Under tmux the screen is all xterm holds,
// so Select has what is on screen (a swiped-back view included), not the
// session's whole history.

import { useBackLayer } from './back-layers.js';
import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { safeTerminalLink, trimPathPunctuation, visibleLocalFileLinks } from './terminal-interactions.js';
import { openPhoneSheet, phoneSheet } from './phone-sheet.js';

const html = htm.bind(h);

export const HOLD_MS = 500;
// A finger that moved further than this is a swipe, not a tap or a hold.
const SLOP_PX = 10;
// A tap is a touch lifted sooner than this.
const TAP_MS = 350;

// screenLines joins screen rows into logical lines: a soft-wrapped row
// continues the line before it, and a line's trailing padding is dropped.
// Each line keeps, per row, the text index each column starts at, so a cell
// maps to a place in its line (indexAt).
export function screenLines(rows) {
  const lines = [];
  for (const r of rows || []) {
    let line = r.wrapped ? lines[lines.length - 1] : null;
    if (!line) { line = { text: '', rows: [] }; lines.push(line); }
    const offsets = [];
    let text = '';
    for (const cell of r.cells) {
      offsets.push(line.text.length + text.length);
      text += cell;
    }
    line.rows.push({ row: r.row, offsets });
    line.text += text;
  }
  for (const line of lines) line.text = line.text.replace(/\s+$/, '');
  return lines;
}

// indexAt is where a cell ({ row, col }, widget.cellAt) falls in lines:
// { line, index }, or null past a line's end.
export function indexAt(lines, cell) {
  if (!cell) return null;
  for (let i = 0; i < lines.length; i++) {
    const at = lines[i].rows.find((r) => r.row === cell.row);
    if (!at) continue;
    const index = at.offsets[cell.col];
    return index === undefined || index >= lines[i].text.length ? null : { line: i, index };
  }
  return null;
}

const URL_RE = /https?:\/\/[^\s"'<>`]+/gi;

// linksIn is the links a line shows, in order: http(s) URLs as the desktop's
// web links take them (credentials refused, safeTerminalLink), and the file
// paths it recovers from plain cells (visibleLocalFileLinks). Each is
// { start, end, text, kind }.
export function linksIn(text) {
  const out = [];
  for (const match of String(text || '').matchAll(URL_RE)) {
    const raw = trimPathPunctuation(match[0]);
    if (safeTerminalLink(raw)?.kind === 'http') out.push({ start: match.index, end: match.index + raw.length, text: raw, kind: 'http' });
  }
  for (const file of visibleLocalFileLinks(text)) {
    if (!out.some((o) => file.start < o.end && o.start < file.end)) out.push({ ...file, kind: 'file' });
  }
  return out.sort((a, b) => a.start - b.start);
}

// linkAt is the link at a place in lines ({ line, index }), or null.
export function linkAt(lines, at) {
  if (!at) return null;
  return linksIn(lines[at.line]?.text).find((l) => at.index >= l.start && at.index < l.end) || null;
}

// wordAt is the run of non-space characters at index in text, as [start,
// end), or null on a space.
export function wordAt(text, index) {
  if (index < 0 || index >= text.length || /\s/.test(text[index])) return null;
  let start = index;
  let end = index + 1;
  while (start > 0 && !/\s/.test(text[start - 1])) start--;
  while (end < text.length && !/\s/.test(text[end])) end++;
  return [start, end];
}

// useTerminalTouch reports a tap (onTap, which says whether it opened
// something) and a still hold (onHold) on the
// element ref points at, each with the touch's client point. A second finger
// (a pinch) or a move past SLOP_PX (a swipe, terminals-core.js) is neither.
// It never cancels a touch the terminal's own gestures use: only the lift
// after a hold, or after a tap that opened something, which nothing else
// takes.
export function useTerminalTouch(ref, { onTap, onHold }) {
  const latest = useRef({ onTap, onHold });
  latest.current = { onTap, onHold };
  useEffect(() => {
    const el = ref.current;
    if (!el) return undefined;
    let touch = null;
    const cancel = () => {
      if (touch?.timer) clearTimeout(touch.timer);
      touch = null;
    };
    const start = (event) => {
      cancel();
      if (event.touches?.length !== 1) return;
      const { clientX: x, clientY: y } = event.touches[0];
      touch = { x, y, at: Date.now(), held: false };
      touch.timer = setTimeout(() => {
        if (!touch) return;
        touch.held = true;
        touch.timer = null;
        latest.current.onHold?.(x, y);
      }, HOLD_MS);
    };
    const move = (event) => {
      if (!touch) return;
      const t = event.touches?.[0];
      if (event.touches?.length !== 1 || !t || Math.abs(t.clientX - touch.x) > SLOP_PX || Math.abs(t.clientY - touch.y) > SLOP_PX) cancel();
    };
    const end = (event) => {
      if (!touch) return;
      const { x, y, at, held } = touch;
      cancel();
      // After a hold, the lift makes no mouse events: their mousedown would
      // land on Select, just opened under the finger, and drop the word it
      // picked.
      if (held) { event.preventDefault(); return; }
      // A tap that opened something (a link's sheet) makes no click either:
      // it would land on the sheet, just opened under the finger, and press
      // whatever is there (Download).
      if ((event.touches?.length ?? 0) === 0 && Date.now() - at < TAP_MS && latest.current.onTap?.(x, y)) event.preventDefault();
    };
    // A hold is Select here: not the browser's own long-press menu.
    const menu = (event) => event.preventDefault();
    const listeners = [['touchstart', start], ['touchmove', move], ['touchend', end], ['touchcancel', cancel], ['contextmenu', menu]];
    // touchend may cancel a held lift's mouse events (it scrolls nothing).
    for (const [type, fn] of listeners) el.addEventListener(type, fn, { passive: type !== 'contextmenu' && type !== 'touchend' });
    return () => {
      cancel();
      for (const [type, fn] of listeners) el.removeEventListener(type, fn);
    };
  }, []);
}

const baseName = (path) => path.split('/').filter(Boolean).pop() || path;

// LinkSheet is a link's sheet: where it goes, then what to do with it.
// widget follows it under the desktop's rules (a file only where this view
// may download it; otherwise Copy is all there is). copy(text, what) copies.
export function LinkSheet({ link, widget, copy, close }) {
  const target = widget?.link?.(link.text) || null;
  const file = link.kind === 'file';
  const run = (fn) => () => { close(); fn(); };
  const entry = (label, fn, sub = '') => html`<li><button data-ui-exempt type="button" class="phone-menu-item" onClick=${run(fn)}>
    <span class="phone-menu-label">${label}</span>${sub && html`<small class="phone-term-link-sub">${sub}</small>`}</button></li>`;
  return html`<div class="phone-term-link-sheet">
    <p class="phone-term-link-target">${target?.target || link.text}</p>
    <ul class="phone-menu">
      ${target && (file
        ? entry(`Download ${baseName(target.target)}`, () => widget.followLink(link.text))
        : entry('Open link ›', () => widget.followLink(link.text)))}
      ${entry(file ? 'Copy path' : 'Copy link', () => copy(link.text, file ? 'the path' : 'the link'))}
    </ul>
    ${!target && html`<p class="phone-sheet-meta">${file ? 'This view cannot download files from where the terminal runs.' : 'This link is not opened from here.'}</p>`}
  </div>`;
}

// openLinkSheet opens link's sheet.
export function openLinkSheet(link, { widget, copy }) {
  openPhoneSheet({ label: link.kind === 'file' ? baseName(link.text) : 'Link', Body: LinkSheet, props: { link, widget, copy } });
}

// Line is one line of Select: its text, with each link a tappable span (a
// span keeps the text selectable across it, as a button would not).
function Line({ text, index, onLink }) {
  const parts = [];
  let at = 0;
  for (const link of linksIn(text)) {
    if (link.start > at) parts.push(text.slice(at, link.start));
    parts.push(html`<span class="phone-term-link" role="link" tabindex="0" data-link=${link.kind}
      onClick=${() => onLink(link)} onKeyDown=${(e) => { if (e.key === 'Enter') onLink(link); }}>${link.text}</span>`);
    at = link.end;
  }
  if (at < text.length) parts.push(text.slice(at));
  return html`<div class="phone-term-text-line" data-line=${index}>${parts.length ? parts : '​'}</div>`;
}

// selectIn picks [start, end) of a rendered line's text, across its spans.
function selectIn(lineEl, [start, end]) {
  const doc = lineEl.ownerDocument;
  const selection = doc.getSelection?.();
  if (!selection || !doc.createRange) return;
  const walker = doc.createTreeWalker(lineEl, 4 /* NodeFilter.SHOW_TEXT */);
  const range = doc.createRange();
  let seen = 0;
  let begun = false;
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    const len = node.textContent.length;
    if (!begun && start < seen + len) { range.setStart(node, start - seen); begun = true; }
    if (begun && end <= seen + len) { range.setEnd(node, end - seen); break; }
    seen += len;
  }
  if (!begun) return;
  selection.removeAllRanges();
  selection.addRange(range);
}

// TerminalText is Select: lines (screenLines) as selectable text, picked
// ([line, start, end]) selected to begin with. onLink opens a link's sheet;
// copy(text, what) copies; onClose leaves.
export function TerminalText({ lines, picked = null, onLink, copy, onClose }) {
  const body = useRef(null);
  useBackLayer(true, () => onClose?.());
  const [selected, setSelected] = useState('');
  const doc = () => body.current?.ownerDocument;
  const selectionHere = () => {
    const selection = doc()?.getSelection?.();
    if (!selection || selection.isCollapsed || !body.current) return '';
    const node = selection.anchorNode;
    return node && body.current.contains(node) ? String(selection) : '';
  };
  useLayoutEffect(() => {
    if (!picked || !body.current) return;
    const lineEl = body.current.querySelector(`[data-line="${picked[0]}"]`);
    if (!lineEl) return;
    lineEl.scrollIntoView?.({ block: 'center' });
    selectIn(lineEl, [picked[1], picked[2]]);
    setSelected(selectionHere());
  }, []);
  useEffect(() => {
    const d = doc();
    if (!d) return undefined;
    const update = () => setSelected(selectionHere());
    d.addEventListener('selectionchange', update);
    return () => d.removeEventListener('selectionchange', update);
  }, []);
  useEffect(() => {
    const d = doc();
    // Escape is the sheet's while one is up over Select (a link's): this
    // listener came first, so the sheet is still open when it hears it.
    const key = (event) => { if (event.key === 'Escape' && !phoneSheet.peek()) onClose(); };
    d?.addEventListener('keydown', key);
    return () => d?.removeEventListener('keydown', key);
  }, []);
  const all = lines.map((l) => l.text).join('\n').replace(/\n+$/, '');
  return html`<div class="phone-term-text" role="dialog" aria-modal="true" aria-label="Select text">
    <header class="phone-term-bar">
      <button data-ui-exempt type="button" class="phone-bar-back" aria-label="Back to the terminal" onClick=${onClose}>‹</button>
      <h1 class="phone-bar-title">Select text</h1>
      <button data-ui-exempt type="button" class="phone-term-text-done" onClick=${onClose}>Done</button>
    </header>
    <div class="phone-term-text-body" ref=${body}>
      ${lines.map((line, i) => html`<${Line} key=${i} index=${i} text=${line.text} onLink=${onLink} />`)}
    </div>
    <div class="phone-term-text-foot">
      <button data-ui-exempt type="button" class="ui-btn phone-btn" disabled=${!all} onClick=${() => copy(all, 'the screen')}>Copy screen</button>
      <button data-ui-exempt type="button" class="ui-btn ui-btn-primary phone-btn" disabled=${!selected}
        onClick=${() => copy(selectionHere() || selected, 'the selection')}>Copy selection</button>
    </div>
  </div>`;
}
