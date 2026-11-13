// groups-taskboard-order.js -- putting a task where it belongs in its column
// by hand: the ⋯ Move to top / up / down / to bottom items, Alt+↑/↓/Home/End
// on a focused card or row, the drop slot of a drag, and the optimistic
// order the views draw while that write is in flight.
//
// Every move is `taskboard move --before|--after <task>` with the stage the
// viewer sees (`stage`), so the board refuses it as stale, rather than
// promoting or demoting it, when the neighbour crossed meanwhile. A move
// never names --top or --bottom: "top" is the top of the cell or the rows
// the viewer sees it among, so it is the place before that cell's first
// card, which reads the same in an agent's lane, a custom column, an epic's
// lane and the List.
//
// A task's order is its place among the live tasks of its stage (idea or
// next). In a cell that mixes stages (Blocked holds both) a card moves among
// the cards of its own stage only. Done is ordered by when a task closed,
// so a done card has no order to change (orderMoves answers null).

// ORDER_STEPS are the moves, in menu order, with their label and key.
export const ORDER_STEPS = [
  ['top', 'Move to top', 'Alt+Home'],
  ['up', 'Move up', 'Alt+↑'],
  ['down', 'Move down', 'Alt+↓'],
  ['bottom', 'Move to bottom', 'Alt+End'],
];

const ORDER_KEYS = { ArrowUp: 'up', ArrowDown: 'down', Home: 'top', End: 'bottom' };

const live = (task) => task?.status === 'open' || task?.status === 'blocked';

// orderPeers are the tasks of list task moves among: the live ones of its
// stage, in list order. null for a task with no order of its own (closed).
export function orderPeers(list, task) {
  if (!live(task)) return null;
  return (list || []).filter((t) => live(t) && t.stage === task.stage);
}

// orderMoves are the four moves of task within list, each the move body
// (`{before|after, stage}`) or null where it would not move the task. null
// when the task has no order there.
export function orderMoves(list, task) {
  const peers = orderPeers(list, task);
  const i = peers ? peers.findIndex((t) => t.id === task.id) : -1;
  if (i < 0) return null;
  const last = peers.length - 1;
  const stage = task.stage;
  return {
    top: i > 0 ? { before: peers[0].id, stage } : null,
    up: i > 0 ? { before: peers[i - 1].id, stage } : null,
    down: i < last ? { after: peers[i + 1].id, stage } : null,
    bottom: i < last ? { after: peers[last].id, stage } : null,
  };
}

// orderItems are a card's or row's ⋯ move items (row-menu.js shape), the ones
// that would not move it greyed; none when it is alone among its peers, has
// no order, or the viewer cannot reorder. where names the column or rows the
// moves are within ("Next", "builder's In progress").
export function orderItems(task, list, actions, where = '') {
  if (typeof actions?.reorder !== 'function' || actions.busy) return [];
  const moves = orderMoves(list, task);
  if (!moves || !Object.values(moves).some(Boolean)) return [];
  const within = where ? ` within ${where}` : '';
  return ORDER_STEPS.map(([key, label, keys]) => ({
    label, disabled: !moves[key],
    attrs: { 'data-task-menu': `order-${key}`, title: `${keys}: taskboard move ${moves[key]?.before ? '--before' : '--after'} …${within}` },
    run: () => actions.reorder(task, moves[key]),
  }));
}

// orderKey is the move a key press on a card or row asks for ('' for none):
// Alt with ↑, ↓, Home or End, nothing else held.
export function orderKey(event) {
  if (!event?.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return '';
  return ORDER_KEYS[event.key] || '';
}

// onOrderKey handles a key press on task's card or row in list: it runs the
// move, keeps the key from the page and puts the focus back on the task once
// it is redrawn. It answers whether it took the key.
export function onOrderKey(event, task, list, actions) {
  const step = orderKey(event);
  if (!step || typeof actions?.reorder !== 'function') return false;
  event.preventDefault();
  event.stopPropagation();
  const move = actions.busy ? null : orderMoves(list, task)?.[step];
  if (!move) return true;
  const doc = event.currentTarget?.ownerDocument || globalThis.document;
  const refocus = () => refocusTask(doc, task.id);
  const done = Promise.resolve(actions.reorder(task, move));
  // Once after the optimistic redraw, once after the board came back.
  (globalThis.requestAnimationFrame || ((fn) => globalThis.setTimeout(fn, 0)))(refocus);
  void done.then(refocus);
  return true;
}

// refocusTask focuses the task's card or row again (its title, which the
// keys are pressed on), after a redraw that moved it.
export function refocusTask(doc, id) {
  const host = doc?.querySelector?.(`[data-task-id="${id}"], tr[data-dnd-task="${id}"]`);
  const target = host?.querySelector?.('.tb-title') || host;
  if (target && doc.activeElement !== target) target.focus?.({ preventScroll: true });
}

// dropSide says whether a drag over el points at the slot before it or after
// it: the upper half is before.
export function dropSide(event, el) {
  const box = el?.getBoundingClientRect?.();
  if (!box || !box.height) return 'before';
  return event.clientY < box.top + box.height / 2 ? 'before' : 'after';
}

// applyPendingMove is entry with a move in flight already made, so the view
// shows the task at its new place at once and does not jump when the board
// comes back; a refused move simply stops being pending (it snaps back).
// pending is { id, before|after, stage }.
export function applyPendingMove(entry, pending) {
  const tasks = entry?.tasks;
  const anchorID = pending?.before || pending?.after;
  if (!tasks || !anchorID || anchorID === pending.id) return entry;
  const moving = tasks.find((t) => t.id === pending.id);
  const anchor = tasks.find((t) => t.id === anchorID);
  if (!moving || !anchor) return entry;
  const rest = tasks.filter((t) => t.id !== pending.id);
  const at = rest.indexOf(anchor) + (pending.before ? 0 : 1);
  const moved = moving.stage === anchor.stage ? moving : { ...moving, stage: anchor.stage };
  return { ...entry, tasks: [...rest.slice(0, at), moved, ...rest.slice(at)] };
}
