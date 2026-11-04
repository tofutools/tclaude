// page-view.js -- a live page's view (tcl-r4sg0c): the header that says who
// showed it, from where and under which rules, and the sandboxed frame. The
// same view shows in a Collab strip tab (PagePane), an overlay tab
// (PageOverlayWindow) and a separate window, which is the dashboard's own
// /terminals?solo=1 page wrapping the same frame: the page's origin never loads
// top level.
//
// Nothing the agent or the page wrote is rendered as HTML: titles, targets and
// worktrees are text, and the page's URL, the only place its token appears in
// this dashboard, goes only to the frame's src.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { LockChip } from './resource-lock.js';
import {
  PAGE_FRAME_SANDBOX, PAGE_WINDOW_KIND, findLivePage, isPageId, pageFrameSrc, pageLabel, pagePaneSeed, pageSandboxRules, pageSender,
} from './page-pane-model.js';
import {
  closeLivePage, markPageSeen, notePageWindow, returnPageToTab, setPageOverlayLabel,
} from './page-place.js';
import { OverlayWindow } from './overlay-window.js';
import { OVERLAY_FOOTER_ROOM, clampPlacement } from './overlay-window-model.js';
import { windowControlsLook } from './overlay-window-controls.js';
import { closeOverlayTab, minimizeOverlayWindow } from './overlay-instances.js';
import { POPUP_BLOCKED, TO_COLLAB_LABEL, registerTabMoves } from './overlay-tab-moves.js';
import { detachSeedWindow, openWebWindowPane } from './terminals-tab.js';
import { elementScreenPoint } from './terminal-drag-out.js';
import { shellToast } from './shell-state.js';
import { dashboardState } from './snapshot-store.js';
import { PageDrawBar, PageDrawLayer, marksPayload, sendPageMarks, usePageMarks, usePageProbe } from './page-draw.js';
import { patchChip } from './page-probe-model.js';

const html = htm.bind(h);

const LIVE_RETRY_MS = 2000;
const LIVE_RETRY_MAX_MS = 30000;

// usePageLive holds the page's live socket (GET /api/pages/{id}/live): whether
// its source answers, and a token that changes when the frame should reload.
// retry asks again and reloads.
//
// The same socket carries the daemon's DOM reads for `tclaude agent page dom`
// ({"type":"dom",req,selector,mode}, answered by onDom with
// {"type":"dom-result",req,...}) and this view's viewport and scroll
// ({"type":"view",...}, from report), which `page snapshot` renders at.
//
// It also carries the page's live patch journal (tcl-14z3fq):
// {"type":"patches",seq,patches,req?} on connect and on every change, which
// onPatches applies (answering {"type":"patch-result",req,...} for the newest
// patch when asked), and {"type":"eval",req,js}, answered by onEval with
// {"type":"eval-result",req,json|error}. The view relays only what this
// socket delivered; journal is the last one, for the header.
const VIEW_REPORT_MS = 500;
export function usePageLive(id, origin, {
  win = globalThis.window, WebSocketCtor = globalThis.WebSocket, onDom = null, onPatches = null, onEval = null,
} = {}) {
  const [reach, setReach] = useState('checking');
  const [reloads, setReloads] = useState(0);
  const [attempt, setAttempt] = useState(0);
  const [journal, setJournal] = useState(null);
  const socketRef = useRef(null);
  const onDomRef = useRef(onDom);
  onDomRef.current = onDom;
  const onPatchesRef = useRef(onPatches);
  onPatchesRef.current = onPatches;
  const onEvalRef = useRef(onEval);
  onEvalRef.current = onEval;
  const lastView = useRef({ json: '', at: 0, timer: null });
  const sendJSON = (message) => {
    const socket = socketRef.current;
    if (!socket || socket.readyState !== 1) return false;
    try { socket.send(JSON.stringify(message)); return true; } catch { return false; }
  };
  const flushView = () => {
    const v = lastView.current;
    v.timer = null;
    if (v.json && sendJSON(JSON.parse(v.json))) v.at = Date.now();
  };
  // report queues this view's viewport and scroll, at most every half second.
  const report = (view) => {
    const v = lastView.current;
    const json = JSON.stringify({ type: 'view', vw: view.vw, vh: view.vh, x: view.x, y: view.y, path: view.path || '' });
    if (json === v.json) return;
    v.json = json;
    if (v.timer) return;
    v.timer = setTimeout(flushView, Math.max(0, VIEW_REPORT_MS - (Date.now() - v.at)));
  };
  useEffect(() => () => clearTimeout(lastView.current.timer), []);
  useEffect(() => {
    if (!isPageId(id) || !origin || !WebSocketCtor || !win?.location || globalThis.__tclaudeRemoteNode) return undefined;
    let disposed = false;
    let socket = null;
    let timer = null;
    let delay = LIVE_RETRY_MS;
    let gone = false;
    setReach('checking');
    const connect = () => {
      if (disposed) return;
      const scheme = win.location.protocol === 'https:' ? 'wss:' : 'ws:';
      try {
        socket = new WebSocketCtor(`${scheme}//${win.location.host}/api/pages/${encodeURIComponent(id)}/live`);
      } catch {
        return;
      }
      socketRef.current = socket;
      socket.onopen = () => { if (lastView.current.json) flushView(); };
      socket.onmessage = (event) => {
        let message = null;
        try { message = JSON.parse(event.data); } catch { return; }
        delay = LIVE_RETRY_MS;
        switch (message?.type) {
          case 'reachable': setReach('reachable'); break;
          case 'unreachable': setReach('unreachable'); break;
          case 'reload': setReach('reachable'); setReloads((n) => n + 1); break;
          // The origin closed; the snapshot says what comes next.
          case 'gone': gone = true; break;
          case 'dom': {
            const req = typeof message.req === 'string' ? message.req : '';
            if (!req) break;
            const answer = (body) => sendJSON({ type: 'dom-result', req, ...body });
            if (!onDomRef.current) { answer({ error: 'this view has no probe' }); break; }
            Promise.resolve(onDomRef.current(String(message.selector || ''), String(message.mode || 'text')))
              .then((result) => answer({
                count: result.count, items: result.items,
                ...(result.truncated ? { truncated: true } : {}), ...(result.error ? { error: result.error } : {}),
              }))
              .catch((cause) => answer({ error: String(cause?.message || cause).slice(0, 200) }));
            break;
          }
          case 'patches': {
            const patches = Array.isArray(message.patches) ? message.patches : [];
            const seq = Number.isFinite(message.seq) ? message.seq : 0;
            setJournal({ seq, patches });
            const req = typeof message.req === 'string' ? message.req : '';
            const newest = patches.length ? Math.max(...patches.map((p) => p.n)) : null;
            const done = Promise.resolve(onPatchesRef.current ? onPatchesRef.current(patches, req ? newest : null) : { applied: 0, state: 'loading' });
            if (req) {
              done.then((result) => sendJSON({ type: 'patch-result', req, applied: result?.applied || 0,
                ...(result?.state ? { state: result.state } : {}), ...(result?.error ? { error: result.error } : {}) }))
                .catch((cause) => sendJSON({ type: 'patch-result', req, applied: 0, error: String(cause?.message || cause).slice(0, 200) }));
            }
            break;
          }
          case 'eval': {
            const req = typeof message.req === 'string' ? message.req : '';
            if (!req) break;
            const answer = (body) => sendJSON({ type: 'eval-result', req, ...body });
            if (!onEvalRef.current) { answer({ error: 'this view has no probe' }); break; }
            Promise.resolve(onEvalRef.current(String(message.js || '')))
              .then((result) => answer(result.error ? { error: result.error } : { json: result.json }))
              .catch((cause) => answer({ error: String(cause?.message || cause).slice(0, 200) }));
            break;
          }
          default: break;
        }
      };
      socket.onclose = () => {
        socket = null;
        socketRef.current = null;
        if (disposed || gone) return;
        timer = setTimeout(connect, delay);
        delay = Math.min(delay * 2, LIVE_RETRY_MAX_MS);
      };
    };
    connect();
    return () => {
      disposed = true;
      clearTimeout(timer);
      if (socket) {
        socket.onclose = null;
        try { socket.close(); } catch { /* already closing */ }
      }
    };
  }, [id, origin, attempt]);
  const retry = () => {
    setAttempt((n) => n + 1);
    setReloads((n) => n + 1);
  };
  return { reach, reloads, retry, report, journal };
}

const baseName = (path) => String(path || '').split('/').filter(Boolean).pop() || String(path || '');

// PageFrame is the sandboxed frame itself. Its attributes are the boundary:
// a sandbox without allow-same-origin, allow-popups, allow-top-navigation* or
// allow-downloads; no permissions; no referrer; credentialless.
export function PageFrame({ src, label, reloadKey, frameRef = null, onLoad = null }) {
  if (!src) return html`<div class="ui-empty-block">This page has no address on this dashboard.</div>`;
  return html`<iframe key=${reloadKey} ref=${frameRef} onLoad=${onLoad} class="page-frame" src=${src} title=${`Live page: ${label}`}
    sandbox=${PAGE_FRAME_SANDBOX} allow="" referrerpolicy="no-referrer" credentialless=${true} loading="eager"></iframe>`;
}

// PageView is the header and the frame. tools are the buttons for where the
// view is (move it elsewhere); onOpenSender opens the agent's pane, where
// there is one.
export function PageView({ pageId, snapshot, tools = null, onOpenSender = null, fetchImpl, notify, win = globalThis.window, WebSocketCtor }) {
  const value = snapshot?.value;
  const page = findLivePage(value, pageId);
  const frameRef = useRef(null);
  const [manual, setManual] = useState(0);
  const [without, setWithout] = useState(false);
  const probe = usePageProbe(frameRef, { win, onReload: () => setManual((n) => n + 1) });
  // A remote viewer's page has no origin of its own; its address keys the socket.
  const live = usePageLive(pageId, page?.origin || pageFrameSrc(page), {
    win, ...(WebSocketCtor && { WebSocketCtor }), onDom: probe.dom, onPatches: probe.syncPatches, onEval: probe.eval,
  });
  useEffect(() => probe.setWithout(without), [without]);
  const chip = patchChip(live.journal);
  // A reset to no patches ends "without": the next patch shows.
  useEffect(() => { if (without && !chip?.live) setWithout(false); }, [without, chip?.live]);
  const [closing, setClosing] = useState(false);
  const [drawing, setDrawing] = useState(false);
  const [tool, setTool] = useState('freedraw');
  const [color, setColor] = useState('#e03131');
  const [note, setNote] = useState('');
  const [sending, setSending] = useState(false);
  const marks = usePageMarks(probe);
  // The lock as just toggled here, until the snapshot carries it.
  const [lockSet, setLockSet] = useState(null);
  const snapLocked = !!page?.agents_locked;
  useEffect(() => setLockSet(null), [snapLocked]);
  useEffect(() => { if (probe.scroll) live.report(probe.scroll); }, [probe.scroll]);
  if (!page) {
    return html`<div class="page-view page-view-closed" data-page=${pageId}>
      <div class="page-head"><span class="page-head-title">${value ? 'This page has closed.' : 'Loading the page…'}</span><span class="spacer"></span>${tools}</div>
    </div>`;
  }
  const label = pageLabel(page);
  const sender = pageSender(value, page);
  const rules = pageSandboxRules(page);
  const src = pageFrameSrc(page);
  const send = async () => {
    setSending(true);
    try {
      const sentMarks = marks.marks;
      const sentNote = note;
      const told = await sendPageMarks(page.id, marksPayload(sentMarks, sentNote, probe.scroll), { fetchImpl });
      // Only what was sent goes: marks drawn or a note typed meanwhile stay.
      marks.drop(sentMarks);
      setNote((current) => (current === sentNote ? '' : current));
      if (sender.group !== undefined) notify?.(`Sent to ${sender.name}: its members read it, and whoever takes the page over keeps it`);
      else notify?.(told.told_agent === false ? `Saved; ${sender.name} is not running, so it was not told` : `Sent to ${sender.name}`, told.told_agent === false);
    } catch (cause) {
      notify?.(`Could not send the marks: ${cause?.message || cause}`, true);
    }
    setSending(false);
  };
  const close = async () => {
    setClosing(true);
    if (!(await closeLivePage(page.id, { fetchImpl, notify }))) setClosing(false);
  };
  return html`<div class="page-view" data-page=${page.id}>
    <div class="page-head">
      <span class="page-head-glyph" aria-hidden="true">◧</span>
      <span class="page-head-title" title=${label}>${label}</span>
      <span class="page-head-from">from ${onOpenSender && sender.agent
        ? html`<${Button} size="sm" variant="link" class="page-head-sender" title=${`Open ${sender.name}'s terminal`} onClick=${() => onOpenSender(sender)}>${sender.name}<//>`
        : html`<b class="page-head-sender">${sender.name}</b>`}</span>
      <code class="page-head-target" title=${`Target: ${page.target}`}>${page.target}</code>
      ${page.worktree ? html`<span class="page-head-worktree" title=${`Worktree: ${page.worktree}`}>${baseName(page.worktree)}</span>` : null}
      <${LockChip} kind="page" id=${page.id} name=${label} locked=${lockSet ?? snapLocked} notify=${notify || (() => {})} onChanged=${setLockSet} />
      <span class="ui-tag page-sandbox-chip" tabIndex="0" role="note" title=${rules.join('\n')} aria-label=${`Sandboxed. ${rules.join(' ')}`}>🔒 sandboxed</span>
      <span class="spacer"></span>
      ${chip ? html`<span class=${`ui-tag page-patch-chip${chip.live ? ' live' : ''}`} role="status"
        title=${chip.live ? `The agent changed this page live. Its source does not have these changes yet.` : 'The page shows its source.'}>${chip.text}</span>` : null}
      ${chip?.live ? html`<${Button} size="sm" class="mux-btn" pressed=${without}
        title=${without ? 'Show the page with the live patches again' : 'Show the page as its source is, without the live patches'}
        onClick=${() => setWithout((v) => !v)}>Show without<//>` : null}
      <span class="page-mode" role="group" aria-label="Mode">
        <${Button} size="sm" class="mux-btn" pressed=${!drawing} title="Use the page" onClick=${() => setDrawing(false)}>Interact<//>
        <${Button} size="sm" class="mux-btn" pressed=${drawing} title="Draw on top of the page to show the agent what to change" onClick=${() => setDrawing(true)}>Draw${marks.marks.length ? ` (${marks.marks.length})` : ''}<//>
      </span>
      <${Button} size="sm" class="mux-btn" title="Reload the page" onClick=${() => setManual((n) => n + 1)}>Reload<//>
      ${tools}
      <${Button} size="sm" variant="danger" class="mux-btn" busy=${closing} disabled=${closing}
        title="Close this page: it ends, and the agent that showed it is told" onClick=${() => void close()}>Close<//>
    </div>
    ${drawing ? html`<${PageDrawBar} tool=${tool} setTool=${setTool} color=${color} setColor=${setColor} marks=${marks.marks}
      onUndo=${marks.undo} onClear=${marks.clear} note=${note} setNote=${setNote} onSend=${() => void send()} sending=${sending} agentName=${sender.name} />` : null}
    <div class="page-body">
      <${PageFrame} src=${src} label=${label} reloadKey=${`${live.reloads}:${manual}:${src}`} frameRef=${frameRef} onLoad=${probe.onLoad} />
      ${src ? html`<${PageDrawLayer} active=${drawing} tool=${tool} color=${color} marks=${marks.marks} onMark=${marks.add} scroll=${probe.scroll} />` : null}
      ${live.reach === 'unreachable' ? html`<div class="page-unreachable" role="alert">
        <p><b>Nothing is answering for this page.</b> The agent's dev server or files may be gone.</p>
        <p class="ui-hint"><code>${page.target}</code></p>
        <${Button} size="sm" variant="primary" onClick=${live.retry}>Retry<//>
      </div>` : null}
    </div>
  </div>`;
}

// PagePane is a page in a tab of the Collab strip, or the one tab of its
// separate window (solo).
export function PagePane({ pane, active, solo = false, manageTitle = false, actions, snapshot, scope = null, fetchImpl, notify }) {
  const id = pane.seed.page;
  const page = findLivePage(snapshot?.value, id);
  const label = page ? pageLabel(page) : '';
  // Seen when it is the tab showing and the Collab view is the one on screen.
  const onScreen = active && (solo || dashboardState.activeTab.value === 'terminals');
  useEffect(() => {
    if (onScreen) markPageSeen(id);
  }, [onScreen, id]);
  useEffect(() => {
    if (label && label !== pane.label) actions.setPaneLabel?.(pane.key, label);
  }, [label, pane.label]);
  useEffect(() => {
    if (active && manageTitle) document.title = `${label ? `${label} — ` : ''}tclaude page`;
  }, [active, manageTitle, label]);
  const tools = solo
    ? html`<${Button} size="sm" class="mux-btn" title="Move this page back to a tab of the dashboard" onClick=${() => void actions.reattachPane(pane.key)}>${TO_COLLAB_LABEL}<//>
      ${pane.seed.floats && globalThis.window?.opener && !globalThis.window.opener.closed
        ? html`<${Button} size="sm" class="mux-btn" title="Float this page in a window over the dashboard" onClick=${() => void actions.reattachPane(pane.key, { overlay: true })}>Open in overlay<//>` : null}`
    : html`<${Button} size="sm" class="mux-btn" title="Float this page in a window over the dashboard" onClick=${() => void actions.openPaneInOverlay(pane.key)}>Open in overlay<//>
      <${Button} size="sm" class="mux-btn" title="Move this page to a browser window of its own" onClick=${() => void actions.popOutPane(pane.key, { floats: true })}>Pop out<//>`;
  return html`<div class=${`mux-pane mux-pane-page${active ? ' active' : ''}${scope?.hidden ? ' scope-hidden' : ''}`} id=${pane.id}
    role=${solo ? null : 'tabpanel'} data-page-pane=${id}>
    <${PageView} pageId=${id} snapshot=${snapshot} tools=${tools} fetchImpl=${fetchImpl} notify=${notify}
      onOpenSender=${solo ? null : (sender) => void openWebWindowPane(sender.agent, sender.name, { harness: sender.harness })} />
  </div>`;
}

// A page goes back to a Collab tab or out to a separate window; closing its
// overlay tab closes the page, as the strip's × does.
registerTabMoves(PAGE_WINDOW_KIND, (tab) => ({
  toCollab: () => returnPageToTab(tab.page),
  toWindow: ({ rect, win, notify }) => {
    const target = detachSeedWindow({ ...pagePaneSeed(tab.page, tab.label), floats: true }, { size: rect, at: elementScreenPoint(win, rect) });
    if (target) {
      notePageWindow(tab.page, target);
      closeOverlayTab(tab.id);
    } else notify?.(POPUP_BLOCKED, true);
  },
  close: () => void closeLivePage(tab.page, { notify: shellToast }),
}));

// An overlay tab names its page.
export const validPageTab = (tab) => isPageId(tab.page);

// A page wants room: most of the screen, short of the footer.
export function pageStartPlacement(viewport) {
  const w = Math.min(1100, viewport.width - 48);
  const h = Math.min(720, viewport.height - OVERLAY_FOOTER_ROOM - 80);
  return clampPlacement({ x: Math.max(24, (viewport.width - w) / 2), y: 56, w, h, snap: '' }, viewport);
}

export function PageOverlayWindow({ instance, overlay, strip, close, snapshot, fetchImpl, notify, win }) {
  const open = !instance.minimized;
  const look = windowControlsLook(snapshot.value?.window_controls);
  const page = findLivePage(snapshot.value, instance.page);
  const label = page ? pageLabel(page) : instance.label || instance.page;
  useEffect(() => { if (page) setPageOverlayLabel(instance.page, pageLabel(page)); }, [page?.title]);
  useEffect(() => { if (open) markPageSeen(instance.page); }, [open, instance.page]);
  const toCollab = () => void returnPageToTab(instance.page);
  const toWindow = () => {
    const rect = overlay.windowRef.current?.getBoundingClientRect?.();
    const target = detachSeedWindow({ ...pagePaneSeed(instance.page, label), floats: true }, { size: rect, at: elementScreenPoint(win, rect) });
    if (target) {
      notePageWindow(instance.page, target);
      closeOverlayTab(instance.id);
    } else notify?.(POPUP_BLOCKED, true);
  };
  const tools = html`<${Button} size="sm" class="mux-btn" title="Move this page to a tab of the Collab view" onClick=${toCollab}>${TO_COLLAB_LABEL}<//>
    <${Button} size="sm" class="mux-btn" title="Move this page to a browser window of its own" onClick=${toWindow}>Pop out<//>`;
  const title = html`${strip}
    ${look === 'classic' && html`<button data-ui-exempt type="button" class="board-overlay-close" title="Minimize" aria-label=${`Minimize ${label}`}
      onClick=${() => minimizeOverlayWindow(instance.id)}>–</button>`}
    ${look === 'classic' && html`<button data-ui-exempt type="button" class="board-overlay-close" title=${close.title} aria-label=${`Close ${label}`}
      onClick=${close.onClose}>×</button>`}`;
  return html`<${OverlayWindow} id=${instance.windowId} window=${overlay} open=${open} className="board-overlay page-overlay" frameClass="board-overlay-resize-frame"
    titlebarClass=${`board-overlay-titlebar overlay-look-${look}`} label=${`Page ${label}`} title=${title}
    controls=${{ look, closeLabel: `Close ${label}`, closeTitle: close.title, onClose: close.onClose, onMinimize: () => minimizeOverlayWindow(instance.id), onCollab: toCollab }}>
    <${PageView} pageId=${instance.page} snapshot=${snapshot} tools=${tools} fetchImpl=${fetchImpl} notify=${notify} win=${win}
      onOpenSender=${(sender) => void openWebWindowPane(sender.agent, sender.name, { harness: sender.harness })} />
  <//>`;
}
