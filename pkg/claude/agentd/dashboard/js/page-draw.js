// page-draw.js -- drawing on top of a live page (tcl-2dwbad). In Draw mode a
// layer in the dashboard, over the page's frame, takes pen, arrow, box and
// text marks with the drawing boards' tools and look. Marks are kept in page
// coordinates (the probe reports the frame's scroll), and when one ends the
// probe names the elements under it. Send hands the marks and a note to the
// agent that showed the page.
//
// The layer is the dashboard's, never the page's: the frame cannot see the
// marks, and what the probe sends back (element paths and text snippets) is
// page content, checked by page-probe-model.js and shown only as text.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { DRAWING_BOARD_COLORS, DRAWING_BOARD_TOOLS, newElement, normalizeBox, simplifyStroke } from './drawing-board-model.js';
import { DrawingBoardElement } from './drawing-board-island.js';
import {
  acceptProbeMessage, cleanText, newProbeNonce, newProbeRequestId, planPatchSync, probeDom, probeElementsAt, probeEval, probeHello,
  probePatch, samplePoints,
} from './page-probe-model.js';

const html = htm.bind(h);

const PROBE_TIMEOUT_MS = 3000;
// An eval may await a promise; the daemon waits ten seconds for its answer.
const PROBE_EVAL_TIMEOUT_MS = 8000;
export const PAGE_NOTE_MAX = 4000;
export const PAGE_MARKS_MAX = 100;
// The daemon takes at most 2000 points a mark; a long stroke is thinned to this.
export const PAGE_MARK_POINTS_MAX = 1000;

// thinPoints keeps at most max points, evenly, always the first and last.
export function thinPoints(points, max = PAGE_MARK_POINTS_MAX) {
  if (points.length <= max) return points;
  const out = [];
  const step = (points.length - 1) / (max - 1);
  for (let i = 0; i < max; i++) out.push(points[Math.round(i * step)]);
  return out;
}

// The drawing board tools a page mark can be made with, under the mark's kind.
const MARK_TOOLS = Object.freeze([['freedraw', 'pen'], ['arrow', 'arrow'], ['rectangle', 'box'], ['text', 'text']]);
export const PAGE_DRAW_TOOLS = Object.freeze(MARK_TOOLS.map(([id, kind]) => ({ ...DRAWING_BOARD_TOOLS.find((t) => t.id === id), kind })));
const KIND_OF = Object.fromEntries(MARK_TOOLS);
// Red first: a mark is a correction on someone else's page.
export const PAGE_DRAW_COLORS = Object.freeze(['#e03131', ...DRAWING_BOARD_COLORS.filter((c) => c !== '#e03131')]);

// usePageProbe talks to the probe in the frame: a fresh nonce per load (sent
// in the hello the probe's 'ready' asks for), the frame's last scroll report,
// and requests answered by id. frameRef holds the iframe.
//
// It also keeps the frame in step with the page's live patch journal
// (tcl-14z3fq): syncPatches takes the journal the daemon sent, and each load
// replays it after the hello. A load holding a patch the journal dropped, or
// a switch to "without", asks onReload for a fresh load.
export function usePageProbe(frameRef, { win = globalThis.window, onReload = null } = {}) {
  const [scroll, setScroll] = useState(null);
  const [connected, setConnected] = useState(false);
  const onReloadRef = useRef(onReload);
  onReloadRef.current = onReload;
  const api = useRef(null);
  if (!api.current) {
    const s = {
      nonce: '', acked: false, pending: new Map(),
      journal: [], applied: [], results: new Map(), without: false, chain: Promise.resolve(),
    };
    const post = (message) => {
      try { frameRef.current?.contentWindow?.postMessage(message, '*'); } catch { /* the frame went away */ }
    };
    const dropPending = () => {
      for (const p of s.pending.values()) {
        clearTimeout(p.timer);
        p.reject(new Error('the page reloaded'));
      }
      s.pending.clear();
    };
    const hello = () => {
      dropPending();
      s.nonce = newProbeNonce();
      s.acked = false;
      s.applied = [];
      s.results = new Map();
      setConnected(false);
      post(probeHello(s.nonce));
    };
    const request = (build, timeout = PROBE_TIMEOUT_MS) => new Promise((resolve, reject) => {
      if (!s.acked) {
        reject(new Error('the page has no probe answering'));
        return;
      }
      const id = newProbeRequestId();
      const timer = setTimeout(() => {
        s.pending.delete(id);
        reject(new Error('the page did not answer'));
      }, timeout);
      s.pending.set(id, { resolve, reject, timer });
      post(build(s.nonce, id));
    });
    const reload = () => {
      s.acked = false;
      dropPending();
      onReloadRef.current?.();
    };
    // sync brings the current load in step with the journal, one step at a
    // time, and answers for patch number answerFor: what applying it did, or
    // the state that kept it from applying now.
    const sync = (answerFor = null) => {
      const run = s.chain.then(async () => {
        if (s.without) return { applied: 0, state: 'without' };
        if (!s.acked) return { applied: 0, state: 'loading' };
        const plan = planPatchSync(s.applied, s.journal);
        if (plan.reload) {
          reload();
          return { applied: 0, state: 'loading' };
        }
        const load = s.nonce;
        for (const patch of plan.apply) {
          if (s.nonce !== load || !s.acked) return { applied: 0, state: 'loading' };
          s.applied.push(patch.n);
          let result;
          try {
            const m = await request((nonce, id) => probePatch(nonce, id, patch));
            result = { applied: typeof m.applied === 'number' ? m.applied : 0, ...(m.error ? { error: m.error } : {}) };
          } catch (cause) {
            result = { applied: 0, error: String(cause?.message || cause).slice(0, 200) };
          }
          s.results.set(patch.n, result);
        }
        return s.results.get(answerFor) || { applied: 0, state: 'loading' };
      });
      s.chain = run.catch(() => {});
      return run;
    };
    api.current = {
      state: s,
      syncPatches: (journal, answerFor = null) => {
        s.journal = Array.isArray(journal) ? journal : [];
        return sync(answerFor);
      },
      setWithout: (without) => {
        if (s.without === !!without) return;
        s.without = !!without;
        if (s.without && s.applied.length) reload();
        else if (!s.without) void sync();
      },
      eval: (js) => request((nonce, id) => probeEval(nonce, id, js), PROBE_EVAL_TIMEOUT_MS),
      onMessage: (event) => {
        const message = acceptProbeMessage(event, frameRef.current?.contentWindow, s.nonce);
        if (!message) return;
        switch (message.kind) {
          case 'ready': hello(); break;
          case 'hello': s.acked = true; setConnected(true); void sync(); break;
          case 'scroll': setScroll(message); break;
          case 'result': {
            const p = s.pending.get(message.id);
            if (!p) break;
            s.pending.delete(message.id);
            clearTimeout(p.timer);
            p.resolve(message);
            break;
          }
          default: break;
        }
      },
      // A document without the probe (or one that ran before the view
      // listened) still gets a hello when it loads.
      onLoad: () => { if (!s.acked) hello(); },
      elementsAt: (points) => request((nonce, id) => probeElementsAt(nonce, id, points))
        .then((m) => (Array.isArray(m.elements) ? m.elements : [])),
      dom: (selector, mode) => request((nonce, id) => probeDom(nonce, id, selector, mode))
        .then((m) => {
          if (!Array.isArray(m.items)) throw new Error('the page sent no DOM result');
          return m;
        }),
      dispose: dropPending,
    };
  }
  useEffect(() => {
    const onMessage = api.current.onMessage;
    win?.addEventListener?.('message', onMessage);
    return () => {
      win?.removeEventListener?.('message', onMessage);
      api.current.dispose();
    };
  }, [win]);
  return { ...api.current, scroll, connected };
}

// elementToMark turns a drawn drawing board element (page coordinates) into the mark
// the daemon stores: its kind, colour, points in page pixels and any text.
export function elementToMark(el) {
  const kind = KIND_OF[el.type];
  const round = ([x, y]) => [Math.round(x), Math.round(y)];
  let points;
  if (kind === 'box') points = [[el.x, el.y], [el.x + el.width, el.y + el.height]];
  else if (kind === 'text') points = [[el.x, el.y]];
  else points = el.points.map(([px, py]) => [el.x + px, el.y + py]);
  return { kind, color: el.strokeColor, points: points.map(round), ...(kind === 'text' ? { text: el.text } : {}) };
}

// marksPayload is what Send posts to /api/pages/{id}/marks.
export function marksPayload(marks, note, scroll) {
  return {
    note: String(note || '').slice(0, PAGE_NOTE_MAX),
    path: scroll?.path || '',
    viewport: { w: scroll?.vw || 0, h: scroll?.vh || 0 },
    scroll: { x: scroll?.x || 0, y: scroll?.y || 0 },
    marks: marks.slice(0, PAGE_MARKS_MAX).map((m) => ({ ...elementToMark(m.el), anchors: m.anchors || [] })),
  };
}

// sendPageMarks posts a batch; it resolves to whether the agent was told.
export async function sendPageMarks(id, payload, { fetchImpl = (...args) => globalThis.fetch(...args) } = {}) {
  const response = await fetchImpl(`/api/pages/${encodeURIComponent(id)}/marks`, {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload),
  });
  let body = null;
  try { body = await response.json(); } catch { /* no body */ }
  if (!response.ok) throw new Error(body?.error?.message || body?.message || `${response.status}`);
  return body || {};
}

// PageDrawLayer is the SVG over the frame. It shows the marks at the frame's
// scroll; in Draw mode (active) it takes the pointer and draws with `tool`.
export function PageDrawLayer({ active, tool, color, marks, onMark, scroll }) {
  const svgRef = useRef(null);
  // The mark being drawn: a ref for the pointer handlers (several events can
  // come before a render), state to draw it.
  const draftRef = useRef(null);
  const [draft, setDraftState] = useState(null);
  const setDraft = (value) => { draftRef.current = value; setDraftState(value); };
  // The text mark being typed, at {x, y} in page coordinates. The ref ends it
  // once: Enter or Escape unmounts the input, whose blur would end it again.
  const typingRef = useRef(null);
  const [typing, setTypingState] = useState(null);
  const setTyping = (value) => { typingRef.current = value; setTypingState(value); };
  const sx = scroll?.x || 0;
  const sy = scroll?.y || 0;
  const at = (event) => {
    const rect = svgRef.current?.getBoundingClientRect?.() || { left: 0, top: 0 };
    return [event.clientX - rect.left + sx, event.clientY - rect.top + sy];
  };
  useEffect(() => { if (!active) { setDraft(null); setTyping(null); } }, [active]);
  const down = (event) => {
    if (!active || event.button > 0) return;
    event.preventDefault();
    const [x, y] = at(event);
    if (tool === 'text') {
      setTyping({ x, y });
      return;
    }
    try { svgRef.current?.setPointerCapture?.(event.pointerId); } catch { /* synthetic pointer */ }
    const props = { x, y, strokeColor: color, strokeWidth: 3 };
    if (tool === 'rectangle') setDraft({ el: newElement('rectangle', props), from: [x, y] });
    else setDraft({ el: newElement(tool, { ...props, points: tool === 'arrow' ? [[0, 0], [0, 0]] : [[0, 0]] }), from: [x, y] });
  };
  const move = (event) => {
    const current = draftRef.current;
    if (!current) return;
    const [x, y] = at(event);
    const { el, from } = current;
    if (el.type === 'rectangle') {
      setDraft({ from, el: { ...el, ...normalizeBox(from[0], from[1], x, y) } });
    } else if (el.type === 'arrow') {
      setDraft({ from, el: { ...el, points: [[0, 0], [x - el.x, y - el.y]] } });
    } else {
      setDraft({ from, el: { ...el, points: [...el.points, [x - el.x, y - el.y]] } });
    }
  };
  const up = () => {
    const current = draftRef.current;
    if (!current) return;
    const { el } = current;
    setDraft(null);
    const tiny = el.type === 'rectangle' ? el.width < 4 && el.height < 4
      : el.points.length < 2 || el.points.every(([px, py]) => Math.hypot(px, py) < 4);
    if (tiny) return;
    onMark(el.type === 'freedraw' ? { ...el, points: thinPoints(simplifyStroke(el.points)) } : el);
  };
  const finishText = (text) => {
    const spot = typingRef.current;
    setTyping(null);
    const value = cleanText(String(text || '')).trim();
    if (!spot || !value) return;
    onMark(newElement('text', { x: spot.x, y: spot.y, text: value.slice(0, 500), fontSize: 20, strokeColor: color }));
  };
  return html`<div class=${`page-draw${active ? ' on' : ''}`}>
    <svg ref=${svgRef} class="page-draw-svg" aria-label=${active ? 'Drawing layer over the page' : null} role=${active ? 'application' : null}
      onPointerDown=${down} onPointerMove=${move} onPointerUp=${up} onPointerCancel=${() => setDraft(null)}>
      <g transform=${`translate(${-sx} ${-sy})`}>
        ${marks.map((m) => html`<${DrawingBoardElement} key=${m.el.id} el=${m.el} />`)}
        ${draft ? html`<${DrawingBoardElement} el=${draft.el} />` : null}
      </g>
    </svg>
    ${typing ? html`<input class="ui-input ui-sm page-draw-text" aria-label="Text for the mark" maxLength="500" autoFocus
      style=${`left:${typing.x - sx}px;top:${typing.y - sy}px`}
      onKeyDown=${(event) => {
        if (event.key === 'Enter') { event.preventDefault(); finishText(event.currentTarget.value); }
        if (event.key === 'Escape') { event.preventDefault(); setTyping(null); }
      }}
      onBlur=${(event) => finishText(event.currentTarget.value)} />` : null}
  </div>`;
}

// PageDrawBar is the toolbar under the header while drawing: the tools, the
// colours, undo and clear, the note and Send.
export function PageDrawBar({ tool, setTool, color, setColor, marks, onUndo, onClear, note, setNote, onSend, sending, agentName }) {
  const canSend = !sending && (marks.length > 0 || note.trim() !== '');
  return html`<div class="page-draw-bar drawing-board-toolbar" role="toolbar" aria-label="Drawing tools">
    ${PAGE_DRAW_TOOLS.map((t) => html`<button key=${t.id} type="button" data-ui-exempt class=${`drawing-board-tool${tool === t.id ? ' on' : ''}`}
      title=${t.label} aria-label=${t.label} aria-pressed=${tool === t.id ? 'true' : 'false'} onClick=${() => setTool(t.id)}>${t.icon}</button>`)}
    <span class="drawing-board-sep"></span>
    ${PAGE_DRAW_COLORS.map((c) => html`<button key=${c} type="button" data-ui-exempt class=${`drawing-board-swatch${color === c ? ' on' : ''}`} style=${`background:${c}`}
      title=${c} aria-label=${`Colour ${c}`} aria-pressed=${color === c ? 'true' : 'false'} onClick=${() => setColor(c)}></button>`)}
    <span class="drawing-board-sep"></span>
    <button type="button" data-ui-exempt class="drawing-board-tool" title="Undo the last mark" aria-label="Undo the last mark" disabled=${!marks.length} onClick=${onUndo}>↶</button>
    <${Button} size="sm" class="mux-btn" disabled=${!marks.length} title="Remove every mark" onClick=${onClear}>Clear<//>
    <input class="ui-input ui-sm page-draw-note" placeholder="Note for the agent (optional)" aria-label="Note for the agent"
      maxLength=${PAGE_NOTE_MAX} value=${note} onInput=${(event) => setNote(event.currentTarget.value)}
      onKeyDown=${(event) => { if (event.key === 'Enter' && canSend) { event.preventDefault(); onSend(); } }} />
    <${Button} size="sm" variant="primary" class="mux-btn" busy=${sending} disabled=${!canSend}
      title=${`Send the marks and the note to ${agentName}'s inbox`} onClick=${onSend}>Send to agent<//>
  </div>`;
}

// usePageMarks holds a view's marks and asks the probe to anchor each one
// as it is drawn.
export function usePageMarks(probe) {
  const [marks, setMarks] = useState([]);
  const scrollRef = useRef(probe.scroll);
  scrollRef.current = probe.scroll;
  const add = (el) => {
    setMarks((list) => (list.length >= PAGE_MARKS_MAX ? list : [...list, { el, anchors: [] }]));
    const mark = elementToMark(el);
    probe.elementsAt(samplePoints(mark, scrollRef.current || { x: 0, y: 0 }))
      .then((anchors) => setMarks((list) => list.map((m) => (m.el.id === el.id ? { ...m, anchors } : m))))
      .catch(() => { /* unanchored: the page has no probe, or did not answer */ });
  };
  return {
    marks,
    add,
    undo: () => setMarks((list) => list.slice(0, -1)),
    clear: () => setMarks([]),
    // drop removes the marks that were sent, keeping any drawn meanwhile.
    drop: (sent) => {
      const ids = new Set(sent.map((m) => m.el.id));
      setMarks((list) => list.filter((m) => !ids.has(m.el.id)));
    },
  };
}
