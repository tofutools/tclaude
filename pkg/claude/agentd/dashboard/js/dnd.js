// dnd.js — drag-and-drop: moving / cloning / retiring agents by dragging
// member rows onto a group's box (its whole <details>, not just the
// header — dropping anywhere over an expanded group counts).
//
// Extracted from dashboard.js in the Stage 2 module split. Owns
// dndDragActive — in-flight member-drag routing state shared with reverse
// dock capture.

import { $, $$, isMacChromeZeroReleaseLeave } from './helpers.js';
import { renderGroupsTab } from './tabs.js';
// refresh()/toast()/confirmModal live in refresh.js;
// lastSnapshot is dashboard.js's shared state (read-only here).
// Deliberate benign module cycles; TDZ-safe.
import { refresh, toast, confirmModal } from './refresh.js';
import {
  openRetireAgentDialog, retireResultNeedsReconcile,
} from './transaction-dialog-controller.js';
import { lastSnapshot } from './dashboard.js';
import { discardQueuedSpawn } from './queued-spawns.js';
import { openCloneAgentDialog } from './action-dialog-controller.js';
import { clearDropBox, createDndOps, dndGesture, dndNode, markDropBox, targetGroupOf } from './dnd-ops.js';

// Drag-and-drop: move a member row from group A onto group B's box
// (anywhere over group B's <details> — header or expanded body) to
// migrate. Optimistic local mutation runs first so the user sees the
// move immediately; the daemon round-trip confirms (or snaps back on
// failure).
//
// Order on success: POST /api/groups/B/members → DELETE
// /api/groups/A/members/{conv}. POST first guarantees the conv is
// never groupless mid-drag — on a failed delete it ends up in both
// groups (visible, recoverable) instead of nowhere (silently lost).
//
// Groups rows and targets are keyed Preact-owned nodes, so snapshot publishes
// retain them during a gesture. This flag routes events; it does not suspend
// the shared poll.
let dndDragActive = false;
// dndSourceUngrouped / dndSourceConversation / dndSourceRetired: which
// virtual group the dragged row comes from. Set in dragstart, cleared
// in dragend. dragover can't read the DataTransfer payload (browsers
// gate getData to the drop event), so these module-level flags are
// how the hover handlers tell an inert no-op (e.g. an ungrouped row
// onto Ungrouped, or a retired row onto Retired) from a real op.
let dndSourceUngrouped = false;
let dndSourceConversation = false;
let dndSourceRetired = false;
// dndSourcePending: the dragged row is a PENDING spawn (JOH-205) — a spawn
// wedged behind a startup gate, not a real agent (no conv-id, no group, no
// permissions). Its ONLY valid drop is the trash, where it routes to a
// DELETE (kill pane + drop rows) rather than a retire. Every hover handler
// keys off this to make a pending source inert over group / ungrouped
// targets and to relabel the trash gesture as a delete. A QUEUED spawn row
// (queued-spawns.js) rides the same flag — also not an agent, also only
// droppable on the trash — and is told apart by its payload's queuedId.
let dndSourcePending = false;
// dndSourceGroup: the real group a dragged member row comes from (''
// for the virtual sources). Like the flags above, dragover/dragenter
// can't read the DataTransfer payload, so this module-level copy is how
// the hover handlers recognise a drop onto the row's OWN group — an
// inert no-op (unless it's a clone) that must not highlight, especially
// now that a drag starts right inside the source group's expanded box.
let dndSourceGroup = '';
// dndSourcePayload: the dragged row's decoded payload, stashed at dragstart
// so a clone can finish from dragend (below) without the DataTransfer, whose
// data browsers only expose to the drop event.
let dndSourcePayload = null;
// dndCloneHoverTarget: the real group a still-green Ctrl/Cmd clone hover
// last pointed at (null otherwise). macOS Chrome can end a native copy drag
// without delivering a usable drop event — and clears metaKey by the time
// anything terminal fires — so the clone is finished from this cached target
// on dragend (or on macOS Chrome's all-zero release dragleave). Cleared on
// every non-clone / inert / off-target hover, on Escape, on leaving the
// document, and by a drop that handled the gesture itself. Mirrors
// group-reorder.js's groupCloneHoverPlan.
let dndCloneHoverTarget = null;
// Every droppable box — real group <details> AND the two droppable
// virtual group <details> (Ungrouped, Retired). The drop target moved
// from the <summary> header to the whole <details> so a release
// anywhere over an expanded group lands in it. The DnD listeners share
// this selector. The Conversations box is a drag SOURCE only.
//
// #dnd-trash (the fixed drag-to-retire bin, dashboard.html) is also a
// target: it carries data-dnd-target-retired, so every handler below —
// which keys off that attribute — treats it exactly like the virtual
// Retired group and routes a drop to runDndRetire. It is a plain <div>,
// not a <details>, hence listed by id rather than the details[...] rules.
//
// In the fused Groups view each box is one node's group: its <details> carries
// data-fleet-node, and data-group-key is the name it is listed by (a peer's
// group@node). A drop there acts on that node (dndContext).
const DND_TARGET_SEL = 'details[data-dnd-target-group],details[data-dnd-target-ungrouped],details[data-dnd-target-retired],#dnd-trash,details[data-fleet-node][data-group-key]';

// dndSameNode: a drop onto a box on another node than its source's is a
// move between nodes, which the fused view's own handler takes (the move
// dialog); the bin stays with the source's node.
function dndSameNode(box) {
  return box.id === 'dnd-trash' || dndNode(box).node === dndSourceNode;
}
// isCloneGesture: a Ctrl/Cmd-held drag onto a REAL-group box with a
// non-retired source. Clone is meaningless for the virtual targets, and
// a retired source always reinstates (never clones). dragover, dragenter
// and the pill all key off this so the hint, the highlight tint and the
// drop branch agree.
// dndSourceNode: the node the dragged row is on ('' for the page's own).
let dndSourceNode = '';
function isCloneGesture(e, box) {
  return (!!e.ctrlKey || !!e.metaKey)
    && !box.hasAttribute('data-dnd-target-ungrouped')
    && !box.hasAttribute('data-dnd-target-retired')
    && !dndSourceRetired;
}
// dndInertOnto: true when a drag from the current source onto `box`
// would change nothing, so the hover handlers skip the highlight + pill
// (and, by not calling preventDefault, stop `drop` from ever firing).
// Folds every no-op into one place:
//   - a row onto the virtual group it already lives in (Ungrouped);
//   - a conversation or an already-retired row onto Retired;
//   - a plain (non-clone) move onto the source's OWN group.
function dndInertOnto(box, isClone) {
  // A pending spawn isn't an agent — it can't join, leave, or be retired.
  // Its only real gesture is delete-via-trash, so it's inert over EVERY
  // target except a retired-target (the trash bin or the virtual Retired
  // group, both carrying data-dnd-target-retired), where the drop handler
  // routes it to runDndDeletePending.
  if (!dndSameNode(box)) return true;
  if (dndSourcePending) return !box.hasAttribute('data-dnd-target-retired');
  if (box.hasAttribute('data-dnd-target-ungrouped')) return dndSourceUngrouped;
  if (box.hasAttribute('data-dnd-target-retired')) return dndSourceRetired || dndSourceConversation;
  // Real-group target: inert only when it's the source's own group and
  // the gesture isn't a clone (cloning a sibling into the same group is
  // a real op). dndSourceGroup is '' for the virtual sources, so this
  // never fires for an add / promote / reinstate onto a real group.
  return !isClone && !!dndSourceGroup && targetGroupOf(box) === dndSourceGroup;
}
// updateDndPill positions + labels the hint pill that tracks the
// cursor during a drag. `info` is null to hide the pill, else
// {text, clone} — text is the action label, clone tints it green.
function updateDndPill(e, info) {
  const pill = $('#dnd-pill');
  if (!info) {
    pill.classList.remove('show', 'clone');
    return;
  }
  // dragover can fire far faster than the label changes. Replacing text on
  // every event invalidates the fixed pill's subtree and can turn a cheap
  // transform-only cursor follow into a full style/paint pass.
  if (pill.textContent !== info.text) pill.textContent = info.text;
  if (pill.classList.contains('clone') !== !!info.clone) {
    pill.classList.toggle('clone', !!info.clone);
  }
  pill.classList.add('show');
  // Offset slightly from the cursor so the pill doesn't sit on top
  // of the user's pointer. clientX/clientY on `dragover` events
  // jitter on some browsers; the offset masks that.
  pill.style.transform = `translate(${e.clientX + 12}px, ${e.clientY + 12}px)`;
}
// showDndTrash / hideDndTrash toggle the fixed drag-to-retire bin. It is
// revealed on dragstart ONLY for a source that a retire would actually
// change — a real-group member or an ungrouped agent — and hidden again
// on dragend. A retired row (already retired) or a plain conversation
// (can't be retired) never shows the bin, mirroring the drop handler's
// `if (sourceRetired || sourceConversation) return;` no-op guard, so the
// bin is never a target that would do nothing.
function showDndTrash(retireable) {
  const bin = $('#dnd-trash');
  if (bin && retireable) bin.classList.add('show');
}
function hideDndTrash() {
  const bin = $('#dnd-trash');
  if (bin) bin.classList.remove('show', 'dnd-drop-over');
}
function bindDnd() {
  const removers = [];
  dndGesture.cancelClone = () => { dndCloneHoverTarget = null; };
  const listen = (target, type, listener, options) => {
    target.addEventListener(type, listener, options);
    removers.push(() => target.removeEventListener(type, listener, options));
  };
  // The whole <tr> is draggable="true", so a press on an in-row control
  // (focus / hide eye, the ⚙ cog and its menu items, the status dot, the
  // click-to-edit name / cwd / role cells, the promote / reinstate
  // buttons) plus even a 1px cursor wobble makes the browser start
  // dragging the ROW instead of firing that control's click — the
  // focus/unfocus button then silently "does nothing".
  //
  // We can't fix this in dragstart: native DnD fires dragstart at the
  // drag SOURCE NODE — the <tr> itself — so e.target there is always the
  // row, never the control that was pressed, and a closest('button') test
  // would never match. pointerdown, by contrast, targets the actual
  // element under the cursor, and it fires BEFORE the drag is initiated.
  // So if the press landed on an interactive descendant, turn the row's
  // draggable OFF for the duration of this gesture so the click lands; a
  // press on a plain cell (id, last-hook, descr, …) leaves it ON so the
  // row can still be dragged between groups.
  //
  // The suppression is strictly gesture-scoped: pointerup / pointercancel
  // restore draggability immediately, so the row is never left
  // un-draggable between gestures (it doesn't have to wait for the next
  // pointerdown or a later snapshot publish to re-arm). Restoring on pointerup is
  // safe — the drag-vs-click decision is made during the move BEFORE
  // pointerup, so re-enabling at gesture end can't trigger an unwanted
  // drag. dndSuppressedRow remembers which row we touched so we restore
  // exactly that one (and only when we actually disabled it).
  let dndSuppressedRow = null;
  let activeSourceRow = null;
  const restoreDraggable = () => {
    if (!dndSuppressedRow) return;
    dndSuppressedRow.draggable = true;
    dndSuppressedRow = null;
  };
  listen(document, 'pointerdown', (e) => {
    const row = e.target.closest('.dnd-draggable');
    if (!row) return;
    const ctl = e.target.closest('button, a, input, select, textarea, label, [data-act], [contenteditable]');
    if (ctl && row.contains(ctl)) {
      row.draggable = false;
      dndSuppressedRow = row;
    }
  });
  listen(document, 'pointerup', restoreDraggable);
  listen(document, 'pointercancel', restoreDraggable);
  listen(document, 'keydown', (e) => {
    if (dndDragActive && e.key === 'Escape') dndCloneHoverTarget = null;
  });
  const endDndDrag = (e) => {
    if (!dndDragActive) return;
    // A source-local listener also invokes this when a structural Preact
    // publish detached the row before dragend could bubble to document.
    // Capture a still-live clone hover before teardown: when no drop consumed
    // it, this is macOS Chrome finishing a copy without a usable drop event.
    const cloneTarget = dndCloneHoverTarget;
    const clonePayload = dndSourcePayload;
    dndCloneHoverTarget = null;
    dndSourcePayload = null;
    dndDragActive = false;
    dndSourceUngrouped = false;
    dndSourceConversation = false;
    dndSourceRetired = false;
    dndSourcePending = false;
    dndSourceGroup = '';
    dndSourceNode = '';
    hideDndTrash();
    const row = activeSourceRow || e?.target?.closest?.('.dnd-draggable');
    activeSourceRow?.removeEventListener('dragend', endDndDrag);
    activeSourceRow = null;
    if (row) row.classList.remove('dnd-source-row');
    $$('.dnd-drop-over').forEach(clearDropBox);
    $('#dnd-pill')?.classList.remove('show', 'clone');
    if (cloneTarget && clonePayload) {
      // runDndClone opens the clone dialog and refreshes on every path.
      const ctx = ops.context(cloneTarget.where);
      if (ops.allowed(ctx, 'clone', clonePayload.agent, cloneTarget.group)) ops.clone(clonePayload, cloneTarget.group, ctx);
      else ops.refused(ctx, 'clone');
      return;
    }
    refresh();
  };
  listen(document, 'dragstart', (e) => {
    const row = e.target.closest('.dnd-draggable');
    if (!row) return;
    const conv = row.getAttribute('data-dnd-conv');
    // The rotation-immune stable agent_id (falls back to the conv-id for a
    // pre-identity / plain-conversation row). Carried alongside `conv` in
    // the payload: the runDnd* endpoint calls route by `agent` (resolved
    // server-side via agent.ResolveSelector), while the optimistic
    // lastSnapshot splice in runDndMove still correlates on `conv` (member
    // rows are keyed by conv_id there) — so neither path breaks (JOH-322).
    const agent = row.getAttribute('data-dnd-agent') || conv;
    const sourceGroup = row.getAttribute('data-dnd-source-group');
    const sourceUngrouped = row.hasAttribute('data-dnd-source-ungrouped');
    const sourceConversation = row.hasAttribute('data-dnd-source-conversation');
    const sourceRetired = row.hasAttribute('data-dnd-source-retired');
    // A PENDING spawn (JOH-205): a stuck spawn, not an agent. Its only
    // valid drag is to the trash (→ delete). It has no conv-id / source
    // group / virtual-source flags, so it needs its own source predicate.
    const sourcePending = row.hasAttribute('data-dnd-pending');
    const queuedId = sourcePending ? row.getAttribute('data-dnd-queued') || '' : '';
    const queuedGroup = queuedId ? row.getAttribute('data-dnd-queued-group') || '' : '';
    const label = row.getAttribute('data-dnd-label') || conv;
    // A draggable row is a real-group member (has a source group), a
    // virtual-Ungrouped row, a virtual-Conversations row, a virtual-Retired
    // row, or a pending spawn. Anything else isn't a valid drag.
    if (!conv || (!sourceGroup && !sourceUngrouped && !sourceConversation && !sourceRetired && !sourcePending)) return;
    // Stash the payload on the DataTransfer so the eventual drop can
    // read it without globals. The MIME type 'text/plain' is the
    // most-supported channel; the JSON body keeps the encoding
    // self-describing. We allow both move (default) and copy effects
    // so Ctrl-drag can flip the cursor hint via dropEffect.
    const where = dndNode(row);
    const payloadObj = {conv, agent, sourceGroup: sourceGroup || '', sourceUngrouped, sourceConversation, sourceRetired, sourcePending, queuedId, queuedGroup, label,
      sourceNode: where.node, sourceNodeName: where.name};
    const payload = JSON.stringify(payloadObj);
    e.dataTransfer.setData('application/x-tclaude-member', payload);
    e.dataTransfer.setData('text/plain', payload);
    e.dataTransfer.effectAllowed = 'copyMove';
    row.classList.add('dnd-source-row');
    activeSourceRow?.removeEventListener('dragend', endDndDrag);
    activeSourceRow = row;
    // dragend targets its source even after that source leaves the document.
    row.addEventListener('dragend', endDndDrag, { once: true });
    dndDragActive = true;
    dndSourceUngrouped = sourceUngrouped;
    dndSourceConversation = sourceConversation;
    dndSourceRetired = sourceRetired;
    dndSourcePending = sourcePending;
    dndSourceGroup = sourceGroup || '';
    dndSourceNode = where.node;
    dndSourcePayload = payloadObj;
    dndCloneHoverTarget = null;
    // Reveal the fixed drag-to-retire bin for a retireable source (a
    // real-group member or an ungrouped agent — not an already-retired
    // row, not a plain conversation) OR a pending spawn (where the bin is
    // its only valid target), so clearing never means dragging all the way
    // to the possibly-offscreen Retired group.
    showDndTrash(sourcePending || (!sourceRetired && !sourceConversation));
  });
  listen(document, 'dragend', endDndDrag);
  listen(document, 'dragover', (e) => {
    if (!dndDragActive) return;
    const box = e.target.closest(DND_TARGET_SEL);
    if (!box) {
      dndCloneHoverTarget = null;
      updateDndPill(e, null);
      return;
    }
    const targetUngrouped = box.hasAttribute('data-dnd-target-ungrouped');
    const targetRetired = box.hasAttribute('data-dnd-target-retired');
    const isClone = isCloneGesture(e, box);
    // No-op drops — don't preventDefault (so `drop` never fires) and
    // don't show a hint. dndInertOnto folds in every inert case: a row
    // onto the virtual group it already lives in, a conversation /
    // retired row onto Retired, and a plain move onto the source's own
    // group (the common one now that a drag starts inside that box).
    // Clear any highlight too, so toggling Ctrl/Cmd over the source's
    // own group (clone ⇄ no-op) doesn't strand a stale tint on the box.
    if (dndInertOnto(box, isClone)) {
      dndCloneHoverTarget = null;
      clearDropBox(box);
      updateDndPill(e, null);
      return;
    }
    e.preventDefault(); // required for drop to fire on this element
    dndCloneHoverTarget = isClone ? { group: targetGroupOf(box), where: dndNode(box) } : null;
    // Own the highlight here rather than leaning on the dragenter that
    // (usually) preceded us: when the gesture flips inert→live in place
    // — e.g. pressing Ctrl/Cmd to clone into the source's own group,
    // which fires no new dragenter — dragover is the only handler that
    // runs, so it must add dnd-drop-over itself. Idempotent on a box
    // dragenter already lit.
    markDropBox(box, isClone ? 'clone' : 'move');
    e.dataTransfer.dropEffect = isClone ? 'copy' : 'move';
    let text;
    if (targetRetired && dndSourcePending) text = dndSourcePayload?.queuedId ? '🗑 discard queued agent' : '🗑 delete stuck spawn';
    else if (targetRetired) text = '↓ retire — demote to conversation';
    else if (targetUngrouped) text = dndSourceRetired ? '↓ reinstate (no group)' : dndSourceConversation ? '↓ promote (no group)' : '↓ remove from group';
    else if (isClone) text = '→ clone into group';
    else if (dndSourceRetired) text = '→ reinstate + join group';
    else if (dndSourceConversation) text = '→ promote into group';
    else if (dndSourceUngrouped) text = '→ add to group';
    else text = '→ move to group';
    updateDndPill(e, {text, clone: isClone});
  });
  listen(document, 'dragenter', (e) => {
    if (!dndDragActive) return;
    const box = e.target.closest(DND_TARGET_SEL);
    if (!box) return;
    // No highlight for the inert no-ops — mirror the dragover guard so
    // the box only lights up when a drop here would actually do something.
    if (dndInertOnto(box, isCloneGesture(e, box))) return;
    box.classList.add('dnd-drop-over');
  });
  listen(document, 'dragleave', (e) => {
    if (dndDragActive) {
      // macOS Chrome's all-zero release dragleave (see helpers.js) arrives
      // 0.5–1s before dragend with no usable drop. Finish the still-green
      // clone immediately instead of making the operator wait for dragend.
      const macChromeZeroRelease = isMacChromeZeroReleaseLeave(e);
      const releaseBox = e.target.closest(DND_TARGET_SEL);
      if (macChromeZeroRelease && dndCloneHoverTarget
          && releaseBox?.classList.contains('dnd-effect-clone')) {
        endDndDrag(e);
        return;
      }
      // Leaving the document can finish as a native copy in another window
      // or application; invalidate the cached clone so its later dragend
      // cannot open a stale dashboard clone dialog.
      const outside = !macChromeZeroRelease && !e.relatedTarget && (
        e.clientX <= 0 || e.clientY <= 0
        || e.clientX >= window.innerWidth - 1
        || e.clientY >= window.innerHeight - 1
      );
      if (outside) dndCloneHoverTarget = null;
    }
    const box = e.target.closest(DND_TARGET_SEL);
    if (!box) return;
    // dragleave fires when the cursor crosses into a child element too;
    // only drop the highlight once the cursor has actually left the box.
    if (box.contains(e.relatedTarget)) return;
    clearDropBox(box);
  });
  listen(document, 'drop', async (e) => {
    // A group-reorder drag (group-reorder.js) carries this custom MIME and
    // deliberately never sets text/plain. Ignore such a drop outright — both
    // modules add a document-level drop listener, and this handler does NOT
    // gate on dndDragActive, so without this guard it would preventDefault +
    // clear the shared pill on a reorder drop before the JSON.parse below
    // happened to bail. The explicit check keeps the two handlers cleanly
    // separated instead of relying on a parse failure.
    if (e.dataTransfer.types.includes('application/x-tclaude-group')) return;
    // A dock profile/role drag (dock-dnd.js) likewise carries its own custom
    // MIME and no text/plain; it opens the spawn dialog, never a membership
    // move. Bail so this handler doesn't preventDefault + clear the shared pill
    // on a dock drop before the JSON.parse below happened to fail on it.
    if (e.dataTransfer.types.includes('application/x-tclaude-dock-item')) return;
    // A process-template drag (process-template-dnd.js) targets the shared bin
    // to delete a template and, like the two above, carries only its own custom
    // MIME. Bail for the same reason: this handler does not gate on
    // dndDragActive, so it would otherwise preventDefault + clear the shared
    // pill on a template drop.
    if (e.dataTransfer.types.includes('application/x-tclaude-process-template')) return;
    // Likewise a saved-process chip drag (groups-saved-process-dnd.js), which
    // removes a process from its group on the bin.
    if (e.dataTransfer.types.includes('application/x-tclaude-saved-process')) return;
    // A terminal-tab drag (terminal-shell-island.js) detaches or reattaches a
    // terminal and, like the three above, carries only its own custom MIME. It
    // also adds a document-level drop listener for the duration of the drag.
    // Membership boxes live in a tab panel that is hidden while the Terminals
    // tab is showing, so this is currently unreachable rather than a live bug —
    // the guard keeps the separation explicit instead of resting on that.
    if (e.dataTransfer.types.includes('application/x-tclaude-terminal-tab')) return;
    // dragover only accepts (preventDefault) while this document's member
    // drag is live, so an inactive drop is one the macOS release path above
    // already finished — or a stray foreign drop. Never act on it twice.
    if (!dndDragActive) return;
    const box = e.target.closest(DND_TARGET_SEL);
    if (!box) return;
    e.preventDefault();
    // This drop handles the gesture itself; dragend must not finish it again.
    dndCloneHoverTarget = null;
    // Read the painted clone tint before clearing it (see isClone below).
    const paintedClone = box.classList.contains('dnd-effect-clone');
    clearDropBox(box);
    $('#dnd-pill').classList.remove('show', 'clone');
    const raw = e.dataTransfer.getData('application/x-tclaude-member')
      || e.dataTransfer.getData('text/plain');
    let payload;
    try { payload = JSON.parse(raw); } catch (_) { return; }
    if (!payload || !payload.conv) return;
    const targetUngrouped = box.hasAttribute('data-dnd-target-ungrouped');
    const targetRetired = box.hasAttribute('data-dnd-target-retired');
    const targetGroup = targetGroupOf(box);
    // The drop acts on its node (the bin: the source's); a box on another
    // node is the fused view's move between nodes, never seen here.
    const where = box.id === 'dnd-trash' ? { node: payload.sourceNode || '', name: payload.sourceNodeName || '' } : dndNode(box);
    if (where.node !== (payload.sourceNode || '')) return;
    const ctx = ops.context(where);
    const sourceUngrouped = !!payload.sourceUngrouped;
    const sourceConversation = !!payload.sourceConversation;
    const sourceRetired = !!payload.sourceRetired;
    const sourcePending = !!payload.sourcePending;
    // Clone applies only to a real-group target, never to a retired
    // source (that path reinstates). Follow the native copy operation and the
    // green tint dragover painted first: macOS Chrome clears metaKey on the
    // terminal drop even though the accepted operation is still a copy.
    const cloneIntent = e.dataTransfer?.dropEffect === 'copy' || paintedClone
      || !!e.ctrlKey || !!e.metaKey;
    const isClone = cloneIntent && !targetUngrouped && !targetRetired && !sourceRetired;
    // A restricted peer takes only the gestures its grants on the agent cover.
    const gesture = sourcePending ? '' : isClone ? 'clone' : targetRetired && !sourceRetired && !sourceConversation ? 'retire' : '';
    if (!ops.allowed(ctx, gesture, payload.agent, targetGroup)) { ops.refused(ctx, gesture); return; }

    // Confirmation gate. Each runDnd* function below opens its own
    // tailored confirmation modal as its first step, BEFORE any
    // daemon call or optimistic snapshot mutation. The no-op short-
    // circuits above have already returned, so a modal is only ever
    // shown for a gesture that would really change something — an
    // inert drop never reaches a runDnd* function and never prompts.
    // On Cancel / Escape / outside-click the runDnd* function calls refresh()
    // to reconcile any drag-local presentation, then returns without touching
    // the daemon or lastSnapshot.
    // runDndRetire opens the keyed transaction dialog — shutdown/worktree
    // choices and all — so drag, row, and palette retire share one owner.

    // A pending spawn can only ever be dropped on a retired-target (the
    // trash bin or the virtual Retired group — dndInertOnto makes every
    // other target inert for it). Both mean the same thing: delete the
    // stuck spawn. This branch precedes the retire branch below because a
    // pending row is not a real agent and must never take the retire path.
    if (sourcePending) {
      if (!targetRetired) return; // inert everywhere else (see dndInertOnto)
      if (payload.queuedId) {
        // Same confirm + DELETE as the row's trash button; it refreshes on
        // success, so only a cancel or failure needs the reconcile here.
        if (!await discardQueuedSpawn({ id: payload.queuedId, group: payload.queuedGroup, draft: { name: payload.label }, ...(ctx.node ? { fleet_node: ctx.node } : {}) })) await refresh();
        return;
      }
      await ops.deletePending(payload, ctx);
      return;
    }
    // Target = the virtual Retired group → retire the agent,
    // demoting it back to a plain conversation.
    if (targetRetired) {
      if (sourceRetired || sourceConversation) return; // no-op (see dragover)
      await ops.retire(payload, ctx);
      return;
    }
    // Target = the virtual Ungrouped group.
    if (targetUngrouped) {
      if (sourceUngrouped) return; // already ungrouped — no-op
      if (sourceRetired) {
        // A retired agent dropped here → reinstate to an active
        // agent, joining no group.
        await ops.reinstate(payload, null, ctx);
        return;
      }
      if (sourceConversation) {
        // A conversation dropped here → promote to agent, no group.
        await ops.promote(payload, ctx);
        return;
      }
      // A real-group member → remove from that group.
      await ops.remove(payload, ctx);
      return;
    }
    // Target = a real group.
    if (sourceRetired) {
      // A retired agent dragged onto a group → reinstate + join.
      await ops.reinstate(payload, targetGroup, ctx);
      return;
    }
    if (isClone) {
      // Clone forks a sibling into the target group. Works whether
      // the source is grouped or ungrouped — runDndClone clones the
      // conv then POSTs the clone into the drop-target group.
      await ops.clone(payload, targetGroup, ctx);
      return;
    }
    if (sourceUngrouped || sourceConversation) {
      // An ungrouped agent OR a conversation dragged onto a group →
      // pure add. The membership write promotes a conversation.
      await ops.add(payload, targetGroup, ctx);
      return;
    }
    // Real group → real group move. Move-onto-self is a no-op.
    if (payload.sourceGroup === targetGroup) return;
    await ops.move(payload, targetGroup, ctx);
  });

  let cleaned = false;
  return () => {
    if (cleaned) return;
    cleaned = true;
    for (const remove of removers.splice(0).reverse()) remove();
    restoreDraggable();
    activeSourceRow?.removeEventListener('dragend', endDndDrag);
    activeSourceRow = null;
    dndDragActive = false;
    dndSourceUngrouped = false;
    dndSourceConversation = false;
    dndSourceRetired = false;
    dndSourcePending = false;
    dndSourceGroup = '';
    dndSourceNode = '';
    dndSourcePayload = null;
    dndCloneHoverTarget = null;
    hideDndTrash();
    $$('.dnd-source-row').forEach((row) => row.classList.remove('dnd-source-row'));
    $$('.dnd-drop-over').forEach(clearDropBox);
    $('#dnd-pill')?.classList.remove('show', 'clone');
  };
}

// ops are the drop operations (dnd-ops.js) on this page.
const ops = createDndOps({
  confirm: (options) => confirmModal(options), toast: (...args) => toast(...args), refresh: () => refresh(),
  render: () => renderGroupsTab(), getSnapshot: () => lastSnapshot,
  openClone: (...args) => openCloneAgentDialog(...args), openRetire: (...args) => openRetireAgentDialog(...args),
  retireNeedsReconcile: (result) => retireResultNeedsReconcile(result),
});

export { bindDnd, dndDragActive };
