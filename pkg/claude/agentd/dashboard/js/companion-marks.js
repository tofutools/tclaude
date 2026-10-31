// companion-marks.js -- what an agent draws on the operator's view to get their
// attention (tcl-s97jr5, `tclaude agent guide mark`). The daemon holds the marks
// (dashboard_marks.go) and open tabs collect the active ones with their
// presence ping; this draws them.
//
// A mark never takes a click: everything here is pointer-events: none except
// the buttons and fields of a tip and the dismiss-all control. A mark
// follows its control or, for a region or stroke, the page's scroll. Like a cue,
// it never points at the operator's own consent.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { consentControl, findGuideTarget, markPath } from './companion-guide.js';

const html = htm.bind(h);

// How often a mark looks for its control again: the page can re-render it.
export const MARK_RESOLVE_MS = 500;
const MARK_TIP_WIDTH = 300;
const MARK_TIP_GAP = 8;
// A tip's height before it is on the page: its chrome plus wrapped text.
const MARK_TIP_CHROME = 58;
const MARK_TIP_LINE = 18;
const MARK_TIP_LINE_CHARS = 44;
const MARK_TIP_FLOAT_BOTTOM = 96;
const MARK_TIP_STEP = 24;
const MARK_REPLY_MAX = 2000;
const CONSENT_SELECTOR = '.companion-approval, .companion-decision, .mail-decision, .access-btn, button';

// markVisibleHere reports whether a mark belongs to this page.
export function markVisibleHere(mark, location) {
  const path = mark?.spec?.path;
  if (!path) return true;
  const [pathname] = path.split('?');
  return location?.pathname === pathname || location?.pathname?.startsWith(`${pathname.replace(/\/$/, '')}/`);
}

export function rectsOverlap(a, b) {
  return a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom;
}

// regionRect places a region or stroke where the page has scrolled it to, given
// the scroll when the mark first appeared.
export function regionRect(region, anchor, scroll) {
  const left = region.x - (scroll.x - anchor.x);
  const top = region.y - (scroll.y - anchor.y);
  return { left, top, width: region.width, height: region.height, right: left + region.width, bottom: top + region.height };
}

export function strokeBounds(points) {
  const xs = points.map((p) => p[0]);
  const ys = points.map((p) => p[1]);
  const x = Math.min(...xs);
  const y = Math.min(...ys);
  return { x, y, width: Math.max(Math.max(...xs) - x, 1), height: Math.max(Math.max(...ys) - y, 1) };
}

export function strokePath(points, dx = 0, dy = 0) {
  return `M${points.map(([x, y]) => `${(x + dx).toFixed(1)},${(y + dy).toFixed(1)}`).join(' L')}`;
}

export function tipHeight(mark) {
  return MARK_TIP_CHROME + Math.ceil((mark.tip || '').length / MARK_TIP_LINE_CHARS) * MARK_TIP_LINE;
}

// placeTips gives each tip a spot beside its mark, below it when there is room,
// clear of the tips already placed, whose buttons it would hide, and of the
// blocked rects: a tip must not sit on the operator's approval controls. A mark
// with nothing to sit beside floats above the companion.
export function placeTips(items, vw, vh, blocked = []) {
  const taken = [...blocked];
  const free = (left, top, height) => !taken.some((other) => rectsOverlap({ left, right: left + MARK_TIP_WIDTH, top, bottom: top + height }, other));
  const lowest = (height) => vh - height - MARK_TIP_GAP;
  return items.map(({ mark, rect }) => {
    const height = tipHeight(mark);
    const left = rect ? Math.max(MARK_TIP_GAP, Math.min(vw - MARK_TIP_WIDTH - MARK_TIP_GAP, rect.left)) : vw - MARK_TIP_WIDTH - 16;
    const below = rect && rect.bottom + height + MARK_TIP_GAP < vh;
    let top = vh - MARK_TIP_FLOAT_BOTTOM - height;
    if (rect) top = below ? rect.bottom + MARK_TIP_GAP : rect.top - height - MARK_TIP_GAP;
    top = Math.max(MARK_TIP_GAP, Math.min(lowest(height), top));
    const place = { left, top };
    // Walk away from the mark, then try the page's edges.
    search: for (const x of [left, MARK_TIP_GAP, vw - MARK_TIP_WIDTH - MARK_TIP_GAP]) {
      for (let step = 0; step * MARK_TIP_STEP < vh; step++) {
        const offset = step * MARK_TIP_STEP * (below ? 1 : -1);
        const y = top + offset < MARK_TIP_GAP || top + offset > lowest(height) ? top - offset : top + offset;
        if (y >= MARK_TIP_GAP && y <= lowest(height) && free(x, y, height)) {
          place.left = x;
          place.top = y;
          break search;
        }
      }
    }
    taken.push({ left: place.left, right: place.left + MARK_TIP_WIDTH, top: place.top, bottom: place.top + height });
    return place;
  });
}

function refTarget(doc, ref) {
  try {
    return doc.querySelector(ref);
  } catch {
    return null;
  }
}

// consentRects are where the operator's approval controls are.
export function consentRects(doc) {
  const rects = [];
  for (const el of doc.querySelectorAll(CONSENT_SELECTOR)) {
    if (!consentControl(el)) continue;
    const rect = el.getBoundingClientRect();
    if (rect.width && rect.height) rects.push(rect);
  }
  return rects;
}

const overConsent = (rect, consent) => consent.some((other) => rectsOverlap(rect, other));

// resolveMark finds what a mark points at now: {element, from}, {rect} for a
// region or stroke, {} for a label with no anchor, or {why} when it cannot be
// drawn here. Overlap is judged by position, because a mark on a wrapper rings
// the approval control inside it.
export function resolveMark(mark, doc, anchor, scroll) {
  const consent = consentRects(doc);
  const { target, ref, from, region, points } = mark.spec || {};
  if (region || points?.length) {
    const box = region || strokeBounds(points);
    const rect = regionRect(box, anchor, scroll);
    return overConsent(rect, consent) ? { why: 'covers an approval control' } : { rect };
  }
  if (!target && !ref) return {};
  const element = ref ? refTarget(doc, ref) : findGuideTarget(doc, target, { loose: true });
  if (!element) return { why: 'target not on this view' };
  if (consentControl(element)) return { why: 'target is an approval control' };
  if (overConsent(element.getBoundingClientRect(), consent)) return { why: 'covers an approval control' };
  if (mark.kind !== 'arrow') return { element };
  const start = findGuideTarget(doc, from, { loose: true });
  if (!start) return { why: 'arrow start not on this view' };
  return overConsent(start.getBoundingClientRect(), consent) ? { why: 'covers an approval control' } : { element, from: start };
}

function liveRect(el) {
  return el?.isConnected === false ? null : el?.getBoundingClientRect?.() || null;
}

function MarkShape({ mark, rect, from, scrollDelta }) {
  const classes = `companion-mark mark-${mark.spec?.color || 'purple'}${mark.spec?.blink ? ' blink' : ''}`;
  if (mark.kind === 'freehand') {
    return html`<path class=${classes} d=${strokePath(mark.spec.points, scrollDelta.x, scrollDelta.y)} />`;
  }
  if (!rect) return null;
  if (mark.kind === 'highlight' || mark.kind === 'box') {
    const pad = mark.kind === 'highlight' ? 5 : 0;
    return html`<rect class=${`${classes}${mark.kind === 'highlight' ? ' filled' : ''}`} rx="6" x=${rect.left - pad} y=${rect.top - pad}
      width=${rect.width + pad * 2} height=${rect.height + pad * 2} />`;
  }
  const d = markPath(mark.kind, rect, from);
  return d ? html`<path class=${classes} d=${d} />` : null;
}

function MarkTip({ mark, why, place, onEnd, onReply }) {
  const [replying, setReplying] = useState(false);
  const [text, setText] = useState('');
  const [sent, setSent] = useState(false);
  const send = async (event) => {
    event.preventDefault();
    const body = text.trim();
    if (!body) return;
    if (await onReply(mark, body)) {
      setText('');
      setReplying(false);
      setSent(true);
    }
  };
  return html`<div class="companion-mark-tip" style=${`left:${place.left}px;top:${place.top}px`} data-mark-tip=${mark.id}>
    <div class="companion-mark-tip-head"><b>${mark.agent_name || 'steward'}</b>
      <button type="button" title="Got it" aria-label="Acknowledge" onClick=${() => onEnd(mark, 'acknowledge')}>✓</button>
      <button type="button" title="Dismiss" aria-label="Dismiss" onClick=${() => onEnd(mark, 'dismiss')}>×</button></div>
    ${mark.tip ? html`<div class="companion-mark-tip-text">${mark.tip}</div>` : null}
    ${why ? html`<small>Couldn't show this: ${why}.</small>` : null}
    ${sent ? html`<small>Reply sent.</small>` : null}
    ${!mark.agent_id ? null : replying
      ? html`<form onSubmit=${send}>
          <textarea rows="2" maxlength=${MARK_REPLY_MAX} aria-label="Reply to this mark" placeholder="Reply here"
            value=${text} onInput=${(e) => setText(e.target.value)}
            onKeyDown=${(e) => { if (e.key === 'Enter' && !e.shiftKey) void send(e); }}></textarea>
          <button type="submit">Send</button></form>`
      : html`<button type="button" class="companion-mark-reply" onClick=${() => setReplying(true)}>Reply</button>`}
  </div>`;
}

// MarksLayer draws the active marks. onReport hands it what was drawn, for
// captures to say so.
export function MarksLayer({ marks, win, onEnd, onDismissAll, onReply, onReport }) {
  const doc = win.document;
  const anchors = useRef(new Map());
  const resolved = useRef(new Map());
  const [, redraw] = useState(0);
  const here = marks.filter((mark) => markVisibleHere(mark, win.location));

  useEffect(() => {
    if (!marks.length) {
      onReport?.([]);
      return undefined;
    }
    const scroll = () => ({ x: win.scrollX || 0, y: win.scrollY || 0 });
    let frame = 0;
    let lastKey = '';
    let lastResolve = 0;
    const resolve = () => {
      const report = [];
      for (const mark of marks) {
        if (!markVisibleHere(mark, win.location)) {
          report.push({ id: mark.id, drawn: false, why: 'on another page' });
          continue;
        }
        if (!anchors.current.has(mark.id)) anchors.current.set(mark.id, scroll());
        const found = resolveMark(mark, doc, anchors.current.get(mark.id), scroll());
        resolved.current.set(mark.id, found);
        report.push(found.why ? { id: mark.id, drawn: false, why: found.why } : { id: mark.id, drawn: true });
      }
      onReport?.(report);
    };
    const follow = () => {
      const now = win.performance?.now?.() ?? Date.now();
      if (now - lastResolve >= MARK_RESOLVE_MS) {
        lastResolve = now;
        resolve();
      }
      const at = scroll();
      const key = marks.map((mark) => {
        const found = resolved.current.get(mark.id);
        const rect = found?.rect || liveRect(found?.element);
        const start = liveRect(found?.from);
        return `${mark.id}:${found?.why || ''}:${rect ? [rect.left, rect.top, rect.width, rect.height].join() : ''}:${start ? [start.left, start.top].join() : ''}`;
      }).join('|') + `|${at.x},${at.y}`;
      if (key !== lastKey) {
        lastKey = key;
        redraw((n) => n + 1);
      }
      frame = win.requestAnimationFrame(follow);
    };
    follow();
    return () => win.cancelAnimationFrame(frame);
  }, [marks]);

  if (!here.length) return null;
  const at = { x: win.scrollX || 0, y: win.scrollY || 0 };
  const drawn = here.map((mark) => {
    const found = resolved.current.get(mark.id) || {};
    const anchor = anchors.current.get(mark.id) || at;
    const box = mark.spec?.region || (mark.spec?.points?.length ? strokeBounds(mark.spec.points) : null);
    const rect = box && !found.why ? regionRect(box, anchor, at) : liveRect(found.element);
    return { mark, rect, from: liveRect(found.from), why: found.why, scrollDelta: { x: anchor.x - at.x, y: anchor.y - at.y } };
  });
  // A bare mark still needs a way to end it, so every mark gets a tip.
  const places = placeTips(drawn, win.innerWidth, win.innerHeight, consentRects(doc));
  return html`<svg class="companion-marks" aria-hidden="true">
      ${drawn.filter((d) => !d.why).map((d) => html`<${MarkShape} key=${d.mark.id} ...${d} />`)}
    </svg>
    ${drawn.map((d, i) => html`<${MarkTip} key=${d.mark.id} mark=${d.mark} why=${d.why} place=${places[i]} onEnd=${onEnd} onReply=${onReply} />`)}
    <button type="button" class="companion-marks-clear" onClick=${onDismissAll}>Dismiss all marks (${here.length})</button>`;
}

// marksKey says whether a ping changed anything to draw.
export function marksKey(marks) {
  return (marks || []).map((mark) => `${mark.id}:${mark.expires_at}:${mark.reply || ''}`).join('|');
}
