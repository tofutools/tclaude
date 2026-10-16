// background-terminals.js — tclaude tmux sessions still running with nothing
// attached to them: agents, plain sessions and shells the operator minimized
// or closed the tab on. The Terminals tab shows them as a counter beside the
// tab strip (with an attach popover) so idle background sessions are not left
// running by mistake. GET /api/background-terminals is the source; a session
// counts as background only while no tmux client (web terminal or native
// window) is attached to it.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';
import { openGroupWebTermPane, openWebTermPane, openWebWindowPane } from './terminals-tab.js';
import { terminalTabStatus } from './terminal-tab-status.js';

const html = htm.bind(h);

export const BACKGROUND_TERMINALS_PATH = '/api/background-terminals';
const POLL_MS = 5000;
// An attach takes a moment to show up as a tmux client; re-read after it so
// the row does not linger (the open pane key already hides it meanwhile).
const SETTLE_MS = 1200;
const STALE_SECONDS = 60 * 60;

export const backgroundTerminals = signal([]);

let refreshImpl = null;

// refreshBackgroundTerminals asks the running poller for an immediate read.
export function refreshBackgroundTerminals() {
  refreshImpl?.();
}

// startBackgroundTerminalsPoll reads the listing now and then every few
// seconds while the page is visible. Returns the stop function.
export function startBackgroundTerminalsPoll({
  fetchImpl = (...args) => fetch(...args), intervalMs = POLL_MS, doc = globalThis.document,
} = {}) {
  let stopped = false;
  let inflight = false;
  const read = async () => {
    if (stopped || inflight || doc?.hidden) return;
    inflight = true;
    try {
      const response = await fetchImpl(BACKGROUND_TERMINALS_PATH, { credentials: 'same-origin' });
      if (!response.ok) return;
      const body = await response.json();
      const rows = Array.isArray(body?.terminals) ? body.terminals : [];
      // Publish only on change so an unchanged poll re-renders nothing.
      if (!stopped && JSON.stringify(rows) !== JSON.stringify(backgroundTerminals.peek())) {
        backgroundTerminals.value = rows;
      }
    } catch {
      // Keep the last good listing; the next tick retries.
    } finally {
      inflight = false;
    }
  };
  const timer = setInterval(read, intervalMs);
  const onVisible = () => { if (!doc?.hidden) void read(); };
  doc?.addEventListener?.('visibilitychange', onVisible);
  refreshImpl = () => void read();
  void read();
  return () => {
    stopped = true;
    clearInterval(timer);
    doc?.removeEventListener?.('visibilitychange', onVisible);
    if (refreshImpl) refreshImpl = null;
  };
}

// visibleBackgroundTerminals drops rows whose pane is already open here: the
// listing lags an attach by up to one poll.
export function visibleBackgroundTerminals(rows, panes) {
  const open = new Set((panes || []).map((pane) => pane.key));
  return (rows || []).filter((row) => !row.key || !open.has(row.key));
}

const KIND_SECTIONS = Object.freeze([
  ['agent', 'Agents'],
  ['session', 'Sessions'],
  ['shell', 'Shells'],
  ['group_shell', 'Group shells'],
  ['other', 'Other tmux sessions'],
]);

function selectorOf(row) {
  return row.agent_id || row.conv_id || '';
}

// rowLabel prefers the roster's live title so a renamed agent reads right
// before the next listing.
function rowView(row, snapshotValue) {
  const selector = selectorOf(row);
  const status = selector ? terminalTabStatus({ label: row.label, seed: { agent: selector } }, snapshotValue) : null;
  const title = status?.agent?.title || row.label || row.tmux_session;
  let label = title;
  if (row.kind === 'shell') label = `shell · ${title}${row.which && row.which !== 'current' ? ` (${row.which})` : ''}`;
  if (row.kind === 'group_shell') label = `group shell · ${row.group}`;
  return { label, title, status };
}

export function idleText(lastActivity, nowSeconds = Date.now() / 1000) {
  const at = Number(lastActivity || 0);
  if (!at) return { text: '', stale: false };
  const age = Math.max(0, nowSeconds - at);
  const text = age < 60 ? 'just now'
    : age < 3600 ? `${Math.floor(age / 60)}m`
      : age < 86400 ? `${Math.floor(age / 3600)}h`
        : `${Math.floor(age / 86400)}d`;
  return { text: age < 60 ? text : `idle ${text}`, stale: age >= STALE_SECONDS };
}

export function attachBackgroundTerminal(row, snapshotValue) {
  const { title } = rowView(row, snapshotValue);
  const selector = selectorOf(row);
  if ((row.kind === 'agent' || row.kind === 'session') && selector) {
    openWebWindowPane(selector, title, { harness: row.harness || '' });
  } else if (row.kind === 'shell' && selector && row.which) {
    openWebTermPane(selector, title, row.which, { group: row.group || '' });
  } else if (row.kind === 'group_shell' && row.group) {
    openGroupWebTermPane(row.group, `group shell · ${row.group}`);
  } else {
    return false;
  }
  setTimeout(refreshBackgroundTerminals, SETTLE_MS);
  return true;
}

function BackgroundIcon() {
  return html`<svg class="mux-bg-icon" width="14" height="14" viewBox="0 0 16 16" fill="none"
    stroke="currentColor" stroke-width="1.4" aria-hidden="true" focusable="false">
    <rect x="1.5" y="2.5" width="13" height="9" rx="1.5" /><path d="M4 14h8" />
    <path d="M5.5 7.5h5" stroke-linecap="round" /></svg>`;
}

// BackgroundTerminalsChip is the right-aligned counter beside the tab strip.
// Hover peeks the breakdown; a click pins it open so the attach buttons are
// reachable without holding the pointer still.
export function BackgroundTerminalsChip({ rows, snapshot = null }) {
  const [open, setOpen] = useState(false);
  const [pinned, setPinned] = useState(false);
  const rootRef = useRef(null);
  const closeTimer = useRef(0);
  const count = rows.length;

  const close = () => { setOpen(false); setPinned(false); };
  const cancelClose = () => { clearTimeout(closeTimer.current); closeTimer.current = 0; };
  const scheduleClose = () => {
    if (pinned) return;
    cancelClose();
    closeTimer.current = setTimeout(() => setOpen(false), 220);
  };
  useEffect(() => () => cancelClose(), []);
  useEffect(() => {
    if (!open) return undefined;
    const onDown = (event) => { if (!rootRef.current?.contains(event.target)) close(); };
    const onKey = (event) => { if (event.key === 'Escape') close(); };
    document.addEventListener('pointerdown', onDown, true);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('pointerdown', onDown, true);
      document.removeEventListener('keydown', onKey);
    };
  }, [open]);
  useEffect(() => { if (!count) close(); }, [count]);
  if (!count) return null;

  const snapshotValue = snapshot?.value;
  const attachable = rows.filter((row) => row.key);
  const attach = (row) => {
    attachBackgroundTerminal(row, snapshotValue);
  };
  const attachAll = () => {
    for (const row of attachable) attachBackgroundTerminal(row, snapshotValue);
    close();
  };
  const now = Date.now() / 1000;
  const noun = count === 1 ? 'session' : 'sessions';

  return html`
    <div ref=${rootRef} class=${`mux-bg${open ? ' open' : ''}`}
      onMouseEnter=${() => { cancelClose(); setOpen(true); }} onMouseLeave=${scheduleClose}>
      <button type="button" class="mux-bg-trigger" aria-expanded=${open ? 'true' : 'false'} aria-haspopup="dialog"
        aria-label=${`${count} tclaude ${noun} running in the background without a terminal`}
        title=${`${count} tclaude ${noun} still running in tmux with no terminal attached`}
        onClick=${() => { if (pinned) close(); else { setOpen(true); setPinned(true); } }}>
        <${BackgroundIcon} /><span class="mux-bg-count">${count}</span>
      </button>
      ${open ? html`
        <div class="mux-bg-pop" role="dialog" aria-label="Background terminals">
          <div class="mux-bg-pop-head"><b>${count} running in background</b><span> · no terminal attached</span></div>
          ${KIND_SECTIONS.map(([kind, title]) => {
            const items = rows.filter((row) => row.kind === kind);
            if (!items.length) return null;
            return html`
              <div class="mux-bg-sec" key=${kind}>
                <div class="mux-bg-sec-head">${title} · ${items.length}</div>
                ${items.map((row) => {
                  const view = rowView(row, snapshotValue);
                  const idle = idleText(row.last_activity, now);
                  // A shell's liveness is not its agent's status; only the
                  // agent/session rows borrow the roster light.
                  const conversation = row.kind === 'agent' || row.kind === 'session';
                  const statusClass = conversation ? (view.status?.className || 'unknown') : 'shell';
                  return html`
                    <div class="mux-bg-row" key=${row.tmux_session}>
                      <span class=${`mux-tab-status mux-tab-status-${statusClass}`}
                        title=${conversation && view.status?.agent ? view.status.description : ''}></span>
                      <span class="mux-bg-name" title=${view.label}>${view.label}</span>
                      ${idle.text ? html`<span class=${`mux-bg-idle${idle.stale ? ' stale' : ''}`}>${idle.text}</span>` : null}
                      ${row.key
                        ? html`<button type="button" class="mux-btn mux-bg-attach" onClick=${() => attach(row)}
                            aria-label=${`Attach ${view.label}`}>Attach</button>`
                        : html`<span class="mux-bg-noattach" title=${`tmux -L tclaude attach -t ${row.tmux_session}`}>not a tclaude terminal</span>`}
                    </div>`;
                })}
              </div>`;
          })}
          ${attachable.length > 1 ? html`
            <div class="mux-bg-foot"><button type="button" class="mux-btn" onClick=${attachAll}>Attach all</button></div>
          ` : null}
        </div>
      ` : null}
    </div>
  `;
}
