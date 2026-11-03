// page-probe-model.js -- the parent's half of the live-page probe protocol
// (tcl-2dwbad; the probe is stage_probe.js, which documents the messages).
// DOM-free, so the checks can be tested directly.
//
// The probe runs inside the page and is as untrusted as the page: whatever it
// sends is page content. acceptProbeMessage admits a message only from the
// frame's own window, carrying the nonce of the frame's current load, within
// the size caps and the schema, and returns a fresh copy holding only the
// known fields as plain strings and numbers. Callers show those as text, never
// as HTML.

export const PROBE_LIMITS = Object.freeze({
  messageBytes: 64 * 1024,
  elements: 50,
  text: 200,
  path: 300,
  items: 10,
  value: 16 * 1024,
  points: 400,
  selector: 500,
  pagePath: 512,
});

const NONCE = /^[0-9a-f]{32}$/;
const REQUEST_ID = /^[0-9a-z]{1,32}$/;
export const PROBE_DOM_MODES = Object.freeze(['text', 'html', 'styles']);

// newProbeNonce mints a load's nonce: 128 random bits, hex.
export function newProbeNonce(cryptoImpl = globalThis.crypto) {
  const bytes = new Uint8Array(16);
  cryptoImpl.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

let requestCounter = 0;
export function newProbeRequestId() {
  requestCounter = (requestCounter + 1) % 1e9;
  return `r${requestCounter.toString(36)}${Math.floor(Math.random() * 1e6).toString(36)}`;
}

const parentMessage = (nonce, kind, rest = {}) => ({ type: 'tclaude-parent', v: 1, nonce, kind, ...rest });

export const probeHello = (nonce) => parentMessage(nonce, 'hello');

export function probeElementsAt(nonce, id, points) {
  const pts = (Array.isArray(points) ? points : []).slice(0, PROBE_LIMITS.points)
    .map(([x, y]) => [Math.round(Number(x) || 0), Math.round(Number(y) || 0)]);
  return parentMessage(nonce, 'elementsAt', { id, points: pts });
}

export function probeDom(nonce, id, selector, mode) {
  return parentMessage(nonce, 'dom', {
    id, selector: String(selector || '').slice(0, PROBE_LIMITS.selector), mode: PROBE_DOM_MODES.includes(mode) ? mode : 'text',
  });
}

const isPlain = (v) => v !== null && typeof v === 'object' && !Array.isArray(v);
const str = (v, max) => (typeof v === 'string' && v.length <= max ? v : null);
const int = (v, min = -1e7, max = 1e7) => (typeof v === 'number' && Number.isFinite(v) && v >= min && v <= max ? Math.round(v) : null);

// messageSize is the message's JSON length, or Infinity when it has none (a
// cycle, a function, a BigInt): such a message is refused.
function messageSize(data) {
  try {
    const json = JSON.stringify(data);
    return typeof json === 'string' ? json.length : Infinity;
  } catch {
    return Infinity;
  }
}

function checkScroll(d) {
  const out = { kind: 'scroll' };
  for (const key of ['x', 'y', 'vw', 'vh', 'docW', 'docH']) {
    const v = int(d[key], key === 'x' || key === 'y' ? -1e7 : 0);
    if (v === null) return null;
    out[key] = v;
  }
  const path = d.path === undefined ? '' : str(d.path, PROBE_LIMITS.pagePath);
  if (path === null) return null;
  out.path = path;
  return out;
}

function checkElements(list) {
  if (!Array.isArray(list) || list.length > PROBE_LIMITS.elements) return null;
  const out = [];
  for (const e of list) {
    if (!isPlain(e)) return null;
    const path = str(e.path, PROBE_LIMITS.path);
    const text = str(e.text, PROBE_LIMITS.text);
    const hits = int(e.hits, 0, PROBE_LIMITS.points);
    if (path === null || text === null || hits === null) return null;
    out.push({ path, text, hits });
  }
  return out;
}

function checkItems(list) {
  if (!Array.isArray(list) || list.length > PROBE_LIMITS.items) return null;
  const out = [];
  for (const e of list) {
    if (!isPlain(e) || !isPlain(e.rect)) return null;
    const path = str(e.path, PROBE_LIMITS.path);
    const value = str(e.value, PROBE_LIMITS.value);
    const rect = {};
    for (const key of ['x', 'y', 'w', 'h']) {
      rect[key] = int(e.rect[key]);
      if (rect[key] === null) return null;
    }
    if (path === null || value === null) return null;
    out.push({ path, rect, value });
  }
  return out;
}

function checkResult(d) {
  const id = typeof d.id === 'string' && REQUEST_ID.test(d.id) ? d.id : null;
  if (!id) return null;
  if (d.elements !== undefined) {
    const elements = checkElements(d.elements);
    return elements ? { kind: 'result', id, elements } : null;
  }
  const count = int(d.count, 0, 1e7);
  const items = checkItems(d.items);
  if (count === null || !items) return null;
  const out = { kind: 'result', id, count, items };
  if (d.error !== undefined) {
    const error = str(d.error, PROBE_LIMITS.text);
    if (error === null) return null;
    out.error = error;
  }
  return out;
}

// acceptProbeMessage checks one message event against the frame's window and
// its current load's nonce. It returns the message's checked copy, or null.
// A 'ready' carries no nonce: it only asks for a hello.
export function acceptProbeMessage(event, frameWindow, nonce) {
  if (!event || !frameWindow || event.source !== frameWindow) return null;
  const d = event.data;
  if (!isPlain(d) || d.type !== 'tclaude-probe' || d.v !== 1) return null;
  if (messageSize(d) > PROBE_LIMITS.messageBytes) return null;
  if (d.kind === 'ready') return { kind: 'ready' };
  if (typeof nonce !== 'string' || !NONCE.test(nonce) || d.nonce !== nonce) return null;
  switch (d.kind) {
    case 'hello': return { kind: 'hello' };
    case 'scroll': return checkScroll(d);
    case 'result': return checkResult(d);
    default: return null;
  }
}

// samplePoints picks the points of a mark the probe looks under, in the
// frame's viewport: a box's corners, centre and a grid inside it; a path's
// points, thinned to a step. At most `max` points.
export function samplePoints(mark, scroll = { x: 0, y: 0 }, max = 64) {
  const ox = scroll?.x || 0;
  const oy = scroll?.y || 0;
  const pts = [];
  if (mark.kind === 'box') {
    const [[x0, y0], [x1, y1]] = mark.points;
    const left = Math.min(x0, x1); const top = Math.min(y0, y1);
    const w = Math.abs(x1 - x0); const h = Math.abs(y1 - y0);
    const n = 5;
    for (let i = 0; i < n; i++) {
      for (let j = 0; j < n; j++) pts.push([left + (w * (i + 0.5)) / n, top + (h * (j + 0.5)) / n]);
    }
    pts.push([left + 2, top + 2], [left + w - 2, top + 2], [left + 2, top + h - 2], [left + w - 2, top + h - 2]);
  } else {
    const src = mark.points || [];
    const step = Math.max(1, Math.ceil(src.length / max));
    for (let i = 0; i < src.length; i += step) pts.push(src[i]);
    if (src.length && pts[pts.length - 1] !== src[src.length - 1]) pts.push(src[src.length - 1]);
  }
  return pts.slice(0, max).map(([x, y]) => [x - ox, y - oy]);
}
