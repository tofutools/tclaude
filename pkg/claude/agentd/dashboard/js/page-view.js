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

const html = htm.bind(h);

const LIVE_RETRY_MS = 2000;
const LIVE_RETRY_MAX_MS = 30000;

// usePageLive holds the page's live socket (GET /api/pages/{id}/live): whether
// its source answers, and a token that changes when the frame should reload.
// retry asks again and reloads.
export function usePageLive(id, origin, { win = globalThis.window, WebSocketCtor = globalThis.WebSocket } = {}) {
  const [reach, setReach] = useState('checking');
  const [reloads, setReloads] = useState(0);
  const [attempt, setAttempt] = useState(0);
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
          default: break;
        }
      };
      socket.onclose = () => {
        socket = null;
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
  return { reach, reloads, retry };
}

const baseName = (path) => String(path || '').split('/').filter(Boolean).pop() || String(path || '');

// PageFrame is the sandboxed frame itself. Its attributes are the boundary:
// a sandbox without allow-same-origin, allow-popups, allow-top-navigation* or
// allow-downloads; no permissions; no referrer; credentialless.
export function PageFrame({ src, label, reloadKey }) {
  if (!src) return html`<div class="page-frame-empty">This page has no address on this dashboard.</div>`;
  return html`<iframe key=${reloadKey} class="page-frame" src=${src} title=${`Live page: ${label}`}
    sandbox=${PAGE_FRAME_SANDBOX} allow="" referrerpolicy="no-referrer" credentialless=${true} loading="eager"></iframe>`;
}

// PageView is the header and the frame. tools are the buttons for where the
// view is (move it elsewhere); onOpenSender opens the agent's pane, where
// there is one.
export function PageView({ pageId, snapshot, tools = null, onOpenSender = null, fetchImpl, notify, win = globalThis.window, WebSocketCtor }) {
  const value = snapshot?.value;
  const page = findLivePage(value, pageId);
  const live = usePageLive(pageId, page?.origin || '', { win, ...(WebSocketCtor && { WebSocketCtor }) });
  const [manual, setManual] = useState(0);
  const [closing, setClosing] = useState(false);
  if (!page) {
    return html`<div class="page-view page-view-closed" data-page=${pageId}>
      <div class="page-head"><span class="page-head-title">${value ? 'This page has closed.' : 'Loading the page…'}</span><span class="spacer"></span>${tools}</div>
    </div>`;
  }
  const label = pageLabel(page);
  const sender = pageSender(value, page);
  const rules = pageSandboxRules(page);
  const src = pageFrameSrc(page);
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
      <span class="ui-tag page-sandbox-chip" tabIndex="0" role="note" title=${rules.join('\n')} aria-label=${`Sandboxed. ${rules.join(' ')}`}>🔒 sandboxed</span>
      <span class="spacer"></span>
      <${Button} size="sm" class="mux-btn" title="Reload the page" onClick=${() => setManual((n) => n + 1)}>Reload<//>
      ${tools}
      <${Button} size="sm" variant="danger" class="mux-btn" busy=${closing} disabled=${closing}
        title="Close this page: it ends, and the agent that showed it is told" onClick=${() => void close()}>Close<//>
    </div>
    <div class="page-body">
      <${PageFrame} src=${src} label=${label} reloadKey=${`${live.reloads}:${manual}:${src}`} />
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
export function PagePane({ pane, active, solo = false, manageTitle = false, actions, snapshot, scope = null, fetchImpl, notify, collab = true }) {
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
    : html`${collab ? html`<${Button} size="sm" class="mux-btn" title="Float this page in a window over the dashboard" onClick=${() => void actions.openPaneInOverlay(pane.key)}>Open in overlay<//>` : null}
      <${Button} size="sm" class="mux-btn" title="Move this page to a browser window of its own" onClick=${() => void actions.popOutPane(pane.key, { floats: collab })}>Pop out<//>`;
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
    controls=${{ look, closeLabel: `Close ${label}`, closeTitle: close.title, onClose: close.onClose, onMinimize: () => minimizeOverlayWindow(instance.id), onCollab: snapshot.value?.collab_enabled === true ? toCollab : null }}>
    <${PageView} pageId=${instance.page} snapshot=${snapshot} tools=${tools} fetchImpl=${fetchImpl} notify=${notify} win=${win}
      onOpenSender=${(sender) => void openWebWindowPane(sender.agent, sender.name, { harness: sender.harness })} />
  <//>`;
}
