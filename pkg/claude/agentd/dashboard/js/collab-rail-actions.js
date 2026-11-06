// collab-rail-actions.js -- what the Collab side panel and its home do with an
// item (tcl-lx7nb7, tcl-u58qst, tcl-8nfzbb): open it, or bring it forward
// where it already is; and, for an item dragged out of the panel, open it as a
// strip tab where it is dropped on the strip or the home, or float it in a
// window where it is dropped anywhere else. The lists come from
// collab-rail-model.js.

import { signal } from '@preact/signals';
import { OTHER_SESSIONS, UNGROUPED, isAgentItem, railItems } from './collab-rail-model.js';
import { MOVABLE_KINDS, canMoveResource, moveResource } from './resource-moves.js';
import { processContext, processRunApi } from './process-context.js';
import { onPeerPage } from './node-api.js';
import { overlayResourceKey } from './resource-model.js';
import { resourcePlaces, revealResourceOverlay } from './resource-places.js';
import { openOverlayWindow, overlayTabs } from './overlay-instances.js';
import { moveTabsToCollab } from './overlay-tab-moves.js';
import { collabDropTarget, collabStripLanding } from './overlay-tab-drag.js';
import { droppedAt } from './overlay-window-model.js';
import { attachBackgroundTerminal, backgroundTerminals, rowView, visibleBackgroundTerminals } from './background-terminals.js';
import { openBoardOverlay, openBoardPane } from './board-overlay-state.js';
import { expectPage, openPageOverlay, openPagePane } from './page-place.js';
import { openProcessOverlay, openProcessTab } from './process-editor-place.js';
import { openGroupWebTermPane, placeTerminalPane } from './terminals-tab.js';
import { terminalTabStatus } from './terminal-tab-status.js';
import { remoteSessionAgents } from './remote-session-status.js';
import { dashPrefs } from './prefs.js';

const USED_PREF = 'tclaude.dash.collab.used';
const USED_KEPT = 100;

function readUsed() {
  try {
    const raw = JSON.parse(dashPrefs.getItem(USED_PREF) || '[]');
    return new Map(Array.isArray(raw) ? raw.filter((e) => Array.isArray(e) && typeof e[0] === 'string' && Number.isFinite(e[1])) : []);
  } catch {
    return new Map();
  }
}

// railUsed is when each rail item was last opened here, which the rail and
// the home order by. It keeps the USED_KEPT most recent, in the dashboard's
// prefs (prefs.js), so it survives a daemon restart. null until first read:
// the prefs load after this module does.
export const railUsed = signal(null);

const usedNow = () => railUsed.value || readUsed();

export function markRailUsed(key, at = Date.now()) {
  if (!key) return;
  const next = new Map(railUsed.peek() || readUsed());
  next.delete(key);
  next.set(key, at);
  const entries = [...next].sort((a, b) => b[1] - a[1]).slice(0, USED_KEPT);
  railUsed.value = new Map(entries);
  dashPrefs.setItem(USED_PREF, JSON.stringify(entries));
}

// terminalStatus is what the agent behind a rail terminal item is doing, with
// the agent itself; null for a terminal with no agent.
function terminalStatus(item, value) {
  const status = item.row ? rowView(item.row, value).status
    : item.pane ? terminalTabStatus(item.pane, value, remoteSessionAgents.value)
      : item.overlay?.agent ? terminalTabStatus({ seed: { agent: item.overlay.agent } }, value) : null;
  return status?.agent ? status : null;
}

// collabItems is everything the rail and the home list now: panes are the
// strip's, snapshot the dashboard's.
export function collabItems(panes, snapshot) {
  const value = snapshot?.value;
  return railItems({
    resources: value?.group_resources || [], groups: value?.groups || [], places: resourcePlaces.value,
    panes, overlays: overlayTabs.value, background: visibleBackgroundTerminals(backgroundTerminals.value, panes),
    titleOf: (row) => rowView(row, value).label, statusOf: (item) => terminalStatus(item, value), used: usedNow(),
  });
}

// overlayOf is the overlay tab holding an item, if one does.
export const overlayOf = (item) => item.overlay || overlayTabs.peek().find((tab) => overlayResourceKey(tab) === item.key) || null;

// openRailItem opens an item, or brings it forward where it already is.
export async function openRailItem(item, { actions, snapshot, fetchImpl = (...args) => globalThis.fetch(...args), notify = () => {} }) {
  markRailUsed(item.key);
  if (item.view === 'open-tab' && item.paneKey) return actions?.activatePane?.(item.paneKey);
  if (item.overlay) return openOverlayWindow(item.overlay);
  if (item.row) return attachBackgroundTerminal(item.row, snapshot?.value);
  const r = item.resource;
  if (!r || revealResourceOverlay(item.key)) return null;
  switch (r.kind) {
    case 'page':
      if (item.view !== 'closed') return openPagePane(r.id, item.title);
      try {
        const response = await fetchImpl(`/api/pages/${encodeURIComponent(r.id)}/reopen`, { method: 'POST', credentials: 'same-origin' });
        if (!response.ok) {
          const body = await response.json().catch(() => null);
          notify(`Could not reopen ${item.title}: ${body?.error || response.status}`, true);
          return null;
        }
        expectPage(r.id);
      } catch (cause) {
        notify(`Could not reopen ${item.title}: ${cause?.message || cause}`, true);
      }
      return null;
    case 'board': return openBoardPane(r.id, item.title);
    case 'process': return openProcessTab(r.id, item.title);
    case 'shell': return openGroupWebTermPane(r.id, `${r.id} shell`);
    default: return null;
  }
}

// railItemToStrip opens an item as a strip tab, beside the tab landing names
// (collabStripLanding), or at the end without one. A tab moves there; a window
// gives its tab to the strip.
export async function railItemToStrip(item, landing, ctx) {
  const place = (key) => {
    if (key && landing?.beside && landing.beside !== key) placeTerminalPane(key, landing.beside, { after: landing.after });
  };
  markRailUsed(item.key);
  if (item.view === 'open-tab' && item.paneKey) return place(item.paneKey);
  const tab = overlayOf(item);
  if (tab) return moveTabsToCollab([tab.id], landing || {});
  const opened = await openRailItem(item, ctx);
  return place(opened?.key || item.paneKey);
}

// railItemToFloat floats an item in a window at the point at. A window already
// holding it comes forward, or back from the dock. A strip tab floats if its
// kind can; a closed page reopens, as a click does.
export async function railItemToFloat(item, at, ctx) {
  markRailUsed(item.key);
  const tab = overlayOf(item);
  if (tab) return openOverlayWindow(tab);
  const floats = (key) => !!key && !!ctx.floatsOf?.(key);
  if (item.view === 'open-tab') return floats(item.paneKey) ? ctx.actions?.openPaneInOverlay?.(item.paneKey, { at }) : null;
  const r = item.resource;
  if (r?.kind === 'page' && item.view !== 'closed') return openPageOverlay(r.id, item.title, at);
  if (r?.kind === 'board') return openBoardOverlay(r.id, item.title, at);
  if (r?.kind === 'process' && openProcessOverlay(r.id, item.title)) return null;
  // A shell or a terminal floats from the tab it opens in.
  const key = (await openRailItem(item, ctx))?.key || item.paneKey;
  return floats(key) ? ctx.actions?.openPaneInOverlay?.(key, { at }) : null;
}

// pressable makes a role="button" span act on a click and on Enter or Space.
// The rail and the home use spans, not buttons, as Firefox starts no drag from
// a button inside a draggable item.
export function pressable(run) {
  return {
    onClick: run,
    onKeyDown: (event) => {
      if (event.key !== 'Enter' && event.key !== ' ') return;
      event.preventDefault();
      run(event);
    },
  };
}

export const RAIL_ITEM_DRAG_MIME = 'application/x-tclaude-rail-item';

// railDrag is the item in flight from the rail or the home: { key }. Otherwise null.
export const railDrag = signal(null);

// railDropZone is what a drop on target does: 'strip' (a tab there), 'home'
// (the empty strip's home, also a tab), 'rail' (nothing: the panel), else
// 'float'.
const elementOf = (target) => (target?.closest ? target : target?.parentElement) || null;

export function railDropZone(target) {
  const element = elementOf(target);
  if (element?.closest?.('[data-collab-strip]')) return 'strip';
  if (element?.closest?.('[data-collab-home]')) return 'home';
  // The whole side panel is the rail's: its padding, the space below a short
  // list and its edge handle too (collab-rail.js renders into #agent-dock).
  if (element?.closest?.('[data-collab-rail], #agent-dock')) return 'rail';
  return 'float';
}

function landingAt(event, zone) {
  if (zone === 'home') return { beside: '', after: false };
  if (zone !== 'strip') return null;
  return collabStripLanding(elementOf(event.target).closest('[data-collab-strip]'), event.clientX);
}

const sameLanding = (a, b) => a?.beside === b?.beside && a?.after === b?.after;

// --- moving an item to another group (tcl-eq8pq9) ---

// railResource is a board or page item as resource-moves.js reads it.
export function railResource(item) {
  if (!MOVABLE_KINDS.includes(item.kind)) return null;
  const id = item.resource?.id || item.key.slice(item.kind.length + 1);
  return { kind: item.kind, id, label: item.title, group: item.group || '', owner: item.resource?.owner || '',
    closed: item.resource ? item.resource.state !== 'running' : false };
}

// railAgent is an agent item's agent on this node, or null (another node's
// agent moves on its own dashboard).
export function railAgent(item) {
  if (!isAgentItem(item) || item.pane?.seed?.remote || item.row?.node) return null;
  const agent = item.status.agent;
  return agent.conv_id || agent.agent_id ? agent : null;
}

// railGroupDrop is what a drop of item on target in the panel does: move it
// to the group section under target ({ section, to, agent | resource }), or
// nothing (null). Not a group section, its own group, Other sessions, a
// peer's page, or a move the Groups tab would not offer, is nothing. to is
// '' for Not in a group.
export function railGroupDrop(item, target, snapshotValue, peer = onPeerPage()) {
  if (peer || !item) return null;
  const section = elementOf(target)?.closest?.('[data-rail-group-id]');
  const id = section?.getAttribute('data-rail-group-id');
  if (!id || id === OTHER_SESSIONS.id) return null;
  const to = id === UNGROUPED.id ? '' : (snapshotValue?.groups || []).find((g) => String(g.id) === id)?.name;
  if (typeof to !== 'string' || to === (item.group || '')) return null;
  const agent = railAgent(item);
  if (agent) return { section, to, agent };
  const resource = railResource(item);
  return resource && canMoveResource(resource, to, snapshotValue) ? { section, to, resource } : null;
}

// railItemToGroup makes the move railGroupDrop found. An agent goes through
// the Groups tab's own drop operations (dnd-ops.js, the same confirm and
// calls): into a group from none it is added, out to none it is removed,
// between two it moves. A board or page moves as its ⋯ → Move to group does.
//   ctx: { notify, groupOps?, api? } (tests pass groupOps and api).
export async function railItemToGroup(item, drop, ctx = {}) {
  const notify = ctx.notify || (() => {});
  if (drop.resource) {
    return moveResource(drop.resource, drop.to, { api: ctx.api || processRunApi, notify, refresh: () => processContext.refresh() });
  }
  const ops = ctx.groupOps || (await import('./dnd.js')).dndOps;
  const conv = drop.agent.conv_id || drop.agent.agent_id;
  const from = item.group || '';
  const payload = { conv, agent: drop.agent.agent_id || conv, label: drop.agent.title || item.title, sourceGroup: from, sourceUngrouped: !from };
  const where = ops.context();
  if (!drop.to) return ops.remove(payload, where);
  if (!from) return ops.add(payload, drop.to, where);
  return ops.move(payload, drop.to, where);
}

let markedSection = null;
function markSection(section) {
  if (markedSection === section) return;
  markedSection?.classList?.remove('cr-drop-over');
  markedSection = section || null;
  markedSection?.classList?.add('cr-drop-over');
}

// startRailDrag begins dragging an item out of the side panel. Until the
// drag ends, the page takes the drop: the strip and the home open the item as a
// tab there, the rest of the page floats it where it lands; the rail itself
// does nothing. The strip shows where it would land. ctx is what openRailItem
// takes, with floatsOf(paneKey).
let endRailDrag = null;

export function startRailDrag(event, item, ctx) {
  endRailDrag?.();
  const source = event.currentTarget;
  const doc = source?.ownerDocument || globalThis.document;
  event.dataTransfer?.setData(RAIL_ITEM_DRAG_MIME, item.key);
  if (event.dataTransfer) event.dataTransfer.effectAllowed = 'move';
  const aim = (next) => {
    if (!sameLanding(collabDropTarget.peek(), next)) collabDropTarget.value = next;
  };
  const over = (e) => {
    const zone = railDropZone(e.target);
    if (zone === 'rail') {
      aim(null);
      const move = railGroupDrop(item, e.target, ctx.snapshot?.value);
      markSection(move?.section);
      if (!move) return;
      e.preventDefault();
      if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
      return;
    }
    markSection(null);
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
    aim(zone === 'strip' ? landingAt(e, zone) : null);
  };
  const finish = () => {
    if (endRailDrag === finish) endRailDrag = null;
    source?.removeEventListener?.('dragend', finish);
    doc.removeEventListener('dragover', over, true);
    doc.removeEventListener('drop', drop, true);
    doc.removeEventListener('dragend', finish, true);
    doc.body?.classList?.remove('rail-dragging');
    markSection(null);
    aim(null);
    railDrag.value = null;
  };
  function drop(e) {
    const zone = railDropZone(e.target);
    if (zone === 'rail') {
      const move = railGroupDrop(item, e.target, ctx.snapshot?.value);
      if (!move) return;
      e.preventDefault();
      e.stopPropagation();
      finish();
      void railItemToGroup(item, move, ctx);
      return;
    }
    e.preventDefault();
    e.stopPropagation();
    let landing = null;
    try {
      landing = landingAt(e, zone);
    } finally {
      finish();
    }
    const at = droppedAt(e);
    void (landing ? railItemToStrip(item, landing, ctx) : railItemToFloat(item, at, ctx));
  }
  doc.addEventListener('dragover', over, true);
  doc.addEventListener('drop', drop, true);
  doc.addEventListener('dragend', finish, true);
  // dragend goes to the source; one the list re-rendered away never reaches
  // the document, so it is heard on the source too, and the next drag ends this one.
  source?.addEventListener?.('dragend', finish);
  endRailDrag = finish;
  // A page's frame would swallow the drag; while it lasts, the frames let it through.
  doc.body?.classList?.add('rail-dragging');
  railDrag.value = { key: item.key };
}
