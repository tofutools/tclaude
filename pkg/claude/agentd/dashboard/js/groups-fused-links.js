// groups-fused-links.js -- the Both lens's hover links: dashed lines between
// an agent's row in a group's Agents section and the process steps it
// performs in the same group's Processes section.
//
// Hovering an agent row links it to every step it performs; hovering a
// step links it to its performers; hovering anywhere else on a run card
// links every step with a performer to its agent. Nothing is drawn at rest,
// and a link never leaves the hovered group (its subgroups have their own).
// A line starts at the row's state pill (its name when the State column is
// hidden), and an end that is off screen becomes a small tag that scrolls
// to it.
//
// FusedLinks owns one SVG overlay over the Both frame. It measures on hover
// only, and while a hover is active it re-measures on scroll, resize and
// re-render. Highlights are classes on the linked rows and steps, removed
// with the hover.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

// Idle → hovering waits a moment, so sweeping the pointer across the table
// does not flash lines; leaving waits too, so the pointer can reach a tag.
// While a tag shows, moving on to another row or card waits as well: the
// way to a tag at the viewport edge crosses other rows and cards.
const ENTER_DELAY_MS = 120;
const SWITCH_DWELL_MS = 280;
const LEAVE_DELAY_MS = 220;
// An off-screen end is pinned this far inside the visible edge (the centre
// of a ~20px tag, plus a margin).
const EDGE_INSET = 16;

// visibleBottom is the viewport bottom above the dashboard's fixed footer.
function visibleBottom() {
  const footer = document.querySelector('body > footer');
  const top = footer ? rectOf(footer).top : 0;
  return top > 0 && top < window.innerHeight ? top : window.innerHeight;
}

const GROUP = 'details[data-group-key]';

function ownGroup(el) {
  return el?.closest?.(GROUP) || null;
}

function inSection(el, section, group) {
  const host = el.closest('[data-fused-section]');
  return !!host && host.dataset.fusedSection === section && ownGroup(host) === group;
}

function performersOf(node) {
  return String(node.dataset.performers || '').split(' ').filter(Boolean);
}

// resolveHover maps the element under the pointer to what it links, or null.
export function resolveHover(target) {
  const el = target?.closest ? target : target?.parentElement;
  const group = ownGroup(el);
  if (!group) return null;
  const row = el.closest('tr[data-dnd-agent]');
  if (row && inSection(row, 'agents', group)) return { kind: 'agent', group, el: row, agent: row.dataset.dndAgent };
  const card = el.closest('[data-process-run-card]');
  if (!card || !inSection(card, 'processes', group)) return null;
  const node = el.closest('[data-process-node]');
  if (node && performersOf(node).length) return { kind: 'node', group, el: node };
  return { kind: 'card', group, el: card };
}

// linkPairs is every [row, step] pair the hover links.
export function linkPairs(hover) {
  const { group } = hover;
  const rows = new Map();
  for (const row of group.querySelectorAll('[data-fused-section="agents"] tr[data-dnd-agent]')) {
    if (inSection(row, 'agents', group)) rows.set(row.dataset.dndAgent, row);
  }
  const steps = (scope) => [...scope.querySelectorAll('[data-process-node][data-performers]')]
    .filter((node) => inSection(node, 'processes', group));
  const pairs = [];
  const add = (node) => {
    for (const id of performersOf(node)) if (rows.has(id)) pairs.push([rows.get(id), node]);
  };
  if (hover.kind === 'agent') {
    for (const node of steps(group)) if (performersOf(node).includes(hover.agent)) pairs.push([hover.el, node]);
  } else if (hover.kind === 'node') {
    add(hover.el);
  } else {
    for (const node of steps(hover.el)) add(node);
  }
  return pairs;
}

function sameHover(a, b) {
  return !!a && !!b && a.kind === b.kind && a.el === b.el;
}

function visible(el) {
  if (!el) return false;
  return typeof el.getClientRects !== 'function' || el.getClientRects().length > 0;
}

const NO_RECT = { left: 0, right: 0, top: 0, bottom: 0, width: 0, height: 0 };

function rectOf(el) {
  return el && typeof el.getBoundingClientRect === 'function' ? el.getBoundingClientRect() : NO_RECT;
}

// rowAnchor is where a row's line starts: its state pill, else its name.
function rowAnchor(row) {
  const pill = row.querySelector('.state-pill-wrap');
  if (visible(pill)) return pill;
  const name = row.querySelector('.rowname');
  return visible(name) ? name : row;
}

function label(el, fallback) {
  return (el.querySelector('.pk-gnode-label') || el.querySelector('.rowname'))?.textContent?.trim() || fallback;
}

// measure turns pairs into overlay coordinates relative to the frame, with an
// off-screen end pinned to the viewport edge as a tag.
function measure(frame, pairs, bottom) {
  const base = rectOf(frame);
  const lines = [];
  const tags = new Map();
  const pin = (rect, y, el, text) => {
    if (rect.top > bottom - EDGE_INSET) {
      tags.set(el, { x: rect.left + rect.width / 2 - base.left, y: bottom - EDGE_INSET - base.top, dir: '↓', text, el });
      return bottom - EDGE_INSET - base.top;
    }
    if (rect.bottom < EDGE_INSET) {
      tags.set(el, { x: rect.left + rect.width / 2 - base.left, y: EDGE_INSET - base.top, dir: '↑', text, el });
      return EDGE_INSET - base.top;
    }
    return y - base.top;
  };
  for (const [row, node] of pairs) {
    const from = rectOf(rowAnchor(row));
    const to = rectOf(node);
    // A wide graph scrolls inside its card: keep the end inside the card.
    const card = node.closest('.gp-card-body');
    const clip = card ? rectOf(card) : null;
    const toX = clip ? Math.min(Math.max(to.left + to.width / 2, clip.left + 8), clip.right - 8) : to.left + to.width / 2;
    lines.push({
      key: `${row.dataset.dndAgent}>${node.closest('[data-process-run-card]')?.dataset.processRunCard}/${node.dataset.processNode}`,
      x1: from.left + from.width / 2 - base.left,
      y1: pin(from, from.bottom + 1, row, label(row, row.dataset.dndLabel || 'agent')),
      x2: toX - base.left,
      y2: pin(to, to.top - 2, node, label(node, 'step')),
    });
  }
  return { lines, tags: [...tags.values()] };
}

const nextFrame = (fn) => (typeof requestAnimationFrame === 'function' ? requestAnimationFrame(fn) : setTimeout(fn, 16));
const cancelFrame = (id) => (typeof cancelAnimationFrame === 'function' ? cancelAnimationFrame(id) : clearTimeout(id));

function curve({ x1, y1, x2, y2 }) {
  const bend = Math.max(24, Math.min(80, Math.abs(y2 - y1) / 2));
  const dir = y2 >= y1 ? 1 : -1;
  return `M${x1},${y1} C${x1},${y1 + bend * dir} ${x2},${y2 - bend * dir} ${x2},${y2}`;
}

export function FusedLinks({ frame }) {
  const [drawn, setDrawn] = useState(null);
  const hover = useRef(null);
  const marked = useRef([]);
  const shown = useRef('');
  const redraw = useRef(() => {});

  useEffect(() => {
    const root = frame.current;
    if (!root) return undefined;
    let enterTimer = 0;
    let leaveTimer = 0;
    let frameRequest = 0;
    const unmark = () => {
      for (const [el, cls] of marked.current) el.classList.remove(cls);
      marked.current = [];
    };
    const mark = (el, cls) => {
      el.classList.add(cls);
      marked.current.push([el, cls]);
    };
    let tagsShown = false;
    const publish = (next) => {
      tagsShown = !!next?.tags.length;
      const key = next ? JSON.stringify([next.lines, next.tags.map((tag) => [tag.x, tag.y, tag.dir, tag.text])]) : '';
      if (key === shown.current) return;
      shown.current = key;
      setDrawn(next);
    };
    const draw = () => {
      unmark();
      const current = hover.current;
      if (!current || !current.el.isConnected) {
        hover.current = null;
        publish(null);
        return;
      }
      const pairs = linkPairs(current);
      if (!pairs.length) { publish(null); return; }
      const cards = new Set();
      for (const [row, node] of pairs) {
        mark(row, 'gf-link-hot');
        mark(node, 'gf-link-hot');
        cards.add(node.closest('[data-process-run-card]'));
      }
      const hot = new Set(pairs.map(([, node]) => node));
      for (const card of cards) {
        for (const node of card?.querySelectorAll('[data-process-node]') || []) if (!hot.has(node)) mark(node, 'gf-link-dim');
      }
      const next = measure(root, pairs, visibleBottom());
      publish({ ...next, width: root.scrollWidth, height: root.scrollHeight });
    };
    const schedule = () => {
      if (!hover.current || frameRequest) return;
      frameRequest = nextFrame(() => { frameRequest = 0; draw(); });
    };
    redraw.current = schedule;
    const leave = () => {
      clearTimeout(enterTimer);
      clearTimeout(leaveTimer);
      leaveTimer = setTimeout(() => { hover.current = null; draw(); }, LEAVE_DELAY_MS);
    };
    const onOver = (event) => {
      if (event.target.closest?.('.gf-link-tag')) { clearTimeout(leaveTimer); clearTimeout(enterTimer); return; }
      const next = resolveHover(event.target);
      if (!next) { if (hover.current) leave(); else clearTimeout(enterTimer); return; }
      clearTimeout(leaveTimer);
      if (sameHover(next, hover.current)) return;
      clearTimeout(enterTimer);
      const apply = () => { hover.current = next; draw(); };
      // Moving from one link to the next is immediate, unless a tag is showing;
      // the first hover always waits.
      if (hover.current && !tagsShown) apply();
      else enterTimer = setTimeout(apply, hover.current ? SWITCH_DWELL_MS : ENTER_DELAY_MS);
    };
    root.addEventListener('pointerover', onOver);
    root.addEventListener('focusin', onOver);
    root.addEventListener('pointerleave', leave);
    root.addEventListener('focusout', leave);
    window.addEventListener('scroll', schedule, true);
    window.addEventListener('resize', schedule);
    return () => {
      clearTimeout(enterTimer);
      clearTimeout(leaveTimer);
      cancelFrame(frameRequest);
      root.removeEventListener('pointerover', onOver);
      root.removeEventListener('focusin', onOver);
      root.removeEventListener('pointerleave', leave);
      root.removeEventListener('focusout', leave);
      window.removeEventListener('scroll', schedule, true);
      window.removeEventListener('resize', schedule);
      unmark();
      hover.current = null;
    };
  }, [frame]);

  // A re-render of the tree (a snapshot poll) can move or replace the linked
  // elements: re-measure after every render while a hover is active.
  useEffect(() => { redraw.current(); });

  if (!drawn) return null;
  return html`<div class="gf-links">
    <svg class="gf-links-svg" width=${drawn.width} height=${drawn.height} aria-hidden="true">
      ${drawn.lines.map((line) => html`<g key=${line.key}>
        <path class="gf-link" d=${curve(line)} />
        <circle class="gf-link-end" cx=${line.x1} cy=${line.y1} r="2.5" />
        <circle class="gf-link-end" cx=${line.x2} cy=${line.y2} r="2.5" />
      </g>`)}
    </svg>
    ${drawn.tags.map((tag) => html`<button type="button" key=${`${tag.dir}${tag.text}${tag.x}`} class="gf-link-tag" tabindex="-1"
      aria-label=${`Scroll to ${tag.text}`}
      style=${`left:${tag.x}px;top:${tag.y}px`} onClick=${() => tag.el.scrollIntoView?.({ block: 'center', behavior: 'smooth' })}
    >${tag.dir} ${tag.text}</button>`)}
  </div>`;
}
