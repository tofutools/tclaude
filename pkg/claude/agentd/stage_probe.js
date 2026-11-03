// tclaude live-page probe.
//
// The stage listener (stage.go) inserts this script into every HTML document
// it serves to a live-page frame. It runs inside the page's sandboxed,
// opaque-origin frame and is exactly as untrusted as the page itself: it holds
// no secret and reaches nothing of the dashboard's. The page's own scripts can
// read and forge everything it sends, so the dashboard treats its messages as
// page content: checked against a schema and size caps, and shown as text.
//
// The protocol (tcl-2dwbad), all over postMessage between the frame and its
// parent, the dashboard view:
//
//   probe → parent  {type:'tclaude-probe', v:1, kind:'ready'}
//                    once, at start: asks the parent for a hello.
//   parent → probe  {type:'tclaude-parent', v:1, nonce, kind:'hello'}
//                    the nonce of this load; every later message carries it.
//   probe → parent  {..., nonce, kind:'scroll', x, y, vw, vh, docW, docH, path}
//                    after the hello and on scroll/resize, throttled.
//   parent → probe  {..., nonce, id, kind:'elementsAt', points:[[x,y],...]}
//   parent → probe  {..., nonce, id, kind:'dom', selector, mode:'text'|'html'|'styles'}
//   probe → parent  {..., nonce, id, kind:'result', elements:[...]} or
//                   {..., nonce, id, kind:'result', count, items:[...], error?}
//
// Live patches (tcl-14z3fq), relayed by the parent from the daemon:
//
//   parent → probe  {..., nonce, id, kind:'patch', patch:{kind:'css'|'dom',...}}
//                    applied with tclaudeApplyPatch (stage_patch.js) once the
//                    document has parsed.
//   probe → parent  {..., nonce, id, kind:'result', applied, error?}
//   parent → probe  {..., nonce, id, kind:'eval', js}
//                    the agent's script, run once with an indirect eval; a
//                    promise is awaited.
//   probe → parent  {..., nonce, id, kind:'result', json} or {..., error}
//
// Only messages from window.parent are read. The parent's target origin is '*'
// (the frame's origin is opaque), so nothing it sends is a secret.
//
// This file must never contain a closing script tag: it is inlined verbatim.
(function () {
  'use strict';
  if (window.__tclaudeProbe) return;
  Object.defineProperty(window, '__tclaudeProbe', {
    value: Object.freeze({ version: 1 }),
    configurable: false,
    enumerable: false,
    writable: false,
  });
  var parentWin = window.parent;
  if (!parentWin || parentWin === window) return;

  var MAX_POINTS = 400;
  var MAX_ELEMENTS = 50;
  var MAX_TEXT = 200;
  var MAX_PATH = 300;
  var MAX_ITEMS = 10;
  var MAX_VALUE = 16 * 1024;
  // The parent refuses a message over 64 KiB of JSON; a DOM result stays
  // under this, or says it is too large.
  var MAX_RESULT_JSON = 60 * 1024;
  var MAX_SELECTOR = 500;
  var MAX_ERROR = 200;
  // The daemon refuses an eval result over 16 KiB of JSON.
  var MAX_EVAL_JSON = 16 * 1024;
  var STYLE_PROPS = [
    'display', 'position', 'top', 'right', 'bottom', 'left', 'width', 'height', 'box-sizing',
    'margin', 'padding', 'border', 'border-radius', 'color', 'background-color', 'background-image',
    'font-family', 'font-size', 'font-weight', 'line-height', 'text-align', 'opacity', 'z-index',
    'overflow', 'flex', 'flex-direction', 'align-items', 'justify-content', 'gap', 'grid-template-columns',
    'visibility', 'transform', 'box-shadow',
  ];

  var nonce = '';

  function send(msg) {
    msg.type = 'tclaude-probe';
    msg.v = 1;
    if (nonce) msg.nonce = nonce;
    try { parentWin.postMessage(msg, '*'); } catch (e) { /* the parent went away */ }
  }

  function clip(s, n) {
    s = String(s == null ? '' : s).replace(/[\s\u0000-\u001f\u007f-\u009f]+/g, ' ').trim();
    return s.length > n ? s.slice(0, n - 1) + '…' : s;
  }

  function num(v) {
    v = Number(v);
    return isFinite(v) ? Math.round(v) : 0;
  }

  function cssIdent(s) {
    if (window.CSS && CSS.escape) return CSS.escape(s);
    return String(s).replace(/[^a-zA-Z0-9_-]/g, '\\$&');
  }

  // elementPath names an element as a short CSS path from the nearest id (or
  // body): tag#id, or tag.class.class:nth-of-type(n), joined with " > ".
  function elementPath(el) {
    var parts = [];
    var node = el;
    while (node && node.nodeType === 1 && parts.length < 8) {
      var tag = node.tagName.toLowerCase();
      if (tag === 'html') break;
      if (node.id && node.id.length <= 64) {
        parts.unshift(tag + '#' + cssIdent(node.id));
        break;
      }
      var part = tag;
      var cls = typeof node.className === 'string' ? node.className.split(/\s+/).filter(Boolean).slice(0, 2) : [];
      for (var i = 0; i < cls.length; i++) {
        if (cls[i].length <= 40) part += '.' + cssIdent(cls[i]);
      }
      var parent = node.parentElement;
      if (parent && tag !== 'body') {
        var same = 0;
        var index = 0;
        for (var c = parent.firstElementChild; c; c = c.nextElementSibling) {
          if (c.tagName === node.tagName) {
            same++;
            if (c === node) index = same;
          }
        }
        if (same > 1) part += ':nth-of-type(' + index + ')';
      }
      parts.unshift(part);
      if (tag === 'body') break;
      node = parent;
    }
    var path = parts.join(' > ');
    return path.length > MAX_PATH ? path.slice(path.length - MAX_PATH) : path;
  }

  function elementText(el) {
    var text = el.innerText || el.textContent || '';
    if (!clip(text, 1)) {
      text = el.getAttribute('aria-label') || el.getAttribute('alt') || el.getAttribute('placeholder') || el.getAttribute('title') || '';
    }
    return clip(text, MAX_TEXT);
  }

  var scrollTimer = 0;
  function reportScroll() {
    scrollTimer = 0;
    if (!nonce) return;
    var doc = document.documentElement;
    var body = document.body;
    send({
      kind: 'scroll',
      x: num(window.scrollX),
      y: num(window.scrollY),
      vw: num(window.innerWidth),
      vh: num(window.innerHeight),
      docW: num(Math.max(doc ? doc.scrollWidth : 0, body ? body.scrollWidth : 0)),
      docH: num(Math.max(doc ? doc.scrollHeight : 0, body ? body.scrollHeight : 0)),
      path: clip(location.pathname + location.search, 512),
    });
  }
  function scheduleScroll() {
    if (!scrollTimer) scrollTimer = setTimeout(reportScroll, 100);
  }

  function elementsAt(points) {
    var hits = [];
    var seen = typeof Map === 'function' ? new Map() : null;
    if (!seen || !Array.isArray(points)) return [];
    for (var i = 0; i < points.length && i < MAX_POINTS; i++) {
      var p = points[i];
      if (!Array.isArray(p)) continue;
      var el = document.elementFromPoint(Number(p[0]) || 0, Number(p[1]) || 0);
      if (!el || el === document.documentElement || el === document.body) continue;
      var hit = seen.get(el);
      if (hit) {
        hit.hits++;
      } else if (seen.size < MAX_ELEMENTS) {
        hit = { el: el, hits: 1 };
        seen.set(el, hit);
        hits.push(hit);
      }
    }
    hits.sort(function (a, b) { return b.hits - a.hits; });
    return hits.map(function (h) {
      return { path: elementPath(h.el), text: elementText(h.el), hits: h.hits };
    });
  }

  function rectOf(el) {
    var r = el.getBoundingClientRect();
    return { x: num(r.left + window.scrollX), y: num(r.top + window.scrollY), w: num(r.width), h: num(r.height) };
  }

  function domRead(selector, mode) {
    if (typeof selector !== 'string' || !selector || selector.length > MAX_SELECTOR) return { count: 0, items: [], error: 'invalid selector' };
    var list;
    try {
      list = document.querySelectorAll(selector);
    } catch (e) {
      return { count: 0, items: [], error: 'invalid selector' };
    }
    var items = [];
    for (var i = 0; i < list.length && i < MAX_ITEMS; i++) {
      var el = list[i];
      var value = '';
      if (mode === 'html') {
        value = el.outerHTML || '';
      } else if (mode === 'styles') {
        var cs = window.getComputedStyle(el);
        var lines = [];
        for (var j = 0; j < STYLE_PROPS.length; j++) {
          var v = cs.getPropertyValue(STYLE_PROPS[j]);
          if (v) lines.push(STYLE_PROPS[j] + ': ' + v);
        }
        value = lines.join('\n');
      } else {
        value = el.innerText || el.textContent || '';
      }
      if (value.length > MAX_VALUE) value = value.slice(0, MAX_VALUE - 1) + '…';
      items.push({ path: elementPath(el), rect: rectOf(el), value: value });
    }
    var out = { count: list.length, items: items };
    // Drop items from the end until the answer fits; say so if none does.
    while (items.length && JSON.stringify(out).length > MAX_RESULT_JSON) items.pop();
    if (!items.length && list.length) {
      var first = list[0];
      var cut = { path: elementPath(first), rect: rectOf(first), value: '' };
      out.items = [cut];
      var room = MAX_RESULT_JSON - JSON.stringify(out).length - 64;
      var whole = mode === 'html' ? first.outerHTML || '' : first.innerText || first.textContent || '';
      // Escaping can double a character's JSON length; halve the room.
      cut.value = whole.slice(0, Math.max(0, Math.floor(room / 2))) + '…';
      if (JSON.stringify(out).length > MAX_RESULT_JSON) return { count: list.length, items: [], error: 'the result is too large; use a narrower selector' };
    }
    if (items.length < Math.min(list.length, MAX_ITEMS)) out.truncated = true;
    return out;
  }

  // whenParsed runs fn once the document has parsed: a patch sent while the
  // head is still loading (the replay after a reload) finds the body.
  var parsedQueue = null;
  function whenParsed(fn) {
    if (document.readyState !== 'loading') { fn(); return; }
    if (!parsedQueue) {
      parsedQueue = [];
      document.addEventListener('DOMContentLoaded', function () {
        var q = parsedQueue;
        parsedQueue = null;
        for (var i = 0; i < q.length; i++) q[i]();
      });
    }
    parsedQueue.push(fn);
  }

  function errorText(e) {
    return clip(e && e.message ? e.message : e, MAX_ERROR) || 'error';
  }

  function applyPatch(id, patch) {
    var out = { kind: 'result', id: id, applied: 0 };
    try {
      out.applied = num(tclaudeApplyPatch(document, patch));
    } catch (e) {
      out.error = errorText(e);
    }
    send(out);
  }

  function evalResult(id, value) {
    var json;
    try {
      json = JSON.stringify(value);
    } catch (e) {
      send({ kind: 'result', id: id, error: clip('the result has no JSON form: ' + errorText(e), MAX_ERROR) });
      return;
    }
    if (json === undefined) json = 'null';
    if (json.length > MAX_EVAL_JSON) {
      send({ kind: 'result', id: id, error: 'the result is ' + json.length + ' characters of JSON; the most is ' + MAX_EVAL_JSON });
      return;
    }
    send({ kind: 'result', id: id, json: json });
  }

  function runEval(id, js) {
    var value;
    try {
      value = (0, eval)(String(js));
    } catch (e) {
      send({ kind: 'result', id: id, error: errorText(e) });
      return;
    }
    if (value && typeof value.then === 'function') {
      Promise.resolve(value).then(function (v) { evalResult(id, v); }, function (e) {
        send({ kind: 'result', id: id, error: errorText(e) });
      });
      return;
    }
    evalResult(id, value);
  }

  window.addEventListener('message', function (event) {
    if (event.source !== parentWin) return;
    var d = event.data;
    if (!d || typeof d !== 'object' || d.type !== 'tclaude-parent' || d.v !== 1) return;
    if (typeof d.nonce !== 'string' || !/^[0-9a-f]{32}$/.test(d.nonce)) return;
    if (d.kind === 'hello') {
      nonce = d.nonce;
      send({ kind: 'hello' });
      reportScroll();
      return;
    }
    if (d.nonce !== nonce) return;
    var id = typeof d.id === 'string' && /^[0-9a-z]{1,32}$/.test(d.id) ? d.id : '';
    if (!id) return;
    if (d.kind === 'elementsAt') {
      send({ kind: 'result', id: id, elements: elementsAt(d.points) });
    } else if (d.kind === 'dom') {
      var mode = d.mode === 'html' || d.mode === 'styles' ? d.mode : 'text';
      var out = domRead(d.selector, mode);
      out.kind = 'result';
      out.id = id;
      send(out);
    } else if (d.kind === 'patch') {
      var patch = d.patch;
      whenParsed(function () { applyPatch(id, patch); });
    } else if (d.kind === 'eval') {
      var js = d.js;
      whenParsed(function () { runEval(id, js); });
    }
  });
  window.addEventListener('scroll', scheduleScroll, { passive: true });
  window.addEventListener('resize', scheduleScroll, { passive: true });
  window.addEventListener('load', scheduleScroll);
  send({ kind: 'ready' });
})();
