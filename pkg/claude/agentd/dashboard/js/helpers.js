// helpers.js — dashboard leaf module.
//
// DOM shortcuts ($/$$), HTML escaping (esc), relative-time and path
// formatting, and the small pure-ish cell / pill / status-dot / row-
// button builders the dashboard render code shares. Extracted verbatim
// from dashboard.js as the first step of the Stage 2 module split.
// A leaf module: it imports nothing.

const $ = (sel, root) => (root || document).querySelector(sel);
const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}
function shortId(id) { return (id || '').slice(0, 8); }
function onlineDot(online) {
  return online
    ? '<span class="online" title="online">●</span>'
    : '<span class="offline" title="offline">○</span>';
}

// agentStatusDot renders an agent's status light as an interactive
// on/off toggle — the agent's SOLE per-row power control (the
// dedicated wake/shutdown row buttons were removed; the dot replaces
// them). It replaces the plain onlineDot on every row that
// represents a real agent (every group member row). Online = green
// dot whose click turns the agent off; offline = grey dot whose
// click turns it back on (resume). It is a real <button> so it is
// keyboard-reachable (Tab + Enter/Space); the delegated
// data-act="dot-toggle" handler hits /api/agents/{conv}/{stop,resume}.
// An online click always pops the 3-way shutdown confirm first
// (Cancel / Soft exit / Force kill — see the dot-toggle handler); an
// offline click wakes immediately.
function agentStatusDot(m) {
  const label = m.title || m.conv_id;
  const online = !!m.online;
  // An online agent whose last turn ended in an error (CC StopFailure
  // hook → state.status === 'error') gets a red dot. Its CC process is
  // still alive — the dot still toggles it off — but the colour flags
  // that it needs attention. Offline always wins: a dead agent has no
  // process to flag, so it stays grey regardless of its last status.
  const errored = online && m.state && m.state.status === 'error';
  const errDetail = errored ? ((m.state && m.state.status_detail) || 'error') : '';
  let tip;
  if (errored) {
    tip = `errored (${errDetail}) — click to turn off ${label} (asks first: soft exit or force kill)`;
  } else if (online) {
    tip = `online — click to turn off ${label} (asks first: soft exit or force kill)`;
  } else {
    tip = `offline — click to turn on (wake ${label})`;
  }
  let cls;
  if (errored) cls = 'status-dot status-dot-error';
  else if (online) cls = 'status-dot status-dot-online';
  else cls = 'status-dot status-dot-offline';
  const glyph = online ? '●' : '○';
  return `<button type="button" class="${cls}" data-act="dot-toggle"` +
    ` data-conv="${esc(m.conv_id)}" data-label="${esc(label)}"` +
    ` data-online="${online ? '1' : '0'}"` +
    ` title="${esc(tip)}" aria-label="${esc(tip)}">${glyph}</button>`;
}

// statusPillClass mirrors session/list.go's getStatusColorFunc so
// the dashboard's pill colors match the terminal `session ls` output.
function statusPillClass(status) {
  if (!status) return 'state-offline';
  if (status === 'working') return 'state-working';
  if (status === 'main_agent_idle') return 'state-working';
  if (status === 'idle') return 'state-idle';
  if (status === 'awaiting_permission' || status === 'awaiting_input') return 'state-awaiting';
  if (status === 'error') return 'state-error';
  if (status === 'exited') return 'state-exited';
  return 'state-idle';
}

// statePill renders a colored pill for an agent's state. For an
// online agent it combines status + status_detail (e.g. "working:
// Bash"). For an offline agent we ignore state.status entirely (the
// hook-recorded status is frozen at whatever it was when the process
// exited, so echoing it would mislabel a dead agent) and render from
// exit_reason instead: a process that died without a clean exit —
// exit_reason 'unexpected', reaper-stamped because no SessionEnd hook
// fired — shows as "crashed"; every other case (a clean exit, or an
// unknown/blank reason such as a pre-exit_reason corpse) stays a
// plain grey "offline". An unknown reason is never a crash. The
// last-active time, when known, goes in the tooltip.
function statePill(state, online) {
  if (!online) {
    const lh = relTime(state && state.last_hook);
    if (((state && state.exit_reason) || '') === 'unexpected') {
      const tip = 'process ended without a clean exit — crash, kill, or reboot'
        + (lh ? ` · last active ${lh}` : '');
      return `<span class="state-pill state-crashed" title="${esc(tip)}">crashed</span>`;
    }
    const tip = lh ? `offline — last active ${lh}` : 'offline';
    return `<span class="state-pill state-offline" title="${esc(tip)}">offline</span>`;
  }
  const s = (state && state.status) || '';
  const detail = (state && state.status_detail) || '';
  let label = s || 'online';
  if (s && detail) label = `${s}: ${detail}`;
  const cls = statusPillClass(s);
  return `<span class="state-pill ${cls}" title="${esc(label)}">${esc(label)}</span>`;
}

// CTX_SEGMENTS is the block count of the context-window meter — a
// value in the 3-6 design range. 5 splits cleanly into 20%-wide
// bands and leaves room for 2 green / 2 yellow / 1 red.
const CTX_SEGMENTS = 5;

// fmtTokens renders a token count compactly for the meter tooltip:
// 1200 → "1k", 120000 → "120k", 1000000 → "1M".
function fmtTokens(n) {
  n = Number(n) || 0;
  if (n >= 1000000) return (n / 1000000).toFixed(n % 1000000 === 0 ? 0 : 1) + 'M';
  if (n >= 1000) return Math.round(n / 1000) + 'k';
  return String(n);
}

// contextMeterTooltip describes the meter on hover. With real token
// counts it mirrors `tclaude agent context-info` ("X / Y tokens —
// N%"); with only a percentage it falls back to "N% full"; with
// nothing reported it says so plainly.
function contextMeterTooltip(state, pct, known) {
  if (!known) return 'context window: usage not reported yet';
  const tin = Number((state && state.tokens_input) || 0);
  const tout = Number((state && state.tokens_output) || 0);
  const win = Number((state && state.context_window_size) || 0);
  const total = tin + tout;
  if (win > 0 && total > 0) {
    return `context: ${fmtTokens(total)} / ${fmtTokens(win)} tokens — ${Math.round(pct)}%`;
  }
  return `context: ${Math.round(pct)}% full`;
}

// contextMeter renders a vertical segmented gauge of an agent's
// context-window fill. It reads state.context_pct — Claude Code's
// authoritative figure, surfaced by /api/snapshot from the same DB
// row the statusline hook keeps current, so the meter rides on data
// the snapshot already has. Segments fill bottom-up and light by
// band (green low → yellow mid → red high). A freshly-spawned agent
// with no usage record renders a neutral all-dim meter, never a
// broken one.
function contextMeter(state) {
  const pct = Math.max(0, Math.min(100, Number((state && state.context_pct) || 0)));
  const winSize = Number((state && state.context_window_size) || 0);
  const known = pct > 0 || winSize > 0;
  // filled = lit segment count. Round to the nearest block so the
  // meter tracks the true percentage instead of running a block
  // ahead (ceil over-reported — 41% lit 3 of 5). max(1, …) keeps any
  // non-zero usage lighting at least one block; clamped so 100% fills
  // exactly CTX_SEGMENTS. pct == 0 (and the unknown state, which
  // pins pct to 0) lights none.
  const filled = pct > 0
    ? Math.min(CTX_SEGMENTS, Math.max(1, Math.round(pct / (100 / CTX_SEGMENTS))))
    : 0;
  let segs = '';
  for (let i = 0; i < CTX_SEGMENTS; i++) {
    // Band colour by segment position (i=0 is the bottom block,
    // because the flex container is column-reverse). 2 green, 2
    // yellow, 1 red for CTX_SEGMENTS=5.
    let band = 'green';
    if (i >= 4) band = 'red';
    else if (i >= 2) band = 'yellow';
    segs += `<span class="ctx-seg${i < filled ? ' lit-' + band : ''}"></span>`;
  }
  const tip = contextMeterTooltip(state, pct, known);
  return `<span class="ctx-meter${known ? '' : ' ctx-unknown'}" title="${esc(tip)}">${segs}</span>`;
}

// roleCell renders the role column for a member row. Mirrors the CLI:
// members who are also owners get an "owner" badge; pure-owners
// (role==="owner" set by the daemon) show the badge alone.
function roleCell(m) {
  if (m.owner) {
    if (!m.role || m.role === 'owner') {
      return '<span class="owner-badge">owner</span>';
    }
    return `${esc(m.role)} <span class="owner-badge">owner</span>`;
  }
  return esc(m.role || '');
}

// memberActions renders the per-row action button cell. Two
// buttons per row:
//   - Toggle owner: revoke if currently owner, grant if not
//   - Remove from group (destructive)
// Both encode the group + conv via data-* attributes; the
// delegated click handler reads them and dispatches to the
// confirm modal + the right /api endpoint.
function memberActions(g, m) {
  return `<div class="row-actions">${lifecycleAndFocusButtons(m)}${cloneAgentButton(m)}${reincarnateAgentButton(m)}${editMemberButton(g, m)}${ownerToggleButton(g, m)}${sudoMemberButton(m)}${permMemberButton(m)}${cronMemberButton(m)}${removeMemberButton(g, m)}</div>`;
}
// cloneAgentButton renders a "clone" button for any row that
// represents a single agent. Clone forks a sibling that inherits the
// source's identity (groups / perms / ownership). The original keeps
// running.
function cloneAgentButton(m) {
  const label = m.title || m.conv_id;
  const cwd = (m.state && m.state.cwd) || m.cwd || '';
  return `<button data-act="clone" data-conv="${esc(m.conv_id)}" data-label="${esc(label)}" data-cwd="${esc(cwd)}" title="Fork a sibling that inherits identity (groups, perms, ownership). The original keeps running.">clone</button>`;
}
// reincarnateAgentButton renders a "reincarnate" button for any row
// that represents a single agent. The modal it opens defaults to
// asking the agent to reincarnate ITSELF (it writes its own handoff);
// a force mode does the immediate daemon-driven reincarnation.
function reincarnateAgentButton(m) {
  const label = m.title || m.conv_id;
  return `<button data-act="reincarnate" data-conv="${esc(m.conv_id)}" data-label="${esc(label)}" title="Reincarnate this agent — by default ask it to do so itself (it writes its own handoff); or force an immediate daemon-driven reincarnation.">reincarnate</button>`;
}
function sudoMemberButton(m) {
  return `<button data-act="sudo-grant" data-conv="${esc(m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" title="Grant a time-bounded sudo elevation to this agent">+ sudo</button>`;
}
// permMemberButton renders the per-row "permissions" affordance —
// opens the permanent-permission editor (grant / deny / default per
// slug). The permanent twin of "+ sudo" right beside it.
function permMemberButton(m) {
  return `<button data-act="perm-edit" data-conv="${esc(m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" title="Edit this agent's permanent permissions (grant / deny / inherit-default)">permissions</button>`;
}
// cronMemberButton renders the ⏰ "schedule a nudge for this member"
// button. Opens the cron-create modal prefilled with Solo target =
// this conv-id, and Owner = this conv-id too (self-nudge is the
// common case from member rows).
function cronMemberButton(m) {
  const label = m.title || m.conv_id;
  const prefill = JSON.stringify({
    targetMode: 'solo',
    target: m.conv_id,
    owner: m.conv_id,
  });
  return `<button data-act="cron-new" data-prefill="${esc(prefill)}" data-label="${esc(label)}" title="Schedule a recurring nudge for ${esc(label)}">⏰</button>`;
}

// termButton renders the "open a terminal in this agent's working
// directory" affordance. Shown whether the agent is online or not —
// the directory is known from the DB regardless of whether the
// agent's tmux pane is currently alive.
function termButton(m) {
  const label = m.title || m.conv_id;
  return `<button data-act="term" data-conv="${esc(m.conv_id)}" data-label="${esc(label)}" title="Open a terminal in this agent's working directory">term</button>`;
}

// lifecycleAndFocusButtons renders the focus/hide/term cluster. focus
// and hide are the window pair, online-only (an offline agent has no
// window to raise or detach): focus raises the agent's terminal
// window, hide detaches it (the per-agent twin of the "windows"
// button's bulk unfocus). term is always present. Powering the agent
// up/down has no button here — the far-left status dot
// (agentStatusDot) is the agent's power control: an offline dot
// resumes, an online dot opens the soft/force shutdown confirm. Used
// by both real-group member rows and the virtual Ungrouped group's
// rows so the surface is identical in both.
function lifecycleAndFocusButtons(m) {
  const label = m.title || m.conv_id;
  if (m.online) {
    return [
      `<button data-act="jump" data-conv="${esc(m.conv_id)}" data-label="${esc(label)}" title="Focus this agent's terminal window">focus</button>`,
      `<button data-act="hide" data-conv="${esc(m.conv_id)}" data-label="${esc(label)}" title="Hide this agent's terminal window — detaches its tmux client. The agent keeps running.">hide</button>`,
      termButton(m),
    ].join('');
  }
  return termButton(m);
}

// editMemberButton renders the per-agent "edit" button — the single
// panel for editing an agent: its title (incl. the "auto" self-rename),
// its group role and its group description. data-current carries the
// title so the modal opens pre-filled.
function editMemberButton(g, m) {
  return `<button data-act="edit-member" data-group="${esc(g.name)}" data-conv="${esc(m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" data-current="${esc(m.title || '')}" data-role="${esc(m.role || '')}" data-descr="${esc(m.descr || '')}" title="Edit this agent — title, role, description">edit</button>`;
}
function ownerToggleButton(g, m) {
  return m.owner
    ? `<button class="warn" data-act="revoke-owner" data-group="${esc(g.name)}" data-conv="${esc(m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" title="Revoke owner status">revoke owner</button>`
    : `<button data-act="grant-owner" data-group="${esc(g.name)}" data-conv="${esc(m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" title="Make this conv an owner of the group">make owner</button>`;
}
function removeMemberButton(g, m) {
  return `<button class="danger" data-act="remove-member" data-group="${esc(g.name)}" data-conv="${esc(m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" title="Remove from group">remove</button>`;
}

// ungroupedMemberActions renders the per-row action cell for a row
// in the virtual "Ungrouped" group. It deliberately OMITS every
// group-affecting button (the edit panel, owner toggle,
// remove-from-group) — the agent belongs to no group, so those are
// meaningless here. What remains is the agent-level lifecycle set,
// identical to a grouped member's lifecycle set: focus / term,
// clone, reincarnate, sudo, self-nudge cron, delete. Powering the
// agent up/down is the status dot's job; renaming is the
// click-to-edit name cell, available on every row. To put an
// ungrouped agent INTO a group, drag its row onto a group header.
function ungroupedMemberActions(m) {
  return `<div class="row-actions">${lifecycleAndFocusButtons(m)}${cloneAgentButton(m)}${reincarnateAgentButton(m)}${sudoMemberButton(m)}${permMemberButton(m)}${cronMemberButton(m)}<button class="danger" data-act="delete-agent" data-conv="${esc(m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" title="Permanently delete this conversation">delete</button></div>`;
}

// relTime renders an ISO timestamp as a coarse "Ns/m/h ago" string.
// Mirrors the session ls UPDATED column. Empty input → "" (no chip).
function relTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d)) return '';
  const sec = Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000));
  if (sec < 60) return sec + 's ago';
  if (sec < 3600) return Math.floor(sec / 60) + 'm ago';
  if (sec < 86400) return Math.floor(sec / 3600) + 'h ago';
  return Math.floor(sec / 86400) + 'd ago';
}

// shortCwd renders an absolute path compactly for table cells.
// Replaces the home prefix with `~` and, if the result still exceeds
// ~40 chars, truncates from the LEFT — `…/git/tclaude` is far more
// useful than `/home/gigur/git/tcla…` because most paths share a
// long common prefix and the distinguishing detail is the tail.
// Empty / unknown input renders as an em dash so the column stays
// visually consistent.
function shortCwd(cwd) {
  if (!cwd) return '—';
  const home = (cwd.match(/^\/(?:home|Users)\/[^/]+/) || [''])[0];
  let out = (home && cwd.startsWith(home)) ? '~' + cwd.slice(home.length) : cwd;
  const cap = 40;
  if (out.length > cap) {
    out = '…' + out.slice(out.length - (cap - 1));
  }
  return out;
}

// stackedLoc renders a startup-vs-current pair of pre-formatted HTML
// cells. When they agree it shows a single line; when they diverge
// it stacks an "init" / "now" pair so the CWD and Branch columns
// stay narrow — the agent's launch location and where it's actually
// working sit on two short rows rather than two extra columns.
function stackedLoc(startHTML, curHTML, differ) {
  if (!differ) return curHTML || startHTML;
  return '<div class="loc-pair">'
    + `<span class="loc-row"><span class="loc-tag">init</span>${startHTML}</span>`
    + `<span class="loc-row"><span class="loc-tag">now</span>${curHTML}</span>`
    + '</div>';
}

// cwdCell renders the CWD column: the launch dir, or — when the
// agent has moved into a sub-repo / worktree — a stacked init/now
// pair. startup_dir falls back to the live session's cwd. Each path
// is a click-to-open-a-terminal target: the launch dir maps to the
// `start` /api/term selector, the live worktree to `worktree` —
// the two selectors that resolve to those exact directories.
function cwdCell(m) {
  const startup = m.startup_dir || (m.state || {}).cwd || '';
  const current = m.current_dir || '';
  const conv = m.conv_id || '';
  const fmt = (d, which) => {
    if (!d) return '<span class="cwd">—</span>';
    return `<span class="cwd cwd-link" data-act="term-dir" data-conv="${esc(conv)}" data-which="${which}" title="Open a terminal here — ${esc(d)}">${esc(shortCwd(d))}</span>`;
  };
  const differ = !!current && !!startup && current !== startup;
  return stackedLoc(fmt(startup, 'start'), fmt(current, 'worktree'), differ);
}

// branchCell renders the Branch column. `m.branch` is the agent's
// *current* branch (the worktree it last edited in); startup_branch
// is the launch dir's branch — empty for a virtual-monorepo launch
// dir. They stack as init/now whenever they differ. When the
// snapshot resolved a GitHub repo, the branch name becomes a link to
// its compare view and an open PR is appended as a `#<num>` link.
// Empty / unknown renders as an em dash so the column stays aligned.
function branchCell(m) {
  const fmt = (branch, url, prNum, prURL) => {
    if (!branch) return '<span class="muted">—</span>';
    const inner = `⎇ ${esc(branch)}`;
    const branchEl = url
      ? `<a class="branch branch-link" href="${esc(url)}" target="_blank" rel="noopener noreferrer" draggable="false" title="Open branch on GitHub — ${esc(branch)}">${inner}</a>`
      : `<span class="branch" title="git branch: ${esc(branch)}">${inner}</span>`;
    const prEl = (prNum && prURL)
      ? ` <a class="pr-link" href="${esc(prURL)}" target="_blank" rel="noopener noreferrer" draggable="false" title="Open pull request #${prNum}">#${prNum}</a>`
      : '';
    return branchEl + prEl;
  };
  const startupEl = fmt(m.startup_branch || '', m.startup_branch_url || '', m.startup_pr_number || 0, m.startup_pr_url || '');
  const currentEl = fmt(m.branch || '', m.branch_url || '', m.branch_pr_number || 0, m.branch_pr_url || '');
  return stackedLoc(startupEl, currentEl, (m.startup_branch || '') !== (m.branch || ''));
}

// offlineDefault returns the tab-wide "show offline" checkbox state
// for the 'groups' tab. Defaults to true (show everything) when
// the checkbox isn't in the DOM yet / the user hasn't touched it.
function offlineDefault(tab) {
  const el = $(`#filter-${tab}-offline`);
  return el ? el.checked : true;
}

// groupOfflineOverride: per-group override — 'show', 'hide', or
// 'inherit' (no override; follows the tab-wide checkbox). Persisted
// in localStorage keyed by group name so it survives reloads.
function groupOfflineOverride(name) {
  const v = localStorage.getItem('tclaude.dash.group.offline.' + name);
  return (v === 'show' || v === 'hide') ? v : 'inherit';
}

// groupShowOffline: effective decision for one group — the override
// when set, else the tab-wide Groups default.
function groupShowOffline(name) {
  const ov = groupOfflineOverride(name);
  if (ov === 'show') return true;
  if (ov === 'hide') return false;
  return offlineDefault('groups');
}

// groupOfflineToggleHTML renders the per-group offline-visibility
// control shown in the group <summary>. Clicking cycles
// inherit → show → hide (handled by the cycle-group-offline
// data-act case). In inherit mode it spells out the effective
// value so the human can see what the tab default resolves to.
function groupOfflineToggleHTML(name) {
  const override = groupOfflineOverride(name);
  let label, cls = 'group-offline-toggle';
  if (override === 'inherit') {
    label = `offline: auto (${groupShowOffline(name) ? 'shown' : 'hidden'})`;
    cls += ' inherit';
  } else {
    label = override === 'show' ? 'offline: shown' : 'offline: hidden';
  }
  return `<span class="${cls}" data-act="cycle-group-offline" data-group="${esc(name)}" data-label="${esc(name)}" title="Per-group offline visibility — click to cycle: inherit tab default → always show → always hide">${esc(label)}</span>`;
}

// Public API — the helpers used outside this module. The rest
// (statusPillClass, fmtTokens, contextMeterTooltip, the per-row button
// builders, lifecycleAndFocusButtons, stackedLoc) are internal
// composition details of the exported builders above.
export {
  $, $$, esc, shortId, onlineDot, agentStatusDot, statePill, contextMeter,
  roleCell, memberActions, ungroupedMemberActions, relTime, shortCwd,
  cwdCell, branchCell, offlineDefault, groupOfflineOverride, groupShowOffline,
  groupOfflineToggleHTML,
};
