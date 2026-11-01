// companion-guide.js -- dashboard guidance cues (tcl-z7sbof): an agent holding
// dashboard.guide (the Steward by default) can move the operator's dashboard
// to a view and highlight a control there with a short note
// (`tclaude agent guide show`). Cues ride the snapshot as companion_cues; the
// companion island plays each once through GuideOverlay. A cue never clicks or
// changes anything: it only points.
//
// The target is named by its visible text, matched against the controls and
// headings on the page; the companion's own window is never a target.

import { h } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

export const GUIDE_SHOW_MS = 14000;
export const GUIDE_FIND_TIMEOUT_MS = 3000;
export const GUIDE_FIND_STEP_MS = 250;
export const GUIDE_MARK_PATIENCE_MS = 1000;
// Give a freshly activated view a moment to render before looking for the
// target.
export const GUIDE_SETTLE_MS = 350;

// The companion's own chrome, which agents never point at. Every overlay window
// is shown from the companion's island, and a board in one is the page's content.
export const COMPANION_CHROME = '#companion-root :not(.board-overlay, .board-overlay *)';

export const TARGET_SELECTOR = [
  'button', 'a[href]', '[role="button"]', '[role="tab"]', '[role="menuitem"]', '[role="link"]',
  'summary', 'label', 'h1', 'h2', 'h3', 'h4', 'th', 'legend', 'input[type="submit"]',
].join(',');

// A cue never points at the operator's own consent: an agent could steer an
// approval of its own request, or a decision, with a ring and a note. Those
// controls cannot be a target or a mark.
const CONSENT_AREAS = '.companion-approval, .companion-decision, .mail-decision, .access-btn';
const CONSENT_WORDS = /^(approve|allow|deny|decline)\b/i;

export function consentControl(el) {
  if (el.closest?.(CONSENT_AREAS)) return true;
  if (!el.matches?.('button, [role="button"], input[type="submit"]')) return false;
  const { label, text } = textOf(el);
  return CONSENT_WORDS.test(label) || CONSENT_WORDS.test(text);
}

export function textOf(el) {
  const label = el.getAttribute?.('aria-label') || el.getAttribute?.('title') || '';
  const text = (el.textContent || '').replace(/\s+/g, ' ').trim();
  return { label: label.replace(/\s+/g, ' ').trim(), text };
}

// guideMatchScore ranks how well an element's text names the target: exact
// beats prefix beats substring, and among equals shorter text wins. 0 means
// no match.
export function guideMatchScore(candidate, target) {
  const want = String(target || '').toLowerCase().replace(/\s+/g, ' ').trim();
  if (!want) return 0;
  let best = 0;
  for (const raw of [candidate.text, candidate.label]) {
    const have = String(raw || '').toLowerCase();
    if (!have) continue;
    let score = 0;
    if (have === want) score = 3000;
    else if (have.startsWith(want)) score = 2000;
    else if (have.includes(want)) score = 1000;
    if (score) best = Math.max(best, score - Math.min(have.length, 999));
  }
  return best;
}

function visible(el) {
  if (!el?.getBoundingClientRect) return false;
  const rect = el.getBoundingClientRect();
  if (!rect.width || !rect.height) return false;
  if (el.closest?.('[hidden], [aria-hidden="true"]')) return false;
  return true;
}

// findGuideTarget returns the visible element best matching the target text,
// outside the companion itself, or null. With loose, an element that is not
// a control or heading (a graph node, a table cell) matches too, and a
// better text match wins over a control: an exact "Review" node beats a
// "Request review" button. Among equal matches the innermost element wins.
export function findGuideTarget(doc, target, { exclude = COMPANION_CHROME, loose = false } = {}) {
  if (!doc || !target) return null;
  const strict = findIn(doc.querySelectorAll(TARGET_SELECTOR), target, exclude);
  if (!loose) return strict.el;
  const short = [...doc.querySelectorAll('body *')].filter((el) => (el.textContent || '').length <= 200);
  const any = findIn(short, target, exclude);
  return (any.score > strict.score ? any : strict).el;
}

function findIn(elements, target, exclude) {
  let best = null;
  let bestScore = 0;
  for (const el of elements) {
    if (exclude && el.closest?.(exclude)) continue;
    if (consentControl(el)) continue;
    const score = guideMatchScore(textOf(el), target);
    // Elements come in document order, so a tie with an element inside the
    // current best is the tighter fit (the span, not its row).
    if (score < bestScore || (score === bestScore && !(best?.contains?.(el) && best !== el))) continue;
    if (!visible(el)) continue;
    best = el;
    bestScore = score;
  }
  return { el: best, score: bestScore };
}

// cuePath turns a cue into the dashboard path to open, or '' to stay.
export function cuePath(cue) {
  if (cue?.path) return cue.path;
  if (cue?.tab) return `/${cue.tab}`;
  return '';
}

// newCues returns the cues not seen before, oldest first, and records them.
// The first call only records: cues already live when the page loads were
// meant for an earlier page and are not replayed. Cue ids restart with the
// daemon, so a cue is known by its id and the time it was posted.
export function newCues(seen, cues, primed) {
  const fresh = [];
  for (const cue of Array.isArray(cues) ? cues : []) {
    const key = cueOpenKey(cue);
    if (!Number.isFinite(Number(cue?.id)) || seen.has(key)) continue;
    seen.add(key);
    if (primed) fresh.push(cue);
  }
  return fresh.sort((a, b) => a.id - b.id);
}

// A cue that hands an agent over while every dashboard tab is hidden waits
// for the operator to come back, but not forever: an old hand-over would
// yank the window to an agent that no longer needs them.
export const CUE_OPEN_PATIENCE_MS = 120000;

// cueOpenDue reports whether a held hand-over cue should still open now.
export function cueOpenDue(cue, now) {
  const at = Date.parse(cue?.at || '');
  return !Number.isFinite(at) || now - at <= CUE_OPEN_PATIENCE_MS;
}

// cueOpenKey names one hand-over for the cross-tab claim. Cue ids restart
// with the daemon, so the time it was posted is part of the key.
export function cueOpenKey(cue) {
  return `${cue?.id}@${cue?.at || ''}`;
}

// claimCueOpen lets one dashboard tab act on a hand-over: the last few any
// tab opened are kept in shared prefs, newest first, and a tab seeing it there
// leaves it. Several can land in one snapshot, so the newest alone would not
// stop a second tab from taking the earlier ones.
export function claimCueOpen(stored, cue) {
  return !String(stored || '').split('\n').includes(cueOpenKey(cue));
}

const CUE_OPEN_CLAIMS_KEPT = 8;

// cueOpenClaimed is the shared pref's value once this tab took the hand-over.
export function cueOpenClaimed(stored, cue) {
  const earlier = String(stored || '').split('\n').filter(Boolean);
  return [cueOpenKey(cue), ...earlier].slice(0, CUE_OPEN_CLAIMS_KEPT).join('\n');
}

// claimCueLock is the claim every open tab sees at once, unlike shared prefs:
// a Web Lock named for the hand-over, which the winner keeps until its page
// closes so a tab that sees the cue later still loses. Without Web Locks (an
// insecure origin, an old browser), or if the request fails, it resolves true
// and leaves the claim to the shared prefs.
export function claimCueLock(locks, cue) {
  if (typeof locks?.request !== 'function') return Promise.resolve(true);
  return new Promise((resolve) => {
    try {
      locks.request(`tclaude.companion.cue-open/${cueOpenKey(cue)}`, { ifAvailable: true }, (lock) => {
        resolve(!!lock);
        return lock ? new Promise(() => {}) : undefined;
      }).catch(() => resolve(true));
    } catch {
      resolve(true);
    }
  });
}

// An unfocused but visible tab (a second monitor) gives the focused one this
// long to take a hand-over first. Tabs poll the snapshot every 2s on their own
// clocks, so the focused tab can see a cue that much later.
export const CUE_OPEN_UNFOCUSED_DELAY_MS = 2500;

function rectKey(r) {
  return r ? `${Math.round(r.left)},${Math.round(r.top)},${Math.round(r.width)},${Math.round(r.height)}` : '-';
}

// markPath draws one mark as a hand-drawn-looking SVG path in viewport
// coordinates: a loose double loop around the target, a wavy underline, or a
// curved arrow from one control to another.
export function markPath(kind, rect, fromRect = null) {
  if (!rect) return '';
  const cx = rect.left + rect.width / 2;
  const cy = rect.top + rect.height / 2;
  if (kind === 'circle') {
    const rx = rect.width / 2 + 14;
    const ry = rect.height / 2 + 10;
    // Start a little past the top and overshoot, like a pen circling twice.
    const pts = [];
    for (let i = 0; i <= 28; i++) {
      const a = -Math.PI / 2 + 0.3 + (i / 24) * 2 * Math.PI;
      const wobble = 1 + 0.04 * Math.sin(i * 1.7);
      pts.push(`${(cx + rx * wobble * Math.cos(a)).toFixed(1)},${(cy + ry * wobble * Math.sin(a)).toFixed(1)}`);
    }
    return `M${pts.join(' L')}`;
  }
  if (kind === 'underline') {
    const y = rect.top + rect.height + 5;
    const x0 = rect.left - 4;
    const x1 = rect.left + rect.width + 4;
    const step = (x1 - x0) / 6;
    let d = `M${x0.toFixed(1)},${y.toFixed(1)}`;
    for (let i = 1; i <= 6; i++) d += ` Q${(x0 + step * (i - 0.5)).toFixed(1)},${(y + (i % 2 ? 3 : -3)).toFixed(1)} ${(x0 + step * i).toFixed(1)},${y.toFixed(1)}`;
    return d;
  }
  if (kind === 'arrow' && fromRect) {
    const fx = fromRect.left + fromRect.width / 2;
    const fy = fromRect.top + fromRect.height / 2;
    // Leave and enter at the boxes' edges, along the centre line.
    const edge = (r, x, y, tx, ty) => {
      const dx = tx - x; const dy = ty - y;
      const t = Math.min(Math.abs((r.width / 2 + 6) / (dx || 1e-6)), Math.abs((r.height / 2 + 6) / (dy || 1e-6)));
      return [x + dx * Math.min(t, 1), y + dy * Math.min(t, 1)];
    };
    const [sx, sy] = edge(fromRect, fx, fy, cx, cy);
    const [ex, ey] = edge(rect, cx, cy, fx, fy);
    const mx = (sx + ex) / 2 - (ey - sy) * 0.15;
    const my = (sy + ey) / 2 + (ex - sx) * 0.15;
    const angle = Math.atan2(ey - my, ex - mx);
    const head = [angle - 0.45, angle + 0.45].map((a) => `${(ex - 14 * Math.cos(a)).toFixed(1)},${(ey - 14 * Math.sin(a)).toFixed(1)}`);
    return `M${sx.toFixed(1)},${sy.toFixed(1)} Q${mx.toFixed(1)},${my.toFixed(1)} ${ex.toFixed(1)},${ey.toFixed(1)} M${head[0]} L${ex.toFixed(1)},${ey.toFixed(1)} L${head[1]}`;
  }
  return '';
}

// GuideMarks follows the marked elements as the page scrolls and redraws
// the marks over them.
function GuideMarks({ marks, win }) {
  const [rects, setRects] = useState([]);
  useEffect(() => {
    let frame = 0;
    let last = '';
    const follow = () => {
      const next = marks.map((m) => ({
        rect: m.element?.isConnected === false ? null : m.element?.getBoundingClientRect?.() || null,
        from: m.from?.isConnected === false ? null : m.from?.getBoundingClientRect?.() || null,
      }));
      const key = next.map((r) => `${rectKey(r.rect)}|${rectKey(r.from)}`).join(';');
      if (key !== last) {
        last = key;
        setRects(next);
      }
      frame = win?.requestAnimationFrame?.(follow) || 0;
    };
    follow();
    return () => win?.cancelAnimationFrame?.(frame);
  }, [marks]);
  return html`<svg class="companion-guide-marks" aria-hidden="true">
    ${marks.map((m, i) => {
      const d = markPath(m.mark.kind, rects[i]?.rect, rects[i]?.from);
      return d ? html`<path key=${i} class=${`mark-${m.mark.kind}`} d=${d} />` : null;
    })}
  </svg>`;
}

// GuideOverlay draws the highlight ring around the target and the note
// bubble beside it, following the target as the page scrolls. Without a
// target the note shows on its own above the companion chips.
export function GuideOverlay({ cue, element, marks = [], onDismiss, win = globalThis.window }) {
  const [rect, setRect] = useState(() => element?.getBoundingClientRect?.() || null);
  useEffect(() => {
    if (!element) return undefined;
    let frame = 0;
    let last = '';
    const follow = () => {
      // The target left the page (the operator navigated away): stop pointing.
      if (element.isConnected === false) {
        onDismiss();
        return;
      }
      const next = element.getBoundingClientRect();
      const key = `${next.left},${next.top},${next.width},${next.height}`;
      if (key !== last) {
        last = key;
        setRect(next);
      }
      frame = win?.requestAnimationFrame?.(follow) || 0;
    };
    frame = win?.requestAnimationFrame?.(follow) || 0;
    return () => win?.cancelAnimationFrame?.(frame);
  }, [element]);
  const who = cue.agent_name || 'steward';
  const missing = cue.target && !element;
  const lost = marks.filter((m) => !m.element || (m.mark.kind === 'arrow' && !m.from))
    .map((m) => (m.mark.kind === 'arrow' && !m.from ? m.mark.from : m.mark.target));
  const drawn = useMemo(() => marks.filter((m) => m.element && (m.mark.kind !== 'arrow' || m.from)), [marks]);
  const markLayer = drawn.length ? html`<${GuideMarks} marks=${drawn} win=${win} />` : null;
  const note = html`<div class="companion-guide-note" role="status">
    <span class="companion-avatar steward" aria-hidden="true">✦</span>
    <div><b>${who}</b>${cue.note ? html`<span>${cue.note}</span>` : null}
      ${missing ? html`<small>Couldn't find “${cue.target}” on this view.</small>` : null}
      ${lost.length ? html`<small>Couldn't find ${lost.map((t) => `“${t}”`).join(', ')} on this view.</small>` : null}</div>
    <button type="button" aria-label="Dismiss" onClick=${onDismiss}>×</button>
  </div>`;
  if (!element || !rect) {
    return html`${markLayer}${(cue.note || missing || lost.length) ? html`<div class="companion-guide-float">${note}</div>` : null}`;
  }
  const vw = win?.innerWidth || 1280;
  const vh = win?.innerHeight || 800;
  const pad = 6;
  const ring = `left:${rect.left - pad}px;top:${rect.top - pad}px;width:${rect.width + pad * 2}px;height:${rect.height + pad * 2}px`;
  // The note goes below the target when there is room, else above it.
  const below = rect.bottom + 120 < vh;
  const left = Math.max(8, Math.min(vw - 340, rect.left));
  const place = below ? `left:${left}px;top:${rect.bottom + pad + 8}px` : `left:${left}px;bottom:${vh - rect.top + pad + 8}px`;
  return html`${markLayer}<div class="companion-guide-ring" aria-hidden="true" style=${ring}></div>
    <div class="companion-guide-anchor" style=${place}>${note}</div>`;
}
