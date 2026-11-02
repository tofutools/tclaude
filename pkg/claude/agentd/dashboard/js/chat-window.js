// chat-window.js -- one agent in an overlay tab (tcl-ppvf8g): its real terminal,
// floating tools, composer, and everything waiting on the operator from it. The
// Steward is one of these. A session has one web view, so an agent has one tab.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { actionsFor, approvalsFor, companionAgents, companionStatus, decisionsFor, elevatedAccess, pendingApprovals } from './companion-model.js';
import { ApprovalCard, ElevatedBand, openAccessPanel } from './companion-approval.js';
import { AgentCog } from './companion-cog.js';
import { minimizeOverlayWindow, overlayCloseHeard } from './overlay-instances.js';
import { windowControlsLook } from './overlay-window-controls.js';
import { OverlayWindow } from './overlay-window.js';
import { PaneDictation } from './terminal-dictation.js';
import { ActionCards } from './message-action.js';
import { ShellHere } from './terminal-here.js';
import { TerminalZoom, readTerminalFont, useTerminalFont } from './terminal-zoom.js';
import { withShortcut } from './keybindings.js';
import { Button } from './ui-components.js';

const html = htm.bind(h);

// Minimizing hides a chat and only releases its terminal later.
const WARM_MS = 5 * 60 * 1000;

const RECONNECTS = ['disconnected', 'error', 'authentication required'];

// What outlives a tab's body, which goes when its window shows another tab: unsent
// text, which of it was dictated, and whether the view goes with a message.
const kept = { drafts: new Map(), dictated: new Set(), viewOff: new Set() };

// chatHasDraft says whether a tab holds unsent text.
export const chatHasDraft = (tabId) => !!kept.drafts.get(tabId)?.trim();

export function ChatWindow({
  instance, overlay, focusRef, strip, close, snapshot, prefs, win, fetchImpl, notify, widgetFactory, loadRuntime, closeTerminalPanes, onSeparateWindow = null, onFloat = null, parked = () => false, onReclaim = null, onMic = null, minimize = null, tabCount = 1, onStewardDown, stewardRoster = [], openPermissions = null, openSudo = null, openStewardHandover = null, openMorph = null, showAgent = null, Terminal, DecisionCard,
  statusOf = companionStatus, starting = () => false, offlineActions = () => null, voice = null, onCompose = null, onTerminals = null, dictationImpl = null, annotate, viewTab,
}) {
  const agentId = instance.agent;
  const agents = companionAgents(snapshot.value);
  const agent = agents.get(agentId) || null;
  const name = agent?.name || instance.label;
  const open = !instance.minimized;
  const [focusToken, setFocusToken] = useState(1);
  focusRef.current = () => setFocusToken((n) => n + 1);
  const [status, setStatus] = useState('');
  const [reconnectable, setCanReconnect] = useState(false);
  const canReconnect = reconnectable || RECONNECTS.includes(status);
  const [generation, setGeneration] = useState(0);
  const widgetRef = useRef(null);
  const [font, onFontChange] = useTerminalFont('companion', agentId, prefs);
  useEffect(() => { if (instance.focusAsked) setFocusToken((n) => n + 1); }, [instance.focusAsked]);
  const away = parked(agentId);
  useEffect(() => {
    if (open && !away) closeTerminalPanes([agentId, agent?.convId].filter(Boolean));
  }, [open, agentId, away]);
  // Only the terminal already attached stays hidden: one mounted for a window
  // nobody can see would take the attach from whatever view holds it.
  const [warm, setWarm] = useState(false);
  useEffect(() => {
    if (open || !agent?.online) setWarm(open && !!agent?.online);
    if (open || !warm) return undefined;
    const timer = setTimeout(() => setWarm(false), WARM_MS);
    return () => clearTimeout(timer);
  }, [open, warm, agent?.online]);
  // Keys must not reach a terminal nobody can see, and one that lost its attach
  // while hidden has nothing to show.
  useEffect(() => {
    if (open) {
      if (canReconnect) setGeneration((n) => n + 1);
      return;
    }
    const focused = win?.document?.activeElement;
    if (overlay.windowRef.current?.contains(focused)) focused.blur?.();
  }, [open]);

  const look = windowControlsLook(snapshot.value?.window_controls);
  const showRoster = !!(openStewardHandover && agent && (agent.steward || stewardRoster.some((entry) => entry.agent_id === agentId)));
  const standby = showRoster ? stewardRoster.filter((entry) => entry.seat === 'standby').length : 0;
  // A running Steward isn't closed in passing: its ✕ asks what ⏻ asks.
  const closeShutsDown = !!(agent?.steward && agent.online);
  const askShutDown = useRef(null);
  const rosterRef = useRef(null);
  useEffect(() => {
    if (!instance.closeAsked) return;
    overlayCloseHeard(instance.id);
    askShutDown.current?.();
  }, [instance.closeAsked]);
  const stewardDown = (down, how) => onStewardDown(down, how, instance.id);
  const onMinimize = minimize || (() => minimizeOverlayWindow(instance.id));
  const approvals = approvalsFor(snapshot.value, agent);
  const decisions = decisionsFor(snapshot.value, agent);
  const actionMessages = actionsFor(snapshot.value, agent);
  const elevated = elevatedAccess(snapshot.value, agent);
  const toApprove = pendingApprovals(snapshot.value);
  const needsYou = agent?.online && statusOf(agent) === 'needs' && !approvals.length && !decisions.length;
  const voiceOn = !!agent && !!voice?.on(agentId);
  const pane = { key: instance.id, id: instance.id, label: name, seed: { agent: agentId, harness: agent?.harness || '' } };
  const actions = { widgetFor: () => widgetRef.current, activatePane: () => setFocusToken((n) => n + 1) };
  const refocus = () => setFocusToken((n) => n + 1);
  const title = html`${strip}
    <div class="companion-controls">
      ${elevated && html`<button data-ui-exempt type="button" class="companion-state-sudo" aria-label=${`${name} has elevated access`}
        title="Elevated access. The band in the window can end it." disabled=${!(agent.steward && agent.online && openSudo)}
        onClick=${() => openSudo(agentId, name)}>🛡 sudo</button>`}
      ${standby > 0 && html`<button data-ui-exempt type="button" class="companion-state-roster" aria-label=${`${standby} on the Steward roster`}
        title="On the Steward roster" onClick=${() => rosterRef.current?.()}>✦ ${standby}</button>`}
      ${toApprove.length > 0 && html`<button data-ui-exempt type="button" class="companion-access"
        aria-label=${`${toApprove.length} permission ${toApprove.length === 1 ? 'request' : 'requests'} waiting. Open them`}
        title="Agents are asking for permission"
        onClick=${(event) => openAccessPanel({ launcher: event.currentTarget, documentRef: win?.document })}>🔑 ${toApprove.length}</button>`}
      ${agent && voice?.supported && html`<button type="button" data-ui-exempt class=${`companion-voice${voiceOn ? ' on' : ''}`}
        aria-pressed=${voiceOn ? 'true' : 'false'} aria-label=${`Speak ${name}'s replies aloud`}
        title=${voiceOn ? 'Voice on: replies are spoken when a turn ends. Typing stops speech.' : 'Speak replies aloud'}
        onClick=${() => voice.toggle(agent)}>${voiceOn ? '🔊' : '🔈'}</button>`}
      ${agent?.online && onCompose && html`<button data-ui-exempt type="button" class="companion-compose" aria-label=${`Message ${name}`} title=${withShortcut('Send a queued message', 'compose')} onClick=${() => onCompose(agent, refocus)}>✉</button>`}
      <${AgentCog} agent=${agent} parked=${away} roster=${stewardRoster} canShutDown=${closeShutsDown} showRoster=${showRoster}
        onCollab=${agent && onTerminals && !away ? () => onTerminals(instance, agent) : null}
        onFloat=${onFloat && (tabCount > 1 || away) ? () => onFloat(agent) : null} onDetach=${onSeparateWindow ? () => onSeparateWindow(agent, overlay.windowRef.current?.getBoundingClientRect?.()) : null} onMorph=${agent && openMorph ? () => openMorph(agent) : null}
        openPermissions=${openPermissions} openSudo=${openSudo} openHandover=${openStewardHandover} agents=${agents} showAgent=${showAgent}
        fetchImpl=${fetchImpl} notify=${notify} onDown=${stewardDown} askShutDownRef=${askShutDown} rosterRef=${rosterRef} />
    </div>
    ${look === 'classic' && html`<button data-ui-exempt type="button" class="board-overlay-close" title="Minimize" aria-label=${`Minimize ${name}`} onClick=${onMinimize}>–</button>`}
    ${look === 'classic' && html`<button data-ui-exempt type="button" class="board-overlay-close" title=${close.title} aria-label=${`Close ${name}`} onClick=${close.onClose}>×</button>`}`;
  const shown = agent?.online && (open || warm);
  return html`<${OverlayWindow} id=${instance.windowId} window=${overlay} open=${open} mounted=${open || (warm && !!agent?.online)}
    className=${`companion-window chat-window${open ? '' : ' companion-window-warm'}`} frameClass="companion-resize-frame"
    titlebarClass=${`board-overlay-titlebar overlay-look-${look}`} label=${`Chat with ${name}`} title=${title}
    controls=${{ look, closeLabel: `Close ${name}`, closeTitle: close.title, onClose: close.onClose, onMinimize }}>
    <div class=${`companion-body${approvals.length + decisions.length + actionMessages.length ? ' has-cards' : ''}`} id=${instance.id} data-chat-agent=${agentId}>
      ${approvals.map((request) => html`<${ApprovalCard} key=${request.id} request=${request} fetchImpl=${fetchImpl} notify=${notify} />`)}
      ${decisions.map((decision) => html`<${DecisionCard} key=${decision.id} decision=${decision} fetchImpl=${fetchImpl} notify=${notify} />`)}
      <${ActionCards} messages=${actionMessages} fetchImpl=${fetchImpl} notify=${notify} />
      ${elevated && html`<${ElevatedBand} agent=${agent} access=${elevated} fetchImpl=${fetchImpl} notify=${notify} />`}
      ${!agent?.online && (starting(agentId)
        ? html`<div class="companion-empty" role="status"><p>Starting the agent… its terminal opens here as soon as it is up.</p></div>`
        : html`<div class="companion-empty"><p><b>${name}</b> is ${agent ? 'offline' : 'gone'}.</p>${agent && offlineActions(agent)}</div>`)}
      ${shown && html`
        ${needsYou && html`<div class="companion-needs-bar" role="status">${name} is waiting for you${agent.detail ? `: ${agent.detail}` : ''}</div>`}
        <${PaneDictation} pane=${pane} active=${open} solo=${true} status=${status} canType=${true} target=${{ agent: agentId }} snapshot=${snapshot} actions=${actions} win=${win} notify=${notify} keep=${kept} onAnnotate=${annotate ? () => annotate(agentId) : null} onMic=${onMic ? (mic) => onMic(agentId, mic) : null} dictationImpl=${dictationImpl} viewTab=${viewTab} quietSpeech=${voice?.quiet} fetchImpl=${fetchImpl}>
          <${ShellHere} agent=${agentId} name=${name} snapshot=${snapshot.value} notify=${notify} />
          <${TerminalZoom} font=${font} widget=${() => widgetRef.current} />
        <//>
        ${away && html`<div class="companion-empty">
          <p><b>${name}</b> is open in its own window.</p>
          <${Button} size="sm" variant="primary" class="companion-primary" onClick=${() => onReclaim(agentId)}>Bring it back here<//>
        </div>`}
        ${!away && status !== 'connected' && html`<div class="companion-floating-anchor"><div class="companion-term-status" role="status">${canReconnect
          ? html`${status || 'disconnected'}. <${Button} size="sm" variant="link" onClick=${() => setGeneration((n) => n + 1)}>Reconnect<//>`
          : status}</div></div>`}
        ${!away && html`<${Terminal} key=${agentId} agent=${agent} generation=${generation} widgetFactory=${widgetFactory} loadRuntime=${loadRuntime}
          focusToken=${focusToken} widgetRef=${widgetRef} fontSize=${() => readTerminalFont('companion', agentId, prefs)} onFontChange=${onFontChange}
          onStatus=${setStatus} onReconnectChange=${setCanReconnect} />`}`}
    </div>
  <//>`;
}
