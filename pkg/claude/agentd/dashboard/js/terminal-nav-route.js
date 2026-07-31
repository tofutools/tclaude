// terminal-nav-route.js — the Terminals tab's half of path routing
// (/terminals/<agent-id>).
//
// The router (js/nav-history.js) owns the URL and the history stack; this
// module owns the tab's two obligations toward it:
//
//   - ANNOUNCE: when the operator switches to a different agent's terminal,
//     tell the router so the address bar follows (`tclaude:navigated`).
//   - RESTORE: when the router asks for a location — a hard refresh on
//     /terminals/<agent-id>, or a Back/Forward across two terminals — reattach
//     that agent's terminal (`tclaude:restore-location`).
//
// Split out of the island so both halves are testable without Preact or a
// rendered DOM: everything here is signals plus two document events.
//
// Only the agent selector travels in the URL. The pane label is NOT encoded —
// it is looked back up from the snapshot roster on restore, so a renamed agent
// deep-links to its current name instead of a stale one, and the URL stays a
// short, typeable, shareable thing.

const RESTORE_EVENT = 'tclaude:restore-location';
const NAVIGATED_EVENT = 'tclaude:navigated';

// agentLabel resolves an agent selector back to the title the tab strip should
// show. The roster is keyed both ways because the selector is `agent_id ||
// conv-id` (see the row-action comment in row-action-handler.js): a
// pre-identity agent is addressed by its conv-id. Falls back to the selector
// itself, which is what the launchers do for a row with no label.
export function agentLabel(snapshotValue, agent) {
  const selector = String(agent || '');
  if (!selector) return '';
  const roster = Array.isArray(snapshotValue?.agents) ? snapshotValue.agents : [];
  const match = roster.find((entry) => entry?.agent_id === selector || entry?.conv_id === selector);
  return (match?.title || '').trim() || selector;
}

// bindTerminalNavRouting wires one mounted (non-solo) terminal shell to the
// router. Returns an unbind function; the island registers it as cleanup.
//
// `openAgentPane(agent, label)` is injected rather than imported so tests can
// drive restore without the xterm runtime — in production it is the same
// openWebWindowPane the "focus this agent" row action uses, which is what makes
// a deep link land on exactly the pane a click would have produced.
export function bindTerminalNavRouting({
  state,
  openAgentPane,
  leaveTab = () => {},
  snapshot = null,
  documentRef = globalThis.document,
} = {}) {
  if (!state || typeof openAgentPane !== 'function') {
    throw new TypeError('terminal nav routing requires state and openAgentPane');
  }
  if (!documentRef) return () => {};

  // While we are applying a location the ROUTER asked for, the resulting
  // activeKey change must not be announced back as a fresh user navigation:
  // that would push a history entry for a move the browser had already made.
  // The router's own `applying` guard cannot cover us — it is cleared
  // synchronously when activate() returns, long before an async reattach
  // lands — so the intent is tracked here instead, exactly as Processes does.
  let restoring = 0;

  async function applyLocation(loc) {
    const agent = loc?.selection ? String(loc.selection) : '';
    // A bare /terminals is honoured by any open pane; there is no single
    // terminal it names, so whatever is active stays active.
    if (!agent) return state.panes.value.length > 0;
    const existing = state.findPaneKey([agent]);
    if (existing) return state.activatePane(existing);
    // Nothing open for this agent — the ordinary hard-refresh case, where the
    // whole pane set is gone. Reattach just this one.
    const opened = await openAgentPane(agent, agentLabel(snapshot?.value, agent));
    return !!opened;
  }

  const onRestore = (event) => {
    const loc = event?.detail?.location;
    if (loc?.tab !== 'terminals') return;
    restoring += 1;
    void Promise.resolve(applyLocation(loc))
      .catch((error) => {
        console.error('terminal deep link restore failed:', error);
        return false;
      })
      .then((ok) => {
        restoring -= 1;
        if (ok) return;
        // We could not land where the URL pointed — a retired agent, or a
        // terminal the daemon refused.
        //
        // With other panes still open the tab remains a real place to be, so
        // answer as a CORRECTION: the router rewrites the current entry to the
        // pane we DID end up on. A push here would both truncate the forward
        // tail and grow history on every retry.
        if (state.panes.value.length > 0) { announce(state.location.value, true); return; }
        // With nothing open at all the tab is hidden and there is nothing to
        // correct TO — this is the cold deep link to a terminal that no longer
        // exists. Leave the tab the same way the island does when its last pane
        // closes; that click is an ordinary navigation the router records on
        // its own, so it must NOT also be announced here.
        leaveTab();
      });
  };

  function announce(location, correction = false) {
    documentRef.dispatchEvent(new CustomEvent(NAVIGATED_EVENT, {
      detail: { location, correction },
    }));
  }

  // Subscribe rather than diff in a Preact effect: the tab strip is not the
  // only thing that moves activeKey (closing a pane elects an heir, a reveal
  // activates the opened pane), and every one of those is a real location
  // change the URL should track. `subscribe` fires once immediately with the
  // current value — skip that so mounting never announces.
  let seen = null;
  let primed = false;
  const unsubscribe = state.location.subscribe((location) => {
    const key = location?.selection || '';
    if (!primed) { primed = true; seen = key; return; }
    if (key === seen) return;
    seen = key;
    // Losing the last agent pane leaves the bare tab, which the router's own
    // snapshot reconcile already corrects (and the tab may be auto-leaving
    // entirely). Announcing it as a navigation would forge a history entry for
    // a move the operator never made.
    if (!key) return;
    if (restoring > 0) return;
    announce(location);
  });

  const unbind = () => {
    unsubscribe();
    documentRef.removeEventListener(RESTORE_EVENT, onRestore);
  };
  documentRef.addEventListener(RESTORE_EVENT, onRestore);
  return unbind;
}
