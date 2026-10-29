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
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

export const GUIDE_SHOW_MS = 14000;
export const GUIDE_FIND_TIMEOUT_MS = 3000;
export const GUIDE_FIND_STEP_MS = 250;
// Give a freshly activated view a moment to render before looking for the
// target.
export const GUIDE_SETTLE_MS = 350;

const TARGET_SELECTOR = [
  'button', 'a[href]', '[role="button"]', '[role="tab"]', '[role="menuitem"]', '[role="link"]',
  'summary', 'label', 'h1', 'h2', 'h3', 'h4', 'th', 'legend', 'input[type="submit"]',
].join(',');

function textOf(el) {
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
// outside the companion itself, or null.
export function findGuideTarget(doc, target, { exclude = '#companion-root' } = {}) {
  if (!doc || !target) return null;
  let best = null;
  let bestScore = 0;
  for (const el of doc.querySelectorAll(TARGET_SELECTOR)) {
    if (exclude && el.closest?.(exclude)) continue;
    const score = guideMatchScore(textOf(el), target);
    if (score <= bestScore) continue;
    if (!visible(el)) continue;
    best = el;
    bestScore = score;
  }
  return best;
}

// cuePath turns a cue into the dashboard path to open, or '' to stay.
export function cuePath(cue) {
  if (cue?.path) return cue.path;
  if (cue?.tab) return `/${cue.tab}`;
  return '';
}

// newCues returns the cues not seen before, oldest first, and records them.
// The first call only records: cues already live when the page loads were
// meant for an earlier page and are not replayed.
export function newCues(seen, cues, primed) {
  const fresh = [];
  for (const cue of Array.isArray(cues) ? cues : []) {
    const id = Number(cue?.id);
    if (!Number.isFinite(id) || seen.has(id)) continue;
    seen.add(id);
    if (primed) fresh.push(cue);
  }
  return fresh.sort((a, b) => a.id - b.id);
}

// GuideOverlay draws the highlight ring around the target and the note
// bubble beside it, following the target as the page scrolls. Without a
// target the note shows on its own above the companion chips.
export function GuideOverlay({ cue, element, onDismiss, win = globalThis.window }) {
  const [rect, setRect] = useState(() => element?.getBoundingClientRect?.() || null);
  useEffect(() => {
    if (!element) return undefined;
    let frame = 0;
    const follow = () => {
      setRect(element.getBoundingClientRect());
      frame = win?.requestAnimationFrame?.(follow) || 0;
    };
    frame = win?.requestAnimationFrame?.(follow) || 0;
    return () => win?.cancelAnimationFrame?.(frame);
  }, [element]);
  const who = cue.agent_name || 'steward';
  const missing = cue.target && !element;
  const note = html`<div class="companion-guide-note" role="status">
    <span class="companion-avatar steward" aria-hidden="true">✦</span>
    <div><b>${who}</b>${cue.note ? html`<span>${cue.note}</span>` : null}
      ${missing ? html`<small>Couldn't find “${cue.target}” on this view.</small>` : null}</div>
    <button type="button" aria-label="Dismiss" onClick=${onDismiss}>×</button>
  </div>`;
  if (!element || !rect) {
    return (cue.note || missing) ? html`<div class="companion-guide-float">${note}</div>` : null;
  }
  const vw = win?.innerWidth || 1280;
  const vh = win?.innerHeight || 800;
  const pad = 6;
  const ring = `left:${rect.left - pad}px;top:${rect.top - pad}px;width:${rect.width + pad * 2}px;height:${rect.height + pad * 2}px`;
  // The note goes below the target when there is room, else above it.
  const below = rect.bottom + 120 < vh;
  const left = Math.max(8, Math.min(vw - 340, rect.left));
  const place = below ? `left:${left}px;top:${rect.bottom + pad + 8}px` : `left:${left}px;bottom:${vh - rect.top + pad + 8}px`;
  return html`<div class="companion-guide-ring" aria-hidden="true" style=${ring}></div>
    <div class="companion-guide-anchor" style=${place}>${note}</div>`;
}
