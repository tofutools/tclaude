// dashboard-capture.js -- lets a permitted agent see what the operator sees
// (tcl-7i8b1t, `tclaude agent guide capture`). The page renders its own DOM to
// a PNG: getDisplayMedia would prompt every time and could show more than the
// dashboard. Terminals are in the DOM as text, so they come out real.
//
// Rendering rebuilds the visible document from its snapshot (capture-snapshot.js)
// inside an SVG foreignObject with the page's own stylesheets and draws that to
// a canvas, so a capture can only hold this page. The snapshot holds the live
// state the DOM does not carry: field values, scroll offsets, canvases and
// images. Hover styles do not survive, and elements marked data-capture-skip
// (the capture's own cue) are left out. Password fields and elements marked
// data-capture-mask, which is any that holds a token, key or secret whatever its
// type, are masked: neither the image, the text nor the context carries what
// they hold.
// dashboard-imperative-boundary: browser-io

import { MASKED, snapshotPage, snapshotText } from './capture-snapshot.js';
import { randomUUID } from './insecure-origin.js';

export const CAPTURE_PRESENCE_MS = 1500;
export const CAPTURE_HIDDEN_PRESENCE_MS = 10000;
// A peer's page cannot see whether this node has Collab on, so it asks, and
// keeps asking rarely in case it is switched on.
export const CAPTURE_ABSENT_PRESENCE_MS = 60000;
export const CAPTURE_NOTICE_MS = 4000;
const CAPTURE_MAX_SIDE = 2560;
const CAPTURE_RENDER_TIMEOUT_MS = 12000;
// A hung exchange must not end presence: the daemon reads a silent tab as gone.
const CAPTURE_PING_TIMEOUT_MS = 8000;
const CAPTURE_ANSWER_TIMEOUT_MS = CAPTURE_RENDER_TIMEOUT_MS + 8000;
const CAPTURE_TEXT_MAX = 240;
const XHTML = 'http://www.w3.org/1999/xhtml';
const SVG = 'http://www.w3.org/2000/svg';

const clip = (value, max = CAPTURE_TEXT_MAX) => String(value || '').replace(/\s+/g, ' ').trim().slice(0, max);

// -- context ----------------------------------------------------------------

// describeElement names an element for an agent. A field's value is never
// included: the agent asks what has focus, not what is typed in it.
function holdsMasked(el, control) {
  return !!(el.closest?.(MASKED) || control.querySelector?.(MASKED));
}

// selectionIsMasked covers a secret selected in place, which a field that
// selects itself on focus does as soon as it is clicked.
function selectionIsMasked(win) {
  const doc = win.document;
  if (doc.activeElement?.closest?.(MASKED)) return true;
  const selection = win.getSelection?.();
  return [...(doc.querySelectorAll?.(MASKED) || [])].some((el) => selection?.containsNode?.(el, true));
}

export function describeElement(el, y = null) {
  if (!el?.tagName) return null;
  const control = el.closest?.('button, a[href], [role="button"], [role="tab"], [role="menuitem"], [role="link"], label, summary, th, input, select, textarea') || el;
  const field = /^(input|select|textarea)$/i.test(control.tagName || '');
  const masked = holdsMasked(el, control);
  const out = {
    tag: String(control.tagName || '').toLowerCase(),
    role: control.getAttribute?.('role') || '',
    // A masked element's title can repeat its value; aria-label only names it.
    label: clip(control.getAttribute?.('aria-label') || (masked ? '' : control.getAttribute?.('title')) || control.getAttribute?.('placeholder')),
    text: field || masked ? '' : clip(terminalRow(el, y)?.textContent ?? control.innerText ?? control.textContent),
    id: control.id || '',
    ref: control.id ? `#${control.id}` : '',
    board_element: el.closest?.('[data-element]')?.getAttribute?.('data-element') || '',
  };
  return out;
}

// terminalRow is the terminal line an element sits on, which is what the
// operator is pointing at, rather than the whole screen of text.
// xterm's layers cover its rows, so the row is found by height.
export function terminalRow(el, y) {
  if (y === null) return null;
  const rows = el.closest?.('.xterm')?.querySelector?.('.xterm-rows');
  if (!rows) return null;
  return [...rows.children].find((row) => {
    const rect = row.getBoundingClientRect();
    return y >= rect.top && y < rect.bottom;
  }) || null;
}

// boardContext reads the open board and the region of it on screen from the
// live drawing, so it is as fresh as the image.
export function boardContext(doc) {
  const viewport = doc.querySelector?.('[data-board-viewport]');
  const svg = viewport?.closest?.('[data-board-id]');
  if (!svg) return null;
  const board = { id: svg.getAttribute('data-board-id') };
  const ctm = viewport.getScreenCTM?.();
  const rect = svg.getBoundingClientRect?.();
  if (ctm?.a && rect?.width) {
    board.zoom = Math.round(ctm.a * 1000) / 1000;
    board.region = {
      x: Math.round((rect.left - ctm.e) / ctm.a), y: Math.round((rect.top - ctm.f) / ctm.d),
      width: Math.round(rect.width / ctm.a), height: Math.round(rect.height / ctm.d),
    };
  }
  return board;
}

export function captureContext({ win, tab, pointer, marks = [], now = Date.now() }) {
  const doc = win.document;
  const context = {
    tab: String(tab || ''),
    path: `${win.location?.pathname || '/'}${win.location?.search || ''}`,
    title: clip(doc.title, 160),
    focused: !!doc.hasFocus?.(),
    viewport: { width: win.innerWidth, height: win.innerHeight, device_pixel_ratio: win.devicePixelRatio || 1 },
  };
  if (pointer) {
    context.cursor = { x: Math.round(pointer.x), y: Math.round(pointer.y), inside: !!pointer.inside, age_ms: Math.max(0, now - pointer.at) };
    if (pointer.inside) {
      const under = describeElement(doc.elementFromPoint?.(pointer.x, pointer.y), pointer.y);
      if (under) context.cursor.under = under;
    }
  }
  const active = doc.activeElement;
  if (active && active !== doc.body && active !== doc.documentElement) context.focus = describeElement(active);
  const selection = clip(win.getSelection?.()?.toString?.(), 400);
  if (selection && !selectionIsMasked(win)) context.selection = selection;
  const board = boardContext(doc);
  if (board) context.board = board;
  if (marks.length) context.marks = marks;
  return context;
}

// -- presence and the capture cycle -------------------------------------------

// trackPointer keeps where the mouse last was: the page cannot ask for it.
export function trackPointer(win, now = Date.now) {
  const doc = win.document;
  const state = { pointer: null };
  const onMove = (event) => { state.pointer = { x: event.clientX, y: event.clientY, inside: true, at: now() }; };
  const onLeave = () => { if (state.pointer) state.pointer = { ...state.pointer, inside: false, at: now() }; };
  const options = { passive: true, capture: true };
  win.addEventListener('pointermove', onMove, options);
  doc.documentElement.addEventListener('pointerleave', onLeave, { passive: true });
  win.addEventListener('blur', onLeave);
  state.stop = () => {
    win.removeEventListener('pointermove', onMove, options);
    doc.documentElement.removeEventListener('pointerleave', onLeave);
    win.removeEventListener('blur', onLeave);
  };
  return state;
}

// startCapturePresence tells the daemon this tab is open and whether it is on
// screen, and answers a capture aimed at it. It returns a stop function.
export function startCapturePresence({
  win, fetchImpl, tab, onCapture, onMarks, marks = () => [], render = renderPage, snapshot = snapshotPage, randomId = () => randomUUID(win.crypto),
  setTimeoutImpl = (fn, ms) => win.setTimeout(fn, ms), clearTimeoutImpl = (id) => win.clearTimeout(id),
}) {
  const doc = win.document;
  const client = randomId();
  const tracker = trackPointer(win);
  let timer = null;
  let stopped = false;
  let inFlight = false;
  let absent = false;

  const hidden = () => doc.visibilityState === 'hidden';
  const within = (work, ms) => new Promise((resolve, reject) => {
    const limit = setTimeoutImpl(() => reject(new Error('timed out')), ms);
    Promise.resolve(work).then(resolve, reject).finally(() => clearTimeoutImpl(limit));
  });
  const send = async (body, signal) => {
    const response = await fetchImpl('/api/companion/presence', {
      method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), signal,
    });
    absent = response?.status === 404;
    return response?.ok ? response.json() : null;
  };
  const answer = async (request) => {
    onCapture?.(request);
    let body;
    try {
      // Let the cue paint first, so the operator sees it before the page
      // stalls for the render.
      await new Promise((resolve) => setTimeoutImpl(resolve, 60));
      const context = captureContext({ win, tab: tab(), pointer: tracker.pointer, marks: marks() });
      const page = snapshot(win);
      const png = await render({ win, fetchImpl, pointer: tracker.pointer?.inside ? tracker.pointer : null, snapshot: page });
      body = { png: await blobToBase64(png, win), context };
      if (request.form) body.form_text = snapshotText(page);
    } catch (err) {
      body = { error: String(err?.message || err || 'render failed') };
    }
    try {
      await fetchImpl(`/api/companion/capture/${encodeURIComponent(request.id)}`, {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
      });
    } catch { /* the daemon times the request out */ }
  };
  const schedule = () => {
    if (stopped) return;
    clearTimeoutImpl(timer);
    const wait = absent ? CAPTURE_ABSENT_PRESENCE_MS : hidden() ? CAPTURE_HIDDEN_PRESENCE_MS : CAPTURE_PRESENCE_MS;
    timer = setTimeoutImpl(ping, wait);
  };
  async function ping() {
    if (stopped || inFlight) return;
    inFlight = true;
    const abort = win.AbortController ? new win.AbortController() : null;
    try {
      const body = { client, focused: !!doc.hasFocus?.(), visible: !hidden() };
      const data = await within(send(body, abort?.signal), CAPTURE_PING_TIMEOUT_MS);
      if (Array.isArray(data?.marks)) onMarks?.(data.marks);
      if (data?.capture?.id) await within(answer(data.capture), CAPTURE_ANSWER_TIMEOUT_MS);
    } catch {
      abort?.abort();
    } finally {
      inFlight = false;
      schedule();
    }
  }
  const pingNow = () => { void ping(); };
  const goodbye = () => {
    stopped = true;
    clearTimeoutImpl(timer);
    try {
      win.navigator.sendBeacon('/api/companion/presence', new win.Blob([JSON.stringify({ client, gone: true })], { type: 'application/json' }));
    } catch { /* the daemon ages the tab out */ }
  };
  const revive = () => {
    if (!stopped) return;
    stopped = false;
    pingNow();
  };
  doc.addEventListener('visibilitychange', pingNow);
  win.addEventListener('focus', pingNow);
  win.addEventListener('blur', pingNow);
  win.addEventListener('pagehide', goodbye);
  win.addEventListener('pageshow', revive);
  pingNow();
  return () => {
    stopped = true;
    clearTimeoutImpl(timer);
    tracker.stop();
    doc.removeEventListener('visibilitychange', pingNow);
    win.removeEventListener('focus', pingNow);
    win.removeEventListener('blur', pingNow);
    win.removeEventListener('pagehide', goodbye);
    win.removeEventListener('pageshow', revive);
  };
}

export async function blobToBase64(blob, win = globalThis) {
  const bytes = new Uint8Array(await blob.arrayBuffer());
  let binary = '';
  for (let i = 0; i < bytes.length; i += 0x8000) binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return win.btoa(binary);
}

// -- rendering ------------------------------------------------------------------

// sheetUrls lists the url(...) references in CSS that still need inlining,
// left as written. Fragments and data: URLs work where they are.
export function cssUrls(css) {
  const out = new Set();
  for (const match of String(css).matchAll(/url\(\s*(['"]?)([^'")]+)\1\s*\)/g)) {
    if (!/^(data:|#)/.test(match[2])) out.add(match[2]);
  }
  return [...out];
}

// rewriteCss makes the page's rules apply inside a foreignObject, where the
// root element is the <svg>: anything on :root moves to the cloned <html>.
export function rewriteCss(css) {
  return String(css).replace(/:root\b/g, 'html');
}

// sameOrigin answers whether a URL may be read at all. Anything else cannot be
// inlined and would taint or fail the render, so it is dropped.
function sameOrigin(url, win) {
  try {
    const parsed = new win.URL(url, win.document.baseURI);
    return parsed.origin === win.location.origin || parsed.protocol === 'blob:';
  } catch {
    return false;
  }
}

function inliner({ win, fetchImpl }) {
  const cache = new Map();
  const read = async (url) => {
    const response = await fetchImpl(url, { credentials: 'same-origin' });
    if (!response?.ok) return '';
    const blob = await response.blob();
    return new Promise((resolve) => {
      const reader = new win.FileReader();
      reader.onload = () => resolve(String(reader.result || ''));
      reader.onerror = () => resolve('');
      reader.readAsDataURL(blob);
    });
  };
  return (url, base) => {
    let absolute;
    try { absolute = new win.URL(url, base || win.document.baseURI).href; } catch { return Promise.resolve(''); }
    if (!sameOrigin(absolute, win)) return Promise.resolve('');
    if (!cache.has(absolute)) cache.set(absolute, read(absolute).catch(() => ''));
    return cache.get(absolute);
  };
}

async function inlineCssUrls(css, base, inline) {
  let out = css;
  for (const url of cssUrls(css)) {
    const data = await inline(url, base);
    out = out.split(url).join(data || 'data:,');
  }
  return out;
}

// pageCss gathers the live rules, including those a library added through the
// CSSOM. Keyframes are dropped along with animations below, because a frozen
// image would otherwise show each animation's first frame, often fully faded.
async function pageCss(doc, inline) {
  const parts = ['*,*::before,*::after{animation:none!important;transition:none!important}'];
  for (const sheet of [...doc.styleSheets, ...(doc.adoptedStyleSheets || [])]) {
    let rules;
    try { rules = sheet.cssRules; } catch { continue; }
    let text = '';
    for (const rule of rules) {
      if (rule.type !== 7) text += `${rule.cssText}\n`; // 7: CSSRule.KEYFRAMES_RULE
    }
    parts.push(await inlineCssUrls(text, sheet.href || doc.baseURI, inline));
  }
  return rewriteCss(parts.join('\n'));
}

function applyField(clone, field) {
  if (field.kind === 'textarea') {
    clone.textContent = field.value;
  } else if (field.kind === 'checkbox' || field.kind === 'radio') {
    if (field.checked) clone.setAttribute('checked', 'checked');
    else clone.removeAttribute('checked');
  } else if (field.kind !== 'select' && field.kind !== 'file') {
    clone.setAttribute('value', field.value);
  }
}

function setAttrs(el, attrs) {
  for (const { name, value, ns } of attrs) {
    try {
      if (ns) el.setAttributeNS(ns, name, value);
      else el.setAttribute(name, value);
    } catch { /* a name only a parser accepts: nothing a picture needs */ }
  }
}

// buildNode makes the DOM of a filtered snapshot node. tasks collects the async
// work (image data) to finish before serialising.
export function buildNode(node, { doc, inline, tasks }, scroller = { x: 0, y: 0 }) {
  if (node.text !== undefined) return doc.createTextNode(node.text);
  if (node.gone) return null;
  if (node.surface) return surfaceElement(node.surface, doc);

  const clone = doc.createElementNS(node.ns || XHTML, node.name);
  setAttrs(clone, node.attrs);
  if (node.field) applyField(clone, node.field);
  if (node.src) {
    clone.removeAttribute('srcset');
    clone.removeAttribute('src');
    tasks.push(inline(node.src).then((data) => { if (data) clone.setAttribute('src', data); }));
  }
  if (node.tag === 'image') {
    const href = clone.getAttribute('href');
    if (href && !href.startsWith('data:')) {
      clone.removeAttribute('href');
      tasks.push(inline(href).then((data) => { if (data) clone.setAttribute('href', data); }));
    }
  }
  const inlineStyle = clone.getAttribute('style') || '';
  if (inlineStyle.includes('url(')) {
    tasks.push(inlineCssUrls(inlineStyle, doc.baseURI, inline).then((css) => clone.setAttribute('style', css)));
  }

  // A rebuilt element has no scroll offset. Moving the children keeps the
  // layout, which a wrapper or margin would not in a flex or grid box. A sticky
  // element stays where it is pinned instead of moving with them.
  const { x: scrollX, y: scrollY } = node.scroll;
  const scrolled = (scrollX || scrollY) && node.ns === XHTML;
  const inner = scrolled ? node.scroll : scroller;
  if (node.sticky && (scroller.x || scroller.y)) {
    clone.style?.setProperty('translate', `${scroller.x}px ${scroller.y}px`);
  }
  for (const child of node.children) {
    let copy = buildNode(child, { doc, inline, tasks }, inner);
    if (!copy) continue;
    if (scrolled) {
      if (copy.nodeType === 3) {
        const span = doc.createElementNS(XHTML, 'span');
        span.appendChild(copy);
        copy = span;
      }
      if (child.sticky) {
        // Its siblings move instead, so it stays put.
        copy.style?.removeProperty('translate');
      } else if (copy.style) {
        copy.style.setProperty('translate', `${-scrollX}px ${-scrollY}px`);
        if (copy.tagName.toLowerCase() === 'span') copy.style.setProperty('display', 'inline-block');
      }
    }
    clone.appendChild(copy);
  }
  if (scrolled && clone.style) clone.style.setProperty('overflow', 'hidden');
  return clone;
}

// surfaceElement draws what the snapshot read from a canvas or video, or a
// labelled box where it could read nothing.
function surfaceElement(surface, doc) {
  const size = `width:${surface.width}px;height:${surface.height}px;${surface.absolute ? `position:absolute;left:${surface.left};top:${surface.top};` : ''}`;
  if (surface.data) {
    const image = doc.createElementNS(XHTML, 'img');
    image.setAttribute('src', surface.data);
    image.setAttribute('style', size);
    image.setAttribute('class', surface.className);
    return image;
  }
  const box = doc.createElementNS(XHTML, 'div');
  box.setAttribute('style', `${size}display:flex;align-items:center;justify-content:center;background:#222;color:#aaa;font:12px sans-serif`);
  box.textContent = `(${surface.tag} not captured)`;
  return box;
}

// drawCursor marks where the pointer is, since no DOM render includes it: a
// ring to find it by and the arrow itself.
export function drawCursor(ctx, x, y, scale) {
  ctx.save();
  ctx.translate(x * scale, y * scale);
  ctx.scale(scale, scale);
  ctx.beginPath();
  ctx.arc(0, 0, 16, 0, Math.PI * 2);
  ctx.fillStyle = 'rgba(255, 64, 64, 0.22)';
  ctx.fill();
  ctx.strokeStyle = 'rgba(255, 64, 64, 0.9)';
  ctx.lineWidth = 2;
  ctx.stroke();
  ctx.beginPath();
  for (const [i, [px, py]] of [[0, 0], [0, 17], [4.5, 13], [7.5, 19.5], [10.5, 18], [7.5, 12], [12.5, 12]].entries()) {
    if (i) ctx.lineTo(px, py);
    else ctx.moveTo(px, py);
  }
  ctx.closePath();
  ctx.fillStyle = '#000';
  ctx.fill();
  ctx.strokeStyle = '#fff';
  ctx.lineWidth = 1.5;
  ctx.stroke();
  ctx.restore();
}

function loadImage(win, url) {
  return new Promise((resolve, reject) => {
    const image = new win.Image();
    image.onload = () => resolve(image);
    image.onerror = () => reject(new Error('this browser could not draw the page'));
    image.src = url;
  });
}

// XML 1.0 has no representation for most control characters, and terminal
// output can hold them.
const stripXmlInvalid = (markup) => markup.replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f￾￿]/g, '');

// renderPage resolves to a PNG Blob of the viewport. draw(ctx, scale) adds to
// it what the page's DOM does not hold, in CSS pixels times scale.
export function renderPage(options) {
  return new Promise((resolve, reject) => {
    const timer = options.win.setTimeout(() => reject(new Error('the render took too long')), CAPTURE_RENDER_TIMEOUT_MS);
    renderViewport(options).then(resolve, reject).finally(() => options.win.clearTimeout(timer));
  });
}

// The snapshot defaults here rather than below so that it is read before the
// first await, at the moment the render was asked for.
async function renderViewport({ win, fetchImpl, pointer, draw, snapshot = snapshotPage(win) }) {
  const doc = win.document;
  const width = win.innerWidth;
  const height = win.innerHeight;
  const inline = inliner({ win, fetchImpl });
  const css = await pageCss(doc, inline);
  const tasks = [];
  // Offsetting the body keeps fixed elements where they are, which a transform
  // would not, and sticky ones still stick to the image's edges by themselves.
  const page = snapshot.scroll;
  const body = buildNode(snapshot.root, { doc, inline, tasks });
  await Promise.all(tasks);
  if (page.x || page.y) {
    body.style.setProperty('position', 'relative');
    body.style.setProperty('left', `${-page.x}px`);
    body.style.setProperty('top', `${-page.y}px`);
  }

  const root = doc.createElementNS(XHTML, 'html');
  for (const { name, value } of doc.documentElement.attributes) root.setAttribute(name, value);
  root.setAttribute('style', `${doc.documentElement.getAttribute('style') || ''};width:${width}px;height:${height}px;overflow:hidden`);
  const styleEl = doc.createElementNS(XHTML, 'style');
  styleEl.textContent = css;
  root.append(styleEl, body);

  const svg = doc.createElementNS(SVG, 'svg');
  svg.setAttribute('width', String(width));
  svg.setAttribute('height', String(height));
  svg.setAttribute('viewBox', `0 0 ${width} ${height}`);
  const object = doc.createElementNS(SVG, 'foreignObject');
  object.setAttribute('width', '100%');
  object.setAttribute('height', '100%');
  object.appendChild(root);
  svg.appendChild(object);

  const markup = stripXmlInvalid(new win.XMLSerializer().serializeToString(svg));
  // A data: URL, because a blob: SVG holding foreignObject taints the canvas.
  const image = await loadImage(win, `data:image/svg+xml;charset=utf-8,${encodeURIComponent(markup)}`);

  const scale = Math.min(win.devicePixelRatio || 1, 2, CAPTURE_MAX_SIDE / Math.max(width, height));
  const canvas = doc.createElement('canvas');
  canvas.width = Math.round(width * scale);
  canvas.height = Math.round(height * scale);
  const ctx = canvas.getContext('2d');
  const background = win.getComputedStyle(doc.body).backgroundColor;
  ctx.fillStyle = !background || background === 'rgba(0, 0, 0, 0)' ? '#0d1117' : background;
  ctx.fillRect(0, 0, canvas.width, canvas.height);
  ctx.drawImage(image, 0, 0, canvas.width, canvas.height);
  if (pointer) drawCursor(ctx, pointer.x, pointer.y, scale);
  draw?.(ctx, scale);
  return new Promise((resolve, reject) => {
    try {
      canvas.toBlob((blob) => (blob ? resolve(blob) : reject(new Error('the page could not be encoded'))), 'image/png');
    } catch {
      reject(new Error('this browser refuses to read back a page render; use Chrome, Edge or Firefox'));
    }
  });
}
