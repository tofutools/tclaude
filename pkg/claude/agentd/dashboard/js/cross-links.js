// cross-links.js -- the links between an agent and its Task board tasks
// (docs/taskboard.md). On the board, a lane head, a card's avatar marker
// and the List's Who name the agent; on the agent's row, "☰ n tasks" names
// its tasks. Each is a link:
//
//   click  unfolds whatever hides the target (the group and its ancestors,
//          the section, a lanes cell past its "+ n more" cap), then scrolls
//          it into view with a short smooth scroll and the drop landing's
//          blip (drop-landing.js), as a landed drop does.
//   hover  never unfolds. It outlines the target while the pointer is on the
//          link; when something folded hides it, the folded header (or the
//          cell's "+ n more") is outlined instead; when it is off-screen, a
//          marker at the top or bottom edge says which way it is.
//
// prefers-reduced-motion drops the smooth scroll and the blip's motion.
// The outline is a fixed overlay that follows the target each frame, as the
// drop landing's is, never a class on a Preact-owned element.
// dashboard-imperative-boundary: media-effects

import { signal } from '@preact/signals';
import { blip, findLanded, live, overlayLayer, reducedMotion, revealDropTarget, track, visible } from './drop-landing.js';

const FIND_MS = 4000;
const esc = (s) => (globalThis.CSS?.escape ? CSS.escape(s) : String(s).replace(/["\\]/g, '\\$&'));

// taskRevealRequest is the task a click asked to see. A lanes cell that
// holds it past its cap opens for it (groups-taskboard-lanes.js
// useCellReveal), also one drawn only once the click unfolded it; `seq`
// tells two clicks on the same task apart and `at` lets an old request lie.
export const REVEAL_FRESH_MS = 5000;
export const taskRevealRequest = signal(null);
let seq = 0;

// --- finding the targets ---

// agentTarget is the drop landing's target for an agent's row in a group:
// the group as the board knows it (its own name) and its node.
function agentTarget({ group, node, id }) {
  return { group, node, section: 'agents', ids: [id] };
}

function findAgent(target, fallback) {
  const hit = findLanded(agentTarget(target), fallback);
  return hit ? { el: hit.el, folded: !hit.thing } : null;
}

// boardSection is a group's drawn Task board section; `shown` is the name
// the view shows the group under (name@node for a linked node's group).
function boardSection(shown) {
  return [...document.querySelectorAll(`[data-fused-taskboard="${esc(shown)}"]`)].find(visible) || null;
}

function findTask({ shown, group, node, taskId }, fallback) {
  const section = boardSection(shown);
  if (section) {
    const el = [...section.querySelectorAll(`[data-task-id="${esc(taskId)}"]`)].find(visible);
    if (el) return { el, folded: false };
    if (!fallback) return null;
    // A lanes cell holds it past its cap: its "+ n more" says so.
    const more = [...section.querySelectorAll('[data-hidden-tasks]')]
      .find((btn) => visible(btn) && btn.getAttribute('data-hidden-tasks').split(' ').includes(taskId));
    if (more) return { el: more, folded: true };
    const head = section.querySelector(':scope > .gf-section-head');
    if (visible(head)) return { el: head, folded: true };
  }
  if (!fallback) return null;
  const box = findLanded({ group, node }, true);
  return box ? { el: box.el, folded: true } : null;
}

// --- click: unfold, then scroll and blip ---

function scrollToEl(el) {
  try { el.scrollIntoView({ block: 'center', inline: 'nearest', behavior: reducedMotion() ? 'auto' : 'smooth' }); } catch (_) {}
}

// land waits for find() to yield (the unfolded parts draw on the next
// render, a task's lanes cell opens after that), then scrolls there and
// blips. It looks again on the next frame after each change to the page
// (a MutationObserver), not on a timer, so the scroll follows the render
// that draws the target. At the deadline it settles for what the fallback
// finds. A newer click cancels the one still waiting; past the deadline, a
// fallback that finds nothing either ends the wait.
let landing = 0;
function land(find) {
  const mine = ++landing;
  let finished = false;
  let observer = null;
  let deadline = 0;
  let frame = 0;
  const finish = (hit) => {
    finished = true;
    observer?.disconnect();
    clearTimeout(deadline);
    if (frame) cancelAnimationFrame(frame);
    if (!hit) return;
    scrollToEl(hit.el);
    blip(hit.el);
  };
  const stale = () => !live() || mine !== landing;
  const look = () => {
    frame = 0;
    if (finished) return;
    if (stale()) { finish(null); return; }
    const hit = find(false);
    if (hit) finish(hit);
  };
  look();
  if (finished) return;
  // Not drawn yet: look again on the frame after the page changes.
  if (typeof MutationObserver === 'function') {
    observer = new MutationObserver(() => { if (!frame && !finished) frame = requestAnimationFrame(look); });
    observer.observe(document.body, { childList: true, subtree: true, attributes: true, attributeFilter: ['open', 'class', 'hidden', 'aria-expanded', 'style'] });
  }
  deadline = setTimeout(() => {
    if (finished) return;
    finish(stale() ? null : find(false) || find(true));
  }, FIND_MS);
}

// jumpToAgent shows an agent's row in a group: {group, node, id}.
export function jumpToAgent(target) {
  if (!live() || !target?.id) return;
  try {
    revealDropTarget(agentTarget(target));
    land((fallback) => findAgent(target, fallback));
  } catch (_) { /* a link must never break the page */ }
}

// jumpToTask shows a task on its group's board: {shown, group, node, taskId}.
export function jumpToTask(target) {
  if (!live() || !target?.taskId) return;
  try {
    revealDropTarget({ group: target.group, node: target.node, section: 'taskboard' });
    seq += 1;
    taskRevealRequest.value = { taskId: target.taskId, seq, at: Date.now() };
    land((fallback) => findTask(target, fallback));
  } catch (_) { /* a link must never break the page */ }
}

// --- hover: outline the target, or what hides it ---

// highlight outlines resolve()'s element until the returned stop() runs, or
// until the link `src` leaves the page (it unmounted while hovered).
// Off-screen, the outline becomes a marker on the viewport's edge.
function highlight(resolve, src) {
  if (!live()) return () => {};
  const overlay = document.createElement('div');
  overlay.className = 'xlink-hover';
  overlayLayer().appendChild(overlay);
  let folded = false;
  const tracker = track(overlay, () => {
    const hit = resolve();
    folded = !!hit?.folded;
    return hit?.el || null;
  });
  // Mark the edge each frame from the box track() placed.
  let raf = 0;
  const stop = () => { cancelAnimationFrame(raf); tracker.stop(); overlay.remove(); if (hovering?.stop === stop) hovering = null; };
  const edge = () => {
    if (src && !src.isConnected) { stop(); return; }
    const el = tracker.el();
    if (el) {
      const r = el.getBoundingClientRect();
      const h = globalThis.innerHeight || document.documentElement.clientHeight;
      const above = r.bottom < 0;
      const below = r.top > h;
      overlay.classList.toggle('xlink-folded', folded);
      overlay.classList.toggle('xlink-above', above);
      overlay.classList.toggle('xlink-below', below);
      if (above || below) {
        overlay.style.transform = `translate(${Math.max(8, r.left)}px, ${above ? 4 : h - 22}px)`;
        overlay.style.height = '';
        overlay.style.width = `${Math.max(48, Math.min(r.width, 240))}px`;
      }
    }
    raf = requestAnimationFrame(edge);
  };
  raf = requestAnimationFrame(edge);
  return stop;
}

// hovering is the one outline up. It lives here, not in a link's props: a
// link re-rendered while hovered gets new handlers, and its mouseleave must
// still take the outline the old ones put up.
let hovering = null;

// linkProps are a link's handlers and attributes: a click (or Enter or
// Space) jumps, the pointer or focus on it outlines the target.
function linkProps(jump, resolve, title) {
  const off = () => { hovering?.stop(); hovering = null; };
  const on = (event) => {
    off();
    const stop = highlight(resolve, event?.currentTarget);
    hovering = { stop };
  };
  return {
    role: 'link', tabindex: '0', title, 'data-xlink': '',
    onClick: (event) => { event.preventDefault(); event.stopPropagation(); off(); jump(); },
    onKeyDown: (event) => {
      if (event.key !== 'Enter' && event.key !== ' ') return;
      event.preventDefault();
      event.stopPropagation();
      off();
      jump();
    },
    onMouseEnter: on, onMouseLeave: off, onFocus: on, onBlur: off,
    // A double-click is two clicks on the link, never the card's own
    // double-click (which opens its edit form).
    onDblClick: (event) => event.stopPropagation(),
  };
}

// agentLink: the props for a link to an agent's row.
export function agentLink({ group, node = '', id, name = '' }) {
  const target = { group, node, id };
  return linkProps(() => jumpToAgent(target), () => findAgent(target, true), `Show ${name || 'the agent'} in the group's agents`);
}

// taskLink: the props for a link to a task on its board.
export function taskLink({ shown, group, node = '', taskId, title = '' }) {
  const target = { shown, group, node, taskId };
  return linkProps(() => jumpToTask(target), () => findTask(target, true), `Show “${title || taskId}” on the task board`);
}
