// collab-rail-bin.js -- what a Collab panel row does when it is dragged onto
// the Groups tab's bin (#dnd-trash, dashboard.html): the bin shows while the
// row is dragged, labelled for the act, and a drop there runs the same
// confirm and request the Groups tab or the row's own menu would. Nothing
// here deletes on its own:
//   - an agent retires, through the Groups tab's drop operations (dnd-ops.js
//     ops.retire: the retire dialog), gated for another node's agent as the
//     fused Groups view gates it (ops.allowed);
//   - a drawing board or page is deleted as its Groups row's 🗑 does
//     (groups-resources.js resourceRemover);
//   - a shell or a background or Other session is killed as the row's ⋯ →
//     Kill session… does (collab-rail-menu.js), or, for a running shell whose
//     menu has none, as its Groups row's 🗑 does (groups-resources.js
//     shellKill).
// collab-rail-actions.js startRailDrag shows, arms and hides the bin.

import { isAgentItem } from './collab-rail-model.js';
import { railResource } from './collab-rail-actions.js';
import { railRowActions } from './collab-rail-menu.js';
import { resourceRemover, shellKill } from './groups-resources.js';
import { processContext, processRunApi } from './process-context.js';
import { onPeerPage } from './node-api.js';

// RAIL_BIN_MODES are the bin's label classes for each act (dashboard.css):
// Retire is its own default voice, Delete the one a Groups resource row's
// drag shows, Kill the panel's own.
export const RAIL_BIN_MODES = Object.freeze({ retire: '', delete: 'dnd-trash-template-mode', kill: 'dnd-trash-kill-mode' });

// binAgent is the agent behind an agent row as the Groups tab's member drag
// carries it ({ conv, agent, label }), with the node it runs on ({ node,
// name }, '' for the page's). null for a row with no agent to name.
function binAgent(item) {
  if (!isAgentItem(item)) return null;
  const agent = item.status.agent;
  const remote = item.pane?.seed?.remote;
  const node = remote?.peer || item.row?.node?.id || '';
  const name = remote ? remote.peerLabel || '' : item.row?.node?.name || '';
  const id = agent.agent_id || (node ? remote?.agent || item.row?.agent_id || '' : '');
  const conv = agent.conv_id || id;
  // Another node's route takes the stable ID only.
  if (!conv || (node && !id)) return null;
  return { payload: { conv, agent: id || conv, label: agent.title || item.title }, where: { node, name } };
}

// deletable is a drawing board or page the panel may delete: the ones its ⚙
// offers Delete… for (collab-rail-gear.js), never a peer's.
function deletable(item) {
  if (item.kind !== 'drawing_board' && item.kind !== 'page') return null;
  if (onPeerPage() || item.resource?.peer) return null;
  if (item.kind === 'page' && !item.resource) return null;
  const resource = railResource(item);
  return resource ? { kind: resource.kind, id: resource.id, label: resource.label, closed: resource.closed } : null;
}

// railBinAction is what a drop of item on the bin does: { act, mode,
// run() }, or null when the bin means nothing for it (the bin does not show).
//   ctx: the row menu's context (collab-rail-menu.js defaultRowContext:
//   confirm, notify, kill), plus groupOps, api, source and killShell, which
//   tests swap.
export function railBinAction(item, ctx = {}) {
  if (!item) return null;
  const agent = binAgent(item);
  if (agent) {
    return { act: 'retire', mode: RAIL_BIN_MODES.retire, run: async () => {
      // The Groups tab's own drop: its gate, then its retire dialog.
      const ops = ctx.groupOps || (await import('./dnd.js')).dndOps;
      const where = ops.context(agent.where);
      if (!ops.allowed(where, 'retire', agent.payload.agent)) {
        ops.refused(where, 'retire');
        return;
      }
      await ops.retire(agent.payload, where);
    } };
  }
  const resource = deletable(item);
  if (resource) {
    const remove = resourceRemover(resource, {
      api: ctx.api || processRunApi, source: ctx.source || processContext, notify: ctx.notify || (() => {}), confirm: ctx.confirm,
    });
    return { act: 'delete', mode: RAIL_BIN_MODES.delete, run: remove };
  }
  const kill = railRowActions(item).end.find((entry) => entry.id === 'kill');
  if (kill) return { act: 'kill', mode: RAIL_BIN_MODES.kill, run: () => kill.run(ctx) };
  const shell = item.kind === 'shell' ? item.resource : null;
  if (shell?.session && shell.state !== 'closed') {
    return { act: 'kill', mode: RAIL_BIN_MODES.kill,
      run: shellKill(item.title, shell.session, { confirm: ctx.confirm, notify: ctx.notify || (() => {}), ...(ctx.killShell ? { kill: ctx.killShell } : {}) }) };
  }
  return null;
}
