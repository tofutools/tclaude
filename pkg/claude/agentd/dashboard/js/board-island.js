// board-island.js -- the collab board (tcl-4f86kp): a shared drawing board the
// operator and agents edit together. A board opens as a tab of the terminal
// shell (terminal-shell-island.js), beside the terminals. See board-model.js
// for the element model and merge rules.
//
// The editor keeps every element in a Map, renders it as SVG, and syncs with
// the daemon: local edits are batched into PATCH /api/boards/{id}, and GET
// ?since=<version> merges what agents changed. A socket says when to read
// (GET /api/boards/{id}/live); without one the editor polls. When the operator's edits
// settle, the board's participating agents get one short text event describing
// them (POST /api/boards/{id}/events) with a PNG of the changed region, and a
// PNG snapshot of the whole board is uploaded so agents can look at the board
// as well as read it.
//
// Element text is agent- or operator-supplied and is rendered as SVG text.

import { h } from 'preact';
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  BOARD_FONTS, BOARD_TEXT_STYLE, boardFont, boardFontSizes, sameTextStyle, textStyleOf,
  ALIGNMENTS, BOARD_COLORS, BOARD_FILLS, BOARD_TOOLS, LINE_ROUTES, alignMoves, attachedText, bindingAt, boardGrid, boardTopZ, bump, changeBounds,
  boardToolDigit, cleanText, clipboardText, cloneElements, contentBounds, describeChanges, detachedLines, drawnElements, dropDirection, elementBounds, elementsInBox,
  entryPoint, labelOf, labelOwners, labelPoint, liveElements, mergeElements, newElement, newLabel, nextShapeSpot, normalizeBox, parseClipboard,
  QUICK_SHAPES, reboundArrows, routeConnector, shapeInside, shapeNear, sideOf, simplifyStroke, snapHit, snapPoints, snapPosition, stackOrder, suggestedShape, toExcalidraw, topHit, withLabels,
  BOARD_ZOOM_MAX, BOARD_ZOOM_MIN, boardAge, clampBoardZoom, groupBoards, wheelIsMouse, wheelPan, wheelPixels,
} from './board-model.js';
import { BoardContextMenu } from './board-context-menu.js';
import { announceBoardDeleted, askDeleteBoard, boardIsGone, exportBoard, freeBoardId, importBoardFile } from './board-manage.js';
import { clipboardReadNote } from './insecure-origin.js';
import { BoardMinimap } from './board-minimap.js';
import { BoardQuickMenu, QUICK_MENU_SIZE } from './board-quick-menu.js';
import { companionAgents } from './companion-model.js';
import { useDismissablePopover } from './hover-popover.js';

const html = htm.bind(h);

// How close the pointer must be, in screen pixels, to a snap point to take it,
// and to a shape to show its snap points.
const SNAP_RADIUS_PX = 10;
const SNAP_MARGIN_PX = 16;
// A press that wanders less than this is a click, not a drag.
const DRAG_THRESHOLD_PX = 4;
// A second press this soon on a clicked snap point is a double-click on the shape.
const DOUBLE_CLICK_MS = 400;
// An arrow key connects from the selected shape's snap point on that side.
const ARROW_SIDES = { ArrowUp: [0.5, 0], ArrowRight: [1, 0.5], ArrowDown: [0.5, 1], ArrowLeft: [0, 0.5] };
// The canvas colour (--board-paper in board.css).
const BOARD_PAPER = '#fbfaf7';

export const BOARD_POLL_MS = 1000;
// With the socket up nothing is polled but a slow check that catches a missed
// signal, and a quicker one while presence badges need to age out.
export const BOARD_LIVE_CHECK_MS = 15000;
export const BOARD_LIVE_PRESENCE_MS = 3000;
export const BOARD_LIVE_SETTLE_MS = 100;
const BOARD_LIVE_RETRY_MS = 1000;
const BOARD_LIVE_RETRY_MAX_MS = 30000;
export const BOARD_FLUSH_MS = 250;
export const BOARD_EVENT_IDLE_MS = 1500;
// The daemon's limit: one event per board in this long (boardEventInterval).
export const BOARD_EVENT_RETRY_MS = 3000;
export const BOARD_SNAPSHOT_IDLE_MS = 2500;
export const BOARD_VIEW_REFRESH_MS = 30000;
// The daemon's limit for an event image (boardMaxRegionBytes).
const BOARD_EVENT_IMAGE_MAX = 2 << 20;
// Past this a batch goes without its undo steps rather than be refused.
const BOARD_MAX_STEPS_BODY = 12 * 1024 * 1024 - 1024;
// A request only outlives its page with keepalive, and a browser allows a page
// 64 KiB of such bodies in flight. Part of that stays free for prefs.js.
export const BOARD_KEEPALIVE_MAX = 60 * 1024;
// How long a page about to go waits for its editors' saves.
export const BOARD_SETTLE_MS = 3000;

// Keys aimed at nothing in particular (focus on the body, as after clicking the
// canvas) go to the editor last used, so two open editors never both act.
let lastEditor = null;

// Keepalive body bytes in flight, shared by every editor in the page: the
// browser's allowance is the page's.
let keepaliveBytes = 0;

// The editors mounted in this page, for settleBoardEdits.
const openEditors = new Set();

// settleBoardEdits saves what the page's editors hold of a board, an open text
// edit included, and says whether all of it is saved. For a caller about to
// close the page: pagehide cannot wait, so a large edit would be lost.
export async function settleBoardEdits(boardId, timeoutMs = BOARD_SETTLE_MS) {
  const settling = [...openEditors].filter((editor) => editor.boardId === boardId).map((editor) => editor.settle());
  if (!settling.length) return true;
  let timer = null;
  const expired = new Promise((resolve) => { timer = setTimeout(() => resolve([false]), timeoutMs); });
  const results = await Promise.race([Promise.all(settling), expired]);
  clearTimeout(timer);
  return results.every(Boolean);
}

// rasterizeBoard draws the board for its agents; board-export.js loads on the
// first use.
export async function rasterizeBoard(svg, box) {
  const { svgToPng } = await import('./board-export.js');
  return svgToPng(svg, box);
}

const WHEEL_SCROLL_GAP_MS = 150;

async function jsonOrThrow(response) {
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(body.message || body.error || `${response.status} ${response.statusText}`);
    error.status = response.status;
    error.code = body.code;
    throw error;
  }
  return body;
}

// Holding this attaches a line's end to a shape's border instead of a snap
// point: Ctrl, or Cmd on a Mac, where Ctrl+click is a right click.
const borderHeld = (event) => !!(event.ctrlKey || event.metaKey);
const borderKeyName = (win) => (/mac/i.test(win?.navigator?.platform || '') ? 'Cmd' : 'Ctrl');

// -- rendering ---------------------------------------------------------------

function smoothPath(points) {
  if (!points.length) return '';
  if (points.length < 3) return `M${points.map(([x, y]) => `${x},${y}`).join(' L')}`;
  let d = `M${points[0][0]},${points[0][1]}`;
  for (let i = 1; i < points.length - 1; i++) {
    const [x, y] = points[i];
    const [nx, ny] = points[i + 1];
    d += ` Q${x},${y} ${(x + nx) / 2},${(y + ny) / 2}`;
  }
  const last = points[points.length - 1];
  return `${d} L${last[0]},${last[1]}`;
}

// Element colours come from agents too: only plain colour values reach the
// SVG attributes and the text editor's inline style.
const SAFE_COLOR = /^(#[0-9a-f]{3,8}|transparent)$/i;
export function safeColor(value, fallback) {
  return typeof value === 'string' && SAFE_COLOR.test(value) ? value : fallback;
}

function dash(el) {
  if (el.strokeStyle === 'dashed') return '8 6';
  if (el.strokeStyle === 'dotted') return '2 5';
  return null;
}

const ZOOM_STEP = 1.25; // a click or key of the zoom controls

const TEXT_ANCHOR = { center: 'middle', right: 'end' };

export function BoardElement({ el, flash = false, onLine = false, editing = false }) {
  const stroke = safeColor(el.strokeColor, '#1e1e1e');
  const background = safeColor(el.backgroundColor, 'transparent');
  const fill = background !== 'transparent' ? background : 'none';
  const common = {
    stroke, 'stroke-width': el.strokeWidth || 2, 'stroke-dasharray': dash(el), 'stroke-linecap': 'round',
    'stroke-linejoin': 'round', opacity: (el.opacity ?? 100) / 100, 'data-element': el.id,
    class: `board-el${flash ? ' board-flash' : ''}${editing ? ' board-editing' : ''}`,
  };
  const { x, y, width: w, height: hgt } = el;
  switch (el.type) {
    case 'rectangle':
      return html`<rect ...${common} x=${x} y=${y} width=${w} height=${hgt} rx=${Math.min(10, w / 4, hgt / 4)} fill=${fill} />`;
    case 'ellipse':
      return html`<ellipse ...${common} cx=${x + w / 2} cy=${y + hgt / 2} rx=${w / 2} ry=${hgt / 2} fill=${fill} />`;
    case 'diamond':
      return html`<polygon ...${common} points=${`${x + w / 2},${y} ${x + w},${y + hgt / 2} ${x + w / 2},${y + hgt} ${x},${y + hgt / 2}`} fill=${fill} />`;
    case 'line':
      return html`<path ...${common} d=${`M${el.points.map(([px, py]) => `${x + px},${y + py}`).join(' L')}`} fill="none" />`;
    case 'arrow': {
      const pts = el.points.map(([px, py]) => [x + px, y + py]);
      const [ax, ay] = pts[pts.length - 2] || pts[0];
      const [bx, by] = pts[pts.length - 1];
      const angle = Math.atan2(by - ay, bx - ax);
      const size = 10 + (el.strokeWidth || 2) * 1.5;
      const head = [angle - 0.45, angle + 0.45].map((a) => `${bx - size * Math.cos(a)},${by - size * Math.sin(a)}`);
      return html`<g ...${common}>
        <path d=${`M${pts.map(([px, py]) => `${px},${py}`).join(' L')}`} fill="none" />
        <path d=${`M${head[0]} L${bx},${by} L${head[1]}`} fill="none" />
      </g>`;
    }
    case 'freedraw':
      return html`<path ...${common} d=${smoothPath(el.points.map(([px, py]) => [x + px, y + py]))} fill="none" />`;
    case 'text': {
      const size = el.fontSize || 20;
      const lines = String(el.text || '').split('\n');
      const box = elementBounds(el);
      const lineX = el.textAlign === 'center' ? x + box.width / 2 : el.textAlign === 'right' ? x + box.width : x;
      // Text on a line masks the line behind it, so it stays readable.
      const halo = onLine ? { stroke: BOARD_PAPER, 'stroke-width': 5, 'paint-order': 'stroke' } : { stroke: 'none' };
      const { fontWeight, fontStyle } = textStyleOf(el);
      return html`<text ...${common} ...${halo} fill=${stroke} x=${lineX} y=${y + size} font-size=${size} text-anchor=${TEXT_ANCHOR[el.textAlign] || null}
        font-family=${boardFont(el).css} font-weight=${fontWeight || null} font-style=${fontStyle || null} xml:space="preserve">
        ${lines.map((line, i) => html`<tspan key=${i} x=${lineX} dy=${i ? size * 1.25 : 0}>${line || ' '}</tspan>`)}
      </text>`;
    }
    default:
      return null;
  }
}

// gridStyle lays the paper's dots on the board, so they stay put under what is
// drawn however the view moves. A dot sits in the middle of its tile.
function gridStyle(view) {
  const { size, x, y } = boardGrid(view);
  return `background-size:${size}px ${size}px;background-position:${x - size / 2}px ${y - size / 2}px`;
}

// -- the editor ---------------------------------------------------------------

// BoardEditor is one board's editor. A tab keeps it mounted while another tab
// or view shows: `onScreen` says it is the tab showing, and `visible` whether
// its surroundings are. headActions are the host's own controls.
export function BoardEditor({
  boardId, fetchImpl, notify = () => {}, snapshot, win, WebSocketCtor = win?.WebSocket, rasterize = rasterizeBoard, onOpenAgent,
  onScreen = true, visible = null, headActions = null, onTitle = null,
}) {
  const [gone, setGone] = useState(false);
  const wheelScroll = useRef({ at: -Infinity, mouse: false });
  const elements = useRef(new Map());
  // The live elements as drawn, by id: lines on their shapes and text on its
  // container. One Map refilled on each change, as the minimap expects.
  const drawnMap = useRef(new Map());
  const version = useRef(0);
  const [tick, setTick] = useState(0);
  const redraw = useCallback(() => setTick((n) => n + 1), []);
  const [meta, setMeta] = useState({ title: boardId, participants: [], group: '', groupMembers: [] });
  const [status, setStatus] = useState('loading');
  const [tool, setTool] = useState('select');
  const [color, setColor] = useState(BOARD_COLORS[0]);
  const [fill, setFill] = useState(BOARD_FILLS[0]);
  const [routing, setRouting] = useState('straight'); // for the next line drawn
  const [textStyle, setTextStyle] = useState(BOARD_TEXT_STYLE); // for the next text
  const [hoverShape, setHoverShape] = useState(''); // id of the shape showing its snap points
  const [selected, setSelected] = useState([]); // ids
  const pointerAt = useRef(null); // the last board point under the pointer, for paste
  const [presence, setPresence] = useState(() => new Map()); // agent id -> drawing | looking
  const viewSent = useRef({ key: '', at: 0 });
  // This editor's token: the daemon keeps one view per open editor.
  const viewClient = useRef(Math.random().toString(36).slice(2, 12));
  const [view, setView] = useState({ x: -40, y: -40, zoom: 1 });
  const viewRef = useRef(view);
  viewRef.current = view;
  const [editingText, setEditingTextState] = useState(null); // {id, x, y, text, isNew}
  // finishText can run twice for one edit (Enter, then the textarea's blur as
  // it unmounts); the ref lets the second call see the edit already finished.
  const editingRef = useRef(null);
  const setEditingText = (value) => { editingRef.current = value; setEditingTextState(value); };
  const textInputRef = useRef(null);
  const [flash, setFlash] = useState(() => new Set());
  const [invite, setInvite] = useState(false);
  const inviteRef = useRef(null);
  useDismissablePopover(inviteRef, invite, () => setInvite(false));
  const svgRef = useRef(null);
  const pending = useRef(new Map()); // id -> element awaiting PATCH
  const saving = useRef(0); // PATCHes in flight
  const refused = useRef(false); // pending edits the daemon refused as a batch
  // Elements sent by a PATCH with no answer: it may have landed, so a retry
  // that loses the merge to them says nothing.
  const resent = useRef(new WeakSet());
  const active = useRef(new Set()); // ids being manipulated right now
  // Undo steps made since the last PATCH. History lives on the daemon, so it
  // survives a reload and is shared by every editor the operator has open.
  const pendingSteps = useRef([]);
  const saved = useRef(Promise.resolve());
  const undoing = useRef(Promise.resolve());
  const eventBatch = useRef({ ids: [], before: new Map() });
  const timers = useRef({});
  const gesture = useRef(null);
  // A snap point's offer: {from, fixedPoint, at}, see suggestion(). With `menu`
  // it stays at `at` while the quick menu picks its `kind`.
  const suggesting = useRef(null);
  const lastKind = useRef(''); // the kind quick connect last made
  const [contextMenu, setContextMenu] = useState(null); // {x, y, p}: page and board point
  const copied = useRef(''); // the last copy, for a paste the browser will not read
  const disposed = useRef(false);
  // Timer callbacks read the latest participants, not their render's.
  const metaRef = useRef(meta);
  metaRef.current = meta;

  const rootRef = useRef(null);
  const editorToken = useRef({}).current;
  // Timer and listener callbacks read the latest props, not their render's.
  const shownRef = useRef(null);
  shownRef.current = () => onScreen && (!visible || visible());
  const onTitleRef = useRef(onTitle);
  onTitleRef.current = onTitle;
  // Input and polling only apply while this editor is on screen.
  const shown = () => shownRef.current();

  useEffect(() => {
    if (onScreen) lastEditor = editorToken;
  }, [onScreen]);
  // Another tool, or leaving the board, takes back a snap point's offer.
  useEffect(() => {
    setContextMenu(null);
    if (!suggesting.current) return;
    suggesting.current = null;
    redraw();
  }, [tool, onScreen]);
  useEffect(() => () => {
    if (lastEditor === editorToken) lastEditor = null;
  }, []);

  const agents = useMemo(() => companionAgents(snapshot.value), [snapshot.value]);

  // Load, then read again whenever the socket says the board changed, or on
  // the poll while there is no socket.
  useEffect(() => {
    disposed.current = false;
    let pollTimer = null;
    let liveOpen = false;
    let liveSocket = null;
    let liveRetry = null;
    let liveDelay = BOARD_LIVE_RETRY_MS;
    let stale = false;
    let retry = false;
    let started = false;
    let ticking = false;
    let settling = false;
    let lastLoad = 0;
    let presenceShown = false;
    let missing = false;
    const load = async (since) => {
      try {
        const query = since ? `?since=${since}` : '';
        const board = await jsonOrThrow(await fetchImpl(`/api/boards/${encodeURIComponent(boardId)}${query}`, { credentials: 'same-origin' }));
        if (disposed.current) return;
        setMeta({
          title: board.title || boardId, participants: Array.isArray(board.participants) ? board.participants : [],
          group: board.group || '', groupMembers: Array.isArray(board.group_members) ? board.group_members : [],
        });
        if (board.title) onTitleRef.current?.(board.title);
        setPresence(new Map((board.presence?.agents || []).map((a) => [a.agent_id, a.state])));
        presenceShown = !!board.presence?.agents?.length;
        const skip = new Set([...active.current, ...pending.current.keys()]);
        const incoming = board.elements || [];
        const changed = mergeElements(elements.current, incoming, skip);
        // An element skipped because the operator holds it must come again on
        // the next poll, so the cursor only moves past fully merged responses.
        const skipped = incoming.some((el) => skip.has(el?.id));
        if (skipped) retry = true;
        if (Number.isFinite(board.version) && !skipped) version.current = Math.max(version.current, board.version);
        if (changed.length) {
          if (since) {
            const remote = changed.filter((id) => elements.current.get(id)?.customData?.author !== 'operator');
            if (remote.length) {
              setFlash(new Set(remote));
              clearTimeout(timers.current.flash);
              timers.current.flash = setTimeout(() => setFlash(new Set()), 1600);
              scheduleSnapshot();
            }
          }
          redraw();
        }
        setStatus('ready');
      } catch (error) {
        retry = true;
        if (disposed.current) return;
        if (boardIsGone(error)) {
          missing = true;
          setGone(true);
          notify(`The board ${boardId} was deleted.`);
          announceBoardDeleted(boardId);
          return;
        }
        setStatus(since ? 'offline' : `error: ${error?.message || error}`);
      }
    };
    const due = () => !liveOpen || stale || retry
      || Date.now() - lastLoad >= (presenceShown ? BOARD_LIVE_PRESENCE_MS : BOARD_LIVE_CHECK_MS);
    const tick = async () => {
      if (disposed.current || missing || ticking) return;
      ticking = true;
      settling = false;
      clearTimeout(pollTimer);
      const looking = win?.document?.visibilityState !== 'hidden' && shown();
      void reportView(looking);
      if (looking && due()) {
        stale = false;
        retry = false;
        lastLoad = Date.now();
        await load(version.current);
      }
      ticking = false;
      if (!disposed.current) pollTimer = setTimeout(tick, stale && looking && liveOpen ? BOARD_LIVE_SETTLE_MS : BOARD_POLL_MS);
    };
    // Signals arrive in bursts while an agent draws; a short wait reads once.
    // An armed wait is left alone, or a steady stream would never read.
    const signal = () => {
      stale = true;
      if (!started || ticking || settling) return;
      settling = true;
      clearTimeout(pollTimer);
      pollTimer = setTimeout(tick, BOARD_LIVE_SETTLE_MS);
    };
    const connect = () => {
      // A peer's page rewrites fetches, which a socket would not follow.
      if (disposed.current || !WebSocketCtor || globalThis.__tclaudeRemoteNode) return;
      const scheme = win.location?.protocol === 'https:' ? 'wss:' : 'ws:';
      try {
        liveSocket = new WebSocketCtor(`${scheme}//${win.location.host}/api/boards/${encodeURIComponent(boardId)}/live`);
      } catch {
        return;
      }
      // Opening reads too: whatever landed while there was no socket.
      liveSocket.onopen = () => { liveOpen = true; liveDelay = BOARD_LIVE_RETRY_MS; signal(); };
      liveSocket.onmessage = (event) => {
        let message = null;
        try { message = JSON.parse(event.data); } catch { /* every read is safe */ }
        // Its own edit is no news, but the cursor and the status still move
        // on a read, so it comes on the next tick rather than straight away.
        if (message && message.client === viewClient.current) retry = true;
        else signal();
      };
      liveSocket.onclose = () => {
        liveOpen = false;
        liveSocket = null;
        if (disposed.current || missing) return;
        liveRetry = setTimeout(connect, liveDelay);
        liveDelay = Math.min(liveDelay * 2, BOARD_LIVE_RETRY_MAX_MS);
      };
    };
    connect();
    const editor = { boardId, settle };
    openEditors.add(editor);
    win?.addEventListener?.('pagehide', saveBeforeLeaving);
    void load(0).then(() => {
      lastLoad = Date.now();
      started = true;
      pollTimer = setTimeout(tick, stale && liveOpen ? BOARD_LIVE_SETTLE_MS : BOARD_POLL_MS);
    });
    return () => {
      disposed.current = true;
      openEditors.delete(editor);
      win?.removeEventListener?.('pagehide', saveBeforeLeaving);
      clearTimeout(pollTimer);
      clearTimeout(liveRetry);
      liveSocket?.close();
      void reportView(false);
      // Text still being typed is an edit, and an editor going away gets no blur
      // to finish it.
      finishText();
      const eventPending = !!timers.current.event;
      for (const timer of Object.values(timers.current)) clearTimeout(timer);
      void flush();
      // Leaving the board still tells its agents what the last edits were.
      if (eventPending) void sendEvent();
    };
  }, [boardId]);

  // reportView tells the daemon which part of the board the operator has on
  // screen (tcl-rawj2c), so an agent can read "this" with `board show
  // --view`: on a change, every BOARD_VIEW_REFRESH_MS while it stays, and
  // cleared when the operator looks away.
  async function reportView(looking) {
    const rect = svgRef.current?.getBoundingClientRect?.();
    const v = viewRef.current;
    const box = looking && rect?.width && rect?.height
      ? { x: Math.round(v.x), y: Math.round(v.y), width: Math.max(1, Math.round(rect.width / v.zoom)), height: Math.max(1, Math.round(rect.height / v.zoom)) }
      : null;
    const key = box ? `${box.x},${box.y},${box.width},${box.height}` : '';
    const last = viewSent.current;
    if (key === last.key && (!key || Date.now() - last.at < BOARD_VIEW_REFRESH_MS)) return;
    viewSent.current = { key, at: Date.now() };
    try {
      await fetchImpl(`/api/boards/${encodeURIComponent(boardId)}/view`, {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ client: viewClient.current, ...(box || {}) }),
      });
    } catch { /* presence is best-effort */ }
  }

  // One PATCH at a time, so steps reach the history in the order they were made.
  function flush() {
    clearTimeout(timers.current.flush);
    const next = () => saveBatch();
    saved.current = saved.current.then(next, next);
    return saved.current;
  }

  // settle saves everything held here, retries included, and says whether all
  // of it was saved.
  async function settle() {
    finishText();
    void flush();
    let waited = null;
    do {
      waited = saved.current;
      await waited;
    } while (waited !== saved.current);
    return !pending.current.size;
  }

  // saveBeforeLeaving is the last chance on a page going away. It sends now, not
  // behind `saved`, which would only move on after the page had gone; a PATCH
  // already in flight is keepalive too, so the daemon may get the two in either
  // order. Should the page come back from the back/forward cache, a failed save
  // is kept and retried as usual.
  function saveBeforeLeaving() {
    finishText();
    clearTimeout(timers.current.flush);
    saved.current = Promise.all([saved.current, saveBatch(true)]).then(() => {});
  }

  async function saveBatch(leaving = false) {
    if (!pending.current.size) return;
    const batch = [...pending.current.values()];
    pending.current.clear();
    const steps = pendingSteps.current;
    pendingSteps.current = [];
    const keep = () => {
      for (const el of batch) if (!pending.current.has(el.id)) pending.current.set(el.id, el);
      pendingSteps.current = [...steps, ...pendingSteps.current];
    };
    let body = JSON.stringify({ elements: batch, steps });
    let size = new Blob([body]).size;
    const room = BOARD_KEEPALIVE_MAX - keepaliveBytes;
    // On a page going away a save that fits matters more than its undo steps.
    if (size > BOARD_MAX_STEPS_BODY || (leaving && size > room)) {
      body = JSON.stringify({ elements: batch });
      size = new Blob([body]).size;
      if (!leaving && !disposed.current) notify('That edit is too large to undo.', false);
    }
    // Every save that fits goes keepalive, or one in flight when the page goes
    // would be cut off, and resending it could save it twice. Past the limit it
    // goes as a plain request, which may not outlive the page.
    const keepalive = size <= room;
    if (keepalive) keepaliveBytes += size;
    saving.current += 1;
    try {
      const result = await jsonOrThrow(await fetchImpl(`/api/boards/${encodeURIComponent(boardId)}`, {
        method: 'PATCH', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-Board-Client': viewClient.current },
        body, keepalive,
      }));
      // The poll cursor is not advanced from here: agent edits that landed
      // before this PATCH have versions below result.version.
      refused.current = false;
      const lost = Number.isFinite(result.accepted) ? batch.length - result.accepted : 0;
      if (lost > 0 && !batch.some((el) => resent.current.has(el))) {
        if (!result.accepted) forgetEdits(batch.map((el) => el.id));
        if (!disposed.current) notify(`The board kept a newer version of ${lost === 1 ? 'a shape' : `${lost} shapes`}: your ${lost === 1 ? 'edit was' : 'edits were'} not saved.`, true);
      }
    } catch (error) {
      if (error?.status === 400 || error?.status === 413) {
        const bad = /^elements\[(\d+)\]/.exec(error.message || '');
        // A batch over the size limit only grows as edits merge into it, so
        // it would hold back every later save.
        if (error.status === 413) {
          forgetEdits(batch.map((el) => el.id));
          rollBack(batch.map((el) => el.id), steps);
          if (!disposed.current) notify(`The board refused your latest edits as too large, so they were not saved: ${error.message}`, true);
          return;
        }
        if (!bad) {
          // Refused as a whole for too many elements, so no one edit is at
          // fault and deleting shapes can make room. Keep them all; the next
          // change retries them, and until then the status says so.
          keep();
          refused.current = true;
          if (!disposed.current) {
            notify(`The board refused your latest edits, so they are not saved yet: ${error.message}`, true);
            redraw();
          }
          return;
        }
        // A refused element is dropped, with the rest retried at once:
        // retrying it would hold every later edit back for good.
        const dropped = batch[Number(bad[1])]?.id;
        forgetEdits([dropped]);
        rollBack([dropped], steps);
        if (!disposed.current) notify(`The board refused an edit: ${error.message}`, true);
        for (const el of batch) if (el.id !== dropped && !pending.current.has(el.id)) pending.current.set(el.id, el);
        const left = steps.map((step) => step.filter(({ id }) => id !== dropped)).filter((step) => step.length);
        pendingSteps.current = [...left, ...pendingSteps.current];
        if (pending.current.size) void flush();
        return;
      }
      // Keep the edits; the next change or flush retries them.
      for (const el of batch) resent.current.add(el);
      keep();
      if (!disposed.current) {
        setStatus('offline');
        clearTimeout(timers.current.flush);
        timers.current.flush = setTimeout(() => void flush(), 2000);
      }
    } finally {
      saving.current -= 1;
      if (keepalive) keepaliveBytes -= size;
    }
  }

  // rollBack puts refused edits back as they were: they never reached the
  // daemon, so nothing could undo them later. A newer edit in hand stays.
  function rollBack(ids, steps) {
    for (const id of ids) {
      if (pending.current.has(id) || active.current.has(id)) continue;
      const entry = steps.flat().find((each) => each.id === id);
      if (!entry) continue;
      if (entry.before) elements.current.set(id, entry.before);
      else elements.current.delete(id);
    }
    if (!disposed.current) redraw();
  }

  // forgetEdits takes edits the board did not keep out of the agents' event.
  function forgetEdits(ids) {
    const { ids: batched, before } = eventBatch.current;
    for (const id of ids) before.delete(id);
    eventBatch.current.ids = batched.filter((id) => !ids.includes(id));
  }

  function scheduleSnapshot() {
    clearTimeout(timers.current.snapshot);
    timers.current.snapshot = setTimeout(async () => {
      const box = contentBounds(liveElements(elements.current));
      if (!box || !svgRef.current || !rasterize) return;
      const png = await rasterize(svgRef.current, box);
      if (!png || disposed.current) return;
      try {
        await fetchImpl(`/api/boards/${encodeURIComponent(boardId)}/snapshot?version=${version.current}`, {
          method: 'PUT', credentials: 'same-origin', headers: { 'Content-Type': 'image/png' }, body: png,
        });
      } catch { /* the next change retries */ }
    }, BOARD_SNAPSHOT_IDLE_MS);
  }

  function scheduleEvent() {
    clearTimeout(timers.current.event);
    timers.current.event = setTimeout(sendEvent, BOARD_EVENT_IDLE_MS);
  }

  async function sendEvent() {
    timers.current.event = null;
    // An event only announces saved edits. A closed editor retries nothing, so
    // what it could not save goes unannounced.
    if (saving.current || pending.current.size) {
      if (!disposed.current || saving.current) timers.current.event = setTimeout(sendEvent, BOARD_FLUSH_MS);
      return;
    }
    const { ids, before } = eventBatch.current;
    eventBatch.current = { ids: [], before: new Map() };
    const { participants, groupMembers } = metaRef.current;
    if (!participants.length && !groupMembers.length) return;
    const text = describeChanges(elements.current, ids, before);
    if (!text) return;
    const event = { text: `The operator ${text}.` };
    // The changed region as an image (tcl-vs77ee), so agents can see exactly
    // what changed. The event goes out without it if it cannot be drawn.
    const box = changeBounds(elements.current, ids, before);
    if (box && svgRef.current && rasterize) {
      try {
        const png = await rasterize(svgRef.current, box);
        if (png && png.size <= BOARD_EVENT_IMAGE_MAX) event.image = await (await import('./board-export.js')).blobToBase64(png);
      } catch { /* text only */ }
    }
    if (await postEvent(event) !== 429) return;
    // The daemon is still limiting this board's last event. A closed editor can
    // only send this one again, drawn image and all. An open one puts the edits
    // back in with any newer ones, so the retry describes them together.
    if (disposed.current) {
      setTimeout(() => void postEvent(event), BOARD_EVENT_RETRY_MS);
      return;
    }
    const newer = eventBatch.current;
    eventBatch.current = { ids: [...ids, ...newer.ids], before: new Map([...newer.before, ...before]) };
    clearTimeout(timers.current.event);
    timers.current.event = setTimeout(sendEvent, BOARD_EVENT_RETRY_MS);
  }

  // postEvent answers the response status, or 0 if none came. Other than a 429,
  // events are best-effort.
  async function postEvent(event) {
    try {
      const response = await fetchImpl(`/api/boards/${encodeURIComponent(boardId)}/events`, {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(event),
      });
      return response.status;
    } catch {
      return 0;
    }
  }

  // commit records operator edits: the merged map, the undo step, the PATCH
  // batch, the agents' event batch and the snapshot.
  function commit(next, { record = true } = {}) {
    if (!next.length) return;
    const step = [];
    const top = boardTopZ(elements.current);
    for (const el of next) {
      const prev = elements.current.get(el.id);
      step.push({ id: el.id, before: prev || null });
      if (!eventBatch.current.before.has(el.id)) eventBatch.current.before.set(el.id, prev);
      eventBatch.current.ids.push(el.id);
      // server_version is the daemon's read stamp, not element data.
      const { server_version: _stamp, ...data } = el;
      const authored = { ...data, ...(!prev && top ? { z: top + 1 } : {}), customData: { ...(el.customData || {}), author: 'operator' } };
      elements.current.set(el.id, authored);
      pending.current.set(el.id, authored);
    }
    if (record) pendingSteps.current.push(step);
    clearTimeout(timers.current.flush);
    timers.current.flush = setTimeout(() => void flush(), BOARD_FLUSH_MS);
    scheduleEvent();
    scheduleSnapshot();
    redraw();
  }

  function withArrows(changed) {
    const ids = changed.map((el) => el.id);
    const temp = new Map(elements.current);
    for (const el of changed) temp.set(el.id, el);
    // A moved arrow that is also bound to a moved shape is saved once.
    const out = new Map(changed.map((el) => [el.id, el]));
    for (const arrow of reboundArrows(temp, ids)) { out.set(arrow.id, arrow); temp.set(arrow.id, arrow); }
    // Text attached to anything that moved goes with it, the lines included.
    for (const text of attachedText(temp, [...out.keys()])) out.set(text.id, text);
    return [...out.values()];
  }

  // Presses run in turn, each after everything before it has been saved.
  function undo(redo = false) {
    if (gesture.current) return undoing.current;
    undoing.current = undoing.current.then(() => undoStep(redo));
    return undoing.current;
  }

  // A batch the board refused never reached the daemon's history, and undoing
  // it may be what makes it fit, so it is undone here.
  function undoRefused() {
    const step = pendingSteps.current.pop();
    // What the step created never reached the daemon, so it just goes.
    const created = step.filter(({ before }) => !before).map(({ id }) => id);
    for (const id of created) {
      pending.current.delete(id);
      elements.current.delete(id);
    }
    forgetEdits(created);
    commit(step.filter(({ before }) => before).map(({ id, before }) => {
      const cur = elements.current.get(id);
      return bump(cur || before, { fontFamily: undefined, fontWeight: undefined, fontStyle: undefined, ...before, version: cur?.version || before.version });
    }), { record: false });
    if (!pending.current.size) refused.current = false;
    setSelected([]);
    redraw();
  }

  async function undoStep(redo) {
    if (!redo && refused.current && pendingSteps.current.length) {
      undoRefused();
      return;
    }
    await flush();
    if (disposed.current) return;
    if (pending.current.size) {
      notify('Your latest edits are not saved yet, so undo has to wait.', true);
      return;
    }
    try {
      const result = await jsonOrThrow(await fetchImpl(`/api/boards/${encodeURIComponent(boardId)}/undo`, {
        method: 'POST', credentials: 'same-origin', body: JSON.stringify({ redo }),
        headers: { 'Content-Type': 'application/json', 'X-Board-Client': viewClient.current },
      }));
      if (disposed.current) return;
      const incoming = result.elements || [];
      for (const el of incoming) {
        if (!eventBatch.current.before.has(el.id)) eventBatch.current.before.set(el.id, elements.current.get(el.id));
        eventBatch.current.ids.push(el.id);
      }
      // What the operator has in hand wins, and the next read brings the rest.
      const skip = new Set([...active.current, ...pending.current.keys()]);
      if (mergeElements(elements.current, incoming, skip).length) {
        scheduleEvent();
        scheduleSnapshot();
        redraw();
      }
      if (result.skipped) {
        notify(`Left ${result.skipped === 1 ? 'a shape' : `${result.skipped} shapes`} alone: ${result.skipped === 1 ? 'it has' : 'they have'} changed since.`, false);
      }
    } catch (error) {
      if (!disposed.current) notify(`Could not ${redo ? 'redo' : 'undo'}: ${error?.message || error}`, true);
    }
  }

  // remove deletes elements; a deleted shape takes its label with it, and the
  // lines attached to it stay where they are, free at that end.
  function remove(ids) {
    const gone = new Set(withLabels(elements.current, ids));
    const next = [...gone].map((id) => elements.current.get(id)).filter((el) => el && !el.isDeleted).map((el) => bump(el, { isDeleted: true }));
    commit([...next, ...detachedLines(elements.current, gone)]);
    setSelected([]);
  }

  // moveBy commits moves of whole elements (each with its label) and
  // rebinds the arrows on them. moves: Map(id -> {dx, dy}).
  function moveBy(moves) {
    const next = new Map();
    for (const [id, { dx, dy }] of moves) {
      for (const each of withLabels(elements.current, [id])) {
        const el = elements.current.get(each);
        if (el && !el.isDeleted && !next.has(each)) next.set(each, bump(el, { x: el.x + dx, y: el.y + dy }));
      }
    }
    commit(withArrows([...next.values()]));
  }

  const attached = (el) => !!(el.startBinding || el.endBinding);

  function align(how) {
    const owners = labelOwners(elements.current);
    const chosen = new Set(selected);
    // Labels follow their shapes and arrows follow their ends.
    const targets = selected.map((id) => elements.current.get(id))
      .filter((el) => el && !el.isDeleted && el.type !== 'arrow' && !attached(el) && !chosen.has(owners.get(el.id)));
    moveBy(alignMoves(targets, how));
  }

  // paste adds copies of elements at the pointer (or beside the originals)
  // and selects them.
  function pasteElements(source) {
    const box = contentBounds(source);
    if (!box) return;
    const at = pointerAt.current;
    const dx = at ? at.x - box.x : 20;
    const dy = at ? at.y - box.y : 20;
    const copies = cloneElements(source, dx, dy);
    commit(copies);
    setSelected(copies.map((el) => el.id));
  }

  // pasteText adds what the clipboard holds. Plain text becomes a text element.
  function pasteText(text) {
    const source = parseClipboard(text);
    if (source) { pasteElements(source); return; }
    const clean = cleanText(text);
    if (!clean.trim()) return;
    const at = pointerAt.current || { x: view.x + 40, y: view.y + 40 };
    const el = newElement('text', { ...textStyle, x: at.x, y: at.y, text: clean, strokeColor: color });
    commit([el]);
    setSelected([el.id]);
  }

  function duplicate() {
    const source = selectionWithLabels();
    if (!source.length) return;
    const copies = cloneElements(source, 20, 20);
    commit(copies);
    setSelected(copies.map((el) => el.id));
  }

  const selectAll = () => setSelected(liveElements(elements.current).map((el) => el.id));

  function chooseColor(c, chosen) {
    setColor(c);
    if (chosen.length) commit(chosen.map((el) => bump(el, { strokeColor: c })));
  }

  function chooseFill(c, chosen) {
    setFill(c);
    commit(chosen.filter((el) => ['rectangle', 'ellipse', 'diamond'].includes(el.type)).map((el) => bump(el, { backgroundColor: c })));
  }

  function selectionWithLabels() {
    return withLabels(elements.current, selected).map((id) => elements.current.get(id)).filter((el) => el && !el.isDeleted);
  }

  // -- pointer input ----------------------------------------------------------

  function toBoard(event) {
    const rect = svgRef.current?.getBoundingClientRect?.() || { left: 0, top: 0 };
    return { x: (event.clientX - rect.left) / view.zoom + view.x, y: (event.clientY - rect.top) / view.zoom + view.y };
  }

  function onPointerDown(event) {
    if (event.button !== 0 && event.button !== 1) return;
    if (editingText) { finishText(); return; }
    const p = toBoard(event);
    pointerAt.current = p;
    const svg = svgRef.current;
    svg?.setPointerCapture?.(event.pointerId);
    const live = drawnElements(elements.current);
    const panning = tool === 'hand' || event.button === 1;
    if (panning) {
      gesture.current = { kind: 'pan', sx: event.clientX, sy: event.clientY, view };
      return;
    }
    if (suggesting.current) {
      const { clicked } = suggesting.current;
      const again = clicked && event.timeStamp - clicked.at < DOUBLE_CLICK_MS && Math.hypot(p.x - clicked.x, p.y - clicked.y) * view.zoom < SNAP_RADIUS_PX;
      if (again) { suggesting.current = null; redraw(); } else acceptSuggestion(p);
      return;
    }
    const snapTol = SNAP_RADIUS_PX / view.zoom;
    const border = borderHeld(event);
    if (tool === 'select') {
      const line = selected.length === 1 && live.find((el) => el.id === selected[0] && (el.type === 'arrow' || el.type === 'line'));
      const end = line && lineEndAt(line, p, snapTol);
      if (end) {
        active.current = new Set([line.id]);
        gesture.current = { kind: 'end', id: line.id, end, drawn: line, orig: elements.current.get(line.id) };
        return;
      }
      const snap = !event.shiftKey && shownSnap(live, p, snapTol);
      if (snap) {
        // A drag draws a line from the snap point; a click offers one.
        beginLine('arrow', p, { startBinding: border ? { elementId: snap.elementId } : snap }, true);
        return;
      }
      // Holding the border key leaves the inside of a frame to the rubber band,
      // for what is drawn in it.
      const hit = pressTarget(live, p, !border);
      if (!hit) {
        // Empty space: a rubber band; with Shift it adds to the selection.
        if (!event.shiftKey) setSelected([]);
        gesture.current = { kind: 'band', start: p, now: p, base: event.shiftKey ? selected : [] };
        return;
      }
      if (event.shiftKey) {
        setSelected(selected.includes(hit.id) ? selected.filter((id) => id !== hit.id) : [...selected, hit.id]);
        return;
      }
      const ids = selected.includes(hit.id) ? selected : [hit.id];
      if (!selected.includes(hit.id)) setSelected(ids);
      const b = elementBounds(hit);
      const onHandle = ids.length === 1 && Math.abs(p.x - (b.x + b.width)) < 10 / view.zoom && Math.abs(p.y - (b.y + b.height)) < 10 / view.zoom && !hit.points && hit.type !== 'text';
      // A move carries the selection's labels; a resize only the shape.
      const movers = onHandle ? [hit.id] : withLabels(elements.current, ids);
      active.current = new Set(movers);
      const origs = new Map(movers.map((id) => [id, elements.current.get(id)]).filter(([, el]) => el && !el.isDeleted));
      gesture.current = { kind: onHandle ? 'resize' : 'move', id: hit.id, start: p, orig: elements.current.get(hit.id), origs };
      return;
    }
    if (tool === 'eraser') {
      gesture.current = { kind: 'erase', erased: new Set() };
      eraseAt(p);
      return;
    }
    if (tool === 'text') {
      const hit = topHit(live, p.x, p.y);
      if (hit?.type === 'text') beginText(hit);
      else beginText(null, p);
      return;
    }
    if (tool === 'arrow' || tool === 'line') {
      const start = bindingAt(live, p.x, p.y, snapTol, '', border);
      const fromPoint = tool === 'arrow' && !!shownSnap(live, p, snapTol);
      beginLine(tool, p, start ? { startBinding: start } : {}, fromPoint);
      return;
    }
    const el = newElement(tool, {
      x: p.x, y: p.y, strokeColor: color,
      backgroundColor: ['rectangle', 'ellipse', 'diamond'].includes(tool) ? fill : 'transparent',
    });
    // A pen stroke started on a snap point moves with that shape.
    const snap = tool === 'freedraw' ? shownSnap(live, p, snapTol) : null;
    if (snap) el.startBinding = snap;
    active.current = new Set([el.id]);
    elements.current.set(el.id, el);
    gesture.current = { kind: 'draw', id: el.id, start: p, raw: [[0, 0]] };
    redraw();
  }

  // pressTarget is what a press at p takes. Attached text is its container, and
  // inside an unfilled shape nothing is hit, but the shape is there to take.
  function pressTarget(live, p, inside = true) {
    const top = topHit(live, p.x, p.y, 6 / view.zoom);
    return (top?.containerId && live.find((el) => el.id === top.containerId)) || top || (inside && shapeInside(live, p.x, p.y)) || null;
  }

  // shownSnap is the snap point under the pointer, of the shape showing them.
  function shownSnap(live, p, tol) {
    const near = shapeNear(live, p.x, p.y, SNAP_MARGIN_PX / view.zoom);
    return near ? snapHit([near], p.x, p.y, tol) : null;
  }

  function beginLine(type, p, props, fromSnap = false) {
    const el = newElement(type, {
      x: p.x, y: p.y, strokeColor: color, backgroundColor: 'transparent', points: [[0, 0], [0, 0]],
      ...(routing !== 'straight' ? { routing } : {}), ...props,
    });
    active.current = new Set([el.id]);
    elements.current.set(el.id, el);
    gesture.current = { kind: 'draw', id: el.id, start: p, raw: [[0, 0]], fromSnap };
    redraw();
  }

  // aimLine points the line being drawn, or the end being dragged, at p: at what
  // it would attach to there, which it shows by doing so.
  function aimLine(g, p, border) {
    const el = elements.current.get(g.id);
    if (!el) return;
    g.at = p;
    g.border = border;
    const ignore = g.kind === 'end' ? g.orig[g.end === 'start' ? 'endBinding' : 'startBinding']?.elementId : el.startBinding?.elementId;
    g.target = bindingAt(liveElements(elements.current), p.x, p.y, SNAP_MARGIN_PX / view.zoom, ignore, border);
    setHoverShape(g.target?.elementId || '');
    if (g.kind === 'end') {
      elements.current.set(g.id, { ...lineWithEnd(g, p), [g.end === 'start' ? 'startBinding' : 'endBinding']: g.target || undefined });
      g.moved = true;
    } else {
      const dx = p.x - g.start.x; const dy = p.y - g.start.y;
      elements.current.set(g.id, { ...el, points: [[0, 0], [dx, dy]], width: Math.abs(dx), height: Math.abs(dy), endBinding: g.target || undefined });
    }
    redraw();
  }

  // lineEndAt says which end of a drawn line is under the point.
  function lineEndAt(line, p, tol) {
    const first = line.points[0]; const last = line.points[line.points.length - 1];
    if (Math.hypot(p.x - (line.x + last[0]), p.y - (line.y + last[1])) <= tol) return 'end';
    if (Math.hypot(p.x - (line.x + first[0]), p.y - (line.y + first[1])) <= tol) return 'start';
    return '';
  }

  // lineWithEnd is a line with one end moved to p, the other kept as drawn.
  function lineWithEnd(g, p) {
    const { drawn, end } = g;
    const first = drawn.points[0]; const last = drawn.points[drawn.points.length - 1];
    const a = end === 'start' ? p : { x: drawn.x + first[0], y: drawn.y + first[1] };
    const b = end === 'end' ? p : { x: drawn.x + last[0], y: drawn.y + last[1] };
    return {
      ...g.orig, x: a.x, y: a.y, width: Math.abs(b.x - a.x), height: Math.abs(b.y - a.y), points: [[0, 0], [b.x - a.x, b.y - a.y]],
      [end === 'start' ? 'startBinding' : 'endBinding']: undefined,
    };
  }

  // suggestion draws a snap point's offer: a line from it, to the shape under
  // the pointer, or to a ghost of the next shape.
  function suggestion() {
    const offer = suggesting.current;
    const from = offer && elements.current.get(offer.from);
    if (!from || from.isDeleted) { suggesting.current = null; return null; }
    const start = { elementId: from.id, fixedPoint: offer.fixedPoint };
    const end = !offer.menu && offer.at && bindingAt(liveElements(elements.current), offer.at.x, offer.at.y, SNAP_MARGIN_PX / view.zoom, from.id);
    const map = new Map(elements.current);
    const ghost = end ? null : { id: 'suggested', ...suggestedShape(from, offer.fixedPoint, offer.at, offer) };
    if (ghost) map.set(ghost.id, ghost);
    const line = {
      id: 'suggested-line', type: 'arrow', x: 0, y: 0, points: [[0, 0], [0, 0]], strokeColor: from.strokeColor, routing,
      startBinding: start, endBinding: end || { elementId: ghost.id, fixedPoint: entryPoint(ghost, snapPosition(from, offer.fixedPoint)) },
    };
    return { ghost, line: { ...line, ...routeConnector(line, map, true) } };
  }

  function acceptSuggestion(p) {
    if (p) suggesting.current.at = p;
    const offer = suggestion();
    if (suggesting.current?.menu && offer?.ghost) lastKind.current = offer.ghost.type;
    suggesting.current = null;
    if (!offer) { redraw(); return; }
    const { id: _ghostId, ...ghostProps } = offer.ghost || {};
    const shape = offer.ghost && newElement(offer.ghost.type, ghostProps);
    const { id: _lineId, routing: lineRouting, ...lineProps } = offer.line;
    const connector = newElement('arrow', {
      ...lineProps, ...(lineRouting !== 'straight' ? { routing: lineRouting } : {}),
      ...(shape ? { endBinding: { ...lineProps.endBinding, elementId: shape.id } } : {}),
    });
    commit(shape ? [shape, connector] : [connector]);
    setSelected([(shape || connector).id]);
    // The next connection starts here, with the pointer or the keyboard.
    if (shape) setHoverShape(shape.id);
  }

  // openQuickMenu asks what shape to make at the end of a connector.
  function openQuickMenu(from, fixedPoint, at, dir) {
    suggesting.current = { from: from.id, fixedPoint, at, dir, menu: true, kind: lastKind.current || from.type };
    redraw();
  }

  function closeQuickMenu() {
    if (!suggesting.current) return;
    suggesting.current = null;
    redraw();
  }

  // quickConnect offers a shape on that side of `from`, for the keyboard.
  function quickConnect(from, fixedPoint) {
    const { at, dir } = nextShapeSpot(from, fixedPoint, liveElements(elements.current));
    openQuickMenu(from, fixedPoint, at, dir);
    const rect = svgRef.current?.getBoundingClientRect?.();
    const sx = (at.x - view.x) * view.zoom; const sy = (at.y - view.y) * view.zoom;
    if (rect?.width && (sx < 0 || sy < 0 || sx > rect.width - QUICK_MENU_SIZE.width || sy > rect.height - QUICK_MENU_SIZE.height)) centerOn(at.x, at.y);
  }

  // chooseRouting sets the routing of the selected lines, and of the next.
  function chooseRouting(id, lines) {
    setRouting(id);
    commit(withArrows(lines.map((el) => {
      const next = { ...el, routing: id };
      return bump(next, routeConnector(next, elements.current, true));
    })));
  }

  function eraseAt(p) {
    const hit = topHit(liveElements(elements.current), p.x, p.y, 8 / view.zoom);
    if (hit && !gesture.current.erased.has(hit.id)) {
      gesture.current.erased.add(hit.id);
      elements.current.set(hit.id, { ...hit, isDeleted: true });
      redraw();
    }
  }

  function onPointerMove(event) {
    const p = toBoard(event);
    pointerAt.current = p;
    const g = gesture.current;
    const aiming = g?.kind === 'end' || (g?.kind === 'draw' && ['arrow', 'line'].includes(elements.current.get(g.id)?.type));
    if (!aiming && tool !== 'hand' && tool !== 'text' && tool !== 'eraser' && !['move', 'resize', 'band', 'pan', 'erase'].includes(g?.kind)) {
      const near = shapeNear(liveElements(elements.current), p.x, p.y, SNAP_MARGIN_PX / view.zoom);
      setHoverShape(near?.id || '');
    }
    if (suggesting.current) {
      if (!suggesting.current.menu) { suggesting.current.at = p; redraw(); }
      return;
    }
    if (!g) return;
    if (aiming) { aimLine(g, p, borderHeld(event)); return; }
    if (g.kind === 'band') { g.now = p; redraw(); return; }
    if (g.kind === 'pan') {
      setView({ ...g.view, x: g.view.x - (event.clientX - g.sx) / g.view.zoom, y: g.view.y - (event.clientY - g.sy) / g.view.zoom });
      return;
    }
    if (g.kind === 'erase') { eraseAt(p); return; }
    const el = elements.current.get(g.id);
    if (!el) return;
    if (g.kind === 'move' || g.kind === 'resize') {
      if (!g.moved && Math.hypot(p.x - g.start.x, p.y - g.start.y) * view.zoom < DRAG_THRESHOLD_PX) return;
      if (g.kind === 'move') {
        for (const [id, orig] of g.origs) {
          const moved = { ...orig, x: orig.x + p.x - g.start.x, y: orig.y + p.y - g.start.y };
          // Dragging a line away pulls its ends off what is staying behind.
          if (orig.startBinding && !g.origs.has(orig.startBinding.elementId)) moved.startBinding = undefined;
          if (orig.endBinding && !g.origs.has(orig.endBinding.elementId)) moved.endBinding = undefined;
          elements.current.set(id, moved);
        }
      } else {
        elements.current.set(g.id, { ...g.orig, width: Math.max(8, g.orig.width + p.x - g.start.x), height: Math.max(8, g.orig.height + p.y - g.start.y) });
      }
      g.moved = true;
    } else if (g.kind === 'draw') {
      const dx = p.x - g.start.x; const dy = p.y - g.start.y;
      if (el.type === 'freedraw') {
        g.raw.push([dx, dy]);
        elements.current.set(g.id, { ...el, points: g.raw.slice() });
      } else {
        elements.current.set(g.id, { ...el, ...normalizeBox(g.start.x, g.start.y, p.x, p.y) });
      }
    }
    redraw();
  }

  function onPointerUp(event) {
    const g = gesture.current;
    gesture.current = null;
    active.current = new Set();
    if (!g) return;
    if (g.target) setHoverShape('');
    if (g.kind === 'band') {
      const box = normalizeBox(g.start.x, g.start.y, g.now.x, g.now.y);
      const inside = box.width > 2 || box.height > 2 ? elementsInBox(liveElements(elements.current), box).map((el) => el.id) : [];
      setSelected([...new Set([...g.base, ...inside])]);
      redraw();
      return;
    }
    if (g.kind === 'erase') {
      const erased = [...g.erased].map((id) => elements.current.get(id)).filter(Boolean);
      // Restore the pre-erase copies, then commit the deletions as edits.
      for (const el of erased) elements.current.set(el.id, { ...el, isDeleted: false });
      remove(erased.map((el) => el.id));
      return;
    }
    if (g.kind === 'move' || g.kind === 'resize') {
      // A click (no drag) on one element of a selection selects just it.
      if (!g.moved) { if (selected.length > 1) setSelected([g.id]); return; }
      const changed = [];
      for (const [id, orig] of g.origs) {
        const now = elements.current.get(id);
        elements.current.set(id, orig);
        const geometry = { x: now.x, y: now.y, width: now.width, height: now.height };
        if (orig.startBinding) geometry.startBinding = now.startBinding;
        if (orig.endBinding) geometry.endBinding = now.endBinding;
        changed.push(bump(orig, geometry));
      }
      commit(withArrows(changed));
      return;
    }
    if (g.kind === 'end') {
      const now = elements.current.get(g.id);
      elements.current.set(g.id, g.orig);
      if (!g.moved) { redraw(); return; }
      const key = g.end === 'start' ? 'startBinding' : 'endBinding';
      const other = g.orig[g.end === 'start' ? 'endBinding' : 'startBinding']?.elementId;
      const p = toBoard(event);
      const binding = bindingAt(liveElements(elements.current), p.x, p.y, SNAP_MARGIN_PX / view.zoom, other, borderHeld(event));
      const next = { ...now, [key]: binding || undefined };
      commit(withArrows([bump(g.orig, { ...next, ...routeConnector(next, elements.current, true) })]));
      return;
    }
    if (g.kind !== 'draw') return;
    const el = elements.current.get(g.id);
    elements.current.delete(g.id);
    if (!el) return;
    const p = toBoard(event);
    let done = el;
    if (el.type === 'freedraw') {
      if (el.points.length < 2) { redraw(); return; }
      const points = simplifyStroke(el.points);
      const b = elementBounds({ ...el, x: 0, y: 0, points });
      done = { ...el, points, width: b.width, height: b.height };
    } else if (el.type === 'arrow' || el.type === 'line') {
      const [, [dx, dy]] = el.points;
      if (Math.hypot(dx, dy) < 6) {
        if (g.fromSnap && el.startBinding?.fixedPoint) {
          const { elementId, fixedPoint } = el.startBinding;
          suggesting.current = { from: elementId, fixedPoint, at: null, clicked: { at: event.timeStamp, x: p.x, y: p.y } };
        }
        redraw();
        return;
      }
      const border = borderHeld(event);
      const to = bindingAt(liveElements(elements.current), p.x, p.y, SNAP_MARGIN_PX / view.zoom, el.startBinding?.elementId, border);
      const from = el.startBinding && elements.current.get(el.startBinding.elementId);
      // Dropped on blank board from a snap point: what goes there? With the
      // border key it is just a line with a free end.
      if (!to && !border && g.fromSnap && from && el.startBinding.fixedPoint) {
        const { fixedPoint } = el.startBinding;
        openQuickMenu(from, fixedPoint, p, dropDirection(from, fixedPoint, p));
        return;
      }
      done = { ...done, endBinding: to || undefined };
      done = { ...done, ...routeConnector(done, elements.current, true) };
    } else if (el.width < 6 && el.height < 6) {
      redraw();
      return;
    }
    commit([done]);
    if (el.type !== 'freedraw') setSelected([done.id]);
  }

  // -- the context menu ---------------------------------------------------------

  function onContextMenu(event) {
    event.preventDefault();
    // A Mac's Ctrl+click arrives as a press first.
    const g = gesture.current;
    if (g?.kind === 'draw') elements.current.delete(g.id);
    if (g?.origs) for (const [id, orig] of g.origs) elements.current.set(id, orig);
    if (g?.kind === 'end') elements.current.set(g.id, g.orig);
    for (const id of g?.erased || []) elements.current.set(id, { ...elements.current.get(id), isDeleted: false });
    gesture.current = null;
    active.current = new Set();
    suggesting.current = null;
    redraw();
    if (editingText) finishText();
    const p = toBoard(event);
    pointerAt.current = p;
    const hit = pressTarget(drawnElements(elements.current), p);
    if (!hit) setSelected([]);
    else if (!selected.includes(hit.id)) setSelected([hit.id]);
    setContextMenu({ x: event.clientX, y: event.clientY, p, empty: !hit });
  }

  function copySelection() {
    copied.current = clipboardText(selectionWithLabels());
    // Runs the copy handler, which reaches the system clipboard on any origin.
    win?.document?.execCommand?.('copy');
  }

  async function pasteAt(p) {
    let text = '';
    try { text = await win.navigator.clipboard.readText(); } catch { /* the browser said no: our own copy will do */ }
    pointerAt.current = p;
    pasteText(text || copied.current);
  }

  function pasteItem(p, keys) {
    const run = () => pasteAt(p);
    if (win?.navigator?.clipboard?.readText) return { label: 'Paste here', keys, run };
    return { label: copied.current ? 'Paste board copy here' : 'Paste here', keys, disabled: !copied.current, note: clipboardReadNote(keys, win), run };
  }

  function addShape(kind, p) {
    const shape = newElement(kind, { x: p.x - 80, y: p.y - 40, width: 160, height: 80, strokeColor: color, backgroundColor: fill });
    commit([shape]);
    setSelected([shape.id]);
  }

  // zoomTo changes the zoom, holding the middle of the view still.
  function zoomTo(next) {
    const rect = svgRef.current?.getBoundingClientRect?.();
    const px = (rect?.width || 0) / 2;
    const py = (rect?.height || 0) / 2;
    setView((v) => {
      const zoom = clampBoardZoom(next(v.zoom));
      return { zoom, x: v.x + px / v.zoom - px / zoom, y: v.y + py / v.zoom - py / zoom };
    });
  }
  const zoomIn = () => zoomTo((zoom) => zoom * ZOOM_STEP);
  const zoomOut = () => zoomTo((zoom) => zoom / ZOOM_STEP);
  const zoomReset = () => zoomTo(() => 1);

  // fitView shows everything, never closer than actual size.
  function fitView() {
    const box = contentBounds(liveElements(elements.current));
    const rect = svgRef.current?.getBoundingClientRect?.();
    if (!box || !rect?.width) return;
    const zoom = clampBoardZoom(Math.min(1, rect.width / (box.width + 80), rect.height / (box.height + 80)));
    setView({ zoom, x: box.x + box.width / 2 - rect.width / 2 / zoom, y: box.y + box.height / 2 - rect.height / 2 / zoom });
  }

  // contextItems offers what applies to what was clicked, through the same
  // actions as the toolbar and the keys.
  function contextItems(menu, sels) {
    const mod = /mac/i.test(win?.navigator?.platform || '') ? 'Cmd' : 'Ctrl';
    const history = [{ label: 'Undo', keys: `${mod}+Z`, run: () => undo(false) }, { label: 'Redo', keys: `${mod}+Shift+Z`, run: () => undo(true) }];
    if (menu.empty || !sels.length) {
      return [
        ...QUICK_SHAPES.map(({ id, label }) => ({ label: `Add ${label.toLowerCase()} here`, run: () => addShape(id, menu.p) })),
        { label: 'Add text here', keys: 'Double-click', run: () => beginText(null, menu.p) },
        'separator',
        pasteItem(menu.p, `${mod}+V`),
        { label: 'Select all', keys: `${mod}+A`, disabled: !live.length, run: selectAll },
        { label: 'Fit board to view', disabled: !live.length, run: fitView },
        'separator',
        ...history,
      ];
    }
    const items = [];
    const one = sels.length === 1 ? sels[0] : null;
    if (one) items.push({ label: one.points ? 'Edit label' : 'Edit text', keys: 'Enter', run: () => (one.type === 'text' ? beginText(one) : beginLabel(one)) }, 'separator');
    const shapes = sels.filter((el) => ['rectangle', 'ellipse', 'diamond'].includes(el.type));
    if (shapes.length === sels.length) {
      for (const { id, label } of QUICK_SHAPES) {
        if (shapes.some((el) => el.type !== id)) {
          items.push({ label: `Change to ${label.toLowerCase()}`, run: () => commit(withArrows(shapes.filter((el) => el.type !== id).map((el) => bump(elements.current.get(el.id), { type: id })))) });
        }
      }
      items.push('separator');
    }
    const routed = sels.filter((el) => el.type === 'arrow' || el.type === 'line');
    if (routed.length === sels.length) {
      for (const route of LINE_ROUTES) {
        items.push({ label: route.label, checked: routed.every((el) => (el.routing || 'straight') === route.id), run: () => chooseRouting(route.id, routed) });
      }
      const heads = routed.some((el) => el.type === 'line');
      items.push({ label: heads ? 'Add arrowhead' : 'Remove arrowhead', run: () => commit(routed.map((el) => bump(elements.current.get(el.id), { type: heads ? 'arrow' : 'line' }))) }, 'separator');
    }
    if (arrangeable >= 2) {
      items.push(...ALIGNMENTS.filter((a) => arrangeable >= (a.min || 2)).map((a) => ({ label: a.label, run: () => align(a.id) })), 'separator');
    }
    items.push(
      { label: 'Duplicate', keys: `${mod}+D`, run: duplicate },
      { label: 'Copy', keys: `${mod}+C`, run: copySelection },
      { label: 'Cut', keys: `${mod}+X`, run: () => { copySelection(); remove(selected); } },
      'separator',
      { label: 'Bring to front', run: () => commit(stackOrder(elements.current, selected, 'front')) },
      { label: 'Send to back', run: () => commit(stackOrder(elements.current, selected, 'back')) },
      'separator',
      { swatches: BOARD_COLORS.map((c) => ({ color: c, label: `Stroke ${c}`, on: sels.every((el) => el.strokeColor === c), run: () => chooseColor(c, sels) })) },
    );
    if (shapes.length) {
      items.push({ swatches: BOARD_FILLS.map((c) => ({ color: c, label: c === 'transparent' ? 'No fill' : `Fill ${c}`, on: shapes.every((el) => el.backgroundColor === c), run: () => chooseFill(c, sels) })) });
    }
    items.push('separator', { label: 'Delete', keys: 'Del', danger: true, run: () => remove(selected) });
    return items;
  }

  // centerOn moves the view to a board point, at the zoom it has.
  const centerOn = useCallback((x, y) => {
    const rect = svgRef.current?.getBoundingClientRect?.();
    if (!rect?.width || !rect?.height) return;
    setView((v) => ({ ...v, x: x - rect.width / 2 / v.zoom, y: y - rect.height / 2 / v.zoom }));
  }, []);

  // onWheel takes the whole editor: Ctrl+wheel (a trackpad pinch arrives as
  // one) must zoom the board over its text inputs and controls too, or the
  // browser zooms the page instead. On the canvas a mouse wheel zooms as well,
  // and a touchpad's two-finger scroll pans.
  function onWheel(event) {
    const target = event.target;
    if (target?.closest?.('.board-minimap-frame')) return; // zooms the map
    const pinching = event.ctrlKey || event.metaKey;
    if (!pinching && !target?.closest?.('.board-canvas-wrap')) return;
    if (!pinching && target?.closest?.('.board-context, .board-quick')) return; // a menu scrolls
    event.preventDefault();
    setContextMenu(null);
    if (!pinching) {
      // One scroll is one device: a stray event must not jump the view.
      const going = event.timeStamp - wheelScroll.current.at < WHEEL_SCROLL_GAP_MS;
      const mouse = !event.shiftKey && (going ? wheelScroll.current.mouse : wheelIsMouse(event));
      wheelScroll.current = { at: event.timeStamp, mouse };
      if (!mouse) {
        const pan = wheelPan(event);
        setView({ ...view, x: view.x + pan.x / view.zoom, y: view.y + pan.y / view.zoom });
        return;
      }
      if (gesture.current?.kind === 'pan') return; // a drag holds the zoom it began with
    }
    const rect = svgRef.current?.getBoundingClientRect?.() || { left: 0, top: 0, width: 0, height: 0 };
    // Over the toolbar or the title, the point to hold still is the middle.
    const canvas = target?.closest?.('.board-canvas-wrap');
    const px = canvas ? event.clientX - rect.left : rect.width / 2;
    const py = canvas ? event.clientY - rect.top : rect.height / 2;
    const delta = pinching ? event.deltaY : wheelPixels(event);
    const zoom = Math.min(4, Math.max(0.2, view.zoom * Math.exp(-delta * 0.002)));
    setView({ zoom, x: px / view.zoom + view.x - px / zoom, y: py / view.zoom + view.y - py / zoom });
  }

  // -- text -------------------------------------------------------------------

  function beginText(el, p = null) {
    if (el) {
      const centred = el.textAlign === 'center';
      const x = centred ? el.x + elementBounds(el).width / 2 : el.x;
      setEditingText({ id: el.id, x, y: el.y, text: el.text || '', style: textStyleOf(el), color: safeColor(el.strokeColor, '#1e1e1e'), isNew: false, centred });
    } else {
      setEditingText({ id: '', x: p.x, y: p.y - 12, text: '', style: textStyle, color, isNew: true });
    }
  }

  // beginLabel edits an element's attached text, or starts it (`<id>-label`, as
  // the CLI writes labels). `shape` is as drawn: a line's stored path may lag.
  function beginLabel(shape) {
    const label = labelOf(elements.current, shape.id);
    if (label) { beginText(drawnElements(elements.current).find((el) => el.id === label.id) || label); return; }
    const c = labelPoint(shape);
    setEditingText({ id: '', shapeId: shape.id, x: c.x, y: c.y - 12, text: '', style: textStyle, color: safeColor(shape.strokeColor, '#1e1e1e'), isNew: true, centred: true });
  }

  function onDoubleClick(event) {
    const p = toBoard(event);
    const live = liveElements(elements.current);
    const hit = topHit(live, p.x, p.y);
    if (hit?.type === 'text') { beginText(hit); return; }
    // On a line or stroke, or anywhere inside a shape (not only on an unfilled
    // shape's outline).
    const shape = hit || shapeInside(live, p.x, p.y);
    if (shape) beginLabel(shape);
    else if (tool === 'select') beginText(null, p);
  }

  function finishText() {
    const t = editingRef.current;
    setEditingText(null);
    if (!t) return;
    const text = t.text.replace(/\s+$/, '');
    if (t.isNew) {
      if (!text) return;
      const shape = t.shapeId && elements.current.get(t.shapeId);
      if (shape && !shape.isDeleted) {
        const label = newLabel({ ...shape, ...routeConnector(shape, elements.current) }, text, { ...t.style, strokeColor: t.color });
        // A deleted `<id>-label` outranks a fresh version 1: revive it.
        const old = elements.current.get(label.id);
        commit([old ? bump(old, { ...label, version: old.version, isDeleted: false }) : label]);
      }
      else commit([newElement('text', { ...t.style, x: t.x, y: t.y, text, strokeColor: t.color })]);
      return;
    }
    const cur = elements.current.get(t.id);
    if (!cur) return;
    if (!text) remove([t.id]);
    else if (text !== cur.text || !sameTextStyle(cur, t.style)) commit(withArrows([bump(cur, { ...t.style, text, width: 0, height: 0 })]));
  }

  // leaveText closes the editor unless the focus went to the text controls,
  // which change what is being typed.
  function leaveText(event) {
    const to = event.relatedTarget;
    if (to?.closest?.('.board-text-input, .board-text-controls')) return;
    finishText();
  }

  // styledText is the text the style controls act on: the selection's own, and
  // what its shapes and lines carry.
  function styledText(chosen, owners) {
    if (!chosen.length) return [];
    const ids = new Set(chosen.map((el) => el.id));
    const found = new Set(chosen.filter((el) => el.type === 'text').map((el) => el.id));
    for (const [textId, ownerId] of owners) if (ids.has(ownerId)) found.add(textId);
    return [...found].map((id) => elements.current.get(id)).filter(Boolean);
  }

  // chooseTextStyle sets the style for the next text, and for what is open or
  // selected now. A box was measured for the old style, so it goes.
  function chooseTextStyle(changes, chosen) {
    setTextStyle((style) => ({ ...style, ...changes }));
    const open = editingRef.current;
    if (open) {
      setEditingText({ ...open, style: { ...open.style, ...changes } });
      rootRef.current?.querySelector?.('.board-text-input')?.focus();
    } else if (chosen.length) {
      commit(withArrows(chosen.map((el) => bump(el, { ...changes, width: 0, height: 0 }))));
    }
  }

  // A browser honours autofocus only once per document, so the open editor
  // takes the focus here, with the caret after any text it already holds.
  const editing = !!editingText;
  useLayoutEffect(() => {
    const input = textInputRef.current;
    if (!input) return;
    input.focus({ preventScroll: true });
    input.setSelectionRange?.(input.value.length, input.value.length);
  }, [editing]);

  // The text tool opens the editor on pointerdown; the mousedown of that same
  // press would otherwise blur it, which closes it again.
  function keepEditorFocus(event) {
    if (editingRef.current) event.preventDefault();
  }

  // -- keyboard ---------------------------------------------------------------

  // A click anywhere else ends the board's claim on keys aimed at the page, or
  // a floating board would take Delete and Ctrl+A from the view under it.
  useEffect(() => {
    const doc = win?.document;
    if (!doc) return undefined;
    const onPointerDown = (event) => {
      if (lastEditor === editorToken && !rootRef.current?.contains?.(event.target)) lastEditor = null;
    };
    doc.addEventListener('pointerdown', onPointerDown, true);
    return () => doc.removeEventListener('pointerdown', onPointerDown, true);
  }, [win, editorToken]);

  useEffect(() => {
    // Enter on a focused button is that button's.
    const onButton = (event) => event.target?.closest?.('button');
    const ours = (event) => {
      const target = event.target;
      // Typing belongs to whatever holds the focus: a field, a terminal (xterm
      // takes keys in a textarea), a select's type-ahead, another editor.
      if (editingText || target?.closest?.('input, textarea, select, [contenteditable]:not([contenteditable="false"]), .companion-window')) return false;
      if (!shown()) return false;
      // A floating board's window holds focus after a press on the board.
      if (target === rootRef.current?.closest?.('.overlay-window')) return true;
      return !!rootRef.current?.contains?.(target) || (target === win?.document?.body && lastEditor === editorToken);
    };
    // Holding or letting go of the border key mid-drag shows at once.
    const onBorderKey = (event) => {
      const g = gesture.current;
      if ((event.key === 'Control' || event.key === 'Meta') && g?.at) aimLine(g, g.at, borderHeld(event));
    };
    const onKey = (event) => {
      onBorderKey(event);
      if (!ours(event)) return;
      // The quick menu's field holds the keys; should it lose them, no more.
      if (suggesting.current?.menu) {
        if (event.key === 'Escape') closeQuickMenu();
        return;
      }
      const key = event.key.toLowerCase();
      const mod = event.ctrlKey || event.metaKey;
      if (mod && key === 'z') {
        event.preventDefault();
        undo(event.shiftKey);
        return;
      }
      if (mod && key === 'y') { event.preventDefault(); undo(true); return; }
      if (mod && key === 'a') { event.preventDefault(); selectAll(); return; }
      if (mod && key === 'd') { event.preventDefault(); duplicate(); return; }
      if (mod || event.altKey) return;
      // By physical key: a layout's Shift+0 may read `=`, which zooms in.
      if (event.shiftKey && event.code === 'Digit0') { event.preventDefault(); zoomReset(); return; }
      if (event.shiftKey && event.code === 'Digit1') { event.preventDefault(); fitView(); return; }
      if (event.key === '+' || event.key === '=') { event.preventDefault(); zoomIn(); return; }
      if (event.key === '-') { event.preventDefault(); zoomOut(); return; }
      if ((event.key === 'Delete' || event.key === 'Backspace') && selected.length) { event.preventDefault(); remove(selected); return; }
      if (event.key === 'Escape') { suggesting.current = null; setSelected([]); setTool('select'); redraw(); return; }
      if (selected.length === 1 && !onButton(event)) {
        const el = drawnElements(elements.current).find(({ id }) => id === selected[0]);
        if (ARROW_SIDES[event.key] && snapPoints(el || {}).length) { event.preventDefault(); quickConnect(el, ARROW_SIDES[event.key]); return; }
        if (event.key === 'Enter' && el) { event.preventDefault(); if (el.type === 'text') beginText(el); else beginLabel(el); return; }
      }
      const t = BOARD_TOOLS.find((candidate, index) => candidate.key === key || boardToolDigit(index) === key);
      if (t) setTool(t.id);
    };
    // Copy and paste use the system clipboard in Excalidraw's format, so
    // elements also move between boards and to and from excalidraw.com.
    const onCopy = (event) => {
      if (!ours(event) || !selected.length) return;
      // Text the operator highlighted is theirs to copy.
      const highlighted = win?.getSelection?.();
      if (highlighted && !highlighted.isCollapsed) return;
      event.preventDefault();
      copied.current = clipboardText(selectionWithLabels());
      event.clipboardData?.setData('text/plain', copied.current);
      if (event.type === 'cut') remove(selected);
    };
    const onPaste = (event) => {
      if (!ours(event)) return;
      const text = event.clipboardData?.getData('text/plain') || '';
      if (!text) return;
      event.preventDefault();
      pasteText(text);
    };
    win?.addEventListener?.('keydown', onKey);
    win?.addEventListener?.('keyup', onBorderKey);
    win?.addEventListener?.('copy', onCopy);
    win?.addEventListener?.('cut', onCopy);
    win?.addEventListener?.('paste', onPaste);
    return () => {
      win?.removeEventListener?.('keydown', onKey);
      win?.removeEventListener?.('keyup', onBorderKey);
      win?.removeEventListener?.('copy', onCopy);
      win?.removeEventListener?.('cut', onCopy);
      win?.removeEventListener?.('paste', onPaste);
    };
  }, [editingText, selected, view, tool, color]);

  // -- participants -------------------------------------------------------------

  async function addParticipant(agentId) {
    setInvite(false);
    try {
      await jsonOrThrow(await fetchImpl(`/api/boards/${encodeURIComponent(boardId)}/participants`, {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agent_id: agentId }),
      }));
      setMeta((m) => ({ ...m, participants: [...new Set([...m.participants, agentId])] }));
      await sendInvite(agentId);
      notify(`Invited ${agents.get(agentId)?.name || agentId} to the board.`);
    } catch (error) {
      notify(`Could not invite: ${error?.message || error}`, true);
    }
  }

  // sendInvite tells an agent how to use the board. This mail is
  // operator-authored, so like the board event it quotes nothing agents wrote:
  // not the title, which any agent with boards.write can set, nor a group name.
  function sendInvite(agentId, why = "You're invited to") {
    return fetchImpl('/api/operator-message', {
      method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        to: agentId, subject: `[board ${boardId}] you're invited`,
        body: `${why} the shared board ${boardId}. The operator draws there; you get a short event when their edits settle.\n`
          + `Look: \`tclaude agent board show ${boardId}\` (elements as text; \`--view\` for what the operator has on screen) or \`tclaude agent board snapshot ${boardId} -o /tmp/${boardId}.png\` (an image to view).\n`
          + `Draw: \`tclaude agent board add ${boardId} rectangle --x 100 --y 100 --w 160 --h 80 --text "Idea"\`, arrows with --from/--to element ids (--from-side/--to-side for a snap point, --routing elbow or curved); build a flow with \`--right-of <id> --link\` and tidy it with \`board layout ${boardId}\`; \`board rm\`, \`board move\`, \`board text\` (any element), \`board connect\`. See \`tclaude agent board --help\`.`,
      }),
    });
  }

  // setGroup makes the board a group's board (tcl-4yhow7): its members are
  // on it without one-by-one invitations. The online ones newly on it are
  // told how to use it.
  async function setGroup(group, select = null) {
    try {
      const result = await jsonOrThrow(await fetchImpl(`/api/boards/${encodeURIComponent(boardId)}/group`, {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ group }),
      }));
      const members = Array.isArray(result.group_members) ? result.group_members : [];
      const before = new Set([...meta.participants, ...meta.groupMembers]);
      setMeta((m) => ({ ...m, group: result.group || '', groupMembers: members }));
      const fresh = members.filter((id) => !before.has(id) && agents.get(id)?.online);
      await Promise.allSettled(fresh.map((id) => sendInvite(id, 'Your group now has')));
      notify(result.group ? `This is now ${result.group}'s board${fresh.length ? `; told ${fresh.length} online member${fresh.length === 1 ? '' : 's'}` : ''}.` : 'The board no longer belongs to a group.');
    } catch (error) {
      // The select shows the old group again.
      if (select) select.value = meta.group;
      notify(`Could not change the board's group: ${error?.message || error}`, true);
    }
  }

  async function removeParticipant(agentId) {
    try {
      await jsonOrThrow(await fetchImpl(`/api/boards/${encodeURIComponent(boardId)}/participants`, {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agent_id: agentId, remove: true }),
      }));
      setMeta((m) => ({ ...m, participants: m.participants.filter((id) => id !== agentId) }));
    } catch (error) {
      notify(`Could not remove: ${error?.message || error}`, true);
    }
  }

  // -- render -------------------------------------------------------------------

  // A pan or a zoom changes no element, so it works none of this out again.
  const live = useMemo(() => {
    const list = drawnElements(elements.current);
    drawnMap.current.clear();
    for (const el of list) drawnMap.current.set(el.id, el);
    return list;
  }, [tick]);
  const drawnById = drawnMap.current;
  const sels = selected.map((id) => drawnById.get(id)).filter(Boolean);
  const sel = sels.length === 1 ? sels[0] : null;
  const lines = sels.filter((el) => el.type === 'arrow' || el.type === 'line');
  const shownRouting = lines[0]?.routing || (lines.length ? 'straight' : routing);
  const snapShape = tool !== 'hand' && tool !== 'text' && tool !== 'eraser' ? drawnById.get(hoverShape) : null;
  const offer = suggesting.current ? suggestion() : null;
  const menu = offer && suggesting.current?.menu ? suggesting.current : null;
  const aimed = gesture.current?.target || null;
  const aimedShape = aimed && drawnById.get(aimed.elementId);
  const band = gesture.current?.kind === 'band' ? normalizeBox(gesture.current.start.x, gesture.current.start.y, gesture.current.now.x, gesture.current.now.y) : null;
  const owners = sels.length ? labelOwners(elements.current) : new Map();
  const arrangeable = (() => {
    const chosen = new Set(selected);
    return sels.filter((el) => el.type !== 'arrow' && !chosen.has(owners.get(el.id))).length;
  })();
  const aiming = !!gesture.current?.at;
  const texts = styledText(sels, owners);
  const showTextControls = !!editingText || tool === 'text' || texts.length > 0;
  const shownText = textStyleOf(editingText ? editingText.style : texts[0] || textStyle);
  const pad = 6 / view.zoom;
  const transform = `scale(${view.zoom}) translate(${-view.x} ${-view.y})`;
  const paper = gridStyle(view);
  const drawing = [...presence].filter(([, state]) => state === 'drawing').map(([id]) => agents.get(id)?.name || id.slice(0, 12));
  const groupNames = [...new Set([meta.group, ...(snapshot.value?.groups || []).filter((g) => g?.name && !g.scribe && !g.archived).map((g) => g.name)].filter(Boolean))].sort();
  const candidates = [...agents.values()].filter((a) => a.online && !meta.participants.includes(a.agentId) && !meta.groupMembers.includes(a.agentId)).sort((a, b) => (b.steward - a.steward) || a.name.localeCompare(b.name));
  void tick;
  // A pan or a zoom renders the editor without changing an element: the same
  // list lets Preact skip every shape, which a board of thousands needs.
  const editingId = editingText?.id || '';
  const drawn = useMemo(() => live.map((el) => html`<${BoardElement} key=${el.id} el=${el} flash=${flash.has(el.id)} onLine=${!!drawnById.get(el.containerId)?.points} editing=${el.id === editingId} />`), [tick, flash, editingId]);

  if (gone) return html`<p class="board-gone" role="status">This board was deleted.</p>`;
  return html`<div ref=${rootRef} class="board-editor" onPointerDownCapture=${() => { lastEditor = editorToken; }} onWheel=${onWheel}>
    <header class="board-head">
      <h2 class="board-title">${meta.title}</h2>
      <span class=${`board-sync ${status}`} title="Sync with the daemon">${status === 'ready' ? (pending.current.size ? (refused.current ? 'not saved' : 'saving…') : 'synced') : status}</span>
      <div class="board-people" aria-label="Agents on this board">
        ${meta.participants.map((id) => {
          const agent = agents.get(id);
          const state = presence.get(id) || '';
          const doing = state === 'drawing' ? ' (drawing…)' : state === 'looking' ? ' (looking at the board)' : '';
          return html`<span key=${id} class=${`board-person${agent?.online ? '' : ' offline'}${state ? ` ${state}` : ''}`} title=${agent ? `${agent.name}${agent.online ? '' : ' (offline)'}${doing}` : id}>
            <button type="button" class="board-person-name" onClick=${() => onOpenAgent?.(id)}>${agent?.steward ? '✦ ' : ''}${agent?.name || id.slice(0, 12)}${state === 'drawing' ? html` <span class="board-person-doing" aria-label="drawing">✎</span>` : ''}</button>
            <button type="button" class="board-person-x" aria-label=${`Remove ${agent?.name || id} from the board`} onClick=${() => removeParticipant(id)}>×</button>
          </span>`;
        })}
        ${meta.groupMembers.filter((id) => !meta.participants.includes(id)).map((id) => {
          const agent = agents.get(id);
          const state = presence.get(id) || '';
          return html`<span key=${id} class=${`board-person via-group${agent?.online ? '' : ' offline'}${state ? ` ${state}` : ''}`} title=${`${agent?.name || id} (in group ${meta.group})${state === 'drawing' ? ' (drawing…)' : ''}`}>
            <button type="button" class="board-person-name" onClick=${() => onOpenAgent?.(id)}>${agent?.name || id.slice(0, 12)}${state === 'drawing' ? html` <span class="board-person-doing" aria-label="drawing">✎</span>` : ''}</button>
          </span>`;
        })}
        <span class="board-invite-wrap" ref=${inviteRef}>
          <button type="button" class="board-invite" aria-expanded=${invite ? 'true' : 'false'} onClick=${() => setInvite((v) => !v)}>＋ Invite agent</button>
          ${invite && html`<div class="board-invite-menu" role="menu">
            ${candidates.length ? candidates.slice(0, 30).map((a) => html`<button key=${a.agentId} type="button" role="menuitem" onClick=${() => addParticipant(a.agentId)}>${a.steward ? '✦ ' : ''}${a.name}</button>`)
              : html`<div class="board-invite-empty">No other live agents. Summon the Steward from the companion (Ctrl+.).</div>`}
          </div>`}
        </span>
        <select class="board-group" aria-label="The board's group" title="A group's members are on its board without invitations"
          value=${meta.group} onChange=${(event) => setGroup(event.currentTarget.value, event.currentTarget)}>
          <option value="">No group</option>
          ${groupNames.map((name) => html`<option key=${name} value=${name}>Group: ${name}</option>`)}
        </select>
      </div>
      <button type="button" class="board-export" title="Download as .excalidraw"
        onClick=${() => import('./board-export.js').then((m) => m.downloadText(`${boardId}.excalidraw`, toExcalidraw(drawnElements(elements.current))))}>⤓</button>
      ${headActions}
    </header>
    <div class="board-toolbar" role="toolbar" aria-label="Board tools">
      ${BOARD_TOOLS.map((t, index) => {
        const digit = boardToolDigit(index);
        const keys = digit ? [digit, t.key.toUpperCase()] : [t.key.toUpperCase()];
        return html`<button key=${t.id} type="button" class=${`board-tool${tool === t.id ? ' on' : ''}`}
          aria-pressed=${tool === t.id ? 'true' : 'false'} title=${`${t.label} (${keys.join(' or ')})`} aria-label=${t.label}
          aria-keyshortcuts=${keys.join(' ')} onClick=${() => setTool(t.id)}>${t.icon}${digit && html`<span class="board-tool-digit" aria-hidden="true">${digit}</span>`}</button>`;
      })}
      <span class="board-sep"></span>
      ${BOARD_COLORS.map((c) => html`<button key=${c} type="button" class=${`board-swatch${color === c ? ' on' : ''}`} style=${`background:${c}`}
        aria-label=${`Stroke ${c}`} onClick=${() => chooseColor(c, sels)}></button>`)}
      <span class="board-sep"></span>
      ${BOARD_FILLS.map((c) => html`<button key=${c} type="button" class=${`board-swatch fill${fill === c ? ' on' : ''}`}
        style=${c === 'transparent' ? '' : `background:${c}`} aria-label=${c === 'transparent' ? 'No fill' : `Fill ${c}`}
        onClick=${() => chooseFill(c, sels)}>${c === 'transparent' ? '∅' : ''}</button>`)}
      ${(lines.length > 0 || tool === 'arrow' || tool === 'line') && html`<span class="board-sep"></span>${LINE_ROUTES.map((r) => html`<button key=${r.id} type="button"
        class=${`board-tool${shownRouting === r.id ? ' on' : ''}`} aria-pressed=${shownRouting === r.id ? 'true' : 'false'} title=${r.label} aria-label=${r.label}
        onClick=${() => chooseRouting(r.id, lines)}>${r.icon}</button>`)}`}
      ${showTextControls && html`<span class="board-sep"></span>
        <span class="board-text-controls" role="group" aria-label="Text style">
          <select class="board-text-select" aria-label="Text size" title="Text size" value=${shownText.fontSize}
            onChange=${(event) => { chooseTextStyle({ fontSize: Number(event.currentTarget.value) }, texts); if (!editingRef.current) event.currentTarget.blur(); }} onBlur=${editingText ? leaveText : null}>
            ${boardFontSizes(shownText.fontSize).map((size) => html`<option key=${size} value=${size}>${size}</option>`)}
          </select>
          <select class="board-text-select" aria-label="Font" title="Font" value=${shownText.fontFamily}
            onChange=${(event) => { chooseTextStyle({ fontFamily: Number(event.currentTarget.value) }, texts); if (!editingRef.current) event.currentTarget.blur(); }} onBlur=${editingText ? leaveText : null}>
            ${BOARD_FONTS.map((font) => html`<option key=${font.id} value=${font.id}>${font.label}</option>`)}
          </select>
          <button type="button" class=${`board-tool${shownText.fontWeight ? ' on' : ''}`} title="Bold" aria-label="Bold" aria-pressed=${shownText.fontWeight ? 'true' : 'false'}
            onMouseDown=${(event) => event.preventDefault()} onClick=${() => chooseTextStyle({ fontWeight: shownText.fontWeight ? undefined : 'bold' }, texts)}><b>B</b></button>
          <button type="button" class=${`board-tool${shownText.fontStyle ? ' on' : ''}`} title="Italic" aria-label="Italic" aria-pressed=${shownText.fontStyle ? 'true' : 'false'}
            onMouseDown=${(event) => event.preventDefault()} onClick=${() => chooseTextStyle({ fontStyle: shownText.fontStyle ? undefined : 'italic' }, texts)}><i>I</i></button>
        </span>`}
      <span class="board-sep"></span>
      <button type="button" class="board-tool" title="Undo (Ctrl+Z)" aria-label="Undo" onClick=${() => undo(false)}>↶</button>
      <button type="button" class="board-tool" title="Redo (Ctrl+Shift+Z)" aria-label="Redo" onClick=${() => undo(true)}>↷</button>
      ${sels.length > 0 && html`<button type="button" class="board-tool" title="Delete (Del)" aria-label="Delete selection" onClick=${() => remove(selected)}>🗑</button>`}
      ${arrangeable >= 2 && html`<span class="board-sep"></span>${ALIGNMENTS.filter((a) => arrangeable >= (a.min || 2)).map((a) => html`<button key=${a.id} type="button" class="board-tool"
        title=${a.label} aria-label=${a.label} onClick=${() => align(a.id)}>${a.icon}</button>`)}`}
      <span class="board-zoom-controls" role="group" aria-label="Board zoom">
        <button type="button" class="board-tool" title="Zoom out (-)" aria-label="Zoom out" disabled=${view.zoom <= BOARD_ZOOM_MIN} onClick=${zoomOut}>−</button>
        <span class="board-zoom">${Math.round(view.zoom * 100)}%</span>
        <button type="button" class="board-tool" title="Zoom in (+)" aria-label="Zoom in" disabled=${view.zoom >= BOARD_ZOOM_MAX} onClick=${zoomIn}>+</button>
        <button type="button" class="board-tool" title="Reset zoom to 100% (Shift+0)" aria-label="Reset zoom to 100%" disabled=${view.zoom === 1} onClick=${zoomReset}>1:1</button>
        <button type="button" class="board-tool" title="Fit everything in view (Shift+1)" aria-label="Fit everything in view" disabled=${!live.length} onClick=${fitView}>⤢</button>
      </span>
    </div>
    <div class="board-canvas-wrap" style=${paper}>
      <svg ref=${svgRef} class=${`board-canvas tool-${tool}`} role="img" aria-label=${`Board ${meta.title}`} data-board-id=${boardId}
        onPointerDown=${onPointerDown} onPointerMove=${onPointerMove} onPointerUp=${onPointerUp} onPointerCancel=${onPointerUp}
        onMouseDown=${keepEditorFocus}
        onPointerLeave=${() => setHoverShape('')}
        onDblClick=${onDoubleClick} onContextMenu=${onContextMenu}>
        <g data-board-viewport transform=${transform}>
          ${drawn}
          ${(sels.length > 0 || band || snapShape || offer) && html`<g data-board-chrome>
            ${sels.map((el) => {
              const b = elementBounds(el);
              return html`<rect key=${el.id} class="board-selection" x=${b.x - pad} y=${b.y - pad} width=${b.width + pad * 2} height=${b.height + pad * 2} />`;
            })}
            ${sel && !sel.points && sel.type !== 'text' && (() => {
              const b = elementBounds(sel);
              return html`<rect class="board-handle" x=${b.x + b.width - 4 / view.zoom} y=${b.y + b.height - 4 / view.zoom} width=${8 / view.zoom} height=${8 / view.zoom} />`;
            })()}
            ${band && html`<rect class="board-band" x=${band.x} y=${band.y} width=${band.width} height=${band.height} />`}
            ${sel && (sel.type === 'arrow' || sel.type === 'line') && [sel.points[0], sel.points[sel.points.length - 1]].map(([px, py], i) => html`<circle key=${i}
              class=${`board-line-end${(i ? sel.endBinding : sel.startBinding) ? ' on' : ''}`} cx=${sel.x + px} cy=${sel.y + py} r=${5 / view.zoom} />`)}
            ${aimedShape && !aimed.fixedPoint && (() => {
              const b = elementBounds(aimedShape);
              return html`<rect class="board-aim" x=${b.x - pad} y=${b.y - pad} width=${b.width + pad * 2} height=${b.height + pad * 2} />`;
            })()}
            ${snapShape && !(aimed && !aimed.fixedPoint) && snapPoints(snapShape).map(({ x, y, fixedPoint }, i) => {
              const taken = aimed?.fixedPoint?.[0] === fixedPoint[0] && aimed.fixedPoint[1] === fixedPoint[1];
              return html`<circle key=${i} class=${`board-snap${taken ? ' on' : ''}`} cx=${x} cy=${y} r=${(taken ? 7 : 5) / view.zoom} />`;
            })}
            ${offer && html`<g class="board-suggestion">
              ${offer.ghost && html`<${BoardElement} el=${{ ...offer.ghost, strokeWidth: 2 }} />`}
              <${BoardElement} el=${offer.line} />
            </g>`}
          </g>`}
        </g>
      </svg>
      ${editingText && html`<textarea ref=${textInputRef} class=${`board-text-input${editingText.centred ? ' centred' : ''}`}
        style=${`left:${(editingText.x - view.x) * view.zoom}px;top:${(editingText.y - view.y) * view.zoom}px;font-size:${shownText.fontSize * view.zoom}px;color:${editingText.color};font-family:${boardFont(shownText).css};font-weight:${shownText.fontWeight || 'normal'};font-style:${shownText.fontStyle || 'normal'}`}
        value=${editingText.text}
        onInput=${(event) => setEditingText({ ...editingText, text: event.currentTarget.value })}
        onKeyDown=${(event) => {
          if (event.key === 'Escape' || (event.key === 'Enter' && !event.shiftKey)) { event.preventDefault(); finishText(); }
        }}
        onBlur=${leaveText}></textarea>`}
      ${menu && (() => {
        const rect = svgRef.current?.getBoundingClientRect?.();
        const fit = (value, room, size) => Math.max(4, room ? Math.min(value, room - size - 4) : value);
        const x = fit((menu.at.x - view.x) * view.zoom + 8, rect?.width, QUICK_MENU_SIZE.width);
        const y = fit((menu.at.y - view.y) * view.zoom + 8, rect?.height, QUICK_MENU_SIZE.height);
        return html`<${BoardQuickMenu} x=${x} y=${y} kind=${menu.kind} onPreview=${(kind) => { if (suggesting.current) { suggesting.current.kind = kind; redraw(); } }}
          onPick=${(kind) => { suggesting.current.kind = kind; acceptSuggestion(); }} onCancel=${closeQuickMenu} />`;
      })()}
      ${contextMenu && html`<${BoardContextMenu} x=${contextMenu.x} y=${contextMenu.y} win=${win} label=${contextMenu.empty ? 'Board actions' : 'Selection actions'}
        items=${contextItems(contextMenu, sels)} onClose=${() => setContextMenu(null)} />`}
      ${aiming && html`<div class="board-hint" role="status">Hold ${borderKeyName(win)} to attach to the border instead of a connection point</div>`}
      ${drawing.length > 0 && html`<div class="board-drawing-note" role="status">✎ ${drawing.join(', ')} ${drawing.length === 1 ? 'is' : 'are'} drawing…</div>`}
      ${!live.length && status === 'ready' && html`<div class="board-empty-hint">Pick a tool and draw. Invite an agent to draw with you; it reads the board as shapes and as an image.</div>`}
      <${BoardMinimap} elements=${drawnMap.current} tick=${tick} view=${view} svgRef=${svgRef} onScreen=${onScreen} win=${win} onNavigate=${centerOn} />
    </div>
  </div>`;
}

// -- the board list ---------------------------------------------------------------

// BoardRow is one board on one line; its export and delete show on hover.
function BoardRow({ board, onOpen, onExport, onDelete }) {
  const label = board.title || board.id;
  const agents = board.agents ?? board.participants?.length ?? 0;
  const elements = `${board.element_count ?? 0} element${board.element_count === 1 ? '' : 's'}`;
  const changed = board.updated_at ? new Date(board.updated_at).toLocaleString() : '';
  return html`<li class="board-row">
    <button type="button" class="board-row-open" title=${`${label}: ${elements}${changed ? `, changed ${changed}` : ''}`} onClick=${() => onOpen(board.id, label)}>
      <span class=${`board-row-dot${board.active ? ' active' : ''}`} title=${board.active ? 'An agent has a session on it' : ''}></span>
      <span class="board-row-title">${label}</span>
      ${agents > 0 && html`<span class="board-row-agents">${agents} agent${agents === 1 ? '' : 's'}</span>`}
      <span class="board-row-age">${boardAge(board.updated_at)}</span>
    </button>
    <button type="button" class="board-row-action" title="Download as .excalidraw" aria-label=${`Export ${label}`} onClick=${() => onExport(board)}>⤓</button>
    <button type="button" class="board-row-action danger" title="Delete this board" aria-label=${`Delete ${label}`} onClick=${() => onDelete(board)}>🗑</button>
  </li>`;
}

// BoardList offers every board, and a new one; one box does both. `compact` is
// the strip's + menu. onPicking says while a file is being chosen and read, when
// a menu around the list must stay open: the file input is inside it.
export function BoardList({ fetchImpl, notify, onOpen, onPicking = () => {}, compact = false }) {
  const [boards, setBoards] = useState(null);
  const [title, setTitle] = useState('');
  const [error, setError] = useState('');
  const [importing, setImporting] = useState(false);
  const fileRef = useRef(null);
  const load = useCallback(async () => {
    try {
      const result = await jsonOrThrow(await fetchImpl('/api/boards', { credentials: 'same-origin' }));
      setBoards(Array.isArray(result.boards) ? result.boards : []);
      setError('');
    } catch (err) {
      setError(err?.message || String(err));
    }
  }, [fetchImpl]);
  useEffect(() => { void load(); }, [load]);
  const clean = title.trim();
  const same = (boards || []).find((b) => (b.title || b.id).toLowerCase() === clean.toLowerCase());
  async function create(event) {
    event.preventDefault();
    if (!clean) return;
    // The box filters too, so naming a board that exists means that board.
    if (same) {
      onOpen(same.id, same.title || same.id);
      return;
    }
    try {
      const board = await jsonOrThrow(await fetchImpl('/api/boards', {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title: clean, id: freeBoardId(clean, (boards || []).map((b) => b.id)) || undefined }),
      }));
      setTitle('');
      onOpen(board.id, board.title || clean);
    } catch (err) {
      notify(`Could not create the board: ${err?.message || err}`, true);
    }
  }
  async function exportRow(board) {
    try {
      await exportBoard(board.id, fetchImpl);
    } catch (err) {
      notify(`Could not export the board: ${err?.message || err}`, true);
    }
  }
  async function importFile(event) {
    const file = event.currentTarget.files?.[0];
    event.currentTarget.value = '';
    if (!file) {
      onPicking(false);
      return;
    }
    setImporting(true);
    try {
      const board = await importBoardFile(file, { fetchImpl, taken: (boards || []).map((b) => b.id) });
      if (board.skipped) notify(`Imported “${board.title}”. ${board.skipped} element${board.skipped === 1 ? ' a board cannot hold was' : 's a board cannot hold were'} left out.`);
      onOpen(board.id, board.title);
    } catch (err) {
      notify(`Could not import ${file.name}: ${err?.message || err}`, true);
    } finally {
      setImporting(false);
      onPicking(false);
    }
  }
  const groups = groupBoards(boards || [], clean);
  return html`<div class=${`board-list${compact ? ' compact' : ''}`}>
    <form class="board-new" onSubmit=${create}>
      <input type="text" placeholder="Search, or type a title to create" aria-label="Search boards, or title a new one" maxLength="120"
        value=${title} onInput=${(event) => setTitle(event.currentTarget.value)} />
      <button type="submit" disabled=${!clean}>${same ? 'Open' : 'Create'}</button>
    </form>
    ${error && html`<p class="board-error" role="alert">Boards are unavailable: ${error}</p>`}
    ${boards === null && !error && html`<p class="board-muted">Loading boards…</p>`}
    ${boards && !boards.length && html`<p class="board-muted">No boards yet. A board is a shared canvas: you draw, agents read it and draw back.</p>`}
    ${boards?.length > 0 && !groups.length && html`<p class="board-muted">No board matches. Create makes one with this title.</p>`}
    <div class="board-groups">
      ${groups.map(({ name, boards: rows }) => html`<div key=${name} class="board-section" role="group" aria-label=${name ? `Group ${name}` : 'Boards without a group'}>
        ${(name || groups.length > 1) && html`<h3 class="board-section-name"><span>${name || 'No group'}</span><span class="board-section-count">${rows.length}</span></h3>`}
        <ul class="board-rows">
          ${rows.map((board) => html`<${BoardRow} key=${board.id} board=${board} onOpen=${onOpen} onExport=${exportRow}
            onDelete=${() => askDeleteBoard({ id: board.id, title: board.title, onDeleted: load })} />`)}
        </ul>
      </div>`)}
    </div>
    <div class="board-store">
      <span>Stored in this tclaude instance, not in a file. ⤓ exports one.</span>
      <button type="button" class="board-import" disabled=${importing} onClick=${() => { onPicking(true); fileRef.current?.click(); }}>${importing ? 'Importing…' : 'Import…'}</button>
      <input ref=${fileRef} type="file" accept=".excalidraw,.json,application/json" hidden aria-label="Import an .excalidraw file" onChange=${importFile} onCancel=${() => onPicking(false)} />
    </div>
  </div>`;
}

// BoardsGate leaves a board out while features.collab is off, and mounts it
// fresh when it turns on, so nothing asks the daemon for what it refuses.
export function BoardsGate({ snapshot, children }) {
  return snapshot.value?.collab_enabled === true ? children : null;
}
