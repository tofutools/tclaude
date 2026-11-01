// companion-island.js -- the dashboard companion (tcl-6i0283): a floating,
// minimizable window over every dashboard view that hosts agents' native
// harness terminals. See companion-model.js for the concept.
//
//   chips   minimized: one pill per pinned agent, bottom-right, showing its
//           status; "needs you" (a permission prompt or a question) turns the
//           pill amber so a minimized agent never looks silently busy.
//   peek    hovering a chip previews the last lines of the agent's pane,
//           read-only (GET /api/companion/{agent}/peek): the keyboard stays
//           with the page until the operator clicks.
//   window  click or Ctrl+. grows the chip into a draggable, resizable window
//           (edge drags snap it into a side panel or bottom drawer) with one
//           tab per agent. The body is the agent's real terminal, so
//           streaming, prompts and Esc interrupts are the harness's own.
//
// A web terminal attach displaces any other web view of the same session, so
// opening an agent here first closes its Terminals-tab pane, and "open in
// Terminals" parks this tab instead of fighting over the session.

import { h, render } from 'preact';
import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  COMPANION_ACTIVE_PREF, COMPANION_CHIP_SPOT_PREF, COMPANION_CLOSED_PREF, COMPANION_DETACHED_PREF, COMPANION_CUE_OPEN_PREF, COMPANION_MESSAGE_MAX, COMPANION_OPEN_PREF, COMPANION_PARKED_PREF, COMPANION_PLACEMENT_PREF, COMPANION_TABS_PREF, COMPANION_WINDOW_SPOT_PREF, DEFAULT_CHIP_SPOT,
  agentBlockedOnOperator, companionAgents, companionMessageFits, elevatedAccess, companionStatus, companionStatusLabel, composeCompanionMessage,
  defaultPlacement, isAgentId, nextActive, parseChipSpot, parseTabs, peekAnchor, spotPlacement, stewardAgent, stewardToResume,
  parseClosedHelpers, syncHelperTabs, viewContext, withTab, withoutTab, actionsFor, approvalsFor, decisionsFor, decisionSpeech, pendingApprovals, pendingDecisions,
  companionAgentOf,
} from './companion-model.js';
import { useChipDrag } from './companion-chip-drag.js';
import { registerCompanionController } from './companion-facade.js';
import { elementScreenPoint } from './terminal-drag-out.js';
import { OverlayWindow, raiseOverlayWindow, readPref, reducedMotion, useOverlayWindow, writePref } from './overlay-window.js';
import { closeOverlayWindow, openOverlayWindow, overlayWindows } from './overlay-instances.js';
import { CHAT_WINDOW_KIND, chatWindowId } from './chat-window-model.js';
import { ChatWindow } from './chat-window.js';
import { TerminalWindow } from './terminal-window.js';
import { TERMINAL_WINDOW_KIND } from './terminal-window-model.js';
import { viewportOf } from './overlay-window-model.js';
import { CompanionViewChannel } from './companion-view-channel.js';
import { GUIDE_FIND_STEP_MS, GUIDE_FIND_TIMEOUT_MS, GUIDE_MARK_PATIENCE_MS, GUIDE_SETTLE_MS, GUIDE_SHOW_MS, CUE_OPEN_UNFOCUSED_DELAY_MS, GuideOverlay, claimCueLock, claimCueOpen, cueOpenClaimed, cueOpenDue, cuePath, findGuideTarget, newCues } from './companion-guide.js';
import { ApprovalCard, ElevatedBand, openAccessPanel, requestStewardSummon, resumeAgent } from './companion-approval.js';
import { relativeAge } from './pr-review-model.js';
import { AgentCog } from './companion-cog.js';
import { windowControlsLook } from './overlay-window-controls.js';
import { hasShownOverlay } from './overlay-stack.js';
import { isComposeMessageShortcut } from './terminal-interactions.js';
import { DraftField, FloatingTools, dictationSendPhrase, micTitle, postOperatorMessage, useAnnotation, useDictation } from './dictation-tools.js';
import { registerStewardDoor } from './voice-wake.js';
import { harnessTakesMailMidTurn, voiceContinuous, voiceMarksDictated, voicePauses, voiceSilenceSeconds } from './voice-config.js';
import { VOICE_PREF, createSpeaker, dictationOpen, createVoiceReader, parseVoiceAgents, speakableText, decisionArrivals, voiceTransitions } from './companion-voice.js';
import { terminalAttachWidgetOptions } from './terminal-attach-config.js';
import { ActionCards } from './message-action.js';
import { ShellHere } from './terminal-here.js';
import { TerminalZoom, readTerminalFont, useTerminalFont } from './terminal-zoom.js';
import { useDismissablePopover } from './hover-popover.js';

const html = htm.bind(h);

const PEEK_DELAY_MS = 220;
const PENDING_MS = 30000;
// How long a minimized window keeps its terminal attached, so reopening it
// shows content at once. After this it detaches and the next open reconnects.
const WARM_MS = 5 * 60 * 1000;

const PEEK_REFRESH_MS = 1500;
// How far a tab is pulled off the strip before it comes away as a window.
const TAB_TEAR_PX = 40;

function Avatar({ agent }) {
  const steward = agent?.steward;
  const mark = steward ? '✦' : agent?.scribe ? '✎' : (agent?.name || '?').slice(0, 1).toUpperCase();
  return html`<span class=${`companion-avatar${steward ? ' steward' : agent?.scribe ? ' scribe' : ''}`} aria-hidden="true">${mark}</span>`;
}

function StatusDot({ status }) {
  return html`<span class=${`companion-dot ${status}`} aria-hidden="true"></span>`;
}

// CompanionTerminal mounts the opaque xterm widget for one agent. It owns the
// widget's whole lifecycle: a new agent id or a reconnect generation disposes
// the old widget before attaching a fresh one.
function CompanionTerminal({ agent, wsPath = '', generation, widgetFactory, loadRuntime, focusToken, onStatus, onReconnectChange, onDisconnect, widgetRef, fontSize, onFontChange }) {
  const hostRef = useRef(null);
  useEffect(() => {
    let cancelled = false;
    let widget = null;
    onStatus('connecting…');
    onReconnectChange(false);
    Promise.resolve(loadRuntime ? loadRuntime() : true).then(() => {
      if (cancelled || !hostRef.current) return;
      widget = widgetFactory({
        ...terminalAttachWidgetOptions(),
        host: hostRef.current,
        wsPath: wsPath || `/api/open-window-ws/${encodeURIComponent(agent.agentId)}`,
        initialRetry: true,
        applicationClipboardShortcuts: agent.harness === 'copilot',
        onStatus: (status) => { if (!cancelled) onStatus(status); },
        onReconnectChange: (available) => { if (!cancelled) onReconnectChange(!!available); },
        ...(onDisconnect && { onDisconnect: (info) => { if (!cancelled) onDisconnect(info); } }),
        initialFontSize: fontSize(),
        onFontChange: (font) => { if (!cancelled) onFontChange(font); },
      });
      widgetRef.current = widget;
      widget.setActive(true);
      void widget.connect();
    }).catch((error) => {
      if (!cancelled) onStatus('error');
      console.error('companion terminal failed to start:', error);
    });
    return () => {
      cancelled = true;
      widgetRef.current = null;
      widget?.dispose();
    };
  }, [agent.agentId, generation]);
  useEffect(() => {
    if (focusToken) widgetRef.current?.focus?.();
  }, [focusToken]);
  return html`<div class="companion-term-host" ref=${hostRef} data-companion-terminal=${agent.agentId}></div>`;
}

function Peek({ agent, lines, anchor }) {
  if (!agent || !anchor) return null;
  const style = Object.entries(anchor).map(([side, px]) => `${side}:${Math.max(8, px)}px`).join(';');
  return html`<div class="companion-peek" role="tooltip" style=${style}>
    <div class="companion-peek-head"><${Avatar} agent=${agent} /><b>${agent.name}</b>
      <span class="companion-peek-meta">${companionStatusLabel(companionStatus(agent))} · click to open</span></div>
    <pre class="companion-peek-body">${lines === null ? 'reading…' : lines.length ? lines.join('\n') : agent.online ? '(empty screen)' : '(offline)'}</pre>
    <div class="companion-peek-foot">Read-only preview. The keyboard stays with the page until you click. Drag to move.</div>
  </div>`;
}

const RECLAIM_CLOSE_MS = 1000;
const DETACHED_CHECK_MS = 1000;

function without(set, value) {
  if (!set.has(value)) return set;
  const next = new Set(set);
  next.delete(value);
  return next;
}

// The agent of the chat window target is in, null outside one.
function chatAgentOf(target, snapshotValue) {
  const id = target?.closest?.('.chat-window')?.querySelector?.('[data-companion-terminal]')?.dataset?.companionTerminal;
  return id ? companionAgents(snapshotValue).get(id) || null : null;
}

// companionContinuous is continuous mode's settings for an agent's tab, null
// where it can't run: on a peer's agent, whose questions this node can't see,
// or a harness that isn't proven to take mail mid-turn.
function companionContinuous(snapshot, agent) {
  const mode = voiceContinuous(snapshot);
  if (!mode || !agent || agent.presence || !harnessTakesMailMidTurn(snapshot, agent.harness || 'claude')) return null;
  return { ...mode, held: () => (agentBlockedOnOperator(snapshot, agent) ? `${agent.name} is waiting on you, so continuous mode stopped. Your words stay in the draft.` : '') };
}

// How long the Steward's window has to come up after its phrase.
const STEWARD_WAKE_MS = 3000;

// How recently a key in the terminal counts as still typing when a cue lands.
const CUE_TYPING_WINDOW_MS = 3000;

// stewardReturnNote says how far back a Steward's conversation picks up.
function stewardReturnNote({ lastRan }) {
  const age = relativeAge(lastRan);
  return age ? `Same conversation. Last ran ${age}.` : 'Same conversation.';
}

// stewardFreshNote says what Start fresh leaves behind. Going on the roster takes
// what the role gave, not what the operator gave this agent separately.
function stewardFreshNote({ name, keeps = [] }) {
  const kept = keeps.length ? ` It keeps what you gave it separately: ${keeps.join(', ')}.` : '';
  return `A new Steward. ${name} goes on the roster.${kept}`;
}

function AddMenu({ agents, tabs, steward, stewardLabel, returning, onResumeSteward, onSummonSteward, onPin, onClose, busy, trigger }) {
  const [filter, setFilter] = useState('');
  const menuRef = useRef(null);
  const filterRef = useRef(null);
  useEffect(() => { filterRef.current?.focus(); }, []);
  useDismissablePopover([menuRef, trigger], true, (why) => {
    onClose();
    if (why !== 'outside') trigger.current?.focus();
  });
  const candidates = useMemo(() => {
    const needle = filter.trim().toLowerCase();
    return [...agents.values()]
      .filter((agent) => agent.online && agent.agentId !== steward?.agentId && !tabs.includes(agent.agentId))
      .filter((agent) => !needle || agent.name.toLowerCase().includes(needle))
      .sort((a, b) => (b.scribe - a.scribe) || a.name.localeCompare(b.name))
      .slice(0, 40);
  }, [agents, tabs, steward, filter]);
  const stewardPinned = steward && tabs.includes(steward.agentId);
  return html`<div ref=${menuRef} class="companion-menu" role="menu" aria-label="Add a companion">
    ${returning && html`<button type="button" role="menuitem" class="companion-menu-steward companion-menu-resume" disabled=${busy} onClick=${onResumeSteward}>
      <span class="companion-avatar steward" aria-hidden="true">✦</span>
      <span><b>Resume ${returning.name}</b><small>${stewardReturnNote(returning)}</small></span>
    </button>`}
    ${returning && html`<button type="button" role="menuitem" class="companion-menu-steward companion-menu-fresh" disabled=${busy} onClick=${onSummonSteward}>
      <span class="companion-avatar steward" aria-hidden="true">✦</span>
      <span><b>Start fresh</b><small>${stewardFreshNote(returning)}</small></span>
    </button>`}
    ${!stewardPinned && !returning && html`<button type="button" role="menuitem" class="companion-menu-steward" disabled=${busy} onClick=${onSummonSteward}>
      <span class="companion-avatar steward" aria-hidden="true">✦</span>
      <span><b>${steward?.online ? 'Open' : 'Summon'} ${stewardLabel}</b><small>Helper for all of tclaude. Config > Steward sets what it can do without asking.</small></span>
    </button>`}
    <input ref=${filterRef} type="search" class="companion-menu-filter" placeholder="Pin a live agent…" aria-label="Filter live agents"
      value=${filter} onInput=${(event) => setFilter(event.currentTarget.value)} />
    <div class="companion-menu-list">
      ${candidates.length ? candidates.map((agent) => html`<button key=${agent.agentId} type="button" role="menuitem" onClick=${() => onPin(agent.agentId)}>
        <${agent.scribe ? Avatar : StatusDot} agent=${agent} status=${companionStatus(agent)} /> ${agent.name}</button>`)
        : html`<div class="companion-menu-empty">${filter ? 'No live agent matches.' : 'No other live agents.'}</div>`}
    </div>
  </div>`;
}

// DecisionCard is a decision the agent put to the operator with
// `notify-human --option ...`: its question, any attached image, and one
// button per option. The pick (plus an optional note) goes back to the agent
// as mail, even if it is offline. All of it is agent text, rendered as text.
function DecisionCard({ decision, fetchImpl, notify, from = '' }) {
  const [busy, setBusy] = useState(0);
  const [note, setNote] = useState('');
  const image = (decision.attachments || []).find((a) => a.previewable);
  async function choose(choice) {
    setBusy(choice);
    try {
      const response = await fetchImpl('/api/human-messages/reply', {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id: decision.id, choice, body: note.trim() }),
      });
      if (!response.ok) throw new Error((await response.text?.()) || `${response.status}`);
    } catch (error) {
      notify(`That decision could not be sent: ${error?.message || error}`, true);
      setBusy(0);
    }
  }
  const question = decision.subject || 'Decision';
  return html`<div class="companion-decision" role="group" aria-label=${`Decision: ${question}`}>
    <div class="companion-approval-head"><b>${question}</b>${from && html`<span class="companion-decision-from">${from}</span>`}</div>
    ${decision.body && decision.body !== decision.subject && html`<p class="companion-decision-body">${decision.body}</p>`}
    <div class="companion-decision-options">
      ${decision.options.map((option, i) => {
        const recommended = i + 1 === decision.recommended;
        return html`<button key=${option} type="button" class=${recommended ? 'companion-primary' : ''} disabled=${!!busy}
          title=${recommended ? 'The agent recommends this one' : ''} onClick=${() => choose(i + 1)}>
          ${busy === i + 1 ? 'Sending…' : option}${recommended && busy !== i + 1 ? html` <span class="companion-decision-rec">★</span>` : ''}
        </button>`;
      })}
    </div>
    <input class="companion-decision-note" type="text" placeholder="Add a note (optional)" value=${note} disabled=${!!busy}
      aria-label="Note to send with your choice" onInput=${(event) => setNote(event.currentTarget.value)} />
    ${image && html`<a class="companion-decision-image" href=${image.url} target="_blank" rel="noopener"><img src=${image.url} alt=${image.filename || 'attached image'} /></a>`}
  </div>`;
}

export function CompanionApp({
  speakerImpl = null,
  dictationImpl = null,
  snapshot,
  activeTab,
  prefs,
  fetchImpl = globalThis.fetch,
  notify = () => {},
  widgetFactory,
  loadRuntime = null,
  closeTerminalPanes = () => {},
  openTerminalsPane = null,
  win = globalThis.window,
  locks = win?.navigator?.locks,
  onComposeMessage = null,
  openMorph = null,
  openStewardHandover = null,
  openPermissions = null,
  openSudo = null,
  detachWindow = null,
  sessionPrefs: sessionPrefsImpl = null,
  registerController = registerCompanionController,
  navigate = null,
}) {
  const agents = useMemo(() => companionAgents(snapshot.value), [snapshot.value]);
  const customName = snapshot.value?.steward_name || '';
  const stewardLabel = customName || 'the Steward';
  const steward = useMemo(() => stewardAgent(agents), [agents]);
  const stewardRoster = snapshot.value?.steward_roster || [];
  const [tabs, setTabs] = useState(() => parseTabs(readPref(prefs, COMPANION_TABS_PREF)));
  const sessionPrefs = useMemo(() => {
    if (sessionPrefsImpl) return sessionPrefsImpl;
    try { return win?.sessionStorage ?? null; } catch { return null; }
  }, [win, sessionPrefsImpl]);
  const [active, setActive] = useState(() => {
    const saved = readPref(sessionPrefs, COMPANION_ACTIVE_PREF) || readPref(prefs, COMPANION_ACTIVE_PREF);
    return isAgentId(saved) ? saved : '';
  });
  const [open, setOpen] = useState(() => readPref(sessionPrefs, COMPANION_OPEN_PREF) === '1');
  const [chipSpot, setChipSpot] = useState(() => parseChipSpot(readPref(prefs, COMPANION_CHIP_SPOT_PREF)));
  const overlay = useOverlayWindow({
    id: 'companion', win, prefs, prefKey: COMPANION_PLACEMENT_PREF, defaultPlacement, snaps: true, open,
    // A floating window opens at the chips' spot until the operator moves it.
    spotted: (base, viewport, top) => {
      const spot = readPref(prefs, COMPANION_WINDOW_SPOT_PREF);
      return spot ? spotPlacement(parseChipSpot(spot), base, viewport, top) : null;
    },
    onPlaced: () => writePref(prefs, COMPANION_WINDOW_SPOT_PREF, ''),
    onRefocus: () => setFocusToken((n) => n + 1),
  });
  const { windowRef, openFrom, releaseFocus } = overlay;
  const [menu, setMenu] = useState(false);
  const addRef = useRef(null);
  const [busy, setBusy] = useState(false);
  // The Steward just shut down, { agentId, name }. It reads as gone from here
  // on: the snapshot can still show it online for a poll.
  const [shutDown, setShutDown] = useState(null);
  const askShutDown = useRef(null);
  const rosterRef = useRef(null);
  const [detached, setDetached] = useState(() => new Set(parseTabs(readPref(sessionPrefs, COMPANION_DETACHED_PREF))));
  const detachedRef = useRef(detached);
  detachedRef.current = detached;
  const detachedWindows = useRef(new Map());
  const [parked, setParked] = useState(() => new Set(parseTabs(readPref(sessionPrefs, COMPANION_PARKED_PREF))));
  const [statuses, setStatuses] = useState({});
  const [reconnectable, setReconnectable] = useState({});
  const terminalWidgetRef = useRef(null);
  const [generation, setGeneration] = useState(0);
  const [focusToken, setFocusToken] = useState(0);
  const [warmId, setWarmId] = useState('');
  const warmTimer = useRef(null);
  const [peek, setPeek] = useState(null); // {agentId, anchor, lines}
  // Unsent text per agent id, so a draft stays with the agent it was written
  // for however the active tab changes.
  const [drafts, setDrafts] = useState({});
  const setDraft = (agentId, text) => setDrafts((current) => {
    const { [agentId]: _replaced, ...rest } = current;
    return text ? { ...rest, [agentId]: text } : rest;
  });
  // deciding shows the fleet-wide "to decide" queue over the window body.
  const [deciding, setDeciding] = useState(false);
  // Answering the last decision ends the queue: a later decision must not
  // cover the window again while the operator is typing in it.
  const queueSize = pendingDecisions(snapshot.value).length;
  useEffect(() => { if (!queueSize) setDeciding(false); }, [queueSize]);
  const draftRef = useRef('');
  // Emptying the field by hand must not take it away from under the caret.
  const [fieldFocused, setFieldFocused] = useState(false);
  const fieldRef = useCallback((element) => { if (!element) setFieldFocused(false); }, []);
  const [withContext, setWithContext] = useState(true);
  // The agent the operator is marking up the page for (tcl-8vjjko), or null.
  // Voice output (tcl-q3qcfw): agents whose replies are spoken aloud.
  const [voiceAgents, setVoiceAgents] = useState(() => new Set(parseVoiceAgents(readPref(prefs, VOICE_PREF))));
  const voiceAgentsRef = useRef(voiceAgents);
  voiceAgentsRef.current = voiceAgents;
  const blockedHinted = useRef(false);
  const speaker = useMemo(() => speakerImpl || createSpeaker(win, {
    onBlocked: () => {
      if (blockedHinted.current) return;
      blockedHinted.current = true;
      notify('The browser blocked the companion\'s voice until you click or type on the page.', true);
    },
  }), [speakerImpl, win]);
  const voiceReader = useMemo(() => createVoiceReader(fetchImpl), [fetchImpl]);
  const voiceStatuses = useRef(new Map());
  const voiceDecisions = useRef(new Map());
  const [sending, setSending] = useState(false);
  const chipRects = useRef(new Map());
  // Agents just summoned or opened but not yet in a snapshot: kept pinned (and
  // shown as starting) until a snapshot lists them or PENDING_MS passes.
  const pending = useRef(new Map());
  // Agents just resumed: the snapshot still shows them offline, so they read as
  // starting until it says online or PENDING_MS passes.
  const [resuming, setResuming] = useState(() => new Set());
  // A tab whose agent has a window of its own is not here: the session has one view.
  const hasWindow = (agentId) => overlayWindows.value.some((window) => window.id === chatWindowId(agentId));
  const docked = tabs.filter((id) => !hasWindow(id));
  const activeId = docked.includes(active) ? active : (docked[0] || '');
  const activeAgent = activeId ? agents.get(activeId) || null : null;
  const [terminalFont, onTerminalFontChange] = useTerminalFont('companion', activeId, prefs);
  const draft = drafts[activeId] || '';
  draftRef.current = draft;
  // Voice input (tcl-sqxkb0): dictation fills the draft; ending with "send"
  // sends it. listeningRef keeps replies from being spoken into the open mic.
  const sendPhrase = dictationSendPhrase(snapshot.value);
  const composerShown = useRef(false);
  const dictation = useDictation({
    win, targetId: activeId, draft, setDraft, notify, dictationImpl, sendPhrase,
    silenceSend: voiceSilenceSeconds(snapshot.value),
    pauses: voicePauses(snapshot.value),
    mark: voiceMarksDictated(snapshot.value),
    continuous: companionContinuous(snapshot.value, activeAgent),
    wakeHost: () => (composerShown.current ? windowRef.current : null),
    send: (text) => send(null, text),
    onStart: () => speaker.cancel(),
    onRelease: () => { setFieldFocused(false); setFocusToken((n) => n + 1); },
  });
  const { listening, listeningRef } = dictation;
  // The Steward phrase. The dictation only starts once the Steward is the
  // target, so no word can land on the agent that had the focus, and the mic
  // stays reserved until then so the wake listener doesn't take it back.
  const stewardWake = useRef(null);
  const wakeDoor = useRef(null);
  wakeDoor.current = {
    refusal() {
      const name = customName || 'The Steward';
      if (!steward?.online || steward.agentId === shutDown?.agentId) return `${name} isn’t running. Start it from the companion.`;
      // Only Bring it back here takes a detached agent from its window.
      if (detachedRef.current.has(steward.agentId)) {
        detachedWindows.current.get(steward.agentId)?.focus?.();
        return `${name} is in its own window. Dictate there, or bring it back here.`;
      }
      if (hasWindow(steward.agentId)) {
        raiseOverlayWindow(chatWindowId(steward.agentId));
        return `${name} is in its own window. Dictate there.`;
      }
      return '';
    },
    reserve: () => dictation.reserve(),
    open(words, carry) {
      dictation.reserve();
      if (activeId === steward.agentId && composerShown.current) {
        dictation.handover(words, carry);
        return;
      }
      clearTimeout(stewardWake.current?.timer);
      const timer = setTimeout(() => {
        stewardWake.current = null;
        dictation.release();
        notify(`Could not open ${stewardLabel}, so nothing is listening.`, true);
      }, STEWARD_WAKE_MS);
      stewardWake.current = { agentId: steward.agentId, words, carry, timer };
      showAgent(steward.agentId);
    },
  };
  useEffect(() => registerStewardDoor({
    refusal: () => wakeDoor.current.refusal(),
    reserve: () => wakeDoor.current.reserve(),
    open: (words, carry) => wakeDoor.current.open(words, carry),
  }), []);
  useEffect(() => {
    const pendingWake = stewardWake.current;
    if (!pendingWake || activeId !== pendingWake.agentId || !composerShown.current) return;
    stewardWake.current = null;
    clearTimeout(pendingWake.timer);
    dictation.handover(pendingWake.words, pendingWake.carry);
  });

  useEffect(() => writePref(prefs, COMPANION_TABS_PREF, JSON.stringify(tabs)), [tabs]);
  useEffect(() => {
    if (!activeId) return;
    writePref(prefs, COMPANION_ACTIVE_PREF, activeId);
    writePref(sessionPrefs, COMPANION_ACTIVE_PREF, activeId);
  }, [activeId, sessionPrefs]);
  useEffect(() => writePref(sessionPrefs, COMPANION_DETACHED_PREF, JSON.stringify([...detached])), [detached, sessionPrefs]);
  useEffect(() => writePref(sessionPrefs, COMPANION_PARKED_PREF, JSON.stringify([...parked])), [parked, sessionPrefs]);
  useEffect(() => writePref(sessionPrefs, COMPANION_OPEN_PREF, open ? '1' : '0'), [open, sessionPrefs]);

  // Live scribes and Stewards keep a tab without being pinned.
  const closedHelpers = useRef(null);
  const closedList = () => {
    if (!closedHelpers.current) closedHelpers.current = parseClosedHelpers(readPref(prefs, COMPANION_CLOSED_PREF));
    return closedHelpers.current;
  };
  const setHelperClosed = (agentId, closed) => {
    const list = closedList();
    if (closed === list.has(agentId)) return;
    if (closed) list.add(agentId); else list.delete(agentId);
    writePref(prefs, COMPANION_CLOSED_PREF, JSON.stringify([...list]));
  };
  useEffect(() => {
    if (!snapshot.value) return;
    const keep = new Set([...pending.current.keys(), ...resuming, open ? activeId : ''].filter(Boolean));
    const next = syncHelperTabs(tabs, agents, closedList(), keep);
    if (next !== tabs) setTabs(next);
  }, [agents, snapshot.value, open, activeId, resuming]);

  // However an agent comes to be attached here (a tab chosen, a reload, the
  // active tab closing), this view takes over from its Terminals pane.
  useEffect(() => {
    if (open && activeId && !parked.has(activeId)) closeTerminalPanes([activeId, activeAgent?.convId].filter(Boolean));
  }, [open, activeId, activeAgent?.convId, parked]);

  // A pinned agent that was retired drops out of the snapshot; forget it once
  // the snapshot is known so the chips never show a dead handle forever.
  useEffect(() => {
    if (!snapshot.value) return;
    const now = Date.now();
    for (const [id, until] of pending.current) {
      if (agents.has(id) || until < now) pending.current.delete(id);
    }
    const keep = (id) => agents.has(id) || pending.current.has(id);
    if (tabs.some((id) => !keep(id))) setTabs((current) => current.filter(keep));
  }, [agents]);

  useEffect(() => {
    const up = [...resuming].filter((id) => agents.get(id)?.online);
    if (up.length) setResuming((current) => new Set([...current].filter((id) => !up.includes(id))));
  }, [agents, resuming]);
  useEffect(() => {
    if (!resuming.size) return undefined;
    const timer = setTimeout(() => setResuming(new Set()), PENDING_MS);
    return () => clearTimeout(timer);
  }, [resuming]);

  // A detached agent stays in its own window: opening its tab only shows where
  // it went. Attaching here would throw that window off the terminal, and the
  // window has no idea why. Only Bring it back here reclaims it.
  const showAgent = useCallback((agentId, { focus = true, starting = false, reclaim = false } = {}) => {
    if (!isAgentId(agentId)) return false;
    // A chat opens in one place: its window, when it has one.
    const chat = overlayWindows.peek().find((window) => window.id === chatWindowId(agentId));
    if (chat && !reclaim) {
      openOverlayWindow(chat);
      return true;
    }
    const agent = agents.get(agentId);
    // An id the snapshot does not know is only accepted for an agent this
    // window just started; other callers keep their own fallback.
    if (!agent && !starting && !pending.current.has(agentId)) return false;
    if (!agent && starting) pending.current.set(agentId, Date.now() + PENDING_MS);
    setHelperClosed(agentId, false);
    stopPeek();
    if (reclaim || !detachedRef.current.has(agentId)) {
      closeTerminalPanes([agentId, agent?.convId].filter(Boolean));
      setParked((current) => without(current, agentId));
      setDetached((current) => without(current, agentId));
    }
    setTabs((current) => withTab(current, agentId));
    setActive(agentId);
    setDeciding(false);
    setMenu(false);
    setOpen(true);
    raiseOverlayWindow('companion');
    if (focus) setFocusToken((n) => n + 1);
    return true;
  }, [agents, closeTerminalPanes]);

  // A tab of the Terminals tab moves here too, parked or not on the tab strip.
  const floatAgent = useRef(null);
  floatAgent.current = (selector) => {
    const agent = companionAgentOf(snapshot.peek(), selector);
    if (!agent) return false;
    floatTab(agent);
    return true;
  };
  useEffect(() => registerController({
    open: (agentId, options) => showAgent(agentId, options),
    float: (selector) => floatAgent.current(selector),
  }), [showAgent]);

  // A shut-down Steward can come back without this window: from the Agents tab,
  // the CLI, or by the seat going to another agent. Then there is nothing left
  // to offer. Seeing it offline first tells that from the snapshot still showing
  // it online for a poll after the shut-down.
  const shutDownSeen = useRef(false);
  useEffect(() => {
    if (!shutDown) {
      shutDownSeen.current = false;
      return;
    }
    const entry = stewardRoster.find((candidate) => candidate.agent_id === shutDown.agentId);
    if (!entry || entry.seat === 'standby') setShutDown(null);
    else if (!entry.online) shutDownSeen.current = true;
    else if (shutDownSeen.current) {
      setShutDown(null);
      setHelperClosed(shutDown.agentId, false);
    }
  });

  // The Steward that can come back, by Resume. One just shut down can still show
  // as online for a poll, so it stands in for the roster until then.
  const rosterReturn = steward?.online && steward.agentId !== shutDown?.agentId ? null : stewardToResume(stewardRoster);
  // One already resuming offers no second start: Start fresh would make two.
  const returning = (shutDown || rosterReturn) && !resuming.has(shutDown?.agentId || rosterReturn.agent_id) ? {
    agentId: shutDown?.agentId || rosterReturn.agent_id,
    name: shutDown?.name || (rosterReturn.agent_id === steward?.agentId ? steward.name : rosterReturn.name),
    lastRan: stewardRoster.find((entry) => entry.agent_id === (shutDown?.agentId || rosterReturn.agent_id))?.last_ran || '',
    keeps: stewardRoster.find((entry) => entry.agent_id === (shutDown?.agentId || rosterReturn.agent_id))?.keeps || [],
  } : null;

  const summonSteward = useCallback(async () => {
    if (steward?.online && steward.agentId !== shutDown?.agentId) {
      showAgent(steward.agentId);
      return;
    }
    setBusy(true);
    try {
      const result = await requestStewardSummon(fetchImpl);
      if (!isAgentId(result.agent_id)) throw new Error('the summon returned no agent id');
      setShutDown(null);
      notify(`${result.reused ? 'Reopened' : 'Summoned'} ${stewardLabel}.`);
      showAgent(result.agent_id, { starting: true });
    } catch (error) {
      notify(`Could not summon ${stewardLabel}: ${error?.message || error}`, true);
    } finally {
      setBusy(false);
    }
  }, [steward, shutDown, stewardLabel, fetchImpl, notify, showAgent]);

  const chatOf = (target) => chatAgentOf(target, snapshot.peek());

  const closeTab = useCallback((agentId, { keepOpen = false } = {}) => {
    if (agents.get(agentId)?.scribe) setHelperClosed(agentId, true);
    setVoiceAgents((current) => {
      if (!current.has(agentId)) return current;
      const next = new Set(current);
      next.delete(agentId);
      return next;
    });
    const next = withoutTab(tabs, agentId);
    if (agentId === activeId) setActive(nextActive(tabs, agentId));
    if (!next.length && !keepOpen) setOpen(false);
    setTabs(next);
  }, [tabs, activeId, agents]);

  // floatTab moves a tab into a window of its own, at at if it was dragged there.
  function floatTab(agent, at = null) {
    // A browser window of it goes, as Bring it back here closes one: it would
    // lose the session to this window with no idea why.
    const opened = detachedWindows.current.get(agent.agentId);
    detachedWindows.current.delete(agent.agentId);
    if (opened) setTimeout(() => opened.close?.(), RECLAIM_CLOSE_MS);
    setParked((current) => without(current, agent.agentId));
    setDetached((current) => without(current, agent.agentId));
    // An agent that never had a tab leaves the companion as it was.
    closeTab(agent.agentId, { keepOpen: !tabs.includes(agent.agentId) });
    openOverlayWindow({ id: chatWindowId(agent.agentId), kind: CHAT_WINDOW_KIND, agent: agent.agentId, label: agent.name, at });
  }

  // A press on a tab stays a click until it is pulled well off the tab strip.
  function onTabPointerDown(event, agent) {
    if (event.button !== 0) return;
    const handle = event.currentTarget;
    const strip = handle.closest('.companion-titlebar')?.getBoundingClientRect?.();
    if (!strip) return;
    handle.setPointerCapture?.(event.pointerId);
    let torn = false;
    const off = (m) => m.clientY < strip.top - TAB_TEAR_PX || m.clientY > strip.bottom + TAB_TEAR_PX
      || m.clientX < strip.left - TAB_TEAR_PX || m.clientX > strip.right + TAB_TEAR_PX;
    const move = (m) => {
      torn = off(m);
      handle.classList.toggle('tearing', torn);
    };
    const up = (m) => {
      handle.removeEventListener('pointermove', move);
      handle.removeEventListener('pointerup', up);
      handle.removeEventListener('pointercancel', up);
      handle.classList.remove('tearing');
      if (torn && m.type === 'pointerup') {
        handle.addEventListener('click', (click) => { click.preventDefault(); click.stopPropagation(); }, { capture: true, once: true });
        floatTab(agent, { x: m.clientX - 120, y: m.clientY - 14 });
      }
    };
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', up);
    handle.addEventListener('pointercancel', up);
  }

  function chatBackToTab(instance) {
    closeOverlayWindow(instance.id);
    showAgent(instance.agent, { reclaim: true });
  }

  // The window stays open on the shut-down state, with the way to start again.
  // A detached window of it goes too: it has nothing left to show.
  // Closing is the window's ✕ asking: then the window goes as a close does, and
  // Resume waits in ✦ Steward.
  function stewardShutDown(agent, { closing = false } = {}) {
    setResuming((current) => without(current, agent.agentId));
    detachedWindows.current.get(agent.agentId)?.close?.();
    detachedWindows.current.delete(agent.agentId);
    setParked((current) => without(current, agent.agentId));
    setDetached((current) => without(current, agent.agentId));
    closeTab(agent.agentId, { keepOpen: !closing });
    setShutDown({ agentId: agent.agentId, name: agent.name });
    notify(`${agent.name} is shut down.`);
  }

  const minimize = useCallback(() => {
    setMenu(false);
    setDeciding(false);
    overlay.shrinkTo(chipRects.current.get(activeId), () => setOpen(false));
  }, [activeId, win]);

  const toggle = useCallback(() => {
    stopPeek();
    if (open) {
      minimize();
      return;
    }
    raiseOverlayWindow('companion');
    if (!docked.length) {
      setOpen(true);
      return;
    }
    openFrom.current = chipRects.current.get(activeId) || null;
    setOpen(true);
    setFocusToken((n) => n + 1);
  }, [open, docked.length, activeId, minimize]);

  // Ctrl+. toggles the window from anywhere, including from inside a focused
  // terminal: capture phase, before xterm sees the key.
  useEffect(() => {
    const onKey = (event) => {
      if (event.ctrlKey && !event.altKey && !event.metaKey && event.key === '.') {
        event.preventDefault();
        event.stopPropagation();
        toggle();
      } else if (event.ctrlKey && event.shiftKey && !event.altKey && !event.metaKey && event.code === 'Period') {
        // Ctrl+Shift+. marks up the page for the agent in the window.
        event.preventDefault();
        event.stopPropagation();
        startAnnotating(chatOf(event.target) || activeAgent);
      }
    };
    win?.addEventListener?.('keydown', onKey, true);
    return () => win?.removeEventListener?.('keydown', onKey, true);
  }, [toggle, win, activeAgent?.agentId, activeAgent?.online]);

  // Minimizing hides the window and only releases its terminal later.
  useEffect(() => {
    if (!open) return undefined;
    setWarmId(activeId || '');
    clearTimeout(warmTimer.current);
    return () => { warmTimer.current = setTimeout(() => setWarmId(''), WARM_MS); };
  }, [open, activeId]);
  // Keys must not reach a terminal nobody can see.
  useEffect(() => {
    if (open) return;
    const focused = win?.document?.activeElement;
    if (windowRef.current?.contains(focused)) focused.blur?.();
  }, [open]);
  useEffect(() => () => clearTimeout(warmTimer.current), []);

  function moveChips(spot) {
    setChipSpot(spot);
    writePref(prefs, COMPANION_CHIP_SPOT_PREF, spot);
    writePref(prefs, COMPANION_WINDOW_SPOT_PREF, spot === DEFAULT_CHIP_SPOT ? '' : spot);
  }

  function closeActive(event) {
    if (!closesTab) {
      minimize();
      return;
    }
    if (closeShutsDown) {
      askShutDown.current?.();
      return;
    }
    closeTab(activeAgent.agentId);
    releaseFocus(event);
  }

  // Peek: a short hover delay, then the pane text, refreshed while hovered.
  const peekTimer = useRef(null);
  const peekRefresh = useRef(null);
  const peekAgent = useRef('');
  function stopPeek() {
    clearTimeout(peekTimer.current);
    clearInterval(peekRefresh.current);
    peekTimer.current = null;
    peekRefresh.current = null;
    peekAgent.current = '';
    setPeek(null);
  }
  useEffect(() => stopPeek, []);
  async function readPeek(agentId) {
    try {
      const response = await fetchImpl(`/api/companion/${encodeURIComponent(agentId)}/peek`, { credentials: 'same-origin' });
      const result = await response.json().catch(() => ({}));
      if (peekAgent.current !== agentId) return;
      const lines = response.ok && Array.isArray(result.lines) ? result.lines.map(String) : [];
      setPeek((current) => (current && current.agentId === agentId ? { ...current, lines } : current));
    } catch {
      if (peekAgent.current === agentId) setPeek((current) => (current ? { ...current, lines: [] } : current));
    }
  }
  function startPeek(agentId, chip) {
    stopPeek();
    peekAgent.current = agentId;
    peekTimer.current = setTimeout(() => {
      const rect = chip.getBoundingClientRect();
      const vp = viewportOf(win);
      setPeek({ agentId, lines: null, anchor: peekAnchor(chipSpot, rect, vp) });
      void readPeek(agentId);
      peekRefresh.current = setInterval(() => void readPeek(agentId), PEEK_REFRESH_MS);
    }, PEEK_DELAY_MS);
  }

  const chipDrag = useChipDrag({ win, spot: chipSpot, onDrop: moveChips, onStart: stopPeek, reducedMotion: () => reducedMotion(win) });

  function onChipClick(agentId, event) {
    stopPeek();
    // Asking for permission is answered where it is quickest, in the panel.
    if (agentId && approvalsFor(snapshot.value, agents.get(agentId)).length) {
      openAccessPanel({ agentId, launcher: event.currentTarget, documentRef: win?.document });
      return;
    }
    openFrom.current = event.currentTarget.getBoundingClientRect();
    if (agentId) showAgent(agentId);
    else toggle();
  }

  async function send(event, text = draft) {
    event?.preventDefault?.();
    const context = withContext
      ? viewContext({ activeTab: activeTab.value, pathname: win?.location?.pathname, search: win?.location?.search })
      : '';
    const message = dictation.outgoing(text);
    if (!companionMessageFits(message, context)) {
      notify('That message is too long to send. Shorten it and try again.', true);
      return false;
    }
    const body = composeCompanionMessage(message, context);
    if (!body || !activeAgent || sending) return false;
    // An open mic would bring the sent text back. Enter or Send with the mic
    // on sends what the draft holds; a failed send leaves it there.
    if (event) dictation.abort?.();
    setSending(true);
    try {
      await postOperatorMessage(fetchImpl, { to: activeAgent.agentId, body });
      // Whatever was typed while this went out stays.
      setDrafts((current) => {
        if (current[activeAgent.agentId] !== text) return current;
        const { [activeAgent.agentId]: _sent, ...rest } = current;
        return rest;
      });
      // Send and the field are about to go, and focus would fall to the page.
      const focused = win?.document?.activeElement;
      if (draftRef.current === text && focused?.matches?.('.companion-draft, .companion-input button[type="submit"]')) {
        setFieldFocused(false);
        setFocusToken((n) => n + 1);
      }
      return true;
    } catch (error) {
      notify(`Message to ${activeAgent.name} failed: ${error?.message || error}`, true);
      return false;
    } finally {
      setSending(false);
    }
  }

  useEffect(() => {
    const pinned = [...voiceAgents].filter((id) => tabs.includes(id));
    writePref(prefs, VOICE_PREF, JSON.stringify(pinned));
    for (const id of pinned) voiceReader.prime(id).catch(() => {});
  }, [voiceAgents, voiceReader, tabs]);

  // Speak when a voice agent's turn ends, or when it starts waiting on the
  // operator. Only transitions speak; the first sight of an agent is silent.
  // Voice applies to pinned tabs only. Several dashboard tabs see the same
  // transition, so each utterance is claimed in shared prefs first and the
  // first tab to claim it speaks.
  useEffect(() => {
    if (!speaker.supported) return;
    const next = new Map();
    const quiet = () => dictationOpen() || listeningRef.current; // never speak into an open mic
    for (const id of voiceAgents) {
      const agent = agents.get(id);
      if (agent && tabs.includes(id)) next.set(id, statusOf(agent));
    }
    const said = voiceTransitions(voiceStatuses.current, next);
    voiceStatuses.current = next;
    const asked = new Map();
    for (const id of next.keys()) asked.set(id, decisionsFor(snapshot.value, agents.get(id)).at(-1)?.id || 0);
    const arrived = decisionArrivals(voiceDecisions.current, asked);
    voiceDecisions.current = asked;
    const named = next.size > 1;
    const claim = (key, value, fresh) => {
      const previous = readPref(prefs, key);
      if (previous && !fresh(previous)) return false;
      writePref(prefs, key, value);
      return true;
    };
    for (const { agentId, kind } of said) {
      const agent = agents.get(agentId);
      if (!agent) continue;
      if (kind === 'needs') {
        const now = Date.now();
        if (!claim(`${VOICE_PREF}.needs.${agentId}`, String(now), (prev) => now - Number(prev) > 5000)) continue;
        const decision = decisionsFor(snapshot.value, agent).at(-1);
        if (!quiet()) speaker.speak(decision ? speakableText(decisionSpeech(agent, decision), 400)
          : `${agent.name} needs you${agent.detail ? `: ${speakableText(agent.detail, 120)}` : '.'}`);
        continue;
      }
      const speakReply = (retry) => voiceReader.latest(agentId).then((reply) => {
        if (!voiceAgentsRef.current.has(agentId)) return;
        if (!reply) {
          // The transcript may land just after the status flips: look once more.
          if (retry) setTimeout(() => speakReply(false), 1500);
          return;
        }
        const fresh = (prev) => { try { return BigInt(reply.at) > BigInt(prev); } catch { return true; } };
        if (!claim(`${VOICE_PREF}.said.${agentId}`, reply.at, fresh)) return;
        const spoken = speakableText(reply.text);
        if (spoken && !quiet()) speaker.speak(named ? `${agent.name}: ${spoken}` : spoken);
      }).catch(() => {});
      speakReply(true);
    }
    // A further decision from an agent already waiting on the operator.
    const spoken = new Set(said.filter((s) => s.kind === 'needs').map((s) => s.agentId));
    for (const agentId of arrived) {
      const agent = agents.get(agentId);
      const decision = agent && !spoken.has(agentId) && decisionsFor(snapshot.value, agent).at(-1);
      if (!decision) continue;
      if (!claim(`${VOICE_PREF}.decision.${agentId}`, String(decision.id), (prev) => decision.id > Number(prev))) continue;
      if (!quiet()) speaker.speak(speakableText(decisionSpeech(agent, decision), 400));
    }
  }, [agents, voiceAgents, tabs, snapshot.value]);

  function toggleVoice(agent) {
    setVoiceAgents((current) => {
      const next = new Set(current);
      if (next.has(agent.agentId)) {
        next.delete(agent.agentId);
        voiceReader.forget(agent.agentId);
        speaker.cancel();
      } else {
        next.add(agent.agentId);
      }
      return next;
    });
  }

  // The window and chips stay mounted (the terminal keeps its stream) but
  // step out of the way while the operator draws.
  const annotation = useAnnotation({
    win, notify,
    viewNow: () => viewContext({ activeTab: activeTab.value, pathname: win?.location?.pathname, search: win?.location?.search }),
  });

  function startAnnotating(agent = activeAgent) {
    if (!agent?.online) {
      notify('Open a live agent in the companion first, then mark up the page for it.', true);
      return;
    }
    annotation.start({ agentId: agent.agentId, name: agent.name, fetchImpl });
  }

  async function resume(agent) {
    const started = () => setResuming((current) => new Set(current).add(agent.agentId));
    try {
      await resumeAgent(fetchImpl, agent.agentId);
      notify(`Resuming ${agent.name}…`);
      started();
      return true;
    } catch (error) {
      if (error?.recoverable) {
        const { resumeAgentReq } = await import('./dashboard-operations.js');
        const resumed = await resumeAgentReq(agent.agentId, agent.name);
        if (resumed) started();
        return resumed;
      }
      notify(`Could not resume ${agent.name}: ${error?.message || error}`, true);
      return false;
    }
  }

  async function resumeSteward() {
    if (!returning || busy) return;
    setBusy(true);
    setMenu(false);
    try {
      if (await resume(returning)) {
        setShutDown(null);
        showAgent(returning.agentId, { starting: true });
      }
    } finally {
      setBusy(false);
    }
  }

  // The window has one attach to the terminal, so it lets go first. A blocked
  // pop-up leaves everything where it was.
  function detachToWindow(agent) {
    const rect = windowRef.current?.getBoundingClientRect?.();
    const size = rect ? { width: rect.width, height: rect.height } : null;
    const opened = detachWindow(agent.agentId, agent.name, { harness: agent.harness, size, at: elementScreenPoint(win, rect) });
    if (!opened) {
      notify('Could not detach: allow pop-ups for this dashboard.', true);
      return;
    }
    detachedWindows.current.set(agent.agentId, opened);
    setParked((current) => new Set(current).add(agent.agentId));
    setDetached((current) => new Set(current).add(agent.agentId));
  }

  // Attach first: the window sees it lost the terminal, so closing it cannot
  // hide the terminal from here.
  function reclaim(agentId) {
    const opened = detachedWindows.current.get(agentId);
    detachedWindows.current.delete(agentId);
    showAgent(agentId, { reclaim: true });
    if (opened) setTimeout(() => opened.close?.(), RECLAIM_CLOSE_MS);
  }

  // A detached window the operator closed hands the agent back.
  useEffect(() => {
    if (!detached.size) return undefined;
    const check = setInterval(() => {
      for (const [agentId, opened] of detachedWindows.current) {
        if (!opened.closed) continue;
        detachedWindows.current.delete(agentId);
        setParked((current) => without(current, agentId));
        setDetached((current) => without(current, agentId));
      }
    }, DETACHED_CHECK_MS);
    return () => clearInterval(check);
  }, [detached.size]);

  function moveToTerminals(agent) {
    if (!openTerminalsPane) return;
    setParked((current) => new Set(current).add(agent.agentId));
    // Let the companion's widget detach before the Terminals pane attaches.
    setTimeout(() => openTerminalsPane(agent.agentId, agent.name, agent.harness), 60);
  }

  const setReconnectFor = useCallback((agentId) => (available) => setReconnectable((current) => (current[agentId] === available ? current : { ...current, [agentId]: available })), []);
  const setStatusFor = useCallback((agentId) => (status) => setStatuses((current) => (current[agentId] === status ? current : { ...current, [agentId]: status })), []);

  // -- dashboard guidance ---------------------------------------------------
  // Play the agents' pointing cues.

  const seenCues = useRef(new Set());
  const cuesPrimed = useRef(false);
  const cueRun = useRef(0);
  const [guideCue, setGuideCue] = useState(null); // {cue, element}
  const dismissGuide = useCallback(() => { cueRun.current += 1; setGuideCue(null); }, []);
  useEffect(() => {
    if (!snapshot.value) return;
    const fresh = newCues(seenCues.current, snapshot.value.companion_cues, cuesPrimed.current);
    cuesPrimed.current = true;
    // Every hand-over counts; only the newest cue's pointing is played.
    for (const cue of fresh) if (cue.open) openCue(cue);
    if (fresh.length) void playCue(fresh[fresh.length - 1]);
  }, [snapshot.value]);
  useEffect(() => {
    if (!guideCue) return undefined;
    const run = cueRun.current;
    const timer = setTimeout(() => { if (cueRun.current === run) setGuideCue(null); }, GUIDE_SHOW_MS);
    return () => clearTimeout(timer);
  }, [guideCue]);

  function composeToActive(agent = activeAgent, restoreFocus = () => setFocusToken((n) => n + 1)) {
    if (!agent?.online) return false;
    onComposeMessage?.({ agent: agent.agentId, label: agent.name, restoreFocus });
    return true;
  }
  const composeRef = useRef(composeToActive);
  composeRef.current = composeToActive;
  useEffect(() => {
    if (!onComposeMessage) return undefined;
    const doc = win?.document;
    if (!doc) return undefined;
    // Capture, like the Terminals tab: the shortcut works from the terminal,
    // the tabs and the title bar, before xterm or the browser sees it.
    const onKey = (event) => {
      if (!isComposeMessageShortcut(event)) return;
      // A repeat with the composer open must not reach macOS Cmd+M, on any tab.
      if (doc.querySelector('#operator-message-modal.show')) {
        event.preventDefault();
        event.stopPropagation();
        return;
      }
      if (!event.target?.closest?.('.companion-window') || hasShownOverlay()) return;
      const chat = event.target.closest('.chat-window');
      const sent = chat
        ? composeRef.current(chatOf(event.target), () => chat.querySelector('.companion-term-host textarea')?.focus?.())
        : composeRef.current();
      if (sent) {
        event.preventDefault();
        event.stopPropagation();
      }
    };
    doc.addEventListener('keydown', onKey, true);
    return () => doc.removeEventListener('keydown', onKey, true);
  }, [win, onComposeMessage]);

  const lastTerminalKey = useRef(0);
  useEffect(() => {
    const doc = win?.document;
    if (!doc) return undefined;
    const onKey = (event) => {
      if (!event.target?.closest?.('.companion-term-host')) return;
      lastTerminalKey.current = Date.now();
      // Modifiers and window shortcuts are not typing.
      const text = event.key?.length === 1 && !event.ctrlKey && !event.metaKey && !event.altKey;
      if (text || event.key === 'Enter' || event.key === 'Backspace') speaker.cancel();
    };
    doc.addEventListener('keydown', onKey, true);
    return () => doc.removeEventListener('keydown', onKey, true);
  }, [win, speaker]);

  // openForCue opens the agent a cue hands over. While the operator is
  // typing or dictating to the current tab it is only pinned, never switched
  // to: a switch would take the terminal they are typing in, or the draft they
  // are writing, away from under them.
  function openForCue(cue) {
    const agent = agents.get(cue.open);
    if (!agent) return;
    const by = cue.agent_name || 'An agent';
    // Focus alone is no signal: it stays in the terminal long after the last key.
    const typing = Date.now() - lastTerminalKey.current < CUE_TYPING_WINDOW_MS;
    const drafting = !showQueue && activeAgent?.online && draftRef.current.trim();
    if (drafting || listeningRef.current || typing) {
      if (!hasWindow(agent.agentId)) setTabs((current) => withTab(current, agent.agentId));
      notify(`${by} pinned ${agent.name} in the companion window for you.`, false);
      return;
    }
    showAgent(agent.agentId, { focus: false });
  }
  const openForCueRef = useRef(openForCue);
  openForCueRef.current = openForCue;

  // A hand-over that lands while this dashboard tab is hidden waits for the
  // operator to come back to it. One tab acts on each hand-over: the first
  // to take its Web Lock, which every open tab shares at once. Shared prefs
  // are a per-page cache, so they only stop a tab that loaded after the claim,
  // and they are the whole claim where Web Locks are missing.
  const heldOpen = useRef([]);
  const handOver = (cue) => {
    const take = async () => {
      if (!claimCueOpen(readPref(prefs, COMPANION_CUE_OPEN_PREF), cue)) return;
      if (!(await claimCueLock(locks, cue))) return;
      writePref(prefs, COMPANION_CUE_OPEN_PREF, cueOpenClaimed(readPref(prefs, COMPANION_CUE_OPEN_PREF), cue));
      openForCueRef.current(cue);
    };
    const focused = win?.document?.hasFocus?.() ?? true;
    if (focused) void take();
    else {
      setTimeout(() => {
        // The operator may have left this tab during the wait.
        if (win?.document?.visibilityState === 'hidden') heldOpen.current.push(cue);
        else void take();
      }, CUE_OPEN_UNFOCUSED_DELAY_MS);
    }
  };
  // An agent the Steward hands over opens in the window, in the dashboard
  // tab the operator is looking at only: a terminal attach takes over from
  // any other view, so a hidden tab holds it until it is shown.
  const openCue = (cue) => {
    if (win?.document?.visibilityState === 'hidden') heldOpen.current.push(cue);
    else handOver(cue);
  };
  useEffect(() => {
    const doc = win?.document;
    const onVisible = () => {
      const held = heldOpen.current;
      if (!held.length || doc?.visibilityState === 'hidden') return;
      heldOpen.current = [];
      for (const cue of held) if (cueOpenDue(cue, Date.now())) handOver(cue);
    };
    doc?.addEventListener?.('visibilitychange', onVisible);
    return () => doc?.removeEventListener?.('visibilitychange', onVisible);
  }, [win]);

  async function playCue(cue) {
    const run = ++cueRun.current;
    setGuideCue(null);
    if (!cue.tab && !cue.path && !cue.target && !cue.note && !cue.marks?.length) return;
    const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
    const path = cuePath(cue);
    if (path && navigate) {
      try { await navigate(path); } catch { /* stay on the current view */ }
      await wait(GUIDE_SETTLE_MS);
    }
    let element = null;
    if (cue.target) {
      const deadline = Date.now() + GUIDE_FIND_TIMEOUT_MS;
      while (cueRun.current === run) {
        element = findGuideTarget(win?.document, cue.target);
        if (element || Date.now() >= deadline) break;
        await wait(GUIDE_FIND_STEP_MS);
      }
    }
    // Marks (tcl-0waby5) name their targets by visible text too.
    let marks = [];
    if (Array.isArray(cue.marks) && cue.marks.length) {
      const deadline = Date.now() + GUIDE_FIND_TIMEOUT_MS;
      const resolved = (m) => m.element && (m.mark.kind !== 'arrow' || m.from);
      let found = 0;
      let progressAt = Date.now();
      while (cueRun.current === run) {
        // Only the marks still missing are looked up again.
        marks = cue.marks.map((mark, i) => (marks[i] && resolved(marks[i]) ? marks[i] : {
          mark,
          element: findGuideTarget(win?.document, mark.target, { loose: true }),
          from: mark.kind === 'arrow' ? findGuideTarget(win?.document, mark.from, { loose: true }) : null,
        }));
        const now = marks.filter(resolved).length;
        if (now > found) { found = now; progressAt = Date.now(); }
        if (now === marks.length || Date.now() >= deadline) break;
        // Once the page has shown something, a mark that stays missing
        // does not hold the rest back.
        if ((found || element) && Date.now() - progressAt >= GUIDE_MARK_PATIENCE_MS) break;
        await wait(GUIDE_FIND_STEP_MS);
      }
    }
    if (cueRun.current !== run) return;
    const focus = element || marks.find((m) => m.element)?.element;
    focus?.scrollIntoView?.({ block: 'center', behavior: reducedMotion(win) ? 'auto' : 'smooth' });
    setGuideCue({ cue, element, marks });
  }

  // -- render -----------------------------------------------------------------

  const approvals = (agent) => approvalsFor(snapshot.value, agent);
  const decisions = (agent) => decisionsFor(snapshot.value, agent);
  // A pending approval or decision is the clearest "needs you" there is.
  const statusOf = (agent) => (approvals(agent).length || decisions(agent).length ? 'needs' : companionStatus(agent));
  const chipAgents = docked.map((id) => agents.get(id)).filter(Boolean);
  const queue = pendingDecisions(snapshot.value);
  const toApprove = pendingApprovals(snapshot.value);
  const elsewhere = toApprove.filter((request) => !chipAgents.some((agent) => approvalsFor({ access_requests: [request] }, agent).length));
  const showQueue = deciding && queue.length > 0;
  const showField = draft || listening || fieldFocused;
  const askerName = (d) => agents.get(d.from_agent)?.name || d.from_title || 'an agent';
  const needsCount = chipAgents.filter((agent) => statusOf(agent) === 'needs').length;
  const peekTarget = peek ? agents.get(peek.agentId) : null;

  const chipSlots = !open && chipDrag.slots && chipDrag.slots.map(({ spot, near, width, height }) => html`<div key=${spot}
    class=${`companion-chip-slot companion-spot-${spot}${near ? ' near' : ''}`} data-chip-spot=${spot}
    style=${`width:${width}px;height:${height}px`} aria-hidden="true"></div>`);

  const chips = !open && html`<div ref=${chipDrag.ref} class=${`companion-chips companion-spot-${chipSpot}${chipDrag.slots ? ' dragging' : ''}`}
    role="toolbar" aria-label="Companions" onPointerDown=${chipDrag.onPointerDown} onClickCapture=${chipDrag.onClickCapture}>
    ${chipAgents.map((agent) => {
      const status = statusOf(agent);
      return html`<button key=${agent.agentId} type="button"
        class=${`companion-chip ${status}`}
        ref=${(node) => { if (node) chipRects.current.set(agent.agentId, node.getBoundingClientRect()); }}
        aria-label=${`${agent.name}: ${companionStatusLabel(status)}. Open companion (Ctrl+.)`}
        onMouseEnter=${(event) => startPeek(agent.agentId, event.currentTarget)}
        onMouseLeave=${stopPeek}
        onFocus=${(event) => startPeek(agent.agentId, event.currentTarget)}
        onBlur=${stopPeek}
        onClick=${(event) => onChipClick(agent.agentId, event)}>
        <${Avatar} agent=${agent} />
        <span class="companion-chip-name">${agent.name}</span>
        ${elevatedAccess(snapshot.value, agent) && html`<span title="Elevated access">🔓</span>`}
        ${status === 'needs' ? html`<span class="companion-chip-needs">${approvals(agent).length ? '🔑 approval' : decisions(agent).length ? 'decide' : 'needs you'}</span>` : html`<${StatusDot} status=${status} />`}
      </button>`;
    })}
    ${queue.length > 0 && html`<button type="button" class="companion-chip companion-chip-decide"
      aria-label=${`${queue.length} ${queue.length === 1 ? 'decision' : 'decisions'} to make. Open them`}
      title="Agents are waiting for you to choose"
      onClick=${(event) => { stopPeek(); openFrom.current = event.currentTarget.getBoundingClientRect(); setDeciding(true); setOpen(true); }}>
      <span class="companion-decide-mark" aria-hidden="true">◆</span><span class="companion-chip-name">${queue.length} to decide</span>
    </button>`}
    ${elsewhere.length > 0 && html`<button type="button" class="companion-chip companion-chip-decide"
      aria-label=${`${elsewhere.length} permission ${elsewhere.length === 1 ? 'request' : 'requests'} from other agents. Open them`}
      title="Agents are asking for permission"
      onClick=${(event) => { stopPeek(); openAccessPanel({ launcher: event.currentTarget, documentRef: win?.document }); }}>
      <span aria-hidden="true">🔑</span><span class="companion-chip-name">${elsewhere.length} to approve</span>
    </button>`}
    ${!chipAgents.length && html`<button type="button" class="companion-chip companion-chip-steward"
      aria-label="Open the companion (Ctrl+.)" title=${`${customName || 'Steward'}: your helper for all of tclaude (Ctrl+.)`}
      onClick=${(event) => onChipClick('', event)}>
      <span class="companion-avatar steward" aria-hidden="true">✦</span><span class="companion-chip-name">${customName || 'Steward'}</span>
    </button>`}
  </div>`;

  const status = activeAgent ? statusOf(activeAgent) : 'offline';
  const activeApprovals = activeAgent ? approvals(activeAgent) : [];
  const activeElevated = elevatedAccess(snapshot.value, activeAgent);
  const activeDecisions = activeAgent ? decisions(activeAgent) : [];
  const activeActions = activeAgent ? actionsFor(snapshot.value, activeAgent) : [];
  const showRoster = !!(openStewardHandover && activeAgent && !showQueue && (activeAgent.steward || stewardRoster.some((entry) => entry.agent_id === activeAgent.agentId)));
  const standby = showRoster ? stewardRoster.filter((entry) => entry.seat === 'standby').length : 0;
  const termStatus = activeAgent ? statuses[activeAgent.agentId] || 'connecting…' : '';
  const canReconnect = activeAgent ? reconnectable[activeAgent.agentId] === true || ['disconnected', 'error', 'authentication required'].includes(termStatus) : false;
  const starting = activeAgent ? !activeAgent.online && resuming.has(activeId) : activeId && pending.current.has(activeId);

  // A terminal that lost its attach while hidden, for example to a Terminals
  // tab pane, has nothing to show, so reopening starts a fresh connection.
  useEffect(() => {
    if (open && canReconnect) setGeneration((n) => n + 1);
  }, [open]);

  // Only the terminal already attached may stay hidden. Mounting one for any
  // other agent, or again after a restart, would take the attach from whatever
  // view holds it, for a window nobody can see.
  const warm = !!warmId && warmId === activeId && activeAgent?.online && !parked.has(warmId);
  useEffect(() => {
    if (!open && warmId && !warm) setWarmId('');
  }, [open, warmId, warm]);

  const showComposer = !showQueue && activeAgent && activeAgent.online;
  composerShown.current = !!(open && showComposer);
  // A mic with no window showing would have no countdown and no Cancel.
  useEffect(() => { if (!composerShown.current) dictation.abort?.(); }, [composerShown.current]);
  const messageTools = (children = null) => html`<${FloatingTools} dictation=${dictation}
    micLabel=${`Dictate a message to ${activeAgent.name}`}
    micTitle=${micTitle(sendPhrase)}
    withContext=${withContext} onContext=${setWithContext}
    contextNote=${(snapshot.value?.steward_access?.capture_off || []).includes(activeAgent.agentId)
      ? `${activeAgent.name} can't capture your screen: that permission is off for it. The attached view says where you are, not what is on screen.` : ''}
    annotateLabel=${`Mark up this page for ${activeAgent.name} (Ctrl+Shift+.)`} onAnnotate=${() => startAnnotating()}>${children}<//>`;
  const terminalZoom = html`<${ShellHere} agent=${activeAgent?.agentId} name=${activeAgent?.name} snapshot=${snapshot.value} notify=${notify} />
    <${TerminalZoom} font=${terminalFont} widget=${() => terminalWidgetRef.current} />`;

  const look = windowControlsLook(snapshot.value?.window_controls);
  // Close takes the tab being shown off the window, as its own × does. With
  // no such tab there is nothing to close, so the window just goes away.
  const closesTab = activeAgent && !showQueue;
  // A running Steward isn't closed in passing: its ✕ asks what ⏻ asks.
  const closeShutsDown = closesTab && activeAgent.steward && activeAgent.online;
  const closeTitle = closeShutsDown ? 'Close: shut it down, after asking' : closesTab ? 'Close (the agent keeps running)' : 'Close';
  const controls = {
    look, closeLabel: closesTab ? `Close ${activeAgent.name}` : 'Close', closeTitle,
    onMinimize: minimize, onClose: closeActive,
  };

  const titlebarContent = html`
      <div class="companion-tabs" role="tablist" aria-label="Companion agents">
        ${chipAgents.map((agent) => {
          const tabStatus = statusOf(agent);
          const selected = agent.agentId === activeId && !showQueue;
          return html`<div key=${agent.agentId} class=${`companion-tab${selected ? ' selected' : ''}`}>
            <button type="button" role="tab" aria-selected=${selected ? 'true' : 'false'}
              title=${`${agent.name}: ${companionStatusLabel(tabStatus)}. Drag out for a window of its own.`}
              onPointerDown=${(event) => onTabPointerDown(event, agent)}
              onClick=${() => showAgent(agent.agentId)}>
              <${Avatar} agent=${agent} /><span>${agent.name}</span>${elevatedAccess(snapshot.value, agent) && html`<span class="companion-tab-elevated" title="Elevated access">🔓</span>`}<${StatusDot} status=${tabStatus} />
            </button>
            ${agent.steward
              ? html`<button type="button" class="companion-tab-close" aria-label=${`Minimize ${agent.name}`} title="Minimize"
                onClick=${minimize}>×</button>`
              : html`<button type="button" class="companion-tab-close" aria-label=${`Unpin ${agent.name}`} title="Unpin (the agent keeps running)"
                onClick=${() => closeTab(agent.agentId)}>×</button>`}
          </div>`;
        })}
        ${queue.length > 0 && html`<div class=${`companion-tab companion-decide-tab${showQueue ? ' selected' : ''}`}>
          <button type="button" role="tab" aria-selected=${showQueue ? 'true' : 'false'} title="Decisions agents are waiting on"
            onClick=${() => setDeciding((v) => !v)}><span class="companion-decide-mark" aria-hidden="true">◆</span><span>${queue.length} to decide</span></button>
        </div>`}
        <button ref=${addRef} type="button" class="companion-add" aria-label="Add a companion" aria-expanded=${menu ? 'true' : 'false'}
          title=${`Summon ${stewardLabel} or pin a live agent`} onClick=${() => setMenu((v) => !v)}>＋</button>
      </div>
      <div class="companion-controls">
        ${activeElevated && html`<button type="button" class="companion-state-sudo" aria-label=${`${activeAgent.name} has elevated access`}
          title="Elevated access. The band in the window can end it." disabled=${!(activeAgent.steward && activeAgent.online && openSudo)}
          onClick=${() => openSudo(activeAgent.agentId, activeAgent.name)}>🛡 sudo</button>`}
        ${standby > 0 && html`<button type="button" class="companion-state-roster" aria-label=${`${standby} on the Steward roster`}
          title="On the Steward roster" onClick=${() => rosterRef.current?.()}>✦ ${standby}</button>`}
        ${toApprove.length > 0 && html`<button type="button" class="companion-access"
          aria-label=${`${toApprove.length} permission ${toApprove.length === 1 ? 'request' : 'requests'} waiting. Open them`}
          title="Agents are asking for permission"
          onClick=${(event) => openAccessPanel({ launcher: event.currentTarget, documentRef: win?.document })}>🔑 ${toApprove.length}</button>`}
        ${activeAgent && speaker.supported && html`<button type="button" class=${`companion-voice${voiceAgents.has(activeAgent.agentId) ? ' on' : ''}`}
          aria-pressed=${voiceAgents.has(activeAgent.agentId) ? 'true' : 'false'} aria-label=${`Speak ${activeAgent.name}'s replies aloud`}
          title=${voiceAgents.has(activeAgent.agentId) ? 'Voice on: replies are spoken when a turn ends. Typing stops speech.' : 'Speak replies aloud'}
          onClick=${() => toggleVoice(activeAgent)}>${voiceAgents.has(activeAgent.agentId) ? '🔊' : '🔈'}</button>`}
        ${activeAgent?.online && !showQueue && onComposeMessage && html`<button type="button" class="companion-compose" aria-label=${`Message ${activeAgent.name}`} title="Send a queued message (Ctrl/Cmd+M)" onClick=${() => composeToActive()}>✉</button>`}
        <${AgentCog} agent=${activeAgent} deciding=${showQueue} parked=${activeAgent ? parked.has(activeAgent.agentId) : false} roster=${stewardRoster} showRoster=${showRoster}
          onCollab=${openTerminalsPane && activeAgent ? () => moveToTerminals(activeAgent) : null} onFloat=${() => floatTab(activeAgent)}
          onDetach=${detachWindow ? () => detachToWindow(activeAgent) : null} onMorph=${openMorph ? () => openMorph(activeAgent) : null}
          openPermissions=${openPermissions} openSudo=${openSudo} openHandover=${openStewardHandover} agents=${agents} showAgent=${showAgent}
          fetchImpl=${fetchImpl} notify=${notify} onDown=${stewardShutDown} askShutDownRef=${askShutDown} rosterRef=${rosterRef} />
        ${look === 'classic' && html`<button type="button" aria-label="Minimize (Ctrl+.)" title="Minimize (Ctrl+.)" onClick=${minimize}>–</button>`}
      </div>
`;

  const windowView = html`<${OverlayWindow} id="companion" window=${overlay} open=${open} mounted=${open || warm}
    className=${`companion-window${open ? '' : ' companion-window-warm'}`} frameClass="companion-resize-frame"
    titlebarClass=${`companion-titlebar overlay-look-${look}`} label="Companion" controls=${controls}
    lead=${html`<span class="companion-grip" aria-hidden="true">⋮⋮</span>`} title=${titlebarContent}>
    ${menu && html`<${AddMenu} agents=${agents} tabs=${tabs} steward=${steward} busy=${busy}
      stewardLabel=${stewardLabel} returning=${returning} onResumeSteward=${resumeSteward} onSummonSteward=${summonSteward} onPin=${(id) => showAgent(id)} onClose=${() => setMenu(false)} trigger=${addRef} />`}
    <div class=${`companion-body${showComposer && showField && activeApprovals.length + activeDecisions.length + activeActions.length ? ' clear-strip' : ''}`}>
      ${showQueue && html`<div class="companion-decide-queue" role="region" aria-label="To decide">
        ${activeApprovals.length > 0 && html`<p class="companion-decide-note" role="status">${activeAgent.name} also has
          ${activeApprovals.length === 1 ? ' an approval' : ` ${activeApprovals.length} approvals`} waiting:
          <button type="button" onClick=${() => setDeciding(false)}>show</button></p>`}
        ${queue.map((decision) => html`<${DecisionCard} key=${decision.id} decision=${decision} fetchImpl=${fetchImpl} notify=${notify} from=${askerName(decision)} />`)}
      </div>`}
      ${activeApprovals.map((request) => html`<${ApprovalCard} key=${request.id} request=${request} fetchImpl=${fetchImpl} notify=${notify} />`)}
      ${!showQueue && activeDecisions.map((decision) => html`<${DecisionCard} key=${decision.id} decision=${decision} fetchImpl=${fetchImpl} notify=${notify} />`)}
      ${!showQueue && html`<${ActionCards} messages=${activeActions} fetchImpl=${fetchImpl} notify=${notify} />`}
      ${starting && html`<div class="companion-empty" role="status"><p>Starting the agent… its terminal opens here as soon as it is up.</p></div>`}
      ${!activeAgent && !starting && html`<div class="companion-empty">
        <p class="companion-empty-title"><span class="companion-avatar steward" aria-hidden="true">✦</span> ${customName || 'The Steward'}</p>
        ${shutDown && html`<p class="companion-shutdown-note" role="status"><b>${shutDown.name}</b> is shut down. You can pick its conversation back up.</p>`}
        <p>An always-available helper for all of tclaude. Ask what you can do on this page, how your
          groups are set up, or to help you set up groups, profiles, processes or your sandbox.</p>
        <p class="companion-empty-note">Config > Steward sets what it can do without asking. Once it is running, ⚙ in the title bar changes its access.</p>
        ${returning ? html`<button type="button" class="companion-primary companion-resume-steward" disabled=${busy} onClick=${resumeSteward}>${busy ? 'Working…' : `Resume ${returning.name}`}</button>
          <p class="companion-empty-note">${stewardReturnNote(returning)}</p>
          <button type="button" class="companion-secondary companion-fresh-steward" disabled=${busy} onClick=${summonSteward}>Start fresh</button>
          <p class="companion-empty-note">${stewardFreshNote(returning)}</p>`
        : html`<button type="button" class="companion-primary" disabled=${busy} onClick=${summonSteward}>${busy ? 'Summoning…' : `${steward?.online ? 'Open' : 'Summon'} ${stewardLabel}`}</button>`}
        <p class="companion-empty-note">Or press ＋ to pin any live agent here.</p>
      </div>`}
      ${activeElevated && html`<${ElevatedBand} agent=${activeAgent} access=${activeElevated} fetchImpl=${fetchImpl} notify=${notify} />`}
      ${activeAgent && parked.has(activeAgent.agentId) && showComposer && html`<div class="companion-floating-anchor">${messageTools()}</div>`}
      ${activeAgent && !activeAgent.online && !starting && html`<div class="companion-empty">
        <p><b>${activeAgent.name}</b> is offline.</p>
        ${activeAgent.agentId === returning?.agentId ? html`<button type="button" class="companion-primary companion-resume-steward" disabled=${busy} onClick=${resumeSteward}>Resume</button>
          <p class="companion-empty-note">${stewardReturnNote(returning)}</p>
          <button type="button" class="companion-secondary companion-fresh-steward" disabled=${busy} onClick=${summonSteward}>Start fresh</button>
          <p class="companion-empty-note">${stewardFreshNote(returning)}</p>`
        : html`<button type="button" class="companion-primary" onClick=${() => resume(activeAgent)}>Resume</button>`}
      </div>`}
      ${activeAgent && activeAgent.online && parked.has(activeAgent.agentId) && html`<div class="companion-empty">
        <p><b>${activeAgent.name}</b> is open ${detached.has(activeAgent.agentId) ? 'in its own window' : 'in the Collab tab'}.</p>
        <button type="button" class="companion-primary" onClick=${() => reclaim(activeAgent.agentId)}>Bring it back here</button>
      </div>`}
      ${activeAgent && activeAgent.online && !parked.has(activeAgent.agentId) && html`
        ${status === 'needs' && !activeApprovals.length && !activeDecisions.length && html`<div class="companion-needs-bar" role="status">${activeAgent.name} is waiting for you${activeAgent.detail ? `: ${activeAgent.detail}` : ''}</div>`}
        <div class="companion-floating-anchor">
          ${showComposer ? messageTools(terminalZoom) : html`<div class="companion-floating" role="group" aria-label="Terminal tools">${terminalZoom}</div>`}
          ${termStatus !== 'connected' && html`<div class="companion-term-status" role="status">${canReconnect
          ? html`${termStatus || 'disconnected'}. <button type="button" onClick=${() => setGeneration((n) => n + 1)}>Reconnect</button>`
          : termStatus}</div>`}
        </div>
        <${CompanionTerminal} key=${activeAgent.agentId} agent=${activeAgent} generation=${generation}
          widgetFactory=${widgetFactory} loadRuntime=${loadRuntime} focusToken=${focusToken} widgetRef=${terminalWidgetRef}
          fontSize=${() => readTerminalFont('companion', activeAgent.agentId, prefs)} onFontChange=${onTerminalFontChange}
          onStatus=${setStatusFor(activeAgent.agentId)} onReconnectChange=${setReconnectFor(activeAgent.agentId)} />`}
    </div>
    ${showComposer && showField && html`<${DraftField} dictation=${dictation} draft=${draft} sending=${sending}
      label=${`Message ${activeAgent.name}`} fieldRef=${fieldRef} onSubmit=${send}
      onFocus=${() => setFieldFocused(true)} onBlur=${() => setFieldFocused(false)}
      onEdit=${(text) => { speaker.cancel(); setDraft(activeAgent.agentId, text); }} />`}
  <//>`;

  return html`
    ${overlayWindows.value.filter((window) => window.kind === CHAT_WINDOW_KIND).map((instance) => html`<${ChatWindow} key=${instance.id} instance=${instance}
      snapshot=${snapshot} prefs=${prefs} win=${win} fetchImpl=${fetchImpl} notify=${notify} widgetFactory=${widgetFactory} loadRuntime=${loadRuntime}
      closeTerminalPanes=${closeTerminalPanes} onBackToTab=${chatBackToTab} onStewardDown=${stewardShutDown} stewardRoster=${stewardRoster} openPermissions=${openPermissions} openSudo=${openSudo} openStewardHandover=${openStewardHandover} showAgent=${showAgent} Terminal=${CompanionTerminal} DecisionCard=${DecisionCard} StatusDot=${StatusDot} />`)}
    ${overlayWindows.value.filter((window) => window.kind === TERMINAL_WINDOW_KIND).map((instance) => html`<${TerminalWindow} key=${instance.id} instance=${instance}
      snapshot=${snapshot} prefs=${prefs} win=${win} notify=${notify} widgetFactory=${widgetFactory} loadRuntime=${loadRuntime} Terminal=${CompanionTerminal} />`)}
    ${windowView}
    ${chipSlots}
    ${chips}
    ${annotation.overlay}
    ${!open && needsCount > 0 && html`<span class="companion-sr" role="status">${needsCount} companion${needsCount === 1 ? '' : 's'} need you</span>`}
    <${CompanionViewChannel} activeTab=${activeTab} notify=${notify} fetchImpl=${fetchImpl} win=${win} />
    ${guideCue && html`<${GuideOverlay} key=${guideCue.cue.id} cue=${guideCue.cue} element=${guideCue.element} marks=${guideCue.marks} onDismiss=${dismissGuide} win=${win} />`}
    ${!open && html`<${Peek} agent=${peekTarget} lines=${peek?.lines ?? null} anchor=${peek?.anchor} />`}`;
}

// CompanionGate keeps the whole companion out of the page while
// features.collab is off: no chips, no Ctrl+., and no registered controller,
// so callers of openInCompanion take their usual path. A peer's page carries a
// peer snapshot, which never says collab is on, and shows that node's agents,
// so it gets no window; but the operator is still looking at this node's
// dashboard, so this node's agents can capture and mark it. The channel's route
// answers 404 here while this node has Collab off.
export function CompanionGate(props) {
  if (props.snapshot.value?.collab_enabled === true) return html`<${CompanionApp} ...${props} />`;
  if (!globalThis.__tclaudeRemoteNode?.id) return null;
  return html`<${CompanionViewChannel} activeTab=${props.activeTab} notify=${props.notify} fetchImpl=${props.fetchImpl} win=${props.win} />`;
}

export function mountCompanionIsland({ host, registerCleanup, ...props }) {
  render(html`<${CompanionGate} ...${props} />`, host);
  registerCleanup(() => render(null, host));
}
