// dnd-ops.js — what a member drop does (dnd.js wires the gesture): move,
// add, remove, clone, retire, promote or reinstate an agent, on the node the
// drop lands on. The page's node is the default; the fused Groups view's
// boxes name other nodes (data-fleet-node), whose drops go through nodeFetch
// to that node's own routes, as its own dashboard sends them.
import { announceNodeChanged, isPageNode, nodeFetch, nodeSnapshot, selfNodeID } from './node-api.js';
import { agentGrantHeld } from './peer-view-limits.js';
import { UNGROUPED_VKEY } from './virtual-groups.js';

// DRAG_GRANT_ACTS are the drop gestures a restricted peer's grants can cover,
// as the row control (AGENT_GRANT_ACTS) each stands for.
export const DRAG_GRANT_ACTS = Object.freeze({ clone: 'clone', retire: 'retire-agent' });

// targetGroupOf is the real group a box stands for, as it is listed.
export function targetGroupOf(box) {
  return box.getAttribute('data-dnd-target-group')
    || (box.matches('details[data-fleet-node]') ? box.getAttribute('data-group-key') || '' : '');
}

// dndNode reads the node a row or box is on: node '' for the page's own (and
// outside the fused view), else the node's ID, with the name it shows.
export function dndNode(el) {
  const box = el?.closest?.('details[data-fleet-node]');
  const id = box?.getAttribute('data-fleet-node') || '';
  const name = box?.getAttribute('data-fleet-node-name') || '';
  return { node: id && !isPageNode(id) ? id : '', name };
}

// dndGesture lets the fused view's own drop handling (a move between nodes)
// cancel the member drag's pending Ctrl/Cmd clone hover, which dnd.js would
// otherwise finish at dragend: dnd.js registers cancelClone.
export const dndGesture = { cancelClone: () => {} };

// markDropBox paints the one drop-target highlight every Groups view uses:
// the whole group box (header and body) under a member drag, tinted for a
// move, a clone, or a drop that is refused there. clearDropBox removes it.
// dnd.js (a drop on the agent's own node) and the fused view's cross-node
// move (fleet-merged-island.js) both paint through these.
export function markDropBox(box, effect = 'move') {
  if (!box) return;
  box.classList.add('dnd-drop-over');
  box.classList.toggle('dnd-effect-clone', effect === 'clone');
  box.classList.toggle('dnd-drop-refused', effect === 'refused');
}

export function clearDropBox(box) {
  box?.classList.remove('dnd-drop-over', 'dnd-effect-clone', 'dnd-drop-refused');
}

// nodeAllowsActing: the page's node and this node always; another node only
// under its unrestricted trust, which lets this operator act there as on its
// own dashboard. (A peer's own page gates its drags itself.)
export function nodeAllowsActing(node) {
  return !node || node === selfNodeID() || nodeSnapshot(node)?.peer_view?.level === 'unrestricted';
}

// groupDragContext reads which node a group header is on and how the drag acts
// there. In the fused Groups view the tree lists every node's groups as
// group@node (its merged snapshot, fusedGroupListing), so a drag plans in
// those names and its requests go to the node (nodeFetch) under the node's
// own names (own). Its sibling order is the fused view's own.
export function groupDragContext(el, { refresh = () => {}, fetchImpl = (...args) => globalThis.fetch(...args) } = {}) {
  const { node, name } = dndNode(el);
  const fused = !!el?.closest?.('details[data-fleet-node]');
  const at = fused && name ? `@${name}` : '';
  return {
    node, name, fused,
    own: (g) => (at && typeof g === 'string' && g.endsWith(at) ? g.slice(0, -at.length) : g),
    api: nodeFetch(node, { fetchImpl }),
    done: () => { announceNodeChanged(node); refresh(); },
  };
}

// sameGroupTree: a drop target in the dragged group's own tree (the same node,
// and the same view).
export function sameGroupTree(ctx, details) {
  if (!ctx) return false;
  const there = groupDragContext(details);
  return there.fused === ctx.fused && there.node === ctx.node;
}

// createDndOps binds the operations to the page: confirm, toast and refresh
// (refresh.js), render (the Groups tab), the page snapshot, the clone and
// retire dialogs, and land, which shows where a done drop ended up
// (drop-landing.js markLanded).
export function createDndOps({
  confirm, toast, refresh, render = () => {}, getSnapshot = () => null,
  openClone, openRetire, retireNeedsReconcile = () => true, fetchImpl = (...args) => globalThis.fetch(...args),
  land = () => {},
}) {
  // context is how a drop acts on its node: that node's routes (nodeFetch),
  // its own group names (the fused view lists a peer's as group@node), its
  // snapshot, and the refresh that rereads it. The page's node is the default.
  function context({ node = '', name = '' } = {}) {
    const at = name ? `@${name}` : '';
    return {
      node, name,
      api: nodeFetch(node, { fetchImpl }),
      group: (g) => (at && typeof g === 'string' && g.endsWith(at) ? g.slice(0, -at.length) : g),
      snapshot: () => (node ? nodeSnapshot(node) : getSnapshot()),
      done: () => { announceNodeChanged(node); return refresh(); },
    };
  }

  // allowed says whether a drop may act on its node: one that lets this one
  // act there (this node, or a peer trusting it unrestricted) takes every
  // gesture; a restricted peer only the clone and the retire (the bin) its
  // grants on the agent cover, as its ⚙ items do; a clone joins the group it
  // is dropped on only with the clone grant there too. A peer's own page is
  // the page's node (ctx.node blank) with the page snapshot's trust.
  function allowed(ctx, gesture = '', agent = '', group = '') {
    const pv = ctx.snapshot()?.peer_view || null;
    const restricted = ctx.node ? !nodeAllowsActing(ctx.node) : (!!globalThis.__tclaudeRemoteNode?.id && pv?.level !== 'unrestricted');
    if (!restricted) return true;
    const act = DRAG_GRANT_ACTS[gesture];
    if (!act || !agentGrantHeld(act, agent, pv)) return false;
    const held = group && gesture === 'clone' ? pv?.groups?.[ctx.group(group)] : null;
    return !group || gesture !== 'clone' || (Array.isArray(held) && held.includes('groups.members.clone'));
  }

  // refusedOn tells the operator a drop on another node is not theirs to make.
  function refusedOn(ctx, gesture = '') {
    const act = DRAG_GRANT_ACTS[gesture];
    toast(act
      ? `${ctx.name || 'That node'} does not share this agent's ${gesture} with you (needs ${act === 'clone' ? 'groups.members.clone on the agent and the group dropped on' : 'groups.members.retire'})`
      : `${ctx.name || 'That node'} trusts this node restricted: move its agents from its own dashboard`, true);
    void refresh();
  }

  // landed shows the dropped agent where it went: a real group (its own name
  // on ctx's node) or, with none, the virtual Ungrouped group.
  function landed(ctx, payload, group = '') {
    const ids = [payload.agent, payload.conv];
    land(group ? { group, node: ctx.node, section: 'agents', ids } : { virtualKey: UNGROUPED_VKEY, ids });
  }

  // memberCwd finds the dragged agent's directory in its node's snapshot (the
  // clone dialog offers that repo's worktrees).
  function memberCwd(ctx, conv) {
    for (const group of ctx.snapshot()?.groups || []) {
      const member = (group.members || []).find((m) => m.conv_id === conv);
      if (member?.cwd) return member.cwd;
    }
    return '';
  }

  // runDndClone opens the regular clone dialog (action-dialog-island.js) for
  // the dragged agent, its clone joining the group dropped on: the clone
  // inherits the source's memberships and joins that group in the same
  // request (POST /api/agents/{agent}/clone with `group`).
  async function runDndClone(payload, targetGroup, ctx = context()) {
    const {conv, label} = payload;
    const sel = payload.agent || conv;
    try {
      openClone(sel, label, memberCwd(ctx, conv), ctx.node, ctx.group(targetGroup));
    } catch (err) {
      toast(`clone failed: ${err && err.message || err}`, true);
    }
    // The drag's own highlight and the dropped row settle now; the dialog
    // refreshes once it clones.
    await refresh();
  }

  // runDndMove performs the optimistic local mutation, then the
  // POST B → DELETE A sequence. Failure of either step rolls back
  // the local mutation and surfaces a toast.
  async function runDndMove(payload, listedTarget, ctx = context()) {
    const {conv, label} = payload;
    const sourceGroup = ctx.group(payload.sourceGroup);
    const targetGroup = ctx.group(listedTarget);
    // sel routes the membership writes (server resolves agent_id or conv-id);
    // the optimistic lastSnapshot splice below still matches on conv_id, so
    // the local member rows correlate regardless of which selector we send.
    const sel = payload.agent || conv;
    // Confirm BEFORE the lastSnapshot read + optimistic splice below,
    // so a cancelled move leaves the snapshot — and the render —
    // completely untouched.
    // On this node the move is one daemon call that can keep the agent's
    // sandbox profile or environment on the group it leaves. Another node's
    // move stays the add-then-remove pair its routes allow.
    const checks = ctx.node ? [] : [
      { key: 'sandbox', checked: true, label: `Apply "${targetGroup}"'s sandbox profile`,
        hint: `Unticked keeps "${sourceGroup}"'s. Takes effect at the next resume or restart.` },
      { key: 'environment', checked: true, label: `Apply "${targetGroup}"'s environment variables`,
        hint: `Unticked keeps "${sourceGroup}"'s. Takes effect at the next resume or restart.` },
    ];
    const confirmed = await confirm({
      title: 'Move agent to another group?',
      body: `Move "${label}" out of group "${sourceGroup}" and into group `
        + `"${targetGroup}". Its membership of "${sourceGroup}" is removed and a `
        + `membership of "${targetGroup}" is added, with the same role. Group permissions `
        + `change immediately; per-agent overrides are kept.`,
      meta: label,
      okLabel: 'Move',
      checks,
    });
    if (!confirmed) { await ctx.done(); return; }
    // Every post-confirm exit — a guard-clause return, the partial-
    // failure return, success, or an error — funnels through the
    // finally so the dashboard re-syncs immediately. Without this a
    // confirmed-then-aborted move could leave optimistic state visible until the
    // next 2s tick.
    try {
      // Another node's rows are the fused view's copy of it, reread once the
      // node answers: no optimistic splice there.
      let rollback = () => {};
      if (!ctx.node) {
        const lastSnapshot = getSnapshot();
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
        render();

        rollback = () => {
          // Re-insert at the original position so the visible ordering
          // doesn't drift mid-failure.
          source.members.splice(idx, 0, memberSnapshot);
          target.members = (target.members || []).filter(m => m.conv_id !== conv);
          render();
        };
      }

      try {
        if (!ctx.node) {
          const keep = (key) => !checks.find((check) => check.key === key).checked;
          const moveRes = await ctx.api(`/api/groups/${encodeURIComponent(sourceGroup)}/members/${encodeURIComponent(sel)}/move`, {
            method: 'POST', credentials: 'same-origin',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({to: targetGroup, keep_sandbox: keep('sandbox'), keep_environment: keep('environment')}),
          });
          if (!moveRes.ok) {
            toast(`move failed: ${await moveRes.text()}`, true);
            rollback();
            return;
          }
          toast(`moved ${label}: ${sourceGroup} → ${targetGroup}`);
          landed(ctx, payload, targetGroup);
          return;
        }
        const addRes = await ctx.api(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
          method: 'POST', credentials: 'same-origin',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({conv: sel}),
        });
        if (!addRes.ok) {
          toast(`move add failed: ${await addRes.text()}`, true);
          rollback();
          return;
        }
        const delRes = await ctx.api(`/api/groups/${encodeURIComponent(sourceGroup)}/members/${encodeURIComponent(sel)}`, {
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
        landed(ctx, payload, targetGroup);
      } catch (err) {
        toast(`move failed: ${err && err.message || err}`, true);
        rollback();
      }
    } finally {
      await ctx.done();
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
  async function runDndAddToGroup(payload, listedTarget, ctx = context()) {
    const targetGroup = ctx.group(listedTarget);
    const {conv, label} = payload;
    const sel = payload.agent || conv;
    // The source is either an ungrouped agent or a plain conversation;
    // for a conversation the membership write also promotes it to an
    // agent, so the modal says so.
    const isConv = !!payload.sourceConversation;
    const confirmed = await confirm({
      title: isConv ? 'Promote conversation into group?' : 'Add agent to group?',
      body: isConv
        ? `Promote the conversation "${label}" to an agent and add it to group `
          + `"${targetGroup}".`
        : `Add the agent "${label}" to group "${targetGroup}". It keeps every `
          + `other group it already belongs to.`,
      meta: label,
      okLabel: isConv ? 'Promote' : 'Add',
    });
    if (!confirmed) { await ctx.done(); return; }
    try {
      const r = await ctx.api(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
        method: 'POST', credentials: 'same-origin',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({conv: sel}),
      });
      if (!r.ok) {
        toast(`add to ${targetGroup} failed: ${await r.text()}`, true);
        return;
      }
      toast(`added ${label} → ${targetGroup}`);
      landed(ctx, payload, targetGroup);
    } catch (err) {
      toast(`add failed: ${err && err.message || err}`, true);
    } finally {
      // The dragend handler's refresh() can race ahead of this
      // round-trip; refresh again once it has landed so the final
      // render reflects the mutation.
      await ctx.done();
    }
  }

  // runDndRemoveFromGroup handles a drag FROM a real group's member
  // row ONTO the virtual Ungrouped group — the agent leaves that
  // group. Pure remove: DELETE /api/groups/{A}/members/{conv}. If A
  // was the agent's only group it reappears in the Ungrouped virtual
  // group on the next snapshot; if it was in other groups too it
  // simply stays in those. Non-optimistic, same rationale as
  // runDndAddToGroup.
  async function runDndRemoveFromGroup(payload, ctx = context()) {
    const {conv, label} = payload;
    const sourceGroup = ctx.group(payload.sourceGroup);
    const sel = payload.agent || conv;
    if (!sourceGroup) return; // not a real-group member — nothing to do
    const confirmed = await confirm({
      title: 'Remove agent from group?',
      body: `Remove "${label}" from group "${sourceGroup}". If this is its only `
        + `group it becomes an ungrouped agent; otherwise it stays in its other `
        + `groups.`,
      meta: label,
      okLabel: 'Remove',
    });
    if (!confirmed) { await ctx.done(); return; }
    try {
      const r = await ctx.api(`/api/groups/${encodeURIComponent(sourceGroup)}/members/${encodeURIComponent(sel)}`, {
        method: 'DELETE', credentials: 'same-origin',
      });
      if (!r.ok) {
        toast(`remove from ${sourceGroup} failed: ${await r.text()}`, true);
        return;
      }
      toast(`removed ${label} from ${sourceGroup}`);
      // It shows under Ungrouped only once it is in no group at all.
      const elsewhere = (ctx.snapshot()?.groups || []).some((g) => g.name !== sourceGroup
        && (g.members || []).some((m) => m.conv_id === conv));
      if (!elsewhere) landed(ctx, payload);
    } catch (err) {
      toast(`remove failed: ${err && err.message || err}`, true);
    } finally {
      await ctx.done();
    }
  }

  // runDndRetire handles a drag of an AGENT row (a real-group member or
  // a virtual-Ungrouped row) ONTO the virtual Retired group — the agent
  // is retired, demoting it back to a plain conversation. Retire
  // revokes group memberships + grants, so it gets the same
  // keyed transaction dialog — checkbox and all — as the per-row retire button.
  async function runDndRetire(payload, ctx = context()) {
    const {conv, label} = payload;
    // This node's retire stays conv-keyed (unlike the other runDnd* endpoint
    // calls): the server's dangling-agent recovery only triggers for a
    // UUID-shaped selector that fails to resolve, so sending the stable
    // agent_id would silently demote a dangling orphan instead of offering to
    // remove it (JOH-322). See row-actions.js's retire-agent case.
    let result = null;
    try {
      // Another node's route (or a peer's own page's) takes the stable
      // agent_id only.
      if ((ctx.node || globalThis.__tclaudeRemoteNode?.id) && payload.agent) result = await openRetire(payload.agent, label, ctx.node);
      else result = await openRetire(conv, label, ctx.node);
    } catch (err) {
      toast(`retire failed: ${err && err.message || err}`, true);
    }
    // Successful normal retirement and successful dangling deletion already
    // refresh inside the transaction action before resolving. Every other final
    // result is presentation-only (cancel/decline/failure), so DnD must repaint
    // once to undo its drag-local optimistic state.
    if (retireNeedsReconcile(result)) await ctx.done();
  }

  // runDndDeletePending handles a drag of a PENDING spawn (JOH-205) onto the
  // trash / Retired target — the escape hatch for a spawn wedged behind a
  // startup gate it will never clear. A pending spawn is keyed by its LABEL
  // (no conv-id yet) and is not an agent, so this hits the dedicated
  // /api/pending/delete/{label} endpoint (kill pane + drop pending + session
  // rows) rather than the conv-keyed retire path. Same op and confirmation
  // wording as the per-row 🗑 delete button (row-actions.js).
  async function runDndDeletePending(payload, ctx = context()) {
    const {label} = payload;
    const confirmed = await confirm({
      title: 'Delete pending spawn?',
      body: 'This spawn never finished starting up — its pane is stuck behind a startup gate (untrusted dir, config prompt, or an OpenAI-auth modal). Deleting kills its pane and removes it from the pending list. It never became a real agent, so there is no conversation to keep.',
      meta: label,
      okLabel: 'Delete',
    });
    if (!confirmed) { await ctx.done(); return; }
    try {
      const r = await ctx.api(`/api/pending/delete/${encodeURIComponent(label)}`, {
        method: 'POST', credentials: 'same-origin',
      });
      if (!r.ok) {
        toast(`delete ${label} failed: ${await r.text()}`, true);
        return;
      }
      toast(`deleted pending spawn: ${label}`);
    } catch (err) {
      toast(`delete failed: ${err && err.message || err}`, true);
    } finally {
      await ctx.done();
    }
  }

  // runDndPromoteToUngrouped handles a drag of a CONVERSATION row ONTO
  // the virtual Ungrouped group — the conversation is promoted to an
  // agent but joins no group, so it lands directly in the Ungrouped
  // virtual group on the next snapshot.
  async function runDndPromoteToUngrouped(payload, ctx = context()) {
    const {conv, label} = payload;
    const sel = payload.agent || conv;
    const confirmed = await confirm({
      title: 'Promote conversation to an agent?',
      body: `Promote the conversation "${label}" to an agent. It joins no group `
        + `and appears under Ungrouped.`,
      meta: label,
      okLabel: 'Promote',
    });
    if (!confirmed) { await ctx.done(); return; }
    try {
      const r = await ctx.api(`/api/agents/${encodeURIComponent(sel)}/promote`, {
        method: 'POST', credentials: 'same-origin',
      });
      if (!r.ok) {
        toast(`promote ${label} failed: ${await r.text()}`, true);
        return;
      }
      toast(`promoted ${label} → agent (no group)`);
      landed(ctx, payload);
    } catch (err) {
      toast(`promote failed: ${err && err.message || err}`, true);
    } finally {
      await ctx.done();
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
  async function runDndReinstate(payload, listedTarget, ctx = context()) {
    const targetGroup = listedTarget ? ctx.group(listedTarget) : listedTarget;
    const {conv, label} = payload;
    const sel = payload.agent || conv;
    const confirmed = await confirm({
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
    if (!confirmed) { await ctx.done(); return; }
    try {
      const r = await ctx.api(`/api/agents/${encodeURIComponent(sel)}/reinstate`, {
        method: 'POST', credentials: 'same-origin',
      });
      if (!r.ok) {
        toast(`reinstate ${label} failed: ${await r.text()}`, true);
        return;
      }
      if (targetGroup) {
        const addRes = await ctx.api(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
          method: 'POST', credentials: 'same-origin',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({conv: sel}),
        });
        if (!addRes.ok) {
          toast(`reinstated ${label}, but join ${targetGroup} failed: ${await addRes.text()}`, true);
          return;
        }
        toast(`reinstated ${label} → ${targetGroup}`);
        landed(ctx, payload, targetGroup);
      } else {
        toast(`reinstated ${label} → agent (no group)`);
        landed(ctx, payload);
      }
    } catch (err) {
      toast(`reinstate failed: ${err && err.message || err}`, true);
    } finally {
      await ctx.done();
    }
  }

  return Object.freeze({
    context, allowed, refused: refusedOn,
    clone: runDndClone, move: runDndMove, add: runDndAddToGroup, remove: runDndRemoveFromGroup,
    retire: runDndRetire, deletePending: runDndDeletePending, promote: runDndPromoteToUngrouped, reinstate: runDndReinstate,
  });
}
