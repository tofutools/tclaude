// dnd.js — drag-and-drop: moving / cloning / retiring agents by dragging
// member rows onto group headers.
//
// Extracted from dashboard.js in the Stage 2 module split. Owns
// dndDragActive — the in-flight-drag flag refreshSuspended consults.

import { $, $$ } from './helpers.js';
import { renderGroupsTab } from './tabs.js';
// refresh()/toast()/confirmModal/retireConfirm/retireToast live in refresh.js;
// lastSnapshot is dashboard.js's shared state (read-only here).
// Deliberate benign cycles (see render.js); TDZ-safe.
import { refresh, toast, confirmModal, retireConfirm, retireToast, maybeHandleDanglingRetire } from './refresh.js';
import { lastSnapshot } from './dashboard.js';

// Drag-and-drop: move a member row from group A onto group B's
// <summary> header to migrate. Optimistic local mutation runs first
// so the user sees the move immediately; the daemon round-trip
// confirms (or snaps back on failure).
//
// Order on success: POST /api/groups/B/members → DELETE
// /api/groups/A/members/{conv}. POST first guarantees the conv is
// never groupless mid-drag — on a failed delete it ends up in both
// groups (visible, recoverable) instead of nowhere (silently lost).
//
// Auto-refresh suspends while a drag is in flight via the
// dndDragActive flag below — refreshSuspended() checks it — so a 2s
// tick doesn't blow our optimistic mutation away while the
// round-trip is mid-air. The drag deliberately does NOT share the
// modal suspension: a single shared boolean let a drag and a modal
// clobber each other's reset, which is how auto-refresh used to
// wedge after a drag-and-drop retire.
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
// Every droppable summary — real group headers AND the two droppable
// virtual group headers (Ungrouped, Retired). The DnD listeners share
// this selector. The Conversations header is a drag SOURCE only.
const DND_TARGET_SEL = 'summary[data-dnd-target-group],summary[data-dnd-target-ungrouped],summary[data-dnd-target-retired]';
// updateDndPill positions + labels the hint pill that tracks the
// cursor during a drag. `info` is null to hide the pill, else
// {text, clone} — text is the action label, clone tints it green.
function updateDndPill(e, info) {
  const pill = $('#dnd-pill');
  if (!info) {
    pill.classList.remove('show', 'clone');
    return;
  }
  pill.textContent = info.text;
  pill.classList.toggle('clone', !!info.clone);
  pill.classList.add('show');
  // Offset slightly from the cursor so the pill doesn't sit on top
  // of the user's pointer. clientX/clientY on `dragover` events
  // jitter on some browsers; the offset masks that.
  pill.style.transform = `translate(${e.clientX + 12}px, ${e.clientY + 12}px)`;
}
function bindDnd() {
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
  // pointerdown or the 2s re-render to re-arm). Restoring on pointerup is
  // safe — the drag-vs-click decision is made during the move BEFORE
  // pointerup, so re-enabling at gesture end can't trigger an unwanted
  // drag. dndSuppressedRow remembers which row we touched so we restore
  // exactly that one (and only when we actually disabled it).
  let dndSuppressedRow = null;
  const restoreDraggable = () => {
    if (!dndSuppressedRow) return;
    dndSuppressedRow.draggable = true;
    dndSuppressedRow = null;
  };
  document.addEventListener('pointerdown', (e) => {
    const row = e.target.closest('.dnd-draggable');
    if (!row) return;
    const ctl = e.target.closest('button, a, input, select, textarea, label, [data-act], [contenteditable]');
    if (ctl && row.contains(ctl)) {
      row.draggable = false;
      dndSuppressedRow = row;
    }
  });
  document.addEventListener('pointerup', restoreDraggable);
  document.addEventListener('pointercancel', restoreDraggable);
  document.addEventListener('dragstart', (e) => {
    const row = e.target.closest('.dnd-draggable');
    if (!row) return;
    const conv = row.getAttribute('data-dnd-conv');
    const sourceGroup = row.getAttribute('data-dnd-source-group');
    const sourceUngrouped = row.hasAttribute('data-dnd-source-ungrouped');
    const sourceConversation = row.hasAttribute('data-dnd-source-conversation');
    const sourceRetired = row.hasAttribute('data-dnd-source-retired');
    const label = row.getAttribute('data-dnd-label') || conv;
    // A draggable row is a real-group member (has a source group), a
    // virtual-Ungrouped row, a virtual-Conversations row, or a
    // virtual-Retired row. Anything else isn't a valid drag.
    if (!conv || (!sourceGroup && !sourceUngrouped && !sourceConversation && !sourceRetired)) return;
    // Stash the payload on the DataTransfer so the eventual drop can
    // read it without globals. The MIME type 'text/plain' is the
    // most-supported channel; the JSON body keeps the encoding
    // self-describing. We allow both move (default) and copy effects
    // so Ctrl-drag can flip the cursor hint via dropEffect.
    const payload = JSON.stringify({conv, sourceGroup: sourceGroup || '', sourceUngrouped, sourceConversation, sourceRetired, label});
    e.dataTransfer.setData('application/x-tclaude-member', payload);
    e.dataTransfer.setData('text/plain', payload);
    e.dataTransfer.effectAllowed = 'copyMove';
    row.classList.add('dnd-source-row');
    dndDragActive = true;
    dndSourceUngrouped = sourceUngrouped;
    dndSourceConversation = sourceConversation;
    dndSourceRetired = sourceRetired;
    // dndDragActive (set above) is what suspends auto-refresh for the
    // duration of the drag — see refreshSuspended().
  });
  document.addEventListener('dragend', (e) => {
    // Clear the drag state FIRST, ahead of any DOM cleanup below: if
    // a classList / query call here ever threw, auto-refresh must
    // still come back. dragend fires for every drag that had a
    // dragstart — a successful drop, an Escape-cancel, or a release
    // over nothing — so this is the one guaranteed reset covering
    // every drag-end outcome (join, leave, retire, reinstate,
    // promote, clone, cancelled drop, error path).
    dndDragActive = false;
    dndSourceUngrouped = false;
    dndSourceConversation = false;
    dndSourceRetired = false;
    const row = e.target.closest('.dnd-draggable');
    if (row) row.classList.remove('dnd-source-row');
    // Clear any lingering hover highlight (Firefox sometimes fires
    // dragend without a final dragleave on the target).
    $$('summary.dnd-drop-over').forEach(s => s.classList.remove('dnd-drop-over', 'dnd-effect-clone'));
    $('#dnd-pill').classList.remove('show', 'clone');
    refresh();
  });
  document.addEventListener('dragover', (e) => {
    if (!dndDragActive) return;
    const summary = e.target.closest(DND_TARGET_SEL);
    if (!summary) {
      updateDndPill(e, null);
      return;
    }
    const targetUngrouped = summary.hasAttribute('data-dnd-target-ungrouped');
    const targetRetired = summary.hasAttribute('data-dnd-target-retired');
    // No-op drops — don't preventDefault (so `drop` never fires) and
    // don't show a hint:
    //   - a row onto the virtual group it already lives in;
    //   - a plain conversation onto Retired (only agents can retire).
    if ((targetUngrouped && dndSourceUngrouped) ||
        (targetRetired && (dndSourceRetired || dndSourceConversation))) {
      updateDndPill(e, null);
      return;
    }
    e.preventDefault(); // required for drop to fire on this element
    // Clone is meaningful only for a real-group target, and never for
    // a retired source (that path reinstates, it doesn't clone).
    const isClone = (!!e.ctrlKey || !!e.metaKey) && !targetUngrouped && !targetRetired && !dndSourceRetired;
    e.dataTransfer.dropEffect = isClone ? 'copy' : 'move';
    summary.classList.toggle('dnd-effect-clone', isClone);
    let text;
    if (targetRetired) text = '↓ retire — demote to conversation';
    else if (targetUngrouped) text = dndSourceRetired ? '↓ reinstate (no group)' : dndSourceConversation ? '↓ promote (no group)' : '↓ remove from group';
    else if (isClone) text = '→ clone into group';
    else if (dndSourceRetired) text = '→ reinstate + join group';
    else if (dndSourceConversation) text = '→ promote into group';
    else if (dndSourceUngrouped) text = '→ add to group';
    else text = '→ move to group';
    updateDndPill(e, {text, clone: isClone});
  });
  document.addEventListener('dragenter', (e) => {
    if (!dndDragActive) return;
    const summary = e.target.closest(DND_TARGET_SEL);
    if (!summary) return;
    // No highlight for the inert no-ops — mirror the dragover guard.
    if ((summary.hasAttribute('data-dnd-target-ungrouped') && dndSourceUngrouped) ||
        (summary.hasAttribute('data-dnd-target-retired') && (dndSourceRetired || dndSourceConversation))) return;
    summary.classList.add('dnd-drop-over');
  });
  document.addEventListener('dragleave', (e) => {
    const summary = e.target.closest(DND_TARGET_SEL);
    if (!summary) return;
    // dragleave fires when the cursor enters a child element too;
    // only remove the highlight when the cursor has actually left
    // the summary.
    if (summary.contains(e.relatedTarget)) return;
    summary.classList.remove('dnd-drop-over', 'dnd-effect-clone');
  });
  document.addEventListener('drop', async (e) => {
    // A group-reorder drag (group-reorder.js) carries this custom MIME and
    // deliberately never sets text/plain. Ignore such a drop outright — both
    // modules add a document-level drop listener, and this handler does NOT
    // gate on dndDragActive, so without this guard it would preventDefault +
    // clear the shared pill on a reorder drop before the JSON.parse below
    // happened to bail. The explicit check keeps the two handlers cleanly
    // separated instead of relying on a parse failure.
    if (e.dataTransfer.types.includes('application/x-tclaude-group')) return;
    const summary = e.target.closest(DND_TARGET_SEL);
    if (!summary) return;
    e.preventDefault();
    summary.classList.remove('dnd-drop-over', 'dnd-effect-clone');
    $('#dnd-pill').classList.remove('show', 'clone');
    const raw = e.dataTransfer.getData('application/x-tclaude-member')
      || e.dataTransfer.getData('text/plain');
    let payload;
    try { payload = JSON.parse(raw); } catch (_) { return; }
    if (!payload || !payload.conv) return;
    const targetUngrouped = summary.hasAttribute('data-dnd-target-ungrouped');
    const targetRetired = summary.hasAttribute('data-dnd-target-retired');
    const targetGroup = summary.getAttribute('data-dnd-target-group');
    const sourceUngrouped = !!payload.sourceUngrouped;
    const sourceConversation = !!payload.sourceConversation;
    const sourceRetired = !!payload.sourceRetired;
    // Clone applies only to a real-group target, never to a retired
    // source (that path reinstates).
    const isClone = (!!e.ctrlKey || !!e.metaKey) && !targetUngrouped && !targetRetired && !sourceRetired;

    // Confirmation gate. Each runDnd* function below opens its own
    // tailored confirmation modal as its first step, BEFORE any
    // daemon call or optimistic snapshot mutation. The no-op short-
    // circuits above have already returned, so a modal is only ever
    // shown for a gesture that would really change something — an
    // inert drop never reaches a runDnd* function and never prompts.
    // On Cancel / Escape / outside-click the runDnd* function calls
    // refresh() (the modal suspended auto-refresh while it was open)
    // and returns without touching the daemon or lastSnapshot.
    // runDndRetire uses the richer retireConfirm modal — shutdown
    // checkbox and all — so a retire-by-drag and the per-row retire
    // button ask the identical question.

    // Target = the virtual Retired group → retire the agent,
    // demoting it back to a plain conversation.
    if (targetRetired) {
      if (sourceRetired || sourceConversation) return; // no-op (see dragover)
      await runDndRetire(payload);
      return;
    }
    // Target = the virtual Ungrouped group.
    if (targetUngrouped) {
      if (sourceUngrouped) return; // already ungrouped — no-op
      if (sourceRetired) {
        // A retired agent dropped here → reinstate to an active
        // agent, joining no group.
        await runDndReinstate(payload, null);
        return;
      }
      if (sourceConversation) {
        // A conversation dropped here → promote to agent, no group.
        await runDndPromoteToUngrouped(payload);
        return;
      }
      // A real-group member → remove from that group.
      await runDndRemoveFromGroup(payload);
      return;
    }
    // Target = a real group.
    if (sourceRetired) {
      // A retired agent dragged onto a group → reinstate + join.
      await runDndReinstate(payload, targetGroup);
      return;
    }
    if (isClone) {
      // Clone forks a sibling into the target group. Works whether
      // the source is grouped or ungrouped — runDndClone clones the
      // conv then POSTs the clone into the drop-target group.
      await runDndClone(payload, targetGroup);
      return;
    }
    if (sourceUngrouped || sourceConversation) {
      // An ungrouped agent OR a conversation dragged onto a group →
      // pure add. The membership write promotes a conversation.
      await runDndAddToGroup(payload, targetGroup);
      return;
    }
    // Real group → real group move. Move-onto-self is a no-op.
    if (payload.sourceGroup === targetGroup) return;
    await runDndMove(payload, targetGroup);
  });
}

// runDndClone forks the source conv via POST /api/agents/{conv}/clone,
// then adds the new conv to the target group with POST
// /api/groups/{target}/members. The clone inherits all source
// memberships (including the source group) — the target-group POST
// is the differentiator: it ensures the clone is in the dropped-on
// group even when source wasn't already there.
//
// No optimistic UI: the new conv-id isn't known until the response
// lands, and inventing a placeholder row would confuse the user
// when the real conv-id replaces it on the next poll. Just await
// both calls and refresh.
async function runDndClone(payload, targetGroup) {
  const {conv, label} = payload;
  const confirmed = await confirmModal({
    title: 'Clone agent into group?',
    body: `Fork a new sibling agent from "${label}" and add the clone to `
      + `group "${targetGroup}". The original keeps running; the clone is a `
      + `sibling conversation that inherits the original's identity and a `
      + `copy of its conversation history.`,
    meta: label,
    okLabel: 'Clone',
  });
  if (!confirmed) { await refresh(); return; }
  try {
    const cloneRes = await fetch(`/api/agents/${encodeURIComponent(conv)}/clone`, {
      method: 'POST', credentials: 'same-origin',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({}),
    });
    if (!cloneRes.ok) {
      toast(`clone failed: ${await cloneRes.text()}`, true);
      return;
    }
    const out = await cloneRes.json();
    const newConv = out.new_conv;
    if (!newConv) {
      toast(`clone: response missing new_conv`, true);
      return;
    }
    // Add the new conv to the drop target group. Idempotent if the
    // clone already inherited that group from the source's
    // memberships.
    const addRes = await fetch(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
      method: 'POST', credentials: 'same-origin',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({conv: newConv}),
    });
    if (!addRes.ok) {
      toast(`clone add-to-${targetGroup} failed: ${await addRes.text()}`, true);
      return;
    }
    toast(`cloned ${label} → ${targetGroup} (new ${newConv.slice(0,8)})`);
  } catch (err) {
    toast(`clone failed: ${err && err.message || err}`, true);
  } finally {
    // The confirm modal suspended auto-refresh while it was open, and
    // the dragend-fired refresh() bailed for the same reason — so the
    // dashboard has not re-rendered since before the drag. Sync now.
    await refresh();
  }
}

// runDndMove performs the optimistic local mutation, then the
// POST B → DELETE A sequence. Failure of either step rolls back
// the local mutation and surfaces a toast.
async function runDndMove(payload, targetGroup) {
  const {conv, sourceGroup, label} = payload;
  // Confirm BEFORE the lastSnapshot read + optimistic splice below,
  // so a cancelled move leaves the snapshot — and the render —
  // completely untouched.
  const confirmed = await confirmModal({
    title: 'Move agent to another group?',
    body: `Move "${label}" out of group "${sourceGroup}" and into group `
      + `"${targetGroup}". Its membership of "${sourceGroup}" is removed and a `
      + `membership of "${targetGroup}" is added.`,
    meta: label,
    okLabel: 'Move',
  });
  if (!confirmed) { await refresh(); return; }
  // Every post-confirm exit — a guard-clause return, the partial-
  // failure return, success, or an error — funnels through the
  // finally so the dashboard re-syncs. The dragend-fired refresh()
  // bailed while the confirm modal was open (refreshSuspended() saw
  // it), so without this a confirmed-then-aborted move would leave
  // the dashboard showing stale state until the next 2s tick.
  try {
    if (!lastSnapshot || !Array.isArray(lastSnapshot.groups)) {
      toast(`move: dashboard snapshot not loaded`, true);
      return;
    }
    // Snapshot the source row so we can restore it on rollback +
    // append it to the target so the optimistic render is correct.
    const source = lastSnapshot.groups.find(g => g.name === sourceGroup);
    const target = lastSnapshot.groups.find(g => g.name === targetGroup);
    if (!source || !target) {
      toast(`move: group not found in snapshot`, true);
      return;
    }
    const idx = (source.members || []).findIndex(m => m.conv_id === conv);
    if (idx < 0) {
      toast(`move: member not found in source group`, true);
      return;
    }
    const memberSnapshot = source.members[idx];
    // Optimistic mutation: pull from source, push onto target.
    source.members.splice(idx, 1);
    target.members = target.members || [];
    target.members.push(memberSnapshot);
    renderGroupsTab();

    const rollback = () => {
      // Re-insert at the original position so the visible ordering
      // doesn't drift mid-failure.
      source.members.splice(idx, 0, memberSnapshot);
      target.members = (target.members || []).filter(m => m.conv_id !== conv);
      renderGroupsTab();
    };

    try {
      const addRes = await fetch(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
        method: 'POST', credentials: 'same-origin',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({conv}),
      });
      if (!addRes.ok) {
        toast(`move add failed: ${await addRes.text()}`, true);
        rollback();
        return;
      }
      const delRes = await fetch(`/api/groups/${encodeURIComponent(sourceGroup)}/members/${encodeURIComponent(conv)}`, {
        method: 'DELETE', credentials: 'same-origin',
      });
      if (!delRes.ok) {
        // Add succeeded but remove failed: the conv is now in BOTH
        // groups. Report it so the human can manually clean up; do
        // NOT roll the optimistic mutation back, because the daemon
        // really did add it to the target.
        toast(`move partial: added to ${targetGroup} but failed to remove from ${sourceGroup}: ${await delRes.text()}`, true);
        return;
      }
      toast(`moved ${label}: ${sourceGroup} → ${targetGroup}`);
    } catch (err) {
      toast(`move failed: ${err && err.message || err}`, true);
      rollback();
    }
  } finally {
    await refresh();
  }
}

// runDndAddToGroup handles a drag FROM the virtual Ungrouped group
// ONTO a real group's header — the agent joins that group. Pure add:
// POST /api/groups/{B}/members. The agent was in no group, so there
// is nothing to remove; on success it drops out of the Ungrouped
// virtual group on the next snapshot.
//
// Non-optimistic (one round-trip, then refresh): the source isn't a
// real group in lastSnapshot.groups, so the optimistic splice
// runDndMove relies on doesn't apply. A single fast call + refresh
// keeps the code simple and the failure mode obvious.
async function runDndAddToGroup(payload, targetGroup) {
  const {conv, label} = payload;
  // The source is either an ungrouped agent or a plain conversation;
  // for a conversation the membership write also promotes it to an
  // agent, so the modal says so.
  const isConv = !!payload.sourceConversation;
  const confirmed = await confirmModal({
    title: isConv ? 'Promote conversation into group?' : 'Add agent to group?',
    body: isConv
      ? `Promote the conversation "${label}" to an agent and add it to group `
        + `"${targetGroup}".`
      : `Add the agent "${label}" to group "${targetGroup}". It keeps every `
        + `other group it already belongs to.`,
    meta: label,
    okLabel: isConv ? 'Promote' : 'Add',
  });
  if (!confirmed) { await refresh(); return; }
  try {
    const r = await fetch(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
      method: 'POST', credentials: 'same-origin',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({conv}),
    });
    if (!r.ok) {
      toast(`add to ${targetGroup} failed: ${await r.text()}`, true);
      return;
    }
    toast(`added ${label} → ${targetGroup}`);
  } catch (err) {
    toast(`add failed: ${err && err.message || err}`, true);
  } finally {
    // The dragend handler's refresh() can race ahead of this
    // round-trip; refresh again once it has landed so the final
    // render reflects the mutation.
    await refresh();
  }
}

// runDndRemoveFromGroup handles a drag FROM a real group's member
// row ONTO the virtual Ungrouped group — the agent leaves that
// group. Pure remove: DELETE /api/groups/{A}/members/{conv}. If A
// was the agent's only group it reappears in the Ungrouped virtual
// group on the next snapshot; if it was in other groups too it
// simply stays in those. Non-optimistic, same rationale as
// runDndAddToGroup.
async function runDndRemoveFromGroup(payload) {
  const {conv, sourceGroup, label} = payload;
  if (!sourceGroup) return; // not a real-group member — nothing to do
  const confirmed = await confirmModal({
    title: 'Remove agent from group?',
    body: `Remove "${label}" from group "${sourceGroup}". If this is its only `
      + `group it becomes an ungrouped agent; otherwise it stays in its other `
      + `groups.`,
    meta: label,
    okLabel: 'Remove',
  });
  if (!confirmed) { await refresh(); return; }
  try {
    const r = await fetch(`/api/groups/${encodeURIComponent(sourceGroup)}/members/${encodeURIComponent(conv)}`, {
      method: 'DELETE', credentials: 'same-origin',
    });
    if (!r.ok) {
      toast(`remove from ${sourceGroup} failed: ${await r.text()}`, true);
      return;
    }
    toast(`removed ${label} from ${sourceGroup}`);
  } catch (err) {
    toast(`remove failed: ${err && err.message || err}`, true);
  } finally {
    await refresh();
  }
}

// runDndRetire handles a drag of an AGENT row (a real-group member or
// a virtual-Ungrouped row) ONTO the virtual Retired group — the agent
// is retired, demoting it back to a plain conversation. Retire
// revokes group memberships + grants, so it gets the same
// retireConfirm modal — checkbox and all — as the per-row retire
// button.
async function runDndRetire(payload) {
  const {conv, label} = payload;
  const choice = await retireConfirm({ label, conv });
  if (!choice) {
    await refresh(); // undo the optimistic dragend state
    return;
  }
  try {
    const q = `?shutdown=${choice.shutdown ? 1 : 0}`
      + (choice.deleteWorktree ? '&delete_worktree=1' : '');
    const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/retire${q}`, {
      method: 'POST', credentials: 'same-origin',
    });
    if (!r.ok) {
      // A dangling entry (conversation gone) can't be retired — offer to
      // remove it instead. On return the finally below re-syncs (which
      // also undoes the optimistic drag state on the cancel branch).
      if (await maybeHandleDanglingRetire(r, conv, label)) return;
      toast(`retire ${label} failed: ${await r.text()}`, true);
      return;
    }
    let retireResp = null;
    try { retireResp = await r.json(); } catch (_) {}
    toast(retireToast(label, choice, retireResp));
  } catch (err) {
    toast(`retire failed: ${err && err.message || err}`, true);
  } finally {
    await refresh();
  }
}

// runDndPromoteToUngrouped handles a drag of a CONVERSATION row ONTO
// the virtual Ungrouped group — the conversation is promoted to an
// agent but joins no group, so it lands directly in the Ungrouped
// virtual group on the next snapshot.
async function runDndPromoteToUngrouped(payload) {
  const {conv, label} = payload;
  const confirmed = await confirmModal({
    title: 'Promote conversation to an agent?',
    body: `Promote the conversation "${label}" to an agent. It joins no group `
      + `and appears under Ungrouped.`,
    meta: label,
    okLabel: 'Promote',
  });
  if (!confirmed) { await refresh(); return; }
  try {
    const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/promote`, {
      method: 'POST', credentials: 'same-origin',
    });
    if (!r.ok) {
      toast(`promote ${label} failed: ${await r.text()}`, true);
      return;
    }
    toast(`promoted ${label} → agent (no group)`);
  } catch (err) {
    toast(`promote failed: ${err && err.message || err}`, true);
  } finally {
    await refresh();
  }
}

// runDndReinstate handles a drag of a RETIRED agent row OUT of the
// virtual Retired group. The agent is reinstated — its retired flag
// is cleared, making it an active agent again. Retire stripped the
// agent's old group memberships and grants and reinstate does not
// restore them, so the agent starts fresh: when targetGroup is given
// (dropped onto a real group) it is then added to that group; when
// null (dropped onto Ungrouped) it joins no group and lands in the
// Ungrouped virtual group on the next snapshot.
async function runDndReinstate(payload, targetGroup) {
  const {conv, label} = payload;
  const confirmed = await confirmModal({
    title: 'Reinstate retired agent?',
    body: targetGroup
      ? `Reinstate the retired agent "${label}" and add it to group `
        + `"${targetGroup}". Group memberships and permission grants stripped `
        + `when it was retired are NOT restored — it starts fresh.`
      : `Reinstate the retired agent "${label}" as an active, ungrouped agent. `
        + `Group memberships and permission grants stripped when it was retired `
        + `are NOT restored — it starts fresh.`,
    meta: label,
    okLabel: 'Reinstate',
  });
  if (!confirmed) { await refresh(); return; }
  try {
    const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/reinstate`, {
      method: 'POST', credentials: 'same-origin',
    });
    if (!r.ok) {
      toast(`reinstate ${label} failed: ${await r.text()}`, true);
      return;
    }
    if (targetGroup) {
      const addRes = await fetch(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
        method: 'POST', credentials: 'same-origin',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({conv}),
      });
      if (!addRes.ok) {
        toast(`reinstated ${label}, but join ${targetGroup} failed: ${await addRes.text()}`, true);
        return;
      }
      toast(`reinstated ${label} → ${targetGroup}`);
    } else {
      toast(`reinstated ${label} → agent (no group)`);
    }
  } catch (err) {
    toast(`reinstate failed: ${err && err.message || err}`, true);
  } finally {
    await refresh();
  }
}

export { bindDnd, dndDragActive };
