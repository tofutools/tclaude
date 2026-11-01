// process-scribe-dock.js -- the process editor's docked scribe (E4,
// tcl-v7byug): a right-rail panel for co-designing the open template with
// its scoped process scribe, in either of two modes.
//
//   chat      the human writes here (operator mail to the scribe); the
//             scribe's turn replies are read back from
//             GET /api/scribe/{agent}/chat and shown as bubbles. While a
//             turn is open the same read carries a live tail of it (the
//             scribe's text so far and its tool calls), polled faster.
//   terminal  the scribe's own terminal, opened through the dashboard's
//             terminal panes (openTerminalPane via the actions' openScribe).
//
// Either way the scribe's edits arrive as a new saved template version that
// the human reviews ON THE CANVAS (process-draft-diff.js): nothing the scribe
// does lands in the editor draft without an explicit accept.
//
// Scribe bodies are untrusted text and are rendered as text only.
//
// The dock's width is resizable: a separator on its canvas edge drags it
// (or arrow keys step it), double-click resets it, and the chosen width is a
// sticky dashboard pref like every other tclaude.dash.* setting.

import { companionAvailable } from './companion-facade.js';
import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { PROCESS_SCRIBE_PROMPT_MAX, processScribeChatLive, processScribeChatTranscript } from './process-scribe.js';
import { dashPrefs } from './prefs.js';

const html = htm.bind(h);

export const SCRIBE_CHAT_POLL_MS = 2500;
// While a turn is open the read is re-polled faster, so its progress streams.
export const SCRIBE_CHAT_LIVE_POLL_MS = 1000;
const DRAFT_PREFIX = 'Draft a process that ';

export const SCRIBE_DOCK_WIDTH_PREF = 'tclaude.dash.processEditor.scribeWidth';
export const SCRIBE_DOCK_MIN_WIDTH = 260;
export const SCRIBE_DOCK_MAX_WIDTH = 760;
const SCRIBE_DOCK_KEY_STEP = 24;
// The dock never takes more than this share of the editor body, so the
// canvas stays usable however wide the saved width is.
const SCRIBE_DOCK_MAX_SHARE = 0.6;

// clampDockWidth bounds a dock width to [min, max], where max also respects
// the editor body's width when it is known (> 0).
export function clampDockWidth(width, bodyWidth = 0) {
  let max = SCRIBE_DOCK_MAX_WIDTH;
  if (bodyWidth > 0) max = Math.max(SCRIBE_DOCK_MIN_WIDTH, Math.min(max, Math.floor(bodyWidth * SCRIBE_DOCK_MAX_SHARE)));
  return Math.round(Math.min(max, Math.max(SCRIBE_DOCK_MIN_WIDTH, width)));
}

function loadDockWidth() {
  try {
    const width = Number(dashPrefs.getItem(SCRIBE_DOCK_WIDTH_PREF));
    return Number.isFinite(width) && width > 0 ? clampDockWidth(width) : null;
  } catch {
    return null; // missing or unreadable pref: the CSS default width
  }
}

function saveDockWidth(width) {
  try {
    if (width == null) dashPrefs.removeItem(SCRIBE_DOCK_WIDTH_PREF);
    else dashPrefs.setItem(SCRIBE_DOCK_WIDTH_PREF, String(width));
  } catch { /* preference only */ }
}

// useDockWidth owns the dock's width: null is the CSS default. The separator
// drags with pointer capture and commits the pref once, on release.
function useDockWidth(dockRef) {
  const [width, setWidth] = useState(loadDockWidth);
  const live = useRef(width);
  live.current = width;
  const bodyWidth = () => dockRef.current?.parentElement?.clientWidth || 0;
  const currentWidth = () => live.current ?? (dockRef.current?.offsetWidth || 320);
  const onPointerDown = (event) => {
    if (event.button !== 0) return;
    event.preventDefault();
    const handle = event.currentTarget;
    handle.setPointerCapture?.(event.pointerId);
    handle.classList.add('is-dragging');
    const startX = event.clientX;
    const startWidth = currentWidth();
    let moved = false;
    const onMove = (move) => {
      // The dock sits right of the canvas: dragging left widens it.
      const next = clampDockWidth(startWidth - (move.clientX - startX), bodyWidth());
      moved = true;
      live.current = next;
      setWidth(next);
    };
    const onUp = () => {
      if (handle.hasPointerCapture?.(event.pointerId)) handle.releasePointerCapture(event.pointerId);
      handle.classList.remove('is-dragging');
      handle.removeEventListener('pointermove', onMove);
      handle.removeEventListener('pointerup', onUp);
      handle.removeEventListener('pointercancel', onUp);
      if (moved) saveDockWidth(live.current);
    };
    handle.addEventListener('pointermove', onMove);
    handle.addEventListener('pointerup', onUp);
    handle.addEventListener('pointercancel', onUp);
  };
  const onKeyDown = (event) => {
    let next;
    if (event.key === 'ArrowLeft') next = currentWidth() + SCRIBE_DOCK_KEY_STEP;
    else if (event.key === 'ArrowRight') next = currentWidth() - SCRIBE_DOCK_KEY_STEP;
    else if (event.key === 'Home') next = SCRIBE_DOCK_MIN_WIDTH;
    else if (event.key === 'End') next = SCRIBE_DOCK_MAX_WIDTH;
    else return;
    event.preventDefault();
    event.stopPropagation();
    next = clampDockWidth(next, bodyWidth());
    setWidth(next);
    saveDockWidth(next);
  };
  const reset = () => {
    setWidth(null);
    saveDockWidth(null);
  };
  return { width, onPointerDown, onKeyDown, reset };
}

// dockScribe picks the scribe scoped to this template: a live one first.
export function dockScribe(scribes, templateId) {
  const scoped = (scribes || []).filter((scribe) => scribe?.scope?.id && scribe.scope.id === templateId);
  return scoped.find((scribe) => scribe.online) || scoped[0] || null;
}

async function defaultFetchChat(agentId, signal) {
  const response = await fetch(`/api/scribe/${encodeURIComponent(agentId)}/chat`, { credentials: 'same-origin', signal });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
  return { messages: processScribeChatTranscript(body), live: processScribeChatLive(body) };
}

// A fetchChat may answer with the bare message list (no live tail).
function chatRead(result) {
  return Array.isArray(result) ? { messages: result, live: null } : { messages: result?.messages || [], live: result?.live || null };
}

function useScribeChat(agentId, enabled, fetchChat, pokeRef) {
  const [state, setState] = useState({ messages: [], live: null, error: '', loaded: false });
  useEffect(() => {
    if (!enabled || !agentId) return undefined;
    const abort = new AbortController();
    let timer = null;
    let stopped = false;
    const poll = async () => {
      clearTimeout(timer);
      let open = false;
      try {
        const read = chatRead(await fetchChat(agentId, abort.signal));
        // Fast only while there is progress to show: a turn whose transcript
        // cannot be read, or one that never got a reply, falls back.
        open = !!read.live?.items?.length;
        if (!stopped) setState({ ...read, error: '', loaded: true });
      } catch (error) {
        if (!stopped && error?.name !== 'AbortError') setState((prev) => ({ ...prev, error: error?.message || String(error), loaded: true }));
      }
      if (!stopped) timer = setTimeout(poll, open ? SCRIBE_CHAT_LIVE_POLL_MS : SCRIBE_CHAT_POLL_MS);
    };
    pokeRef.current = poll;
    void poll();
    return () => { stopped = true; clearTimeout(timer); abort.abort(); pokeRef.current = null; };
  }, [agentId, enabled]);
  return state;
}

// LiveTurn is the open turn so far: the scribe's text as a pending bubble and
// each tool call as a one-line step. Rendered as text only.
function LiveTurn({ live }) {
  if (!live?.items?.length) return null;
  return html`<div class="psd-msg is-scribe is-live" data-scribe-msg="live">
    <span class="psd-msg-who">scribe · in progress</span>
    ${live.truncated && html`<p class="psd-live-more">earlier steps of this turn are not shown</p>`}
    ${live.items.map((item, index) => item.kind === 'tool'
      ? html`<p key=${index} class="psd-live-tool" data-scribe-live="tool">${item.text}</p>`
      : html`<div key=${index} class="psd-msg-body" data-scribe-live="text">${item.text}</div>`)}
  </div>`;
}

function ChatLog({ messages, live, loaded, error, pendingSend }) {
  const log = useRef(null);
  // Live progress follows the bottom only while the reader is there, so
  // scrolling up to re-read an earlier reply is not undone every poll.
  const atBottom = useRef(true);
  const liveSize = (live?.items || []).reduce((sum, item) => sum + item.text.length, 0);
  useLayoutEffect(() => {
    if (log.current) log.current.scrollTop = log.current.scrollHeight;
    atBottom.current = true;
  }, [messages.length, pendingSend]);
  useLayoutEffect(() => {
    if (log.current && atBottom.current) log.current.scrollTop = log.current.scrollHeight;
  }, [liveSize]);
  const onScroll = (event) => {
    const el = event.currentTarget;
    atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
  };
  const last = messages.at(-1);
  const working = pendingSend || last?.role === 'human' || !!live;
  return html`<div ref=${log} onScroll=${onScroll} class="psd-log process-scroll-surface" role="log" aria-live="polite" aria-label="Scribe conversation">
    ${!loaded && html`<p class="psd-empty">Loading the conversation…</p>`}
    ${loaded && !messages.length && !error && html`<p class="psd-empty">Ask for a change in plain words. The scribe saves a new version, and you accept or reject each change on the canvas.</p>`}
    ${messages.map((message) => html`<div key=${message.id} class=${`psd-msg is-${message.role}`} data-scribe-msg=${message.role}>
      <span class="psd-msg-who">${message.role === 'scribe' ? 'scribe' : 'you'}</span>
      <div class="psd-msg-body">${message.body}</div>
    </div>`)}
    ${loaded && html`<${LiveTurn} live=${live} />`}
    ${working && loaded && html`<p class="psd-working" role="status" title=${live?.unavailable || ''}><span class="psd-dot" aria-hidden="true"></span>scribe is working…</p>`}
    ${error && html`<p class="psd-error" role="alert">Could not read the conversation: ${error}</p>`}
  </div>`;
}

export function ScribeDock({ controller, view }) {
  const dock = view.scribeDock;
  const api = controller.options?.scribeDock;
  const templateId = (view.model?.id || '').trim();
  const scribe = dock?.open && api ? dockScribe(api.scribes?.() || [], templateId) : null;
  const [draft, setDraft] = useState('');
  const [sending, setSending] = useState(false);
  const [notice, setNotice] = useState({ text: '', error: false });
  const poke = useRef(null);
  const input = useRef(null);
  const terminalOpened = useRef('');
  const dockRef = useRef(null);
  const resize = useDockWidth(dockRef);
  const chatOn = !!(dock?.open && dock.mode === 'chat' && scribe);
  const chat = useScribeChat(scribe?.agentId || '', chatOn, api?.fetchChat || defaultFetchChat, poke);
  useEffect(() => {
    // Terminal mode opens the scribe's terminal pane once per scribe; the
    // "open terminal" button reopens it on demand.
    if (!dock?.open || dock.mode !== 'terminal' || !scribe?.online || terminalOpened.current === scribe.agentId) return;
    terminalOpened.current = scribe.agentId;
    void api?.openTerminal?.(scribe);
  }, [dock?.open, dock?.mode, scribe?.agentId, scribe?.online]);
  if (!dock?.open || !api) return null;

  const context = controller.scribeDockContext();
  const live = !!scribe?.online;
  const send = async (kind, prompt = '') => {
    if (sending || !live) return;
    setSending(true);
    setNotice({ text: '', error: false });
    const result = await controller.sendToScribe(scribe, { kind, prompt });
    setSending(false);
    if (result?.ok) {
      if (kind === 'chat') setDraft('');
      setNotice({ text: dock.mode === 'terminal' ? 'Sent; it arrives in the scribe\'s terminal.' : '', error: false });
      void poke.current?.();
    } else {
      setNotice({ text: result?.error || 'Could not send.', error: true });
    }
  };
  const submit = (event) => {
    event?.preventDefault?.();
    const text = (input.current?.value ?? draft).trim();
    if (text) void send('chat', text);
  };
  const external = view.external;
  const proposal = (external?.kind === 'clean' || external?.kind === 'dirty') && !view.diff?.open;

  const bodyWidth = dockRef.current?.parentElement?.clientWidth || 0;
  const shownWidth = resize.width ?? (dockRef.current?.offsetWidth || 320);
  return html`<div class="psd-resize" role="separator" aria-orientation="vertical" tabindex="0"
    aria-label="Resize scribe panel" title="Drag to resize the scribe panel; double-click to reset"
    aria-valuemin=${SCRIBE_DOCK_MIN_WIDTH} aria-valuemax=${clampDockWidth(SCRIBE_DOCK_MAX_WIDTH, bodyWidth)} aria-valuenow=${shownWidth}
    data-scribe-resize onPointerDown=${resize.onPointerDown} onKeyDown=${resize.onKeyDown} onDblClick=${resize.reset}></div>
  <aside ref=${dockRef} class="process-scribe-dock process-scroll-surface" aria-label="Process scribe" data-scribe-dock=${dock.mode}
    style=${resize.width ? { width: `${resize.width}px` } : undefined}>
    <div class="psd-head">
      <div class="psd-title-row">
        <strong class="psd-title">${scribe ? scribe.name : 'Process scribe'}</strong>
        ${scribe && html`<span class=${`psd-state ${live ? 'is-live' : 'is-stopped'}`}>${live ? 'live' : 'stopped'}</span>`}
        <span class="spacer"></span>
        <button type="button" class="psd-close" aria-label="Close scribe panel" title="Close the scribe panel"
          onClick=${() => controller.toggleScribeDock(false)}>✕</button>
      </div>
      <div class="psd-modes" role="tablist" aria-label="Scribe mode">
        ${[['chat', '💬 Chat'], ['terminal', '⌨ Terminal']].map(([mode, label]) => html`<button key=${mode} type="button" role="tab"
          class=${`psd-mode${dock.mode === mode ? ' is-active' : ''}`} aria-selected=${dock.mode === mode ? 'true' : 'false'}
          data-scribe-mode=${mode} onClick=${() => controller.setScribeDockMode(mode)}>${label}</button>`)}
      </div>
      ${scribe && html`<div class="psd-scope">scope: ${scribe.scopeLabel}</div>`}
    </div>

    ${!scribe && dock.starting && html`<div class="psd-summon" role="status">
      <p>Starting the scribe in the background…</p>
    </div>`}
    ${!scribe && !dock.starting && html`<div class="psd-summon">
      <p>No scribe is scoped to this template yet.</p>
      <button type="button" class="process-action primary" disabled=${!context.saved}
        title=${context.saved ? 'Summon a scribe scoped to this template' : 'Save the template once first'}
        onClick=${() => controller.requestScribe('template')}>Summon scribe</button>
    </div>`}
    ${scribe && !live && html`<div class="psd-summon">
      <p>${scribe.name} is stopped. Summon a fresh scribe to continue.</p>
      <button type="button" class="process-action" onClick=${() => controller.requestScribe('template')}>Summon scribe</button>
    </div>`}

    ${proposal && html`<div class="psd-proposal" role="status">
      <span>A new version is waiting.</span>
      <button type="button" class="process-action primary" data-review-on-canvas
        onClick=${() => controller.openDiffReview()}>Review on canvas</button>
    </div>`}

    ${live && html`<div class="psd-context" aria-label="Editor context">
      <div class="psd-chips">
        <span class=${`psd-chip${context.selection ? '' : ' is-empty'}`}>selection: ${context.selection ? (context.singleNode || `${context.selection} items`) : 'none'}</span>
        <span class=${`psd-chip${context.errors ? ' is-error' : ' is-empty'}`}>validation errors (${context.errors})</span>
      </div>
      <div class="psd-actions">
        <button type="button" class="process-action" disabled=${sending || !context.selection || !context.saved} data-scribe-action="selection"
          onClick=${() => send('selection')}>send selection</button>
        <button type="button" class="process-action" disabled=${sending || !context.singleNode || !context.saved} data-scribe-action="explain"
          onClick=${() => send('explain')}>explain this node</button>
        <button type="button" class="process-action" disabled=${sending || !context.hasIssue || !context.saved} data-scribe-action="diagnostic"
          onClick=${() => send('diagnostic')}>fix this issue</button>
        <button type="button" class="process-action" data-scribe-action="draft"
          onClick=${() => { controller.setScribeDockMode('chat'); setDraft(DRAFT_PREFIX); queueMicrotask(() => input.current?.focus?.()); }}>draft from description</button>
      </div>
    </div>`}

    ${live && dock.mode === 'chat' && html`<${ChatLog} messages=${chat.messages} live=${chat.live} loaded=${chat.loaded} error=${chat.error} pendingSend=${sending} />`}
    ${live && dock.mode === 'terminal' && html`<div class="psd-terminal">
      <p>${companionAvailable() ? 'The scribe\'s terminal opens in the floating companion window over the editor (Ctrl+. toggles it).' : 'The scribe\'s terminal opens in its own window, or in the Terminals tab if no window can be opened.'} Messages and context actions sent from here arrive there too.</p>
      <button type="button" class="process-action" data-scribe-open-terminal onClick=${() => api.openTerminal?.(scribe)}>Open terminal</button>
    </div>`}

    ${live && html`<form class="psd-compose" onSubmit=${submit}>
      <textarea ref=${input} class="psd-input" rows="3" maxLength=${PROCESS_SCRIBE_PROMPT_MAX} aria-label="Message the scribe"
        placeholder="Ask for a change… (Ctrl+Enter to send)" value=${draft} disabled=${sending}
        onInput=${(event) => setDraft(event.currentTarget.value)}
        onKeyDown=${(event) => {
          if (event.key === 'Enter' && (event.ctrlKey || event.metaKey) && !event.isComposing) submit(event);
          event.stopPropagation();
        }}></textarea>
      <div class="psd-compose-row">
        ${notice.text && html`<span class=${`psd-notice${notice.error ? ' is-error' : ''}`} role=${notice.error ? 'alert' : 'status'}>${notice.text}</span>`}
        <span class="spacer"></span>
        <button type="submit" class="process-action primary" disabled=${sending || !draft.trim()}>${sending ? 'Sending…' : 'Send'}</button>
      </div>
    </form>`}
  </aside>`;
}
