// Remote per-node view: when the page URL carries ?node=<instance_id>, the
// whole per-node dashboard shows that trusted peer instead of this node, like
// switching virtual desktops. The browser keeps talking to the local agentd,
// which forwards /api/peer/<id>/... to the peer over the federation plane; the
// peer applies its peer-view contract (docs/federation.md).
//
// This is a classic script loaded before the module graph (after
// auth-session.js), so every feature that captures globalThis.fetch is routed
// without knowing about remote mode. Fleet-level reads (federation status,
// node summaries, the proxy itself) stay local: the chip row and map always
// describe this operator's fleet. So do the operator's own surfaces — UI
// prefs, sign-in session, human inbox, browser notifications, theme audio —
// which belong to the person at this browser, not to the node on screen.
(() => {
  const ID_RE = /^inst_[a-z0-9]{4,64}$/;
  const id = new URLSearchParams(window.location.search).get('node') || '';
  if (!ID_RE.test(id)) return;

  const LOCAL_PREFIXES = [
    '/api/federation/', '/api/peer/', '/api/node-summary', '/api/node/update',
    '/api/dashboard/prefs', '/api/auth/', '/api/human-messages', '/api/browser-notifications', '/api/slop/',
  ];
  const PROXIED_METHODS = new Set(['GET', 'HEAD', 'POST']);
  const prefix = '/api/peer/' + encodeURIComponent(id) + '/';
  const nativeFetch = window.fetch.bind(window);

  // health tracks the remote snapshot poll so the marker can say "stale" or
  // "offline" instead of the local connection banner blaming this agentd.
  const health = { ok: null, lastOK: null, failure: null };
  function noteSnapshot(response, body) {
    if (response.ok) {
      health.ok = true; health.lastOK = Date.now(); health.failure = null;
    } else {
      health.ok = false;
      health.failure = { status: response.status, code: body?.code || '', reason: body?.reason || '', lastSeen: body?.last_seen || null };
    }
    window.dispatchEvent(new CustomEvent('tclaude:remote-health', { detail: { ...health } }));
  }

  function rewrite(url) {
    if (url.origin !== window.location.origin || !url.pathname.startsWith('/api/')) return null;
    if (LOCAL_PREFIXES.some((p) => url.pathname.startsWith(p))) return null;
    return prefix + url.pathname.slice('/api/'.length) + url.search;
  }

  // localFetch reaches this node's own API from a peer's page: the fused
  // Groups view reads this node's snapshot alongside the peers'.
  window.__tclaudeRemoteNode = Object.freeze({ id, health, localFetch: nativeFetch });
  document.documentElement.classList.add('remote-node');

  window.fetch = async (input, init) => {
    const request = input instanceof Request ? input : null;
    const url = new URL(request ? request.url : String(input), window.location.href);
    const target = rewrite(url);
    if (!target) return nativeFetch(input, init);
    const method = String(init?.method || request?.method || 'GET').toUpperCase();
    if (!PROXIED_METHODS.has(method)) {
      // The peer proxy carries reads and the few writes the receiving node
      // permits; anything else is refused here rather than sent somewhere
      // it could be mistaken for a local change.
      return new Response(JSON.stringify({ error: 'not available in a peer view', code: 'peer_view_read_only' }), { status: 403, headers: { 'Content-Type': 'application/json' } });
    }
    const response = await nativeFetch(request ? new Request(new URL(target, url).href, request) : target, init);
    if (url.pathname === '/api/snapshot') {
      let body = null;
      if (!response.ok) { try { body = await response.clone().json(); } catch (_) { body = null; } }
      noteSnapshot(response, body);
    }
    return response;
  };
})();
