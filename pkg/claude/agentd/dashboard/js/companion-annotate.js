// companion-annotate.js -- freehand annotation over the dashboard
// (tcl-8vjjko): the operator draws on top of whatever view they are on and
// sends the marked-up view to the companion agent. Agents cannot see the
// operator's screen, so the message carries an image of the view with the ink
// on it, and each stroke as words: what kind of mark it is (circled, underlined,
// an arrow, a mark, a drawing) and which control or text it lands on, read from
// the page under the stroke.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { COMPANION_CHROME, TARGET_SELECTOR, textOf } from './companion-guide.js';
import { terminalRow } from './dashboard-capture.js';

const html = htm.bind(h);

export const ANNOTATE_COLORS = Object.freeze(['#ff4d4f', '#ffd43b', '#4dabf7', '#69db7c']);
const LABEL_MAX = 80;
export const ANNOTATE_NOTE_MAX = 2000;
// A stroke this much longer than the distance it covers wanders, so it is no
// arrow: a half circle, the most a curved arrow bends, is 0.64.
const ARROW_STRAIGHTNESS = 0.6;
const DRAWING_MIN_LENGTH = 60;
// Handwriting is a run of strokes each close to the last in time and on screen.
const WRITING_STROKES = 3;
const WRITING_PAUSE_MS = 1500;
const WRITING_REACH = 80;

function bounds(points) {
  let minX = Infinity; let minY = Infinity; let maxX = -Infinity; let maxY = -Infinity;
  for (const [x, y] of points) {
    minX = Math.min(minX, x); minY = Math.min(minY, y); maxX = Math.max(maxX, x); maxY = Math.max(maxY, y);
  }
  return { x: minX, y: minY, width: maxX - minX, height: maxY - minY };
}

function pathLength(points) {
  return points.slice(1).reduce((sum, [x, y], i) => sum + Math.hypot(x - points[i][0], y - points[i][1]), 0);
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
  const length = pathLength(points);
  if (gap > 40 && gap / length >= ARROW_STRAIGHTNESS) return 'arrow';
  return length >= DRAWING_MIN_LENGTH ? 'drawing' : 'mark';
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

// holdsMasked says an element's text may hold a secret marked for capture, which
// an annotation carries to the agent just as a capture does.
function holdsMasked(node) {
  return !!(node.closest?.('[data-capture-mask]') || node.querySelector?.('[data-capture-mask]'));
}

// labelFor names an element the way the operator sees it: its own short text,
// else the nearest ancestor with short text (a cell, a card title, a button).
export function labelFor(el) {
  let node = el;
  for (let depth = 0; node && depth < 6; depth++, node = node.parentElement) {
    if (node.tagName === 'BODY' || node.tagName === 'HTML') break;
    if (holdsMasked(node)) continue;
    const { label, text } = textOf(node);
    // A field is named by its label or placeholder, never by what is typed
    // in it (a token, a password).
    const field = ['INPUT', 'TEXTAREA', 'SELECT'].includes(node.tagName)
      ? ((node.labels?.[0]?.textContent || '').replace(/\s+/g, ' ').trim() || node.getAttribute?.('placeholder') || node.getAttribute?.('name') || '')
      : '';
    const name = label || field || text;
    if (name && name.length <= LABEL_MAX) {
      const role = roleOf(node) || (node.closest?.(TARGET_SELECTOR) ? roleOf(node.closest(TARGET_SELECTOR)) : '');
      return { text: name, role };
    }
  }
  if (['INPUT', 'TEXTAREA', 'SELECT'].includes(el?.tagName)) return { text: 'an unlabelled field', role: 'field' };
  const fallback = el && !holdsMasked(el) ? textOf(el).text : '';
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

// terminalLineAt labels a point in a terminal by the line it is on: the whole
// terminal is one element, whose text would name every point alike. A blank
// line is null, so it reads as empty space.
function terminalLineAt(el, y) {
  const row = terminalRow(el, y);
  if (!row) return undefined;
  const text = (row.textContent || '').replace(/\s+/g, ' ').trim().slice(0, LABEL_MAX);
  return text ? { text, role: '' } : null;
}

// resolveStroke turns one stroke into {kind, line, drawn} for the agent, drawn
// being its line should it turn out to be handwriting. `at(x, y)` resolves a
// viewport point to a label (labelFor of the element there).
export function resolveStroke(points, at) {
  const b = bounds(points);
  const cx = b.x + b.width / 2; const cy = b.y + b.height / 2;
  const drawn = `drew by hand over ${quote(at(cx, cy))}`;
  return { ...readStroke(points, at, b, cx, cy), drawn };
}

function readStroke(points, at, b, cx, cy) {
  let kind = classifyStroke(points);
  if (kind === 'drawing') return { kind, line: `drew by hand over ${quote(at(cx, cy))}` };
  const [sx, sy] = points[0];
  const [ex, ey] = points[points.length - 1];
  if (kind === 'circle') return { kind, line: `circled ${quote(at(cx, cy))}` };
  if (kind === 'underline') {
    const above = at(cx, b.y - 8) || at(cx, cy);
    const end = at(ex, ey);
    // A flat stroke ending on a control that is not the text above it is a
    // horizontal arrow pointing at that control.
    if (!(end?.role && end.text !== above?.text)) return { kind, line: `underlined ${quote(above)}` };
    kind = 'arrow';
  }
  if (kind === 'arrow') return { kind, line: `drew an arrow from ${quote(at(sx, sy))} to ${quote(at(ex, ey))}` };
  return { kind, line: `marked ${quote(at(cx, cy))}` };
}

export function describeStroke(points, at) {
  return resolveStroke(points, at).line;
}

function continuesWriting(previous, next) {
  if (!(next.startedAt - previous.endedAt < WRITING_PAUSE_MS)) return false;
  const a = bounds(previous.points);
  const b = bounds(next.points);
  return b.x - (a.x + a.width) < WRITING_REACH && a.x - (b.x + b.width) < WRITING_REACH
    && b.y - (a.y + a.height) < WRITING_REACH && a.y - (b.y + b.height) < WRITING_REACH;
}

// readStrokes returns the strokes as the agent reads them: the letters of a
// word are no circles and arrows, however each looks alone.
export function readStrokes(strokes) {
  const out = [...strokes];
  let start = 0;
  for (let i = 1; i <= strokes.length; i++) {
    if (i < strokes.length && continuesWriting(strokes[i - 1], strokes[i])) continue;
    if (i - start >= WRITING_STROKES) {
      for (let j = start; j < i; j++) out[j] = { ...strokes[j], kind: 'drawing', line: strokes[j].drawn };
    }
    start = i;
  }
  return out;
}

// numberedLines numbers the lines as the overlay numbers the strokes, with
// neighbours that say the same on one line.
function numberedLines(lines) {
  const out = [];
  lines.forEach((line, i) => {
    const last = out[out.length - 1];
    if (last?.line === line) last.to = i + 1;
    else out.push({ line, from: i + 1, to: i + 1 });
  });
  return out.map(({ line, from, to }) => `${from === to ? from : `${from}–${to}`}. ${line}`);
}

// composeAnnotation builds the mail body: the note, then one numbered line
// per stroke, matching the numbers drawn on the overlay.
// The view itself rides on the message as the usual "[dashboard view: ...]"
// line, and with image set, as the picture attached to it.
export function composeAnnotation({ note = '', lines = [], image = false }) {
  const parts = [];
  const clean = String(note || '').trim().slice(0, ANNOTATE_NOTE_MAX);
  if (clean) parts.push(clean);
  if (lines.length) {
    const intro = image
      ? 'The operator marked up the dashboard view they are on. The attached image shows the view with their ink on it, numbered as below:'
      : 'The operator marked up the dashboard view they are on:';
    parts.push(`${intro}\n${numberedLines(lines).join('\n')}`);
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
  const [l, r] = [angle - 0.5, angle + 0.5].map((a) => [ex - 14 * Math.cos(a), ey - 14 * Math.sin(a)]);
  return [l, [ex, ey], r];
}

function tracePath(ctx, points) {
  ctx.beginPath();
  points.forEach(([x, y], i) => (i ? ctx.lineTo(x, y) : ctx.moveTo(x, y)));
  ctx.stroke();
}

// drawInk puts the strokes on a canvas as the overlay shows them, numbers
// included, for the image that goes to the agent.
export function drawInk(ctx, strokes, scale) {
  ctx.save();
  ctx.scale(scale, scale);
  ctx.lineWidth = 3.5;
  ctx.lineCap = 'round';
  ctx.lineJoin = 'round';
  ctx.textAlign = 'center';
  ctx.textBaseline = 'middle';
  ctx.font = '700 11px ui-sans-serif, system-ui, sans-serif';
  strokes.forEach((stroke, i) => {
    const [x, y] = stroke.points[0];
    ctx.strokeStyle = stroke.color;
    tracePath(ctx, stroke.points);
    if (stroke.kind === 'arrow') tracePath(ctx, arrowHead(stroke.points));
    ctx.fillStyle = stroke.color;
    ctx.beginPath();
    ctx.arc(x, y, 9, 0, Math.PI * 2);
    ctx.fill();
    ctx.fillStyle = '#0d1117';
    ctx.fillText(String(i + 1), x, y);
  });
  ctx.restore();
}

// AnnotateOverlay is the drawing layer over the whole page.
export function AnnotateOverlay({ agentName, onSend, onCancel, win = globalThis.window }) {
  const [strokes, setStrokes] = useState([]);
  const [color, setColor] = useState(ANNOTATE_COLORS[0]);
  const [note, setNote] = useState('');
  const [sending, setSending] = useState(false);
  const drawing = useRef(null);
  const rootRef = useRef(null);
  const noteRef = useRef(null);
  const [, setTick] = useState(0);

  // Focus moves into the layer (so no key reaches a terminal underneath) and
  // back to where it was when the layer closes. The page cannot scroll out
  // from under the strokes meanwhile.
  useEffect(() => {
    const opener = win?.document?.activeElement;
    noteRef.current?.focus?.();
    const root = rootRef.current;
    const stopScroll = (event) => event.preventDefault();
    root?.addEventListener?.('wheel', stopScroll, { passive: false });
    return () => {
      root?.removeEventListener?.('wheel', stopScroll);
      setTimeout(() => { if (opener?.isConnected) opener.focus?.(); }, 0);
    };
  }, [win]);

  useEffect(() => {
    const onKey = (event) => {
      if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); onCancel(); }
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'z' && !event.target?.closest?.('.annotate-note')) {
        event.preventDefault();
        setStrokes((list) => list.slice(0, -1));
      }
      if (event.key === 'Tab') keepFocusInside(event);
    };
    win?.addEventListener?.('keydown', onKey, true);
    return () => win?.removeEventListener?.('keydown', onKey, true);
  }, [onCancel, win]);

  // The layer is modal: Tab wraps around its controls and never reaches the
  // page underneath, or a terminal there.
  function keepFocusInside(event) {
    const stops = [...(rootRef.current?.querySelectorAll?.('button:not([disabled]), textarea') || [])];
    if (!stops.length) return;
    const first = stops[0];
    const last = stops[stops.length - 1];
    const at = win?.document?.activeElement;
    if (stops.includes(at) && at !== (event.shiftKey ? first : last)) return;
    event.preventDefault();
    (event.shiftKey ? last : first).focus?.();
  }

  function down(event) {
    if (event.button !== 0 || drawing.current) return;
    event.currentTarget.setPointerCapture?.(event.pointerId);
    drawing.current = { color, pointerId: event.pointerId, points: [[event.clientX, event.clientY]], startedAt: Date.now() };
    setTick((n) => n + 1);
  }
  function move(event) {
    const d = drawing.current;
    if (!d || event.pointerId !== d.pointerId) return;
    const [lx, ly] = d.points[d.points.length - 1];
    if (Math.hypot(event.clientX - lx, event.clientY - ly) < 2) return;
    d.points.push([event.clientX, event.clientY]);
    setTick((n) => n + 1);
  }
  // resolve reads what is under a finished stroke right away, while the page
  // still shows what the operator drew on.
  function resolve(points) {
    const doc = win?.document;
    const root = rootRef.current;
    // Look through the layer at the page underneath.
    if (root) root.style.pointerEvents = 'none';
    try {
      return resolveStroke(points, (x, y) => {
        const el = doc && elementAt(doc, x, y, `.annotate-root, ${COMPANION_CHROME}`);
        if (!el) return null;
        const line = terminalLineAt(el, y);
        return line === undefined ? labelFor(el) : line;
      });
    } finally {
      if (root) root.style.pointerEvents = '';
    }
  }

  function up(event) {
    const d = drawing.current;
    if (!d || event?.pointerId !== d.pointerId) return;
    drawing.current = null;
    if (d.points.length > 1) setStrokes((list) => [...list, { ...d, ...resolve(d.points), endedAt: Date.now() }]);
    else setTick((n) => n + 1);
  }

  async function send(event) {
    event?.preventDefault?.();
    if (sending || (!strokes.length && !note.trim())) return;
    const marks = readStrokes(strokes);
    setSending(true);
    try {
      await onSend({ note, lines: marks.map((stroke) => stroke.line), strokes: marks });
    } finally {
      setSending(false);
    }
  }

  const done = readStrokes(strokes);
  const live = drawing.current ? [...done, drawing.current] : done;
  return html`<div class="annotate-root" ref=${rootRef} data-capture-skip role="dialog" aria-modal="true" aria-label=${`Mark up this view for ${agentName}`}>
    <svg class="annotate-canvas" onPointerDown=${down} onPointerMove=${move} onPointerUp=${up} onPointerCancel=${up}
      aria-label="Annotation layer: draw on the page">
      ${live.map((stroke, i) => {
        const [x, y] = stroke.points[0];
        return html`<g key=${i}>
          <path d=${strokePath(stroke.points)} stroke=${stroke.color} />
          ${i < strokes.length && stroke.kind === 'arrow' && html`<path d=${strokePath(arrowHead(stroke.points))} stroke=${stroke.color} />`}
          ${i < strokes.length && html`<circle cx=${x} cy=${y} r="9" fill=${stroke.color} /><text x=${x} y=${y + 4}>${i + 1}</text>`}
        </g>`;
      })}
    </svg>
    <form class="annotate-bar" onSubmit=${send}>
      <span class="annotate-title">✎ Mark up this view for <b>${agentName}</b>: circle, underline, point with an arrow or write</span>
      ${ANNOTATE_COLORS.map((c) => html`<button key=${c} type="button" class=${`annotate-swatch${color === c ? ' on' : ''}`}
        style=${`background:${c}`} aria-label=${`Pen ${c}`} aria-pressed=${color === c ? 'true' : 'false'} onClick=${() => setColor(c)}></button>`)}
      <button type="button" class="annotate-btn" title="Undo (Ctrl+Z)" disabled=${!strokes.length} onClick=${() => setStrokes((list) => list.slice(0, -1))}>↶</button>
      <textarea class="annotate-note" ref=${noteRef} rows="1" maxLength=${ANNOTATE_NOTE_MAX} placeholder="Add a note (optional)" aria-label="Note" value=${note}
        onInput=${(event) => setNote(event.currentTarget.value)}
        onKeyDown=${(event) => { if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) send(event); }}></textarea>
      <button type="submit" class="annotate-send" disabled=${sending || (!strokes.length && !note.trim())}>${sending ? 'Sending…' : 'Send'}</button>
      <button type="button" class="annotate-btn" onClick=${onCancel} title="Cancel (Esc)">Cancel</button>
    </form>
  </div>`;
}
