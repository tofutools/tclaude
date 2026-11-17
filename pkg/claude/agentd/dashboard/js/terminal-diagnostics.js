// terminal-diagnostics.js -- the terminal's Diagnostics overlay (tk_f7e6804f).
//
// Measures, over a rolling window, what a terminal gets and draws: socket
// messages (rate, sizes, the gaps between arrivals, where radio batching
// shows as clumps), xterm's parse time (write → callback) and render cadence
// (onRender), the time from a write to the next render, long tasks and
// dropped animation frames, and, per scroll step sent (a phone swipe notch or
// a desktop wheel notch), the time until the next frame arrives and until it
// is drawn. A local terminal also reads the server's side of its socket
// (dashboard_term_diag.go): when each frame went out, how big it was, how long
// the write took, and how soon output followed input there. Copy report
// gives the lot as text, to compare a phone with a desktop.
//
// dashboard-imperative-boundary: platform-layout
// The readout is plain DOM inside the xterm host (terminals-core.js owns it
// and stops it with the widget), as the xterm core's own nodes are; see
// CONTRIBUTING.md.
//
// Diagnostics only: it changes nothing about the stream, the terminal or the
// renderer. Off by default and remembered per device (localStorage, not the
// dashboard prefs, which every device shares). Off, the widget's hooks are a
// null check each.

export const DIAG_STORAGE_KEY = 'tclaude.terminal.diagnostics';
export const DIAG_WINDOW_MS = 5000;
// How often the overlay redraws, and the server's side is read.
export const DIAG_TICK_MS = 1000;
// A scroll step's latency waits this long for its frame, then is dropped.
const LATENCY_GIVE_UP_MS = 3000;

const listeners = new Set();

function storage() {
  try { return globalThis.localStorage || null; } catch { return null; }
}

// diagnosticsEnabled says whether this device has the overlay on.
export function diagnosticsEnabled() {
  try { return storage()?.getItem(DIAG_STORAGE_KEY) === '1'; } catch { return false; }
}

// setDiagnosticsEnabled turns the overlay on or off on this device, for every
// open terminal.
export function setDiagnosticsEnabled(on) {
  try {
    if (on) storage()?.setItem(DIAG_STORAGE_KEY, '1');
    else storage()?.removeItem(DIAG_STORAGE_KEY);
  } catch { /* the overlay still follows this page's choice */ }
  for (const listener of [...listeners]) listener(!!on);
}

// Another tab or window of this device turning it on or off.
try {
  globalThis.addEventListener?.('storage', (event) => {
    if (event.key !== DIAG_STORAGE_KEY) return;
    for (const listener of [...listeners]) listener(event.newValue === '1');
  });
} catch { /* no window */ }

// onDiagnosticsChange calls fn(on) on each change. Returns the unsubscribe.
export function onDiagnosticsChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

// summarize is a list of numbers' count, p50, p95 and max (nulls when empty).
export function summarize(values) {
  const sorted = values.filter(Number.isFinite).sort((a, b) => a - b);
  if (!sorted.length) return { n: 0, p50: null, p95: null, max: null };
  const at = (p) => sorted[Math.min(sorted.length - 1, Math.max(0, Math.ceil(p * sorted.length) - 1))];
  return { n: sorted.length, p50: at(0.5), p95: at(0.95), max: sorted[sorted.length - 1] };
}

// gapsOf is the gaps between consecutive times.
export function gapsOf(times) {
  const out = [];
  for (let i = 1; i < times.length; i += 1) out.push(times[i] - times[i - 1]);
  return out;
}

// droppedFrames counts the animation frames missed between rAF callbacks:
// a gap of k refresh intervals is k - 1 missed. The interval is the
// display's own, read from the shortest gaps (60 Hz, 120 Hz alike).
export function droppedFrames(gaps) {
  // A gap of a second or more is a page that was hidden, not frames dropped.
  gaps = gaps.filter((gap) => gap < 1000);
  if (gaps.length < 2) return { interval: null, dropped: 0 };
  const sorted = [...gaps].sort((a, b) => a - b);
  const interval = sorted[Math.floor(sorted.length * 0.1)];
  if (!(interval > 0)) return { interval: null, dropped: 0 };
  let dropped = 0;
  for (const gap of gaps) dropped += Math.max(0, Math.round(gap / interval) - 1);
  return { interval, dropped };
}

const SPARK = '▁▂▃▄▅▆▇█';

// sparkline draws values as block characters, scaled to their max.
export function sparkline(values, width = 40) {
  const shown = values.slice(-width);
  const max = Math.max(...shown, 0);
  if (!shown.length || max <= 0) return '';
  return shown.map((v) => SPARK[Math.min(SPARK.length - 1, Math.floor((v / max) * (SPARK.length - 1)))]).join('');
}

// createDiagRecorder keeps the rolling window. Each method is one event at
// now(); stats() summarizes the window.
export function createDiagRecorder({ now = () => globalThis.performance?.now?.() ?? Date.now(), windowMs = DIAG_WINDOW_MS } = {}) {
  const messages = []; // {t, bytes}
  const parses = []; // {t, ms}
  const renders = []; // t
  const writeToRender = []; // {t, ms}
  const longTasks = []; // {t, ms}
  const frames = []; // t (rAF)
  const steps = []; // {t, arrival, render} scroll steps
  const keys = []; // t, other input
  let pendingWrites = []; // parsed writes' times, awaiting a render
  let waitingArrival = []; // steps awaiting a frame
  let arrived = []; // steps whose frame arrived, awaiting its parse
  let waitingRender = []; // steps whose frame is parsed, awaiting a render
  const started = now();

  const prune = (list, t, get = (x) => x.t) => {
    while (list.length && get(list[0]) < t - windowMs) list.shift();
  };
  const pruneAll = (t) => {
    for (const list of [messages, parses, writeToRender, longTasks, steps]) prune(list, t);
    for (const list of [renders, frames, keys]) prune(list, t, (x) => x);
    // A terminal that does not draw (hidden, a background tab) keeps
    // receiving: what waits on a render is given up after a while.
    const stale = t - LATENCY_GIVE_UP_MS;
    waitingArrival = waitingArrival.filter((step) => step.t >= stale);
    arrived = arrived.filter((step) => step.t >= stale);
    waitingRender = waitingRender.filter((step) => step.t >= stale);
    pendingWrites = pendingWrites.filter((w) => w >= stale).slice(-1000);
  };

  return {
    message(bytes) {
      const t = now();
      messages.push({ t, bytes });
      for (const step of waitingArrival) { step.arrival = t - step.t; arrived.push(step); }
      waitingArrival = [];
      pruneAll(t);
    },
    // wrote marks a term.write; call the returned function from its callback.
    // Only a parsed write waits for a render: a render before the parse (a
    // cursor blink, earlier data) did not draw it.
    wrote() {
      const t = now();
      const steps = arrived;
      arrived = [];
      return () => {
        const done = now();
        parses.push({ t: done, ms: done - t });
        pendingWrites.push(t);
        waitingRender.push(...steps);
      };
    },
    rendered() {
      const t = now();
      pruneAll(t);
      renders.push(t);
      for (const w of pendingWrites) writeToRender.push({ t, ms: t - w });
      pendingWrites = [];
      for (const step of waitingRender) step.render = t - step.t;
      waitingRender = [];
      pruneAll(t);
    },
    // input is bytes sent to the session: scroll says it was a scroll step.
    input(scroll) {
      const t = now();
      if (!scroll) { keys.push(t); return; }
      const step = { t, arrival: null, render: null };
      steps.push(step);
      waitingArrival.push(step);
      pruneAll(t);
    },
    longTask(ms) { longTasks.push({ t: now(), ms }); },
    frame() { frames.push(now()); },
    stats() {
      const t = now();
      pruneAll(t);
      const span = Math.max(1, Math.min(windowMs, t - started)) / 1000;
      const arrivals = messages.map((m) => m.t);
      const gaps = gapsOf(arrivals);
      const frameGaps = gapsOf(frames);
      const done = steps.filter((s) => s.arrival !== null);
      return {
        windowS: span,
        messages: { perS: messages.length / span, bytesPerS: messages.reduce((sum, m) => sum + m.bytes, 0) / span, size: summarize(messages.map((m) => m.bytes)) },
        gaps: { ...summarize(gaps), spark: sparkline(gaps) },
        parse: summarize(parses.map((p) => p.ms)),
        writeToRender: summarize(writeToRender.map((w) => w.ms)),
        renders: { perS: renders.length / span, gap: summarize(gapsOf(renders)) },
        longTasks: { n: longTasks.length, ms: longTasks.reduce((sum, l) => sum + l.ms, 0), max: summarize(longTasks.map((l) => l.ms)).max },
        frames: { perS: frames.length / span, ...droppedFrames(frameGaps) },
        scroll: {
          stepsPerS: steps.length / span,
          toArrival: summarize(done.map((s) => s.arrival)),
          toRender: summarize(done.filter((s) => s.render !== null).map((s) => s.render)),
        },
        keysPerS: keys.length / span,
      };
    },
  };
}

// serverStats summarizes the server's log of the socket over the window
// ending at its own clock (nowMs): frames sent, their gaps, sizes and write
// times, and how soon output followed input there.
export function serverStats(events, nowMs, windowMs = DIAG_WINDOW_MS) {
  const recent = events.filter((ev) => ev.at_ms >= nowMs - windowMs);
  const out = recent.filter((ev) => ev.kind === 'out');
  const span = windowMs / 1000;
  const turnaround = [];
  let i = 0;
  for (const ev of recent) {
    if (ev.kind !== 'in') continue;
    while (i < out.length && out[i].at_ms < ev.at_ms) i += 1;
    if (i < out.length) turnaround.push(out[i].at_ms - ev.at_ms);
  }
  return {
    framesPerS: out.length / span,
    bytesPerS: out.reduce((sum, ev) => sum + ev.bytes, 0) / span,
    size: summarize(out.map((ev) => ev.bytes)),
    gaps: summarize(gapsOf(out.map((ev) => ev.at_ms))),
    writeMs: summarize(out.map((ev) => ev.write_ms || 0)),
    inputPerS: recent.filter((ev) => ev.kind === 'in').length / span,
    turnaround: summarize(turnaround),
  };
}

const num = (v, digits = 0) => (v === null || v === undefined || !Number.isFinite(v) ? '–' : v.toFixed(digits));
const dist = (s, unit = 'ms', digits = 0) => (s.n ? `p50 ${num(s.p50, digits)} · p95 ${num(s.p95, digits)} · max ${num(s.max, digits)} ${unit}` : '–');
const kb = (bytes) => (bytes >= 1024 ? `${(bytes / 1024).toFixed(1)} KiB` : `${Math.round(bytes)} B`);

// reportLines is the overlay's text and the copied report: the client, the
// server when known, and the environment.
export function reportLines(stats, env = {}, server = null) {
  const lines = [
    `Terminal diagnostics · last ${num(stats.windowS, 1)} s`,
    `Frames:   received ${num(stats.messages.perS, 1)}/s · rendered ${num(stats.renders.perS, 1)}/s`,
    `Latency (per scroll step, ${num(stats.scroll.stepsPerS, 1)} steps/s):`,
    `  → arrival ${dist(stats.scroll.toArrival)}`,
    `  → render  ${dist(stats.scroll.toRender)}`,
    `Messages: ${kb(stats.messages.bytesPerS)}/s · size ${stats.messages.size.n ? `p50 ${kb(stats.messages.size.p50)} · p95 ${kb(stats.messages.size.p95)} · max ${kb(stats.messages.size.max)}` : '–'}`,
    `Arrival gaps: ${dist(stats.gaps)}`,
    `  ${stats.gaps.spark || '(no gaps yet)'}`,
    `xterm:    parse ${dist(stats.parse, 'ms', 1)}`,
    `          write→render ${dist(stats.writeToRender)}`,
    `          render gaps ${dist(stats.renders.gap)}`,
    `Main thread: ${stats.longTasks.n} long tasks (${num(stats.longTasks.ms)} ms${stats.longTasks.n ? `, max ${num(stats.longTasks.max)} ms` : ''})${env.longTasks === false ? ' · not reported by this browser' : ''}`,
    `Frames (rAF): ${num(stats.frames.perS, 1)}/s · ${stats.frames.dropped} dropped · interval ${num(stats.frames.interval, 1)} ms`,
    `Keys sent: ${num(stats.keysPerS, 1)}/s`,
  ];
  if (server) {
    lines.push(
      `Server:   sent ${num(server.framesPerS, 1)} frames/s · ${kb(server.bytesPerS)}/s · input ${num(server.inputPerS, 1)}/s`,
      `  send gaps ${dist(server.gaps)}`,
      `  size ${server.size.n ? `p50 ${kb(server.size.p50)} · p95 ${kb(server.size.p95)} · max ${kb(server.size.max)}` : '–'}`,
      `  write ${dist(server.writeMs, 'ms', 1)}`,
      `  input→output ${dist(server.turnaround)}`,
    );
  } else if (env.serverNote) {
    lines.push(`Server:   ${env.serverNote}`);
  }
  lines.push(
    `Renderer: ${env.renderer || '–'} · dpr ${num(env.dpr, 2)} · ${env.cols || '–'}×${env.rows || '–'} · font ${num(env.fontSize, 1)} px`,
    `Device:   ${env.layout || '–'} · ${env.connection || 'connection –'} · ${env.viewport || ''}`,
    `UA:       ${env.userAgent || '–'}`,
  );
  return lines;
}

// rendererOf says which xterm renderer draws in host: a canvas there means
// the canvas or WebGL addon, else xterm's DOM renderer.
export function rendererOf(host) {
  const canvas = host?.querySelector?.('.xterm-screen canvas');
  if (!canvas) return 'DOM';
  try { return canvas.getContext?.('webgl2') || canvas.getContext?.('webgl') ? 'WebGL' : 'canvas'; } catch { return 'canvas'; }
}

// environment reads what the report says about this device and terminal.
export function environment({ host, term, win = globalThis }) {
  const nav = win.navigator || {};
  const c = nav.connection;
  const doc = host?.ownerDocument;
  return {
    renderer: rendererOf(host),
    dpr: win.devicePixelRatio || 1,
    cols: term?.cols, rows: term?.rows, fontSize: term?.options?.fontSize,
    layout: doc?.documentElement?.dataset?.layout === 'phone' ? 'phone layout' : 'desktop layout',
    connection: c ? `net ${c.effectiveType || '?'}${c.rtt ? ` rtt ${c.rtt} ms` : ''}${c.downlink ? ` ${c.downlink} Mb/s` : ''}` : '',
    viewport: `${win.innerWidth || '?'}×${win.innerHeight || '?'}`,
    userAgent: String(nav.userAgent || '').slice(0, 160),
    longTasks: !!win.PerformanceObserver?.supportedEntryTypes?.includes?.('longtask'),
  };
}

function randomID() {
  const bytes = new Uint8Array(9);
  try { globalThis.crypto.getRandomValues(bytes); } catch { for (let i = 0; i < bytes.length; i += 1) bytes[i] = Math.floor(Math.random() * 256); }
  return 'td_' + [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('');
}

// mountDiagnostics starts measuring a terminal and shows the overlay on
// host. It returns the recorder hooks the widget calls ({message, wrote,
// input}) and stop(). serverSide: whether the socket is this node's own
// (the server's log can then be read); sendControl(json) sends the socket a
// control frame.
export function mountDiagnostics({ host, term, serverSide = false, sendControl = () => false, fetchImpl = globalThis.fetch, copy, onClose, win = globalThis }) {
  const doc = host.ownerDocument;
  const recorder = createDiagRecorder();
  // A fresh id per socket: an old socket's teardown never touches the new log.
  let id = randomID();
  const disposers = [];
  let server = null;
  let serverNote = serverSide ? 'waiting for the server…' : 'not read for a remote terminal';
  let serverEvents = [];
  let serverSeq = 0;
  let serverNow = 0;
  let stopped = false;

  // xterm draws on its own schedule: onRender is each draw.
  const render = term.onRender?.(() => recorder.rendered());
  if (render) disposers.push(() => render.dispose());
  // The main thread: long tasks, and the animation frames that never ran.
  if (win.PerformanceObserver?.supportedEntryTypes?.includes?.('longtask')) {
    try {
      const observer = new win.PerformanceObserver((list) => { for (const entry of list.getEntries()) recorder.longTask(entry.duration); });
      observer.observe({ type: 'longtask' });
      disposers.push(() => observer.disconnect());
    } catch { /* not reported here */ }
  }
  let raf = 0;
  const frame = () => { if (stopped) return; recorder.frame(); raf = win.requestAnimationFrame?.(frame) || 0; };
  if (typeof win.requestAnimationFrame === 'function') {
    raf = win.requestAnimationFrame(frame);
    disposers.push(() => win.cancelAnimationFrame?.(raf));
  }

  const panel = doc.createElement('div');
  panel.className = 'term-diag';
  panel.setAttribute('data-term-diag', '');
  panel.setAttribute('role', 'status');
  const head = doc.createElement('div');
  head.className = 'term-diag-head';
  const title = doc.createElement('b');
  title.textContent = 'Diagnostics';
  const button = (text, label = '') => {
    const made = doc.createElement('button');
    made.className = 'ui-btn ui-btn-ghost ui-sm term-diag-btn';
    made.type = 'button';
    made.textContent = text;
    if (label) made.setAttribute('aria-label', label);
    return made;
  };
  const copyButton = button('Copy report');
  copyButton.setAttribute('data-term-diag-copy', '');
  const foldButton = button('▾', 'Fold the readout');
  foldButton.setAttribute('aria-expanded', 'true');
  const closeButton = button('✕', 'Turn diagnostics off');
  const body = doc.createElement('pre');
  body.className = 'term-diag-body';
  head.append(title, foldButton, copyButton, closeButton);
  panel.append(head, body);
  host.append(panel);
  // The panel sits over the terminal's corner: its box anchors it.
  const wasStatic = (win.getComputedStyle?.(host)?.position || 'static') === 'static';
  if (wasStatic) {
    const previous = host.style.position;
    host.style.position = 'relative';
    disposers.push(() => { host.style.position = previous; });
  }
  // Taps on the overlay are not the terminal's.
  for (const type of ['pointerdown', 'mousedown', 'touchstart', 'wheel']) panel.addEventListener(type, (event) => event.stopPropagation());

  const report = () => {
    const env = { ...environment({ host, term, win }), serverNote };
    const srv = server && serverEvents.length ? serverStats(serverEvents, serverNow) : null;
    return reportLines(recorder.stats(), env, srv).join('\n');
  };
  const paint = () => { if (!stopped && !body.hidden) body.textContent = report(); };
  copyButton.addEventListener('click', () => {
    const text = `${report()}\nAt: ${new Date().toISOString()}`;
    void Promise.resolve(copy ? copy(text) : win.navigator?.clipboard?.writeText?.(text)).catch(() => {});
  });
  closeButton.addEventListener('click', () => onClose?.());
  // Folded, only the bar shows: the terminal under it takes swipes again.
  foldButton.addEventListener('click', () => {
    const folded = !body.hidden;
    body.hidden = folded;
    foldButton.textContent = folded ? '▸' : '▾';
    foldButton.setAttribute('aria-expanded', folded ? 'false' : 'true');
    if (!folded) paint();
  });

  // The server's log of this socket, read each tick.
  const announce = () => { if (serverSide) sendControl({ type: 'diag', id }); };
  // Read only while this terminal can be seen: the reads ride the same
  // connection, and a hidden pane's would only add to what is measured.
  const shown = () => doc.visibilityState !== 'hidden' && host.isConnected && host.offsetParent !== null;
  const readServer = async () => {
    if (!serverSide || typeof fetchImpl !== 'function' || !shown()) return;
    const asked = id;
    try {
      const response = await fetchImpl(`/api/terminal-diag?id=${asked}&since=${serverSeq}`, { cache: 'no-store', credentials: 'same-origin' });
      if (asked !== id) return;
      if (!response.ok) { serverNote = response.status === 404 ? 'no log yet (connecting)' : `unavailable (${response.status})`; server = null; return; }
      const data = await response.json();
      if (asked !== id) return;
      serverNow = Number(data.now_ms) || serverNow;
      const fresh = Array.isArray(data.events) ? data.events : [];
      if (fresh.length) serverSeq = fresh[fresh.length - 1].seq;
      serverEvents = [...serverEvents, ...fresh].filter((ev) => ev.at_ms >= serverNow - DIAG_WINDOW_MS);
      server = true;
    } catch {
      serverNote = 'unavailable';
      server = null;
    }
  };
  announce();
  const tick = win.setInterval(() => { void readServer().then(paint); }, DIAG_TICK_MS);
  disposers.push(() => win.clearInterval(tick));
  paint();

  return {
    message: (bytes) => recorder.message(bytes),
    wrote: () => recorder.wrote(),
    input: (scroll) => recorder.input(scroll),
    // reconnected: a new socket starts its own server log.
    reconnected() { id = randomID(); serverEvents = []; serverSeq = 0; server = null; serverNote = 'waiting for the server…'; announce(); },
    report,
    stop() {
      if (stopped) return;
      stopped = true;
      if (serverSide) sendControl({ type: 'diag', id: '' });
      for (const dispose of disposers.splice(0)) { try { dispose(); } catch { /* best effort */ } }
      panel.remove();
    },
  };
}
