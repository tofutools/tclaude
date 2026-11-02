// companion-island.js -- the dashboard companion (tcl-6i0283): the Steward's chip
// and the agents the operator keeps at hand, over every dashboard view. See
// companion-model.js for the concept. An agent here is a chat tab of an ordinary
// overlay window (chat-window.js), so this holds what is shared between them.
//
//   chips   the Steward's pill, with what needs the operator but belongs to no
//           one tab: decisions, and permission requests. Bottom-right by default,
//           and movable. It opens the window holding the Steward. Any other
//           window minimizes to a dock pill.
//   peek    hovering a chip or a chat's pill previews the last lines of the
//           agent's pane, read-only (GET /api/companion/{agent}/peek): the
//           keyboard stays with the page until the operator clicks.
//   landing no window is home: a tab nobody placed joins the Steward's window,
//           its start tab's, or the chats used last, or else gets its own.
//
// A web terminal attach displaces any other web view of the same session, so
// showing an agent here first closes its Terminals-tab pane, and "open in
// Terminals" closes this tab instead of fighting over the session.

import { h, render } from 'preact';
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  COMPANION_ACTIVE_PREF, COMPANION_CHIP_ROOM_SPOT_PREF, COMPANION_CHIP_SPOT_PREF, COMPANION_CLOSED_PREF, COMPANION_DETACHED_PREF, COMPANION_CUE_OPEN_PREF, COMPANION_OPEN_PREF, COMPANION_PARKED_PREF, COMPANION_PLACEMENT_PREF, COMPANION_SEEDED_PREF, COMPANION_TABS_PREF, COMPANION_WINDOW_SPOT_PREF, DEFAULT_CHIP_SPOT, DEFAULT_ROOM_CHIP_SPOT,
  companionAgents, elevatedAccess, companionStatus, companionStatusLabel,
  FOOTER_CHIP_SPOTS, defaultPlacement, footerSpotFits, footerSpotLeft, isAgentId, isFooterSpot, lastRoomSpot, parseChipSpot, parseTabs, peekAnchor, spotPlacement, stewardAgent, stewardToResume,
  parseClosedHelpers, syncHelperTabs, viewContext, approvalsFor, decisionsFor, decisionSpeech, pendingApprovals, pendingDecisions,
  companionAgentOf,
} from './companion-model.js';
import { useChipDrag } from './companion-chip-drag.js';
import { chipReserve, compactWidth, footerChipReserve, measureFooterRoom, useFooterRest } from './companion-chip-footer.js';
import { registerCompanionController } from './companion-facade.js';
import { DecideTab, StartTab, stewardFreshNote, stewardReturnNote } from './companion-home.js';
import { elementScreenPoint } from './terminal-drag-out.js';
import { overlayWindowsByStack, readPref, reducedMotion, setSnapAssistEnabled, writePref } from './overlay-window.js';
import { registerTabMoves } from './overlay-tab-moves.js';
import { activateOverlayTab, closeOverlayTab, focusOverlayWindow, minimizeOverlayWindow, openOverlayWindow, openTabInWindow, overlayHostUp, overlayTabs, overlayWindows, swapOverlayTab, tearOverlayTabOut } from './overlay-instances.js';
import { CHAT_WINDOW_KIND, DECIDE_WINDOW_KIND, START_WINDOW_KIND, chatTab, chatWindowId, decideTab, startTab } from './chat-window-model.js';
import { OverlayTabGhost, OverlayTabMenu } from './overlay-tab-strip.js';
import { chatHasDraft } from './chat-window.js';
import { OverlayTabWindow, tabViewOf } from './overlay-tab-window.js';
import { SnapAssist } from './overlay-snap-assist.js';
import { activeTabOf, windowHolding } from './overlay-tab-model.js';
import { BOARD_WINDOW_KIND } from './board-overlay-state.js';
import { PROCESS_WINDOW_KIND } from './process-editor-place.js';
import { TERMINAL_WINDOW_KIND } from './terminal-window-model.js';
import { viewportOf } from './overlay-window-model.js';
import { CompanionViewChannel } from './companion-view-channel.js';
import { GUIDE_FIND_STEP_MS, GUIDE_FIND_TIMEOUT_MS, GUIDE_MARK_PATIENCE_MS, GUIDE_SETTLE_MS, GUIDE_SHOW_MS, CUE_OPEN_UNFOCUSED_DELAY_MS, GuideOverlay, claimCueLock, claimCueOpen, cueOpenClaimed, cueOpenDue, cuePath, findGuideTarget, newCues } from './companion-guide.js';
import { openAccessPanel, requestStewardSummon, resumeAgent } from './companion-approval.js';
import { hasShownOverlay } from './overlay-stack.js';
import { overlayExposeOpen } from './overlay-expose.js';
import { isComposeMessageShortcut } from './terminal-interactions.js';
import { useAnnotation, useDictation } from './dictation-tools.js';
import { registerStewardDoor } from './voice-wake.js';
import { VOICE_PREF, createSpeaker, dictationOpen, createVoiceReader, parseVoiceAgents, speakableText, decisionArrivals, voiceTransitions } from './companion-voice.js';
import { terminalAttachWidgetOptions } from './terminal-attach-config.js';
import { useDismissablePopover } from './hover-popover.js';
import { matchesShortcut, shortcutText, withShortcut } from './keybindings.js';

const html = htm.bind(h);

const PEEK_DELAY_MS = 220;
const PENDING_MS = 30000;

const PEEK_REFRESH_MS = 1500;

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
  const id = target?.closest?.('.chat-window')?.querySelector?.('[data-chat-agent]')?.dataset?.chatAgent;
  return id ? companionAgents(snapshotValue).get(id) || null : null;
}

// How long the Steward's window has to come up after its phrase.
const STEWARD_WAKE_MS = 3000;

// How recently a key in the terminal counts as still typing when a cue lands.
const CUE_TYPING_WINDOW_MS = 3000;

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

// AddTab is a window's ＋: what the operator can bring into it.
function AddTab({ window, ...menu }) {
  const [shown, setShown] = useState(false);
  const trigger = useRef(null);
  return html`<button ref=${trigger} type="button" class="companion-add" aria-label="Add a companion" aria-expanded=${shown ? 'true' : 'false'}
    title=${`Summon ${menu.stewardLabel} or pin a live agent`} onClick=${() => setShown((v) => !v)}>＋</button>
    ${shown && html`<${AddMenu} ...${menu} onPin=${(id) => { setShown(false); menu.onPin(id); }}
      onResumeSteward=${() => { setShown(false); menu.onResumeSteward(); }} onSummonSteward=${() => { setShown(false); menu.onSummonSteward(); }}
      onClose=${() => setShown(false)} trigger=${trigger} />`}`;
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
  const sessionPrefs = useMemo(() => {
    if (sessionPrefsImpl) return sessionPrefsImpl;
    try { return win?.sessionStorage ?? null; } catch { return null; }
  }, [win, sessionPrefsImpl]);
  // What a page saved before the Steward moved into a window comes back as one,
  // and so does what the operator keeps at hand, in a dashboard tab new to it. A
  // tab an older page parked for the Terminals tab is not restored: the pane has
  // the session, and the tab would only take it back.
  useState(() => {
    if (readPref(sessionPrefs, COMPANION_SEEDED_PREF) === '1') return;
    writePref(sessionPrefs, COMPANION_SEEDED_PREF, '1');
    const inWindows = new Set(parseTabs(readPref(sessionPrefs, COMPANION_DETACHED_PREF)));
    const gone = new Set(parseTabs(readPref(sessionPrefs, COMPANION_PARKED_PREF)).filter((id) => !inWindows.has(id)));
    const held = parseTabs(readPref(prefs, COMPANION_TABS_PREF), Infinity).filter((id) => !gone.has(id) && agents.has(id) && !overlayTabs.peek().some((tab) => tab.id === chatWindowId(id)));
    const saved = readPref(sessionPrefs, COMPANION_ACTIVE_PREF) || readPref(prefs, COMPANION_ACTIVE_PREF);
    const shown = held.includes(saved) ? saved : held[0];
    const into = held[0] ? chatWindowId(held[0]) : '';
    for (const id of held) openTabInWindow(into, COMPANION_PLACEMENT_PREF, chatTab(id, agents.get(id).name), { activate: false });
    if (shown && readPref(sessionPrefs, COMPANION_OPEN_PREF) === '1') openOverlayWindow(chatTab(shown));
    else if (shown) activateOverlayTab(chatWindowId(shown));
  });
  const [chipSpot, setChipSpot] = useState(() => parseChipSpot(readPref(prefs, COMPANION_CHIP_SPOT_PREF)));
  // A window opened from the chips, which saves its placement as the companion
  // did, opens at their spot until the operator moves it.
  const homeOverlay = useRef(null);
  const chipRects = useRef(new Map());
  const home = {
    place: COMPANION_PLACEMENT_PREF,
    spotted: (base, viewport, top) => {
      const spot = readPref(prefs, COMPANION_WINDOW_SPOT_PREF);
      return spot ? spotPlacement(parseChipSpot(spot), base, viewport, top) : null;
    },
    onPlaced: () => writePref(prefs, COMPANION_WINDOW_SPOT_PREF, ''),
    // The window holding the Steward plays down into its chip.
    shrinks: (window, overlay) => {
      if (!window.tabs.some((tab) => tab.agent === steward?.agentId)) return null;
      homeOverlay.current = overlay;
      return () => overlay.shrinkTo(chipRects.current.get(steward.agentId), () => minimizeOverlayWindow(window.active));
    },
  };
  const [busy, setBusy] = useState(false);
  // The Steward just shut down, { agentId, name }. It reads as gone from here
  // on: the snapshot can still show it online for a poll.
  const [shutDown, setShutDown] = useState(null);
  const [detached, setDetached] = useState(() => new Set(parseTabs(readPref(sessionPrefs, COMPANION_DETACHED_PREF))));
  const detachedRef = useRef(detached);
  detachedRef.current = detached;
  const detachedWindows = useRef(new Map());
  // An agent parked for a browser window of it, which keeps the way back.
  const [parked, setParked] = useState(() => new Set(parseTabs(readPref(sessionPrefs, COMPANION_PARKED_PREF)).filter((id) => detached.has(id))));
  const [peek, setPeek] = useState(null); // {agentId, anchor, lines}
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
  // Agents just summoned or opened but not yet in a snapshot: kept here (and
  // shown as starting) until a snapshot lists them or PENDING_MS passes.
  const pending = useRef(new Map());
  // Agents just resumed: the snapshot still shows them offline, so they read as
  // starting until it says online or PENDING_MS passes.
  const [resuming, setResuming] = useState(() => new Set());
  useLayoutEffect(() => {
    overlayHostUp.value = true;
    return () => { overlayHostUp.value = false; };
  }, []);
  // An agent is here as a chat tab, of a window or on the dock.
  const chats = overlayTabs.value.filter((tab) => tab.kind === CHAT_WINDOW_KIND);
  const held = chats.map((tab) => tab.agent);
  const heldKey = held.join();
  const heldHere = (id) => held.includes(id);
  const holder = windowHolding(overlayWindows.value, steward ? chatWindowId(steward.agentId) : '');
  // With no Steward's chat here the chip opens the start tab, shown just the same.
  const startHolder = windowHolding(overlayWindows.value, startTab().id);
  const stewardShown = holder
    ? !holder.minimized && activeTabOf(holder)?.agent === steward.agentId
    : !!startHolder && !startHolder.minimized && activeTabOf(startHolder)?.kind === START_WINDOW_KIND;
  // Where a tab nobody placed goes: in with the Steward, its start tab, or the
  // chats the operator was in last. No window is home: with none of those, the
  // tab gets a window of its own.
  const landing = holder || startHolder || null;
  const landingId = () => {
    const windows = overlayWindows.peek();
    const stack = overlayWindowsByStack();
    const chats = windows.filter((window) => window.tabs.some((tab) => tab.kind === CHAT_WINDOW_KIND));
    const recent = chats.sort((a, b) => stack.indexOf(b.id) - stack.indexOf(a.id))[0];
    return windowHolding(windows, steward ? chatWindowId(steward.agentId) : '')?.id || windowHolding(windows, startTab().id)?.id || recent?.id || '';
  };
  // The chat showing at the front, for what acts on whichever agent is in view.
  const shownChats = overlayWindows.value.filter((window) => !window.minimized).map((window) => activeTabOf(window)).filter((tab) => tab?.kind === CHAT_WINDOW_KIND);
  const frontAgent = () => agents.get(shownChats.at(-1)?.agent) || null;
  // The Steward phrase. The dictation only starts once the Steward's tab shows, so
  // no word can land on the agent that had the focus, and the mic stays reserved
  // until then so the wake listener doesn't take it back.
  const doorMic = useDictation({ win, targetId: '', draft: '', setDraft: () => {}, notify, dictationImpl, send: () => {} });
  const mics = useRef(new Map());
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
      return '';
    },
    reserve: () => doorMic.reserve(),
    open(words, carry) {
      doorMic.reserve();
      clearTimeout(stewardWake.current?.timer);
      const timer = setTimeout(() => {
        stewardWake.current = null;
        doorMic.release();
        notify(`Could not open ${stewardLabel}, so nothing is listening.`, true);
      }, STEWARD_WAKE_MS);
      stewardWake.current = { agentId: steward.agentId, words, carry, timer };
      showAgent(steward.agentId);
      wakeWhenShown();
    },
  };
  function wakeWhenShown() {
    const wake = stewardWake.current;
    const mic = wake && mics.current.get(wake.agentId);
    if (!mic) return;
    stewardWake.current = null;
    clearTimeout(wake.timer);
    mic.handover(wake.words, wake.carry);
    doorMic.release();
  }
  const onMic = (agentId, mic) => {
    mics.current.set(agentId, mic);
    wakeWhenShown();
    return () => { if (mics.current.get(agentId) === mic) mics.current.delete(agentId); };
  };
  useEffect(() => registerStewardDoor({
    refusal: () => wakeDoor.current.refusal(),
    reserve: () => wakeDoor.current.reserve(),
    open: (words, carry) => wakeDoor.current.open(words, carry),
  }), []);

  useEffect(() => writePref(prefs, COMPANION_TABS_PREF, JSON.stringify(held)), [heldKey]);
  useEffect(() => writePref(sessionPrefs, COMPANION_DETACHED_PREF, JSON.stringify([...detached])), [detached, sessionPrefs]);
  useEffect(() => writePref(sessionPrefs, COMPANION_PARKED_PREF, JSON.stringify([...parked])), [parked, sessionPrefs]);

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
  // However an agent stops being held here, voice stops with it, as Unpin says,
  // and a helper the operator let go of stays gone.
  const heldBefore = useRef(new Set());
  const ended = useRef(new Set());
  useEffect(() => {
    const now = new Set(held);
    for (const id of heldBefore.current) {
      if (now.has(id)) continue;
      silence(id);
      if (agents.get(id)?.scribe && !ended.current.has(id)) setHelperClosed(id, true);
      ended.current.delete(id);
    }
    heldBefore.current = now;
  }, [heldKey]);

  const shownKey = shownChats.map((tab) => tab.agent).join();
  useEffect(() => {
    if (!snapshot.value) return;
    const keep = new Set([...pending.current.keys(), ...resuming, ...shownChats.map((tab) => tab.agent)]);
    const next = syncHelperTabs(held, agents, closedList(), keep);
    if (next === held) return;
    for (const id of held) {
      if (next.includes(id)) continue;
      ended.current.add(id);
      closeOverlayTab(chatWindowId(id));
    }
    for (const id of next) if (!held.includes(id)) openTabInWindow(landingId(), COMPANION_PLACEMENT_PREF, chatTab(id, agents.get(id).name), { activate: false });
  }, [agents, snapshot.value, heldKey, shownKey, resuming]);

  // An agent that was retired drops out of the snapshot; forget it once the
  // snapshot is known so nothing shows a dead handle forever.
  useEffect(() => {
    if (!snapshot.value) return;
    const now = Date.now();
    for (const [id, until] of pending.current) {
      if (agents.has(id) || until < now) pending.current.delete(id);
    }
    for (const id of held) if (!agents.has(id) && !pending.current.has(id)) closeOverlayTab(chatWindowId(id));
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

  const putStart = () => openTabInWindow(landingId(), COMPANION_PLACEMENT_PREF, startTab());
  // A detached agent stays in its own window: its tab only shows where it went.
  // Attaching here would throw that window off the terminal, and the window has
  // no idea why. Only Bring it back here reclaims it.
  const showAgent = useCallback((agentId, { focus = true, starting = false, reclaim = false, into = null } = {}) => {
    if (!isAgentId(agentId)) return false;
    const agent = agents.get(agentId);
    const tab = chatTab(agentId, agent?.name);
    // An id the snapshot does not know is only accepted for an agent this
    // window just started; other callers keep their own fallback.
    if (!agent && !starting && !pending.current.has(agentId) && !overlayTabs.peek().some((other) => other.id === tab.id)) return false;
    if (!agent && starting) pending.current.set(agentId, Date.now() + PENDING_MS);
    setHelperClosed(agentId, false);
    stopPeek();
    if (reclaim || !detachedRef.current.has(agentId)) {
      closeTerminalPanes([agentId, agent?.convId].filter(Boolean));
      setParked((current) => without(current, agentId));
      setDetached((current) => without(current, agentId));
    }
    openTabInWindow(into ?? landingId(), COMPANION_PLACEMENT_PREF, tab);
    if (focus) focusOverlayWindow(tab.id);
    if (overlayTabs.peek().some((other) => other.id === startTab().id)) closeOverlayTab(startTab().id);
    return true;
  }, [agents, closeTerminalPanes]);

  // A tab of the Terminals tab moves here too.
  const floatAgent = useRef(null);
  floatAgent.current = (selector, at) => {
    const agent = companionAgentOf(snapshot.peek(), selector);
    if (!agent) return false;
    floatTab(agent, at);
    return true;
  };
  const reclaimAgent = useRef(null);
  reclaimAgent.current = (selector) => {
    const agentId = companionAgentOf(snapshot.peek(), selector)?.agentId || selector;
    if (!parked.has(agentId)) return false;
    reclaim(agentId);
    return true;
  };
  const revealAgent = useRef(null);
  revealAgent.current = (selector, { tabs: orTab }) => {
    const agentId = companionAgentOf(snapshot.peek(), selector)?.agentId || selector;
    if (detachedRef.current.has(agentId)) {
      const opened = detachedWindows.current.get(agentId);
      if (!opened) return showAgent(agentId);
      opened.focus?.();
      return true;
    }
    const chat = overlayTabs.peek().find((tab) => tab.id === chatWindowId(agentId));
    // One not showing has let go of the session, which a Terminals pane may have.
    const showing = overlayWindows.peek().some((window) => !window.minimized && window.active === chat?.id);
    if (!chat || (!showing && !orTab)) return false;
    openOverlayWindow(chat);
    focusOverlayWindow(chat.id);
    return true;
  };
  const peekRef = useRef(null);
  useEffect(() => registerController({
    open: (agentId, options) => showAgent(agentId, options),
    float: (selector, at) => floatAgent.current(selector, at),
    reclaim: (selector) => reclaimAgent.current(selector),
    reveal: (selector, options) => revealAgent.current(selector, options),
    peek: (agentId, element) => peekRef.current?.(agentId, element),
    endPeek: () => stopPeek(),
  }), [showAgent]);
  // A pill peeks upwards from its right edge, in the footer as over the room.
  peekRef.current = (agentId, element) => { if (agents.has(agentId)) startPeek(agentId, element, element?.closest?.('footer') ? 'footer-right' : DEFAULT_ROOM_CHIP_SPOT); };

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

  const silence = (agentId) => setVoiceAgents((current) => without(current, agentId));

  // floatTab gives an agent a window of its own, at at if it was dragged there.
  function floatTab(agent, at = null) {
    // A browser window of it goes, as Bring it back here closes one: it would
    // lose the session to this window with no idea why.
    const opened = detachedWindows.current.get(agent.agentId);
    detachedWindows.current.delete(agent.agentId);
    if (opened) setTimeout(() => opened.close?.(), RECLAIM_CLOSE_MS);
    setParked((current) => without(current, agent.agentId));
    setDetached((current) => without(current, agent.agentId));
    const tab = chatTab(agent.agentId, agent.name);
    const window = windowHolding(overlayWindows.peek(), tab.id);
    if (window?.tabs.length > 1) tearOverlayTabOut(tab.id, at);
    else openOverlayWindow({ ...tab, at });
  }

  // The tab stays on the shut-down state, with the way to start again. A detached
  // window of it goes too: it has nothing left to show.
  // Closing is the window's ✕ asking: then the tab goes as a close does, and
  // Resume waits behind ✦ Steward.
  function stewardShutDown(agent, { closing = false } = {}, tabId = chatWindowId(agent.agentId)) {
    setResuming((current) => without(current, agent.agentId));
    detachedWindows.current.get(agent.agentId)?.close?.();
    detachedWindows.current.delete(agent.agentId);
    setParked((current) => without(current, agent.agentId));
    setDetached((current) => without(current, agent.agentId));
    // A window already offering the way back only loses the tab.
    if (closing || overlayTabs.peek().some((tab) => tab.id === startTab().id)) closeOverlayTab(tabId);
    else swapOverlayTab(tabId, startTab());
    setShutDown({ agentId: agent.agentId, name: agent.name });
    notify(`${agent.name} is shut down.`);
  }

  // Ctrl+. and the chip bring up the window holding the Steward, or the one that
  // would, and Ctrl+. puts it away again.
  const toggle = useCallback(() => {
    stopPeek();
    // The Steward's own window, or its start tab's. A worker's window only stands
    // in while there is no Steward at all, as the chats kept at hand.
    const windows = overlayWindows.peek();
    const window = windowHolding(windows, steward ? chatWindowId(steward.agentId) : '') || windowHolding(windows, startTab().id)
      || (!steward && windows.find((candidate) => candidate.id === landingId())) || null;
    const docked = !holder && !!steward && overlayTabs.peek().some((tab) => tab.id === chatWindowId(steward.agentId));
    if (window && !window.minimized && !docked) {
      const shrinks = home.shrinks(window, homeOverlay.current);
      if (shrinks && homeOverlay.current) shrinks();
      else minimizeOverlayWindow(window.active);
      return;
    }
    if (steward && (holder || steward.online) && steward.agentId !== shutDown?.agentId) showAgent(steward.agentId);
    else if (window) { openOverlayWindow(activeTabOf(window)); focusOverlayWindow(window.active); } else putStart();
  }, [holder?.id, holder?.minimized, steward?.agentId, steward?.online, shutDown, showAgent]);

  // Ctrl . toggles the window from anywhere, including from inside a focused
  // terminal: capture phase, before xterm sees the key.
  useEffect(() => {
    const onKey = (event) => {
      // Show all windows covers the companion's window and marks: Escape first.
      if (overlayExposeOpen.peek()) return;
      if (matchesShortcut(event, 'steward')) {
        event.preventDefault();
        event.stopPropagation();
        toggle();
      } else if (matchesShortcut(event, 'annotate')) {
        // Ctrl Shift . marks up the page for the agent in the window.
        event.preventDefault();
        event.stopPropagation();
        startAnnotating(chatOf(event.target) || frontAgent());
      }
    };
    win?.addEventListener?.('keydown', onKey, true);
    return () => win?.removeEventListener?.('keydown', onKey, true);
  }, [toggle, win, shownKey, agents]);

  function moveChips(spot) {
    setChipSpot(spot);
    writePref(prefs, COMPANION_CHIP_SPOT_PREF, spot);
    if (isFooterSpot(spot)) footerRest.measure();
    const roomSpot = lastRoomSpot(spot, shownSpot);
    if (roomSpot) writePref(prefs, COMPANION_CHIP_ROOM_SPOT_PREF, roomSpot);
    writePref(prefs, COMPANION_WINDOW_SPOT_PREF, spot === DEFAULT_CHIP_SPOT ? '' : spot);
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
  function startPeek(agentId, chip, spot = shownSpot) {
    stopPeek();
    peekAgent.current = agentId;
    peekTimer.current = setTimeout(() => {
      const rect = chip.getBoundingClientRect();
      const vp = viewportOf(win);
      setPeek({ agentId, lines: null, anchor: peekAnchor(spot, rect, vp) });
      void readPeek(agentId);
      peekRefresh.current = setInterval(() => void readPeek(agentId), PEEK_REFRESH_MS);
    }, PEEK_DELAY_MS);
  }

  // A row saved in the footer rests at its last spot in the room when it doesn't fit.
  const chipRow = useRef(null);
  const footerRest = useFooterRest({ win, watching: isFooterSpot(chipSpot) && !stewardShown, row: chipRow, spot: chipSpot });
  const restSpot = () => { const spot = parseChipSpot(readPref(prefs, COMPANION_CHIP_ROOM_SPOT_PREF)); return isFooterSpot(spot) ? DEFAULT_ROOM_CHIP_SPOT : spot; };
  const shownSpot = isFooterSpot(chipSpot) && !footerRest.fits ? restSpot() : chipSpot;
  const inFooter = isFooterSpot(shownSpot);
  // The row saved in the footer keeps its room there, whether it fits now or not:
  // the overlay dock gives way first.
  const reserve = isFooterSpot(chipSpot) && !stewardShown ? chipReserve(footerRest.width) : 0;
  useEffect(() => {
    footerChipReserve.value = { width: reserve, spot: reserve ? chipSpot : '' };
  }, [reserve, chipSpot]);
  useEffect(() => () => { footerChipReserve.value = { width: 0, spot: '' }; }, []);
  const footerSlots = (node) => {
    const width = compactWidth(node);
    return FOOTER_CHIP_SPOTS.map((spot) => ({ spot, room: measureFooterRoom(win, spot, width) }))
      .filter(({ room }) => footerSpotFits(room, width)).map(({ spot, room }) => ({ spot, left: footerSpotLeft(spot, room, width), width }));
  };
  const chipDrag = useChipDrag({ win, spot: shownSpot, row: chipRow, extraSlots: footerSlots, onDrop: moveChips, onStart: stopPeek, reducedMotion: () => reducedMotion(win) });

  function onChipClick(agentId, event) {
    stopPeek();
    // Asking for permission is answered where it is quickest, in the panel.
    if (agentId && approvalsFor(snapshot.value, agents.get(agentId)).length) {
      openAccessPanel({ agentId, launcher: event.currentTarget, documentRef: win?.document });
      return;
    }
    if (homeOverlay.current) homeOverlay.current.openFrom.current = event.currentTarget.getBoundingClientRect();
    if (agentId) showAgent(agentId);
    else putStart();
  }

  useEffect(() => {
    const pinned = [...voiceAgents].filter(heldHere);
    writePref(prefs, VOICE_PREF, JSON.stringify(pinned));
    for (const id of pinned) voiceReader.prime(id).catch(() => {});
  }, [voiceAgents, voiceReader, heldKey]);

  // Speak when a voice agent's turn ends, or when it starts waiting on the
  // operator. Only transitions speak; the first sight of an agent is silent.
  // Voice applies to pinned tabs only. Several dashboard tabs see the same
  // transition, so each utterance is claimed in shared prefs first and the
  // first tab to claim it speaks.
  useEffect(() => {
    if (!speaker.supported) return;
    const next = new Map();
    const quiet = () => dictationOpen(); // never speak into an open mic
    for (const id of voiceAgents) {
      const agent = agents.get(id);
      if (agent && heldHere(id)) next.set(id, statusOf(agent));
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
  }, [agents, voiceAgents, heldKey, snapshot.value]);

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

  // The windows and chips stay mounted (the terminal keeps its stream) but
  // step out of the way while the operator draws.
  const annotation = useAnnotation({
    win, notify,
    viewNow: () => viewContext({ activeTab: activeTab.value, pathname: win?.location?.pathname, search: win?.location?.search }),
  });

  function startAnnotating(agent) {
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
    try {
      if (await resume(returning)) {
        setShutDown(null);
        showAgent(returning.agentId, { starting: true });
      }
    } finally {
      setBusy(false);
    }
  }

  // The window has one attach to the terminal, so the tab lets go first, and stays
  // as the way back. A blocked pop-up leaves everything where it was.
  function detachToWindow(agent, rect) {
    const size = rect ? { width: rect.width, height: rect.height } : null;
    const opened = detachWindow(agent.agentId, agent.name, { harness: agent.harness, size, at: elementScreenPoint(win, rect) });
    if (!opened) {
      notify('Could not detach: allow pop-ups for this dashboard.', true);
      return false;
    }
    detachedWindows.current.set(agent.agentId, opened);
    setParked((current) => new Set(current).add(agent.agentId));
    setDetached((current) => new Set(current).add(agent.agentId));
    return true;
  }

  function chatToTerminals(instance, agent) {
    silence(agent.agentId);
    closeOverlayTab(instance.id);
    // Let the window's widget detach before the Terminals pane attaches.
    return new Promise((resolve) => {
      setTimeout(() => resolve(openTerminalsPane(agent.agentId, agent.name, agent.harness)), 60);
    });
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

  function composeTo(agent, restoreFocus) {
    if (!agent?.online) return false;
    onComposeMessage?.({ agent: agent.agentId, label: agent.name, restoreFocus });
    return true;
  }
  const composeRef = useRef(composeTo);
  composeRef.current = composeTo;
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
      const chat = event.target?.closest?.('.chat-window');
      if (!chat || hasShownOverlay()) return;
      if (composeRef.current(chatOf(event.target), () => chat.querySelector('.companion-term-host textarea')?.focus?.())) {
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
  // typing or dictating to a tab it only joins the tabs, never showing: that would
  // take the terminal they are typing in, or the draft they are writing, away from
  // under them.
  function openForCue(cue) {
    const agent = agents.get(cue.open);
    if (!agent) return;
    const by = cue.agent_name || 'An agent';
    // Focus alone is no signal: it stays in the terminal long after the last key.
    const typing = Date.now() - lastTerminalKey.current < CUE_TYPING_WINDOW_MS;
    const chatting = overlayWindows.peek().some((window) => !window.minimized && chatHasDraft(window.active));
    if (chatting || dictationOpen() || typing) {
      openTabInWindow(landingId(), COMPANION_PLACEMENT_PREF, chatTab(agent.agentId, agent.name), { activate: false });
      notify(`${by} pinned ${agent.name} in a tab for you.`, false);
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
  const chipAgent = steward && (heldHere(steward.agentId) || (steward.online && steward.agentId !== shutDown?.agentId)) ? steward : null;
  const heldAgents = held.map((id) => agents.get(id)).filter(Boolean);
  const queue = pendingDecisions(snapshot.value);
  const toApprove = pendingApprovals(snapshot.value);
  // A tab showing answers its own agent's requests, and the Steward's chip its
  // own, so a chip counts the rest.
  const answered = [chipAgent, ...shownChats.map((tab) => agents.get(tab.agent))].filter(Boolean);
  const elsewhere = toApprove.filter((request) => !answered.some((agent) => approvalsFor({ access_requests: [request] }, agent).length));
  const askerName = (d) => agents.get(d.from_agent)?.name || d.from_title || 'an agent';
  const hidden = heldAgents.filter((agent) => !shownChats.some((tab) => tab.agent === agent.agentId));
  const needsCount = hidden.filter((agent) => statusOf(agent) === 'needs').length;
  const peekTarget = peek ? agents.get(peek.agentId) : null;

  // The queue is a tab of the Steward's window while that shows, as the chips are not.
  const queueWindow = landing?.id || '';
  const queueUp = !!landing && !landing.minimized;
  useEffect(() => {
    const tab = decideTab();
    const there = overlayTabs.peek().some((other) => other.id === tab.id);
    if (!queue.length && there) closeOverlayTab(tab.id);
    if (queue.length && !there && queueUp) openTabInWindow(queueWindow, COMPANION_PLACEMENT_PREF, tab, { activate: false });
  }, [queue.length, queueUp, queueWindow]);

  const chipSlots = !stewardShown && chipDrag.slots && chipDrag.slots.map(({ spot, near, width, height, left }) => html`<div key=${spot}
    class=${`companion-chip-slot companion-spot-${spot}${near ? ' near' : ''}`} data-chip-spot=${spot}
    style=${`width:${width}px;${left === undefined ? `height:${height}px` : `left:${left}px`}`} aria-hidden="true"></div>`);

  const chips = !stewardShown && html`<div ref=${chipRow} class=${`companion-chips companion-spot-${shownSpot}${inFooter ? ' compact' : ''}${chipDrag.slots ? ' dragging' : ''}`}
    style=${inFooter ? `left:${footerSpotLeft(shownSpot, footerRest.room, footerRest.width)}px` : null}
    role="toolbar" aria-label="Companions" onPointerDown=${chipDrag.onPointerDown} onClickCapture=${chipDrag.onClickCapture}>
    ${chipAgent && (() => {
      const own = statusOf(chipAgent);
      // Its window has no pill, so the chip speaks for the tabs waiting out of
      // sight. A prompt or an approval holds an agent up, and a decision need not,
      // so one never hides behind the other.
      const unseen = (holder?.tabs || []).filter((tab) => tab.kind === CHAT_WINDOW_KIND && (holder.minimized || tab.id !== holder.active)).map((tab) => agents.get(tab.agent)).filter((agent) => agent && agent !== chipAgent);
      const stopped = (agent) => approvals(agent).length > 0 || companionStatus(agent) === 'needs';
      const behind = stopped(chipAgent) ? null : unseen.find(stopped) || (own === 'needs' ? null : unseen.find((agent) => statusOf(agent) === 'needs'));
      const status = behind ? 'needs' : own;
      const peekAt = (behind || chipAgent).agentId;
      return html`<button key=${chipAgent.agentId} type="button"
        class=${`companion-chip ${status}`}
        ref=${(node) => { if (node) chipRects.current.set(chipAgent.agentId, node.getBoundingClientRect()); }}
        aria-label=${behind ? `${behind.name}: ${companionStatusLabel('needs')}, in the window of ${chipAgent.name}. Open it` : `${chipAgent.name}: ${companionStatusLabel(status)}. Open companion (${shortcutText('steward')})`}
        onMouseEnter=${(event) => startPeek(peekAt, event.currentTarget)}
        onMouseLeave=${stopPeek}
        onFocus=${(event) => startPeek(peekAt, event.currentTarget)}
        onBlur=${stopPeek}
        onClick=${(event) => onChipClick(peekAt, event)}>
        <${Avatar} agent=${chipAgent} />
        <span class="companion-chip-name">${behind ? behind.name : chipAgent.name}</span>
        ${elevatedAccess(snapshot.value, chipAgent) && html`<span title="Elevated access">🔓</span>`}
        ${status === 'needs' ? html`<span class="companion-chip-needs">${behind ? 'needs you' : approvals(chipAgent).length ? '🔑 approval' : decisions(chipAgent).length ? 'decide' : 'needs you'}</span>` : html`<${StatusDot} status=${status} />`}
      </button>`;
    })()}
    ${queue.length > 0 && html`<button type="button" class="companion-chip companion-chip-decide"
      aria-label=${`${queue.length} ${queue.length === 1 ? 'decision' : 'decisions'} to make. Open them`}
      title="Agents are waiting for you to choose"
      onClick=${() => { stopPeek(); openTabInWindow(queueWindow, COMPANION_PLACEMENT_PREF, decideTab()); }}>
      <span class="companion-decide-mark" aria-hidden="true">◆</span><span class="companion-chip-name">${queue.length} to decide</span>
    </button>`}
    ${elsewhere.length > 0 && html`<button type="button" class="companion-chip companion-chip-decide"
      aria-label=${`${elsewhere.length} permission ${elsewhere.length === 1 ? 'request' : 'requests'} from other agents. Open them`}
      title="Agents are asking for permission"
      onClick=${(event) => { stopPeek(); openAccessPanel({ launcher: event.currentTarget, documentRef: win?.document }); }}>
      <span aria-hidden="true">🔑</span><span class="companion-chip-name">${elsewhere.length} to approve</span>
    </button>`}
    ${!chipAgent && html`<button type="button" class="companion-chip companion-chip-steward"
      aria-label=${withShortcut('Open the companion', 'steward')} title=${`${customName || 'Steward'}: your helper for all of tclaude (${shortcutText('steward')})`}
      onClick=${(event) => onChipClick('', event)}>
      <span class="companion-avatar steward" aria-hidden="true">✦</span><span class="companion-chip-name">${customName || 'Steward'}</span>
    </button>`}
  </div>`;

  const offlineActions = (agent) => (agent.agentId === returning?.agentId ? html`<button type="button" class="companion-primary companion-resume-steward" disabled=${busy} onClick=${resumeSteward}>Resume</button>
    <p class="companion-empty-note">${stewardReturnNote(returning)}</p>
    <button type="button" class="companion-secondary companion-fresh-steward" disabled=${busy} onClick=${summonSteward}>Start fresh</button>
    <p class="companion-empty-note">${stewardFreshNote(returning)}</p>`
    : html`<button type="button" class="companion-primary" onClick=${() => resume(agent)}>Resume</button>`);


  // A chat's moves out of the overlay, which its tab's menu offers as its ⚙ does.
  const chatMoves = useRef(null);
  chatMoves.current = (tab) => {
    const agent = agents.get(tab.agent);
    const away = parked.has(tab.agent);
    return {
      toCollab: agent && openTerminalsPane && !away ? () => chatToTerminals(tab, agent) : null,
      toWindow: agent?.online && detachWindow && !away ? ({ rect }) => detachToWindow(agent, rect) : null,
    };
  };
  useEffect(() => registerTabMoves(CHAT_WINDOW_KIND, (tab) => chatMoves.current(tab)), []);
  // Config can turn Snap Assist off; it is on unless the snapshot says so.
  const snapAssistOn = snapshot.value?.snap_assist !== false;
  useEffect(() => setSnapAssistEnabled(snapAssistOn), [snapAssistOn]);

  const chatTabTools = {
    statusOf, offlineActions, openMorph, dictationImpl, starting: (agentId) => resuming.has(agentId) || pending.current.has(agentId),
    voice: speaker.supported ? { supported: true, on: (agentId) => voiceAgents.has(agentId), toggle: toggleVoice, quiet: () => speaker.cancel() } : null,
    viewTab: () => activeTab.value,
    // Drawing happens above the windows, which step aside, so it cannot live in one.
    annotate: (agentId) => startAnnotating(agents.get(agentId) || null),
    onCompose: onComposeMessage ? (agent, restoreFocus) => composeRef.current(agent, restoreFocus) : null,
    onTerminals: openTerminalsPane ? chatToTerminals : null,
    parked: (agentId) => parked.has(agentId),
    onReclaim: reclaim,
    onMic,
  };
  const shownTerminal = { snapshot, prefs, win, widgetFactory, loadRuntime, Terminal: CompanionTerminal };
  const chatView = ({ agent: agentId, label }) => {
    const agent = agents.get(agentId);
    const name = agent?.name || label;
    const tabStatus = agent ? statusOf(agent) : 'offline';
    return { label: name, lead: html`<${StatusDot} status=${tabStatus} />`, title: `${name}: ${agent ? companionStatusLabel(tabStatus) : 'not on this node any more'}` };
  };
  const stewardHome = { win, steward, shutDown, returning, busy, stewardLabel, customName, snapshot };
  const tabKinds = {
    [CHAT_WINDOW_KIND]: { view: chatView, asksToClose: ({ agent }) => !!(agents.get(agent)?.steward && agents.get(agent).online), ...shownTerminal, fetchImpl, notify, closeTerminalPanes, onSeparateWindow: detachWindow ? detachToWindow : null, onFloat: floatTab, onStewardDown: stewardShutDown, stewardRoster, openPermissions, openSudo, openStewardHandover, showAgent, DecisionCard, StatusDot, ...chatTabTools },
    [TERMINAL_WINDOW_KIND]: { ...shownTerminal, notify },
    [BOARD_WINDOW_KIND]: { snapshot, fetchImpl, notify, win },
    [PROCESS_WINDOW_KIND]: { snapshot, fetchImpl, notify, win },
    [START_WINDOW_KIND]: { Body: StartTab, view: () => ({ label: customName || 'Steward', lead: html`<span class="companion-avatar steward" aria-hidden="true">✦</span>`, title: customName || 'Steward' }), ...stewardHome, onResume: resumeSteward, onSummon: summonSteward },
    [DECIDE_WINDOW_KIND]: { Body: DecideTab, view: () => ({ label: `${queue.length} to decide`, lead: html`<span class="companion-decide-mark" aria-hidden="true">◆</span>`, title: 'Decisions agents are waiting on' }), snapshot, queue, askerName, fetchImpl, notify, DecisionCard },
  };
  const addTab = (window) => html`<${AddTab} window=${window} agents=${agents} tabs=${held} steward=${steward} busy=${busy}
    stewardLabel=${stewardLabel} returning=${returning} onResumeSteward=${resumeSteward} onSummonSteward=${summonSteward} onPin=${(id) => showAgent(id, { into: window.id })} />`;

  return html`
    ${overlayWindows.value.map((window) => html`<${OverlayTabWindow} key=${window.id} window=${window} snapshot=${snapshot} prefs=${prefs} win=${win} kinds=${tabKinds} home=${home} add=${addTab} />`)}
    <${OverlayTabGhost} />
    <${SnapAssist} win=${win} viewOf=${tabViewOf(tabKinds)} />
    <${OverlayTabMenu} notify=${notify} />
    ${chipSlots}
    ${chips}
    ${annotation.overlay}
    ${needsCount > 0 && html`<span class="companion-sr" role="status">${needsCount} companion${needsCount === 1 ? '' : 's'} need you</span>`}
    <${CompanionViewChannel} activeTab=${activeTab} notify=${notify} fetchImpl=${fetchImpl} win=${win} />
    ${guideCue && html`<${GuideOverlay} key=${guideCue.cue.id} cue=${guideCue.cue} element=${guideCue.element} marks=${guideCue.marks} onDismiss=${dismissGuide} win=${win} />`}
    <${Peek} agent=${peekTarget} lines=${peek?.lines ?? null} anchor=${peek?.anchor} />`;
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
