import { h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { effect } from '@preact/signals';
import { mountTerminalWidget } from './terminals-core.js';
import { arcanePaletteEnabled } from './terminal-theme.js';
import { terminalComposeShortcutAction } from './terminal-compose-route.js';
import { openWebWindowPane, registerTerminalShellController, revealHeldSeed } from './terminals-tab.js';
import { bindTerminalNavRouting } from './terminal-nav-route.js';
import { hasShownOverlay } from './overlay-stack.js';
import { loadXtermRuntime } from './xterm-loader.js';
import { bindTerminalHandoffReceiver } from './terminal-handoff.js';
import { dragLeftRegion, dragScreenPoint } from './terminal-drag-out.js';
import { TabContextMenu, TabGroupRename, useArmedCollapse } from './tab-controls.js';
import { terminalAttachWidgetOptions } from './terminal-attach-config.js';
import { TerminalZoom, readTerminalFont, useTerminalFont } from './terminal-zoom.js';
import { leaveTerminalsAfterLastClose } from './terminal-close-nav.js';
import { terminalTabStatus } from './terminal-tab-status.js';
import {
  BackgroundTerminalsChip, backgroundPeers, backgroundTerminals, hasPeerBackgroundAgent, refreshBackgroundTerminals,
  startBackgroundTerminalsPoll, visibleBackgroundTerminals,
} from './background-terminals.js';
import { fmtCredits, fmtExactUSD, fmtUSD } from './costs-model.js';
import { terminalCostsHidden, subscribeTerminalCosts, toggleTerminalCosts } from './cost-display-toggle.js';
import {
  memberHumanMessages, openHumanNotificationReader,
} from './human-notification-attention.js';
import { StewardShutdown, openAccessPanel, requestStewardSummon, resumeAgent } from './companion-approval.js';
import { pendingApprovals } from './companion-model.js';
import { companionAvailable, openInCompanion, reclaimInCompanion } from './companion-facade.js';
import { BoardEditor, BoardList, BoardsGate } from './board-island.js';
import { BoardDeleteHost, askDeleteBoard, boardDeleteRequest, onBoardDeleted } from './board-manage.js';
import { boardOverlays, openBoardPane, openLinkedBoard, revealBoardWindow } from './board-overlay-state.js';
import { terminalWindowsShowing } from './terminal-window.js';
import { agentSessionSeed, chatAgentOfPane, overlayablePane } from './terminal-window-model.js';
import { droppedAt } from './overlay-window-model.js';
import { CHAT_WINDOW_KIND } from './chat-window-model.js';
import { overlayTabs } from './overlay-instances.js';
import { shellToast } from './shell-state.js';
import { usePeerNotifications } from './peer-notifications.js';
import { isPageNode, nodeFetch } from './node-api.js';
import { ViewersBadge } from './remote-viewers.js';
import { dashboardState } from './snapshot-store.js';
import { RemoteFilesPanel } from './remote-files-panel.js';
import { TransferCards } from './transfer-card.js';
import { fleetNodes, paneNode, paneScope } from './fleet-scope.js';
import { nodeColor, remoteNodeID } from './fleet-model.js';
import { remoteSessionAgents, startRemoteSessionStatusPoll } from './remote-session-status.js';
import { AgentGearMenu } from './groups-member-table.js';
import { GroupsInteractionProvider, useGroupsInteractions } from './groups-interactions.js';
import { ProcessPaneOverlay, ProcessTabIcons, processOverlayActions, reassignCandidates } from './process-terminal-overlay.js';
import { agentRunBounds } from './process-agent-stepper.js';
import { useDismissablePopover } from './hover-popover.js';
import { PaneDictation } from './terminal-dictation.js';
import { ShellHere, paneShellAgent } from './terminal-here.js';

const html = htm.bind(h);
const INTERACTION_HINT = 'Select: Option-drag (macOS) / Shift-drag (Linux/Windows) · Copy: Ctrl/Cmd+Shift+C';
const TERMINAL_TAB_DRAG_MIME = 'application/x-tclaude-terminal-tab';
const TERMINAL_GROUP_DRAG_MIME = 'application/x-tclaude-terminal-group';
// A pointer click on a group pill waits this long before collapsing so a
// double-click (rename) can cancel it. Roughly the platform double-click
// threshold; keyboard clicks bypass it entirely.

export function terminalTabReorderOffset(event) {
  if (event?.type !== 'keydown' || event.target?.closest?.('button')) return null;
  if (!event.shiftKey || !event.altKey) return null;
  if (event.key === 'ArrowLeft') return -1;
  if (event.key === 'ArrowRight') return 1;
  return null;
}

// The whole tab is the drag handle, so a drag whose gesture started on the
// close button has to be declined — otherwise pressing × and moving would drag
// the tab (a browser drags the nearest draggable ancestor even from a
// draggable=false child). This predicate is the guard; it is exported so it can
// be unit-tested against real rendered nodes, because the test DOM cannot
// deliver a bubbled/target-spoofed dragstart to the tab's own listener.
export function tabDragStartedOnClose(target) {
  return Boolean(target?.closest?.('.mux-tab-close'));
}

// composeTarget is who Ctrl/Cmd+M messages from a pane: its agent, or a
// peer pane's agent on that peer (seed.remote), sent through that node.
// paneFloats reports whether a Terminals pane offers Open in overlay. An agent
// floats through the companion, which a page without one cannot do.
function paneFloats(pane, snapshot) {
  if (snapshot?.collab_enabled !== true) return false;
  return !!pane.seed.board || overlayablePane(pane) || (companionAvailable() && !!chatAgentOfPane(pane, snapshot));
}

// soloFloats reports whether a separate window offers Open in overlay: the page
// that opened it said it could float, and that page is still there to take it.
function soloFloats(pane, win = globalThis.window) {
  return !!pane.seed.floats && !!win?.opener && !win.opener.closed;
}

// A chat window in the page. With Collab off the saved set is still there.
const chatWindowShowing = () => companionAvailable() && overlayTabs.peek().some((tab) => tab.kind === CHAT_WINDOW_KIND && !tab.minimized);

function composeTarget(pane, actions) {
  const { initialRetry: _initialRetry, ...seed } = pane.seed;
  const remote = pane.seed.remote;
  return Object.freeze({
    ...seed,
    ...(!seed.agent && remote?.agent ? { agent: remote.agent, node: remote.peer, nodeName: remote.peerLabel || '' } : {}),
    // activatePane refits and focuses the opaque xterm widget. If this exact
    // pane closed while the composer was open, activation safely returns false.
    restoreFocus: () => actions.activatePane(pane.key),
  });
}

// composeAgent is the agent a pane's Ctrl/Cmd+M messages, or ''.
function composeAgent(pane) {
  return pane?.seed?.agent || (SAFE_REMOTE_AGENT.test(pane?.seed?.remote?.agent || '') ? pane.seed.remote.agent : '');
}
const SAFE_REMOTE_AGENT = /^agt_[A-Za-z0-9]{4,64}$/;

// While a terminal drag is in flight, follow the pointer so the surface can say
// "release here and this terminal leaves" before the user commits. The same
// geometry decides the gesture on dragend, so the hint never lies.
//
// The listener also accepts the drag across the rest of the page. A browser
// draws the "no drop" cursor over anything that has not called preventDefault
// on dragover, so without this the gesture spent its whole life under a
// prohibition sign while being perfectly willing to fire on release. Accepting
// is scoped to a live terminal drag and never overrides a target that already
// claimed the event, so the tab strip's own reorder edges keep their meaning.
function useDragOutArmed(active, regionRef) {
  const [armed, setArmed] = useState(false);
  useEffect(() => {
    if (!active) {
      setArmed(false);
      return undefined;
    }
    const onDragOver = (event) => {
      // Arming first is deliberate: it answers "what happens if you release
      // here", which is pure geometry. Whether some other target claimed the
      // event has no bearing on that — the gesture fires from dragend either
      // way — so skipping the hint for a claimed dragover would hide a detach
      // that is still going to happen. The guard below is about a different
      // question: whose dropEffect wins.
      setArmed(dragLeftRegion(event, regionRef.current));
      if (event.defaultPrevented) return;
      event.preventDefault();
      if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
    };
    // Accepting the drag means a drop now lands here on release, so consume it:
    // the gesture is decided on dragend, and an unclaimed drop would otherwise
    // be offered to whatever else is listening.
    const onDrop = (event) => {
      if (!event.defaultPrevented) event.preventDefault();
    };
    document.addEventListener('dragover', onDragOver);
    document.addEventListener('drop', onDrop);
    return () => {
      document.removeEventListener('dragover', onDragOver);
      document.removeEventListener('drop', onDrop);
    };
  }, [active]);
  return armed;
}

function useTerminalThemeState() {
  const read = () => ({
    wizard: document.body.classList.contains('wizard'),
    palette: arcanePaletteEnabled(),
  });
  const [theme, setTheme] = useState(read);
  useEffect(() => {
    const sync = () => setTheme(read());
    document.addEventListener('tclaude:wizard', sync);
    document.addEventListener('tclaude:terminal-palette', sync);
    return () => {
      document.removeEventListener('tclaude:wizard', sync);
      document.removeEventListener('tclaude:terminal-palette', sync);
    };
  }, []);
  return theme;
}

// OpaqueTerminalHost is the one component/xterm boundary. Preact owns this div
// but never renders a child into it; the adapter alone owns its descendants.
function OpaqueTerminalHost({
  descriptor,
  runtimeID,
  active,
  activationToken,
  authenticate,
  autoReattach = true,
  className,
  fitClassName,
  actions,
  widgetFactory,
  onStatus,
  onReconnectChange,
  onSelectionChange,
  onComposeMessage,
  onDisconnect,
  onRemoteChange,
  initialFontSize = null,
  onFontChange,
  disconnectNotice = null,
}) {
  const hostRef = useRef(null);
  const widgetRef = useRef(null);
  useLayoutEffect(() => {
    const widget = widgetFactory({
      ...terminalAttachWidgetOptions(descriptor.seed.terminalAttach),
      host: hostRef.current,
      wsPath: descriptor.seed.ws,
      authenticate,
      autoReattach,
      active,
      onStatus,
      onReconnectChange,
      onSelectionChange,
      onComposeMessage,
      applicationClipboardShortcuts: descriptor.seed.harness === 'copilot',
      onDisconnect,
      onRemoteChange,
      initialFontSize,
      onFontChange,
      initialRetry: descriptor.seed.initialRetry === true,
    });
    widgetRef.current = widget;
    const unregister = actions.registerWidget(runtimeID, widget);
    widget.setActive(active);
    void widget.connect();
    return () => {
      widget.dispose();
      unregister();
      if (widgetRef.current === widget) widgetRef.current = null;
    };
  }, [descriptor.id]);
  useLayoutEffect(() => widgetRef.current?.setActive(active), [active, activationToken]);
  // Files pasted or dropped into this terminal show their upload progress
  // over it (transfer-card.js).
  return html`<div class=${className}><div ref=${hostRef} class=${fitClassName}></div><${TransferCards} scope=${descriptor.seed.ws} />${disconnectNotice}</div>`;
}

function CopyButton({ className, id, hasSelection, actions, runtimeID }) {
  const label = hasSelection
    ? 'Copy selected terminal text (Ctrl/Cmd+Shift+C)'
    : `Copy terminal text. ${INTERACTION_HINT}`;
  return html`
    <button
      type="button"
      class=${className}
      id=${id || null}
      data-has-selection=${hasSelection ? 'true' : 'false'}
      aria-label=${label}
      onClick=${() => void actions.widgetFor(runtimeID)?.copy()}
    >Copy</button>
  `;
}

function TerminalDisconnectNotice({ onReconnect }) {
  // While the dashboard-wide "Disconnected from agentd" curtain is up, this
  // terminal dropped for the same reason (the daemon is gone) and its restart
  // watch reconnects once agentd answers again; a second card on top of the
  // first only repeats it. Reading the signal here, not in TerminalPane, keeps
  // the subscription to panes that are actually disconnected.
  if (dashboardState.connection.value.status === 'disconnected') return null;
  return html`
    <div class="mux-terminal-disconnect-overlay" role="alert" aria-live="assertive">
      <div class="mux-terminal-disconnect-card">
        <div class="mux-terminal-disconnect-icon" aria-hidden="true">⚠️</div>
        <h2 class="mux-terminal-disconnect-title">Terminal disconnected</h2>
        <p class="mux-terminal-disconnect-body">This terminal is no longer connected.</p>
        <button type="button" class="mux-terminal-disconnect-reconnect" aria-label="Reconnect terminal" onClick=${onReconnect}>
          <span aria-hidden="true">⟳</span> Reconnect
        </button>
        <p class="mux-terminal-disconnect-status">Also available in the header above.</p>
      </div>
    </div>
  `;
}

// RemoteSharedNotice explains a shared remote terminal: the peer's session is
// also attached elsewhere, and the smallest attached terminal sets the size.
// When the peer granted sessions.takeover, taking it over disconnects the
// others so this view alone sizes the pane.
export function RemoteSharedNotice({ seed, remote, onTakeOver }) {
  if (!seed?.remote || !remote?.shared) return null;
  const others = remote.others || [];
  if (!others.length && !remote.limited) return null;
  const peer = seed.remote.peerLabel || seed.remote.peer;
  const text = others.length
    ? `Also attached on ${peer}: ${others.join(', ')} · sharing the size${remote.limited ? ' · size limited by another viewer' : ''}`
    : 'Size limited by another viewer';
  return html`<div class="term-remote-shared" role="status">
    <span>${text}</span>
    ${others.length && remote.canTakeOver ? html`<button type="button" class="mux-btn" data-term-take-over title=${`Disconnect the other viewers of this session on ${peer} and size it to this window`} onClick=${onTakeOver}>Take over</button>` : null}
  </div>`;
}

// RemoteBadge marks a peer's terminal in its pane header, as one chip in the
// node's colour: whose it is (the node) and whether this view can
// type (interactive) or only watch, as the peer granted it.
function RemoteBadge({ seed, remote, node }) {
  if (!seed.remote) return null;
  const peer = node?.name || seed.remote.peerLabel || seed.remote.peer;
  const mode = remote?.mode;
  const text = mode === 'interactive' ? '⌨ interactive' : mode === 'watch' ? '👁 watch-only' : 'remote';
  const title = mode === 'interactive'
    ? `${peer}'s agent: your keystrokes reach it. The peer shows REMOTE INPUT on the pane and can disconnect you.`
    : mode === 'watch' ? `${peer}'s agent, read-only: you see the pane but cannot type. The peer shows REMOTE WATCH on the pane.`
    : `${peer}'s agent, through this node`;
  const style = node?.color ? `--nc:${node.color}` : null;
  return html`<span class=${`term-remote-badge${mode ? ` ${mode}` : ''}`} style=${style} title=${title}><span class="term-remote-node">${peer}</span>${text}</span>`;
}

function TerminalPane({
  pane, active, activationToken, solo, manageTitle, actions, widgetFactory, onComposeMessage,
  snapshot = null, scope = null, processSnapshot = null,
}) {
  const [status, setStatus] = useState('disconnected');
  const [reconnect, setReconnect] = useState(false);
  const [hasSelection, setHasSelection] = useState(false);
  const [remote, setRemote] = useState(null);
  const [files, setFiles] = useState(false);
  const shellAgent = paneShellAgent(pane, solo);
  const filesOK = !!(pane.seed.remote && remote?.files && remote?.viewer && !remote?.closed);
  const [dragging, setDragging] = useState(false);
  const [stewardDown, setStewardDown] = useState(false);
  const [font, onFontChange] = useTerminalFont('tab', pane.key);
  // Dashboard panes get their status from PaneTab. Only the solo title needs
  // to subscribe to the standalone snapshot signal; keeping this null in the
  // dashboard avoids an extra signal subscriber per opaque xterm host.
  const agentStatus = manageTitle ? terminalTabStatus(pane, snapshot?.value, remoteSessionAgents.value) : null;
  const headerRef = useRef(null);
  // Only a detached window asks: in the dashboard the companion owns this.
  const liveSteward = solo && snapshot?.value?.agents?.[0]?.steward === true;
  // A solo pop-out has no tab strip to drag out of, so its header is the home
  // region: pull the title off the header and the terminal goes back to the
  // dashboard — the mirror image of dragging a dashboard tab out of the strip.
  const reattachArmed = useDragOutArmed(solo && dragging, headerRef);
  const theme = useTerminalThemeState();
  const composeMessage = composeAgent(pane) && onComposeMessage
    ? () => onComposeMessage(composeTarget(pane, actions))
    : null;
  const startTitleDrag = (event) => {
    event.stopPropagation();
    event.dataTransfer?.setData(TERMINAL_TAB_DRAG_MIME, pane.key);
    if (event.dataTransfer) event.dataTransfer.effectAllowed = 'move';
    setDragging(true);
  };
  const endTitleDrag = (event) => {
    setDragging(false);
    if (dragLeftRegion(event, headerRef.current)) void actions.reattachPane(pane.key);
  };
  const reconnectPane = () => void actions.widgetFor(pane.id)?.connect();
  const snapshotReady = Boolean(snapshot?.value);
  useEffect(() => {
    if (active && manageTitle) {
      const prefix = snapshotReady ? `${agentStatus.symbol} ` : '';
      document.title = `${prefix}${pane.label ? `${pane.label} — ` : ''}tclaude terminals`;
    }
  }, [active, agentStatus?.symbol, manageTitle, pane.label, snapshotReady]);
  return html`
    <div
      class=${`mux-pane${active ? ' active' : ''}${theme.wizard && theme.palette ? ' arcane-palette' : ''}${scope?.hidden ? ' scope-hidden' : ''}`}
      id=${pane.id}
      role=${solo ? null : 'tabpanel'}
    >
      <div ref=${headerRef} class=${`mux-pane-header${reattachArmed ? ' drag-out-armed' : ''}`}>
        ${solo ? html`
          ${snapshotReady ? html`
            <span
              class=${`mux-tab-status mux-pane-agent-status mux-tab-status-${agentStatus.className}`}
              role="img"
              aria-label=${agentStatus.ariaLabel}
              title=${agentStatus.title}
            ><span class="mux-tab-status-symbol">${agentStatus.symbol}</span></span>
          ` : null}
          <span
            class=${`mux-pane-title mux-pane-title-drag${dragging ? ' dragging' : ''}`}
            draggable="true"
            title="Drag off this header — or click Back to a tab — to move this terminal back to the dashboard's Terminals tab"
            onDragStart=${startTitleDrag}
            onDragEnd=${endTitleDrag}
          >${paneTitle(pane)}</span>
          ${reattachArmed ? html`<span class="mux-drag-out-hint">Release anywhere — even outside the browser — to send this terminal back to the dashboard</span>` : null}
        ` : html`<span class="mux-pane-title">${paneTitle(pane)}</span>`}
        <${RemoteBadge} seed=${pane.seed} remote=${remote} node=${scope?.node} />
        ${!pane.seed.remote && pane.seed.agent ? html`<${ViewersBadge} agentId=${pane.seed.agent} className="term-viewers-badge" />` : null}
        <span class="mux-pane-status" role="status" aria-live="polite" aria-atomic="true">${status}</span>
        <span class="terminal-interaction-hint">${INTERACTION_HINT}</span>
        ${reconnect ? html`<button type="button" class="mux-btn" onClick=${reconnectPane}>Reconnect</button>` : null}
        ${filesOK ? html`<button type="button" class="mux-btn" data-term-files aria-pressed=${files ? 'true' : 'false'} title="Browse the agent's project files on the peer: preview or download" onClick=${() => setFiles(!files)}>Files</button>` : null}
        <${CopyButton} className="mux-btn" hasSelection=${hasSelection} actions=${actions} runtimeID=${pane.id} />
        ${liveSteward && !stewardDown ? html`<${StewardShutdown} agent=${{ agentId: pane.seed.agent, name: pane.label || 'the Steward' }}
          fetchImpl=${fetchFromPage} onDown=${() => setStewardDown(true)} />` : null}
        ${composeMessage ? html`
          <button type="button" class="mux-btn" title="Send a queued message to this agent (Ctrl/Cmd+M)" onClick=${composeMessage}>✉ Message</button>
        ` : null}
        <label
          class="mux-palette-toggle"
          hidden=${!theme.wizard}
          title="Recolour terminal defaults with the persisted wizard palette; explicit application RGB colours are unchanged"
        >
          <input
            type="checkbox"
            checked=${theme.palette}
            aria-label="Arcane terminal palette"
            onChange=${(event) => actions.setArcanePaletteEnabled(event.currentTarget.checked)}
          />
          <span>Arcane palette</span>
        </label>
        ${solo ? html`
          <button type="button" class="mux-btn" title="Move this terminal back to a tab of the dashboard" onClick=${() => void actions.reattachPane(pane.key)}>Back to a tab</button>
          ${soloFloats(pane) ? html`<button type="button" class="mux-btn" title="Float this terminal in a window over the dashboard" onClick=${() => void actions.reattachPane(pane.key, { overlay: true })}>Open in overlay</button>` : null}
        ` : html`
          ${paneFloats(pane, processSnapshot?.value) ? html`<button type="button" class="mux-btn" title="Float this terminal in a window over the dashboard" onClick=${() => void actions.openPaneInOverlay(pane.key)}>Open in overlay</button>` : null}
          <button type="button" class="mux-btn" title="Move this terminal to a browser window of its own" onClick=${() => void actions.popOutPane(pane.key, { floats: paneFloats(pane, processSnapshot?.value) })}>Open in separate window</button>
        `}
      </div>
      ${stewardDown ? html`<${StewardDownNotice} pane=${pane} actions=${actions} />` : null}
      ${!pane.seed.remote && pane.seed.agent ? html`<${PaneProcessOverlay} agentId=${pane.seed.agent}
        snapshot=${processSnapshot} onComposeMessage=${onComposeMessage
          ? (seed) => onComposeMessage({ ...seed, restoreFocus: () => actions.activatePane(pane.key) })
          : null} />` : null}
      <${RemoteSharedNotice} seed=${pane.seed} remote=${remote}
        onTakeOver=${() => void actions.widgetFor(pane.id)?.takeOver()} />
      ${files && filesOK ? html`<${RemoteFilesPanel} terminal=${pane.seed.ws} viewer=${remote.viewer}
        peer=${pane.seed.remote.peerLabel || pane.seed.remote.peer} onClose=${() => setFiles(false)} />` : null}
      <${PaneDictation} pane=${pane} active=${active} solo=${solo} status=${status} actions=${actions}
        canType=${!pane.seed.remote || remote?.mode === 'interactive'} snapshot=${processSnapshot}
        target=${composeAgent(pane) ? composeTarget(pane, actions) : null}>
        ${shellAgent && html`<${ShellHere} ...${shellAgent} name=${pane.label} snapshot=${processSnapshot?.value} />`}
        <${TerminalZoom} font=${font} widget=${() => actions.widgetFor(pane.id)} />
      <//>
      <${OpaqueTerminalHost}
        descriptor=${pane}
        runtimeID=${pane.id}
        active=${active}
        activationToken=${activationToken}
        authenticate=${true}
        className="mux-pane-xterm"
        fitClassName="mux-pane-xterm-fit"
        actions=${actions}
        widgetFactory=${widgetFactory}
        onStatus=${setStatus}
        onReconnectChange=${setReconnect}
        onSelectionChange=${setHasSelection}
        onRemoteChange=${setRemote}
        initialFontSize=${readTerminalFont('tab', pane.key)}
        onFontChange=${onFontChange}
        onDisconnect=${(info) => { actions.paneDisconnected?.(pane.key); if (info?.closeTab) void actions.closePane(pane.key, { skipDetach: true }); }}
        onComposeMessage=${composeMessage}
        disconnectNotice=${status === 'disconnected' && reconnect ? html`<${TerminalDisconnectNotice} onReconnect=${reconnectPane} />` : null}
      />
    </div>
  `;
}

const fetchFromPage = (...args) => globalThis.fetch(...args);

// The page is going away by script and must not ask first.
const TERMINAL_LEAVING_EVENT = 'tclaude:terminal-leaving';

// StewardDownNotice is a detached Steward window after its shut down. A fresh
// Steward lives in the dashboard's companion, not here: the companion would
// attach it anyway, and a terminal has one attach, so this window would lose it.
// With no dashboard behind it, this window becomes one.
function StewardDownNotice({ pane, actions }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  // This window was attached to the same agent, so it comes back here.
  async function resume() {
    setBusy(true);
    setError('');
    try {
      await resumeAgent(fetchFromPage, pane.seed.agent);
      window.dispatchEvent(new Event(TERMINAL_LEAVING_EVENT));
      window.location.reload();
    } catch (cause) {
      setError(`Could not resume: ${cause?.message || cause}`);
      setBusy(false);
    }
  }
  async function start() {
    setBusy(true);
    setError('');
    try {
      const result = await requestStewardSummon(fetchFromPage);
      if (!result.agent_id) throw new Error('the summon returned no agent id');
      await actions.closePane(pane.key, { skipDetach: true });
      // The pane is gone, but its unload guard only lifts after the next paint.
      window.dispatchEvent(new Event(TERMINAL_LEAVING_EVENT));
      if (window.opener && !window.opener.closed) {
        try { window.opener.focus(); } catch (_) { /* browser focus is best-effort */ }
        window.close();
      } else {
        window.location.replace('/');
      }
    } catch (cause) {
      setError(`Could not start fresh: ${cause?.message || cause}`);
      setBusy(false);
    }
  }
  return html`<div class="steward-down-notice" role="status">
    <span><b>${pane.label || 'The Steward'}</b> is shut down. You can pick its conversation back up.</span>
    <button type="button" class="mux-btn" disabled=${busy} onClick=${resume}>Resume</button>
    <button type="button" class="mux-btn" disabled=${busy} onClick=${start}>${busy ? 'Starting…' : 'Start fresh'}</button>
    ${error ? html`<span class="steward-shutdown-error" role="alert">${error}</span>` : null}
  </div>`;
}

// BoardPane is a pane with a board in it rather than a terminal: no widget, and
// none of the agent chrome. The editor mounts the first time the tab is looked
// at, so a tab opened in the background costs nothing until then, and then stays
// mounted like a background terminal. A pop-out's page has no collab gate of
// its own: the daemon answers there.
function BoardPane({ pane, active, solo, manageTitle, actions, snapshot, scope = null }) {
  const seen = useRef(false);
  if (active) seen.current = true;
  useEffect(() => {
    if (active && manageTitle) document.title = `${pane.label ? `${pane.label} — ` : ''}tclaude board`;
  }, [active, manageTitle, pane.label]);
  // A pop-out's page is this one board: it stays, and the editor says so.
  useEffect(() => onBoardDeleted((id) => {
    if (id === pane.seed.board && !solo) void actions.closePane(pane.key);
  }), [pane.key, pane.seed.board, solo]);
  const editor = seen.current && snapshot ? html`<${BoardEditor}
    boardId=${pane.seed.board} onScreen=${active} snapshot=${snapshot} win=${window}
    visible=${() => solo || !!document.getElementById('tab-terminals')?.classList.contains('active')}
    fetchImpl=${fetchFromPage} notify=${shellToast} onOpenAgent=${(agent) => openInCompanion(agent)}
    onTitle=${(title) => actions.setPaneLabel(pane.key, title)}
    headActions=${solo
      ? html`<button type="button" class="mux-btn" title="Move this board back to a tab of the dashboard" onClick=${() => void actions.reattachPane(pane.key)}>Back to a tab</button>
        ${soloFloats(pane) ? html`<button type="button" class="mux-btn" title="Float this board in a window over the dashboard" onClick=${() => void actions.reattachPane(pane.key, { overlay: true })}>Open in overlay</button>` : null}`
      : html`
        <button type="button" class="mux-btn" title="Float this board in a window over the dashboard" onClick=${() => void actions.openPaneInOverlay(pane.key)}>Open in overlay</button>
        <button type="button" class="mux-btn" title="Move this board to a browser window of its own" onClick=${() => void actions.popOutPane(pane.key, { floats: true })}>Open in separate window</button>`} />` : null;
  return html`<div
    class=${`mux-pane mux-pane-board${active ? ' active' : ''}${scope?.hidden ? ' scope-hidden' : ''}`}
    id=${pane.id}
    role=${solo ? null : 'tabpanel'}
  >${solo ? editor : html`<${BoardsGate} snapshot=${snapshot}>${editor}<//>`}</div>`;
}

// BoardMenu is the strip's + button: open a board as a tab, or make a new one.
// It reads the snapshot itself so the strip does not re-render on each poll.
function BoardMenu({ state, snapshot }) {
  const [open, setOpen] = useState(false);
  const [picking, setPicking] = useState(false);
  const rootRef = useRef(null);
  const request = state.boardMenuRequest.value;
  useEffect(() => { if (request) setOpen(true); }, [request]);
  // The delete dialog and the file picker are outside the menu, and the menu is
  // waiting for them.
  useDismissablePopover(rootRef, open && !boardDeleteRequest.value && !picking, () => setOpen(false));
  if (snapshot?.value?.collab_enabled !== true) return null;
  return html`<div ref=${rootRef} class="mux-board-slot">
    <button type="button" class="mux-board-add" aria-haspopup="dialog" aria-expanded=${open ? 'true' : 'false'}
      title="Open a board in a tab, or create one" aria-label="Open or create a board"
      onClick=${() => setOpen((value) => !value)}>＋</button>
    ${open ? html`<div class="mux-board-menu" role="dialog" aria-label="Boards">
      <${BoardList} compact fetchImpl=${fetchFromPage} notify=${shellToast} onPicking=${setPicking}
        onOpen=${(id, label) => { setOpen(false); void openBoardPane(id, label); }} />
    </div>` : null}
  </div>`;
}

// PaneProcessOverlay reads the snapshot signal itself so only this small
// component, not the pane around the opaque xterm host, re-renders on each
// snapshot; it shows nothing unless features.processes is on.
function PaneProcessOverlay({ agentId, snapshot, onComposeMessage }) {
  const enabled = !!snapshot?.value?.processes_enabled;
  const [actions] = useState(() => processOverlayActions());
  return html`<${ProcessPaneOverlay} agentId=${agentId} enabled=${enabled}
    bounds=${enabled ? agentRunBounds(snapshot?.value, agentId) : null}
    candidatesFor=${(run, current) => reassignCandidates(snapshot?.value, run, current)}
    actions=${{ ...actions, onComposeMessage }} />`;
}

// paneUnreadNotifications returns the unread human notifications sent by the
// agent behind one terminal pane, newest first. A pane seed carries a single
// agent selector, which is the stable agent id where one exists and the conv id
// otherwise — the same both-identities match the Groups rows use. A peer's
// pane names its agent in seed.remote; its notifications are that peer's
// (peerLists, read from its mailbox), or the page's on that peer's own page.
export function paneUnreadNotifications(pane, snapshotValue, peerLists = {}) {
  const remote = paneRemoteNode(pane);
  const selector = pane?.seed?.agent || pane?.seed?.remote?.agent || '';
  if (remote) {
    const list = peerLists[remote];
    return selector && list ? memberHumanMessages(selector, { messages: list }, true) : [];
  }
  if (!selector || !snapshotValue) return [];
  return memberHumanMessages(selector, snapshotValue, true);
}

const PANE_NODE_ID = /^inst_[A-Za-z0-9]{4,64}$/;

// paneRemoteNode is the peer a pane's agent lives on when that is not the
// page's own node, else ''.
export function paneRemoteNode(pane) {
  const peer = pane?.seed?.remote?.peer || '';
  return PANE_NODE_ID.test(peer) && !isPageNode(peer) ? peer : '';
}

function unreadNotificationLabel(pane, count) {
  if (!count) return '';
  const label = pane.label || 'this agent';
  return `${count} unread notification${count === 1 ? '' : 's'} from ${label} — open quick reader`;
}

// openPaneNotification raises the quick reader on one of a pane's unread
// notifications. The sender is identified from the message itself: it carries
// both the stable agent id and the sending conversation generation, where the
// pane seed holds only whichever one the launcher happened to have.
function openPaneNotification(pane, message, launcher, returnFocus) {
  const node = paneRemoteNode(pane);
  return openHumanNotificationReader({
    sender: {
      agent: message.from_agent || '',
      conv: message.from_conv || '',
      label: message.from_title || pane.label || 'this agent',
      // A peer's agent: its notification lives there.
      ...(node ? { node, nodeName: pane.seed.remote.peerLabel || '' } : {}),
    },
    messageID: message.id,
    launcher,
    returnFocus: returnFocus || null,
  });
}

// TabAttention is the Terminals-tab twin of the Groups member row's yellow "!"
// glyph, and opens the very same quick reader. The strip is cramped and clips
// its own overflow, so the glyph sits inline in the tab and states its content
// in the tooltip instead of hovering a preview card.
function TabAttention({ pane, snapshot, messages: suppliedMessages = null }) {
  const messages = suppliedMessages ?? paneUnreadNotifications(pane, snapshot?.value);
  if (!messages.length) return null;
  const message = messages[0];
  const subject = message.subject || '(no subject)';
  const title = `${unreadNotificationLabel(pane, messages.length)}\n\n${subject}`;
  const open = (event) => {
    event.preventDefault();
    event.stopPropagation();
    // The glyph can disappear under focus — the operator marks the message
    // read from the reader, or a reply does — so the tab, not the glyph, is
    // the surface focus can reliably come back to.
    openPaneNotification(pane, message, event.currentTarget, event.currentTarget.closest('.mux-tab'));
  };
  return html`
    <button
      type="button"
      class="mux-tab-attention"
      draggable="false"
      title=${title}
      aria-label=${title}
      onClick=${open}
    ><span class="human-notification-attention-glyph" aria-hidden="true">!</span></button>
  `;
}

// TabApproval marks a tab whose agent is asking for permission, and answers it
// in the quick panel without leaving the terminal. It keeps to this node's panes
// and to Collab, which the panel belongs to.
export function paneApprovals(pane, snapshotValue) {
  if (snapshotValue?.collab_enabled !== true || paneRemoteNode(pane)) return [];
  const selector = pane?.seed?.agent || '';
  if (!selector) return [];
  return pendingApprovals(snapshotValue).filter((request) => [request.agent_id, request.conv_id, request.current_conv_id].includes(selector));
}

function TabApproval({ pane, snapshot }) {
  const requests = paneApprovals(pane, snapshot?.value);
  if (!requests.length) return null;
  const title = `${pane.label || 'This agent'} is asking for permission. Open the request`;
  return html`<button type="button" class="mux-tab-attention" draggable="false" title=${title} aria-label=${title}
    onClick=${(event) => {
      event.preventDefault();
      event.stopPropagation();
      openAccessPanel({ agentId: requests[0].agent_id || '', launcher: event.currentTarget, returnFocus: event.currentTarget.closest('.mux-tab') });
    }}><span aria-hidden="true">🔑</span></button>`;
}

function TerminalCostToggle() {
  const [hidden, setHidden] = useState(terminalCostsHidden);
  useLayoutEffect(() => subscribeTerminalCosts(setHidden), []);
  return html`<button type="button" class=${`cost-toggle mux-cost-toggle${hidden ? ' off' : ''}`}
    aria-label="Show agent costs" aria-pressed=${!hidden}
    title="Show or hide per-agent costs on these tabs (Groups has its own toggle); hovering a tab shows its cost either way"
    onClick=${toggleTerminalCosts}>$</button>`;
}

function TerminalCosts({ agent }) {
  const state = agent?.state || {};
  const cost = Number(state.cost_usd || 0);
  const virtualCost = Number(state.virtual_cost_usd || 0);
  const credits = Number(state.virtual_cost_credits || 0);
  const realTitle = `API cost this session: ${fmtExactUSD(cost)} (API/enterprise pricing)`;
  const virtualTitle = (state.harness === 'copilot' && credits > 0
    ? `${fmtCredits(credits)} — ${fmtExactUSD(virtualCost)} subscription value`
    : `${fmtExactUSD(virtualCost)} WHAT-IF cost this session`)
    + ' — estimated pay-per-token-equivalent cost; hypothetical, not a real charge (subscription)';
  return html`
    ${cost > 0 ? html`<span class="mux-tab-cost" title=${realTitle}
      aria-label=${realTitle} tabindex="0">${fmtUSD(cost)}</span>` : null}
    ${virtualCost > 0 ? html`<span class="mux-tab-cost mux-tab-cost-whatif" title=${virtualTitle}
      aria-label=${virtualTitle} tabindex="0">≈${fmtUSD(virtualCost)}</span>` : null}
  `;
}

// costTooltip is the cost line of a tab's hover title, shown whatever the $
// toggle says. Nothing when cost tracking is off (body.hide-costs) or the
// agent has no cost yet; the WHAT-IF figure only where the dashboard shows it.
function costTooltip(agent) {
  const body = globalThis.document?.body;
  if (!agent || body?.classList.contains('hide-costs')) return '';
  const state = agent.state || {};
  const cost = Number(state.cost_usd || 0);
  const virtualCost = Number(state.virtual_cost_usd || 0);
  const parts = [];
  if (cost > 0) parts.push(`API cost ${fmtExactUSD(cost)}`);
  if (virtualCost > 0 && body?.classList.contains('cost-whatif')) parts.push(`≈${fmtExactUSD(virtualCost)} WHAT-IF (hypothetical)`);
  return parts.length ? `Cost this session: ${parts.join(' · ')}` : '';
}

// tabGearMember is who a tab's ⚙ acts on: the agent this node's roster
// matched to the pane, or the peer agent a remote pane attaches to. Plain
// shells, and a peer's own view (whose roster is not this browser's panes),
// get none.
function tabGearMember(pane, agent, snapshotValue, scope) {
  if (snapshotValue?.peer_view) return null;
  const remote = pane?.seed?.remote;
  if (remote?.agent) {
    const node = scope?.node || { id: remote.peer, name: remote.peerLabel || remote.peer.slice(0, 13), local: false };
    return { member: { agent_id: remote.agent, conv_id: '', title: pane.label || remote.agent, state: {} }, node };
  }
  return agent?.conv_id ? { member: agent, node: null } : null;
}

// TabGear holds a tab's ⚙ menu. The strip scrolls sideways and so clips
// anything hanging below it: an open menu is pinned to the viewport under its
// button instead. Clicks and keys inside never reach the tab, so they neither
// switch nor close it.
function TabGear({ gear, snapshot, menuKey }) {
  const ref = useRef(null);
  // Reading the interactions context re-renders this on open and close, so
  // the menu is placed as it opens rather than on the next snapshot publish.
  const open = useGroupsInteractions().openMenuKey === menuKey;
  useLayoutEffect(() => {
    const menu = ref.current?.querySelector('.action-menu');
    const button = ref.current?.querySelector('.cog-btn');
    if (!open || !menu || !button) return;
    const rect = button.getBoundingClientRect();
    const width = menu.offsetWidth || 0;
    const top = Math.round(rect.bottom + 4);
    menu.style.top = `${top}px`;
    menu.style.left = `${Math.max(4, Math.round(Math.min(rect.left, (globalThis.innerWidth || 0) - width - 8)))}px`;
    // A fixed menu cannot be scrolled to: it scrolls itself instead.
    menu.style.maxHeight = `calc(100vh - ${top + 8}px)`;
  }, [open]);
  // row-actions gives the menu the Groups row menu's styling (and its wizard
  // theme) from the same rules, rather than a copy of them.
  return html`<span ref=${ref} class="mux-tab-gear row-actions" draggable="false">
    <${AgentGearMenu} member=${gear.member} node=${gear.node} snapshot=${snapshot?.value} menuKey=${menuKey} />
  </span>`;
}

const inTabGear = (event) => !!event.target?.closest?.('.mux-tab-gear');

// paneTitle is the name a tab and its pane header show. A peer's pane is
// labelled by its NodePill, so the " @ peer" suffix older remote panes were
// opened with is dropped rather than shown twice.
export function paneTitle(pane) {
  const label = pane?.label || '';
  const peer = pane?.seed?.remote?.peerLabel;
  const suffix = peer ? ` @ ${peer}` : '';
  return suffix && label.endsWith(suffix) && label.length > suffix.length ? label.slice(0, -suffix.length) : label;
}

// NodePill names a remote pane's node (fleet-scope.js), in that node's
// colour. This node's panes go unlabelled: the label is there to
// set a peer's panes apart. A tab is short of room for the agent's name, so it
// gets the compact form, the node's initial, with the full name on hover; the
// pane header has room for the whole name.
function NodePill({ node, compact = false }) {
  if (!node || node.local) return null;
  const cls = `mux-node-pill${compact ? ' compact' : ''}`;
  return html`<span class=${cls} style=${`--nc:${node.color}`} title=${`On ${node.name}`}>${compact ? [...node.name][0]?.toUpperCase() : node.name}</span>`;
}

// A board has no agent behind it, so its tab says what it is instead.
function boardTabStatus(pane) {
  const label = `Board ${paneTitle(pane)}`;
  return { key: 'board', className: 'board', symbol: '✎', title: label, ariaLabel: label, agent: null };
}

function PaneTab({
  pane, active, menuOpen, groupId = null, actions, openMenu, dragging, dropSide,
  onDragStart, onDragEnd, onDragOver, onDragLeave, onDrop, onReordered, snapshot = null, scope = null,
  peerNotifications = {},
}) {
  const board = !!pane.seed.board;
  const agentStatus = board ? boardTabStatus(pane) : terminalTabStatus(pane, snapshot?.value, remoteSessionAgents.value);
  const unreadMessages = paneUnreadNotifications(pane, snapshot?.value, peerNotifications);
  const notificationLabel = unreadNotificationLabel(pane, unreadMessages.length);
  const ariaLabel = [agentStatus.ariaLabel, notificationLabel].filter(Boolean).join('. ');
  const costLine = costTooltip(agentStatus.agent);
  const gear = tabGearMember(pane, agentStatus.agent, snapshot?.value, scope);
  const activate = (event) => {
    if (inTabGear(event)) return;
    if (event.type === 'keydown' && event.target.closest('button')) return;
    const reorderOffset = terminalTabReorderOffset(event);
    if (reorderOffset !== null) {
      event.preventDefault();
      event.stopPropagation();
      const moved = actions.movePaneByOffset(pane.key, reorderOffset);
      if (moved) onReordered(moved);
      return;
    }
    if (event.type === 'keydown' && event.key !== 'Enter' && event.key !== ' ' && event.key !== 'Spacebar') return;
    if (event.type === 'keydown') event.preventDefault();
    actions.activatePane(pane.key);
  };
  const openContextMenu = (event) => {
    if (inTabGear(event)) return false;
    const keyboard = event.type === 'keydown';
    if (keyboard && event.key !== 'ContextMenu' && !(event.key === 'F10' && event.shiftKey)) return false;
    event.preventDefault();
    event.stopPropagation();
    const rect = event.currentTarget.getBoundingClientRect();
    openMenu({
      kind: 'pane',
      key: pane.key,
      board,
      boardId: pane.seed.board,
      floats: paneFloats(pane, snapshot?.value),
      groupId,
      label: pane.label,
      trigger: event.currentTarget,
      x: keyboard ? rect.left : event.clientX,
      y: keyboard ? rect.bottom : event.clientY,
    });
    return true;
  };
  const onKeyDown = (event) => {
    if (!openContextMenu(event)) activate(event);
  };
  // The whole tab is the drag handle, not just the label, so the grab target is
  // the full tab. A drag that starts on the close button is cancelled, so the ×
  // stays a click target and never begins a tab drag.
  const startDrag = (event) => {
    if (tabDragStartedOnClose(event.target) || inTabGear(event)) {
      event.preventDefault();
      return;
    }
    onDragStart(event, pane.key);
  };
  return html`
    <div
      class=${`mux-tab${active ? ' active' : ''}${dragging ? ' dragging' : ''}${dropSide ? ` drop-${dropSide}` : ''}${scope?.hidden ? ' scope-hidden' : ''}`}
      role="tab"
      data-pane-key=${pane.key}
      data-agent-status=${agentStatus.key}
      tabIndex="0"
      draggable="true"
      aria-selected=${active ? 'true' : 'false'}
      aria-label=${ariaLabel}
      aria-controls=${pane.id}
      aria-keyshortcuts="Alt+Shift+ArrowLeft Alt+Shift+ArrowRight"
      aria-describedby="terminal-tab-reorder-help"
      aria-haspopup="menu"
      aria-expanded=${menuOpen ? 'true' : 'false'}
      title=${`${agentStatus.title}.${costLine ? ` ${costLine}.` : ''} Drag to reorder · drop onto another tab to group them · drag off the strip to detach · Alt+Shift+Left/Right to move · Right-click or Shift+F10 for actions`}
      onClick=${activate}
      onKeyDown=${onKeyDown}
      onContextMenu=${openContextMenu}
      onDragStart=${startDrag}
      onDragEnd=${onDragEnd}
      onDragOver=${(event) => onDragOver(event, pane.key)}
      onDragLeave=${(event) => onDragLeave(event, pane.key)}
      onDrop=${(event) => onDrop(event, pane.key)}
    >
      <span
        class=${`mux-tab-status mux-tab-status-${agentStatus.className}`}
        aria-hidden="true"
        title=${agentStatus.title}
      ><span class="mux-tab-status-symbol">${agentStatus.symbol}</span></span>
      ${gear ? html`<${TabGear} gear=${gear} snapshot=${snapshot} menuKey=${`tab:${pane.key}`} />` : null}
      <${TabAttention} pane=${pane} snapshot=${snapshot} messages=${unreadMessages} />
      <${TabApproval} pane=${pane} snapshot=${snapshot} />
      <${NodePill} node=${scope?.node} compact />
      <span class="mux-tab-label">${paneTitle(pane)}</span>
      <${TerminalCosts} agent=${agentStatus.agent} />
      ${!pane.seed.remote && pane.seed.agent ? html`<${ProcessTabIcons} agentId=${pane.seed.agent}
        enabled=${!!snapshot?.value?.processes_enabled} />` : null}
      <button
        type="button"
        class="mux-tab-close"
        draggable="false"
        title=${board ? 'Close this board' : 'Close this terminal'}
        aria-label=${`Close ${pane.label}`}
        onClick=${(event) => { event.stopPropagation(); void actions.closePane(pane.key); }}
      >×</button>
    </div>
  `;
}

// GroupStack renders one tab stack: a pill that names and collapses the stack,
// followed by its member tabs. The pill is the stack's drop target — releasing
// a tab on it joins the stack — and its own context-menu anchor.
//
// The wrapper carries role="group" inside the strip's tablist so assistive tech
// announces "N tabs, <name>" around the members instead of a flat run. The pill
// itself is a button, not a tab: it selects nothing, it only opens and closes.
function GroupStack({
  group, panes, activeKey, actions, renaming, onRenameStart, onRenameCommit, onRenameCancel,
  openMenu, menuOpen, dropActive, dragging, onPillDragOver, onPillDragLeave, onPillDrop,
  onPillDragStart, onPillDragEnd, onKeyboardMove, renderTab, snapshot = null, peerNotifications = {},
}) {
  // A single click collapses, a double click renames — the two share the pill.
  // Collapsing is NOT a pure toggle: collapsing a group that owns the active
  // terminal deliberately moves activation outside the group, and expanding
  // does not move it back. So the collapse cannot fire on the first click of a
  // double-click and be "undone" — that would silently switch the active
  // terminal. Instead a pointer click ARMS the collapse on a short timer that a
  // second click (the double-click) cancels, so renaming never collapses at
  // all. A keyboard-synthesised click (Enter/Space, detail 0) has no
  // double-click to wait for, so it collapses immediately. F2 renames.
  const { onPillClick, clearCollapseTimer } = useArmedCollapse(() => actions.toggleGroupCollapsed(group.id));
  const startRename = () => {
    clearCollapseTimer();
    onRenameStart(group.id);
  };
  const renameOnDblClick = (event) => {
    event.preventDefault();
    event.stopPropagation();
    startRename();
  };
  // A collapsed stack still shows the terminal the operator is looking at when
  // there is nowhere outside the stack for activation to move to — collapsing
  // must never leave the strip with no visible active tab.
  const visible = group.collapsed ? panes.filter((pane) => pane.key === activeKey) : panes;
  // A collapsed stack hides every tab but the active one, and with them their
  // attention glyphs. The pill carries a hint so a notification inside a folded
  // stack is still visible from the strip.
  const hiddenNotifications = group.collapsed
    ? panes
      .filter((pane) => !visible.includes(pane))
      .flatMap((pane) => paneUnreadNotifications(pane, snapshot?.value, peerNotifications)
        .map((message) => ({ pane, message })))
    : [];
  const hiddenUnread = hiddenNotifications.length;
  // The pill hint opens the quick reader on the first hidden notification,
  // like a tab's own glyph would. It is a span inside the pill button (a
  // button may not nest another), so it is a pointer shortcut only; keyboard
  // users expand the stack and reach the tab glyphs.
  const openHiddenNotification = (event) => {
    const first = hiddenNotifications[0];
    if (!first) return;
    event.preventDefault();
    event.stopPropagation();
    openPaneNotification(first.pane, first.message, event.currentTarget.closest('.mux-group-pill'),
      event.currentTarget.closest('.mux-group-pill'));
  };
  const openMenuAt = (event) => {
    const keyboard = event.type === 'keydown';
    if (keyboard && event.key !== 'ContextMenu' && !(event.key === 'F10' && event.shiftKey)) return false;
    event.preventDefault();
    event.stopPropagation();
    const rect = event.currentTarget.getBoundingClientRect();
    openMenu({
      kind: 'group',
      id: group.id,
      label: group.name,
      collapsed: group.collapsed,
      trigger: event.currentTarget,
      x: keyboard ? rect.left : event.clientX,
      y: keyboard ? rect.bottom : event.clientY,
    });
    return true;
  };
  const onPillKeyDown = (event) => {
    if (event.key === 'F2') {
      event.preventDefault();
      event.stopPropagation();
      startRename();
      return;
    }
    // Alt+Shift+Left/Right moves the whole stack among its neighbours — the
    // keyboard mirror of dragging the pill.
    if (event.altKey && event.shiftKey && (event.key === 'ArrowLeft' || event.key === 'ArrowRight')) {
      event.preventDefault();
      event.stopPropagation();
      onKeyboardMove(group.id, event.key === 'ArrowRight' ? 1 : -1);
      return;
    }
    openMenuAt(event);
  };
  return html`
    <div
      class=${`mux-tab-group mux-group-${group.color}${group.collapsed ? ' collapsed' : ''}${dropActive ? ' drop-into' : ''}${dragging ? ' dragging' : ''}`}
      role="group"
      aria-label=${`${group.name} — ${panes.length} terminal${panes.length === 1 ? '' : 's'}`}
      onDragOver=${(event) => onPillDragOver(event, group.id)}
      onDragLeave=${(event) => onPillDragLeave(event, group.id)}
      onDrop=${(event) => onPillDrop(event, group.id)}
    >
      ${renaming ? html`
        <${TabGroupRename} group=${group} label=${`Rename terminal group ${group.name}`} onCommit=${onRenameCommit} onCancel=${onRenameCancel} />
      ` : html`
        <button
          type="button"
          class="mux-group-pill"
          draggable="true"
          data-group-id=${group.id}
          aria-expanded=${group.collapsed ? 'false' : 'true'}
          aria-haspopup="menu"
          aria-describedby="terminal-group-pill-help"
          aria-keyshortcuts="Alt+Shift+ArrowLeft Alt+Shift+ArrowRight"
          title=${`Drag to move the whole ${group.name} group · click to ${group.collapsed ? 'expand' : 'collapse'} · double-click or F2 to rename · right-click or Shift+F10 for group actions`}
          onClick=${onPillClick}
          onDblClick=${renameOnDblClick}
          onContextMenu=${openMenuAt}
          onKeyDown=${onPillKeyDown}
          onDragStart=${(event) => onPillDragStart(event, group.id)}
          onDragEnd=${onPillDragEnd}
          aria-label=${`${group.name}, ${panes.length} terminal${panes.length === 1 ? '' : 's'}`
            + (hiddenUnread ? `, ${hiddenUnread} unread notification${hiddenUnread === 1 ? '' : 's'} in a collapsed tab` : '')}
          data-menu-open=${menuOpen ? 'true' : 'false'}
        >
          <span class="mux-group-caret" aria-hidden="true">${group.collapsed ? '▸' : '▾'}</span>
          <span class="mux-group-name">${group.name}</span>
          <span class="mux-group-count" aria-hidden="true">${panes.length}</span>
          ${hiddenUnread ? html`<span class="human-notification-attention-glyph mux-group-attention"
            aria-hidden="true" title=${`${hiddenUnread} unread notification${hiddenUnread === 1 ? '' : 's'} in a collapsed tab — open quick reader`}
            onClick=${openHiddenNotification} onDblClick=${(event) => event.stopPropagation()}>!</span>` : null}
        </button>
      `}
      ${visible.map((pane) => renderTab(pane))}
    </div>
  `;
}

// StripGap is a thin drop target at a group boundary — the one place a plain
// tab drop cannot land, because the only tab to drop onto there belongs to the
// stack and dropping on it would join.
//
// It is ALWAYS rendered, even at rest, and only reveals itself (via the
// `active` class → width) while a drag is in flight. Rendering it conditionally
// would insert a node next to the group the moment a drag begins; that
// insertion makes Preact reconcile — and recreate — the neighbouring group
// subtree, and recreating the DOM node the browser just picked up as a drag
// source silently aborts the native drag. Grabbing a grouped tab then "did
// nothing" most of the time. Keeping the node present and toggling a class
// never disturbs the drag source.
function StripGap({ active, armed, grow = false, onDragOver, onDragLeave, onDrop }) {
  return html`
    <div
      class=${`mux-strip-gap${grow ? ' grow' : ''}${active ? ' active' : ''}${armed ? ' armed' : ''}`}
      aria-hidden="true"
      onDragOver=${onDragOver}
      onDragLeave=${onDragLeave}
      onDrop=${onDrop}
    ></div>
  `;
}

function PaneContextMenu({
  menu, actions, groups, closeMenu, focusAfterAction, focusAfterDismiss, onRenameGroup,
}) {
  return html`<${TabContextMenu} menu=${menu} closeMenu=${closeMenu} focusAfterAction=${focusAfterAction} focusAfterDismiss=${focusAfterDismiss}
    items=${({ item, separator }) => {
      // The grouping commands live in this menu rather than only on the drag
      // gesture so every one of them is reachable from the keyboard (Shift+F10 on a
      // focused tab), which drag-and-drop alone can never be.
      return menu.kind === 'group' ? [
        item(menu.collapsed ? 'Expand group' : 'Collapse group', () => actions.toggleGroupCollapsed(menu.id)),
        item('Rename group…', () => onRenameGroup(menu.id), { refocus: false }),
        item('Ungroup tabs', () => actions.dissolveGroup(menu.id)),
        separator,
        item('Close tabs in group', () => actions.closeGroupPanes(menu.id), { className: 'mux-tab-menu-item danger' }),
      ] : [
        item('New group from this tab', () => {
          const group = actions.createGroup({ keys: [menu.key] });
          if (group) onRenameGroup(group.id);
        }, { refocus: false }),
        ...groups
          .filter((group) => group.id !== menu.groupId)
          .map((group) => item(`Add to “${group.name}”`, () => actions.assignPaneToGroup(menu.key, group.id))),
        ...(menu.groupId ? [item('Remove from group', () => actions.assignPaneToGroup(menu.key, null))] : []),
        separator,
        ...(menu.floats ? [item('Open in overlay', () => actions.openPaneInOverlay(menu.key))] : []),
        item('Open in separate window', () => actions.popOutPane(menu.key, { floats: menu.floats })),
        separator,
        ...(menu.board ? [item('Delete board…', () => askDeleteBoard({ id: menu.boardId, title: menu.label }), { className: 'mux-tab-menu-item danger', refocus: false })] : []),
        item('Close tab', () => actions.closePane(menu.key)),
        item('Close other tabs', () => actions.closeOtherPanes(menu.key)),
        item('Close all tabs', () => actions.closeAllPanes(), { className: 'mux-tab-menu-item danger' }),
      ];
    }} />`;
}

function TerminalTabs({
  state, actions, widgetFactory, onComposeMessage, composeMessageReady = null,
  composeMessageDialogKind = () => '',
  solo = false, manageTitle = false, empty = false, snapshot = null, scope = null,
}) {
  const current = state.view.value;
  // A fused node scope (fleet-scope.js) shows the ticked nodes' panes, each
  // labelled with its node; the rest stay open, just out of sight.
  const scopeView = scope?.view.value;
  // A peer's pane carries its node pill in any view, fused or not: the pill is
  // the one place a tab says whose terminal it is.
  const scopeOf = (pane) => {
    const scoped = paneScope(scopeView?.fleet, scopeView?.fused, pane.seed, nodeColor);
    if (scoped.node || !pane.seed?.remote?.peer) return scoped;
    return { ...scoped, node: paneNode(scopeView?.fleet || { peers: [] }, pane.seed, nodeColor) };
  };
  const shownPanes = current.panes.filter((pane) => !scopeOf(pane).hidden);
  // A peer's agent panes raise their ! from that peer's unread notifications,
  // read while the strip is on screen from each peer the hub reports online.
  const offline = new Set((scopeView?.fleet ? fleetNodes(scopeView.fleet) : []).filter((n) => n?.online === false).map((n) => n.id));
  const notifyPeers = [...new Set(current.panes.map(paneRemoteNode).filter((id) => id && !offline.has(id)))].sort();
  const stripShown = solo || dashboardState.activeTab.value === 'terminals';
  const peerNotifications = usePeerNotifications({ active: stripShown, peers: notifyPeers, fetchFor: (id) => nodeFetch(id) });
  const activeHidden = current.activeKey && scopeOf(current.panes.find((p) => p.key === current.activeKey) || {}).hidden;
  // Quietly: unticking a node must not pull the operator into this tab.
  useEffect(() => {
    if (activeHidden && shownPanes.length) actions.activatePane(shownPanes[0].key, { reveal: false });
  }, [activeHidden, shownPanes[0]?.key]);
  // skipHidden makes a keyboard move step over tabs the scope hides: one
  // press moves past the next visible neighbour, never only past unseen ones.
  const skipHidden = (move, movedKeys) => (id, offset) => {
    if (!scopeView?.fused) return move(id, offset);
    const dir = offset > 0 ? 1 : -1;
    let moved = null;
    for (let guard = state.view.value.panes.length; guard >= 0; guard -= 1) {
      const before = state.view.value.panes.map((p) => p.key);
      const out = move(id, dir);
      if (!out) break;
      moved = out;
      const after = state.view.value.panes;
      const own = new Set(movedKeys(id));
      const at = (keys, k) => keys.indexOf(k);
      const firstOwn = (keys) => keys.findIndex((k) => own.has(k));
      const afterKeys = after.map((p) => p.key);
      const passed = after.filter((p) => !own.has(p.key)
        && (at(before, p.key) < firstOwn(before)) !== (at(afterKeys, p.key) < firstOwn(afterKeys)));
      if (!passed.length || passed.some((p) => !scopeOf(p).hidden)) break;
    }
    return moved;
  };
  const scopedActions = scopeView?.fused
    ? { ...actions, movePaneByOffset: skipHidden(actions.movePaneByOffset, (key) => [key]) }
    : actions;
  // Renames land in the roster snapshot; carry them onto the open tabs. A
  // signal effect (not a render dependency) so each poll does not re-render
  // the whole strip just to find nothing changed.
  useEffect(() => (snapshot ? effect(() => { state.syncAgentLabels?.(snapshot.value); }) : undefined), [snapshot, state]);
  const moveGroupScoped = skipHidden(actions.moveGroupByOffset, (gid) => (current.segments.find((sg) => sg.type === 'group' && sg.group.id === gid)?.panes || []).map((p) => p.key));
  const composeMessageAvailable = Boolean(onComposeMessage) &&
    (composeMessageReady === null || composeMessageReady.value);
  const hasPanes = current.panes.length > 0;
  // Sessions still running in tmux with no terminal attached. The dashboard
  // tab lists them; a pop-out window shows only its own panes.
  const backgroundRows = solo ? [] : visibleBackgroundTerminals(backgroundTerminals.value, current.panes);
  const hasBackground = backgroundRows.length > 0;
  const shellRef = useRef(null);
  const tabsRef = useRef(null);
  const panesRef = useRef(null);
  const dragKeyRef = useRef(null);
  const dragGroupIdRef = useRef(null);
  const droppedRef = useRef(false);
  const [dragKey, setDragKey] = useState(null);
  const [dragGroupId, setDragGroupId] = useState(null);
  const [dropTarget, setDropTarget] = useState(null);
  const [reorderAnnouncement, setReorderAnnouncement] = useState('');
  const [tabMenu, setTabMenu] = useState(null);
  const [menuFocusRequest, setMenuFocusRequest] = useState(0);
  const [renamingGroup, setRenamingGroup] = useState(null);
  const [groupDropTarget, setGroupDropTarget] = useState(null);
  const [stripGap, setStripGap] = useState(null);

  const detachArmed = useDragOutArmed(Boolean(dragKey), tabsRef);
  const clearDrag = () => {
    dragKeyRef.current = null;
    dragGroupIdRef.current = null;
    droppedRef.current = false;
    setDragKey(null);
    setDragGroupId(null);
    setDropTarget(null);
    setGroupDropTarget(null);
    setStripGap(null);
  };
  // The live region has to say where a tab landed AND which stack it landed in,
  // because a keyboard move at a stack edge changes both at once.
  const announceReorder = ({ pane, index, count, group, leftGroup }) => {
    const where = group ? ` in group ${group.name}`
      : leftGroup ? `, out of group ${leftGroup.name}` : '';
    setReorderAnnouncement(`Moved ${pane.label} to position ${index + 1} of ${count}${where}.`);
  };
  const groupIdOfKey = (key) => {
    const segment = current.segments.find((candidate) => candidate.panes.some((pane) => pane.key === key));
    return segment?.type === 'group' ? segment.group.id : null;
  };
  const labelOfKey = (key) => current.panes.find((pane) => pane.key === key)?.label || 'terminal';
  // A tab has three drop zones: the outer quarter on each side reorders the
  // dragged tab before/after it, and the centre half combines the two into a
  // group. Dropping a tab on another tab that is not yet in a group is the
  // discoverable way to CREATE a group — the same gesture Chrome uses — so it
  // no longer requires finding the context menu first. When the two tabs are
  // already in the same group the centre would do nothing, so it degrades to a
  // plain before/after reorder instead of highlighting a no-op.
  const tabDropSide = (event, element, sourceKey, targetKey) => {
    const bounds = element.getBoundingClientRect();
    const fraction = bounds.width ? (event.clientX - bounds.left) / bounds.width : 0.5;
    const half = fraction < 0.5 ? 'before' : 'after';
    if (fraction < 0.25 || fraction > 0.75) return half;
    const targetGroup = groupIdOfKey(targetKey);
    return targetGroup && targetGroup === groupIdOfKey(sourceKey) ? half : 'group';
  };
  const focusPaneTab = (key) => {
    for (const tab of shellRef.current?.querySelectorAll('.mux-tab[data-pane-key]') || []) {
      if (tab.getAttribute('data-pane-key') === key) { tab.focus(); return; }
    }
  };
  // A keyboard move can carry a tab across a group boundary, which changes its
  // DOM parent (group wrapper ⇄ strip), so Preact recreates the node and focus
  // falls to <body>. Put focus back on the tab the operator is still moving,
  // after the render that reparented it — otherwise the next arrow keypress is
  // swallowed. Drops keep the pointer and need no such repair.
  const announceAndRefocus = (moved) => {
    announceReorder(moved);
    queueMicrotask(() => focusPaneTab(moved.pane.key));
  };
  const startTabDrag = (event, key) => {
    event.stopPropagation();
    event.dataTransfer.setData(TERMINAL_TAB_DRAG_MIME, key);
    event.dataTransfer.effectAllowed = 'move';
    dragKeyRef.current = key;
    droppedRef.current = false;
    setDragKey(key);
    setDropTarget(null);
  };
  // A stack moves as one block. Dragging its pill is a distinct gesture from
  // dragging a member tab (which reorders that one tab), so it carries its own
  // drag id; the drop handlers below route to moveGroup instead of reorderPane
  // when a group drag is in flight. A group is never detached off the strip.
  const announceGroupMove = ({ group, index, count }) => {
    setReorderAnnouncement(
      `Moved group ${group.name} (${count} terminal${count === 1 ? '' : 's'}) to position ${index + 1}.`,
    );
  };
  const focusGroupPill = (groupID) => {
    for (const pill of shellRef.current?.querySelectorAll('.mux-group-pill[data-group-id]') || []) {
      if (pill.getAttribute('data-group-id') === groupID) { pill.focus(); return; }
    }
  };
  const announceGroupAndRefocus = (moved) => {
    announceGroupMove(moved);
    queueMicrotask(() => focusGroupPill(moved.group.id));
  };
  const moveGroupKeyboard = (groupID, offset) => {
    const moved = moveGroupScoped(groupID, offset);
    if (moved) announceGroupAndRefocus(moved);
  };
  const startGroupDrag = (event, groupID) => {
    event.stopPropagation();
    event.dataTransfer.setData(TERMINAL_GROUP_DRAG_MIME, groupID);
    event.dataTransfer.effectAllowed = 'move';
    dragGroupIdRef.current = groupID;
    dragKeyRef.current = null;
    droppedRef.current = false;
    setDragGroupId(groupID);
    setDragKey(null);
    setDropTarget(null);
  };
  const endGroupDrag = () => clearDrag();
  // Nothing in the strip accepted the drag and it ended clear of the strip:
  // treat that as "pull this terminal out of the dashboard". A drop on a
  // sibling tab (reorder) or a cancelled drag never reaches the detach.
  //
  // Dragging a terminal out asks for it out of the way, so it floats where it
  // was dropped. What cannot float lands in a browser window of its own, sized to
  // the pane it is leaving and placed where the drag was released.
  const endTabDrag = (event) => {
    const key = dragKeyRef.current;
    const detach = !droppedRef.current && key && dragLeftRegion(event, tabsRef.current);
    clearDrag();
    if (!detach) return;
    if (paneFloats(state.panes.value.find((pane) => pane.key === key), snapshot?.value)) {
      void actions.openPaneInOverlay(key, { at: droppedAt(event) });
      return;
    }
    void actions.popOutPane(key, {
      detachTo: {
        size: panesRef.current?.getBoundingClientRect?.(),
        at: dragScreenPoint(event),
      },
    });
  };
  // A before/after split, no centre group zone — used while dragging a whole
  // stack, which drops next to a tab rather than combining with it.
  const beforeAfter = (event, element) => {
    const bounds = element.getBoundingClientRect();
    return event.clientX < bounds.left + bounds.width / 2 ? 'before' : 'after';
  };
  const tabDragOver = (event, targetKey) => {
    const groupDrag = dragGroupIdRef.current;
    if (groupDrag) {
      // A stack cannot land on one of its own tabs.
      if (groupIdOfKey(targetKey) === groupDrag) { setDropTarget(null); return; }
      event.preventDefault();
      event.stopPropagation();
      if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
      const side = beforeAfter(event, event.currentTarget);
      if (dropTarget?.key !== targetKey || dropTarget.side !== side) setDropTarget({ key: targetKey, side });
      return;
    }
    const sourceKey = dragKeyRef.current;
    if (!sourceKey || sourceKey === targetKey) {
      setDropTarget(null);
      return;
    }
    event.preventDefault();
    event.stopPropagation();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
    const side = tabDropSide(event, event.currentTarget, sourceKey, targetKey);
    if (dropTarget?.key !== targetKey || dropTarget.side !== side) {
      setDropTarget({ key: targetKey, side });
    }
  };
  const tabDragLeave = (event, targetKey) => {
    if (dropTarget?.key !== targetKey || event.currentTarget.contains(event.relatedTarget)) return;
    setDropTarget(null);
  };
  const dropTab = (event, targetKey) => {
    event.preventDefault();
    event.stopPropagation();
    droppedRef.current = true;
    const groupDrag = dragGroupIdRef.current;
    if (groupDrag) {
      const moved = groupIdOfKey(targetKey) !== groupDrag
        && actions.moveGroup(groupDrag, targetKey, { after: beforeAfter(event, event.currentTarget) === 'after' });
      clearDrag();
      if (moved) announceGroupMove(moved);
      return;
    }
    const key = event.dataTransfer?.getData(TERMINAL_TAB_DRAG_MIME) || dragKeyRef.current;
    // A tab dropped on itself is a no-op. tabDragOver already declines to accept
    // a self-hover, so a real browser never fires this drop; the guard keeps a
    // future unconditional-accept change from building a singleton group from
    // keys:[X, X].
    if (!key || key === targetKey) { clearDrag(); return; }
    const side = tabDropSide(event, event.currentTarget, key, targetKey);
    if (side === 'group') {
      // Centre drop: combine the two tabs. If the target already has a group,
      // the dragged tab joins it; otherwise a new group is born around both.
      const existing = groupIdOfKey(targetKey);
      if (existing) {
        const joined = actions.assignPaneToGroup(key, existing);
        const group = joined && current.groups.find((candidate) => candidate.id === existing);
        if (group) setReorderAnnouncement(`Grouped ${labelOfKey(key)} into ${group.name}.`);
      } else {
        const group = actions.createGroup({ keys: [targetKey, key] });
        if (group) {
          setReorderAnnouncement(
            `Grouped ${labelOfKey(key)} with ${labelOfKey(targetKey)} as ${group.name}.`,
          );
        }
      }
      clearDrag();
      return;
    }
    const moved = actions.reorderPane(key, targetKey, { after: side === 'after' });
    clearDrag();
    if (moved) announceReorder(moved);
  };
  // Dropping ON a stack (its pill or its padding) joins the stack at the end.
  // Dropping on a member TAB is handled by that tab and adopts its membership,
  // so the same gesture covers "join here" and "join at this exact position".
  const groupMembers = (groupID) =>
    current.segments.find((segment) => segment.type === 'group' && segment.group.id === groupID)?.panes || [];
  const groupDragOver = (event, groupID) => {
    const groupDrag = dragGroupIdRef.current;
    if (groupDrag) {
      // Dropping one stack on another places it beside that stack, never inside.
      if (groupDrag === groupID || event.defaultPrevented) return;
      event.preventDefault();
      if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
      if (groupDropTarget !== groupID) setGroupDropTarget(groupID);
      return;
    }
    const sourceKey = dragKeyRef.current;
    if (!sourceKey || event.defaultPrevented) return;
    event.preventDefault();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
    if (groupDropTarget !== groupID) setGroupDropTarget(groupID);
  };
  const groupDragLeave = (event, groupID) => {
    if (groupDropTarget !== groupID || event.currentTarget.contains(event.relatedTarget)) return;
    setGroupDropTarget(null);
  };
  const dropOnGroup = (event, groupID) => {
    if (event.defaultPrevented) return;
    event.preventDefault();
    event.stopPropagation();
    droppedRef.current = true;
    const groupDrag = dragGroupIdRef.current;
    if (groupDrag) {
      const anchor = groupMembers(groupID)[0]?.key;
      const moved = groupDrag !== groupID && anchor
        && actions.moveGroup(groupDrag, anchor, { after: beforeAfter(event, event.currentTarget) === 'after' });
      clearDrag();
      if (moved) announceGroupMove(moved);
      return;
    }
    const key = event.dataTransfer?.getData(TERMINAL_TAB_DRAG_MIME) || dragKeyRef.current;
    const pane = key && current.panes.find((candidate) => candidate.key === key);
    const joined = key && actions.assignPaneToGroup(key, groupID);
    clearDrag();
    const group = joined && current.groups.find((candidate) => candidate.id === groupID);
    if (group && pane) setReorderAnnouncement(`Moved ${pane.label} into group ${group.name}.`);
  };
  // Inter-stack drop gaps. A drop adopts the target's membership, and at a
  // group boundary the only thing to drop onto is a member — which would join.
  // So the position just before a leading stack, or between two adjacent
  // stacks, is unreachable by drag (the keyboard hop covers it). These thin
  // gaps at the group boundaries close that hole: a drop parks the tab there,
  // ungrouped, beside the stack. `gap` is `${groupID}:before|after`.
  const gapDragOver = (event, gap) => {
    if ((!dragKeyRef.current && !dragGroupIdRef.current) || event.defaultPrevented) return;
    event.preventDefault();
    event.stopPropagation();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
    if (stripGap !== gap) setStripGap(gap);
  };
  const gapDragLeave = (event, gap) => {
    if (stripGap !== gap || event.currentTarget.contains(event.relatedTarget)) return;
    setStripGap(null);
  };
  const dropOnGap = (event, groupID, after) => {
    event.preventDefault();
    event.stopPropagation();
    droppedRef.current = true;
    const groupDrag = dragGroupIdRef.current;
    if (groupDrag) {
      // A group boundary gap places the dragged stack just before/after the
      // gap's stack.
      const members = groupMembers(groupID);
      const anchor = after ? members.at(-1)?.key : members[0]?.key;
      const moved = groupDrag !== groupID && anchor && actions.moveGroup(groupDrag, anchor, { after });
      clearDrag();
      if (moved) announceGroupMove(moved);
      return;
    }
    const key = event.dataTransfer?.getData(TERMINAL_TAB_DRAG_MIME) || dragKeyRef.current;
    const moved = key && actions.movePaneOutsideGroup(key, groupID, { after });
    clearDrag();
    if (moved) announceReorder(moved);
  };
  // The strip's empty right margin is one large drop zone. Releasing anywhere
  // past the last tab or group lands the drag at the very end — the whole
  // far-right area counts as the right edge of the furthest-right segment, not
  // just that segment's own right quarter. The zone grows to fill the free
  // space during a drag (see .mux-strip-gap.grow) and resolves to the last
  // segment here, so it covers a trailing plain tab and a trailing group alike.
  const lastSegment = () => current.segments.at(-1);
  // Dropping the furthest-right segment onto the end is a no-op — a tab already
  // last, or the group being dragged. Skip arming so the marker never promises
  // a move that will not happen.
  const endZoneActive = () => {
    const segment = lastSegment();
    if (!segment) return false;
    const groupDrag = dragGroupIdRef.current;
    if (groupDrag) return !(segment.type === 'group' && segment.group.id === groupDrag);
    const key = dragKeyRef.current;
    return Boolean(key) && !(segment.type === 'pane' && segment.pane.key === key);
  };
  const endZoneDragOver = (event) => {
    if ((!dragKeyRef.current && !dragGroupIdRef.current) || event.defaultPrevented) return;
    if (!endZoneActive()) { if (stripGap === 'end') setStripGap(null); return; }
    event.preventDefault();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
    if (stripGap !== 'end') setStripGap('end');
    // Deliberately NOT stopPropagation, unlike the boundary gaps: letting the
    // dragover reach the document-level drag-out listener lets it recompute the
    // detach hint, which clears a stale "release to detach" affordance if the
    // pointer wandered out past the strip and then back onto this end-zone. The
    // point is inside the strip, so that recompute can only ever disarm detach,
    // and preventDefault above keeps it from touching the drop effect.
  };
  const endZoneDragLeave = (event) => {
    if (stripGap !== 'end' || event.currentTarget.contains(event.relatedTarget)) return;
    setStripGap(null);
  };
  const dropOnEnd = (event) => {
    event.preventDefault();
    event.stopPropagation();
    droppedRef.current = true;
    const segment = lastSegment();
    const groupDrag = dragGroupIdRef.current;
    if (groupDrag) {
      const anchor = segment?.panes.at(-1)?.key;
      const moved = anchor && !(segment.type === 'group' && segment.group.id === groupDrag)
        && actions.moveGroup(groupDrag, anchor, { after: true });
      clearDrag();
      if (moved) announceGroupMove(moved);
      return;
    }
    const key = event.dataTransfer?.getData(TERMINAL_TAB_DRAG_MIME) || dragKeyRef.current;
    if (!key || !segment) { clearDrag(); return; }
    const moved = segment.type === 'group'
      ? actions.movePaneOutsideGroup(key, segment.group.id, { after: true })
      : actions.reorderPane(key, segment.pane.key, { after: true });
    clearDrag();
    if (moved) announceReorder(moved);
  };
  const commitGroupRename = (groupID, name) => {
    setRenamingGroup(null);
    actions.renameGroup(groupID, name);
  };
  const closeTabMenu = (restoreFocus = false) => {
    const trigger = tabMenu?.trigger;
    setTabMenu(null);
    if (restoreFocus) queueMicrotask(() => trigger?.focus());
  };
  const focusAfterTabMenuAction = () => setMenuFocusRequest((value) => value + 1);
  useLayoutEffect(() => {
    if (!menuFocusRequest) return;
    const activeTab = shellRef.current?.querySelector('.mux-tab[aria-selected="true"]');
    if (activeTab) activeTab.focus();
    // No tab left: the Terminals tab stays selected (it never auto-hides), so
    // its navigation entry is the natural place for focus to land — unless
    // the last-close effect below moves on to Groups, which refocuses there.
    else document.querySelector('nav [data-tab="terminals"]')?.focus();
  }, [menuFocusRequest]);
  const focusAfterTabMenuDismiss = (reverse) => {
    const trigger = tabMenu?.trigger;
    queueMicrotask(() => {
      const paneControl = shellRef.current?.querySelector(
        '.mux-pane.active button:not([disabled]), .mux-pane.active input:not([disabled]), .mux-pane.active [tabindex]:not([tabindex="-1"])',
      );
      (reverse ? trigger : paneControl || trigger)?.focus();
    });
  };

  // A menu anchored to something that is gone — a closed tab, or a stack whose
  // last member closed — has nothing left to act on.
  useEffect(() => {
    if (!tabMenu) return;
    const alive = tabMenu.kind === 'group'
      ? current.segments.some((segment) => segment.type === 'group' && segment.group.id === tabMenu.id)
      : current.panes.some((pane) => pane.key === tabMenu.key);
    if (!alive) setTabMenu(null);
  }, [current.panes, current.segments, tabMenu]);

  useEffect(() => {
    if (renamingGroup && !current.groups.some((group) => group.id === renamingGroup)) setRenamingGroup(null);
  }, [current.groups, renamingGroup]);

  // Collapse and expand happen from the pill, the group menu, and implicitly
  // when a collapsed stack's member is activated — three paths, one place to
  // announce them. Diffing the collapsed state across renders covers all of
  // them without each caller having to remember to speak. Focus is often off
  // the pill (menu just closed) when it changes, so aria-expanded alone is not
  // enough for a screen reader.
  const collapsedRef = useRef(new Map());
  useEffect(() => {
    const previous = collapsedRef.current;
    const next = new Map();
    for (const group of current.groups) {
      next.set(group.id, group.collapsed);
      if (previous.has(group.id) && previous.get(group.id) !== group.collapsed) {
        setReorderAnnouncement(`${group.name} group ${group.collapsed ? 'collapsed' : 'expanded'}.`);
      }
    }
    collapsedRef.current = next;
  }, [current.groups]);

  // Closing or opening a pane changes what is attached; re-read the listing
  // once tmux has seen the client come or go rather than waiting for a poll.
  useEffect(() => {
    if (solo) return undefined;
    const timer = setTimeout(refreshBackgroundTerminals, 1200);
    return () => clearTimeout(timer);
  }, [current.panes.length, solo]);

  // Preact flushes layout effects child-first, so the active pane's own
  // activation attempt runs while this tab is still display:none, and a real
  // browser drops focus on an unrendered xterm. Refit and refocus the active
  // widget here, once the reveal above has actually made the pane visible.
  //
  // Only revealRequest may trigger this. It is a monotonic counter bumped by
  // requestReveal(), so a plain (foreground) open advances it while a
  // Ctrl/Cmd-click background open deliberately does not. hasPanes must stay
  // out of the dependency list: it flips false->true whenever any pane appears,
  // including a background open, and the `=== 0` guard cannot catch that once
  // an earlier reveal has left the counter >= 1. Keying on revealRequest alone
  // means a background open never clicks the Terminals tab.
  useLayoutEffect(() => {
    if (solo || !hasPanes || current.revealRequest === 0) return;
    const terminalSection = document.getElementById('tab-terminals');
    const needsPostRevealFocus = !terminalSection?.classList.contains('active');
    document.querySelector('nav [data-tab="terminals"]')?.click();
    // Ordinary pane switches already run setActive(true) while this section is
    // visible. Only repeat fit/focus when the request had to reveal the section.
    if (!needsPostRevealFocus || !terminalSection?.classList.contains('active')) return;
    const active = current.panes.find((pane) => pane.key === current.activeKey);
    const widget = active && actions.widgetFor(active.id);
    if (!widget) return;
    widget.fit();
    widget.focus();
  }, [current.revealRequest, solo]);

  // A board pop-out goes away by script after Back to a tab, which must not prompt.
  const confirmsReload = solo ? current.panes.some((pane) => !pane.seed.board) : hasPanes;
  useEffect(() => {
    if (!confirmsReload) return undefined;
    let armed = true;
    const confirmUnload = (event) => {
      event.preventDefault();
      event.returnValue = true;
    };
    const disarm = () => {
      if (!armed) return;
      armed = false;
      window.removeEventListener('beforeunload', confirmUnload);
    };
    window.addEventListener('beforeunload', confirmUnload);
    window.addEventListener('tclaude:auth-expired', disarm);
    window.addEventListener(TERMINAL_LEAVING_EVENT, disarm);
    return () => {
      window.removeEventListener('tclaude:auth-expired', disarm);
      window.removeEventListener(TERMINAL_LEAVING_EVENT, disarm);
      window.removeEventListener('beforeunload', confirmUnload);
    };
  }, [confirmsReload]);

  // The last open tab closing leaves an empty section behind; move on to
  // Groups unless config says to stay (terminal-close-nav.js). Only the
  // has-panes -> no-panes edge counts, so loading with no terminals or
  // unticking a node in a fused scope never switches tabs. It waits a task and
  // re-checks: a pop-out reattach closes and reopens the same pane, and must
  // not bounce through Groups on the way.
  const hadPanesRef = useRef(hasPanes);
  useEffect(() => {
    const had = hadPanesRef.current;
    hadPanesRef.current = hasPanes;
    if (solo || !had || hasPanes) return undefined;
    const timer = setTimeout(() => {
      // A board, a shell or an agent that moved to the overlay leaves the operator working here.
      if (!state.panes.value.length) leaveTerminalsAfterLastClose({ stayHere: boardOverlays.peek().length > 0 || terminalWindowsShowing() || chatWindowShowing() || actions.floatingAgent() });
    }, 0);
    return () => clearTimeout(timer);
  }, [hasPanes, solo]);

  useEffect(() => {
    if (!hasPanes && manageTitle) document.title = 'tclaude terminals';
  }, [hasPanes, manageTitle]);

  useEffect(() => {
    if (!composeMessageAvailable) return undefined;
    const onComposeShortcut = (event) => {
      // The companion window answers for its own terminal.
      if (event.target?.closest?.('.companion-window')) return;
      const pane = current.panes.find((candidate) => candidate.key === current.activeKey);
      const dialogKind = composeMessageDialogKind();
      const action = terminalComposeShortcutAction(event, {
        // The standalone page is itself the active terminal surface. Keep this
        // document-level capture path in both modes so header/tab controls get
        // the shortcut too, before xterm or browser Cmd+M handling sees it.
        tabActive: solo || document.getElementById('tab-terminals')?.classList.contains('active'),
        operatorModalOpen: dialogKind === 'operator-message',
        blockingOverlayOpen: hasShownOverlay(),
        eligiblePane: Boolean(composeAgent(pane)),
      });
      if (action === 'ignore') return;
      event.preventDefault();
      event.stopPropagation();
      if (action === 'open') onComposeMessage(composeTarget(pane, actions));
    };
    // Capture before xterm or terminal-tab chrome sees Ctrl/Cmd+M. This keeps
    // the shortcut available from the pane, tab strip, and header alike.
    document.addEventListener('keydown', onComposeShortcut, true);
    return () => document.removeEventListener('keydown', onComposeShortcut, true);
  }, [
    actions, composeMessageAvailable, composeMessageDialogKind, current.activeKey,
    current.panes, onComposeMessage, solo,
  ]);

  return html`
    <div ref=${shellRef} class="terminal-shell-root">
      ${!solo ? html`
        <span id="terminal-tab-reorder-help" class="mux-tab-a11y">
          Drag tabs to reorder them, or press Alt+Shift+Left Arrow or Alt+Shift+Right Arrow on a focused tab.
          Drag a tab off the strip to float it in an overlay window, or a separate window if it cannot float, or use the tab's context menu to open it in either.
          Tabs can be collected into named groups: drop one tab onto the middle of another to group them, use a tab's context menu to start
          or join a group, drop a tab on a group to join it, and press Alt+Shift+Left Arrow or Alt+Shift+Right Arrow at the edge of a group
          to move the tab out of it.
          A group's pill collapses and expands the group; double-click it or press F2 to rename, and it carries its own context menu for
          renaming, ungrouping, and closing its tabs.
        </span>
        <span id="terminal-group-pill-help" class="mux-tab-a11y">
          Enter or Space collapses or expands this group. Press F2 to rename it, or Shift+F10 for its actions menu.
          Drag this pill, or press Alt+Shift+Left Arrow or Alt+Shift+Right Arrow, to move the whole group among the other tabs.
        </span>
        <span class="mux-tab-a11y" role="status" aria-live="polite" aria-atomic="true">${reorderAnnouncement}</span>
        <div class="mux-tabbar"><${GroupsInteractionProvider}><div ref=${tabsRef} class=${`mux-tabs${detachArmed ? ' drag-out-armed' : ''}`} role="tablist" aria-label="Open terminals">
          ${current.segments.map((segment) => {
            const renderTab = (pane) => html`
              <${PaneTab}
                key=${pane.key}
                pane=${pane}
                peerNotifications=${peerNotifications}
                active=${current.activeKey === pane.key}
                menuOpen=${tabMenu?.kind !== 'group' && tabMenu?.key === pane.key}
                groupId=${segment.type === 'group' ? segment.group.id : null}
                actions=${scopedActions}
                openMenu=${setTabMenu}
                dragging=${dragKey === pane.key}
                dropSide=${dropTarget?.key === pane.key ? dropTarget.side : ''}
                onDragStart=${startTabDrag}
                onDragEnd=${endTabDrag}
                onDragOver=${tabDragOver}
                onDragLeave=${tabDragLeave}
                onDrop=${dropTab}
                onReordered=${announceAndRefocus}
                snapshot=${snapshot}
                scope=${scopeOf(pane)}
              />
            `;
            if (segment.type !== 'group') return renderTab(segment.pane);
            // A stack none of whose panes are on a ticked node is out of sight too.
            if (segment.panes.every((pane) => scopeOf(pane).hidden)) return null;
            const gid = segment.group.id;
            const dragging = Boolean(dragKey) || Boolean(dragGroupId);
            // A leading gap is only reachable — and only needed — when this
            // stack is not the very first segment, or when it is: the position
            // before a leading stack is exactly the unreachable one. The
            // position after the last segment is covered by the strip's trailing
            // end-zone (rendered after this map), so no per-group trailing gap is
            // needed. Redundant gaps elsewhere are harmless but omitted to keep
            // the strip quiet.
            const leadingGap = html`
              <${StripGap}
                key=${`${gid}:gap-before`}
                active=${dragging}
                armed=${stripGap === `${gid}:before`}
                onDragOver=${(event) => gapDragOver(event, `${gid}:before`)}
                onDragLeave=${(event) => gapDragLeave(event, `${gid}:before`)}
                onDrop=${(event) => dropOnGap(event, gid, false)}
              />`;
            return html`
              ${leadingGap}
              <${GroupStack}
                key=${segment.key}
                group=${segment.group}
                panes=${segment.panes}
                peerNotifications=${peerNotifications}
                activeKey=${current.activeKey}
                actions=${actions}
                renaming=${renamingGroup === gid}
                onRenameStart=${setRenamingGroup}
                onRenameCommit=${commitGroupRename}
                onRenameCancel=${() => setRenamingGroup(null)}
                openMenu=${setTabMenu}
                menuOpen=${tabMenu?.kind === 'group' && tabMenu.id === gid}
                dropActive=${groupDropTarget === gid}
                dragging=${dragGroupId === gid}
                onPillDragOver=${groupDragOver}
                onPillDragLeave=${groupDragLeave}
                onPillDrop=${dropOnGroup}
                onPillDragStart=${startGroupDrag}
                onPillDragEnd=${endGroupDrag}
                onKeyboardMove=${moveGroupKeyboard}
                renderTab=${renderTab}
                snapshot=${snapshot}
              />
            `;
          })}
          ${hasPanes ? html`<${TerminalCostToggle} />` : null}
          ${hasPanes ? html`
            <${StripGap}
              key="strip-end"
              grow=${true}
              active=${Boolean(dragKey) || Boolean(dragGroupId)}
              armed=${stripGap === 'end'}
              onDragOver=${endZoneDragOver}
              onDragLeave=${endZoneDragLeave}
              onDrop=${dropOnEnd}
            />` : null}
        </div><//>
        <${BoardMenu} state=${state} snapshot=${snapshot} />
        ${hasBackground ? html`<div class="mux-bg-slot"><${BackgroundTerminalsChip} rows=${backgroundRows} snapshot=${snapshot} /></div>` : null}
        </div>
        ${detachArmed ? html`<div class="mux-drag-out-hint">Release anywhere — even outside the browser — to detach this terminal into its own window</div>` : null}
      ` : null}
      ${snapshot ? html`<${BoardDeleteHost} snapshot=${snapshot} fetchImpl=${fetchFromPage} notify=${shellToast} />` : null}
      ${tabMenu ? html`<${PaneContextMenu} menu=${tabMenu} actions=${actions} groups=${current.groups}
        closeMenu=${closeTabMenu} focusAfterAction=${focusAfterTabMenuAction}
        focusAfterDismiss=${focusAfterTabMenuDismiss} onRenameGroup=${setRenamingGroup} />` : null}
      ${!solo && !hasPanes ? html`
        <div class="mux-bg-empty">
          <div class="mux-empty-title">No terminals open</div>
          ${hasBackground
            ? html`<div>${backgroundRows.length} tclaude ${backgroundRows.length === 1 ? 'session is' : 'sessions are'} still running without a tab open here. Hover the counter at the top right to attach or review them.</div>`
            : html`<div>Open one from an agent's ⚙ <code>terminals &amp; windows…</code> menu and pick a browser option.</div>`}
          ${snapshot ? html`<${BoardsGate} snapshot=${snapshot}><div>Or open a board from the ＋ above.</div><//>` : null}
        </div>
      ` : null}
      ${hasPanes || !empty ? html`
        <div ref=${panesRef} class="mux-panes">
          ${current.panes.map((pane) => (pane.seed.board ? html`
            <${BoardPane}
              key=${pane.key}
              pane=${pane}
              active=${current.activeKey === pane.key}
              solo=${solo}
              manageTitle=${manageTitle}
              actions=${actions}
              snapshot=${snapshot}
              scope=${scopeOf(pane)}
            />
          ` : html`
            <${TerminalPane}
              key=${pane.key}
              pane=${pane}
              active=${current.activeKey === pane.key}
              activationToken=${current.activeKey === pane.key ? current.revealRequest : 0}
              solo=${solo}
              manageTitle=${manageTitle}
              actions=${actions}
              widgetFactory=${widgetFactory}
              onComposeMessage=${composeMessageAvailable ? onComposeMessage : null}
              snapshot=${manageTitle ? snapshot : null}
              processSnapshot=${snapshot}
              scope=${scopeOf(pane)}
            />
          `))}
        </div>
      ` : null}
      ${hasPanes && !shownPanes.length ? html`
        <div class="mux-scope-empty">None of your open terminals is on the ticked nodes. Tick more nodes in <b>nodes ▾</b> to see them.</div>
      ` : null}
      ${empty && !hasPanes ? html`
        <div id="mux-empty">
          <div class="mux-empty-title">No terminals open</div>
          <div>Open one from an agent's ⚙ <code>terminals &amp; windows…</code> menu item and pick a browser option — it opens in the dashboard's <code>Terminals</code> tab.</div>
          <div>Each terminal there has an <code>Open in separate window</code> button that moves it to a browser window of its own.</div>
        </div>
      ` : null}
    </div>
  `;
}

function hasRemotePane(state) {
  return state.panes.peek().some((pane) => pane.seed?.remote?.peer);
}

// receiveSeparateWindow takes a terminal back from its separate window: to the
// overlay if it asked and can float, to the companion tab waiting on it, to where
// it already is, or else to a tab here.
export async function receiveSeparateWindow(seed, { actions, snapshot = null, runtimeLoader = null }) {
  const overlay = !!seed.overlay && paneFloats({ seed }, snapshot?.value);
  if (!overlay && agentSessionSeed(seed) && reclaimInCompanion(seed.agent)) return true;
  // It went somewhere else while this window was open, and it stays there.
  if (!overlay && (seed.board ? revealBoardWindow(seed.board) : revealHeldSeed(seed))) return true;
  if (runtimeLoader && !seed.board) await runtimeLoader();
  return actions.receiveHandoffPane(seed, { overlay });
}

function TerminalBadge({ state }) {
  const count = state.view.value.panes.filter((pane) => !pane.seed.board).length;
  const background = visibleBackgroundTerminals(backgroundTerminals.value, state.panes.value).length;
  const title = background
    ? `${count} open, ${background} running in the background without a terminal`
    : `${count} open`;
  return html`<span id="terminals-badge" class="tab-badge count" title=${title}
    hidden=${count === 0 && background === 0}>${count}${background ? ` +${background}` : ''}</span>`;
}


export function mountTerminalShellIsland({
  host,
  badgeHost,
  state,
  actions,
  registerCleanup,
  widgetFactory = mountTerminalWidget,
  onComposeMessage = null,
  composeMessageDialogKind = () => '',
  snapshot = null,
  scope = null,
}) {
  // A custom widget factory (tests or another embedding) owns its own runtime.
  // The production xterm adapter asks the facade to load the classic core
  // before it lets the first pane enter Preact state.
  const runtimeLoader = widgetFactory === mountTerminalWidget ? loadXtermRuntime : null;
  const unregisterController = registerTerminalShellController(actions, runtimeLoader);
  const unbindHandoff = bindTerminalHandoffReceiver({
    openSeed: (seed) => receiveSeparateWindow(seed, { actions, snapshot, runtimeLoader }),
  });
  // Path routing for the tab (/terminals/<agent-id>). Bound here rather than in
  // TerminalTabs because the standalone pop-out shares that component and owns
  // no dashboard URL — only the mounted island is a routable location.
  // openWebWindowPane (not actions.openPane) so a restored deep link goes
  // through the very same launcher as the "focus this agent" row action,
  // including its lazy xterm runtime load.
  const unbindNavRouting = bindTerminalNavRouting({
    state,
    snapshot,
    openAgentPane: (agent, label, harness) => openWebWindowPane(agent, label, { harness }),
    openBoardPane: (id) => openLinkedBoard(id),
  });
  // A board has no tab while features.collab is off. The router keeps /boards
  // URLs from opening one; this takes away what was open, or came by handoff.
  const stopCollabGate = snapshot ? snapshot.subscribe((value) => {
    if (!value || value.collab_enabled === true) return;
    state.removePanes(state.panes.peek().filter((pane) => pane.seed.board).map((pane) => pane.key));
  }) : () => {};
  render(html`<${TerminalTabs} state=${state} actions=${actions} widgetFactory=${widgetFactory}
    onComposeMessage=${onComposeMessage} composeMessageDialogKind=${composeMessageDialogKind}
    snapshot=${snapshot} scope=${scope} />`, host);
  render(html`<${TerminalBadge} state=${state} />`, badgeHost);
  // A fused view, or a peer's per-node view, also lists the shown peers'
  // unattached agents; a change of scope re-reads at once.
  const peers = () => (scope ? backgroundPeers(scope.view.peek(), remoteNodeID()) : []);
  const stopBackgroundPoll = startBackgroundTerminalsPoll({ peers });
  // The background popover's peer agents light up from the same catalog.
  const stopRemoteStatusPoll = startRemoteSessionStatusPoll({
    hasRemote: () => hasRemotePane(state) || hasPeerBackgroundAgent(backgroundTerminals.peek()),
  });
  let peerKey = null;
  const stopPeerWatch = scope ? effect(() => {
    const next = backgroundPeers(scope.view.value, remoteNodeID()).map((peer) => peer.id).join(',');
    if (peerKey !== null && next !== peerKey) refreshBackgroundTerminals();
    peerKey = next;
  }) : () => {};
  registerCleanup(() => {
    stopRemoteStatusPoll();
    stopPeerWatch();
    stopBackgroundPoll();
    stopCollabGate();
    unbindNavRouting();
    unbindHandoff();
    unregisterController();
    render(null, badgeHost);
    render(null, host);
    actions.dispose();
  });
}

export function mountStandaloneTerminalShell({
  host,
  state,
  actions,
  widgetFactory = mountTerminalWidget,
  onComposeMessage = null,
  composeMessageReady = null,
  composeMessageDialogKind = () => '',
  snapshot = null,
}) {
  let disposed = false;
  const stopRemoteStatusPoll = startRemoteSessionStatusPoll({ hasRemote: () => hasRemotePane(state) });
  render(html`
    <${TerminalTabs}
      state=${state}
      actions=${actions}
      widgetFactory=${widgetFactory}
      onComposeMessage=${onComposeMessage}
      composeMessageReady=${composeMessageReady}
      composeMessageDialogKind=${composeMessageDialogKind}
      snapshot=${snapshot}
      solo=${true}
      manageTitle=${true}
      empty=${true}
    />
  `, host);
  return () => {
    if (disposed) return;
    disposed = true;
    stopRemoteStatusPoll();
    render(null, host);
    actions.dispose();
  };
}

export {
  GroupStack, OpaqueTerminalHost, PaneContextMenu, PaneTab, TabAttention, TerminalBadge,
  TerminalPane, TerminalTabs,
};
