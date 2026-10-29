// companion-annotate.js -- freehand annotation over the dashboard
// (tcl-8vjjko): the operator draws on top of whatever view they are on and
// sends the marked-up view to the companion agent. Agents cannot see the
// operator's screen, so each stroke is sent as words: what kind of mark it is
// (circled, underlined, an arrow, a mark) and which control or text it lands
// on, read from the page under the stroke.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { TARGET_SELECTOR, textOf } from './companion-guide.js';

const html = htm.bind(h);

export const ANNOTATE_COLORS = Object.freeze(['#ff4d4f', '#ffd43b', '#4dabf7', '#69db7c']);
const LABEL_MAX = 80;

function bounds(points) {
  let minX = Infinity; let minY = Infinity; let maxX = -Infinity; let maxY = -Infinity;
  for (const [x, y] of points) {
    minX = Math.min(minX, x); minY = Math.min(minY, y); maxX = Math.max(maxX, x); maxY = Math.max(maxY, y);
  }
  return { x: minX, y: minY, width: maxX - minX, height: maxY - minY };
}

// classifyStroke names what a stroke (viewport points) most looks like.
export function classifyStroke(points) {
  if (!points?.length) return 'mark';
  const b = bounds(points);
  const span = Math.max(b.width, b.height);
  const [sx, sy] = points[0];
  const [ex, ey] = points[points.length - 1];
  const gap = Math.hypot(ex - sx, ey - sy);
  if (points.length >= 8 && span > 24 && gap < span * 0.35 && Math.min(b.width, b.height) > 10) return 'circle';
  if (b.width > 30 && b.height < b.width * 0.25 && gap > b.width * 0.6) return 'underline';
  if (gap > 40) return 'arrow';
  return 'mark';
}

function roleOf(el) {
  const tag = el.tagName?.toLowerCase();
  if (tag === 'button' || el.getAttribute?.('role') === 'button' || el.getAttribute?.('role') === 'tab') return 'button';
  if (tag === 'a') return 'link';
  if (tag === 'input' || tag === 'textarea' || tag === 'select') return 'field';
  if (/^h[1-4]$/.test(tag || '')) return 'heading';
  if (tag === 'th') return 'column';
  return '';
}

// labelFor names an element the way the operator sees it: its own short text,
// else the nearest ancestor with short text (a cell, a card title, a button).
export function labelFor(el) {
  let node = el;
  for (let depth = 0; node && depth < 6; depth++, node = node.parentElement) {
    if (node.tagName === 'BODY' || node.tagName === 'HTML') break;
    const { label, text } = textOf(node);
    const field = ['INPUT', 'TEXTAREA'].includes(node.tagName) ? (node.value || node.getAttribute?.('placeholder') || '') : '';
    const name = label || field || text;
    if (name && name.length <= LABEL_MAX) {
      const role = roleOf(node) || (node.closest?.(TARGET_SELECTOR) ? roleOf(node.closest(TARGET_SELECTOR)) : '');
      return { text: name, role };
    }
  }
  const fallback = el ? textOf(el).text : '';
  return fallback ? { text: `${fallback.slice(0, LABEL_MAX - 1)}…`, role: '' } : null;
}

// elementAt returns the page element at a viewport point, skipping the
// annotation layer and the companion itself.
function elementAt(doc, x, y, skip) {
  for (const el of doc.elementsFromPoint?.(x, y) || []) {
    if (skip && el.closest?.(skip)) continue;
    return el;
  }
  return null;
}

function quote(found) {
  if (!found) return 'empty space';
  return `“${found.text}”${found.role ? ` (${found.role})` : ''}`;
}

// describeStroke turns one stroke into a line for the agent. `at(x, y)`
// resolves a viewport point to a label (labelFor of the element there).
export function describeStroke(points, at) {
  const kind = classifyStroke(points);
  const b = bounds(points);
  const cx = b.x + b.width / 2; const cy = b.y + b.height / 2;
  if (kind === 'circle') return `circled ${quote(at(cx, cy))}`;
  if (kind === 'underline') return `underlined ${quote(at(cx, b.y - 8) || at(cx, cy))}`;
  if (kind === 'arrow') {
    const [sx, sy] = points[0];
    const [ex, ey] = points[points.length - 1];
    return `drew an arrow from ${quote(at(sx, sy))} to ${quote(at(ex, ey))}`;
  }
  return `marked ${quote(at(cx, cy))}`;
}

// composeAnnotation builds the mail body: the note, then one numbered line
// per stroke, matching the numbers drawn on the overlay.
export function composeAnnotation({ note = '', lines = [], view = '' }) {
  const parts = [];
  const clean = String(note || '').trim();
  if (clean) parts.push(clean);
  if (lines.length) {
    parts.push(`The operator marked up the dashboard view ${view || 'they are on'}:\n${lines.map((line, i) => `${i + 1}. ${line}`).join('\n')}`);
  }
  return parts.join('\n\n');
}

function strokePath(points) {
  return points.length ? `M${points.map(([x, y]) => `${x},${y}`).join(' L')}` : '';
}

// arrowHead draws a head at a stroke's end, so the operator sees that the
// stroke will be read as an arrow.
function arrowHead(points) {
  const [ex, ey] = points[points.length - 1];
  const back = points[Math.max(0, points.length - 4)];
  const angle = Math.atan2(ey - back[1], ex - back[0]);
  const [l, r] = [angle - 0.5, angle + 0.5].map((a) => `${ex - 14 * Math.cos(a)},${ey - 14 * Math.sin(a)}`);
  return `M${l} L${ex},${ey} L${r}`;
}

// AnnotateOverlay is the drawing layer over the whole page.
export function AnnotateOverlay({ agentName, onSend, onCancel, win = globalThis.window }) {
  const [strokes, setStrokes] = useState([]);
  const [color, setColor] = useState(ANNOTATE_COLORS[0]);
  const [note, setNote] = useState('');
  const [sending, setSending] = useState(false);
  const drawing = useRef(null);
  const rootRef = useRef(null);
  const [, setTick] = useState(0);

  useEffect(() => {
    const onKey = (event) => {
      if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); onCancel(); }
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'z' && !event.target?.closest?.('textarea')) {
        event.preventDefault();
        setStrokes((list) => list.slice(0, -1));
      }
    };
    win?.addEventListener?.('keydown', onKey, true);
    return () => win?.removeEventListener?.('keydown', onKey, true);
  }, [onCancel, win]);

  function down(event) {
    if (event.button !== 0) return;
    event.currentTarget.setPointerCapture?.(event.pointerId);
    drawing.current = { color, points: [[event.clientX, event.clientY]] };
    setTick((n) => n + 1);
  }
  function move(event) {
    const d = drawing.current;
    if (!d) return;
    const [lx, ly] = d.points[d.points.length - 1];
    if (Math.hypot(event.clientX - lx, event.clientY - ly) < 2) return;
    d.points.push([event.clientX, event.clientY]);
    setTick((n) => n + 1);
  }
  function up() {
    const d = drawing.current;
    drawing.current = null;
    if (d && d.points.length > 1) setStrokes((list) => [...list, d]);
    else setTick((n) => n + 1);
  }

  async function send(event) {
    event?.preventDefault?.();
    if (sending || (!strokes.length && !note.trim())) return;
    const doc = win?.document;
    const root = rootRef.current;
    // Look through the layer at the page underneath.
    if (root) root.style.pointerEvents = 'none';
    let lines;
    try {
      const at = (x, y) => {
        const el = doc && elementAt(doc, x, y, '.annotate-root, #companion-root');
        return el ? labelFor(el) : null;
      };
      lines = strokes.map((stroke) => describeStroke(stroke.points, at));
    } finally {
      if (root) root.style.pointerEvents = '';
    }
    setSending(true);
    try {
      await onSend({ note, lines });
    } finally {
      setSending(false);
    }
  }

  const live = drawing.current ? [...strokes, drawing.current] : strokes;
  return html`<div class="annotate-root" ref=${rootRef}>
    <svg class="annotate-canvas" onPointerDown=${down} onPointerMove=${move} onPointerUp=${up} onPointerCancel=${up}
      aria-label="Annotation layer: draw on the page">
      ${live.map((stroke, i) => {
        const [x, y] = stroke.points[0];
        return html`<g key=${i}>
          <path d=${strokePath(stroke.points)} stroke=${stroke.color} />
          ${i < strokes.length && classifyStroke(stroke.points) === 'arrow' && html`<path d=${arrowHead(stroke.points)} stroke=${stroke.color} />`}
          ${i < strokes.length && html`<circle cx=${x} cy=${y} r="9" fill=${stroke.color} /><text x=${x} y=${y + 4}>${i + 1}</text>`}
        </g>`;
      })}
    </svg>
    <form class="annotate-bar" onSubmit=${send}>
      <span class="annotate-title">✎ Mark up this view for <b>${agentName}</b>: circle, underline or point with an arrow</span>
      ${ANNOTATE_COLORS.map((c) => html`<button key=${c} type="button" class=${`annotate-swatch${color === c ? ' on' : ''}`}
        style=${`background:${c}`} aria-label=${`Pen ${c}`} aria-pressed=${color === c ? 'true' : 'false'} onClick=${() => setColor(c)}></button>`)}
      <button type="button" class="annotate-btn" title="Undo (Ctrl+Z)" disabled=${!strokes.length} onClick=${() => setStrokes((list) => list.slice(0, -1))}>↶</button>
      <textarea class="annotate-note" rows="1" placeholder="Add a note (optional)" aria-label="Note" value=${note}
        onInput=${(event) => setNote(event.currentTarget.value)}
        onKeyDown=${(event) => { if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) send(event); }}></textarea>
      <button type="submit" class="annotate-send" disabled=${sending || (!strokes.length && !note.trim())}>${sending ? 'Sending…' : 'Send'}</button>
      <button type="button" class="annotate-btn" onClick=${onCancel} title="Cancel (Esc)">Cancel</button>
    </form>
  </div>`;
}
