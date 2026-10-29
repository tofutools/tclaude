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
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  COMPANION_ACTIVE_PREF, COMPANION_MESSAGE_MAX, COMPANION_MIN_H, COMPANION_MIN_W, COMPANION_PLACEMENT_PREF, COMPANION_TABS_PREF,
  clampPlacement, companionAgents, companionStatus, companionStatusLabel, composeCompanionMessage,
  defaultPlacement, isAgentId, nextActive, parsePlacement, parseTabs, snapZone, stewardAgent,
  stewardSummonBody, viewContext, withTab, withoutTab, zoneRect, approvalsFor, approvalSummary,
} from './companion-model.js';
import { registerCompanionController } from './companion-facade.js';
import { GUIDE_FIND_STEP_MS, GUIDE_FIND_TIMEOUT_MS, GUIDE_SETTLE_MS, GUIDE_SHOW_MS, GuideOverlay, cuePath, findGuideTarget, newCues } from './companion-guide.js';
import { AnnotateOverlay, composeAnnotation } from './companion-annotate.js';
import { terminalAttachWidgetOptions } from './terminal-attach-config.js';

const html = htm.bind(h);

const PEEK_DELAY_MS = 220;
const PENDING_MS = 30000;

// snapTopOf is where a snapped side panel starts: just below the nav bar.
function snapTopOf(win) {
  try {
    const bottom = win?.document?.querySelector?.('nav')?.getBoundingClientRect?.().bottom;
    return Number.isFinite(bottom) && bottom > 0 ? Math.round(bottom) : 0;
  } catch {
    return 0;
  }
}
const PEEK_REFRESH_MS = 1500;

function viewportOf(win) {
  return { width: win?.innerWidth || 1280, height: win?.innerHeight || 800 };
}

function reducedMotion(win) {
  try {
    return !!win?.matchMedia?.('(prefers-reduced-motion: reduce)')?.matches;
  } catch {
    return false;
  }
}

function readPref(prefs, key) {
  try {
    return prefs?.getItem?.(key) ?? null;
  } catch {
    return null;
  }
}

function writePref(prefs, key, value) {
  try {
    prefs?.setItem?.(key, value);
  } catch { /* prefs unavailable: the session keeps working */ }
}

function Avatar({ agent }) {
  const steward = agent?.steward;
  return html`<span class=${`companion-avatar${steward ? ' steward' : ''}`} aria-hidden="true">${steward ? '✦' : (agent?.name || '?').slice(0, 1).toUpperCase()}</span>`;
}

function StatusDot({ status }) {
  return html`<span class=${`companion-dot ${status}`} aria-hidden="true"></span>`;
}

// CompanionTerminal mounts the opaque xterm widget for one agent. It owns the
// widget's whole lifecycle: a new agent id or a reconnect generation disposes
// the old widget before attaching a fresh one.
function CompanionTerminal({ agent, generation, widgetFactory, loadRuntime, focusToken, onStatus, onReconnectChange }) {
  const hostRef = useRef(null);
  const widgetRef = useRef(null);
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
        wsPath: `/api/open-window-ws/${encodeURIComponent(agent.agentId)}`,
        initialRetry: true,
        applicationClipboardShortcuts: agent.harness === 'copilot',
        onStatus: (status) => { if (!cancelled) onStatus(status); },
        onReconnectChange: (available) => { if (!cancelled) onReconnectChange(!!available); },
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
  const style = `right:${Math.max(8, anchor.right)}px;bottom:${anchor.bottom}px`;
  return html`<div class="companion-peek" role="tooltip" style=${style}>
    <div class="companion-peek-head"><${Avatar} agent=${agent} /><b>${agent.name}</b>
      <span class="companion-peek-meta">${companionStatusLabel(companionStatus(agent))} · click to open</span></div>
    <pre class="companion-peek-body">${lines === null ? 'reading…' : lines.length ? lines.join('\n') : agent.online ? '(empty screen)' : '(offline)'}</pre>
    <div class="companion-peek-foot">Read-only preview. The keyboard stays with the page until you click.</div>
  </div>`;
}

function AddMenu({ agents, tabs, steward, onSummonSteward, onPin, onClose, busy }) {
  const [filter, setFilter] = useState('');
  const candidates = useMemo(() => {
    const needle = filter.trim().toLowerCase();
    return [...agents.values()]
      .filter((agent) => agent.online && !agent.steward && !tabs.includes(agent.agentId))
      .filter((agent) => !needle || agent.name.toLowerCase().includes(needle))
      .sort((a, b) => a.name.localeCompare(b.name))
      .slice(0, 40);
  }, [agents, tabs, filter]);
  useEffect(() => {
    const onKey = (event) => { if (event.key === 'Escape') onClose(); };
    globalThis.addEventListener?.('keydown', onKey);
    return () => globalThis.removeEventListener?.('keydown', onKey);
  }, [onClose]);
  const stewardPinned = steward && tabs.includes(steward.agentId);
  return html`<div class="companion-menu" role="menu" aria-label="Add a companion">
    ${!stewardPinned && html`<button type="button" role="menuitem" class="companion-menu-steward" disabled=${busy} onClick=${onSummonSteward}>
      <span class="companion-avatar steward" aria-hidden="true">✦</span>
      <span><b>${steward?.online ? 'Open the Steward' : 'Summon the Steward'}</b><small>Read-only helper for all of tclaude. Asks you before changing anything.</small></span>
    </button>`}
    <input type="search" class="companion-menu-filter" placeholder="Pin a live agent…" aria-label="Filter live agents"
      value=${filter} onInput=${(event) => setFilter(event.currentTarget.value)} />
    <div class="companion-menu-list">
      ${candidates.length ? candidates.map((agent) => html`<button key=${agent.agentId} type="button" role="menuitem" onClick=${() => onPin(agent.agentId)}>
        <${StatusDot} status=${companionStatus(agent)} /> ${agent.name}</button>`)
        : html`<div class="companion-menu-empty">${filter ? 'No live agent matches.' : 'No other live agents.'}</div>`}
    </div>
  </div>`;
}

function secondsLeft(deadline, now) {
  const at = Date.parse(deadline || '');
  return Number.isFinite(at) ? Math.max(0, Math.round((at - now) / 1000)) : null;
}

// ApprovalCard is an agent's pending access request shown in its companion
// tab: a whole plan's temporary permissions (`tclaude agent sudo`) or one
// --ask-human command. Approve and Deny decide it the same way the Messages
// tab does. The request body is agent-supplied text and is rendered as text.
function ApprovalCard({ request, fetchImpl, notify }) {
  const [busy, setBusy] = useState('');
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  const summary = approvalSummary(request);
  const left = secondsLeft(request.deadline, now);
  const expired = left === 0;
  async function decide(decision) {
    setBusy(decision);
    try {
      const response = await fetchImpl(`/api/access-requests/${encodeURIComponent(request.id)}/decision`, {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ decision }),
      });
      if (!response.ok) throw new Error((await response.text?.()) || `${response.status}`);
      // An extension leaves the request pending: free the buttons again.
      if (decision === 'extend') setBusy('');
    } catch (error) {
      notify(`That decision could not be recorded: ${error?.message || error}`, true);
      setBusy('');
    }
  }
  return html`<div class=${`companion-approval ${summary.kind}`} role="group" aria-label=${summary.title}>
    <div class="companion-approval-head">
      <b>${summary.title}</b>
      ${left !== null && html`<span class="companion-approval-timer" title="Declined automatically when this runs out">${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}</span>`}
    </div>
    <div class="companion-approval-slugs">${summary.slugs.map((slug) => html`<code key=${slug}>${slug}</code>`)}</div>
    <dl class="companion-approval-meta">
      ${request.target_group && html`<dt>Group</dt><dd>${request.target_group}</dd>`}
      ${request.target_conv_id && html`<dt>Target</dt><dd>${request.target_conv_title || request.target_conv_id}</dd>`}
      ${request.scope_display && html`<dt>Scope</dt><dd><code>${request.scope_display}</code></dd>`}
      ${request.path && !request.body && html`<dt>Endpoint</dt><dd><code>${request.method ? `${request.method} ` : ''}${request.path}</code></dd>`}
    </dl>
    ${request.body && html`<pre class="companion-approval-body">${request.body}</pre>`}
    <div class="companion-approval-actions">
      ${expired ? html`<span class="companion-approval-expired">Timing out…</span>` : html`<button type="button" class="companion-approval-extend" disabled=${!!busy}
        title="Push the auto-decline back five minutes" onClick=${() => decide('extend')}>+5 min</button>`}
      <button type="button" class="companion-approval-deny" disabled=${!!busy || expired} onClick=${() => decide('deny')}>${busy === 'deny' ? 'Denying…' : 'Deny'}</button>
      <button type="button" class="companion-primary" disabled=${!!busy || expired} onClick=${() => decide('approve')}>${busy === 'approve' ? 'Approving…' : summary.kind === 'plan' ? 'Approve plan' : 'Approve'}</button>
    </div>
  </div>`;
}

export function CompanionApp({
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
  registerController = registerCompanionController,
  navigate = null,
}) {
  const agents = useMemo(() => companionAgents(snapshot.value), [snapshot.value]);
  const steward = useMemo(() => stewardAgent(agents), [agents]);
  const [tabs, setTabs] = useState(() => parseTabs(readPref(prefs, COMPANION_TABS_PREF)));
  const [active, setActive] = useState(() => {
    const saved = readPref(prefs, COMPANION_ACTIVE_PREF);
    return isAgentId(saved) ? saved : '';
  });
  const [open, setOpen] = useState(false);
  const [placement, setPlacement] = useState(() => {
    const vp = viewportOf(win);
    const saved = parsePlacement(readPref(prefs, COMPANION_PLACEMENT_PREF));
    if (saved?.snap) return zoneRect(saved.snap, vp, snapTopOf(win));
    return clampPlacement(saved || defaultPlacement(vp), vp);
  });
  const [guide, setGuide] = useState(null);
  const [menu, setMenu] = useState(false);
  const [busy, setBusy] = useState(false);
  const [parked, setParked] = useState(() => new Set());
  const [statuses, setStatuses] = useState({});
  const [reconnectable, setReconnectable] = useState({});
  const [generation, setGeneration] = useState(0);
  const [focusToken, setFocusToken] = useState(0);
  const [peek, setPeek] = useState(null); // {agentId, anchor, lines}
  const [draft, setDraft] = useState('');
  const [withContext, setWithContext] = useState(true);
  // The agent the operator is marking up the page for (tcl-8vjjko), or null.
  const [annotating, setAnnotating] = useState(null);
  const [sending, setSending] = useState(false);
  const windowRef = useRef(null);
  const chipRects = useRef(new Map());
  const openFrom = useRef(null);
  // Agents just summoned or opened but not yet in a snapshot: kept pinned (and
  // shown as starting) until a snapshot lists them or PENDING_MS passes.
  const pending = useRef(new Map());
  // The cleanup of an in-flight drag or resize, run if the island unmounts.
  const gesture = useRef(null);
  useEffect(() => () => gesture.current?.(), []);

  const activeId = tabs.includes(active) ? active : (tabs[0] || '');
  const activeAgent = activeId ? agents.get(activeId) || null : null;

  useEffect(() => writePref(prefs, COMPANION_TABS_PREF, JSON.stringify(tabs)), [tabs]);
  useEffect(() => { if (activeId) writePref(prefs, COMPANION_ACTIVE_PREF, activeId); }, [activeId]);

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

  const showAgent = useCallback((agentId, { focus = true, starting = false } = {}) => {
    if (!isAgentId(agentId)) return false;
    const agent = agents.get(agentId);
    // An id the snapshot does not know is only accepted for an agent this
    // window just started; other callers keep their own fallback.
    if (!agent && !starting && !pending.current.has(agentId)) return false;
    if (!agent && starting) pending.current.set(agentId, Date.now() + PENDING_MS);
    stopPeek();
    closeTerminalPanes([agentId, agent?.convId].filter(Boolean));
    setParked((current) => {
      if (!current.has(agentId)) return current;
      const next = new Set(current);
      next.delete(agentId);
      return next;
    });
    setTabs((current) => withTab(current, agentId));
    setActive(agentId);
    setMenu(false);
    setOpen(true);
    if (focus) setFocusToken((n) => n + 1);
    return true;
  }, [agents, closeTerminalPanes]);

  useEffect(() => registerController({ open: (agentId, options) => showAgent(agentId, options) }), [showAgent]);

  const summonSteward = useCallback(async () => {
    if (steward?.online) {
      showAgent(steward.agentId);
      return;
    }
    setBusy(true);
    try {
      const response = await fetchImpl('/api/scribe', {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(stewardSummonBody()),
      });
      const result = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(result.message || result.error || `${response.status} ${response.statusText}`);
      if (!isAgentId(result.agent_id)) throw new Error('the summon returned no agent id');
      notify(`${result.reused ? 'Reopened' : 'Summoned'} the Steward.`);
      showAgent(result.agent_id, { starting: true });
    } catch (error) {
      notify(`Could not summon the Steward: ${error?.message || error}`, true);
    } finally {
      setBusy(false);
    }
  }, [steward, fetchImpl, notify, showAgent]);

  const closeTab = useCallback((agentId) => {
    const next = withoutTab(tabs, agentId);
    if (agentId === activeId) setActive(nextActive(tabs, agentId));
    if (!next.length) setOpen(false);
    setTabs(next);
  }, [tabs, activeId]);

  const minimize = useCallback(() => {
    setMenu(false);
    const node = windowRef.current;
    const chip = chipRects.current.get(activeId);
    if (!node || !chip || reducedMotion(win) || typeof node.animate !== 'function') {
      setOpen(false);
      return;
    }
    const from = node.getBoundingClientRect();
    const animation = node.animate([
      { transform: 'none', opacity: 1 },
      { transform: `translate(${chip.left - from.left}px, ${chip.top - from.top}px) scale(${chip.width / from.width}, ${chip.height / from.height})`, opacity: 0.4 },
    ], { duration: 160, easing: 'cubic-bezier(.4,0,.8,.4)', fill: 'forwards' });
    animation.onfinish = () => { setOpen(false); animation.cancel(); };
  }, [activeId, win]);

  const toggle = useCallback(() => {
    stopPeek();
    if (open) {
      minimize();
      return;
    }
    if (!tabs.length) {
      setOpen(true);
      return;
    }
    openFrom.current = chipRects.current.get(activeId) || null;
    setOpen(true);
    setFocusToken((n) => n + 1);
  }, [open, tabs, activeId, minimize]);

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
        startAnnotating();
      }
    };
    win?.addEventListener?.('keydown', onKey, true);
    return () => win?.removeEventListener?.('keydown', onKey, true);
  }, [toggle, win, activeAgent?.agentId, activeAgent?.online]);

  // Grow the window out of the chip it was opened from.
  useLayoutEffect(() => {
    if (!open) return;
    const from = openFrom.current;
    openFrom.current = null;
    const node = windowRef.current;
    if (!from || !node || reducedMotion(win) || typeof node.animate !== 'function') return;
    const to = node.getBoundingClientRect();
    if (!to.width || !to.height) return;
    node.animate([
      { transform: `translate(${from.left - to.left}px, ${from.top - to.top}px) scale(${from.width / to.width}, ${from.height / to.height})`, opacity: 0.5, borderRadius: '20px' },
      { transform: 'none', opacity: 1, borderRadius: '10px' },
    ], { duration: 180, easing: 'cubic-bezier(.2,.8,.2,1)' });
  }, [open]);

  // Keep a floating window on screen when the browser resizes.
  useEffect(() => {
    const onResize = () => setPlacement((p) => {
      const vp = viewportOf(win);
      return p.snap ? zoneRect(p.snap, vp, snapTop()) : clampPlacement(p, vp);
    });
    win?.addEventListener?.('resize', onResize);
    return () => win?.removeEventListener?.('resize', onResize);
  }, [win]);

  function snapTop() {
    return snapTopOf(win);
  }

  function savePlacement(p) {
    writePref(prefs, COMPANION_PLACEMENT_PREF, JSON.stringify(p));
  }

  function onTitlePointerDown(event) {
    if (event.button !== 0 || event.target.closest?.('button, input, [role="tab"]')) return;
    const node = windowRef.current;
    if (!node) return;
    event.preventDefault();
    const handle = event.currentTarget;
    handle.setPointerCapture?.(event.pointerId);
    const vp = viewportOf(win);
    let start = placement;
    if (start.snap) {
      // Tearing a snapped window off restores a floating size under the pointer.
      const floating = clampPlacement({ ...defaultPlacement(vp), ...(parsePlacement(readPref(prefs, COMPANION_PLACEMENT_PREF)) || {}), snap: '' }, vp);
      start = { ...floating, x: event.clientX - floating.w / 2, y: event.clientY - 14, snap: '' };
      setPlacement(start);
    }
    const sx = event.clientX;
    const sy = event.clientY;
    let zone = '';
    let last = start;
    const move = (m) => {
      last = clampPlacement({ ...start, x: start.x + m.clientX - sx, y: start.y + m.clientY - sy, snap: '' }, viewportOf(win));
      setPlacement(last);
      zone = snapZone({ x: m.clientX, y: m.clientY }, viewportOf(win));
      setGuide(zone ? zoneRect(zone, viewportOf(win), snapTop()) : null);
    };
    const up = () => {
      handle.removeEventListener('pointermove', move);
      handle.removeEventListener('pointerup', up);
      handle.removeEventListener('pointercancel', up);
      setGuide(null);
      if (zone) {
        savePlacement({ ...last, snap: '' });
        setPlacement(zoneRect(zone, viewportOf(win), snapTop()));
        writePref(prefs, COMPANION_PLACEMENT_PREF, JSON.stringify({ ...last, snap: zone }));
      } else {
        savePlacement(last);
      }
    };
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', up);
    handle.addEventListener('pointercancel', up);
    gesture.current = () => {
      handle.removeEventListener('pointermove', move);
      handle.removeEventListener('pointerup', up);
      handle.removeEventListener('pointercancel', up);
    };
  }

  function onResizePointerDown(event) {
    if (event.button !== 0) return;
    event.preventDefault();
    event.stopPropagation();
    const handle = event.currentTarget;
    handle.setPointerCapture?.(event.pointerId);
    const start = placement;
    const sx = event.clientX;
    const sy = event.clientY;
    let last = start;
    const move = (m) => {
      const dx = m.clientX - sx;
      const dy = m.clientY - sy;
      // A snapped panel resizes along its free edge only.
      if (start.snap === 'right') last = { ...start, x: start.x + dx, w: start.w - dx };
      else if (start.snap === 'left') last = { ...start, w: start.w + dx };
      else if (start.snap === 'bottom') last = { ...start, y: start.y + dy, h: start.h - dy };
      else last = { ...start, w: start.w + dx, h: start.h + dy };
      if (start.snap) {
        const vp = viewportOf(win);
        const w = Math.min(vp.width, Math.max(COMPANION_MIN_W, last.w));
        const h = Math.max(COMPANION_MIN_H, last.h);
        // Keep the panel glued to its edge however far the drag overshoots.
        last = { ...last, w, h };
        if (start.snap === 'right') last.x = vp.width - w;
        if (start.snap === 'bottom') last.y = start.y + start.h - h;
      } else {
        last = clampPlacement(last, viewportOf(win));
      }
      setPlacement(last);
    };
    const up = () => {
      handle.removeEventListener('pointermove', move);
      handle.removeEventListener('pointerup', up);
      handle.removeEventListener('pointercancel', up);
      if (!start.snap) savePlacement(last);
    };
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', up);
    handle.addEventListener('pointercancel', up);
    gesture.current = () => {
      handle.removeEventListener('pointermove', move);
      handle.removeEventListener('pointerup', up);
      handle.removeEventListener('pointercancel', up);
    };
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
      setPeek({ agentId, lines: null, anchor: { right: vp.width - rect.right, bottom: vp.height - rect.top + 8 } });
      void readPeek(agentId);
      peekRefresh.current = setInterval(() => void readPeek(agentId), PEEK_REFRESH_MS);
    }, PEEK_DELAY_MS);
  }

  function onChipClick(agentId, event) {
    stopPeek();
    openFrom.current = event.currentTarget.getBoundingClientRect();
    if (agentId) showAgent(agentId);
    else toggle();
  }

  async function send(event) {
    event?.preventDefault?.();
    const body = composeCompanionMessage(draft, withContext
      ? viewContext({ activeTab: activeTab.value, pathname: win?.location?.pathname, search: win?.location?.search })
      : '');
    if (!body || !activeAgent || sending) return;
    setSending(true);
    try {
      const response = await fetchImpl('/api/operator-message', {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ to: activeAgent.agentId, subject: '', body }),
      });
      const result = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(result.message || result.error || `${response.status} ${response.statusText}`);
      setDraft('');
    } catch (error) {
      notify(`Message to ${activeAgent.name} failed: ${error?.message || error}`, true);
    } finally {
      setSending(false);
    }
  }

  // The window and chips stay mounted (the terminal keeps its stream) but
  // step out of the way while the operator draws.
  useEffect(() => {
    win?.document?.body?.classList?.toggle?.('companion-annotating', !!annotating);
    return () => win?.document?.body?.classList?.remove?.('companion-annotating');
  }, [annotating, win]);

  function startAnnotating() {
    if (!activeAgent?.online) {
      notify('Open a live agent in the companion first, then mark up the page for it.', true);
      return;
    }
    setAnnotating({ agentId: activeAgent.agentId, name: activeAgent.name });
  }

  async function sendAnnotation({ note, lines }) {
    const target = annotating;
    if (!target) return;
    const view = viewContext({ activeTab: activeTab.value, pathname: win?.location?.pathname, search: win?.location?.search });
    const body = composeCompanionMessage(composeAnnotation({ note, lines, view }), view);
    try {
      const response = await fetchImpl('/api/operator-message', {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ to: target.agentId, subject: '[annotation]', body }),
      });
      const result = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(result.message || result.error || `${response.status} ${response.statusText}`);
      setAnnotating(null);
      notify(`Sent your markup to ${target.name}.`);
    } catch (error) {
      notify(`Markup to ${target.name} failed: ${error?.message || error}`, true);
    }
  }

  async function resume(agent) {
    try {
      const response = await fetchImpl(`/api/agents/${encodeURIComponent(agent.agentId)}/resume`, { method: 'POST', credentials: 'same-origin' });
      const result = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(result.message || result.error || `${response.status} ${response.statusText}`);
      notify(`Resuming ${agent.name}…`);
    } catch (error) {
      notify(`Could not resume ${agent.name}: ${error?.message || error}`, true);
    }
  }

  function moveToTerminals(agent) {
    if (!openTerminalsPane) return;
    setParked((current) => new Set(current).add(agent.agentId));
    // Let the companion's widget detach before the Terminals pane attaches.
    setTimeout(() => openTerminalsPane(agent.agentId, agent.name, agent.harness), 60);
  }

  const setReconnectFor = useCallback((agentId) => (available) => setReconnectable((current) => (current[agentId] === available ? current : { ...current, [agentId]: available })), []);
  const setStatusFor = useCallback((agentId) => (status) => setStatuses((current) => (current[agentId] === status ? current : { ...current, [agentId]: status })), []);

  // -- dashboard guidance ---------------------------------------------------
  // Report the operator's view so companion agents can ask where they are
  // (GET /v1/dashboard/view), and play the agents' pointing cues.

  const lastView = useRef('');
  useEffect(() => {
    const doc = win?.document;
    const report = () => {
      if (doc?.visibilityState === 'hidden') return;
      const view = {
        tab: String(activeTab.value || ''),
        path: `${win?.location?.pathname || '/'}${win?.location?.search || ''}`.slice(0, 240),
        title: String(doc?.title || '').slice(0, 160),
      };
      const key = JSON.stringify(view);
      if (key === lastView.current) return;
      lastView.current = key;
      Promise.resolve(fetchImpl('/api/companion/view', {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: key,
      })).then((response) => { if (response && response.ok === false) lastView.current = ''; })
        .catch(() => { lastView.current = ''; });
    };
    // A top-level nav click switches the view without any navigation event;
    // report once its handlers have run.
    const onNavClick = (event) => {
      if (event.target?.closest?.('nav [data-tab]')) setTimeout(report, 0);
    };
    report();
    doc?.addEventListener?.('click', onNavClick);
    doc?.addEventListener?.('tclaude:snapshot', report);
    doc?.addEventListener?.('tclaude:navigated', report);
    doc?.addEventListener?.('visibilitychange', report);
    win?.addEventListener?.('popstate', report);
    return () => {
      doc?.removeEventListener?.('click', onNavClick);
      doc?.removeEventListener?.('tclaude:snapshot', report);
      doc?.removeEventListener?.('tclaude:navigated', report);
      doc?.removeEventListener?.('visibilitychange', report);
      win?.removeEventListener?.('popstate', report);
    };
  }, [win, fetchImpl]);

  const seenCues = useRef(new Set());
  const cuesPrimed = useRef(false);
  const cueRun = useRef(0);
  const [guideCue, setGuideCue] = useState(null); // {cue, element}
  const dismissGuide = useCallback(() => { cueRun.current += 1; setGuideCue(null); }, []);
  useEffect(() => {
    if (!snapshot.value) return;
    const fresh = newCues(seenCues.current, snapshot.value.companion_cues, cuesPrimed.current);
    cuesPrimed.current = true;
    if (fresh.length) void playCue(fresh[fresh.length - 1]);
  }, [snapshot.value]);
  useEffect(() => {
    if (!guideCue) return undefined;
    const run = cueRun.current;
    const timer = setTimeout(() => { if (cueRun.current === run) setGuideCue(null); }, GUIDE_SHOW_MS);
    return () => clearTimeout(timer);
  }, [guideCue]);

  async function playCue(cue) {
    const run = ++cueRun.current;
    setGuideCue(null);
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
    if (cueRun.current !== run) return;
    element?.scrollIntoView?.({ block: 'center', behavior: reducedMotion(win) ? 'auto' : 'smooth' });
    setGuideCue({ cue, element });
  }

  // -- render -----------------------------------------------------------------

  const approvals = (agent) => approvalsFor(snapshot.value, agent);
  // A pending approval is the clearest "needs you" there is.
  const statusOf = (agent) => (approvals(agent).length ? 'needs' : companionStatus(agent));
  const chipAgents = tabs.map((id) => agents.get(id)).filter(Boolean);
  const needsCount = chipAgents.filter((agent) => statusOf(agent) === 'needs').length;
  const peekTarget = peek ? agents.get(peek.agentId) : null;

  const chips = !open && html`<div class="companion-chips" role="toolbar" aria-label="Companions">
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
        ${status === 'needs' ? html`<span class="companion-chip-needs">${approvals(agent).length ? 'approval' : 'needs you'}</span>` : html`<${StatusDot} status=${status} />`}
      </button>`;
    })}
    ${!chipAgents.length && html`<button type="button" class="companion-chip companion-chip-steward"
      aria-label="Open the companion (Ctrl+.)" title="Steward: your helper for all of tclaude (Ctrl+.)"
      onClick=${(event) => onChipClick('', event)}>
      <span class="companion-avatar steward" aria-hidden="true">✦</span><span class="companion-chip-name">Steward</span>
    </button>`}
  </div>`;

  const style = `left:${placement.x}px;top:${placement.y}px;width:${placement.w}px;height:${placement.h}px`;
  const status = activeAgent ? statusOf(activeAgent) : 'offline';
  const activeApprovals = activeAgent ? approvals(activeAgent) : [];
  const termStatus = activeAgent ? statuses[activeAgent.agentId] || 'connecting…' : '';
  const canReconnect = activeAgent ? reconnectable[activeAgent.agentId] === true || ['disconnected', 'error', 'authentication required'].includes(termStatus) : false;
  const starting = !activeAgent && activeId && pending.current.has(activeId);

  const windowView = open && html`<section ref=${windowRef}
    class=${`companion-window${placement.snap ? ` snapped snapped-${placement.snap}` : ''}`}
    style=${style} role="dialog" aria-label="Companion" aria-modal="false">
    <header class="companion-titlebar" onPointerDown=${onTitlePointerDown}>
      <span class="companion-grip" aria-hidden="true">⋮⋮</span>
      <div class="companion-tabs" role="tablist" aria-label="Companion agents">
        ${chipAgents.map((agent) => {
          const tabStatus = statusOf(agent);
          const selected = agent.agentId === activeId;
          return html`<div key=${agent.agentId} class=${`companion-tab${selected ? ' selected' : ''}`}>
            <button type="button" role="tab" aria-selected=${selected ? 'true' : 'false'}
              title=${`${agent.name}: ${companionStatusLabel(tabStatus)}`}
              onClick=${() => showAgent(agent.agentId)}>
              <${Avatar} agent=${agent} /><span>${agent.name}</span><${StatusDot} status=${tabStatus} />
            </button>
            <button type="button" class="companion-tab-close" aria-label=${`Unpin ${agent.name}`} title="Unpin (the agent keeps running)"
              onClick=${() => closeTab(agent.agentId)}>×</button>
          </div>`;
        })}
        <button type="button" class="companion-add" aria-label="Add a companion" aria-expanded=${menu ? 'true' : 'false'}
          title="Summon the Steward or pin a live agent" onClick=${() => setMenu((v) => !v)}>＋</button>
      </div>
      <div class="companion-controls">
        ${activeAgent && openTerminalsPane && html`<button type="button" aria-label="Open in the Terminals tab" title="Open in the Terminals tab"
          onClick=${() => moveToTerminals(activeAgent)}>⤢</button>`}
        <button type="button" aria-label="Minimize (Ctrl+.)" title="Minimize (Ctrl+.)" onClick=${minimize}>–</button>
      </div>
    </header>
    ${menu && html`<${AddMenu} agents=${agents} tabs=${tabs} steward=${steward} busy=${busy}
      onSummonSteward=${summonSteward} onPin=${(id) => showAgent(id)} onClose=${() => setMenu(false)} />`}
    <div class="companion-body">
      ${activeApprovals.map((request) => html`<${ApprovalCard} key=${request.id} request=${request} fetchImpl=${fetchImpl} notify=${notify} />`)}
      ${starting && html`<div class="companion-empty" role="status"><p>Starting the agent… its terminal opens here as soon as it is up.</p></div>`}
      ${!activeAgent && !starting && html`<div class="companion-empty">
        <p class="companion-empty-title"><span class="companion-avatar steward" aria-hidden="true">✦</span> The Steward</p>
        <p>An always-available helper for all of tclaude. Ask what you can do on this page, how your
          groups are set up, or to help you set up groups, profiles, processes or your sandbox.</p>
        <p class="companion-empty-note">It starts read-only and asks you to approve each change.</p>
        <button type="button" class="companion-primary" disabled=${busy} onClick=${summonSteward}>${busy ? 'Summoning…' : steward?.online ? 'Open the Steward' : 'Summon the Steward'}</button>
        <p class="companion-empty-note">Or press ＋ to pin any live agent here.</p>
      </div>`}
      ${activeAgent && !activeAgent.online && html`<div class="companion-empty">
        <p><b>${activeAgent.name}</b> is offline.</p>
        <button type="button" class="companion-primary" onClick=${() => resume(activeAgent)}>Resume</button>
      </div>`}
      ${activeAgent && activeAgent.online && parked.has(activeAgent.agentId) && html`<div class="companion-empty">
        <p><b>${activeAgent.name}</b> is open in the Terminals tab.</p>
        <button type="button" class="companion-primary" onClick=${() => showAgent(activeAgent.agentId)}>Bring it back here</button>
      </div>`}
      ${activeAgent && activeAgent.online && !parked.has(activeAgent.agentId) && html`
        ${status === 'needs' && !activeApprovals.length && html`<div class="companion-needs-bar" role="status">${activeAgent.name} is waiting for you${activeAgent.detail ? `: ${activeAgent.detail}` : ''}</div>`}
        ${termStatus !== 'connected' && html`<div class="companion-term-status" role="status">${canReconnect
          ? html`${termStatus || 'disconnected'}. <button type="button" onClick=${() => setGeneration((n) => n + 1)}>Reconnect</button>`
          : termStatus}</div>`}
        <${CompanionTerminal} key=${activeAgent.agentId} agent=${activeAgent} generation=${generation}
          widgetFactory=${widgetFactory} loadRuntime=${loadRuntime} focusToken=${focusToken}
          onStatus=${setStatusFor(activeAgent.agentId)} onReconnectChange=${setReconnectFor(activeAgent.agentId)} />`}
    </div>
    ${activeAgent && activeAgent.online && html`<form class="companion-input" onSubmit=${send}>
      <label class="companion-context" title="Attach the dashboard view you are on, so the agent knows what you are looking at">
        <input type="checkbox" checked=${withContext} onChange=${(event) => setWithContext(event.currentTarget.checked)} /> view
      </label>
      <input type="text" class="companion-draft" maxLength=${COMPANION_MESSAGE_MAX}
        placeholder=${`Message ${activeAgent.name} (queued to its inbox; or type in the terminal above)`}
        aria-label=${`Message ${activeAgent.name}`} value=${draft} onInput=${(event) => setDraft(event.currentTarget.value)} />
      <button type="submit" disabled=${sending || !draft.trim()}>Send</button>
      <button type="button" class="companion-annotate" aria-label=${`Mark up this page for ${activeAgent.name} (Ctrl+Shift+.)`}
        title="Draw on the page and send the marked-up view (Ctrl+Shift+.)" onClick=${startAnnotating}>✎</button>
    </form>`}
    <div class="companion-resize" aria-hidden="true" onPointerDown=${onResizePointerDown}></div>
  </section>`;

  return html`
    ${guide && html`<div class="companion-snap-guide" aria-hidden="true" style=${`left:${guide.x}px;top:${guide.y}px;width:${guide.w}px;height:${guide.h}px`}></div>`}
    ${windowView}
    ${chips}
    ${annotating && html`<${AnnotateOverlay} agentName=${annotating.name} win=${win}
      onSend=${sendAnnotation} onCancel=${() => setAnnotating(null)} />`}
    ${!open && needsCount > 0 && html`<span class="companion-sr" role="status">${needsCount} companion${needsCount === 1 ? '' : 's'} need you</span>`}
    ${guideCue && html`<${GuideOverlay} key=${guideCue.cue.id} cue=${guideCue.cue} element=${guideCue.element} onDismiss=${dismissGuide} win=${win} />`}
    ${!open && html`<${Peek} agent=${peekTarget} lines=${peek?.lines ?? null} anchor=${peek?.anchor} />`}`;
}

export function mountCompanionIsland({ host, registerCleanup, ...props }) {
  render(html`<${CompanionApp} ...${props} />`, host);
  registerCleanup(() => render(null, host));
}
