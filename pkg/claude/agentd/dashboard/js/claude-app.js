// claude-app.js -- "Open in Claude app ↗": jump from an agent to the same
// session in Anthropic's Claude app, through Claude Code's Remote Access.
//
// Claude Code writes the session link (https://claude.ai/code/…) into the
// agent's transcript each time Remote Access comes up; the daemon's
// transcript follower records it on the launch's session row and the
// snapshot carries it (state.remote_control_url, remote_control_url.go). It
// is a universal link: a plain tap on it opens the Claude app where it is
// installed, claude.ai in the browser otherwise. A peer's page shows it for
// the agents the peer may attach to.
//
// Where no link is known yet, the operator's own page offers to get one:
// "Turn on Remote Access & open" (or, when Remote Access is on but no link
// was recorded yet, the same Open entry) asks the remote-control route for
// intent "open": it switches Remote Access on, or, when it is already on
// with no link coming for this launch, off and on again so Claude Code
// announces a fresh one. Then it asks the daemon for the recorded link
// (intent "link": no pane capture) until the follower has it, then offers it: a
// toast with Open in Claude app ↗, a second tap. The second tap is
// deliberate. A tab opened in the first tap and pointed at the link later
// fails on phones: iOS suspends the page that would point it while that tab
// is in front, and a universal link opens the app only from a tap, not from
// a script's navigation. The menu shows the link itself from then on.
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

// openInClaudeApp gets agent conv's session link (switching Remote Access
// on first when turnOn) and offers it in a toast whose Open tap opens it.
export async function openInClaudeApp({
  conv, label = 'the agent', turnOn = false,
  fetchImpl = (...args) => globalThis.fetch(...args), notify = () => {},
  win = globalThis.window, timeoutMs = 30_000, intervalMs = 1_000,
  sleep = delay, now = () => Date.now(),
}) {
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
    await call('open');
    const deadline = now() + timeoutMs;
    for (;;) {
      const status = await call('link');
      url = claudeAppURL({ remote_control_url: status.session_url });
      if (url || now() >= deadline) break;
      await sleep(intervalMs);
    }
  } catch (err) {
    failure = err?.message || String(err);
  }
  if (!url) {
    const why = failure ? `: ${failure}` : ` within ${Math.round(timeoutMs / 1000)} s`;
    notify(`${turnOn ? 'Remote Access did not come up' : 'No Remote Access link'} for ${label}${why}. Try again, or check its terminal.`, true);
    return { ok: false };
  }
  notify(`Remote Access is on for ${label}.`, false, {
    action: { label: OPEN_IN_CLAUDE_APP, run: () => win?.open?.(url, '_blank', 'noopener') },
  });
  return { ok: true, url };
}
