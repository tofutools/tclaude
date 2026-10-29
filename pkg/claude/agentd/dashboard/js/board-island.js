// board-island.js -- the collab board (tcl-4f86kp): a shared drawing board the
// operator and agents edit together, under the Collab nav entry next to
// Terminals. See board-model.js for the element model and merge rules.
//
// The editor keeps every element in a Map, renders it as SVG, and syncs with
// the daemon: local edits are batched into PATCH /api/boards/{id}, and a poll
// (GET ?since=<version>) merges what agents changed. When the operator's edits
// settle, the board's participating agents get one short text event describing
// them (POST /api/boards/{id}/events), and a PNG snapshot is uploaded so agents
// can look at the board as well as read it.
//
// Element text is agent- or operator-supplied and is rendered as SVG text.

import { h, render } from 'preact';
import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { signal } from '@preact/signals';
import {
  BOARD_COLORS, BOARD_FILLS, BOARD_TOOLS, boardIdFromTitle, bump, connectArrow, contentBounds,
  describeChanges, elementBounds, isBoardId, liveElements, mergeElements, newElement, normalizeBox,
  reboundArrows, shapeAt, simplifyStroke, toExcalidraw, topHit,
} from './board-model.js';
import { companionAgents } from './companion-model.js';

const html = htm.bind(h);

export const BOARD_POLL_MS = 1000;
export const BOARD_FLUSH_MS = 250;
export const BOARD_EVENT_IDLE_MS = 1500;
export const BOARD_SNAPSHOT_IDLE_MS = 2500;
const HISTORY_MAX = 100;

async function jsonOrThrow(response) {
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.message || body.error || `${response.status} ${response.statusText}`);
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

function dash(el) {
  if (el.strokeStyle === 'dashed') return '8 6';
  if (el.strokeStyle === 'dotted') return '2 5';
  return null;
}

export function BoardElement({ el, flash = false }) {
  const stroke = el.strokeColor || '#1e1e1e';
  const fill = el.backgroundColor && el.backgroundColor !== 'transparent' ? el.backgroundColor : 'none';
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

function BoardEditor({ boardId, fetchImpl, notify, snapshot, onBack, win, rasterize, onOpenAgent }) {
  const elements = useRef(new Map());
  const version = useRef(0);
  const [tick, setTick] = useState(0);
  const redraw = useCallback(() => setTick((n) => n + 1), []);
  const [meta, setMeta] = useState({ title: boardId, participants: [] });
  const [status, setStatus] = useState('loading');
  const [tool, setTool] = useState('select');
  const [color, setColor] = useState(BOARD_COLORS[0]);
  const [fill, setFill] = useState(BOARD_FILLS[0]);
  const [selected, setSelected] = useState('');
  const [view, setView] = useState({ x: -40, y: -40, zoom: 1 });
  const [editingText, setEditingText] = useState(null); // {id, x, y, text, isNew}
  const [flash, setFlash] = useState(() => new Set());
  const [invite, setInvite] = useState(false);
  const svgRef = useRef(null);
  const pending = useRef(new Map()); // id -> element awaiting PATCH
  const active = useRef(new Set()); // ids being manipulated right now
  const history = useRef({ undo: [], redo: [] });
  const eventBatch = useRef({ ids: [], before: new Map() });
  const timers = useRef({});
  const gesture = useRef(null);
  const disposed = useRef(false);
  // Timer callbacks read the latest participants, not their render's.
  const metaRef = useRef(meta);
  metaRef.current = meta;

  const agents = useMemo(() => companionAgents(snapshot.value), [snapshot.value]);

  // Load, then poll for agents' edits.
  useEffect(() => {
    disposed.current = false;
    let pollTimer = null;
    const load = async (since) => {
      try {
        const query = since ? `?since=${since}` : '';
        const board = await jsonOrThrow(await fetchImpl(`/api/boards/${encodeURIComponent(boardId)}${query}`, { credentials: 'same-origin' }));
        if (disposed.current) return;
        setMeta({ title: board.title || boardId, participants: Array.isArray(board.participants) ? board.participants : [] });
        const skip = new Set([...active.current, ...pending.current.keys()]);
        const changed = mergeElements(elements.current, board.elements || [], skip);
        if (Number.isFinite(board.version)) version.current = Math.max(version.current, board.version);
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
        if (!disposed.current) setStatus(since ? 'offline' : `error: ${error?.message || error}`);
      }
    };
    void load(0).then(() => {
      const tickPoll = async () => {
        if (disposed.current) return;
        if (win?.document?.visibilityState !== 'hidden') await load(version.current);
        if (!disposed.current) pollTimer = setTimeout(tickPoll, BOARD_POLL_MS);
      };
      pollTimer = setTimeout(tickPoll, BOARD_POLL_MS);
    });
    return () => {
      disposed.current = true;
      clearTimeout(pollTimer);
      for (const timer of Object.values(timers.current)) clearTimeout(timer);
      void flush();
    };
  }, [boardId]);

  async function flush() {
    clearTimeout(timers.current.flush);
    if (!pending.current.size) return;
    const batch = [...pending.current.values()];
    pending.current.clear();
    try {
      const result = await jsonOrThrow(await fetchImpl(`/api/boards/${encodeURIComponent(boardId)}`, {
        method: 'PATCH', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ elements: batch }),
      }));
      if (Number.isFinite(result.version)) version.current = Math.max(version.current, result.version);
    } catch (error) {
      // Keep the edits; the next change or flush retries them.
      for (const el of batch) if (!pending.current.has(el.id)) pending.current.set(el.id, el);
      if (!disposed.current) {
        setStatus('offline');
        clearTimeout(timers.current.flush);
        timers.current.flush = setTimeout(() => void flush(), 2000);
      }
    }
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
    timers.current.event = setTimeout(async () => {
      const { ids, before } = eventBatch.current;
      eventBatch.current = { ids: [], before: new Map() };
      if (!metaRef.current.participants.length) return;
      const text = describeChanges(elements.current, ids, before);
      if (!text) return;
      try {
        await fetchImpl(`/api/boards/${encodeURIComponent(boardId)}/events`, {
          method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ text: `The operator ${text}.` }),
        });
      } catch { /* events are best-effort */ }
    }, BOARD_EVENT_IDLE_MS);
  }

  // commit records operator edits: the merged map, the undo history, the
  // PATCH batch, the agents' event batch and the snapshot.
  function commit(next, { record = true } = {}) {
    if (!next.length) return;
    const undoEntry = [];
    for (const el of next) {
      const prev = elements.current.get(el.id);
      undoEntry.push({ id: el.id, before: prev || null });
      if (!eventBatch.current.before.has(el.id)) eventBatch.current.before.set(el.id, prev);
      eventBatch.current.ids.push(el.id);
      const authored = { ...el, customData: { ...(el.customData || {}), author: 'operator' } };
      elements.current.set(el.id, authored);
      pending.current.set(el.id, authored);
    }
    if (record) {
      history.current.undo.push(undoEntry);
      if (history.current.undo.length > HISTORY_MAX) history.current.undo.shift();
      history.current.redo = [];
    }
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
    return [...changed, ...reboundArrows(temp, ids)];
  }

  function undo(redo = false) {
    const from = redo ? history.current.redo : history.current.undo;
    const to = redo ? history.current.undo : history.current.redo;
    const entry = from.pop();
    if (!entry) return;
    const inverse = [];
    const next = entry.map(({ id, before }) => {
      const cur = elements.current.get(id);
      inverse.push({ id, before: cur || null });
      if (before) return bump(cur || before, { ...before, version: cur?.version || before.version });
      return cur ? bump(cur, { isDeleted: true }) : null;
    }).filter(Boolean);
    to.push(inverse);
    commit(next, { record: false });
  }

  function remove(ids) {
    const next = ids.map((id) => elements.current.get(id)).filter((el) => el && !el.isDeleted).map((el) => bump(el, { isDeleted: true }));
    commit(next);
    setSelected('');
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
    const svg = svgRef.current;
    svg?.setPointerCapture?.(event.pointerId);
    const live = liveElements(elements.current);
    const panning = tool === 'hand' || event.button === 1 || event.shiftKey && tool === 'select' && !topHit(live, p.x, p.y);
    if (panning) {
      gesture.current = { kind: 'pan', sx: event.clientX, sy: event.clientY, view };
      return;
    }
    if (tool === 'select') {
      const hit = topHit(live, p.x, p.y, 6 / view.zoom);
      setSelected(hit?.id || '');
      if (hit) {
        const b = elementBounds(hit);
        const onHandle = Math.abs(p.x - (b.x + b.width)) < 10 / view.zoom && Math.abs(p.y - (b.y + b.height)) < 10 / view.zoom && !hit.points && hit.type !== 'text';
        active.current = new Set([hit.id]);
        gesture.current = { kind: onHandle ? 'resize' : 'move', id: hit.id, start: p, orig: hit };
      }
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
    const g = gesture.current;
    if (!g) return;
    const p = toBoard(event);
    if (g.kind === 'pan') {
      setView({ ...g.view, x: g.view.x - (event.clientX - g.sx) / g.view.zoom, y: g.view.y - (event.clientY - g.sy) / g.view.zoom });
      return;
    }
    if (g.kind === 'erase') { eraseAt(p); return; }
    const el = elements.current.get(g.id);
    if (!el) return;
    if (g.kind === 'move') {
      const moved = { ...g.orig, x: g.orig.x + p.x - g.start.x, y: g.orig.y + p.y - g.start.y };
      elements.current.set(g.id, moved);
      for (const arrow of reboundArrows(elements.current, [g.id])) elements.current.set(arrow.id, arrow);
      g.moved = true;
    } else if (g.kind === 'resize') {
      elements.current.set(g.id, { ...g.orig, width: Math.max(8, g.orig.width + p.x - g.start.x), height: Math.max(8, g.orig.height + p.y - g.start.y) });
      for (const arrow of reboundArrows(elements.current, [g.id])) elements.current.set(arrow.id, arrow);
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
    if (g.kind === 'erase') {
      const erased = [...g.erased].map((id) => elements.current.get(id)).filter(Boolean);
      // Restore the pre-erase copies, then commit the deletions as edits.
      for (const el of erased) elements.current.set(el.id, { ...el, isDeleted: false });
      remove(erased.map((el) => el.id));
      return;
    }
    if (g.kind === 'move' || g.kind === 'resize') {
      if (!g.moved) return;
      const now = elements.current.get(g.id);
      elements.current.set(g.id, g.orig);
      commit(withArrows([bump(g.orig, { x: now.x, y: now.y, width: now.width, height: now.height })]));
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
    if (el.type !== 'freedraw') setSelected(done.id);
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
      setEditingText({ id: el.id, x: el.x, y: el.y, text: el.text || '', size: el.fontSize || 20, color: el.strokeColor, isNew: false });
    } else {
      setEditingText({ id: '', x: p.x, y: p.y - 12, text: '', size: 20, color, isNew: true });
    }
  }

  function finishText() {
    const t = editingText;
    setEditingText(null);
    if (!t) return;
    const text = t.text.replace(/\s+$/, '');
    if (t.isNew) {
      if (text) {
        const el = newElement('text', { x: t.x, y: t.y, text, fontSize: t.size, strokeColor: t.color });
        commit([el]);
      }
      return;
    }
    const cur = elements.current.get(t.id);
    if (!cur) return;
    if (!text) remove([t.id]);
    else if (text !== cur.text) commit([bump(cur, { text, width: 0, height: 0 })]);
  }

  // -- keyboard ---------------------------------------------------------------

  useEffect(() => {
    const onKey = (event) => {
      const target = event.target;
      if (editingText || target?.closest?.('input, textarea, [contenteditable="true"], .companion-window')) return;
      if (!target?.closest?.('#tab-boards') && target !== win?.document?.body) return;
      const key = event.key.toLowerCase();
      if ((event.ctrlKey || event.metaKey) && key === 'z') {
        event.preventDefault();
        undo(event.shiftKey);
        return;
      }
      if ((event.ctrlKey || event.metaKey) && key === 'y') { event.preventDefault(); undo(true); return; }
      if (event.ctrlKey || event.metaKey || event.altKey) return;
      if ((event.key === 'Delete' || event.key === 'Backspace') && selected) { event.preventDefault(); remove([selected]); return; }
      if (event.key === 'Escape') { setSelected(''); setTool('select'); return; }
      const t = BOARD_TOOLS.find((candidate) => candidate.key === key);
      if (t) setTool(t.id);
    };
    win?.addEventListener?.('keydown', onKey);
    return () => win?.removeEventListener?.('keydown', onKey);
  }, [editingText, selected, view, tool]);

  // -- participants -------------------------------------------------------------

  async function addParticipant(agentId) {
    setInvite(false);
    try {
      await jsonOrThrow(await fetchImpl(`/api/boards/${encodeURIComponent(boardId)}/participants`, {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agent_id: agentId }),
      }));
      setMeta((m) => ({ ...m, participants: [...new Set([...m.participants, agentId])] }));
      const name = agents.get(agentId)?.name || agentId;
      await fetchImpl('/api/operator-message', {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          to: agentId, subject: `[board ${boardId}] you're invited`,
          body: `You're invited to the shared board "${meta.title}" (${boardId}). The operator draws there; you get a short event when their edits settle.\n`
            + `Look: \`tclaude agent board show ${boardId}\` (elements as text) or \`tclaude agent board snapshot ${boardId} -o /tmp/${boardId}.png\` (an image to view).\n`
            + `Draw: \`tclaude agent board add ${boardId} rectangle --x 100 --y 100 --w 160 --h 80 --text "Idea"\`, arrows with --from/--to element ids, \`board rm\`, \`board move\`, \`board text\`. See \`tclaude agent board --help\`.`,
        }),
      });
      notify(`Invited ${name} to the board.`);
    } catch (error) {
      notify(`Could not invite: ${error?.message || error}`, true);
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
  const sel = selected ? elements.current.get(selected) : null;
  const selBox = sel && !sel.isDeleted ? elementBounds(sel) : null;
  const pad = 6 / view.zoom;
  const transform = `scale(${view.zoom}) translate(${-view.x} ${-view.y})`;
  const candidates = [...agents.values()].filter((a) => a.online && !meta.participants.includes(a.agentId)).sort((a, b) => (b.steward - a.steward) || a.name.localeCompare(b.name));
  void tick;

  return html`<div class="board-editor">
    <header class="board-head">
      <button type="button" class="board-back" onClick=${onBack} aria-label="All boards">‹ Boards</button>
      <h2 class="board-title">${meta.title}</h2>
      <span class=${`board-sync ${status}`} title="Sync with the daemon">${status === 'ready' ? (pending.current.size ? 'saving…' : 'synced') : status}</span>
      <div class="board-people" aria-label="Agents on this board">
        ${meta.participants.map((id) => {
          const agent = agents.get(id);
          return html`<span key=${id} class=${`board-person${agent?.online ? '' : ' offline'}`} title=${agent ? `${agent.name}${agent.online ? '' : ' (offline)'}` : id}>
            <button type="button" class="board-person-name" onClick=${() => onOpenAgent?.(id)}>${agent?.steward ? '✦ ' : ''}${agent?.name || id.slice(0, 12)}</button>
            <button type="button" class="board-person-x" aria-label=${`Remove ${agent?.name || id} from the board`} onClick=${() => removeParticipant(id)}>×</button>
          </span>`;
        })}
        <span class="board-invite-wrap">
          <button type="button" class="board-invite" aria-expanded=${invite ? 'true' : 'false'} onClick=${() => setInvite((v) => !v)}>＋ Invite agent</button>
          ${invite && html`<div class="board-invite-menu" role="menu">
            ${candidates.length ? candidates.slice(0, 30).map((a) => html`<button key=${a.agentId} type="button" role="menuitem" onClick=${() => addParticipant(a.agentId)}>${a.steward ? '✦ ' : ''}${a.name}</button>`)
              : html`<div class="board-invite-empty">No other live agents. Summon the Steward from the companion (Ctrl+.).</div>`}
          </div>`}
        </span>
      </div>
      <button type="button" class="board-export" title="Download as .excalidraw"
        onClick=${() => import('./board-export.js').then((m) => m.downloadText(`${boardId}.excalidraw`, toExcalidraw(liveElements(elements.current))))}>⤓</button>
    </header>
    <div class="board-toolbar" role="toolbar" aria-label="Board tools">
      ${BOARD_TOOLS.map((t) => html`<button key=${t.id} type="button" class=${`board-tool${tool === t.id ? ' on' : ''}`}
        aria-pressed=${tool === t.id ? 'true' : 'false'} title=${`${t.label} (${t.key.toUpperCase()})`} aria-label=${t.label}
        onClick=${() => setTool(t.id)}>${t.icon}</button>`)}
      <span class="board-sep"></span>
      ${BOARD_COLORS.map((c) => html`<button key=${c} type="button" class=${`board-swatch${color === c ? ' on' : ''}`} style=${`background:${c}`}
        aria-label=${`Stroke ${c}`} onClick=${() => { setColor(c); if (sel) commit([bump(sel, { strokeColor: c })]); }}></button>`)}
      <span class="board-sep"></span>
      ${BOARD_FILLS.map((c) => html`<button key=${c} type="button" class=${`board-swatch fill${fill === c ? ' on' : ''}`}
        style=${c === 'transparent' ? '' : `background:${c}`} aria-label=${c === 'transparent' ? 'No fill' : `Fill ${c}`}
        onClick=${() => { setFill(c); if (sel && ['rectangle', 'ellipse', 'diamond'].includes(sel.type)) commit([bump(sel, { backgroundColor: c })]); }}>${c === 'transparent' ? '∅' : ''}</button>`)}
      <span class="board-sep"></span>
      <button type="button" class="board-tool" title="Undo (Ctrl+Z)" aria-label="Undo" onClick=${() => undo(false)}>↶</button>
      <button type="button" class="board-tool" title="Redo (Ctrl+Shift+Z)" aria-label="Redo" onClick=${() => undo(true)}>↷</button>
      ${sel && html`<button type="button" class="board-tool" title="Delete (Del)" aria-label="Delete selection" onClick=${() => remove([sel.id])}>🗑</button>`}
      <span class="board-zoom">${Math.round(view.zoom * 100)}%</span>
    </div>
    <div class="board-canvas-wrap">
      <svg ref=${svgRef} class=${`board-canvas tool-${tool}`} role="img" aria-label=${`Board ${meta.title}`}
        onPointerDown=${onPointerDown} onPointerMove=${onPointerMove} onPointerUp=${onPointerUp} onPointerCancel=${onPointerUp}
        onWheel=${onWheel} onDblClick=${(event) => { const p = toBoard(event); const hit = topHit(live, p.x, p.y); if (hit?.type === 'text') beginText(hit); else if (tool === 'select') beginText(null, p); }}>
        <g data-board-viewport transform=${transform}>
          ${live.map((el) => html`<${BoardElement} key=${el.id} el=${el} flash=${flash.has(el.id)} />`)}
          ${selBox && html`<g data-board-chrome>
            <rect class="board-selection" x=${selBox.x - pad} y=${selBox.y - pad} width=${selBox.width + pad * 2} height=${selBox.height + pad * 2} />
            ${!sel.points && sel.type !== 'text' && html`<rect class="board-handle" x=${selBox.x + selBox.width - 4 / view.zoom} y=${selBox.y + selBox.height - 4 / view.zoom} width=${8 / view.zoom} height=${8 / view.zoom} />`}
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
      ${!live.length && status === 'ready' && html`<div class="board-empty-hint">Pick a tool and draw. Invite an agent to draw with you; it reads the board as shapes and as an image.</div>`}
    </div>
  </div>`;
}

// -- the board list ---------------------------------------------------------------

function BoardList({ fetchImpl, notify, onOpen }) {
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
      onOpen(board.id);
    } catch (err) {
      notify(`Could not create the board: ${err?.message || err}`, true);
    }
  }
  return html`<div class="board-list">
    <form class="board-new" onSubmit=${create}>
      <input type="text" placeholder="New board title, e.g. Release flow" aria-label="New board title" maxLength="120"
        value=${title} onInput=${(event) => setTitle(event.currentTarget.value)} />
      <button type="submit" disabled=${!title.trim()}>Create board</button>
    </form>
    ${error && html`<p class="board-error" role="alert">Boards are unavailable: ${error}</p>`}
    ${boards === null && !error && html`<p class="board-muted">Loading boards…</p>`}
    ${boards && !boards.length && html`<p class="board-muted">No boards yet. A board is a shared canvas: you draw, agents read it as shapes and as an image, and draw back.</p>`}
    <div class="board-cards">
      ${(boards || []).map((b) => html`<button key=${b.id} type="button" class="board-card" onClick=${() => onOpen(b.id)}>
        <b>${b.title || b.id}</b>
        <span>${b.element_count ?? 0} element${b.element_count === 1 ? '' : 's'}${b.participants?.length ? ` · ${b.participants.length} agent${b.participants.length === 1 ? '' : 's'}` : ''}</span>
        <small>${b.updated_at ? new Date(b.updated_at).toLocaleString() : ''}</small>
      </button>`)}
    </div>
  </div>`;
}

// -- the Collab switch --------------------------------------------------------------

// CollabSwitch is the Terminals | Boards control at the top of both Collab
// sections. It clicks the real nav anchors, so routing and lazy loading stay
// with the nav.
export function CollabSwitch({ current, win = globalThis.window }) {
  const go = (tab) => {
    if (tab === current) return;
    win?.document?.querySelector?.(`nav [data-tab="${tab}"]`)?.click();
  };
  return html`<div class="collab-switch" role="tablist" aria-label="Collab views">
    ${[['terminals', '⌨ Terminals'], ['boards', '✎ Boards']].map(([tab, label]) => html`<button key=${tab} type="button" role="tab"
      aria-selected=${tab === current ? 'true' : 'false'} class=${tab === current ? 'on' : ''} onClick=${() => go(tab)}>${label}</button>`)}
  </div>`;
}

// -- the island ---------------------------------------------------------------------

export function createBoardsState() {
  return { location: signal({ tab: 'boards' }) };
}

export function BoardsApp({ state, snapshot, fetchImpl = globalThis.fetch, notify = () => {}, win = globalThis.window, rasterize = null, openAgent = null }) {
  const [boardId, setBoardId] = useState(() => (isBoardId(state.location.value.selection) ? state.location.value.selection : ''));
  const open = useCallback((id, { announce = true } = {}) => {
    const next = isBoardId(id) ? id : '';
    setBoardId(next);
    const location = next ? { tab: 'boards', selection: next } : { tab: 'boards' };
    state.location.value = location;
    if (announce) win?.document?.dispatchEvent?.(new win.CustomEvent('tclaude:navigated', { detail: { location } }));
  }, [state, win]);
  // The router asks for a location (Back/Forward, a deep link, a guide cue).
  useEffect(() => {
    const onRestore = (event) => {
      const location = event.detail?.location;
      if (location?.tab !== 'boards') return;
      open(location.selection || '', { announce: false });
    };
    win?.document?.addEventListener?.('tclaude:restore-location', onRestore);
    return () => win?.document?.removeEventListener?.('tclaude:restore-location', onRestore);
  }, [open, win]);
  return html`<div class="boards-app">
    <${CollabSwitch} current="boards" win=${win} />
    ${boardId
      ? html`<${BoardEditor} key=${boardId} boardId=${boardId} fetchImpl=${fetchImpl} notify=${notify} snapshot=${snapshot}
          win=${win} rasterize=${rasterize} onBack=${() => open('')} onOpenAgent=${openAgent} />`
      : html`<${BoardList} fetchImpl=${fetchImpl} notify=${notify} onOpen=${(id) => open(id)} />`}
  </div>`;
}

export function mountBoardsIsland({ host, switchHost, registerCleanup, ...props }) {
  render(html`<${BoardsApp} ...${props} />`, host);
  registerCleanup(() => render(null, host));
  if (switchHost) {
    render(html`<${CollabSwitch} current="terminals" win=${props.win} />`, switchHost);
    registerCleanup(() => render(null, switchHost));
  }
}
