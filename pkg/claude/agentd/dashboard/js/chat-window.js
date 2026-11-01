// chat-window.js -- one agent's terminal in an overlay window of its own, a tab
// dragged out of the companion (tcl-ppvf8g). It is the same terminal as a tab:
// the agent's real session, with the same floating tools. A session has one web
// view, so the agent leaves the tab strip while its window is open.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { actionsFor, approvalsFor, companionAgents, companionStatus, companionStatusLabel, decisionsFor, defaultPlacement } from './companion-model.js';
import { ApprovalCard } from './companion-approval.js';
import { AgentCog } from './companion-cog.js';
import { parsePlacement, viewportOf } from './overlay-window-model.js';
import { closeOverlayWindow, minimizeOverlayWindow, placedOverlayWindow } from './overlay-instances.js';
import { windowControlsLook } from './overlay-window-controls.js';
import { OverlayWindow, readPref, useOverlayWindow, writePref } from './overlay-window.js';
import { PaneDictation } from './terminal-dictation.js';
import { ActionCards } from './message-action.js';
import { ShellHere } from './terminal-here.js';
import { TerminalZoom, readTerminalFont, useTerminalFont } from './terminal-zoom.js';

const html = htm.bind(h);

export const CHAT_PLACEMENT_PREF = 'tclaude.dash.chat-window.placement';

export function ChatWindow({
  instance, snapshot, prefs, win, fetchImpl, notify, widgetFactory, loadRuntime, closeTerminalPanes, onBackToTab, onStewardDown, stewardRoster = [], openPermissions = null, openSudo = null, openStewardHandover = null, showAgent = null, Terminal, DecisionCard, StatusDot,
}) {
  const agentId = instance.agent;
  const agent = companionAgents(snapshot.value).get(agentId) || null;
  const name = agent?.name || instance.label;
  const open = !instance.minimized;
  const placementKey = `${CHAT_PLACEMENT_PREF}.${agentId}`;
  // Where it was dropped is its place from now on, floating, and every panel
  // size it had stays saved.
  useState(() => {
    if (!instance.at) return;
    const saved = parsePlacement(readPref(prefs, placementKey)) || defaultPlacement(viewportOf(win));
    writePref(prefs, placementKey, JSON.stringify({ ...saved, ...instance.at, snap: '' }));
  });
  useEffect(() => { if (instance.at) placedOverlayWindow(instance.id); }, []);
  const overlay = useOverlayWindow({
    id: instance.id, win, prefs, prefKey: placementKey, defaultPlacement, snaps: true, open,
    onRefocus: () => setFocusToken((n) => n + 1),
  });
  const [focusToken, setFocusToken] = useState(1);
  const [status, setStatus] = useState('');
  const [canReconnect, setCanReconnect] = useState(false);
  const [generation, setGeneration] = useState(0);
  const widgetRef = useRef(null);
  const [font, onFontChange] = useTerminalFont('companion', agentId, prefs);
  useEffect(() => { if (instance.focusAsked) setFocusToken((n) => n + 1); }, [instance.focusAsked]);
  useEffect(() => {
    if (open) closeTerminalPanes([agentId, agent?.convId].filter(Boolean));
  }, [open, agentId]);

  const look = windowControlsLook(snapshot.value?.window_controls);
  const showRoster = !!(openStewardHandover && agent && (agent.steward || stewardRoster.some((entry) => entry.agent_id === agentId)));
  // A running Steward isn't closed in passing: its ✕ asks what ⏻ asks.
  const closeShutsDown = !!(agent?.steward && agent.online);
  const closeTitle = closeShutsDown ? 'Close: shut it down, after asking' : 'Close (the agent keeps running)';
  const askShutDown = useRef(null);
  const close = () => {
    if (closeShutsDown) askShutDown.current?.();
    else closeOverlayWindow(instance.id);
  };
  const stewardDown = (down) => {
    closeOverlayWindow(instance.id);
    onStewardDown?.(down);
  };
  const label = agent ? companionStatusLabel(companionStatus(agent)) : 'not on this node any more';
  const approvals = approvalsFor(snapshot.value, agent);
  const decisions = decisionsFor(snapshot.value, agent);
  const actionMessages = actionsFor(snapshot.value, agent);
  const pane = { key: instance.id, id: instance.id, label: name, seed: { agent: agentId, harness: agent?.harness || '' } };
  const actions = { widgetFor: () => widgetRef.current, activatePane: () => setFocusToken((n) => n + 1) };
  const title = html`<span class="chat-window-title" title=${`${name}: ${label}`}><${StatusDot} status=${agent ? companionStatus(agent) : 'offline'} /> ${name}</span>
    <button type="button" class="board-overlay-action" title="Move this agent back to a tab of the companion" onClick=${() => onBackToTab(instance)}>Back to a tab</button>
    <div class="companion-controls"><${AgentCog} agent=${agent} roster=${stewardRoster} canShutDown=${closeShutsDown} showRoster=${showRoster}
      openPermissions=${openPermissions} openSudo=${openSudo} openHandover=${openStewardHandover} agents=${companionAgents(snapshot.value)} showAgent=${showAgent}
      fetchImpl=${fetchImpl} notify=${notify} onDown=${stewardDown} askShutDownRef=${askShutDown} /></div>
    ${look === 'classic' && html`<button type="button" class="board-overlay-close" title=${closeTitle} aria-label=${`Close ${name}`} onClick=${close}>×</button>`}`;
  return html`<${OverlayWindow} id=${instance.id} window=${overlay} open=${open} className="companion-window chat-window" frameClass="companion-resize-frame"
    titlebarClass=${`board-overlay-titlebar overlay-look-${look}`} label=${`Chat with ${name}`} title=${title}
    controls=${{ look, closeLabel: `Close ${name}`, closeTitle, onClose: close, onMinimize: () => minimizeOverlayWindow(instance.id) }}>
    <div class="companion-body" id=${instance.id}>
      ${approvals.map((request) => html`<${ApprovalCard} key=${request.id} request=${request} fetchImpl=${fetchImpl} notify=${notify} />`)}
      ${decisions.map((decision) => html`<${DecisionCard} key=${decision.id} decision=${decision} fetchImpl=${fetchImpl} notify=${notify} />`)}
      <${ActionCards} messages=${actionMessages} fetchImpl=${fetchImpl} notify=${notify} />
      ${!agent?.online && html`<div class="companion-empty"><p><b>${name}</b> is ${agent ? 'offline' : 'gone'}.</p>
        ${agent && html`<button type="button" class="companion-primary" onClick=${() => onBackToTab(instance)}>Back to a tab, to resume it</button>`}</div>`}
      ${agent?.online && open && html`
        <${PaneDictation} pane=${pane} active=${true} solo=${true} status=${status} canType=${true} target=${{ agent: agentId }} snapshot=${snapshot} actions=${actions} win=${win} notify=${notify}>
          <${ShellHere} agent=${agentId} name=${name} snapshot=${snapshot.value} notify=${notify} />
          <${TerminalZoom} font=${font} widget=${() => widgetRef.current} />
        <//>
        ${status !== 'connected' && html`<div class="companion-floating-anchor"><div class="companion-term-status" role="status">${canReconnect
          ? html`${status || 'disconnected'}. <button type="button" onClick=${() => setGeneration((n) => n + 1)}>Reconnect</button>`
          : status}</div></div>`}
        <${Terminal} key=${agentId} agent=${agent} generation=${generation} widgetFactory=${widgetFactory} loadRuntime=${loadRuntime}
          focusToken=${focusToken} widgetRef=${widgetRef} fontSize=${() => readTerminalFont('companion', agentId, prefs)} onFontChange=${onFontChange}
          onStatus=${setStatus} onReconnectChange=${setCanReconnect} />`}
    </div>
  <//>`;
}
