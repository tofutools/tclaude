// collab-rail-menu.js -- a Collab panel row's menu (its ⋯ button, or a right
// click on the row): every per-item action the strip's old menus offered, now
// at the item (tcl-ievs1w). Attach or open; stop, resume or retire an agent, as
// its Groups row does; kill a background session, an Other session included;
// end the agents' sessions on a drawing board; download or delete a drawing
// board. Create and import live in the panel's ＋ New menu
// (collab-new-menu.js).

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDismissablePopover } from './hover-popover.js';
import { Menu, MenuItem, MenuSep } from './ui-components.js';
import { killBackgroundTerminal } from './background-terminals.js';
import { askDeleteDrawingBoard, exportDrawingBoard } from './drawing-board-manage.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { isAgentItem } from './collab-rail-model.js';
import { agentGrantHeld } from './peer-view-limits.js';
import { nodeSnapshot, onPeerPage } from './node-api.js';
import { dashboardState } from './snapshot-store.js';

const html = htm.bind(h);

const drawingBoardID = (item) => item.resource?.id || item.key.slice('drawing_board:'.length);

// tmuxAttachHint is how to reach a session the dashboard cannot attach: from
// a shell, by hand.
export const tmuxAttachHint = (row) => `Attach from a shell: tmux -L tclaude attach -t '${row.tmux_session}'`;

// railPeerView is the peer view an agent's controls are gated by, as on its
// Groups row: a linked node's (its fused rows), the page's on a peer's own
// page, and none for this node's own agents (null: nothing gated).
export function railPeerView(node) {
  if (node) return nodeSnapshot(node)?.peer_view || {};
  return onPeerPage() ? dashboardState.snapshot.value?.peer_view || {} : null;
}

// lifecycleAgent is the agent behind an agent row, with what its Groups row
// controls name it by: the selector (agent_id, else conv), the label, and the
// node it runs on ('' for the page's).
function lifecycleAgent(item) {
  if (!isAgentItem(item)) return null;
  const agent = item.status.agent;
  const selector = agent.agent_id || agent.conv_id;
  if (!selector) return null;
  return { agent, selector, conv: agent.conv_id || selector, label: agent.title || agent.conv_id || selector,
    // A background row's node is { id, name, color }.
    node: item.pane?.seed?.remote?.peer || item.row?.node?.id || '' };
}

// lifecycleEntries are an agent row's Stop or Resume, and Retire…: the
// Groups row's status light (dot-toggle) and retire, through the same dialogs
// and requests (row-action-handler.js). A peer's agent gets each only where
// the peer grants it, as its fused Groups row does.
function lifecycleEntries(item, peerView) {
  const a = lifecycleAgent(item);
  if (!a) return { power: null, retire: null };
  const view = peerView(a.node);
  const online = !!a.agent.online;
  const may = (act, dataset = {}) => view === null || agentGrantHeld(act, a.agent.agent_id || a.selector, view, dataset);
  const power = !may('dot-toggle', { online: online ? '1' : '0' }) ? null : online
    ? { id: 'stop', label: 'Stop…', title: `Turn off ${a.label} (asks first: soft exit or force kill)`,
      run: (ctx) => ctx.stopAgent(a.selector, a.label, a.node) }
    : { id: 'resume', label: 'Resume', title: `Wake ${a.label}`,
      run: (ctx) => ctx.resumeAgent(a.selector, a.label, a.node) };
  // Retire names a peer's agent by its stable ID (its node's route takes no
  // other), this node's by its conversation, as the Groups row does.
  const retireAs = a.node || onPeerPage() ? (a.agent.agent_id || a.selector) : a.conv;
  const retire = !may('retire-agent') ? null : { id: 'retire', label: 'Retire…', danger: true,
    title: `Retire ${a.label}: demote it back to a plain conversation, revoking its group memberships and permission grants`,
    run: (ctx) => ctx.retireAgent(retireAs, a.label, a.node) };
  return { power, retire };
}

// railRowActions is what item's menu offers, in order: [{ id, label,
// danger, run(ctx) }], with a separator before the destructive ones. ctx is
// { open, confirm, notify, fetchImpl, kill, exportDrawingBoard, askDeleteDrawingBoard },
// the seams tests swap.
export function railRowActions(item, { peerView = railPeerView } = {}) {
  const open = [];
  const end = [];
  const row = item.row;
  if (!row || row.key || row.attachLock) {
    open.push({ id: 'open', label: row && row.kind !== 'drawing_board' ? (row.attachLock ? 'Attach (locked)' : 'Attach') : 'Open',
      disabled: !!row?.attachLock, title: row?.attachLock || '', run: (ctx) => ctx.open(item) });
  }
  const { power, retire } = lifecycleEntries(item, peerView);
  if (power) open.push(power);
  if (item.kind === 'drawing_board') {
    open.push({ id: 'download', label: 'Download .excalidraw', run: (ctx) => ctx.exportDrawingBoard(drawingBoardID(item), ctx.fetchImpl)
      .catch((err) => ctx.notify(`Could not export the drawing board: ${err?.message || err}`, true)) });
  }
  if (row?.killable && row.kind !== 'drawing_board') end.push(killEntry(item, row, 'Kill this session?'));
  if (item.shellSession) end.push(killEntry(item, item.shellSession, 'Kill this shell?'));
  if (retire) end.push(retire);
  if (item.sessions) {
    end.push(confirmEntry(item.sessions, {
      id: 'end-sessions', label: 'End agents’ sessions…', title: 'End the agents’ sessions on this drawing board?',
      body: 'Every agent session on the drawing board ends. The drawing board stays.', okLabel: 'End', busyLabel: 'Ending…',
      done: `Ended the sessions on ${item.title}`, failed: 'Could not end the sessions',
    }));
  }
  // A peer's drawing board (tcl-ug8bb8) is deleted on its own node.
  if (item.kind === 'drawing_board' && !item.resource?.peer) {
    end.push({ id: 'delete-drawing-board', label: 'Delete drawing board…', danger: true, run: (ctx) => ctx.askDeleteDrawingBoard({ id: drawingBoardID(item), title: item.title }) });
  }
  return { open, end };
}

// confirmEntry is a destructive entry: it asks first, then ends row's
// session(s) through ctx.kill, and says how that went.
function confirmEntry(row, { id, label, title, body, okLabel, busyLabel, done, failed }) {
  return { id, label, danger: true, run: (ctx) => ctx.confirm({
    title, body, okLabel, busyLabel,
    action: async () => { const failure = await ctx.kill(row); if (failure) throw new Error(failure); return true; },
  }).then((ok) => { if (ok) ctx.notify(done); }, (err) => ctx.notify(`${failed}: ${err?.message || err}`, true)) };
}

function killEntry(item, row, title) {
  return confirmEntry(row, {
    id: 'kill', label: 'Kill session…', title, body: `Ends tmux session ${row.tmux_session} and whatever runs in it.`,
    okLabel: 'Kill', busyLabel: 'Killing…', done: `Killed ${item.title}`, failed: `Could not kill ${item.title}`,
  });
}

// hasRowActions is whether item's menu has anything to offer.
export function hasRowActions(item) {
  const { open, end } = railRowActions(item);
  return open.length + end.length > 0;
}

export const defaultRowContext = () => ({
  confirm: shellConfirm, notify: shellToast, kill: killBackgroundTerminal, exportDrawingBoard, askDeleteDrawingBoard,
  // The Groups row's own dialogs and requests (row-action-handler.js
  // dot-toggle and retire-agent), loaded on use: they sit above the panel
  // in the module graph.
  stopAgent: async (agent, label, node) => (await import('./transaction-dialog-controller.js')).openShutdownAgentDialog(agent, label, node),
  resumeAgent: async (agent, label, node) => {
    const { ensureClaudeSubscription } = await import('./subscription-relaunch.js');
    if (!(await ensureClaudeSubscription({ agent, label, node, verb: 'wake it' }))) return;
    await (await import('./dashboard-operations.js')).resumeAgentReq(agent, label, false, node);
  },
  retireAgent: async (agent, label, node) => (await import('./transaction-dialog-controller.js')).openRetireAgentDialog(agent, label, node),
  fetchImpl: (...args) => globalThis.fetch(...args),
});

// RailRowMenu is the ⋯ button and its menu. open/onOpen let the row hold the
// state, so a right click on the row opens the same menu.
export function RailRowMenu({ item, open, onOpen, ctx }) {
  const ref = useRef(null);
  useDismissablePopover(ref, open, (why) => { if (why !== 'away') onOpen(false); });
  const { open: first, end } = railRowActions(item);
  if (!first.length && !end.length) return null;
  const entry = (action) => html`<${MenuItem} key=${action.id} danger=${action.danger} disabled=${action.disabled} title=${action.title || undefined}
    data-rail-action=${action.id} onClick=${(event) => { event.stopPropagation(); onOpen(false); void action.run(ctx); }}>${action.label}<//>`;
  return html`<span class="cr-menu" ref=${ref}>
    <button data-ui-exempt type="button" class="cr-tool cr-more" aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
      title="More actions" aria-label=${`More actions: ${item.title}`}
      onClick=${(event) => { event.stopPropagation(); onOpen(!open); }}>⋯</button>
    ${open && html`<${Menu} size="sm" class="cr-row-menu" aria-label=${item.title}>
      ${first.map(entry)}${first.length && end.length ? html`<${MenuSep} />` : null}${end.map(entry)}
    <//>`}
  </span>`;
}

// useRowMenu is a row's menu state and its right-click handler.
export function useRowMenu() {
  const [open, setOpen] = useState(false);
  const onContextMenu = (event, item) => {
    if (item && !hasRowActions(item)) return;
    event.preventDefault();
    setOpen(true);
  };
  return { open, setOpen, onContextMenu };
}
