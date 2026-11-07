// collab-rail-menu.js -- a Collab panel row's menu (its ⋯ button, or a right
// click on the row): every per-item action the strip's old menus offered, now
// at the item (tcl-ievs1w). Attach or open; kill a background session,
// an Other session included; end the agents' sessions on a drawing board; download
// or delete a drawing board. Create and import live in the panel's ＋ New menu
// (collab-new-menu.js).

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDismissablePopover } from './hover-popover.js';
import { Menu, MenuItem, MenuSep } from './ui-components.js';
import { killBackgroundTerminal } from './background-terminals.js';
import { askDeleteDrawingBoard, exportDrawingBoard } from './drawing-board-manage.js';
import { shellConfirm, shellToast } from './shell-state.js';

const html = htm.bind(h);

const drawingBoardID = (item) => item.resource?.id || item.key.slice('drawing_board:'.length);

// tmuxAttachHint is how to reach a session the dashboard cannot attach: from
// a shell, by hand.
export const tmuxAttachHint = (row) => `Attach from a shell: tmux -L tclaude attach -t '${row.tmux_session}'`;

// railRowActions is what item's menu offers, in order: [{ id, label,
// danger, run(ctx) }], with a separator before the destructive ones. ctx is
// { open, confirm, notify, fetchImpl, kill, exportDrawingBoard, askDeleteDrawingBoard },
// the seams tests swap.
export function railRowActions(item) {
  const open = [];
  const end = [];
  const row = item.row;
  if (!row || row.key || row.attachLock) {
    open.push({ id: 'open', label: row && row.kind !== 'drawing_board' ? (row.attachLock ? 'Attach (locked)' : 'Attach') : 'Open',
      disabled: !!row?.attachLock, title: row?.attachLock || '', run: (ctx) => ctx.open(item) });
  }
  if (item.kind === 'drawing_board') {
    open.push({ id: 'download', label: 'Download .excalidraw', run: (ctx) => ctx.exportDrawingBoard(drawingBoardID(item), ctx.fetchImpl)
      .catch((err) => ctx.notify(`Could not export the drawing board: ${err?.message || err}`, true)) });
  }
  if (row?.killable && row.kind !== 'drawing_board') end.push(killEntry(item, row, 'Kill this session?'));
  if (item.shellSession) end.push(killEntry(item, item.shellSession, 'Kill this shell?'));
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
