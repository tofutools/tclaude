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
import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ALIGNMENTS, BOARD_COLORS, BOARD_FILLS, BOARD_TOOLS, alignMoves, boardIdFromTitle, bump, center, changeBounds,
  cleanText, clipboardText, cloneElements, connectArrow, contentBounds, describeChanges, elementBounds, elementsInBox,
  labelOf, labelOwners, liveElements, mergeElements, newElement, newLabel, normalizeBox, parseClipboard,
  reboundArrows, shapeAt, simplifyStroke, toExcalidraw, topHit, withLabels,
} from './board-model.js';
import { companionAgents } from './companion-model.js';

const html = htm.bind(h);

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

async function jsonOrThrow(response) {
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(body.message || body.error || `${response.status} ${response.statusText}`);
    error.status = response.status;
    throw error;
  }
  return body;
}

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

export function BoardElement({ el, flash = false }) {
  const stroke = safeColor(el.strokeColor, '#1e1e1e');
  const background = safeColor(el.backgroundColor, 'transparent');
  const fill = background !== 'transparent' ? background : 'none';
  const common = {
    stroke, 'stroke-width': el.strokeWidth || 2, 'stroke-dasharray': dash(el), 'stroke-linecap': 'round',
    'stroke-linejoin': 'round', opacity: (el.opacity ?? 100) / 100, 'data-element': el.id,
    class: flash ? 'board-el board-flash' : 'board-el',
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
      return html`<text ...${common} stroke="none" fill=${stroke} x=${x} y=${y + size} font-size=${size}
        font-family="'Segoe Print','Comic Sans MS','Chalkboard SE',cursive" xml:space="preserve">
        ${lines.map((line, i) => html`<tspan key=${i} x=${x} dy=${i ? size * 1.25 : 0}>${line || ' '}</tspan>`)}
      </text>`;
    }
    default:
      return null;
  }
}

// -- the editor ---------------------------------------------------------------

// BoardEditor is one board's editor. A tab keeps it mounted while another tab
// or view shows: `onScreen` says it is the tab showing, and `visible` whether
// its surroundings are. headActions are the host's own controls.
export function BoardEditor({
  boardId, fetchImpl, notify = () => {}, snapshot, win, WebSocketCtor = win?.WebSocket, rasterize = rasterizeBoard, onOpenAgent,
  onScreen = true, visible = null, headActions = null, onTitle = null,
}) {
  const elements = useRef(new Map());
  const version = useRef(0);
  const [tick, setTick] = useState(0);
  const redraw = useCallback(() => setTick((n) => n + 1), []);
  const [meta, setMeta] = useState({ title: boardId, participants: [], group: '', groupMembers: [] });
  const [status, setStatus] = useState('loading');
  const [tool, setTool] = useState('select');
  const [color, setColor] = useState(BOARD_COLORS[0]);
  const [fill, setFill] = useState(BOARD_FILLS[0]);
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
  const [flash, setFlash] = useState(() => new Set());
  const [invite, setInvite] = useState(false);
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
        if (!disposed.current) setStatus(since ? 'offline' : `error: ${error?.message || error}`);
      }
    };
    const due = () => !liveOpen || stale || retry
      || Date.now() - lastLoad >= (presenceShown ? BOARD_LIVE_PRESENCE_MS : BOARD_LIVE_CHECK_MS);
    const tick = async () => {
      if (disposed.current || ticking) return;
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
        if (disposed.current) return;
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
    try {
      await fetchImpl(`/api/boards/${encodeURIComponent(boardId)}/events`, {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(event),
      });
    } catch { /* events are best-effort */ }
  }

  // commit records operator edits: the merged map, the undo step, the PATCH
  // batch, the agents' event batch and the snapshot.
  function commit(next, { record = true } = {}) {
    if (!next.length) return;
    const step = [];
    for (const el of next) {
      const prev = elements.current.get(el.id);
      step.push({ id: el.id, before: prev || null });
      if (!eventBatch.current.before.has(el.id)) eventBatch.current.before.set(el.id, prev);
      eventBatch.current.ids.push(el.id);
      // server_version is the daemon's read stamp, not element data.
      const { server_version: _stamp, ...data } = el;
      const authored = { ...data, customData: { ...(el.customData || {}), author: 'operator' } };
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
    for (const arrow of reboundArrows(temp, ids)) out.set(arrow.id, arrow);
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
      return bump(cur || before, { ...before, version: cur?.version || before.version });
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

  // remove deletes elements; a deleted shape takes its label with it.
  function remove(ids) {
    const next = withLabels(elements.current, ids).map((id) => elements.current.get(id)).filter((el) => el && !el.isDeleted).map((el) => bump(el, { isDeleted: true }));
    commit(next);
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

  function align(how) {
    const owners = labelOwners(elements.current);
    const chosen = new Set(selected);
    // Labels follow their shapes and arrows follow their ends.
    const targets = selected.map((id) => elements.current.get(id))
      .filter((el) => el && !el.isDeleted && el.type !== 'arrow' && !chosen.has(owners.get(el.id)));
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
    const live = liveElements(elements.current);
    const panning = tool === 'hand' || event.button === 1;
    if (panning) {
      gesture.current = { kind: 'pan', sx: event.clientX, sy: event.clientY, view };
      return;
    }
    if (tool === 'select') {
      const hit = topHit(live, p.x, p.y, 6 / view.zoom);
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
      // The bound arrows' originals: the drag redraws them from these, and
      // pointerup restores them so the commit (and its undo) starts clean.
      const arrows = live.filter((el) => el.type === 'arrow' && !origs.has(el.id) && (origs.has(el.startBinding?.elementId) || origs.has(el.endBinding?.elementId)));
      gesture.current = { kind: onHandle ? 'resize' : 'move', id: hit.id, start: p, orig: hit, origs, arrows };
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
    const el = newElement(tool, {
      x: p.x, y: p.y, strokeColor: color,
      backgroundColor: ['rectangle', 'ellipse', 'diamond'].includes(tool) ? fill : 'transparent',
      ...(tool === 'arrow' || tool === 'line' ? { points: [[0, 0], [0, 0]] } : {}),
    });
    if (tool === 'arrow') {
      const from = shapeAt(live, p.x, p.y);
      if (from) el.startBinding = { elementId: from.id };
    }
    active.current = new Set([el.id]);
    elements.current.set(el.id, el);
    gesture.current = { kind: 'draw', id: el.id, start: p, raw: [[0, 0]] };
    redraw();
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
    if (!g) return;
    if (g.kind === 'band') { g.now = p; redraw(); return; }
    if (g.kind === 'pan') {
      setView({ ...g.view, x: g.view.x - (event.clientX - g.sx) / g.view.zoom, y: g.view.y - (event.clientY - g.sy) / g.view.zoom });
      return;
    }
    if (g.kind === 'erase') { eraseAt(p); return; }
    const el = elements.current.get(g.id);
    if (!el) return;
    if (g.kind === 'move' || g.kind === 'resize') {
      if (g.kind === 'move') {
        for (const [id, orig] of g.origs) elements.current.set(id, { ...orig, x: orig.x + p.x - g.start.x, y: orig.y + p.y - g.start.y });
      } else {
        elements.current.set(g.id, { ...g.orig, width: Math.max(8, g.orig.width + p.x - g.start.x), height: Math.max(8, g.orig.height + p.y - g.start.y) });
      }
      // Rebind from the originals so a long drag does not inflate versions.
      const temp = new Map(elements.current);
      for (const arrow of g.arrows) temp.set(arrow.id, arrow);
      for (const arrow of reboundArrows(temp, [...g.origs.keys()])) if (!g.origs.has(arrow.id)) elements.current.set(arrow.id, arrow);
      g.moved = true;
    } else if (g.kind === 'draw') {
      const dx = p.x - g.start.x; const dy = p.y - g.start.y;
      if (el.type === 'freedraw') {
        g.raw.push([dx, dy]);
        elements.current.set(g.id, { ...el, points: g.raw.slice() });
      } else if (el.type === 'arrow' || el.type === 'line') {
        elements.current.set(g.id, { ...el, points: [[0, 0], [dx, dy]], width: Math.abs(dx), height: Math.abs(dy) });
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
        changed.push(bump(orig, { x: now.x, y: now.y, width: now.width, height: now.height }));
      }
      for (const arrow of g.arrows) elements.current.set(arrow.id, arrow);
      commit(withArrows(changed));
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
      if (Math.hypot(dx, dy) < 6) { redraw(); return; }
      if (el.type === 'arrow') {
        const to = shapeAt(liveElements(elements.current), p.x, p.y, el.startBinding?.elementId);
        const from = el.startBinding && elements.current.get(el.startBinding.elementId);
        if (to) done = { ...done, endBinding: { elementId: to.id } };
        if (from && to) done = { ...done, ...connectArrow(from, to) };
      }
    } else if (el.width < 6 && el.height < 6) {
      redraw();
      return;
    }
    commit([done]);
    if (el.type !== 'freedraw') setSelected([done.id]);
  }

  function onWheel(event) {
    event.preventDefault();
    const rect = svgRef.current?.getBoundingClientRect?.() || { left: 0, top: 0 };
    if (event.ctrlKey || event.metaKey) {
      const zoom = Math.min(4, Math.max(0.2, view.zoom * Math.exp(-event.deltaY * 0.002)));
      const mx = (event.clientX - rect.left) / view.zoom + view.x;
      const my = (event.clientY - rect.top) / view.zoom + view.y;
      setView({ zoom, x: mx - (event.clientX - rect.left) / zoom, y: my - (event.clientY - rect.top) / zoom });
    } else {
      setView({ ...view, x: view.x + event.deltaX / view.zoom, y: view.y + event.deltaY / view.zoom });
    }
  }

  // -- text -------------------------------------------------------------------

  function beginText(el, p = null) {
    if (el) {
      setEditingText({ id: el.id, x: el.x, y: el.y, text: el.text || '', size: el.fontSize || 20, color: safeColor(el.strokeColor, '#1e1e1e'), isNew: false });
    } else {
      setEditingText({ id: '', x: p.x, y: p.y - 12, text: '', size: 20, color, isNew: true });
    }
  }

  // beginLabel edits a shape's label, or starts one (`<id>-label`, as the
  // CLI writes labels).
  function beginLabel(shape) {
    const label = labelOf(elements.current, shape.id);
    if (label) { beginText(label); return; }
    const c = center(shape);
    setEditingText({ id: '', shapeId: shape.id, x: c.x - 60, y: c.y - 12, text: '', size: 20, color: safeColor(shape.strokeColor, '#1e1e1e'), isNew: true });
  }

  function onDoubleClick(event) {
    const p = toBoard(event);
    const live = liveElements(elements.current);
    const hit = topHit(live, p.x, p.y);
    if (hit?.type === 'text') { beginText(hit); return; }
    // Anywhere inside a shape (not only on an unfilled shape's outline).
    const shape = shapeAt(live, p.x, p.y);
    if (shape && shape.type !== 'text') beginLabel(shape);
    else if (!hit && tool === 'select') beginText(null, p);
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
        const label = newLabel(shape, text, { strokeColor: t.color });
        // A deleted `<id>-label` outranks a fresh version 1: revive it.
        const old = elements.current.get(label.id);
        commit([old ? bump(old, { ...label, version: old.version, isDeleted: false }) : label]);
      }
      else commit([newElement('text', { x: t.x, y: t.y, text, fontSize: t.size, strokeColor: t.color })]);
      return;
    }
    const cur = elements.current.get(t.id);
    if (!cur) return;
    if (!text) remove([t.id]);
    else if (text !== cur.text) commit([bump(cur, { text, width: 0, height: 0 })]);
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
    const ours = (event) => {
      const target = event.target;
      if (editingText || target?.closest?.('input, textarea, [contenteditable="true"], .companion-window')) return false;
      if (!shown()) return false;
      return !!rootRef.current?.contains?.(target) || (target === win?.document?.body && lastEditor === editorToken);
    };
    const onKey = (event) => {
      if (!ours(event)) return;
      const key = event.key.toLowerCase();
      const mod = event.ctrlKey || event.metaKey;
      if (mod && key === 'z') {
        event.preventDefault();
        undo(event.shiftKey);
        return;
      }
      if (mod && key === 'y') { event.preventDefault(); undo(true); return; }
      if (mod && key === 'a') { event.preventDefault(); setSelected(liveElements(elements.current).map((el) => el.id)); return; }
      if (mod && key === 'd') {
        event.preventDefault();
        const source = selectionWithLabels();
        if (source.length) {
          const copies = cloneElements(source, 20, 20);
          commit(copies);
          setSelected(copies.map((el) => el.id));
        }
        return;
      }
      if (mod || event.altKey) return;
      if ((event.key === 'Delete' || event.key === 'Backspace') && selected.length) { event.preventDefault(); remove(selected); return; }
      if (event.key === 'Escape') { setSelected([]); setTool('select'); return; }
      const t = BOARD_TOOLS.find((candidate) => candidate.key === key);
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
      event.clipboardData?.setData('text/plain', clipboardText(selectionWithLabels()));
      if (event.type === 'cut') remove(selected);
    };
    const onPaste = (event) => {
      if (!ours(event)) return;
      const text = event.clipboardData?.getData('text/plain') || '';
      if (!text) return;
      event.preventDefault();
      const source = parseClipboard(text);
      if (source) { pasteElements(source); return; }
      // Plain text becomes a text element.
      const clean = cleanText(text);
      if (!clean.trim()) return;
      const at = pointerAt.current || { x: view.x + 40, y: view.y + 40 };
      const el = newElement('text', { x: at.x, y: at.y, text: clean, fontSize: 20, strokeColor: color });
      commit([el]);
      setSelected([el.id]);
    };
    win?.addEventListener?.('keydown', onKey);
    win?.addEventListener?.('copy', onCopy);
    win?.addEventListener?.('cut', onCopy);
    win?.addEventListener?.('paste', onPaste);
    return () => {
      win?.removeEventListener?.('keydown', onKey);
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

  // sendInvite tells an agent how to use the board.
  function sendInvite(agentId, why = "You're invited to") {
    return fetchImpl('/api/operator-message', {
      method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        to: agentId, subject: `[board ${boardId}] you're invited`,
        body: `${why} the shared board "${meta.title}" (${boardId}). The operator draws there; you get a short event when their edits settle.\n`
          + `Look: \`tclaude agent board show ${boardId}\` (elements as text; \`--view\` for what the operator has on screen) or \`tclaude agent board snapshot ${boardId} -o /tmp/${boardId}.png\` (an image to view).\n`
          + `Draw: \`tclaude agent board add ${boardId} rectangle --x 100 --y 100 --w 160 --h 80 --text "Idea"\`, arrows with --from/--to element ids; build a flow with \`--right-of <id> --link\` and tidy it with \`board layout ${boardId}\`; \`board rm\`, \`board move\`, \`board text\`. See \`tclaude agent board --help\`.`,
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
      await Promise.allSettled(fresh.map((id) => sendInvite(id, `Your group "${result.group}" now has`)));
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

  const live = liveElements(elements.current);
  const sels = selected.map((id) => elements.current.get(id)).filter((el) => el && !el.isDeleted);
  const sel = sels.length === 1 ? sels[0] : null;
  const band = gesture.current?.kind === 'band' ? normalizeBox(gesture.current.start.x, gesture.current.start.y, gesture.current.now.x, gesture.current.now.y) : null;
  const arrangeable = (() => {
    const owners = labelOwners(elements.current);
    const chosen = new Set(selected);
    return sels.filter((el) => el.type !== 'arrow' && !chosen.has(owners.get(el.id))).length;
  })();
  const pad = 6 / view.zoom;
  const transform = `scale(${view.zoom}) translate(${-view.x} ${-view.y})`;
  const drawing = [...presence].filter(([, state]) => state === 'drawing').map(([id]) => agents.get(id)?.name || id.slice(0, 12));
  const groupNames = [...new Set([meta.group, ...(snapshot.value?.groups || []).filter((g) => g?.name && !g.scribe && !g.archived).map((g) => g.name)].filter(Boolean))].sort();
  const candidates = [...agents.values()].filter((a) => a.online && !meta.participants.includes(a.agentId) && !meta.groupMembers.includes(a.agentId)).sort((a, b) => (b.steward - a.steward) || a.name.localeCompare(b.name));
  void tick;

  return html`<div ref=${rootRef} class="board-editor" onPointerDownCapture=${() => { lastEditor = editorToken; }}>
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
        <span class="board-invite-wrap">
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
        onClick=${() => import('./board-export.js').then((m) => m.downloadText(`${boardId}.excalidraw`, toExcalidraw(liveElements(elements.current))))}>⤓</button>
      ${headActions}
    </header>
    <div class="board-toolbar" role="toolbar" aria-label="Board tools">
      ${BOARD_TOOLS.map((t) => html`<button key=${t.id} type="button" class=${`board-tool${tool === t.id ? ' on' : ''}`}
        aria-pressed=${tool === t.id ? 'true' : 'false'} title=${`${t.label} (${t.key.toUpperCase()})`} aria-label=${t.label}
        onClick=${() => setTool(t.id)}>${t.icon}</button>`)}
      <span class="board-sep"></span>
      ${BOARD_COLORS.map((c) => html`<button key=${c} type="button" class=${`board-swatch${color === c ? ' on' : ''}`} style=${`background:${c}`}
        aria-label=${`Stroke ${c}`} onClick=${() => { setColor(c); if (sels.length) commit(sels.map((el) => bump(el, { strokeColor: c }))); }}></button>`)}
      <span class="board-sep"></span>
      ${BOARD_FILLS.map((c) => html`<button key=${c} type="button" class=${`board-swatch fill${fill === c ? ' on' : ''}`}
        style=${c === 'transparent' ? '' : `background:${c}`} aria-label=${c === 'transparent' ? 'No fill' : `Fill ${c}`}
        onClick=${() => { setFill(c); commit(sels.filter((el) => ['rectangle', 'ellipse', 'diamond'].includes(el.type)).map((el) => bump(el, { backgroundColor: c }))); }}>${c === 'transparent' ? '∅' : ''}</button>`)}
      <span class="board-sep"></span>
      <button type="button" class="board-tool" title="Undo (Ctrl+Z)" aria-label="Undo" onClick=${() => undo(false)}>↶</button>
      <button type="button" class="board-tool" title="Redo (Ctrl+Shift+Z)" aria-label="Redo" onClick=${() => undo(true)}>↷</button>
      ${sels.length > 0 && html`<button type="button" class="board-tool" title="Delete (Del)" aria-label="Delete selection" onClick=${() => remove(selected)}>🗑</button>`}
      ${arrangeable >= 2 && html`<span class="board-sep"></span>${ALIGNMENTS.filter((a) => arrangeable >= (a.min || 2)).map((a) => html`<button key=${a.id} type="button" class="board-tool"
        title=${a.label} aria-label=${a.label} onClick=${() => align(a.id)}>${a.icon}</button>`)}`}
      <span class="board-zoom">${Math.round(view.zoom * 100)}%</span>
    </div>
    <div class="board-canvas-wrap">
      <svg ref=${svgRef} class=${`board-canvas tool-${tool}`} role="img" aria-label=${`Board ${meta.title}`} data-board-id=${boardId}
        onPointerDown=${onPointerDown} onPointerMove=${onPointerMove} onPointerUp=${onPointerUp} onPointerCancel=${onPointerUp}
        onWheel=${onWheel} onDblClick=${onDoubleClick}>
        <g data-board-viewport transform=${transform}>
          ${live.map((el) => html`<${BoardElement} key=${el.id} el=${el} flash=${flash.has(el.id)} />`)}
          ${(sels.length > 0 || band) && html`<g data-board-chrome>
            ${sels.map((el) => {
              const b = elementBounds(el);
              return html`<rect key=${el.id} class="board-selection" x=${b.x - pad} y=${b.y - pad} width=${b.width + pad * 2} height=${b.height + pad * 2} />`;
            })}
            ${sel && !sel.points && sel.type !== 'text' && (() => {
              const b = elementBounds(sel);
              return html`<rect class="board-handle" x=${b.x + b.width - 4 / view.zoom} y=${b.y + b.height - 4 / view.zoom} width=${8 / view.zoom} height=${8 / view.zoom} />`;
            })()}
            ${band && html`<rect class="board-band" x=${band.x} y=${band.y} width=${band.width} height=${band.height} />`}
          </g>`}
        </g>
      </svg>
      ${editingText && html`<textarea class="board-text-input" autoFocus
        style=${`left:${(editingText.x - view.x) * view.zoom}px;top:${(editingText.y - view.y) * view.zoom}px;font-size:${editingText.size * view.zoom}px;color:${editingText.color}`}
        value=${editingText.text}
        onInput=${(event) => setEditingText({ ...editingText, text: event.currentTarget.value })}
        onKeyDown=${(event) => {
          if (event.key === 'Escape' || (event.key === 'Enter' && !event.shiftKey)) { event.preventDefault(); finishText(); }
        }}
        onBlur=${finishText}></textarea>`}
      ${drawing.length > 0 && html`<div class="board-drawing-note" role="status">✎ ${drawing.join(', ')} ${drawing.length === 1 ? 'is' : 'are'} drawing…</div>`}
      ${!live.length && status === 'ready' && html`<div class="board-empty-hint">Pick a tool and draw. Invite an agent to draw with you; it reads the board as shapes and as an image.</div>`}
    </div>
  </div>`;
}

// -- the board list ---------------------------------------------------------------

// BoardList offers every board, and a new one; `compact` is the strip's + menu.
export function BoardList({ fetchImpl, notify, onOpen, compact = false }) {
  const [boards, setBoards] = useState(null);
  const [title, setTitle] = useState('');
  const [error, setError] = useState('');
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
  async function create(event) {
    event.preventDefault();
    const clean = title.trim();
    if (!clean) return;
    try {
      const board = await jsonOrThrow(await fetchImpl('/api/boards', {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title: clean, id: boardIdFromTitle(clean) || undefined }),
      }));
      setTitle('');
      onOpen(board.id, board.title || clean);
    } catch (err) {
      notify(`Could not create the board: ${err?.message || err}`, true);
    }
  }
  return html`<div class=${`board-list${compact ? ' compact' : ''}`}>
    <form class="board-new" onSubmit=${create}>
      <input type="text" placeholder="New board title, e.g. Release flow" aria-label="New board title" maxLength="120"
        value=${title} onInput=${(event) => setTitle(event.currentTarget.value)} />
      <button type="submit" disabled=${!title.trim()}>Create board</button>
    </form>
    ${error && html`<p class="board-error" role="alert">Boards are unavailable: ${error}</p>`}
    ${boards === null && !error && html`<p class="board-muted">Loading boards…</p>`}
    ${boards && !boards.length && html`<p class="board-muted">No boards yet. A board is a shared canvas: you draw, agents read it as shapes and as an image, and draw back.</p>`}
    <div class="board-cards">
      ${(boards || []).map((b) => html`<button key=${b.id} type="button" class="board-card" onClick=${() => onOpen(b.id, b.title || b.id)}>
        <b>${b.title || b.id}</b>
        <span>${b.element_count ?? 0} element${b.element_count === 1 ? '' : 's'}${b.participants?.length ? ` · ${b.participants.length} agent${b.participants.length === 1 ? '' : 's'}` : ''}${b.group ? ` · group ${b.group}` : ''}</span>
        <small>${b.updated_at ? new Date(b.updated_at).toLocaleString() : ''}</small>
      </button>`)}
    </div>
  </div>`;
}

// BoardsGate leaves a board out while features.collab is off, and mounts it
// fresh when it turns on, so nothing asks the daemon for what it refuses.
export function BoardsGate({ snapshot, children }) {
  return snapshot.value?.collab_enabled === true ? children : null;
}
