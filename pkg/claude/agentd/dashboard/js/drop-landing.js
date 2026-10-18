// drop-landing.js — show where a confirmed drop ended up. Once a drag
// (dnd.js, group-reorder.js, dock-dnd.js, the fused view's moves between
// nodes) has done its work, the receiving group opens (with its ancestors and
// the section the dropped thing lives in), scrolls into view, and a soft blip
// plays over what landed: the agent's row, the process chip, or the group's
// header when the thing itself is not drawn (another lens, an offline row).
//
// A move to another node takes a while, so it first gets a transit marker on
// the receiving group's header ("arriving from …") that turns into the blip
// once the agent's row shows up there, or fades when the move fails.
//
// The effect is a fixed overlay that follows the element's box each frame,
// never a class on it: the group boxes and rows are Preact-owned and keyed,
// and a re-render (each snapshot publish) would wipe a class it did not set.
// The overlay nodes are cosmetic and self-removing (a blip after BLIP_MS, a
// transit marker when it lands, fails or expires) and own no operation state.
// dashboard-imperative-boundary: media-effects

import { dashPrefs } from './prefs.js';
import { fusedGroupListing } from './group-order.js';
import { revealGroupPath, setFusedSectionOpen } from './groups-fused-sections.js';
import { featureState } from './feature-state-registry.js';
import { isPageNode, selfNodeID } from './node-api.js';

// How long a landing waits for the dropped thing to be drawn before it blips
// the group header instead (a membership write lands on the next refresh).
const FIND_MS = 4000;
// A transit marker never outlives the move dialog's own watch.
const TRANSIT_MS = 120000;
const BLIP_MS = 1500;

const visible = (el) => !!el && el.isConnected && el.getClientRects().length > 0;
const esc = (s) => (globalThis.CSS?.escape ? CSS.escape(s) : String(s).replace(/["\\]/g, '\\$&'));

function reducedMotion() {
  try { return !!globalThis.matchMedia?.('(prefers-reduced-motion: reduce)').matches; } catch (_) { return false; }
}

// listings are where a group may be drawn: the page's own Groups view (its
// snapshot, under its own names) and the fused view (every node's groups as
// group@node). Each names the group as it is listed there.
function listings(group, node) {
  const out = [];
  const page = isPageNode(node || '');
  if (page) out.push({ name: group, groups: featureState('groups')?.snapshot?.value?.groups || [] });
  const fused = fusedGroupListing();
  if (fused && Array.isArray(fused.groups)) {
    const id = page ? selfNodeID() : node;
    const hit = fused.groups.find((g) => g.fleet_node && (g.fleet_node.id === id || (page && isPageNode(g.fleet_node.id)))
      && g.name === `${group}@${g.fleet_node.name}`);
    if (hit) out.push({ name: hit.name, groups: fused.groups });
  }
  return out;
}

// revealDropTarget opens a group where it is listed: its row, every ancestor
// and its Subgroups line, and `section` (agents / processes / subgroups) in
// the Both lens. `virtualKey` names a virtual group (Ungrouped) instead. A
// dropped group itself (`header`) keeps its own fold: only its ancestors open.
export function revealDropTarget({ group = '', node = '', section = '', virtualKey = '', header = false, prefs = dashPrefs } = {}) {
  try {
    if (virtualKey) prefs.setItem(`tclaude.dash.group.${virtualKey}`, '1');
    for (const where of group ? listings(group, node) : []) {
      if (!header) prefs.setItem(`tclaude.dash.group.${where.name}`, '1');
      revealGroupPath(where.name, where.groups, prefs);
      if (section) setFusedSectionOpen(where.name, section, true, prefs);
    }
  } catch (_) { /* a view preference must never break the drop */ }
  featureState('groups')?.rerender?.();
  document.getElementById('fleetview-fused-groups-root')?.dispatchEvent(new CustomEvent('tclaude:group-disclosure'));
}

// groupBoxes are the drawn <details> of a group: the page's own view keys it
// by its name; the fused view by group@node on the node's box.
function groupBoxes({ group, node, virtualKey }) {
  if (virtualKey) return [...document.querySelectorAll(`details[data-group-key="${esc(virtualKey)}"]`)].filter(visible);
  const page = isPageNode(node || '');
  return [...document.querySelectorAll('details[data-group-key]')].filter((box) => {
    if (!visible(box)) return false;
    const key = box.getAttribute('data-group-key') || '';
    const fleet = box.getAttribute('data-fleet-node');
    if (fleet === null) return page && key === group;
    const name = box.getAttribute('data-fleet-node-name') || '';
    const here = isPageNode(fleet) || fleet === selfNodeID();
    return (page ? here : fleet === node) && key === `${group}@${name}`;
  });
}

// boxRows are a box's own agent rows: not a nested subgroup's, nor an away
// agent's placeholder.
function boxRows(box) {
  return [...box.querySelectorAll('tr[data-dnd-agent], tr[data-conv]')].filter((row) =>
    row.closest('details[data-group-key]') === box && !row.classList.contains('away-placeholder') && visible(row));
}

const rowIDs = (row) => [row.getAttribute('data-dnd-agent'), row.getAttribute('data-conv')].filter(Boolean);

function ownRows(box, ids) {
  return boxRows(box).filter((row) => rowIDs(row).some((id) => ids.includes(id)));
}

// findLanded resolves what to mark: the dropped thing in the box, else (when
// `fallback`) the box's header.
function findLanded(target, fallback) {
  const ids = (target.ids || []).filter(Boolean);
  for (const box of groupBoxes(target)) {
    if (ids.length) {
      const row = ownRows(box, ids)[0];
      if (row) return { el: row, thing: true };
    }
    if (target.process) {
      const chip = [...box.querySelectorAll(`[data-saved-process-drag="${esc(target.process)}"]`)]
        .find((el) => el.closest('details[data-group-key]') === box && visible(el));
      if (chip) return { el: chip, thing: true };
    }
    if (target.header) return { el: box.querySelector(':scope > summary') || box, thing: true };
    if (fallback) return { el: box.querySelector(':scope > summary') || box, thing: false };
  }
  return null;
}

function overlayLayer() {
  let layer = document.getElementById('drop-landing-layer');
  if (!layer) {
    layer = document.createElement('div');
    layer.id = 'drop-landing-layer';
    layer.setAttribute('aria-hidden', 'true');
    document.body.appendChild(layer);
  }
  return layer;
}

// live: a page that can draw the effect. Outside one (the node test DOM, a
// torn-down page) the landing does nothing rather than throw.
const live = () => typeof document !== 'undefined' && !!document.body && typeof requestAnimationFrame === 'function';

// track keeps an overlay over `resolve()`'s element each frame until stop().
// A lost element is looked for again at most every RESOLVE_MS, not per frame.
const RESOLVE_MS = 250;
function track(overlay, resolve) {
  let el = null;
  let raf = 0;
  let looked = -Infinity;
  const frame = () => {
    if (!live()) return;
    if (!visible(el) && Date.now() - looked >= RESOLVE_MS) { looked = Date.now(); el = resolve(); }
    else if (!visible(el)) el = null;
    if (el) {
      const r = el.getBoundingClientRect();
      overlay.style.transform = `translate(${r.left}px, ${r.top}px)`;
      overlay.style.width = `${r.width}px`;
      overlay.style.height = `${r.height}px`;
      overlay.hidden = false;
    } else overlay.hidden = true;
    raf = requestAnimationFrame(frame);
  };
  frame();
  return { el: () => el, stop: () => cancelAnimationFrame(raf) };
}

function scrollTo(el) {
  try { el.scrollIntoView({ block: 'nearest', behavior: reducedMotion() ? 'auto' : 'smooth' }); } catch (_) {}
}

// blip plays the landing over an element, following it while it plays.
function blip(el, { failed = false } = {}) {
  const overlay = document.createElement('div');
  overlay.className = `drop-landing drop-landing-blip${failed ? ' failed' : ''}`;
  overlayLayer().appendChild(overlay);
  const tracker = track(overlay, () => (visible(el) ? el : null));
  setTimeout(() => { tracker.stop(); overlay.remove(); }, BLIP_MS);
}

// waitFor polls `find` until it yields, then hands it on; at the deadline it
// asks once more with the fallback allowed.
function waitFor(target, done, ms = FIND_MS) {
  const started = Date.now();
  const tick = () => {
    if (!live()) return;
    const hit = findLanded(target, false);
    if (hit) return done(hit);
    if (Date.now() - started >= ms) return done(findLanded(target, true));
    setTimeout(tick, 120);
  };
  tick();
}

// markLanded reveals a drop's target and blips what landed there. target:
// {group, node, virtualKey, section, ids (agent_id / conv_id of the dropped
// agent), process (a pinned template's id), header (the group itself
// landed: blip its header)}. Cosmetic: it never throws into the operation
// that called it.
export function markLanded(target = {}) {
  if (!live()) return;
  try {
    revealDropTarget(target);
    waitFor(target, (hit) => {
      if (!hit) return;
      scrollTo(hit.el);
      blip(hit.el);
    });
  } catch (_) { /* the drop itself already succeeded */ }
}

const NO_TRANSIT = Object.freeze({ land() {}, fail() {}, cancel() {} });

// markInTransit puts the "arriving" marker on the receiving group of a move
// to another node. The returned handle lands it (with the agent IDs the copy
// runs under there) or fails it; left alone, it lands by itself when the
// agent's row appears there, and gives up after TRANSIT_MS. `newRow` (a
// clone this page cannot follow, which runs under a new ID) also lands on any
// row the group did not have when the marker went up. Like markLanded it
// never throws: the move was already sent.
export function markInTransit(options = {}) {
  if (!live()) return NO_TRANSIT;
  try { return transit(options); } catch (_) { return NO_TRANSIT; }
}

function transit({ group = '', node = '', ids = [], from = '', label = '', newRow = false } = {}) {
  const target = { group, node, section: 'agents', ids: ids.filter(Boolean) };
  revealDropTarget(target);
  const overlay = document.createElement('div');
  overlay.className = 'drop-landing drop-landing-transit';
  const tag = document.createElement('span');
  tag.className = 'drop-landing-tag';
  tag.textContent = `✈ ${label ? `${label} ` : ''}arriving${from ? ` from ${from}` : ''}…`;
  overlay.appendChild(tag);
  overlayLayer().appendChild(overlay);
  let scrolled = false;
  const tracker = track(overlay, () => {
    const hit = findLanded({ group, node }, true);
    if (hit && !scrolled) { scrolled = true; scrollTo(hit.el); }
    return hit?.el || null;
  });
  let ended = false;
  let watch = 0;
  const end = () => {
    if (ended) return false;
    ended = true;
    clearInterval(watch);
    clearTimeout(expiry);
    tracker.stop();
    overlay.remove();
    return true;
  };
  const expiry = setTimeout(end, TRANSIT_MS);
  const handle = {
    land(more = []) {
      if (!end()) return;
      markLanded({ ...target, ids: [...target.ids, ...more.filter(Boolean)] });
    },
    fail() {
      const el = tracker.el();
      if (end() && visible(el)) blip(el, { failed: true });
    },
    cancel() { end(); },
  };
  // A move whose record this page cannot follow (a pull, a relay, a dialog
  // closed early) lands when the agent's row does.
  let known = null;
  watch = setInterval(() => {
    if (!live()) { end(); return; }
    const box = groupBoxes(target)[0];
    if (!box) return;
    const rows = boxRows(box);
    if (newRow && !known) known = new Set(rows.flatMap(rowIDs));
    const hit = rows.find((row) => rowIDs(row).some((id) => target.ids.includes(id) || (known && !known.has(id))));
    if (hit) handle.land(rowIDs(hit));
  }, 500);
  return handle;
}
