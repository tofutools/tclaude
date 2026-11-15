// claude-app.js -- "Open in Claude app ↗": jump from an agent to the same
// session in Anthropic's Claude app, through Claude Code's Remote Access.
//
// The daemon reads the session link (https://claude.ai/code/…) from the
// agent's /rc footer pill and puts it on the snapshot (state.
// remote_control_url, remote_control_url.go). It is a universal link: a
// plain tap on it opens the Claude app where it is installed, claude.ai in
// the browser otherwise. A peer's page shows it for the agents the peer may
// attach to.
//
// Where no link is known yet, the operator's own page offers to get one:
// "Turn on Remote Access & open" (or, when Remote Access is on but its link
// was not read yet, the same Open entry) switches it on through the existing
// remote-control route, asks the pane for the link until it shows, then
// opens it. A browser opens a tab only inside the tap, so the tab is opened
// at once and pointed at the link when it arrives; where the browser blocks
// that, a toast offers a second Open tap.
//
// dashboard-imperative-boundary: browser-io

export const CLAUDE_SESSION_PREFIX = 'https://claude.ai/code/';
export const OPEN_IN_CLAUDE_APP = 'Open in Claude app ↗';
export const TURN_ON_AND_OPEN = 'Turn on Remote Access & open';

// claudeAppURL is an agent state's session link, only on the exact origin.
export function claudeAppURL(state) {
  const url = state?.remote_control_url;
  return typeof url === 'string' && url.startsWith(CLAUDE_SESSION_PREFIX) ? url : '';
}

// claudeAppOffer is what an agent's menu offers: { kind: 'open', url, label }
// when its link is known; { kind: 'get', turnOn, label } when the page may
// switch its Remote Access on (canToggle: this node's own page) and it is
// running on a harness that has Remote Access; otherwise null.
export function claudeAppOffer(state, { online = false, canRemote = false, canToggle = false } = {}) {
  const url = claudeAppURL(state);
  if (url) return { kind: 'open', url, label: OPEN_IN_CLAUDE_APP };
  if (!online || !canRemote || !canToggle) return null;
  const turnOn = !state?.remote_control;
  return { kind: 'get', turnOn, label: turnOn ? TURN_ON_AND_OPEN : OPEN_IN_CLAUDE_APP };
}

// pageCanToggleRemote is true on this node's own page, where the operator
// switches an agent's Remote Access; a peer's page only opens known links.
export function pageCanToggleRemote(snapshot) {
  return !snapshot?.peer_view && !globalThis.__tclaudeRemoteNode?.id;
}

const delay = (ms) => new Promise((resolve) => { setTimeout(resolve, ms); });

// openInClaudeApp gets agent conv's session link and opens it. Call it from
// the tap itself (before any await): it opens the tab synchronously.
export async function openInClaudeApp({
  conv, label = 'the agent', turnOn = false,
  fetchImpl = (...args) => globalThis.fetch(...args), notify = () => {},
  win = globalThis.window, timeoutMs = 10_000, intervalMs = 1_000,
  sleep = delay, now = () => Date.now(),
}) {
  let tab = null;
  try {
    tab = win?.open?.('', '_blank') || null;
  } catch {
    tab = null;
  }
  if (tab) {
    try {
      tab.document.title = 'Opening the Claude app…';
      tab.document.body.textContent = `Waiting for ${label}'s Remote Access link…`;
    } catch { /* not ours to write: it is navigated below all the same */ }
  }
  notify(turnOn ? `Turning on Remote Access for ${label}…` : `Finding ${label}'s Remote Access link…`);
  const call = async (intent) => {
    const r = await fetchImpl(`/api/agents/${encodeURIComponent(conv)}/remote-control`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ intent }),
    });
    if (!r.ok) throw new Error((await r.text()).trim() || `HTTP ${r.status}`);
    try { return await r.json(); } catch { return {}; }
  };
  let url = '';
  let failure = '';
  try {
    if (turnOn) await call('on');
    const deadline = now() + timeoutMs;
    for (;;) {
      const status = await call('status');
      url = claudeAppURL({ remote_control_url: status.session_url });
      if (url || now() >= deadline) break;
      await sleep(intervalMs);
    }
  } catch (err) {
    failure = err?.message || String(err);
  }
  if (!url) {
    try { tab?.close(); } catch { /* already gone */ }
    const why = failure ? `: ${failure}` : ` within ${Math.round(timeoutMs / 1000)} s`;
    notify(`${turnOn ? 'Remote Access did not come up' : 'No Remote Access link'} for ${label}${why}. Try again, or check its terminal.`, true);
    return { ok: false };
  }
  if (tab && !tab.closed) {
    tab.location.href = url;
    return { ok: true, url };
  }
  // The browser blocked the tab: a second tap opens the link.
  notify(`Remote Access is on for ${label}.`, false, {
    action: { label: OPEN_IN_CLAUDE_APP, run: () => win?.open?.(url, '_blank', 'noopener') },
  });
  return { ok: true, url, deferred: true };
}
