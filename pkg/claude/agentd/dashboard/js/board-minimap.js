// board-minimap.js -- the whole board in the corner of the editor, with the
// part on screen marked (tcl-9tax7h). A press or a drag moves the main view.
//
// The minimap has a zoom of its own (tcl-h3e5p8): its window onto the board is a
// second viewport, 1× being the whole board. Past that it follows the main view
// when that leaves its window, and Shift+drag or the middle button moves it. The
// window stays still during a drag, or the map would slide under the pointer.
//
// It draws on a canvas, not in SVG: a board can hold 5000 elements, and the map
// must cost the editor next to nothing. The canvas redraws only when the board
// or the window changed, at most once a frame and less often when a draw is
// slow. A pan inside the map only moves the marker; one past the board's edge
// grows the map, so it redraws within that limit.

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { elementBounds } from './board-model.js';
import { dashPrefs } from './prefs.js';

const html = htm.bind(h);

// Both apply to every board: the operator's taste, not the board's.
export const BOARD_MINIMAP_COLLAPSED_PREF = 'tclaude.dash.board.minimap.collapsed';
export const BOARD_MINIMAP_ZOOM_PREF = 'tclaude.dash.board.minimap.zoom';
export const BOARD_MINIMAP_ZOOM_MAX = 32;
const ZOOM_STEP = 1.5;
const MARGIN = 0.06; // of the board's size, around it
const MIN_MARKER_PX = 4;
const MIN_SHAPE_PX = 2; // anything smaller is a dot
const MIN_TEXT_PX = 5; // smaller text is a bar
const MAX_LINE_POINTS = 32;
const DRAW_BUDGET = 4; // a draw of n ms waits n * this before the next

const clamp = (value, lo, hi) => Math.min(hi, Math.max(lo, value));

export function clampMinimapZoom(zoom) {
  return Number.isFinite(zoom) ? clamp(zoom, 1, BOARD_MINIMAP_ZOOM_MAX) : 1;
}

// Elements are replaced, never changed, so a box holds as long as its element.
// Without this a board of long strokes walks every point on each frame.
const boxes = new WeakMap();
function boxOf(el) {
  let box = boxes.get(el);
  if (!box) {
    box = elementBounds(el);
    boxes.set(el, box);
  }
  return box;
}

function union(a, b) {
  if (!a) return b;
  if (!b) return a;
  const x = Math.min(a.x, b.x);
  const y = Math.min(a.y, b.y);
  return { x, y, width: Math.max(a.x + a.width, b.x + b.width) - x, height: Math.max(a.y + a.height, b.y + b.height) - y };
}

// minimapContent is the box around the live elements of Map(id -> element).
export function minimapContent(elements) {
  let minX = Infinity; let minY = Infinity; let maxX = -Infinity; let maxY = -Infinity;
  for (const el of elements.values()) {
    if (el.isDeleted) continue;
    const b = boxOf(el);
    minX = Math.min(minX, b.x); minY = Math.min(minY, b.y);
    maxX = Math.max(maxX, b.x + b.width); maxY = Math.max(maxY, b.y + b.height);
  }
  return Number.isFinite(minX) ? { x: minX, y: minY, width: maxX - minX, height: maxY - minY } : null;
}

// minimapWorld is what 1× shows: the content and the main view, with a margin.
export function minimapWorld(content, viewBox) {
  const box = union(content, viewBox);
  const mx = Math.max(box.width, 1) * MARGIN;
  const my = Math.max(box.height, 1) * MARGIN;
  return { x: box.x - mx, y: box.y - my, width: Math.max(box.width, 1) + mx * 2, height: Math.max(box.height, 1) + my * 2 };
}

// minimapWindow is the part of the world a frame of {width, height} shows at
// `zoom` around `center`: a box in board units and its scale. The window stays
// inside the world, and at 1× it holds all of it, centred.
export function minimapWindow(world, frame, zoom = 1, center = null) {
  const scale = Math.min(frame.width / world.width, frame.height / world.height) * zoom;
  const width = frame.width / scale;
  const height = frame.height / scale;
  const axis = (at, size, start, span) => (size >= span ? start + span / 2 : clamp(at, start + size / 2, start + span - size / 2));
  const cx = axis(center?.x ?? 0, width, world.x, world.width);
  const cy = axis(center?.y ?? 0, height, world.y, world.height);
  return { x: cx - width / 2, y: cy - height / 2, width, height, scale };
}

// followView moves a window's centre by no more than it takes to keep the
// main view's centre in the window.
export function followView(center, window, viewBox) {
  const past = (at, start, size) => (at < start ? at - start : at > start + size ? at - start - size : 0);
  return {
    x: center.x + past(viewBox.x + viewBox.width / 2, window.x, window.width),
    y: center.y + past(viewBox.y + viewBox.height / 2, window.y, window.height),
  };
}

// minimapZoomAt changes the zoom by `factor`, keeping the board point under the
// frame point (px, py) where it is. It returns the zoom and the centre.
export function minimapZoomAt(world, frame, zoom, center, px, py, factor) {
  const before = minimapWindow(world, frame, zoom, center);
  const next = clampMinimapZoom(zoom * factor);
  const scale = before.scale * (next / zoom);
  return {
    zoom: next,
    center: {
      x: before.x + px / before.scale + (frame.width / 2 - px) / scale,
      y: before.y + py / before.scale + (frame.height / 2 - py) / scale,
    },
  };
}

function drawElement(ctx, el, box, scale) {
  // A canvas ignores a colour it cannot parse, so the default goes first and an
  // agent's value needs no vetting here.
  ctx.strokeStyle = '#1e1e1e';
  ctx.strokeStyle = el.strokeColor || '#1e1e1e';
  if (el.type === 'text') {
    ctx.fillStyle = ctx.strokeStyle;
    const size = el.fontSize || 20;
    if (size * scale < MIN_TEXT_PX) {
      ctx.globalAlpha = 0.45;
      ctx.fillRect(box.x, box.y, box.width, box.height);
      ctx.globalAlpha = 1;
      return;
    }
    ctx.font = `${size}px sans-serif`;
    String(el.text || '').split('\n').forEach((line, i) => ctx.fillText(line, box.x, box.y + size * (i + 1) * 1.1));
    return;
  }
  if (el.points?.length) {
    const step = Math.ceil(el.points.length / MAX_LINE_POINTS);
    ctx.beginPath();
    for (let i = 0; i < el.points.length; i += step) {
      const [px, py] = el.points[i];
      if (i) ctx.lineTo(el.x + px, el.y + py);
      else ctx.moveTo(el.x + px, el.y + py);
    }
    const [lx, ly] = el.points[el.points.length - 1];
    ctx.lineTo(el.x + lx, el.y + ly);
    ctx.stroke();
    return;
  }
  if (box.width * scale < MIN_SHAPE_PX || box.height * scale < MIN_SHAPE_PX) {
    ctx.fillStyle = ctx.strokeStyle;
    ctx.fillRect(box.x, box.y, Math.max(box.width, MIN_SHAPE_PX / scale), Math.max(box.height, MIN_SHAPE_PX / scale));
    return;
  }
  ctx.beginPath();
  if (el.type === 'ellipse') {
    ctx.ellipse(box.x + box.width / 2, box.y + box.height / 2, box.width / 2, box.height / 2, 0, 0, Math.PI * 2);
  } else if (el.type === 'diamond') {
    ctx.moveTo(box.x + box.width / 2, box.y);
    ctx.lineTo(box.x + box.width, box.y + box.height / 2);
    ctx.lineTo(box.x + box.width / 2, box.y + box.height);
    ctx.lineTo(box.x, box.y + box.height / 2);
    ctx.closePath();
  } else {
    ctx.rect(box.x, box.y, box.width, box.height);
  }
  ctx.fillStyle = 'transparent';
  ctx.fillStyle = el.backgroundColor || 'transparent';
  ctx.fill();
  ctx.stroke();
}

function drawBoard(canvas, elements, window, frame, ratio) {
  const ctx = canvas.getContext?.('2d');
  if (!ctx) return;
  const width = Math.round(frame.width * ratio);
  const height = Math.round(frame.height * ratio);
  if (canvas.width !== width) canvas.width = width;
  if (canvas.height !== height) canvas.height = height;
  ctx.setTransform(1, 0, 0, 1, 0, 0);
  ctx.clearRect(0, 0, width, height);
  const scale = window.scale * ratio;
  ctx.setTransform(scale, 0, 0, scale, -window.x * scale, -window.y * scale);
  ctx.lineWidth = 1.25 / window.scale;
  ctx.textBaseline = 'alphabetic';
  for (const el of elements.values()) {
    if (el.isDeleted) continue;
    const box = boxOf(el);
    if (box.x > window.x + window.width || box.y > window.y + window.height || box.x + box.width < window.x || box.y + box.height < window.y) continue;
    drawElement(ctx, el, box, window.scale);
  }
}

function sizeOf(node) {
  const rect = node?.getBoundingClientRect?.();
  return rect?.width && rect?.height ? rect : null;
}

// BoardMinimap maps `elements`, which change in place: `tick` says they did.
// `view` is the main view, and svgRef the canvas it shows in. onNavigate centres
// the main view on a board point.
export function BoardMinimap({ elements, tick, view, svgRef, onScreen = true, win, onNavigate, prefs = dashPrefs }) {
  const [collapsed, setCollapsed] = useState(() => prefs.getItem(BOARD_MINIMAP_COLLAPSED_PREF) === '1');
  const [zoom, setZoom] = useState(() => clampMinimapZoom(Number(prefs.getItem(BOARD_MINIMAP_ZOOM_PREF) || 1)));
  const frameRef = useRef(null);
  const canvasRef = useRef(null);
  const markerRef = useRef(null);
  const center = useRef(null); // the window's centre, past 1×
  const gesture = useRef(null);
  const placed = useRef(null); // what the last place() worked out
  const content = useRef({ tick: -1, box: null });
  const drawn = useRef('');
  const pendingDraw = useRef(0);
  const nextDraw = useRef(0);
  const viewKey = useRef('');
  const latest = useRef(null);

  function schedule() {
    if (pendingDraw.current) return;
    const run = () => {
      pendingDraw.current = 0;
      const canvas = canvasRef.current;
      const now = performance.now();
      if (!canvas) return;
      if (now < nextDraw.current) { schedule(); return; }
      drawBoard(canvas, elements, placed.current.window, placed.current.frame, win?.devicePixelRatio || 1);
      const after = performance.now();
      nextDraw.current = after + (after - now) * DRAW_BUDGET;
    };
    pendingDraw.current = win?.requestAnimationFrame ? win.requestAnimationFrame(run) : setTimeout(run, 16);
  }

  // place puts the marker where the view is, and asks for a draw if the map on
  // the canvas is out of date. It runs for every pan, so it stays cheap.
  function place() {
    const frame = sizeOf(frameRef.current);
    const main = sizeOf(svgRef.current);
    if (!frame || !main || !onScreen) return;
    const viewBox = { x: view.x, y: view.y, width: main.width / view.zoom, height: main.height / view.zoom };
    if (content.current.tick !== tick) content.current = { tick, box: minimapContent(elements) };
    const world = minimapWorld(content.current.box, viewBox);
    let window = gesture.current?.window;
    if (!window) {
      if (zoom === 1) center.current = null;
      else if (!center.current) center.current = { x: viewBox.x + viewBox.width / 2, y: viewBox.y + viewBox.height / 2 };
      window = minimapWindow(world, frame, zoom, center.current);
      const key = `${view.x},${view.y},${view.zoom}`;
      if (zoom > 1 && key !== viewKey.current && !gesture.current) {
        center.current = followView({ x: window.x + window.width / 2, y: window.y + window.height / 2 }, window, viewBox);
        window = minimapWindow(world, frame, zoom, center.current);
      }
      viewKey.current = key;
    }
    placed.current = { world, frame, window, viewBox };
    const marker = markerRef.current;
    if (marker) {
      const width = Math.max(viewBox.width * window.scale, MIN_MARKER_PX);
      const height = Math.max(viewBox.height * window.scale, MIN_MARKER_PX);
      marker.style.left = `${(viewBox.x - window.x) * window.scale}px`;
      marker.style.top = `${(viewBox.y - window.y) * window.scale}px`;
      marker.style.width = `${width}px`;
      marker.style.height = `${height}px`;
    }
    // The ratio changes when the page moves to another screen.
    const key = `${tick},${window.x},${window.y},${window.scale},${frame.width},${frame.height},${win?.devicePixelRatio}`;
    if (key !== drawn.current) {
      drawn.current = key;
      schedule();
    }
  }
  latest.current = place;

  useLayoutEffect(() => { place(); }, [tick, view, zoom, collapsed, onScreen]);

  // The frame or the main canvas changing size moves everything, and a hidden
  // editor comes back this way too.
  useEffect(() => {
    if (collapsed || !win?.ResizeObserver) return undefined;
    const observer = new win.ResizeObserver(() => latest.current());
    if (frameRef.current) observer.observe(frameRef.current);
    if (svgRef.current) observer.observe(svgRef.current);
    return () => observer.disconnect();
  }, [collapsed, win]);

  // A tab stays mounted while another is showing, and that one may have changed
  // the map's settings.
  useEffect(() => {
    if (!onScreen) return;
    drawn.current = '';
    setCollapsed(prefs.getItem(BOARD_MINIMAP_COLLAPSED_PREF) === '1');
    setZoom(clampMinimapZoom(Number(prefs.getItem(BOARD_MINIMAP_ZOOM_PREF) || 1)));
  }, [onScreen]);

  useEffect(() => () => {
    if (win?.cancelAnimationFrame) win.cancelAnimationFrame(pendingDraw.current);
    else clearTimeout(pendingDraw.current);
  }, [win]);

  function changeZoom(next, nextCenter) {
    center.current = next === 1 ? null : nextCenter;
    setZoom(next);
    prefs.setItem(BOARD_MINIMAP_ZOOM_PREF, String(Math.round(next * 100) / 100));
  }

  function step(factor) {
    const now = placed.current;
    if (!now) return;
    const { world, frame, window } = now;
    const middle = { x: window.x + window.width / 2, y: window.y + window.height / 2 };
    const at = minimapZoomAt(world, frame, zoom, middle, frame.width / 2, frame.height / 2, factor);
    // Leaving 1× goes in on the main view; the whole board has no centre to keep.
    changeZoom(at.zoom, zoom === 1 ? null : at.center);
  }

  function setHidden(hidden) {
    setCollapsed(hidden);
    drawn.current = '';
    prefs.setItem(BOARD_MINIMAP_COLLAPSED_PREF, hidden ? '1' : '0');
  }

  function framePoint(event) {
    const rect = frameRef.current.getBoundingClientRect();
    return { px: clamp(event.clientX - rect.left, 0, rect.width), py: clamp(event.clientY - rect.top, 0, rect.height) };
  }

  function onPointerDown(event) {
    const now = placed.current;
    if (!now || (event.button !== 0 && event.button !== 1)) return;
    frameRef.current.setPointerCapture?.(event.pointerId);
    const { window, viewBox } = now;
    if (event.button === 1 || event.shiftKey) {
      gesture.current = { kind: 'window', sx: event.clientX, sy: event.clientY, x: window.x + window.width / 2, y: window.y + window.height / 2 };
      return;
    }
    const { px, py } = framePoint(event);
    const x = window.x + px / window.scale;
    const y = window.y + py / window.scale;
    // A press on the marker takes hold of it; anywhere else brings it there.
    const held = x >= viewBox.x && x <= viewBox.x + viewBox.width && y >= viewBox.y && y <= viewBox.y + viewBox.height;
    const dx = held ? viewBox.x + viewBox.width / 2 - x : 0;
    const dy = held ? viewBox.y + viewBox.height / 2 - y : 0;
    gesture.current = { kind: 'view', window, dx, dy };
    if (!held) onNavigate(x, y);
  }

  function onPointerMove(event) {
    const g = gesture.current;
    if (!g) return;
    if (g.kind === 'view') {
      const { px, py } = framePoint(event);
      onNavigate(g.window.x + px / g.window.scale + g.dx, g.window.y + py / g.window.scale + g.dy);
      return;
    }
    const { scale } = placed.current.window;
    center.current = { x: g.x - (event.clientX - g.sx) / scale, y: g.y - (event.clientY - g.sy) / scale };
    place();
  }

  function onPointerUp() {
    gesture.current = null;
    place();
  }

  function onWheel(event) {
    event.preventDefault();
    const now = placed.current;
    if (!now || gesture.current) return;
    const { px, py } = framePoint(event);
    const { window, world, frame } = now;
    const middle = { x: window.x + window.width / 2, y: window.y + window.height / 2 };
    const at = minimapZoomAt(world, frame, zoom, middle, px, py, Math.exp(-event.deltaY * 0.002));
    if (at.zoom !== zoom) changeZoom(at.zoom, at.center);
  }

  if (collapsed) {
    return html`<button type="button" class="board-minimap-open" title="Show the minimap" aria-label="Show the minimap" onClick=${() => setHidden(false)}>▦</button>`;
  }
  return html`<div class="board-minimap" role="group" aria-label="Minimap">
    <div ref=${frameRef} class="board-minimap-frame" title="Click or drag to move the view. Scroll to zoom the map, Shift+drag to move it."
      onPointerDown=${onPointerDown} onPointerMove=${onPointerMove} onPointerUp=${onPointerUp} onPointerCancel=${onPointerUp} onWheel=${onWheel}>
      <canvas ref=${canvasRef} aria-hidden="true"></canvas>
      <div ref=${markerRef} class="board-minimap-view"></div>
    </div>
    <div class="board-minimap-bar">
      <button type="button" title="Zoom the map out" aria-label="Zoom the map out" disabled=${zoom <= 1} onClick=${() => step(1 / ZOOM_STEP)}>−</button>
      <button type="button" class="board-minimap-zoom" title="Show the whole board" aria-label="Show the whole board" disabled=${zoom <= 1} onClick=${() => changeZoom(1, null)}>${zoom <= 1 ? 'Fit' : `${Math.round(zoom * 10) / 10}×`}</button>
      <button type="button" title="Zoom the map in" aria-label="Zoom the map in" disabled=${zoom >= BOARD_MINIMAP_ZOOM_MAX} onClick=${() => step(ZOOM_STEP)}>+</button>
      <button type="button" class="board-minimap-hide" title="Hide the minimap" aria-label="Hide the minimap" onClick=${() => setHidden(true)}>▾</button>
    </div>
  </div>`;
}
