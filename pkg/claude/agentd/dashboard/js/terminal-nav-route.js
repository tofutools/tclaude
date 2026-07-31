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
//
// The subtle half of this module is deciding WHICH active-pane change is a user
// navigation. `activeKey` moving is not that signal: it also moves for a
// background (Ctrl/Cmd-click) open the operator deliberately did NOT switch to,
// and for an heir elected when a pane closes underneath them. Announcing those
// as navigations would push history entries for moves nobody made — and, worse,
// would rewrite the address bar to a terminal the operator is not looking at,
// so the next hard refresh lands somewhere they never were. See classifyChange.

// FALLBACK_TAB is where a terminals deep link goes when there is nothing left to
// show. It mirrors the Terminals island's own "last pane closed" behaviour and
// the router's DEFAULT_TAB.
const FALLBACK_TAB = 'groups';

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
  const match = rosterEntry(snapshotValue, selector);
  return (match?.title || '').trim() || selector;
}

function rosterEntry(snapshotValue, agent) {
  const roster = Array.isArray(snapshotValue?.agents) ? snapshotValue.agents : [];
  return roster.find((entry) => entry?.agent_id === agent || entry?.conv_id === agent) || null;
}

// knownAgent reports whether a selector names an agent the daemon currently
// knows about. This is what makes a dead deep link fail EARLY: opening a pane
// succeeds for any well-formed seed — the socket only 404s later — so without
// this check "/terminals/<retired-agent>" would silently leave the operator
// staring at a terminal that can never connect.
//
// An empty roster means "no snapshot to judge by" (the poll has not landed, or
// this shell was mounted without one), not "no agents exist". Attempting the
// attach is the right degradation there: the worst case is the pane the
// operator would have got before this check existed.
function knownAgent(snapshotValue, agent) {
  const roster = Array.isArray(snapshotValue?.agents) ? snapshotValue.agents : [];
  if (!roster.length) return true;
  return !!rosterEntry(snapshotValue, agent);
}

// bindTerminalNavRouting wires one mounted (non-solo) terminal shell to the
// router. Returns an unbind function; the island registers it as cleanup.
//
// `openAgentPane(agent, label)` is injected rather than imported so tests can
// drive restore without the xterm runtime — in production it is the same
// openWebWindowPane the "focus this agent" row action uses, which is what makes
// a deep link land on exactly the pane a click would have produced.
//
// `isTabActive()` reports whether the Terminals section is the one on screen.
// A pane change while the operator is looking at another tab is never their
// navigation — that is exactly the background-open case.
export function bindTerminalNavRouting({
  state,
  openAgentPane,
  leaveTab = () => {},
  snapshot = null,
  documentRef = globalThis.document,
  isTabActive = () => !!documentRef?.getElementById('tab-terminals')?.classList?.contains('active'),
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
  // Restores can overlap (two quick Back presses). Only the newest may decide
  // where we end up or answer the router; an older one resolving late must not
  // drag the tab back to a location the browser has already left.
  let restoreGeneration = 0;
  // The terminal the in-flight restore is attaching, so a pane change during
  // that window can be told apart from the restore's own landing.
  let restoreTarget = '';
  // A terminal the operator picked WHILE a restore was in flight. Their click
  // is newer than the restore, so it wins: when the attach lands it must not
  // silently steal the tab back.
  let interrupted = null;
  let disposed = false;

  function announce(location, correction = false) {
    if (disposed) return;
    documentRef.dispatchEvent(new CustomEvent(NAVIGATED_EVENT, {
      detail: { location, correction },
    }));
  }

  async function applyLocation(loc) {
    const agent = loc?.selection ? String(loc.selection) : '';
    // A bare /terminals is honoured by any open pane; there is no single
    // terminal it names, so whatever is active stays active.
    if (!agent) return state.panes.value.length > 0;
    const existing = state.findPaneKey([agent]);
    if (existing) return state.activatePane(existing);
    if (!knownAgent(snapshot?.value, agent)) return false;
    // Nothing open for this agent — the ordinary hard-refresh case, where the
    // whole pane set is gone. Reattach just this one.
    const opened = await openAgentPane(agent, agentLabel(snapshot?.value, agent));
    return !!opened;
  }

  const onRestore = (event) => {
    const loc = event?.detail?.location;
    if (loc?.tab !== 'terminals') return;
    restoring += 1;
    restoreGeneration += 1;
    const generation = restoreGeneration;
    restoreTarget = loc.selection ? String(loc.selection) : '';
    interrupted = null;
    void Promise.resolve(applyLocation(loc))
      .catch((error) => {
        console.error('terminal deep link restore failed:', error);
        return false;
      })
      .then((ok) => {
        restoring -= 1;
        if (disposed || generation !== restoreGeneration) return;
        if (ok) {
          // The operator may have switched terminals while the reattach was in
          // flight (a cold restore waits on the xterm runtime, which is a
          // network fetch). The URL still names where the router sent us, so
          // their move has to be announced now or it would be lost silently.
          reconcileAfterRestore(loc);
          return;
        }
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
        // exists. Correct the URL to the fallback tab FIRST, then leave: the
        // click leaveTab performs is itself an ordinary navigation the router
        // records, and having already replaced this entry makes that record a
        // duplicate it suppresses. Done in the other order the click would push
        // a fresh entry, and Back would bounce off this dead link forever
        // instead of returning where the operator came from.
        announce({ tab: FALLBACK_TAB }, true);
        leaveTab();
      });
  };

  // reconcileAfterRestore compares where we actually are against the location
  // the router asked for, once the restore has settled. They differ only when
  // the operator navigated during the attach — a real move, so it is announced
  // as a navigation, not a correction.
  function reconcileAfterRestore(requested) {
    const claim = interrupted;
    interrupted = null;
    // The operator picked a terminal while the attach was in flight. Their
    // click is the newer intent, so hand the tab back to it rather than letting
    // the restore's landing quietly override a deliberate choice.
    if (claim && state.panes.value.some((pane) => pane.key === claim.key)) {
      state.activatePane(claim.key);
    }
    const location = state.location.value;
    const selection = location?.selection || '';
    seenSelection = selection;
    // A bare /terminals restore names no terminal and moves nothing, so there
    // is never anything to reconcile against it.
    if (!requested?.selection) return;
    if (!selection || selection === String(requested.selection)) return;
    if (!isTabActive()) return;
    announce(location);
  }

  // classifyChange decides what an active-pane change MEANS.
  //
  //   'silent'   — the operator is looking at another tab, so this is not their
  //                navigation at all. A background Ctrl/Cmd-click open lands
  //                here: the Terminals tab collects the pane while the operator
  //                stays on Groups, and the address bar must stay put with it.
  //   'correct'  — the pane they were on is GONE, so something moved them: an
  //                agent retired and the poll closed its pane, a pop-out took
  //                it, a bulk unfocus closed it. The URL should track the heir,
  //                but as a replace — they never asked to go there, and a push
  //                would put a terminal they never chose in their Back history.
  //   'navigate' — the pane they were on is still open and they are looking at
  //                the tab, so they moved themselves.
  function classifyChange(previousActiveKey) {
    if (!isTabActive()) return 'silent';
    if (!previousActiveKey) return 'navigate';
    return state.panes.value.some((pane) => pane.key === previousActiveKey)
      ? 'navigate'
      : 'correct';
  }

  // Subscribe rather than diff in a Preact effect: the tab strip is not the
  // only thing that moves the active pane. `subscribe` fires once immediately
  // with the current value — that first call only primes the baseline.
  let primed = false;
  let seenSelection = null;
  let previousActiveKey = null;
  const unsubscribe = state.location.subscribe((location) => {
    const selection = location?.selection || '';
    const activeKey = state.view.value.activeKey || null;
    const wasOn = previousActiveKey;
    previousActiveKey = activeKey;
    if (!primed) { primed = true; seenSelection = selection; return; }
    if (selection === seenSelection) return;
    // A restore drives this same signal. Track the new value as seen (the URL
    // already names it) but say nothing: reconcileAfterRestore has the last
    // word once the attach settles, and it is the only thing that can tell an
    // operator's mid-flight move from the restore's own landing.
    if (restoring > 0) {
      seenSelection = selection;
      // Anything that is NOT the restore arriving at its own target is the
      // operator moving underneath it — remember it so the landing can defer.
      if (selection && selection !== restoreTarget && classifyChange(wasOn) === 'navigate') {
        interrupted = { selection, key: activeKey };
      }
      return;
    }
    seenSelection = selection;
    // Losing the last agent pane leaves the bare tab, which the router's own
    // snapshot reconcile already corrects (and the tab may be auto-leaving
    // entirely). Announcing it would forge an entry for a move nobody made.
    if (!selection) return;
    const kind = classifyChange(wasOn);
    if (kind === 'silent') return;
    announce(location, kind === 'correct');
  });

  const unbind = () => {
    disposed = true;
    unsubscribe();
    documentRef.removeEventListener(RESTORE_EVENT, onRestore);
  };
  documentRef.addEventListener(RESTORE_EVENT, onRestore);
  return unbind;
}
