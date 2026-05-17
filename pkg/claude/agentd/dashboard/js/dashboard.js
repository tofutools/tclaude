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
  // on/off toggle. It replaces the plain onlineDot on every row that
  // represents a real agent (every group member row). Online =
  // green dot whose click turns the agent off (soft /exit); offline =
  // grey dot whose click turns it back on (resume). It is a real
  // <button> so it is keyboard-reachable (Tab + Enter/Space); the
  // delegated data-act="dot-toggle" handler reuses the very same
  // /api/agents/{conv}/{stop,resume} endpoints the "shut down" / "wake"
  // row buttons hit — no parallel endpoint. An online click always
  // pops a confirm dialog first (see the dot-toggle handler); an
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
      tip = `errored (${errDetail}) — click to turn off (asks first, then soft-stops ${label})`;
    } else if (online) {
      tip = `online — click to turn off (asks first, then soft-stops ${label})`;
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

  // groupTagsCell renders the Groups column on the Agents view. Each
  // group becomes a tag; groups the agent owns get an "owner" badge
  // appended to the tag so the manager-vs-member distinction is
  // visible at a glance.
  function groupTagsCell(a) {
    const groups = a.groups || [];
    if (!groups.length) return '<span class="muted">(none)</span>';
    const owned = new Set(a.owned_groups || []);
    return groups.map(g => {
      const ownBadge = owned.has(g) ? '<span class="owner-badge">owner</span>' : '';
      return `<span class="tag">${esc(g)}${ownBadge}</span>`;
    }).join(' ');
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
    return `<div class="row-actions">${lifecycleAndFocusButtons(m)}${cloneAgentButton(m)}${reincarnateAgentButton(m)}${renameAgentButton(m)}${editMemberButton(g, m)}${ownerToggleButton(g, m)}${sudoMemberButton(m)}${permMemberButton(m)}${cronMemberButton(m)}${removeMemberButton(g, m)}</div>`;
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
  // renameAgentButton renders a "rename" button for any row that
  // represents a single agent. Opens a modal where the user can type
  // an explicit title or check "auto" to ask the agent to pick one
  // for itself via the agent-rename skill / CLI.
  function renameAgentButton(m) {
    const label = m.title || m.conv_id;
    const current = m.title || '';
    return `<button data-act="rename-agent" data-conv="${esc(m.conv_id)}" data-label="${esc(label)}" data-current="${esc(current)}" title="Change this conversation's title. Type one or check 'auto' to let the agent pick.">rename</button>`;
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

  // lifecycleAndFocusButtons renders the focus/term/wake/shutdown
  // cluster. focus + shutdown vs wake are mutually exclusive on online
  // state so the row stays visually stable as the agent toggles; term
  // is always present. Used by both real-group member rows and the
  // virtual Ungrouped group's rows so the surface is identical in both.
  function lifecycleAndFocusButtons(m) {
    const label = m.title || m.conv_id;
    if (m.online) {
      return [
        `<button data-act="jump" data-conv="${esc(m.conv_id)}" data-label="${esc(label)}" title="Focus this agent's terminal window">focus</button>`,
        termButton(m),
        `<button data-act="shutdown-agent" data-conv="${esc(m.conv_id)}" data-label="${esc(label)}" title="Soft-exit this agent (force kill available in confirm)">shut down</button>`,
      ].join('');
    }
    return termButton(m) +
      `<button data-act="wake-agent" data-conv="${esc(m.conv_id)}" data-label="${esc(label)}" title="Wake this agent — spawns a tmux session resumed onto its conv">wake</button>`;
  }

  function editMemberButton(g, m) {
    return `<button data-act="edit-member" data-group="${esc(g.name)}" data-conv="${esc(m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" data-role="${esc(m.role || '')}" data-descr="${esc(m.descr || '')}" title="Edit role / description">edit</button>`;
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
  // group-affecting button (edit role/descr, owner toggle,
  // remove-from-group) — the agent belongs to no group, so those are
  // meaningless here. What remains is the agent-level lifecycle set,
  // identical to a grouped member's lifecycle set: focus / term / shut down / wake,
  // clone, reincarnate, rename, sudo, self-nudge cron, delete. To put
  // an ungrouped agent INTO a group, drag its row onto a group header.
  function ungroupedMemberActions(m) {
    return `<div class="row-actions">${lifecycleAndFocusButtons(m)}${cloneAgentButton(m)}${reincarnateAgentButton(m)}${renameAgentButton(m)}${sudoMemberButton(m)}${permMemberButton(m)}${cronMemberButton(m)}<button class="danger" data-act="delete-agent" data-conv="${esc(m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" title="Permanently delete this conversation">delete</button></div>`;
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

  // --- column sorting --------------------------------------------------
  // Every primary table (group members, cron, sudo, links) has
  // clickable headers. The active sort — a {col, dir} pair keyed by a
  // stable table name — lives in sortState and is mirrored to
  // localStorage so it survives reloads and the 5s auto-refresh.
  // Clicking a header cycles asc → desc → unsorted; the third click
  // drops back to the server's own ordering.
  const SORT_LS_KEY = 'tclaude.dash.sort';
  let sortState = {};
  try { sortState = JSON.parse(localStorage.getItem(SORT_LS_KEY)) || {}; }
  catch (_) { sortState = {}; }

  // cycleSort advances one table's sort through the three-state cycle
  // and persists the result.
  function cycleSort(tableKey, col) {
    const cur = sortState[tableKey];
    if (!cur || cur.col !== col) {
      sortState[tableKey] = { col, dir: 'asc' };
    } else if (cur.dir === 'asc') {
      sortState[tableKey] = { col, dir: 'desc' };
    } else {
      delete sortState[tableKey];
    }
    try { localStorage.setItem(SORT_LS_KEY, JSON.stringify(sortState)); }
    catch (_) { /* private-mode / quota — sort still works in-memory */ }
  }

  // sortHead builds a table's <thead> from a column spec. Each spec
  // entry is {label, col}; entries without a `col` (the online-dot and
  // row-action columns) render as plain, non-clickable headers. The
  // active column shows a solid ▲/▼; the rest carry a faint arrow that
  // surfaces on hover, hinting they're clickable.
  function sortHead(tableKey, cols) {
    const active = sortState[tableKey];
    const cells = cols.map(c => {
      if (!c.col) return `<th>${esc(c.label || '')}</th>`;
      const on = active && active.col === c.col;
      const arrow = on ? (active.dir === 'asc' ? '▲' : '▼') : '▾';
      const cls = on ? 'sortable sort-active' : 'sortable';
      return `<th class="${cls}" data-sort-table="${esc(tableKey)}" `
           + `data-sort-col="${esc(c.col)}" title="Sort by ${esc(c.label)}">`
           + `${esc(c.label)}<span class="sort-arrow">${arrow}</span></th>`;
    });
    return `<thead><tr>${cells.join('')}</tr></thead>`;
  }

  // cmpSortValues orders two non-empty accessor outputs: booleans and
  // numbers compare naturally, everything else as case-insensitive
  // strings (ISO timestamps included — lexical order is chronological).
  function cmpSortValues(a, b) {
    if (typeof a === 'boolean' || typeof b === 'boolean') {
      return (a === b) ? 0 : (a ? -1 : 1);
    }
    if (typeof a === 'number' && typeof b === 'number') {
      return a - b;
    }
    return String(a).toLowerCase().localeCompare(String(b).toLowerCase());
  }

  // applySort returns a sorted copy of `rows` for the given table.
  // With no active sort (or an accessor the table doesn't define) the
  // original array is handed back untouched, preserving server order.
  // Blank/nullish cells always sort last, whichever the direction, so
  // empty values never crowd the top.
  function applySort(tableKey, rows, accessors) {
    const st = sortState[tableKey];
    if (!st || !accessors || !accessors[st.col]) return rows;
    const get = accessors[st.col];
    const sign = st.dir === 'desc' ? -1 : 1;
    return rows.slice().sort((ra, rb) => {
      const a = get(ra), b = get(rb);
      const ae = (a == null || a === ''), be = (b == null || b === '');
      if (ae || be) return ae === be ? 0 : (ae ? 1 : -1);
      return sign * cmpSortValues(a, b);
    });
  }

  // Column specs + value accessors for each sortable table. The `col`
  // strings are opaque keys shared between the header (sortHead) and
  // the sorter (applySort); they need not match the data field name.
  const MEMBER_COLS = [
    { label: '' },
    { label: 'ID', col: 'id' },
    { label: 'Name', col: 'title' },
    { label: 'State', col: 'state' },
    { label: 'Last', col: 'last' },
    { label: 'CWD', col: 'cwd' },
    { label: 'Branch', col: 'branch' },
    { label: 'Role', col: 'role' },
    { label: 'Description', col: 'descr' },
    { label: '' },
  ];
  const MEMBER_ACCESSORS = {
    id:     m => m.conv_id,
    title:  m => m.title,
    state:  m => (m.state || {}).status,
    last:   m => (m.state || {}).last_hook,
    cwd:    m => m.current_dir || (m.state || {}).cwd,
    branch: m => m.branch,
    role:   m => m.role,
    descr:  m => m.descr,
  };

  const CRON_COLS = [
    { label: '' },
    { label: 'ID', col: 'id' },
    { label: 'Name', col: 'name' },
    { label: 'Owner', col: 'owner' },
    { label: 'Target', col: 'target' },
    { label: 'Every', col: 'every' },
    { label: 'Last run', col: 'last' },
    { label: 'Status', col: 'status' },
    { label: 'Body', col: 'body' },
    { label: '' },
  ];
  const CRON_ACCESSORS = {
    id:     j => j.id,
    name:   j => j.name,
    owner:  j => j.owner_label || j.owner_conv,
    target: j => j.group_name || j.target_label || j.target_conv,
    every:  j => j.interval_seconds,
    last:   j => j.last_run_at,
    status: j => j.last_run_status,
    body:   j => j.body,
  };

  const SUDO_COLS = [
    { label: 'Conv', col: 'conv' },
    { label: 'Slug', col: 'slug' },
    { label: 'Granted at', col: 'granted' },
    { label: 'Expires in', col: 'expires' },
    { label: 'Reason', col: 'reason' },
    { label: 'Granted by', col: 'by' },
    { label: '' },
  ];
  const SUDO_ACCESSORS = {
    conv:    r => r.conv_title || r.conv_id,
    slug:    r => r.slug,
    granted: r => r.granted_at,
    expires: r => r.remaining_seconds,
    reason:  r => r.reason,
    by:      r => r.granted_by,
  };

  const LINK_COLS = [
    { label: 'ID', col: 'id' },
    { label: 'From', col: 'from' },
    { label: '' },
    { label: 'To', col: 'to' },
    { label: 'Mode', col: 'mode' },
    { label: 'Created', col: 'created' },
    { label: '' },
  ];
  const LINK_ACCESSORS = {
    id:      l => l.id,
    from:    l => l.from,
    to:      l => l.to,
    mode:    l => l.mode,
    created: l => l.created_at,
  };

  // The virtual "Ungrouped" group. UNGROUPED_LABEL is the display
  // name; UNGROUPED_VKEY is its identity for localStorage / DOM keying.
  // The key has a leading space, which validateGroupName (server-side)
  // rejects — so it can never collide with a real group's name, even
  // if a human creates a real group literally called "Ungrouped".
  const UNGROUPED_LABEL = 'Ungrouped';
  const UNGROUPED_VKEY = ' ungrouped-virtual';

  // virtualUngroupedGroup builds the synthetic group object from the
  // snapshot's groupless agents. Always returns the object (with an
  // empty members[] when there are none) — once the human has ticked
  // "show ungrouped" the group stays visible even while empty, so it
  // reads as a stable, discoverable drop target rather than something
  // that blinks in and out as agents come and go. The `virtual` flag
  // is the discriminator every group-affecting code path keys off to
  // suppress rename / delete / multicast / cron / add-member.
  //
  // Online AND offline agents are kept: ungrouped[] carries both (a
  // freshly-promoted offline conversation belongs here so it can be
  // dragged into a group). renderVirtualGroup applies the "show
  // offline" filter at render time, like a real group.
  function virtualUngroupedGroup(agents) {
    const rows = (agents || []).slice();
    return {
      name: UNGROUPED_LABEL,
      key: UNGROUPED_VKEY,
      virtual: true,
      descr: 'agents not in any group',
      members: rows,
      online: rows.filter(a => a.online).length,
    };
  }

  // ungroupedVisible reports the "show ungrouped" checkbox state.
  // Defaults to true when the checkbox isn't in the DOM yet.
  function ungroupedVisible() {
    const el = $('#filter-groups-ungrouped');
    return el ? el.checked : true;
  }

  // The virtual "Conversations" group — non-agent conversations,
  // surfaced in the Groups tab so a raw conversation can be dragged
  // into a group (which promotes it) without leaving the tab.
  const CONVERSATIONS_LABEL = 'Conversations';
  const CONVERSATIONS_VKEY = ' conversations-virtual';

  // virtualConversationsGroup builds the synthetic group from the
  // snapshot's conversations[] (recent non-enrolled convs). The
  // `conversations` flag is the discriminator renderGroups keys off to
  // pick the lighter conversation-row renderer.
  function virtualConversationsGroup(convs) {
    const rows = (convs || []).slice();
    return {
      name: CONVERSATIONS_LABEL,
      key: CONVERSATIONS_VKEY,
      virtual: true,
      conversations: true,
      descr: 'recent conversations that are not agents',
      members: rows,
      online: rows.filter(c => c.online).length,
    };
  }

  // conversationsVisible reports the "show conversations" checkbox
  // state. Defaults to false — there can be a lot of conversations, so
  // the virtual group is opt-in (unlike "show ungrouped").
  function conversationsVisible() {
    const el = $('#filter-groups-conversations');
    return el ? el.checked : false;
  }

  // The virtual "Retired" group — agents that were demoted back to
  // plain conversations (retire). Surfaced in the Groups tab so a
  // retired agent doesn't silently vanish off the tab: it lands here
  // and can be reinstated in place.
  const RETIRED_LABEL = 'Retired';
  const RETIRED_VKEY = ' retired-virtual';

  // virtualRetiredGroup builds the synthetic group from the snapshot's
  // retired[] rows. The `retired` flag is the discriminator renderGroups
  // keys off to pick the retired-row renderer.
  function virtualRetiredGroup(retired) {
    const rows = (retired || []).slice();
    return {
      name: RETIRED_LABEL,
      key: RETIRED_VKEY,
      virtual: true,
      retired: true,
      descr: 'agents demoted back to plain conversations',
      members: rows,
      online: rows.filter(r => r.online).length,
    };
  }

  // retiredVisible reports the "show retired" checkbox state. Defaults
  // to true when the checkbox isn't in the DOM yet — a retired agent
  // must not silently disappear from the Groups tab.
  function retiredVisible() {
    const el = $('#filter-groups-retired');
    return el ? el.checked : true;
  }

  // memberRowHTML renders one draggable member <tr>. `ctx` selects the
  // drag wiring + action set:
  //   - {group: <groupObj>}  — a real group member: the source group
  //     is recorded for drag-and-drop; full memberActions (incl. owner
  //     toggle / remove-from-group).
  //   - {ungrouped: true}    — a row in the virtual Ungrouped group:
  //     tagged as an ungrouped drag source; agent-level actions only.
  function memberRowHTML(m, ctx) {
    const state = m.state || {};
    const subagents = state.subagent_count || 0;
    const dndSource = ctx.ungrouped
      ? 'data-dnd-source-ungrouped="1"'
      : `data-dnd-source-group="${esc(ctx.group.name)}"`;
    const actions = ctx.ungrouped ? ungroupedMemberActions(m) : memberActions(ctx.group, m);
    return `
                <tr class="dnd-draggable" draggable="true" ${dndSource}
                    data-dnd-conv="${esc(m.conv_id)}"
                    data-dnd-label="${esc(m.title || m.conv_id)}">
                  <td>${agentStatusDot(m)}</td>
                  <td class="id">${esc(shortId(m.conv_id))}</td>
                  <td>
                    <div class="rowname">${esc(m.title || '(unnamed)')}${sudoBadge(sudoByConv[m.conv_id], m.conv_id)}</div>
                  </td>
                  <td class="state-cell">
                    ${contextMeter(state)}
                    ${statePill(state, m.online)}
                    ${subagents > 0 ? `<span class="state-detail">+${subagents}</span>` : ''}
                  </td>
                  <td><span class="last-hook">${esc(relTime(state.last_hook))}</span></td>
                  <td>${cwdCell(m)}</td>
                  <td>${branchCell(m)}</td>
                  <td>${roleCell(m)}</td>
                  <td class="muted">${esc(m.descr || '')}</td>
                  <td>${actions}</td>
                </tr>`;
  }

  // renderVirtualGroup renders the synthetic "Ungrouped" group. It is
  // intentionally inert AS A GROUP — no rename / delete / multicast /
  // cron / add-member / spawn buttons, no default-cwd / default-context.
  // Its only interactive role is drag-and-drop:
  //   - member rows are draggable INTO real groups (→ join);
  //   - the summary is a drop target for real-group members (→ leave
  //     that group; if it was their only group they reappear here).
  // The summary carries data-dnd-target-ungrouped (NOT -target-group),
  // so the move/clone code paths can never mistake it for a real group.
  //
  // Offline visibility: the group honours the tab-wide "show offline"
  // checkbox (it has no per-group override). Hidden members still
  // count toward the header total so it stays truthful.
  function renderVirtualGroup(g) {
    const members = g.members || [];
    const visible = offlineDefault('groups') ? members : members.filter(m => m.online);
    const hiddenOffline = members.length - visible.length;
    const key = g.key || g.name;
    const isOpen = localStorage.getItem('tclaude.dash.group.' + key) === '1';
    const body = members.length === 0
      ? '<div class="muted">(no ungrouped agents)</div>'
      : visible.length === 0
      ? `<div class="muted">(${hiddenOffline} offline agent${hiddenOffline === 1 ? '' : 's'} hidden — toggle "show offline" to see ${hiddenOffline === 1 ? 'it' : 'them'})</div>`
      : `
          <table>
            ${sortHead('members', MEMBER_COLS)}
            <tbody>
              ${applySort('members', visible, MEMBER_ACCESSORS).map(m => memberRowHTML(m, {ungrouped: true})).join('')}
            </tbody>
          </table>`;
    return `
      <details class="group-virtual" data-group-key="${esc(key)}"${isOpen ? ' open' : ''}>
        <summary data-dnd-target-ungrouped="1">
          <strong class="group-name">${esc(g.name)}</strong>
          <span class="group-virtual-badge" title="A virtual group, not a real one — it can't be renamed, deleted, messaged or scheduled. It just collects agents that aren't in any group.">virtual</span>
          <span class="muted">— ${members.length} agent${members.length === 1 ? '' : 's'} not in any group${hiddenOffline > 0 ? ` · ${hiddenOffline} offline hidden` : ''}</span>
          <span class="muted group-virtual-hint">drag a row onto a group to add it · drag a group member here to remove it</span>
        </summary>
        <div class="subtable">
          ${body}
        </div>
      </details>
    `;
  }

  // renderVirtualConversationsGroup renders the synthetic
  // "Conversations" group — non-agent conversations. Like the virtual
  // Ungrouped group it is inert AS A GROUP, but it IS a drag source:
  // its rows (data-dnd-source-conversation) drag onto a real group →
  // promote + join, or onto the Ungrouped header → promote with no
  // group. It is NOT a drop target — retiring an agent is done by
  // dragging it onto the "Retired" group instead.
  function renderVirtualConversationsGroup(g) {
    const members = g.members || [];
    const key = g.key || g.name;
    const isOpen = localStorage.getItem('tclaude.dash.group.' + key) === '1';
    const body = members.length === 0
      ? '<div class="muted">(no non-agent conversations)</div>'
      : `
          <table>
            <thead><tr><th></th><th>conv</th><th>title</th><th>last activity</th><th></th></tr></thead>
            <tbody>
              ${members.map(c => `
                <tr class="dnd-draggable" draggable="true" data-dnd-source-conversation="1"
                    data-dnd-conv="${esc(c.conv_id)}"
                    data-dnd-label="${esc(c.title || c.conv_id)}">
                  <td>${onlineDot(c.online)}</td>
                  <td class="id">${esc(shortId(c.conv_id))}</td>
                  <td><span class="rowname">${esc(c.title || '(untitled)')}</span></td>
                  <td><span class="last-hook">${esc(c.modified ? relTime(c.modified) : '')}</span></td>
                  <td><div class="row-actions"><button class="primary" data-act="promote-agent" data-conv="${esc(c.conv_id)}" data-label="${esc(c.title || c.conv_id)}" title="Promote this conversation into an agent">promote</button></div></td>
                </tr>`).join('')}
            </tbody>
          </table>`;
    return `
      <details class="group-virtual" data-group-key="${esc(key)}"${isOpen ? ' open' : ''}>
        <summary>
          <strong class="group-name">${esc(g.name)}</strong>
          <span class="group-virtual-badge" title="A virtual group, not a real one — recent conversations that aren't agents. Drag one onto a group, or click promote, to make it an agent.">virtual</span>
          <span class="muted">— ${members.length} conversation${members.length === 1 ? '' : 's'} that aren't agents</span>
          <span class="muted group-virtual-hint">drag a row onto a group to promote + add it</span>
        </summary>
        <div class="subtable">
          ${body}
        </div>
      </details>
    `;
  }

  // renderVirtualRetiredGroup renders the synthetic "Retired" group —
  // agents demoted back to plain conversations. Like the virtual
  // Conversations group it is inert AS A GROUP, but it IS a drag source
  // AND a drag target:
  //   - its summary (data-dnd-target-retired) accepts an agent row →
  //     retire it, demoting the agent back to a plain conversation;
  //   - its rows (data-dnd-source-retired) drag onto a real group →
  //     reinstate + join, or onto the Ungrouped header → reinstate with
  //     no group.
  // It is the landing surface so a just-retired agent stays visible on
  // the tab. Each row also keeps the per-row "reinstate" button.
  function renderVirtualRetiredGroup(g) {
    const members = g.members || [];
    const key = g.key || g.name;
    const isOpen = localStorage.getItem('tclaude.dash.group.' + key) === '1';
    const body = members.length === 0
      ? '<div class="muted">(no retired agents)</div>'
      : `
          <table>
            <thead><tr><th></th><th>conv</th><th>title</th><th>retired</th><th>by</th><th>reason</th><th></th></tr></thead>
            <tbody>
              ${members.map(a => `
                <tr class="dnd-draggable" draggable="true" data-dnd-source-retired="1"
                    data-dnd-conv="${esc(a.conv_id)}"
                    data-dnd-label="${esc(a.title || a.conv_id)}">
                  <td>${onlineDot(a.online)}</td>
                  <td class="id">${esc(shortId(a.conv_id))}</td>
                  <td><span class="rowname">${esc(a.title || '(untitled)')}</span></td>
                  <td><span class="last-hook">${esc(a.retired_at ? relTime(a.retired_at) : '')}</span></td>
                  <td>${esc(a.retired_by || '')}</td>
                  <td class="muted">${esc(a.retire_reason || '')}</td>
                  <td><div class="row-actions"><button class="primary" data-act="reinstate-agent" data-conv="${esc(a.conv_id)}" data-label="${esc(a.title || a.conv_id)}" title="Reinstate this agent back to active status">reinstate</button></div></td>
                </tr>`).join('')}
            </tbody>
          </table>`;
    return `
      <details class="group-virtual" data-group-key="${esc(key)}"${isOpen ? ' open' : ''}>
        <summary data-dnd-target-retired="1">
          <strong class="group-name">${esc(g.name)}</strong>
          <span class="group-virtual-badge" title="A virtual group, not a real one — agents that were retired (demoted back to plain conversations). Drag an agent here to retire it; drag a retired row onto a group, or click reinstate, to bring it back.">virtual</span>
          <span class="muted">— ${members.length} retired agent${members.length === 1 ? '' : 's'}</span>
          <span class="muted group-virtual-hint">drag an agent here to retire it · drag a retired row onto a group to reinstate + join it</span>
        </summary>
        <div class="subtable">
          ${body}
        </div>
      </details>
    `;
  }

  function renderGroups(groups) {
    if (!groups || !groups.length) {
      return '<div class="empty">No groups yet. Create one with: <code>tclaude agent groups create &lt;name&gt;</code></div>';
    }
    return groups.map(g => {
      if (g.virtual) return g.conversations ? renderVirtualConversationsGroup(g)
        : g.retired ? renderVirtualRetiredGroup(g)
        : renderVirtualGroup(g);
      const members = g.members || [];
      // Offline visibility: per-group override falls back to the
      // tab-wide checkbox. Hidden members still count toward the
      // header total so "5 members, 2 online" stays truthful.
      const visible = groupShowOffline(g.name) ? members : members.filter(m => m.online);
      const hiddenOffline = members.length - visible.length;
      // Restore expanded state across the 5s polling re-renders by
      // keying on group name. Persisted in localStorage so it
      // survives a full page reload too.
      const isOpen = localStorage.getItem('tclaude.dash.group.' + g.name) === '1';
      return `
      <details data-group-key="${esc(g.name)}"${isOpen ? ' open' : ''}>
        <summary data-dnd-target-group="${esc(g.name)}">
          <strong class="group-name" data-group-name="${esc(g.name)}">${esc(g.name)}</strong>
          <span class="muted">— ${members.length} members, ${g.online || 0} online${hiddenOffline > 0 ? ` · ${hiddenOffline} offline hidden` : ''}</span>
          ${g.descr ? `<span class="muted"> — ${esc(g.descr)}</span>` : ''}
          <span class="group-default-cwd${g.default_cwd ? '' : ' unset'}" data-act="set-group-dir" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-cwd="${esc(g.default_cwd || '')}" title="${g.default_cwd ? 'Default spawn directory: ' + esc(g.default_cwd) + ' — click to edit' : 'No default spawn directory — click to set one'}">📁 ${g.default_cwd ? esc(shortCwd(g.default_cwd)) : 'no default dir'}</span>
          <span class="group-default-context${g.default_context ? '' : ' unset'}" data-act="set-group-context" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${g.default_context ? 'Startup context (' + g.default_context.length + ' chars) delivered to the inbox of agents spawned here — click to edit' : 'No startup context — click to set one'}">📋 ${g.default_context ? 'startup context' : 'no startup context'}</span>
          <span class="group-max-members${g.max_members ? (members.length >= g.max_members ? ' full' : '') : ' unset'}" data-act="set-group-max-members" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-max="${g.max_members || 0}" title="${g.max_members ? 'Member cap: ' + members.length + '/' + g.max_members + (members.length >= g.max_members ? ' — group is full, spawns refused' : '') + ' — click to edit' : 'No member cap — a spawn-capable agent can grow this group without bound; click to set one'}">👥 ${g.max_members ? members.length + '/' + g.max_members : 'no member cap'}</span>
          ${groupOfflineToggleHTML(g.name)}
          <span class="group-actions">
            <button data-act="spawn-agent" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Spawn a new tclaude session and join this group">+ spawn agent</button>
            <button data-act="add-member" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Add an existing conversation to this group">+ add member</button>
            <button data-act="cron-new" data-prefill='${esc(JSON.stringify({targetMode: 'group', groupName: g.name, scopeGroup: g.name}))}' data-label="${esc(g.name)}" title="Schedule a recurring cron job scoped to ${esc(g.name)} — multicast the whole group, or nudge a single member">⏰ multicast</button>
            <button data-act="message-new" data-prefill='${esc(JSON.stringify({targetMode: 'group', groupName: g.name}))}' data-label="${esc(g.name)}" title="Send a one-shot message to ${esc(g.name)} — the whole group, or a ticked subset of its members">✉ message</button>
            <button data-act="rename-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Rename this group">rename</button>
            <button data-act="export-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Export this whole group — members, permissions, messages and every conversation — to a portable .zip archive">⤓ export</button>
            <button data-act="cleanup-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Remove confirmed-offline members from this group">🧹 cleanup</button>
            <button data-act="window-modal-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" aria-label="Focus or unfocus this group's agent windows" title="Focus / unfocus agent windows — open a modal to bulk-attach (focus) or bulk-detach (unfocus) the terminal windows of agents in this group. Window-only: the agents keep running either way.">🪟 windows…</button>
            <button class="warn" data-act="emergency-shutdown-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Emergency shutdown — stop every running agent in this group. Sends /exit, then force-kills any agent still alive after a grace period. Stop only: nothing is deleted, every session can simply be resumed.">🛑 emergency shutdown</button>
            <button class="danger" data-act="delete-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-members="${members.length}" title="Delete this group">delete group</button>
          </span>
        </summary>
        <div class="subtable">
          ${members.length === 0
            ? '<div class="muted">(no members yet)</div>'
            : visible.length === 0
            ? `<div class="muted">(${hiddenOffline} offline member${hiddenOffline === 1 ? '' : 's'} hidden — toggle "show offline" to see ${hiddenOffline === 1 ? 'it' : 'them'})</div>`
            : `
          <table>
            ${sortHead('members', MEMBER_COLS)}
            <tbody>
              ${applySort('members', visible, MEMBER_ACCESSORS).map(m => memberRowHTML(m, {group: g})).join('')}
            </tbody>
          </table>`}
          ${renderGroupLinksSection(g.name)}
          <code class="copy-cmd" data-copy="tclaude agent groups members ${g.name}">tclaude agent groups members ${esc(g.name)}</code>
        </div>
      </details>
    `;
    }).join('');
  }

  // renderGroupLinksSection: per-group outbound/inbound link rows. Reads
  // from lastSnapshot.links (the snapshot already carries every edge)
  // so we don't need a second round-trip. Edit/delete buttons hit the
  // shared row-action dispatcher; add opens the link modal preset to
  // FROM=this group.
  function renderGroupLinksSection(groupName) {
    const all = (lastSnapshot && lastSnapshot.links) || [];
    const out = all.filter(l => l.from === groupName);
    const inc = all.filter(l => l.to === groupName);
    const total = out.length + inc.length;
    if (total === 0) {
      return `
        <div class="group-links-section">
          <span class="muted" style="font-size:11px">No links involving this group.</span>
          <button data-act="link-new" data-from="${esc(groupName)}" data-label="${esc(groupName)}" title="Add an outbound link from this group">+ link</button>
        </div>
      `;
    }
    const renderRow = (l, dir) => {
      const other = dir === 'out' ? l.to : l.from;
      const arrow = dir === 'out' ? '→' : '←';
      return `
        <tr>
          <td><span class="muted">${arrow}</span></td>
          <td><span class="rowname">${esc(other || '(deleted)')}</span></td>
          <td><span class="id">${esc(l.mode)}</span></td>
          <td><div class="row-actions">
            <button data-act="link-edit" data-id="${l.id}" data-from="${esc(l.from)}" data-to="${esc(l.to)}" data-mode="${esc(l.mode)}" title="Change this link's mode">edit</button>
            <button class="danger" data-act="link-delete" data-id="${l.id}" data-group="${esc(groupName)}" data-from="${esc(l.from)}" data-to="${esc(l.to)}" title="Remove this link">×</button>
          </div></td>
        </tr>
      `;
    };
    return `
      <div class="group-links-section">
        <strong style="font-size:11px">Links</strong>
        <table>
          <thead>
            <tr><th></th><th>Other group</th><th>Mode</th><th></th></tr>
          </thead>
          <tbody>
            ${out.map(l => renderRow(l, 'out')).join('')}
            ${inc.map(l => renderRow(l, 'in')).join('')}
          </tbody>
        </table>
        <button data-act="link-new" data-from="${esc(groupName)}" data-label="${esc(groupName)}" title="Add an outbound link from this group">+ link</button>
      </div>
    `;
  }

  function renderPermissions(perm, agents) {
    const titleByConv = Object.fromEntries((agents || []).map(a => [a.conv_id, a.title]));
    const overrides = perm.overrides || {};
    const defaults = perm.defaults || [];
    // Split each conv's tri-state override map into granted / denied
    // slug lists for display. The per-agent "permissions" button on the
    // Groups tab is the write path; this tab is the roster.
    const rows = Object.entries(overrides).map(([k, slugEffects]) => {
      const granted = [], denied = [];
      for (const [slug, effect] of Object.entries(slugEffects || {})) {
        (effect === 'deny' ? denied : granted).push(slug);
      }
      granted.sort(); denied.sort();
      return { k, granted, denied };
    }).sort((a, b) => a.k < b.k ? -1 : 1);
    return `
      <h3 style="margin-top:0">Defaults <span class="muted" style="font-size:11px">— granted to every agent (config.json)</span></h3>
      ${defaults.length === 0
        ? '<div class="empty">No defaults set. Grant with: <code>tclaude agent permissions grant default &lt;slug&gt;</code></div>'
        : `<div>${defaults.map(s => `<span class="tag default slug">${esc(s)}</span>`).join(' ')}</div>`}
      <h3>Per-agent overrides <span class="muted" style="font-size:11px">— permanent grant / deny on top of defaults (SQLite agent_permissions). Edit via the per-agent “permissions” button.</span></h3>
      ${rows.length === 0
        ? '<div class="empty">No per-agent overrides yet. Use the per-agent “permissions” button, or <code>tclaude agent permissions grant|deny &lt;conv&gt; &lt;slug&gt;</code></div>'
        : `<table>
            <thead><tr><th>ID</th><th>Title</th><th>Granted</th><th>Denied</th></tr></thead>
            <tbody>
              ${rows.map(r => `
                <tr>
                  <td class="id">${esc(shortId(r.k))}</td>
                  <td class="rowname">${esc(titleByConv[r.k] || '(unknown)')}</td>
                  <td>${r.granted.map(s => `<span class="tag slug">${esc(s)}</span>`).join(' ') || '<span class="muted">—</span>'}</td>
                  <td>${r.denied.map(s => `<span class="tag slug deny">${esc(s)}</span>`).join(' ') || '<span class="muted">—</span>'}</td>
                </tr>
              `).join('')}
            </tbody>
          </table>`}
    `;
  }

  function renderSlugs(slugs) {
    if (!slugs || !slugs.length) return '<div class="empty">No slugs registered.</div>';
    return `
      <table>
        <thead><tr><th>Slug</th><th>Description</th></tr></thead>
        <tbody>
          ${slugs.map(s => `
            <tr>
              <td><span class="slug">${esc(s.slug)}</span></td>
              <td>${esc(s.description || '')}</td>
            </tr>
          `).join('')}
        </tbody>
      </table>
    `;
  }

  function showStatus(text, isError) {
    const el = $('#status');
    el.textContent = text;
    el.classList.toggle('error', !!isError);
  }

  // === Messages tab — human-facing notifications from agents ===
  // The unread count drives a badge on the Messages nav button, so the
  // human sees there's something to read from whatever tab they're on.
  function renderMessagesBadge(unread) {
    const badge = $('#messages-badge');
    if (!badge) return;
    badge.textContent = unread > 99 ? '99+' : String(unread);
    badge.hidden = unread === 0;
  }

  function renderMessagesTab() {
    if (!lastSnapshot) return;
    const all = lastSnapshot.messages || [];
    // Conv-ids with a live tmux window — focus is only meaningful for
    // those. Cross-referenced from the agent lists already in the
    // snapshot, so no extra round-trip.
    const onlineConvs = new Set();
    (lastSnapshot.agents || []).concat(lastSnapshot.ungrouped || [])
      .forEach(a => { if (a && a.online) onlineConvs.add(a.conv_id); });
    const q = ($('#filter-messages').value || '').toLowerCase();
    const filtered = q
      ? all.filter(m => [m.from_title, m.group, m.subject, m.body]
          .some(s => (s || '').toLowerCase().includes(q)))
      : all;
    $('#messages-list').innerHTML = renderMessages(filtered, onlineConvs);
    const total = all.length;
    $('#filter-messages-count').textContent = q
      ? `${filtered.length} / ${total}`
      : `${total} message${total === 1 ? '' : 's'}`;
  }

  function renderMessages(msgs, onlineConvs) {
    if (!msgs || !msgs.length) return '<div class="empty">No messages.</div>';
    return msgs.map(m => {
      const unread = !m.read;
      const when = m.created_at ? new Date(m.created_at).toLocaleString() : '';
      const grp = m.group ? `<span class="msg-group">· ${esc(m.group)}</span>` : '';
      const subj = m.subject ? `<div class="msg-subject">${esc(m.subject)}</div>` : '';
      // Focus raises the sending agent's terminal window. Only offered
      // when the agent is online — disabled otherwise, never an error.
      const focusable = m.from_conv && onlineConvs.has(m.from_conv);
      const focusBtn = m.from_conv
        ? `<button data-act="msg-focus" data-conv="${esc(m.from_conv)}" data-id="${m.id}" data-label="${esc(m.from_title || m.from_conv)}"${focusable ? '' : ' disabled'} title="${focusable ? 'Focus this agent’s terminal window and mark the message read' : 'Sending agent is offline — no window to focus'}">focus</button>`
        : '';
      const readBtn = unread
        ? `<button data-act="msg-mark-read" data-id="${m.id}" title="Mark this message read">mark read</button>`
        : '';
      return `<div class="msg-card${unread ? ' msg-unread' : ''}">
        <div class="msg-head">
          ${unread ? '<span class="msg-dot" title="unread">●</span>' : ''}
          <span class="msg-from">${esc(m.from_title || '(unknown sender)')}</span>
          ${grp}
          <span class="msg-id">#${m.id}</span>
          <span class="spacer"></span>
          <span class="msg-time">${esc(when)}</span>
        </div>
        ${subj}
        <div class="msg-body">${esc(m.body)}</div>
        <div class="msg-actions">${focusBtn}${readBtn}</div>
      </div>`;
    }).join('');
  }

  // === Subscription-usage readout (top bar, left of the live dot) ===
  // Account-wide, not per-agent: one readout for the whole dashboard.
  // Mirrors the statusbar's "5h [bar] 17% (2h16m) | 7d ..." format.
  const USAGE_BAR_WIDTH = 8;

  // usageBarColor matches the statusbar thresholds: red >=80, yellow
  // >=60, green otherwise.
  function usageBarColor(pct) {
    if (pct >= 80) return '#f85149';
    if (pct >= 60) return '#d29922';
    return '#3fb950';
  }

  // usageBar renders a mini progress bar from a single block glyph (█)
  // for BOTH the filled and the empty cells — they differ only in
  // colour, never in glyph. Using one glyph guarantees the filled and
  // empty segments render at an identical height: mixing in a shade
  // glyph (░) for the empty run made it render taller than the █ fill
  // in the browser's monospace font.
  function usageBar(pct) {
    const p = Math.max(0, Math.min(100, pct || 0));
    const filled = Math.round(p / 100 * USAGE_BAR_WIDTH);
    const empty = USAGE_BAR_WIDTH - filled;
    return '<span class="ubar-fill" style="color:' + usageBarColor(p) + '">'
      + '█'.repeat(filled) + '</span>'
      + '<span class="ubar-empty">' + '█'.repeat(empty) + '</span>';
  }

  // usageWindowHTML renders one rolling-limit window: label, coloured
  // mini bar, percent, and the remaining-time hint.
  function usageWindowHTML(label, win) {
    const pct = win.pct || 0;
    const rem = win.remaining
      ? ' <span class="urem">(' + esc(win.remaining) + ')</span>' : '';
    return '<span class="uw"><span class="ulabel">' + label + '</span>'
      + '<span class="ubar">' + usageBar(pct) + '</span>'
      + '<span class="upct">' + Math.round(pct) + '%</span>' + rem + '</span>';
  }

  // renderUsage paints the top-bar readout from snapshot.usage. When
  // usage data is unavailable it degrades to a muted "usage: n/a"
  // rather than a broken or error state.
  function renderUsage(u) {
    const el = $('#usage');
    if (!el) return;
    const wins = [];
    if (u && u.available) {
      if (u.five_hour) wins.push(usageWindowHTML('5h', u.five_hour));
      if (u.seven_day) wins.push(usageWindowHTML('7d', u.seven_day));
    }
    if (wins.length) {
      el.classList.remove('na');
      el.innerHTML = wins.join('');
      el.title = 'Subscription usage limits — 5-hour and 7-day rolling windows';
    } else {
      el.classList.add('na');
      el.textContent = 'usage: n/a';
      el.title = 'Subscription usage data is currently unavailable';
    }
  }

  // Last successful snapshot, kept so the filter inputs can re-render
  // without a server roundtrip when the user types.
  let lastSnapshot = null;
  // True while an inline rename input is open; suspends the auto-
  // refresh so the 5s tick doesn't blow the input away mid-edit.
  let renameEditing = false;

  // refreshSuspended() is the single source of truth for whether the
  // auto-refresh is allowed to re-render the DOM right now. refresh()
  // consults it both BEFORE its /api/snapshot fetch and AGAIN after,
  // so a refresh that started before a drag/modal opened can never
  // resume mid-gesture and re-render underneath it.
  //
  // Modal state is derived from the DOM (.modal-overlay.show) rather
  // than a hand-maintained boolean on purpose: a flag must be reset on
  // every close path or it leaks and wedges auto-refresh forever — the
  // exact failure mode behind the drag-retire-freezes-refresh bug. The
  // DOM cannot leak: once an overlay's .show class is gone the modal
  // simply stops suspending, with no reset to forget. It is also
  // uniform — every modal, present and future, shares .modal-overlay,
  // so all of them suspend auto-refresh while open without each having
  // to remember to toggle a flag.
  function refreshSuspended() {
    // An inline rename <input> is open — re-rendering would destroy it
    // mid-keystroke.
    if (renameEditing) return true;
    // A drag-and-drop gesture is in flight — re-rendering would detach
    // the dragged row, and a dragend dispatched on a now-detached node
    // never bubbles up to the document-level handler, so the drag's
    // own cleanup (this suspension included) would be lost forever.
    if (dndDragActive) return true;
    // Any modal overlay is open.
    if (document.querySelector('.modal-overlay.show')) return true;
    return false;
  }

  function filterGroups(groups, q) {
    if (!q) return groups;
    const needle = q.toLowerCase();
    const out = [];
    for (const g of groups) {
      const nameHit = (g.name || '').toLowerCase().includes(needle);
      const descrHit = (g.descr || '').toLowerCase().includes(needle);
      const matchedMembers = (g.members || []).filter(m => {
        const state = m.state || {};
        return ((m.title || '').toLowerCase().includes(needle)) ||
               ((m.conv_id || '').toLowerCase().includes(needle)) ||
               ((m.role || '').toLowerCase().includes(needle)) ||
               ((m.descr || '').toLowerCase().includes(needle)) ||
               ((m.branch || '').toLowerCase().includes(needle)) ||
               ((m.startup_branch || '').toLowerCase().includes(needle)) ||
               ((state.cwd || '').toLowerCase().includes(needle)) ||
               ((m.startup_dir || '').toLowerCase().includes(needle)) ||
               ((m.current_dir || '').toLowerCase().includes(needle));
      });
      if (nameHit || descrHit) {
        // Group name / descr matched: keep all members so the user can
        // see the whole group context.
        out.push(g);
      } else if (matchedMembers.length > 0) {
        // Members matched: show only the matching subset.
        out.push({ ...g, members: matchedMembers });
      }
    }
    return out;
  }

  function renderGroupsTab() {
    if (!lastSnapshot) return;
    const q = $('#filter-groups').value;
    const realGroups = lastSnapshot.groups || [];
    // Append the virtual "Ungrouped" group LAST so it always sorts to
    // the bottom of the listing. filterGroups preserves order, so the
    // text filter narrows it like any other group without moving it.
    // Gated solely on the "show ungrouped" checkbox — once ticked, the
    // group stays visible even when empty (a no-text filter never
    // hides it; a text filter narrows it like any group).
    const list = realGroups.slice();
    if (ungroupedVisible()) {
      list.push(virtualUngroupedGroup(lastSnapshot.ungrouped || []));
    }
    // The virtual "Retired" group sits above Conversations: a retired
    // agent is one step further along (it WAS an agent), so it lands
    // somewhere visible on the tab instead of vanishing the moment it
    // leaves its last group. On by default for the same reason.
    if (retiredVisible()) {
      list.push(virtualRetiredGroup(lastSnapshot.retired || []));
    }
    // The virtual "Conversations" group sorts below even Ungrouped —
    // it's the rawest bucket (not even agents yet). Opt-in via its
    // checkbox.
    if (conversationsVisible()) {
      list.push(virtualConversationsGroup(lastSnapshot.conversations || []));
    }
    const filtered = filterGroups(list, q);
    $('#groups-list').innerHTML = renderGroups(filtered);
    // The count reflects real groups only — the virtual group is a
    // derived bucket, not a group the human created.
    const total = realGroups.length;
    const shownReal = filtered.filter(g => !g.virtual).length;
    $('#filter-groups-count').textContent = q
      ? `${shownReal} / ${total}` : `${total} group${total === 1 ? '' : 's'}`;
  }

  // formatInterval renders an integer second count as a coarse human
  // string ("30s", "5m", "2h", "1d"). Mirrors the cron CLI output so
  // dashboard + terminal read the same.
  function formatInterval(sec) {
    if (!sec) return '';
    if (sec < 60) return sec + 's';
    if (sec < 3600) return Math.floor(sec / 60) + 'm';
    if (sec < 86400) return Math.floor(sec / 3600) + 'h';
    return Math.floor(sec / 86400) + 'd';
  }

  // cronTargetCell describes where a cron job fires. Two shapes:
  //  - group:<name>  → group-target job; the scheduler fans the body
  //                    out to every current member of the group.
  //  - <conv-label>  → conv target; one recipient (the conv-routing
  //                    group_id, if any, is not shown here).
  // The discriminator is target_kind, NOT group_id>0 — a conv-target
  // job routed through a shared group also carries a non-zero group_id.
  function cronTargetCell(j) {
    if (j.target_kind === 'group') {
      return `<span class="tag">group:${esc(j.group_name || ('#' + j.group_id))}</span>`;
    }
    if (j.target_conv) {
      return `<span class="rowname">${esc(j.target_label || j.target_conv.slice(0, 8))}</span>`;
    }
    return '<span class="muted">(no target)</span>';
  }

  // cronStatusPill colorises the last_run_status. Empty / "ok" /
  // anything else map to neutral / green / red respectively.
  function cronStatusPill(s) {
    if (!s) return '<span class="state-pill state-offline" title="never run">never run</span>';
    if (s === 'ok') return `<span class="state-pill state-working" title="${esc(s)}">${esc(s)}</span>`;
    return `<span class="state-pill state-awaiting" title="${esc(s)}">${esc(s)}</span>`;
  }

  function renderCron(jobs) {
    if (!jobs || !jobs.length) {
      return '<div class="empty">No cron jobs yet. Create one with: <code>tclaude agent cron add &lt;name&gt; &lt;interval&gt; &lt;target&gt; -- &lt;body&gt;</code></div>';
    }
    return `
      <table>
        ${sortHead('cron', CRON_COLS)}
        <tbody>
          ${applySort('cron', jobs, CRON_ACCESSORS).map(j => {
            const enabledDot = j.enabled
              ? '<span class="online" title="enabled">●</span>'
              : '<span class="offline" title="disabled">○</span>';
            const enableBtn = j.enabled
              ? `<button class="warn" data-act="cron-disable" data-id="${j.id}" data-label="${esc(j.name)}" title="Pause this cron job">disable</button>`
              : `<button data-act="cron-enable" data-id="${j.id}" data-label="${esc(j.name)}" title="Re-enable this cron job">enable</button>`;
            const runBtn = `<button data-act="cron-run-now" data-id="${j.id}" data-label="${esc(j.name)}" title="Fire this job immediately (also stamps last_run_at)">run now</button>`;
            const editBtn = `<button data-act="cron-edit" data-id="${j.id}" data-label="${esc(j.name)}" title="Edit this cron job">edit</button>`;
            const delBtn = `<button class="danger" data-act="cron-delete" data-id="${j.id}" data-label="${esc(j.name)}" title="Delete this cron job">delete</button>`;
            const bodySummary = (j.body || '').replace(/\s+/g, ' ').trim();
            const bodyTrunc = bodySummary.length > 80 ? bodySummary.slice(0, 80) + '…' : bodySummary;
            return `
              <tr>
                <td>${enabledDot}</td>
                <td class="id">${j.id}</td>
                <td><div class="rowname">${esc(j.name)}</div>${j.subject ? `<div class="muted">${esc(j.subject)}</div>` : ''}</td>
                <td><span class="muted">${esc(j.owner_label || j.owner_conv.slice(0, 8))}</span></td>
                <td>${cronTargetCell(j)}</td>
                <td><span class="id">${esc(formatInterval(j.interval_seconds))}</span></td>
                <td><span class="last-hook">${esc(relTime(j.last_run_at) || '—')}</span></td>
                <td>${cronStatusPill(j.last_run_status)}</td>
                <td><span class="muted" title="${esc(j.body || '')}">${esc(bodyTrunc)}</span></td>
                <td><div class="row-actions">${runBtn}${editBtn}${enableBtn}${delBtn}</div></td>
              </tr>
            `;
          }).join('')}
        </tbody>
      </table>
    `;
  }

  function filterCron(jobs, q) {
    if (!q) return jobs;
    const needle = q.toLowerCase();
    return jobs.filter(j =>
      ((j.name || '').toLowerCase().includes(needle)) ||
      ((j.owner_label || '').toLowerCase().includes(needle)) ||
      ((j.target_label || '').toLowerCase().includes(needle)) ||
      ((j.group_name || '').toLowerCase().includes(needle)) ||
      ((j.subject || '').toLowerCase().includes(needle)) ||
      ((j.body || '').toLowerCase().includes(needle))
    );
  }

  function renderCronTab() {
    if (!lastSnapshot) return;
    const q = $('#filter-cron').value;
    const filtered = filterCron(lastSnapshot.cron || [], q);
    $('#cron-list').innerHTML = renderCron(filtered);
    const total = (lastSnapshot.cron || []).length;
    $('#filter-cron-count').textContent = q
      ? `${filtered.length} / ${total}` : `${total} job${total === 1 ? '' : 's'}`;
  }

  // -- Sudo tab ---------------------------------------------------------

  function fmtRemaining(secs) {
    if (!secs || secs <= 0) return 'expired';
    if (secs < 60) return secs + 's';
    if (secs < 3600) {
      const m = Math.floor(secs / 60);
      const s = secs % 60;
      return s > 0 ? `${m}m${s}s` : `${m}m`;
    }
    const h = Math.floor(secs / 3600);
    const m = Math.floor((secs % 3600) / 60);
    return m > 0 ? `${h}h${m}m` : `${h}h`;
  }

  function filterSudo(rows, q) {
    if (!q) return rows;
    const needle = q.toLowerCase();
    return rows.filter(r =>
      (r.conv_title || '').toLowerCase().includes(needle) ||
      (r.conv_id || '').toLowerCase().includes(needle) ||
      (r.slug || '').toLowerCase().includes(needle) ||
      (r.reason || '').toLowerCase().includes(needle));
  }

  function renderSudo(rows) {
    if (!rows || !rows.length) {
      return '<div class="empty">No active sudo grants.</div>';
    }
    return `
      <table>
        ${sortHead('sudo', SUDO_COLS)}
        <tbody>
          ${applySort('sudo', rows, SUDO_ACCESSORS).map(r => `
            <tr>
              <td>
                <span class="rowname">${esc(r.conv_title || '(unknown)')}</span>
                <span class="id">${esc(shortId(r.conv_id))}</span>
              </td>
              <td><span class="tag slug">${esc(r.slug)}</span></td>
              <td><span class="last-hook">${esc(relTime(r.granted_at))}</span></td>
              <td><span class="last-hook">${esc(fmtRemaining(r.remaining_seconds))}</span></td>
              <td>${esc(r.reason || '')}</td>
              <td><span class="muted" title="${esc(r.granted_by || '')}">${esc(r.granted_by || '')}</span></td>
              <td><button class="danger" data-act="sudo-revoke" data-id="${r.id}" data-slug="${esc(r.slug)}" data-conv="${esc(r.conv_title || r.conv_id)}" title="Revoke this grant">revoke</button></td>
            </tr>`).join('')}
        </tbody>
      </table>
    `;
  }

  function renderSudoTab() {
    if (!lastSnapshot) return;
    const q = $('#filter-sudo').value;
    const rows = lastSnapshot.sudo || [];
    const filtered = filterSudo(rows, q);
    $('#sudo-list').innerHTML = renderSudo(filtered);
    $('#filter-sudo-count').textContent = q
      ? `${filtered.length} / ${rows.length}`
      : `${rows.length} active grant${rows.length === 1 ? '' : 's'}`;
  }

  // -- Links tab --------------------------------------------------------
  // Inter-group communication links surface as a flat read-only table
  // in v1. Use `tclaude agent groups link add/rm` to mutate. The list
  // shows direction (FROM → TO) and mode so the human can reason about
  // who can message whom.
  function renderLinks(rows) {
    if (!rows || !rows.length) {
      return '<div class="empty">No inter-group links yet. Create one with the <strong>+ new link</strong> button above, or: <code>tclaude agent groups link add &lt;from&gt; &lt;to&gt; [--bidir]</code></div>';
    }
    return `
      <table>
        ${sortHead('links', LINK_COLS)}
        <tbody>
          ${applySort('links', rows, LINK_ACCESSORS).map(l => `
            <tr>
              <td class="id">${l.id}</td>
              <td><span class="rowname">${esc(l.from || '(deleted)')}</span></td>
              <td class="muted">→</td>
              <td><span class="rowname">${esc(l.to || '(deleted)')}</span></td>
              <td><span class="id">${esc(l.mode)}</span></td>
              <td><span class="muted">${esc(relTime(l.created_at) || '')}</span></td>
              <td><div class="row-actions">
                <button data-act="link-edit" data-id="${l.id}" data-from="${esc(l.from)}" data-to="${esc(l.to)}" data-mode="${esc(l.mode)}" title="Change this link's mode">edit</button>
                <button class="danger" data-act="link-delete" data-id="${l.id}" data-group="${esc(l.from)}" data-from="${esc(l.from)}" data-to="${esc(l.to)}" title="Remove this link">delete</button>
              </div></td>
            </tr>
          `).join('')}
        </tbody>
      </table>
    `;
  }
  function filterLinks(rows, q) {
    if (!q) return rows;
    const needle = q.toLowerCase();
    return rows.filter(l =>
      ((l.from || '').toLowerCase().includes(needle)) ||
      ((l.to || '').toLowerCase().includes(needle)) ||
      ((l.mode || '').toLowerCase().includes(needle))
    );
  }
  function renderLinksTab() {
    if (!lastSnapshot) return;
    const q = $('#filter-links').value;
    const rows = lastSnapshot.links || [];
    const filtered = filterLinks(rows, q);
    $('#links-list').innerHTML = renderLinks(filtered);
    $('#filter-links-count').textContent = q
      ? `${filtered.length} / ${rows.length}`
      : `${rows.length} link${rows.length === 1 ? '' : 's'}`;
  }

  // openSudoGrantModal: builds the slug picker from the snapshot's
  // registry, restores the conv field from a per-page memory so
  // reopening keeps focus, and traps Escape to close. Submission
  // hits POST /api/sudo and falls through to refresh() on success
  // so the new grant lands on the list immediately.
  let sudoGrantBlocklist = ['permissions.grant', 'permissions.revoke'];
  // sudoByConv: conv-id → list of active grants. Built from
  // snapshot.sudo on every refresh so any renderer (Agents, Groups
  // members) can consult it for the 🔓 badge without a server-side
  // duplication of dashboardMember.active_sudo.
  let sudoByConv = {};
  function openSudoGrantModal(prefillConv) {
    const slugs = (lastSnapshot && lastSnapshot.slugs) || [];
    const wrap = $('#sudo-grant-slugs');
    wrap.innerHTML = slugs.map(s => {
      const blocked = sudoGrantBlocklist.includes(s.slug);
      return `<label class="${blocked ? 'blocked' : ''}" title="${esc(s.descr || '')}">
        <input type="checkbox" value="${esc(s.slug)}"${blocked ? ' disabled' : ''}>
        ${esc(s.slug)}
      </label>`;
    }).join('');
    wrap.querySelectorAll('input[type=checkbox]').forEach(cb => {
      cb.addEventListener('change', () => {
        cb.parentElement.classList.toggle('checked', cb.checked);
      });
    });
    if (prefillConv != null) $('#sudo-grant-conv').value = prefillConv;
    $('#sudo-grant-error').textContent = '';
    $('#sudo-grant-modal').classList.add('show');
    setTimeout(() => $('#sudo-grant-conv').focus(), 0);
  }
  function closeSudoGrantModal() {
    $('#sudo-grant-modal').classList.remove('show');
  }

  async function submitSudoGrant() {
    const conv = $('#sudo-grant-conv').value.trim();
    const slugs = $$('#sudo-grant-slugs input[type=checkbox]:checked').map(cb => cb.value);
    const duration = $('#sudo-grant-duration').value.trim();
    const reason = $('#sudo-grant-reason').value.trim();
    const errEl = $('#sudo-grant-error');
    errEl.textContent = '';
    if (!conv) { errEl.textContent = 'Conv is required.'; return; }
    if (!slugs.length) { errEl.textContent = 'Pick at least one slug.'; return; }
    const btn = $('#sudo-grant-submit');
    btn.disabled = true;
    try {
      const r = await fetch('/api/sudo', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ conv, slugs, duration, reason }),
      });
      if (!r.ok) {
        const text = await r.text();
        errEl.textContent = text || ('HTTP ' + r.status);
        return;
      }
      const resp = await r.json();
      const ok = (resp.grants || []).filter(g => g.id > 0).length;
      const failed = (resp.grants || []).length - ok;
      toast(`Granted ${ok} slug${ok === 1 ? '' : 's'} to ${resp.conv_id ? shortId(resp.conv_id) : conv}` +
        (failed > 0 ? ` (${failed} failed)` : ''));
      closeSudoGrantModal();
      await refresh();
    } catch (e) {
      errEl.textContent = 'Network error: ' + (e.message || e);
    } finally {
      btn.disabled = false;
    }
  }

  // Per-row sudo-revoke is handled by bindRowActions (data-act="sudo-revoke").
  // The Grant modal hooks into bindSudoModal below.

  // pickSudoAgentModal opens a filtered agent picker and resolves to
  // the chosen conv-id (or "" on cancel). Reuses the .add-member-modal
  // CSS shape so the look matches the existing "Add member" overlay.
  // Simpler than addMemberModal: no per-group "exclude existing" — the
  // daemon's policy already handles policy enforcement on submit.
  function pickSudoAgentModal() {
    return new Promise(resolve => {
      const overlay = $('#sudo-pick-agent-modal');
      const search = $('#sudo-pick-agent-search');
      const list = $('#sudo-pick-agent-list');
      const includeAll = $('#sudo-pick-agent-all');
      search.value = '';
      includeAll.checked = false;
      let highlight = 0;
      let candidates = [];

      function buildCandidates() {
        const out = [];
        const seen = new Set();
        for (const a of (lastSnapshot?.agents || [])) {
          if (!a.conv_id || seen.has(a.conv_id)) continue;
          if (!includeAll.checked && !a.online) continue;
          seen.add(a.conv_id);
          out.push(a);
        }
        out.sort((a, b) => {
          if (!!b.online !== !!a.online) return (b.online ? 1 : 0) - (a.online ? 1 : 0);
          return (a.title || '').localeCompare(b.title || '');
        });
        return out;
      }

      function applyFilter(rows, q) {
        if (!q) return rows;
        const needle = q.toLowerCase();
        return rows.filter(a =>
          (a.title || '').toLowerCase().includes(needle) ||
          (a.conv_id || '').toLowerCase().includes(needle) ||
          (a.groups || []).some(g => g.toLowerCase().includes(needle)));
      }

      function render() {
        candidates = applyFilter(buildCandidates(), search.value);
        if (highlight >= candidates.length) highlight = Math.max(0, candidates.length - 1);
        if (highlight < 0) highlight = 0;
        if (!candidates.length) {
          list.innerHTML = '<div class="add-member-empty">No matching conversations. ' +
            (includeAll.checked
              ? '(Try a different filter.)'
              : '(Try ticking "Include offline / archived" for a wider pool.)') +
            '</div>';
          return;
        }
        list.innerHTML = candidates.map((a, i) => {
          const dot = a.online
            ? '<span class="online" title="online">●</span>'
            : '<span class="offline" title="offline">○</span>';
          const groups = (a.groups || []).length
            ? `<span class="groups-tag">in: ${esc((a.groups || []).join(', '))}</span>`
            : '';
          // Surface the 🔓 badge inline so the human can see who
          // already holds active grants while picking — useful for
          // "extend alice's window" without a tab switch.
          const badge = sudoBadge(sudoByConv[a.conv_id], a.conv_id);
          return `<div class="add-member-row${i === highlight ? ' highlighted' : ''}" data-i="${i}">` +
                 `${dot}<span class="rowname">${esc(a.title || '(unnamed)')}</span>` +
                 `<span class="id">${esc(shortId(a.conv_id))}</span>${badge}${groups}` +
                 `</div>`;
        }).join('');
        const hl = list.querySelector('.add-member-row.highlighted');
        if (hl) hl.scrollIntoView({block: 'nearest'});
      }

      function close(convID) {
        overlay.classList.remove('show');
        document.removeEventListener('keydown', onKey);
        resolve(convID || '');
      }

      function onKey(e) {
        if (!overlay.classList.contains('show')) return;
        if (e.key === 'Escape') { e.preventDefault(); close(''); }
        else if (e.key === 'ArrowDown') { e.preventDefault(); highlight++; render(); }
        else if (e.key === 'ArrowUp') { e.preventDefault(); highlight--; render(); }
        else if (e.key === 'Enter') {
          e.preventDefault();
          const c = candidates[highlight];
          if (c) close(c.conv_id);
        }
      }

      list.onclick = (e) => {
        const row = e.target.closest('.add-member-row');
        if (!row) return;
        const i = parseInt(row.dataset.i, 10);
        const c = candidates[i];
        if (c) close(c.conv_id);
      };
      search.oninput = () => { highlight = 0; render(); };
      includeAll.onchange = render;
      overlay.onclick = (e) => { if (e.target === overlay) close(''); };
      document.addEventListener('keydown', onKey);

      overlay.classList.add('show');
      render();
      setTimeout(() => search.focus(), 0);
    });
  }

  // -- Cron create / edit form ----------------------------------------
  //
  // One modal serves both Create and Edit. Edit mode prefills every
  // field from the selected row and routes the submit to PATCH; create
  // mode starts blank and POSTs. The "Save & create another" button
  // posts then resets the form for the next entry — handy when building
  // out a multi-cron team in one sitting.
  //
  // Target has two radio modes: Solo agent (text input + agent picker
  // overlay) or Group (dropdown of group names from the snapshot).
  // Owner is always a text input + optional picker — humans rarely
  // need to override the default, but per-agent self-nudge cron jobs
  // benefit from prefill (owner = the agent itself).

  // Active edit id; null in create mode. Reset every time the modal
  // opens or closes.
  let cronEditId = null;
  let cronOriginalTarget = null;       // for PATCH: only send `target` if user changed it
  let cronOriginalGroupID = null;      // ditto for `group_id`

  // openCronCreateModal opens the modal in create mode. prefill is an
  // optional object: { targetMode, target, owner, name, subject, body,
  // interval, enabled }. All optional. Used by the context-aware
  // entry-point buttons (Agents row ⏰, Groups member ⏰, group header ⏰)
  // to drop the relevant defaults into the form before the user opens
  // it.
  function openCronCreateModal(prefill) {
    cronEditId = null;
    cronOriginalTarget = null;
    cronOriginalGroupID = null;
    const scopeGroup = prefill && prefill.scopeGroup;
    $('#cron-create-title').textContent = scopeGroup
      ? `Schedule a cron job for group "${scopeGroup}"`
      : 'Schedule a cron job';
    $('#cron-create-meta').style.display = 'none';
    $('#cron-create-submit').textContent = 'Create';
    $('#cron-create-save-another').style.display = '';
    populateCronForm(prefill || {});
    showCronCreateModal();
  }

  // openCronEditModal opens the modal in edit mode, prefilled from the
  // selected cron job row. Submit PATCHes /api/cron/{id} instead of
  // POSTing /api/cron.
  function openCronEditModal(job) {
    cronEditId = job.id;
    cronOriginalTarget = job.target_conv || '';
    cronOriginalGroupID = job.group_id || 0;
    $('#cron-create-title').textContent = 'Edit cron job';
    const meta = $('#cron-create-meta');
    meta.style.display = 'block';
    meta.textContent = `#${job.id} · ${job.name || '(unnamed)'}`;
    $('#cron-create-submit').textContent = 'Save';
    // Edit mode hides "Save & create another" — that pattern only
    // makes sense for create.
    $('#cron-create-save-another').style.display = 'none';
    populateCronForm(jobToPrefill(job));
    showCronCreateModal();
  }

  function jobToPrefill(job) {
    // target_kind, not group_id>0: a conv-target job routed through a
    // shared group also has a non-zero group_id but is not a multicast.
    const isGroup = job.target_kind === 'group';
    return {
      name: job.name || '',
      owner: job.owner_label || job.owner_conv || '',
      targetMode: isGroup ? 'group' : 'solo',
      target: isGroup ? '' : (job.target_label || job.target_conv || ''),
      groupName: isGroup ? (job.group_name || '') : '',
      interval: formatInterval(job.interval_seconds) || '',
      subject: job.subject || '',
      body: job.body || '',
      enabled: !!job.enabled,
    };
  }

  function populateCronForm(p) {
    $('#cron-create-name').value = p.name || '';
    $('#cron-create-owner').value = p.owner || '';
    $('#cron-create-subject').value = p.subject || '';
    $('#cron-create-body').value = p.body || '';
    $('#cron-create-enabled').checked = p.enabled === undefined ? true : !!p.enabled;
    // Interval: prefill the text input; if it matches a chip preset,
    // visually highlight the chip too.
    const interval = p.interval || '';
    $('#cron-create-interval').value = interval;
    setSelectedChip(interval);
    // Target — shared solo/group picker. Accepts { targetMode, target,
    // groupName } straight off the prefill object. scopeGroup, set only
    // by a group header's "⏰ multicast" button, locks the picker to
    // that group: the dropdown cannot retarget, and Solo mode offers
    // only that group's members. Absent (global "+ new cron job", a
    // member ⏰, an edit) → the picker is unrestricted, as before.
    setTargetPickerScope('cron-create', p.scopeGroup);
    populateTargetPicker('cron-create', p);
    $('#cron-create-error').textContent = '';
  }

  function setSelectedChip(value) {
    const chips = $$('#cron-create-chips button');
    chips.forEach(c => c.classList.toggle('selected', c.dataset.chip === value));
  }

  // --- shared solo/group target picker --------------------------------
  // A solo-agent / group-multicast target selector shared by the cron
  // form and the one-shot message form, so the two never drift. Each
  // host passes a unique idPrefix; the markup + element ids are derived
  // from it (e.g. prefix "cron-create" → #cron-create-target,
  // #cron-create-group, radio group name "cron-create-target-mode"), so
  // a host's own JS can still address fields directly. The host places
  // an empty <div id="${prefix}-target-mount"> in its modal markup;
  // bindTargetPicker mounts the picker into it once at page init.
  //
  // targetPickerScopes[prefix] — when set to a group name, the picker
  // is "scoped" to that group: Group mode locks its dropdown to that
  // one group, and Solo mode offers a <select> of only that group's
  // members instead of the all-agents free-text input + 🔍. The
  // selection then cannot structurally leave the group. setTargetPicker‑
  // Scope arms / clears it; only the cron form's group-multicast entry
  // point ("⏰ multicast" on a group header) sets a scope today.
  const targetPickerScopes = {};
  function setTargetPickerScope(prefix, groupName) {
    if (groupName) targetPickerScopes[prefix] = groupName;
    else delete targetPickerScopes[prefix];
    // Keep the group dropdown's locked (disabled) state in sync with
    // the scope right here — so it can never be left disabled by an
    // earlier scoped open, independent of when populateTargetPickerGroups
    // next runs.
    const sel = $('#' + prefix + '-group');
    if (sel) sel.disabled = !!groupName;
  }

  function targetPickerMarkup(prefix) {
    return `
      <div class="cron-target-modes">
        <label><input type="radio" name="${prefix}-target-mode" value="solo" checked /> Solo agent</label>
        <label><input type="radio" name="${prefix}-target-mode" value="group" /> Group (multicast)</label>
      </div>
      <div class="cron-target-input-row" id="${prefix}-target-solo">
        <input id="${prefix}-target" type="text" placeholder="title / conv-id / 8+-char prefix" autocomplete="off" spellcheck="false" />
        <button type="button" id="${prefix}-target-pick" title="Pick from agent list">🔍</button>
      </div>
      <!-- Scoped solo row — shown instead of the free-text input when
           the picker is scoped to a group (setTargetPickerScope): a
           <select> of just that group's members, so a scoped solo
           target cannot structurally leave the group. -->
      <div class="cron-target-input-row" id="${prefix}-target-scoped" style="display:none">
        <select id="${prefix}-scoped-member"></select>
      </div>
      <div class="cron-target-input-row" id="${prefix}-target-group" style="display:none">
        <select id="${prefix}-group"></select>
      </div>`;
  }

  // bindTargetPicker mounts the picker markup into #${prefix}-target-mount
  // (idempotent) and wires the mode radios + the 🔍 agent-picker button.
  function bindTargetPicker(prefix) {
    const mount = $('#' + prefix + '-target-mount');
    if (mount && !mount.dataset.mounted) {
      mount.innerHTML = targetPickerMarkup(prefix);
      mount.dataset.mounted = '1';
    }
    $$('input[name=' + prefix + '-target-mode]').forEach(rdo => {
      rdo.addEventListener('change', () => setTargetPickerMode(prefix, rdo.value, false));
    });
    $('#' + prefix + '-target-pick').addEventListener('click', async () => {
      const conv = await pickCronTargetModal();
      if (conv) $('#' + prefix + '-target').value = conv;
    });
  }

  function setTargetPickerMode(prefix, mode, populateOnly) {
    const solo = mode === 'solo';
    const scoped = !!targetPickerScopes[prefix];
    // Solo mode shows the free-text input + 🔍 normally, or — when the
    // picker is scoped to a group — a <select> of that group's members.
    $('#' + prefix + '-target-solo').style.display = (solo && !scoped) ? '' : 'none';
    $('#' + prefix + '-target-scoped').style.display = (solo && scoped) ? '' : 'none';
    $('#' + prefix + '-target-group').style.display = solo ? 'none' : '';
    if (!populateOnly) {
      if (solo && scoped) populateTargetPickerMembers(prefix);
      else if (!solo) populateTargetPickerGroups(prefix);
    }
  }

  function populateTargetPickerGroups(prefix) {
    const sel = $('#' + prefix + '-group');
    const scope = targetPickerScopes[prefix];
    // Scoped → the dropdown is locked to the one scoped group, so a
    // scoped multicast cannot be retargeted to a different group.
    const groups = scope
      ? [scope]
      : (lastSnapshot?.groups || []).map(g => g.name).sort();
    const prev = sel.value;
    sel.innerHTML = groups.length
      ? groups.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join('')
      : '<option value="">(no groups — create one first)</option>';
    if (prev && groups.includes(prev)) sel.value = prev;
    sel.disabled = !!scope;
  }

  // populateTargetPickerMembers fills the scoped-mode solo <select>
  // with the members of the scoped group — the structural guarantee
  // that a scoped solo target can only ever be a member of that group.
  function populateTargetPickerMembers(prefix) {
    const sel = $('#' + prefix + '-scoped-member');
    const scope = targetPickerScopes[prefix];
    const g = (lastSnapshot?.groups || []).find(x => x.name === scope);
    const members = (g && g.members) || [];
    const prev = sel.value;
    sel.innerHTML = members.length
      ? members.map(m => `<option value="${esc(m.conv_id)}">${esc(m.title || m.conv_id)}${m.online ? '' : ' (offline)'}</option>`).join('')
      : '<option value="">(no members in this group)</option>';
    if (prev && members.some(m => m.conv_id === prev)) sel.value = prev;
  }

  // populateTargetPicker fills the picker from a prefill object
  // { targetMode, target, groupName } — all optional, defaulting to a
  // blank solo target.
  function populateTargetPicker(prefix, p) {
    const mode = (p && p.targetMode) || 'solo';
    $$('input[name=' + prefix + '-target-mode]').forEach(r => {
      r.checked = r.value === mode;
    });
    setTargetPickerMode(prefix, mode, /*populateOnly=*/true);
    if (mode === 'solo') {
      if (targetPickerScopes[prefix]) {
        // Scoped solo → pick from the group's member <select>. Honour a
        // prefilled target only when it is actually a member.
        populateTargetPickerMembers(prefix);
        const sel = $('#' + prefix + '-scoped-member');
        const want = (p && p.target) || '';
        if (want && Array.from(sel.options).some(o => o.value === want)) {
          sel.value = want;
        } else if (sel.options.length) {
          sel.selectedIndex = 0;
        }
      } else {
        $('#' + prefix + '-target').value = (p && p.target) || '';
      }
    } else {
      populateTargetPickerGroups(prefix);
      const sel = $('#' + prefix + '-group');
      const want = (p && p.groupName) || '';
      // Preserve a target group that is no longer in the snapshot
      // (archived / deleted since the job was created) as an explicit
      // "(missing)" option — silently falling back to the first group
      // would, on a cron edit-save, reroute the job to the wrong group.
      const found = Array.from(sel.options).some(o => o.value === want);
      if (want && !found) {
        const opt = document.createElement('option');
        opt.value = want;
        opt.textContent = `${want} (missing)`;
        sel.prepend(opt);
      }
      if (want) sel.value = want;
      else if (sel.options.length) sel.selectedIndex = 0;
    }
  }

  // readTargetPicker returns { mode, target } where target is a raw
  // solo selector or a "group:NAME" multicast token, or "" when the
  // picker has no usable value (the caller surfaces the inline error).
  function readTargetPicker(prefix) {
    const mode = ($$('input[name=' + prefix + '-target-mode]:checked')[0] || {}).value || 'solo';
    let target = '';
    if (mode === 'solo') {
      target = targetPickerScopes[prefix]
        ? $('#' + prefix + '-scoped-member').value.trim()
        : $('#' + prefix + '-target').value.trim();
    } else {
      const g = $('#' + prefix + '-group').value;
      if (g) target = 'group:' + g;
    }
    return { mode, target };
  }

  function showCronCreateModal() {
    $('#cron-create-modal').classList.add('show');
    setTimeout(() => $('#cron-create-name').focus(), 0);
  }
  function closeCronCreateModal() {
    $('#cron-create-modal').classList.remove('show');
    cronEditId = null;
    // Drop the scope so the registry's lifetime matches the modal's;
    // the next open re-arms it from its prefill regardless.
    setTargetPickerScope('cron-create', null);
  }

  // submitCronForm POSTs (create) or PATCHes (edit). On success, the
  // dashboard refreshes; on `Save & create another`, the form resets
  // for the next entry instead.
  async function submitCronForm(keepOpen) {
    const errEl = $('#cron-create-error');
    errEl.textContent = '';
    const name = $('#cron-create-name').value.trim();
    const owner = $('#cron-create-owner').value.trim();
    const { mode, target } = readTargetPicker('cron-create');
    const interval = $('#cron-create-interval').value.trim();
    const subject = $('#cron-create-subject').value.trim();
    const bodyText = $('#cron-create-body').value;
    const enabled = $('#cron-create-enabled').checked;

    // Client-side gates with inline errors — same shape as the sudo
    // modal's #sudo-grant-error pattern. Daemon does authoritative
    // validation too, so we don't need to enumerate every rule here.
    if (!target) {
      // Scoped solo mode has no free-text input / 🔍 — its empty case
      // is an empty group, so the instruction must not mention them.
      const scopedSolo = mode === 'solo' && !!targetPickerScopes['cron-create'];
      errEl.textContent = mode === 'group'
        ? 'Pick a group from the dropdown (or create one first via the Groups tab).'
        : scopedSolo
          ? 'This group has no members to nudge — switch to Group (multicast), or add a member to the group first.'
          : 'Target is required — type a title / conv-id or use 🔍 to pick.';
      return;
    }
    if (!bodyText) {
      errEl.textContent = 'Body is required (the message text the cron job sends).';
      return;
    }
    if (!cronEditId && !interval) {
      errEl.textContent = 'Schedule is required — click a chip or type a custom duration.';
      return;
    }

    const submitBtn = $('#cron-create-submit');
    const otherBtn = $('#cron-create-save-another');
    submitBtn.disabled = true; otherBtn.disabled = true;
    try {
      let r;
      if (cronEditId) {
        const patch = { name, body: bodyText, subject, enabled };
        if (owner) patch.owner = owner;
        if (interval) patch.interval = interval;
        // Only send target/group_id if the user actually changed
        // them — avoids re-resolving and possibly tripping validation
        // on an unchanged field.
        if (mode === 'solo' && target !== cronOriginalTarget) {
          patch.target = target;
          patch.group_id = 0;
        } else if (mode === 'group') {
          // Switching to group mode (or staying in it with a different
          // pick): send target=group:<name>; daemon resolves to the
          // group's conv set on its own.
          patch.target = target;
        }
        r = await fetch(`/api/cron/${cronEditId}`, {
          method: 'PATCH', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(patch),
        });
      } else {
        const payload = { name, target, interval, subject, body: bodyText, enabled };
        if (owner) payload.owner = owner;
        r = await fetch('/api/cron', {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload),
        });
      }
      if (!r.ok) {
        errEl.textContent = (await r.text()) || ('HTTP ' + r.status);
        return;
      }
      const resp = await r.json();
      const verb = cronEditId ? 'saved' : 'created';
      toast(`cron ${verb}: ${resp.name || ('#' + (resp.id || ''))}`);
      // Optimistic insert/update so the table updates before the next
      // 5s snapshot poll. We just got the canonical row back; splice it
      // into lastSnapshot.cron and re-render.
      if (lastSnapshot) {
        lastSnapshot.cron = lastSnapshot.cron || [];
        const idx = lastSnapshot.cron.findIndex(j => j.id === resp.id);
        if (idx >= 0) lastSnapshot.cron[idx] = resp;
        else lastSnapshot.cron.push(resp);
        renderCronTab();
      }
      if (keepOpen) {
        // Reset body + name for the next entry; keep target/schedule
        // since "create another" is usually batch-style.
        $('#cron-create-name').value = '';
        $('#cron-create-subject').value = '';
        $('#cron-create-body').value = '';
        $('#cron-create-name').focus();
        return;
      }
      closeCronCreateModal();
      // Fire a snapshot refresh so anything we missed (e.g. server
      // re-routed the job through a shared group) gets picked up.
      refresh();
    } catch (e) {
      errEl.textContent = 'Network error: ' + (e.message || e);
    } finally {
      submitBtn.disabled = false; otherBtn.disabled = false;
    }
  }

  // pickCronTargetModal opens a filtered candidate list. Reuses the
  // .add-member-modal CSS. Mode "agent" → solo conv pool (matches the
  // sudo picker); mode "group" → would surface groups but in v1 we
  // already have a <select> for groups, so this is agent-only. Returns
  // the picked conv-id ("" on cancel).
  function pickCronTargetModal() {
    return new Promise(resolve => {
      const overlay = $('#cron-pick-target-modal');
      const search = $('#cron-pick-target-search');
      const list = $('#cron-pick-target-list');
      const includeAll = $('#cron-pick-target-all');
      search.value = '';
      includeAll.checked = false;
      let highlight = 0;
      let candidates = [];

      function buildCandidates() {
        const out = [];
        const seen = new Set();
        for (const a of (lastSnapshot?.agents || [])) {
          if (!a.conv_id || seen.has(a.conv_id)) continue;
          if (!includeAll.checked && !a.online) continue;
          seen.add(a.conv_id);
          out.push(a);
        }
        out.sort((a, b) => {
          if (!!b.online !== !!a.online) return (b.online ? 1 : 0) - (a.online ? 1 : 0);
          return (a.title || '').localeCompare(b.title || '');
        });
        return out;
      }

      function applyFilter(rows, q) {
        if (!q) return rows;
        const needle = q.toLowerCase();
        return rows.filter(a =>
          (a.title || '').toLowerCase().includes(needle) ||
          (a.conv_id || '').toLowerCase().includes(needle) ||
          (a.groups || []).some(g => g.toLowerCase().includes(needle)));
      }

      function render() {
        candidates = applyFilter(buildCandidates(), search.value);
        if (highlight >= candidates.length) highlight = Math.max(0, candidates.length - 1);
        if (highlight < 0) highlight = 0;
        if (!candidates.length) {
          list.innerHTML = '<div class="add-member-empty">No matching conversations. ' +
            (includeAll.checked
              ? '(Try a different filter.)'
              : '(Try ticking "Include offline / archived" for a wider pool.)') +
            '</div>';
          return;
        }
        list.innerHTML = candidates.map((a, i) => {
          const dot = a.online
            ? '<span class="online" title="online">●</span>'
            : '<span class="offline" title="offline">○</span>';
          const groups = (a.groups || []).length
            ? `<span class="groups-tag">in: ${esc((a.groups || []).join(', '))}</span>`
            : '';
          return `<div class="add-member-row${i === highlight ? ' highlighted' : ''}" data-i="${i}">` +
                 `${dot}<span class="rowname">${esc(a.title || '(unnamed)')}</span>` +
                 `<span class="id">${esc(shortId(a.conv_id))}</span>${groups}` +
                 `</div>`;
        }).join('');
        const hl = list.querySelector('.add-member-row.highlighted');
        if (hl) hl.scrollIntoView({block: 'nearest'});
      }

      function close(convID) {
        overlay.classList.remove('show');
        document.removeEventListener('keydown', onKey);
        resolve(convID || '');
      }
      function onKey(e) {
        if (!overlay.classList.contains('show')) return;
        if (e.key === 'Escape') { e.preventDefault(); close(''); }
        else if (e.key === 'ArrowDown') { e.preventDefault(); highlight++; render(); }
        else if (e.key === 'ArrowUp') { e.preventDefault(); highlight--; render(); }
        else if (e.key === 'Enter') {
          e.preventDefault();
          const c = candidates[highlight];
          if (c) close(c.conv_id);
        }
      }
      list.onclick = (e) => {
        const row = e.target.closest('.add-member-row');
        if (!row) return;
        const i = parseInt(row.dataset.i, 10);
        const c = candidates[i];
        if (c) close(c.conv_id);
      };
      search.oninput = () => { highlight = 0; render(); };
      includeAll.onchange = render;
      overlay.onclick = (e) => { if (e.target === overlay) close(''); };
      document.addEventListener('keydown', onKey);
      overlay.classList.add('show');
      render();
      setTimeout(() => search.focus(), 0);
    });
  }

  function bindCronModal() {
    $('#cron-create-open').addEventListener('click', () => openCronCreateModal({}));
    $('#cron-create-cancel').addEventListener('click', closeCronCreateModal);
    $('#cron-create-submit').addEventListener('click', () => submitCronForm(false));
    $('#cron-create-save-another').addEventListener('click', () => submitCronForm(true));
    $('#cron-create-modal').addEventListener('click', (e) => {
      if (e.target.id === 'cron-create-modal') closeCronCreateModal();
    });
    // Solo/group target picker — markup + mode radios + 🔍 button.
    bindTargetPicker('cron-create');
    // Schedule chips push value into the text input + highlight.
    $('#cron-create-chips').addEventListener('click', (e) => {
      const b = e.target.closest('button[data-chip]');
      if (!b) return;
      const v = b.dataset.chip;
      $('#cron-create-interval').value = v;
      setSelectedChip(v);
    });
    // Typing in the custom interval input clears the chip highlight.
    $('#cron-create-interval').addEventListener('input', () => {
      setSelectedChip($('#cron-create-interval').value.trim());
    });
    // Owner picker reuses the cron-pick-target overlay (the target
    // picker's own 🔍 is wired by bindTargetPicker above).
    $('#cron-create-owner-pick').addEventListener('click', async () => {
      const conv = await pickCronTargetModal();
      if (conv) $('#cron-create-owner').value = conv;
    });
    document.addEventListener('keydown', (e) => {
      // Skip when the shared agent-picker overlay is open — its own Esc
      // handler closes it; closing the modal too would drop the draft.
      if (e.key === 'Escape'
          && $('#cron-create-modal').classList.contains('show')
          && !$('#cron-pick-target-modal').classList.contains('show')) {
        closeCronCreateModal();
      }
    });
  }

  // --- one-shot message modal -----------------------------------------
  // Sends a single immediate message through POST /api/message. The
  // modal has two shapes, chosen by the entry-point button:
  //
  //   - per-agent ✉ → SOLO mode: the shared solo/group target picker
  //     (prefix "message-create") picks any agent or any group, exactly
  //     as before.
  //   - per-group ✉ message → GROUP-SCOPED mode: the modal locks to
  //     that one group and replaces the target picker with a checkbox
  //     list of its members, all ticked by default. Sending with every
  //     box ticked is a plain group multicast; unticking some narrows
  //     the send to the chosen subset.
  //
  // Either way there is no schedule — the send fires once, now.

  // messageScopedGroup holds { name, members:[{conv_id,title,online,
  // checked}] } while the modal is open in group-scoped mode, and null
  // in solo mode. openMessageCreateModal sets it; submitMessageForm
  // branches on it.
  let messageScopedGroup = null;

  // openMessageCreateModal opens the modal. prefill is an optional
  // object { from, targetMode, target, groupName } — the context-aware
  // entry-point buttons (per-agent ✉, per-group ✉ message) drop the
  // relevant defaults in before opening. A prefill naming a group
  // (targetMode 'group' + groupName) opens the modal scoped to that
  // group; anything else opens the solo/group target picker.
  function openMessageCreateModal(prefill) {
    prefill = prefill || {};
    $('#message-create-from').value = prefill.from || '';
    $('#message-create-subject').value = '';
    $('#message-create-body').value = '';
    $('#message-create-error').textContent = '';
    // Enabled by default; group-scoped mode disables it again below
    // when the group has no members to tick (updateMessageMembersCount).
    $('#message-create-submit').disabled = false;
    const scoped = prefill.targetMode === 'group' && !!prefill.groupName;
    if (scoped) {
      setupMessageGroupScope(prefill.groupName);
    } else {
      messageScopedGroup = null;
      $('#message-create-target-row').style.display = '';
      $('#message-create-group-row').style.display = 'none';
      populateTargetPicker('message-create', prefill);
    }
    $('#message-create-title').textContent = scoped
      ? `Send a message to group "${prefill.groupName}"`
      : 'Send a message';
    $('#message-create-desc').textContent = scoped
      ? `Delivers one immediate message to the members of "${prefill.groupName}" ticked below — every member is ticked by default, untick any to send to just a subset. Each recipient gets an inbox row plus a tmux nudge if online.`
      : 'Delivers one immediate message to a single agent, or multicasts it to every member of a group. Each recipient gets an inbox row plus a tmux nudge if online.';
    $('#message-create-modal').classList.add('show');
    setTimeout(() => $('#message-create-from').focus(), 0);
  }

  // setupMessageGroupScope switches the modal into group-scoped mode:
  // it hides the solo/group target picker, shows the member checkbox
  // list, and snapshots the group's members (all ticked) into
  // messageScopedGroup.
  function setupMessageGroupScope(groupName) {
    $('#message-create-target-row').style.display = 'none';
    $('#message-create-group-row').style.display = '';
    const g = (lastSnapshot?.groups || []).find(x => x.name === groupName);
    const members = ((g && g.members) || []).map(m => ({
      conv_id: m.conv_id, title: m.title || '', online: !!m.online, checked: true,
    }));
    messageScopedGroup = { name: groupName, members };
    $('#message-create-group-hint').textContent = members.length
      ? `Members of "${groupName}" — all ticked; untick any to message a subset.`
      : `Group "${groupName}" has no members to message.`;
    renderMessageMembers();
  }

  // renderMessageMembers redraws the member checkbox list from
  // messageScopedGroup (preserving each member's checked state) and
  // refreshes the "N of M selected" count.
  function renderMessageMembers() {
    if (!messageScopedGroup) return;
    const listEl = $('#message-create-members');
    const members = messageScopedGroup.members;
    if (members.length === 0) {
      listEl.innerHTML = '<div class="cleanup-empty">no members in this group</div>';
    } else {
      listEl.innerHTML = members.map(m => {
        const online = m.online ? '<span class="cleanup-badge online">online</span>' : '';
        return `<div class="cleanup-row"><label>`
          + `<input type="checkbox" data-conv="${esc(m.conv_id)}"${m.checked ? ' checked' : ''} />`
          + `<span class="title">${esc(m.title || '(untitled)')}</span>`
          + `<span class="id">${esc(m.conv_id.slice(0, 8))}</span>`
          + `${online}</label></div>`;
      }).join('');
    }
    updateMessageMembersCount();
  }

  // updateMessageMembersCount refreshes the "N of M selected" readout
  // without re-rendering the whole list (cheap on a checkbox toggle),
  // and disables Send when nothing is ticked — an empty group, or every
  // box cleared, has no recipient, so the send is blocked at the button
  // rather than failing on submit.
  function updateMessageMembersCount() {
    if (!messageScopedGroup) return;
    const members = messageScopedGroup.members;
    const n = members.filter(m => m.checked).length;
    $('#message-create-members-count').textContent = `${n} of ${members.length} selected`;
    $('#message-create-submit').disabled = n === 0;
  }

  function closeMessageCreateModal() {
    $('#message-create-modal').classList.remove('show');
    messageScopedGroup = null;
  }

  // submitMessageForm POSTs /api/message. In solo mode it reads the
  // shared target picker; in group-scoped mode it sends to the ticked
  // members — a plain group: multicast when every box is ticked, or an
  // explicit member subset when some are unticked. On a multicast it
  // reports how many members the send reached; on a solo send whether
  // the recipient was nudged live or the row just queued in their inbox.
  async function submitMessageForm() {
    const errEl = $('#message-create-error');
    errEl.textContent = '';
    const from = $('#message-create-from').value.trim();
    const subject = $('#message-create-subject').value.trim();
    const bodyText = $('#message-create-body').value;
    // Client-side gates with inline errors — the daemon validates
    // authoritatively too, so this only catches the obvious misses.
    if (!from) {
      errEl.textContent = 'From is required — type a sender agent or use 🔍 to pick.';
      return;
    }
    // Resolve the recipient(s): `mode` drives the success toast, `to`
    // is the raw selector / "group:NAME" token, `members` is an
    // explicit conv-id subset (group-scoped mode only) or null.
    let mode, to, members = null;
    if (messageScopedGroup) {
      mode = 'group';
      to = 'group:' + messageScopedGroup.name;
      const all = messageScopedGroup.members;
      const picked = all.filter(m => m.checked);
      if (picked.length === 0) {
        errEl.textContent = 'Pick at least one recipient — tick the members this message should reach.';
        return;
      }
      // Every member ticked → a plain group: multicast, which tracks the
      // LIVE roster (a member who joined since the modal opened is still
      // reached). A subset → send the explicit conv-id list.
      if (picked.length < all.length) {
        members = picked.map(m => m.conv_id);
      }
    } else {
      const picked = readTargetPicker('message-create');
      mode = picked.mode;
      to = picked.target;
      if (!to) {
        errEl.textContent = mode === 'solo'
          ? 'Target is required — type a title / conv-id or use 🔍 to pick.'
          : 'Pick a group from the dropdown (or create one first via the Groups tab).';
        return;
      }
    }
    if (!bodyText) {
      errEl.textContent = 'Body is required (the message text to send).';
      return;
    }
    const payload = { from, to, subject, body: bodyText };
    if (members) payload.members = members;
    const submitBtn = $('#message-create-submit');
    submitBtn.disabled = true;
    try {
      const r = await fetch('/api/message', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (!r.ok) {
        errEl.textContent = (await r.text()) || ('HTTP ' + r.status);
        return;
      }
      const resp = await r.json();
      if (mode === 'group') {
        const recipients = resp.recipients || [];
        const n = recipients.length;
        const live = recipients.filter(x => x.delivered).length;
        toast(n
          ? `multicast reached ${n} member${n === 1 ? '' : 's'} of ${resp.via_group || to} (${live} nudged live)`
          : `no recipients reached in ${resp.via_group || to} — nothing sent`);
      } else {
        toast(resp.delivered
          ? 'message sent — recipient nudged live'
          : 'message sent — queued in recipient inbox');
      }
      closeMessageCreateModal();
    } catch (e) {
      errEl.textContent = 'Network error: ' + e;
    } finally {
      // In group-scoped mode the Send button's disabled state tracks
      // the live recipient selection — re-derive it (rather than
      // blindly re-enabling) so a failed request still honours the
      // disabled-when-empty invariant, even if the user cleared the
      // last checkbox while the request was in flight.
      if (messageScopedGroup) updateMessageMembersCount();
      else submitBtn.disabled = false;
    }
  }

  function bindMessageModal() {
    // Solo/group target picker — markup + mode radios + 🔍 button.
    bindTargetPicker('message-create');
    $('#message-create-cancel').addEventListener('click', closeMessageCreateModal);
    $('#message-create-submit').addEventListener('click', submitMessageForm);
    $('#message-create-modal').addEventListener('click', (e) => {
      if (e.target.id === 'message-create-modal') closeMessageCreateModal();
    });
    // Group-scoped recipient list: per-member checkbox changes + the
    // select all / none shortcuts. The list markup is (re)rendered by
    // renderMessageMembers; these delegated listeners are bound once.
    $('#message-create-members').addEventListener('change', (e) => {
      const cb = e.target.closest('input[type=checkbox]');
      if (!cb || !messageScopedGroup) return;
      const m = messageScopedGroup.members.find(x => x.conv_id === cb.getAttribute('data-conv'));
      if (m) m.checked = cb.checked;
      updateMessageMembersCount();
    });
    $('#message-create-members-all').addEventListener('click', () => {
      if (!messageScopedGroup) return;
      messageScopedGroup.members.forEach(m => { m.checked = true; });
      renderMessageMembers();
    });
    $('#message-create-members-none').addEventListener('click', () => {
      if (!messageScopedGroup) return;
      messageScopedGroup.members.forEach(m => { m.checked = false; });
      renderMessageMembers();
    });
    // From picker reuses the cron-pick-target agent overlay.
    $('#message-create-from-pick').addEventListener('click', async () => {
      const conv = await pickCronTargetModal();
      if (conv) $('#message-create-from').value = conv;
    });
    document.addEventListener('keydown', (e) => {
      // Skip when the shared agent-picker overlay is open — its own Esc
      // handler closes it; closing the modal too would drop the draft.
      if (e.key === 'Escape'
          && $('#message-create-modal').classList.contains('show')
          && !$('#cron-pick-target-modal').classList.contains('show')) {
        closeMessageCreateModal();
      }
    });
  }

  function bindSudoModal() {
    $('#sudo-grant-open').addEventListener('click', async () => {
      const convID = await pickSudoAgentModal();
      if (convID) openSudoGrantModal(convID);
    });
    $('#sudo-grant-cancel').addEventListener('click', closeSudoGrantModal);
    $('#sudo-grant-submit').addEventListener('click', submitSudoGrant);
    // Select-all / select-none act on every non-disabled checkbox
    // (disabled ones are blocklisted slugs that the server will
    // reject anyway). Per-checkbox onchange handlers update the
    // .checked styling so the visual matches state.
    $('#sudo-grant-select-all').addEventListener('click', () => {
      $$('#sudo-grant-slugs input[type=checkbox]:not([disabled])').forEach(cb => {
        if (!cb.checked) {
          cb.checked = true;
          cb.parentElement.classList.add('checked');
        }
      });
    });
    $('#sudo-grant-select-none').addEventListener('click', () => {
      $$('#sudo-grant-slugs input[type=checkbox]').forEach(cb => {
        if (cb.checked) {
          cb.checked = false;
          cb.parentElement.classList.remove('checked');
        }
      });
    });
    $('#sudo-grant-modal').addEventListener('click', (e) => {
      // Click on backdrop closes; clicks inside the dialog don't.
      if (e.target.id === 'sudo-grant-modal') closeSudoGrantModal();
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && $('#sudo-grant-modal').classList.contains('show')) {
        closeSudoGrantModal();
      }
    });
  }

  // ---- Permanent-permission editor ----------------------------------------
  //
  // openPermEditModal builds one tri-state row (Default / Grant / Deny)
  // per registry slug, pre-selected from the agent's current per-conv
  // overrides in the snapshot. Save POSTs the full selection to
  // /api/permissions, which diffs it against what's persisted. This is
  // the PERMANENT analog of the "+ sudo" elevation — the banner in the
  // modal spells out the difference.
  let permEditConv = '';

  // permRowEffective recomputes the "✓ granted / ✗ denied" indicator on
  // one row from its selected effect plus whether the slug is a global
  // default — mirroring resolvePermission's defaults∪grants−denies.
  function permRowEffective(row) {
    const active = row.querySelector('.perm-tristate button.active');
    const effect = active ? active.dataset.effect : 'default';
    const inDefault = row.dataset.indefault === '1';
    const granted = effect === 'grant' || (effect === 'default' && inDefault);
    const el = row.querySelector('.perm-row-eff');
    el.textContent = granted ? '✓ granted' : '✗ denied';
    el.className = 'perm-row-eff ' + (granted ? 'granted' : 'denied');
  }

  function openPermEditModal(conv, label) {
    const snap = lastSnapshot || {};
    const perms = snap.permissions || {};
    const slugs = (snap.slugs || []).slice().sort((a, b) => a.slug < b.slug ? -1 : 1);
    const defaultSet = new Set(perms.defaults || []);
    const overrides = (perms.overrides || {})[conv] || {};
    permEditConv = conv;
    $('#perm-edit-subtitle').textContent =
      `Agent: ${label || shortId(conv)} · ${shortId(conv)}`;
    $('#perm-edit-error').textContent = '';
    $('#perm-edit-filter').value = '';
    const list = $('#perm-edit-list');
    if (!slugs.length) {
      list.innerHTML = '<div class="empty" style="padding:10px">No permission slugs registered.</div>';
    } else {
      list.innerHTML = slugs.map(s => {
        const cur = overrides[s.slug] || 'default'; // grant | deny | default
        const inDefault = defaultSet.has(s.slug);
        const mk = (eff, txt) =>
          `<button type="button" data-effect="${eff}"${cur === eff ? ' class="active"' : ''}>${txt}</button>`;
        return `<div class="perm-row" data-slug="${esc(s.slug)}" data-indefault="${inDefault ? '1' : '0'}">
          <div class="perm-row-info">
            <span class="perm-row-slug">${esc(s.slug)}</span>
            <span class="perm-row-desc" title="${esc(s.description || '')}">${esc(s.description || '')}</span>
          </div>
          <div class="perm-tristate">${mk('default', 'Default')}${mk('grant', 'Grant')}${mk('deny', 'Deny')}</div>
          <span class="perm-row-eff"></span>
        </div>`;
      }).join('');
      list.querySelectorAll('.perm-row').forEach(permRowEffective);
    }
    list.scrollTop = 0;
    $('#perm-edit-modal').classList.add('show');
    setTimeout(() => $('#perm-edit-filter').focus(), 0);
  }

  function closePermEditModal() {
    $('#perm-edit-modal').classList.remove('show');
  }

  async function submitPermEdit() {
    const errEl = $('#perm-edit-error');
    errEl.textContent = '';
    if (!permEditConv) { errEl.textContent = 'No agent selected.'; return; }
    const overrides = {};
    $$('#perm-edit-list .perm-row').forEach(row => {
      const active = row.querySelector('.perm-tristate button.active');
      overrides[row.dataset.slug] = active ? active.dataset.effect : 'default';
    });
    if (!Object.keys(overrides).length) { errEl.textContent = 'Nothing to save.'; return; }
    const btn = $('#perm-edit-submit');
    btn.disabled = true;
    try {
      const r = await fetch('/api/permissions', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ conv: permEditConv, overrides }),
      });
      if (!r.ok) {
        errEl.textContent = (await r.text()) || ('HTTP ' + r.status);
        return;
      }
      const resp = await r.json().catch(() => ({}));
      const n = resp.changed || 0;
      toast(`Permissions saved — ${n} change${n === 1 ? '' : 's'}`);
      closePermEditModal();
      await refresh();
    } catch (e) {
      errEl.textContent = 'Network error: ' + (e.message || e);
    } finally {
      btn.disabled = false;
    }
  }

  function bindPermEditModal() {
    $('#perm-edit-cancel').addEventListener('click', closePermEditModal);
    $('#perm-edit-submit').addEventListener('click', submitPermEdit);
    // Tri-state clicks are delegated: pick the clicked effect, drop the
    // active class from its siblings, refresh the row's effective hint.
    $('#perm-edit-list').addEventListener('click', (e) => {
      const b = e.target.closest('.perm-tristate button');
      if (!b) return;
      b.parentElement.querySelectorAll('button')
        .forEach(x => x.classList.toggle('active', x === b));
      permRowEffective(b.closest('.perm-row'));
    });
    $('#perm-edit-filter').addEventListener('input', () => {
      const q = $('#perm-edit-filter').value.trim().toLowerCase();
      $$('#perm-edit-list .perm-row').forEach(row => {
        const slug = row.dataset.slug.toLowerCase();
        const desc = (row.querySelector('.perm-row-desc').textContent || '').toLowerCase();
        row.classList.toggle('hidden', q !== '' && !slug.includes(q) && !desc.includes(q));
      });
    });
    $('#perm-edit-reset').addEventListener('click', () => {
      $$('#perm-edit-list .perm-row').forEach(row => {
        row.querySelectorAll('.perm-tristate button')
          .forEach(x => x.classList.toggle('active', x.dataset.effect === 'default'));
        permRowEffective(row);
      });
    });
    $('#perm-edit-modal').addEventListener('click', (e) => {
      if (e.target.id === 'perm-edit-modal') closePermEditModal();
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && $('#perm-edit-modal').classList.contains('show')) {
        closePermEditModal();
      }
    });
  }

  // ---- Group create modal -------------------------------------------------

  function openGroupCreateModal() {
    $('#group-create-name').value = '';
    $('#group-create-descr').value = '';
    $('#group-create-cwd').value = '';
    $('#group-create-context').value = '';
    $('#group-create-max-members').value = '';
    $('#group-create-error').textContent = '';
    $('#group-create-modal').classList.add('show');
    setTimeout(() => $('#group-create-name').focus(), 0);
  }

  function closeGroupCreateModal() {
    $('#group-create-modal').classList.remove('show');
  }

  async function submitGroupCreate() {
    const name = $('#group-create-name').value.trim();
    const descr = $('#group-create-descr').value.trim();
    const cwd = $('#group-create-cwd').value.trim();
    const context = $('#group-create-context').value.trim();
    const errEl = $('#group-create-error');
    errEl.textContent = '';
    if (!name) {
      errEl.textContent = 'name is required';
      return;
    }
    // Max members: blank means unlimited (0); a negative value is a
    // mistake — surface it rather than letting the daemon clamp it.
    const maxRaw = $('#group-create-max-members').value.trim();
    let maxMembers = 0;
    if (maxRaw !== '') {
      maxMembers = parseInt(maxRaw, 10);
      if (!Number.isInteger(maxMembers) || maxMembers < 0) {
        errEl.textContent = 'max members must be a non-negative integer (0 = unlimited)';
        return;
      }
    }
    const submitBtn = $('#group-create-submit');
    submitBtn.disabled = true;
    try {
      const r = await fetch('/api/groups', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, descr, default_cwd: cwd, default_context: context, max_members: maxMembers }),
      });
      if (!r.ok) {
        errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
        return;
      }
      closeGroupCreateModal();
      toast(`group created: ${name}`);
      // Persist the expanded state so the new group shows expanded on next render.
      try { localStorage.setItem('tclaude.dash.group.' + name, '1'); } catch (_) {}
      refresh();
    } catch (err) {
      errEl.textContent = (err && err.message) || String(err);
    } finally {
      submitBtn.disabled = false;
    }
  }

  function bindGroupCreateModal() {
    $('#group-create-open').addEventListener('click', openGroupCreateModal);
    // 🧹 cleanup: the Groups tab's "clean up" button opens the rich
    // multi-category cleanup modal — bulk unjoin / retire / delete /
    // reinstate spanning active agents, retired agents and plain
    // conversations (openCleanupModal mode 'agents').
    $('#cleanup-all-open').addEventListener('click', () => openCleanupModal({ mode: 'agents' }));
    $('#group-create-cancel').addEventListener('click', closeGroupCreateModal);
    $('#group-create-submit').addEventListener('click', submitGroupCreate);
    $('#group-create-modal').addEventListener('click', (e) => {
      if (e.target.id === 'group-create-modal') closeGroupCreateModal();
    });
    $('#group-create-modal').addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && (e.target.id === 'group-create-name' || e.target.id === 'group-create-descr' || e.target.id === 'group-create-cwd')) {
        e.preventDefault();
        submitGroupCreate();
      }
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && $('#group-create-modal').classList.contains('show')) {
        closeGroupCreateModal();
      }
    });
  }

  // ---- Group templates --------------------------------------------------
  //
  // A template is a reusable blueprint for a working group: a name, a
  // shared default context, and an ordered list of agent specs (name,
  // role, descr, task brief, owner flag, permission slugs).
  // Instantiating one creates a fresh group and spawns its whole team.
  //
  // templateEditorEditing holds the original name while editing an
  // existing template (the PATCH target); null while creating.
  // templateEditorAgents mirrors the editor's agent rows so add/remove
  // can re-render the container without losing typed values.
  let templateEditorEditing = null;
  let templateEditorAgents = [];

  function filterTemplates(list, q) {
    if (!q) return list;
    const n = q.toLowerCase();
    return list.filter(t =>
      (t.name || '').toLowerCase().includes(n) ||
      (t.descr || '').toLowerCase().includes(n) ||
      (t.agents || []).some(a =>
        (a.name || '').toLowerCase().includes(n) ||
        (a.role || '').toLowerCase().includes(n)));
  }

  function renderTemplatesTab() {
    if (!lastSnapshot) return;
    const q = $('#filter-templates').value;
    const all = lastSnapshot.templates || [];
    const list = filterTemplates(all, q);
    const countEl = $('#filter-templates-count');
    if (countEl) countEl.textContent = q ? `${list.length} / ${all.length}` : `${all.length}`;
    const host = $('#templates-list');
    if (!list.length) {
      host.innerHTML = `<div class="template-empty">${all.length
        ? 'No templates match the filter.'
        : 'No templates yet — press <b>+ new template</b> to define one, or <b>⤓ from a group</b> to snapshot an existing group.'}</div>`;
      return;
    }
    host.innerHTML = list.map(templateCardHTML).join('');
  }

  function templateCardHTML(t) {
    const agents = (t.agents || []).map(a => {
      const owner = a.is_owner ? '<span class="tc-owner" title="group owner">★</span> ' : '';
      const role = a.role ? ` <span class="tc-role">${esc(a.role)}</span>` : '';
      const np = (a.permissions || []).length;
      const perms = np
        ? ` <span class="tc-role" title="${esc((a.permissions || []).join(', '))}">+${np}🔑</span>`
        : '';
      return `<span class="tc-agent">${owner}${esc(a.name)}${role}${perms}</span>`;
    }).join('');
    const n = (t.agents || []).length;
    return `<div class="template-card" data-template="${esc(t.name)}">
      <div class="tc-head">
        <span class="tc-name">${esc(t.name)}</span>
        ${t.descr ? `<span class="tc-descr">${esc(t.descr)}</span>` : ''}
        <span class="tc-count">${n} agent${n === 1 ? '' : 's'}</span>
        <span class="tc-actions">
          <button class="primary" data-tact="instantiate" data-template="${esc(t.name)}" title="Create a group from this template">⎘ instantiate</button>
          <button class="tool" data-tact="edit" data-template="${esc(t.name)}">edit</button>
          <button class="tool" data-tact="delete" data-template="${esc(t.name)}">delete</button>
        </span>
      </div>
      ${agents ? `<div class="tc-agents">${agents}</div>` : ''}
    </div>`;
  }

  function templatesByName() {
    const m = {};
    for (const t of (lastSnapshot && lastSnapshot.templates) || []) m[t.name] = t;
    return m;
  }

  function blankTemplateAgent() {
    return { name: '', role: '', descr: '', initial_message: '', is_owner: false, permissions: [] };
  }

  // ---- Template editor modal --------------------------------------------

  function openTemplateEditor(tmpl) {
    templateEditorEditing = tmpl ? tmpl.name : null;
    $('#template-editor-title').textContent =
      tmpl ? `Edit template: ${tmpl.name}` : 'New group template';
    $('#template-editor-name').value = tmpl ? tmpl.name : '';
    $('#template-editor-descr').value = tmpl ? (tmpl.descr || '') : '';
    $('#template-editor-context').value = tmpl ? (tmpl.default_context || '') : '';
    $('#template-editor-error').textContent = '';
    templateEditorAgents = tmpl
      ? (tmpl.agents || []).map(a => ({
          name: a.name || '', role: a.role || '', descr: a.descr || '',
          initial_message: a.initial_message || '', is_owner: !!a.is_owner,
          permissions: (a.permissions || []).slice(),
        }))
      : [blankTemplateAgent()];
    renderEditorAgents();
    $('#template-editor-modal').classList.add('show');
    setTimeout(() => $('#template-editor-name').focus(), 0);
  }

  function closeTemplateEditor() { $('#template-editor-modal').classList.remove('show'); }

  function renderEditorAgents() {
    const slugs = (lastSnapshot && lastSnapshot.slugs) || [];
    $('#template-editor-agents').innerHTML =
      templateEditorAgents.map((a, i) => editorAgentRowHTML(a, i, slugs)).join('');
  }

  function editorAgentRowHTML(a, idx, slugs) {
    const perms = new Set(a.permissions || []);
    const checks = slugs.map(s =>
      `<label title="${esc(s.description || '')}"><input type="checkbox" class="ta-perm" data-slug="${esc(s.slug)}"${perms.has(s.slug) ? ' checked' : ''} /> ${esc(s.slug)}</label>`
    ).join('');
    return `<div class="template-agent-row" data-idx="${idx}">
      <div class="template-agent-row-head">
        <span class="template-agent-num">Agent ${idx + 1}</span>
        <label class="template-agent-owner" title="Mark this agent as an owner of the instantiated group — a group can have several owners">
          <input type="checkbox" class="ta-owner"${a.is_owner ? ' checked' : ''} /> owner
        </label>
        <button type="button" class="tool ta-remove" title="Remove this agent">✕</button>
      </div>
      <div class="template-agent-grid">
        <input type="text" class="ta-name" placeholder="name (e.g. PO, dev1)" value="${esc(a.name)}" />
        <input type="text" class="ta-role" placeholder="role (e.g. product-owner)" value="${esc(a.role)}" />
      </div>
      <input type="text" class="ta-descr" placeholder="one-line description (dashboard column)" value="${esc(a.descr)}" />
      <textarea class="ta-initmsg" rows="3" placeholder="task brief for this agent — delivered to its inbox at spawn (newlines OK)">${esc(a.initial_message)}</textarea>
      <details class="ta-perms-details">
        <summary>Permissions (<span class="ta-perms-count">${perms.size}</span>)</summary>
        <div class="ta-perms-list">${checks}</div>
      </details>
    </div>`;
  }

  // scrapeEditorAgents reads the agent rows back into templateEditorAgents
  // — called before any add/remove (which re-renders the container) and
  // before submit, so typed-but-uncommitted values are never lost.
  function scrapeEditorAgents() {
    templateEditorAgents = $$('#template-editor-agents .template-agent-row').map(row => ({
      name: $('.ta-name', row).value.trim(),
      role: $('.ta-role', row).value.trim(),
      descr: $('.ta-descr', row).value.trim(),
      initial_message: $('.ta-initmsg', row).value,
      is_owner: $('.ta-owner', row).checked,
      permissions: $$('.ta-perm', row).filter(c => c.checked).map(c => c.dataset.slug),
    }));
  }

  async function submitTemplateEditor() {
    scrapeEditorAgents();
    const name = $('#template-editor-name').value.trim();
    const errEl = $('#template-editor-error');
    errEl.textContent = '';
    if (!name) { errEl.textContent = 'template name is required'; return; }
    const payload = {
      name,
      descr: $('#template-editor-descr').value.trim(),
      default_context: $('#template-editor-context').value,
      agents: templateEditorAgents,
    };
    const editing = templateEditorEditing;
    const url = editing ? `/api/templates/${encodeURIComponent(editing)}` : '/api/templates';
    const btn = $('#template-editor-submit');
    btn.disabled = true;
    try {
      const r = await fetch(url, {
        method: editing ? 'PATCH' : 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (!r.ok) { errEl.textContent = (await r.text()) || `HTTP ${r.status}`; return; }
      closeTemplateEditor();
      toast(editing ? `template updated: ${name}` : `template created: ${name}`);
      refresh();
    } catch (err) {
      errEl.textContent = (err && err.message) || String(err);
    } finally {
      btn.disabled = false;
    }
  }

  async function deleteTemplate(name) {
    const ok = await confirmModal({
      title: 'Delete template?',
      body: `Delete the template "${name}"? This removes the blueprint only — any groups already instantiated from it are left untouched.`,
      meta: name,
      okLabel: 'Delete template',
    });
    if (!ok) return;
    try {
      const r = await fetch(`/api/templates/${encodeURIComponent(name)}`, {
        method: 'DELETE', credentials: 'same-origin',
      });
      if (!r.ok && r.status !== 204) { toast((await r.text()) || `HTTP ${r.status}`, true); return; }
      toast(`template deleted: ${name}`);
      refresh();
    } catch (err) {
      toast((err && err.message) || String(err), true);
    }
  }

  // ---- Instantiate-from-template modal ----------------------------------

  function openInstantiateModal(presetName) {
    const templates = (lastSnapshot && lastSnapshot.templates) || [];
    if (!templates.length) {
      toast('no templates yet — define one in the Templates tab first', true);
      return;
    }
    const sel = $('#template-instantiate-template');
    sel.innerHTML = templates.map(t =>
      `<option value="${esc(t.name)}">${esc(t.name)}</option>`).join('');
    if (presetName && templates.some(t => t.name === presetName)) sel.value = presetName;
    $('#template-instantiate-group').value = '';
    $('#template-instantiate-task').value = '';
    $('#template-instantiate-cwd').value = '';
    $('#template-instantiate-error').textContent = '';
    renderInstantiatePreview();
    $('#template-instantiate-modal').classList.add('show');
    setTimeout(() => $('#template-instantiate-group').focus(), 0);
  }

  function closeInstantiateModal() { $('#template-instantiate-modal').classList.remove('show'); }

  // renderInstantiatePreview paints the live "final agent names" list as
  // the human types the group name — agent "PO" shows as "<group>-PO".
  function renderInstantiatePreview() {
    const t = templatesByName()[$('#template-instantiate-template').value];
    const prefix = $('#template-instantiate-group').value.trim();
    const host = $('#template-instantiate-preview');
    const agents = (t && t.agents) || [];
    if (!agents.length) {
      host.innerHTML = '<span class="tp-empty">this template has no agents</span>';
      return;
    }
    const shown = prefix || '‹group›';
    host.innerHTML = agents.map(a => {
      const owner = a.is_owner ? '<span class="tp-owner" title="group owner">★ owner</span>' : '';
      const np = (a.permissions || []).length;
      const meta = [a.role ? esc(a.role) : '', np ? `+${np}🔑` : '', owner]
        .filter(Boolean).join(' · ');
      return `<div class="tp-row"><span class="tp-name">${esc(shown)}-${esc(a.name)}</span>`
        + (meta ? ` <span class="tp-meta">${meta}</span>` : '') + `</div>`;
    }).join('');
  }

  async function submitInstantiate() {
    const tmplName = $('#template-instantiate-template').value;
    const groupName = $('#template-instantiate-group').value.trim();
    const errEl = $('#template-instantiate-error');
    errEl.textContent = '';
    if (!tmplName) { errEl.textContent = 'pick a template'; return; }
    if (!groupName) { errEl.textContent = 'group name is required'; return; }
    const payload = {
      group_name: groupName,
      task: $('#template-instantiate-task').value,
      cwd: $('#template-instantiate-cwd').value.trim(),
    };
    const btn = $('#template-instantiate-submit');
    btn.disabled = true;
    btn.textContent = 'Spawning…';
    try {
      const r = await fetch(`/api/templates/${encodeURIComponent(tmplName)}/instantiate`, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      const txt = await r.text();
      if (!r.ok) { errEl.textContent = txt || `HTTP ${r.status}`; return; }
      let resp = {};
      try { resp = JSON.parse(txt); } catch (_) {}
      closeInstantiateModal();
      const failed = resp.failed || 0;
      toast(failed
        ? `group ${groupName}: spawned ${resp.spawned || 0}, ${failed} failed — check the group`
        : `group ${groupName}: spawned ${resp.spawned || 0} agent${resp.spawned === 1 ? '' : 's'}`,
        failed > 0);
      try { localStorage.setItem('tclaude.dash.group.' + groupName, '1'); } catch (_) {}
      // Jump to the Groups tab so the freshly-spawned group is visible.
      const gbtn = $$('nav button').find(b => b.dataset.tab === 'groups');
      if (gbtn) gbtn.click();
      refresh();
    } catch (err) {
      errEl.textContent = (err && err.message) || String(err);
    } finally {
      btn.disabled = false;
      btn.textContent = 'Create & spawn';
    }
  }

  // ---- Save-group-as-template modal -------------------------------------

  function openFromGroupModal() {
    const groups = ((lastSnapshot && lastSnapshot.groups) || []).map(g => g.name);
    if (!groups.length) { toast('no groups to snapshot', true); return; }
    const sel = $('#template-from-group-group');
    sel.innerHTML = groups.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join('');
    $('#template-from-group-name').value = '';
    $('#template-from-group-error').textContent = '';
    $('#template-from-group-modal').classList.add('show');
    setTimeout(() => $('#template-from-group-name').focus(), 0);
  }

  function closeFromGroupModal() { $('#template-from-group-modal').classList.remove('show'); }

  async function submitFromGroup() {
    const group = $('#template-from-group-group').value;
    const name = $('#template-from-group-name').value.trim();
    const errEl = $('#template-from-group-error');
    errEl.textContent = '';
    if (!group) { errEl.textContent = 'pick a group'; return; }
    if (!name) { errEl.textContent = 'template name is required'; return; }
    const btn = $('#template-from-group-submit');
    btn.disabled = true;
    try {
      const r = await fetch('/api/templates/from-group', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ group, template_name: name }),
      });
      const txt = await r.text();
      if (!r.ok) { errEl.textContent = txt || `HTTP ${r.status}`; return; }
      let tmpl = null;
      try { tmpl = JSON.parse(txt); } catch (_) {}
      closeFromGroupModal();
      toast(`template created from ${group}: ${name}`);
      refresh();
      // Open the editor on the fresh template so the human can fill in
      // per-agent task briefs (from-group leaves those blank).
      if (tmpl) openTemplateEditor(tmpl);
    } catch (err) {
      errEl.textContent = (err && err.message) || String(err);
    } finally {
      btn.disabled = false;
    }
  }

  function bindTemplatesUI() {
    // Entry points: Templates tab + the Groups tab's "⎘ from template".
    $('#template-create-open').addEventListener('click', () => openTemplateEditor(null));
    $('#template-from-group-open').addEventListener('click', openFromGroupModal);
    $('#group-from-template-open').addEventListener('click', () => openInstantiateModal(null));

    // Template-card actions (delegated — the list re-renders every poll).
    // data-tact (not data-act) keeps these off the global row-action bus.
    $('#templates-list').addEventListener('click', e => {
      const btn = e.target.closest('[data-tact]');
      if (!btn) return;
      const name = btn.dataset.template;
      if (btn.dataset.tact === 'instantiate') openInstantiateModal(name);
      else if (btn.dataset.tact === 'edit') {
        const t = templatesByName()[name];
        if (t) openTemplateEditor(t);
      } else if (btn.dataset.tact === 'delete') deleteTemplate(name);
    });

    // Editor modal.
    $('#template-editor-cancel').addEventListener('click', closeTemplateEditor);
    $('#template-editor-submit').addEventListener('click', submitTemplateEditor);
    $('#template-editor-add-agent').addEventListener('click', () => {
      scrapeEditorAgents();
      templateEditorAgents.push(blankTemplateAgent());
      renderEditorAgents();
    });
    // Delegated handlers on the (re-rendered) agent container.
    $('#template-editor-agents').addEventListener('click', e => {
      const rm = e.target.closest('.ta-remove');
      if (!rm) return;
      const row = rm.closest('.template-agent-row');
      scrapeEditorAgents();
      templateEditorAgents.splice(parseInt(row.dataset.idx, 10), 1);
      renderEditorAgents();
    });
    // Keep each agent row's permission count in sync as boxes toggle.
    // Owner is a plain per-agent checkbox — a group can have several
    // owners, so there is no single-select enforcement.
    $('#template-editor-agents').addEventListener('change', e => {
      if (e.target.classList.contains('ta-perm')) {
        const row = e.target.closest('.template-agent-row');
        $('.ta-perms-count', row).textContent =
          $$('.ta-perm', row).filter(c => c.checked).length;
      }
    });
    $('#template-editor-modal').addEventListener('click', e => {
      if (e.target.id === 'template-editor-modal') closeTemplateEditor();
    });

    // Instantiate modal.
    $('#template-instantiate-cancel').addEventListener('click', closeInstantiateModal);
    $('#template-instantiate-submit').addEventListener('click', submitInstantiate);
    $('#template-instantiate-template').addEventListener('change', renderInstantiatePreview);
    $('#template-instantiate-group').addEventListener('input', renderInstantiatePreview);
    $('#template-instantiate-modal').addEventListener('click', e => {
      if (e.target.id === 'template-instantiate-modal') closeInstantiateModal();
    });

    // From-group modal.
    $('#template-from-group-cancel').addEventListener('click', closeFromGroupModal);
    $('#template-from-group-submit').addEventListener('click', submitFromGroup);
    $('#template-from-group-modal').addEventListener('click', e => {
      if (e.target.id === 'template-from-group-modal') closeFromGroupModal();
    });

    // Escape closes whichever template modal is showing.
    document.addEventListener('keydown', e => {
      if (e.key !== 'Escape') return;
      ['template-editor-modal', 'template-instantiate-modal', 'template-from-group-modal']
        .forEach(id => {
          const m = $('#' + id);
          if (m && m.classList.contains('show')) m.classList.remove('show');
        });
    });
  }

  // ---- Import-group modal ------------------------------------------------
  //
  // The ⤒ import button uploads a .zip produced by ⤓ export and
  // recreates the group on this machine. Browsers cannot stream a raw
  // body the way the CLI does, so the file rides in a multipart form to
  // POST /api/groups/import.
  //
  // Before committing, the modal POSTs the same archive to
  // /api/groups/import/inspect — a server-side dry run that returns the
  // manifest summary plus a collision report (does the group name exist
  // here? which conv-ids will be remapped to "-i-N" copies?) without
  // writing anything. The Import button stays disabled until that
  // preview is clean, so an import is never a blind action; a malformed
  // or unsupported archive surfaces its error in the preview and blocks
  // the confirm outright.

  let giInspectSeq = 0;        // monotonic — stale inspect responses are dropped
  let giLastInspection = null; // last successful inspection JSON, or null
  let giAsDebounce = null;     // debounce timer for the "Import as" field

  function openGroupImportModal() {
    $('#group-import-file').value = '';
    $('#group-import-into').value = '';
    $('#group-import-as').value = '';
    $('#group-import-error').textContent = '';
    giLastInspection = null;
    giInspectSeq++; // invalidate any inspect still in flight from a prior open
    const prev = $('#group-import-preview');
    prev.style.display = 'none';
    prev.innerHTML = '';
    $('#group-import-submit').disabled = true;
    $('#group-import-submit').textContent = 'Import';
    $('#group-import-modal').classList.add('show');
    setTimeout(() => $('#group-import-file').focus(), 0);
  }

  function closeGroupImportModal() {
    $('#group-import-modal').classList.remove('show');
    if (giAsDebounce) { clearTimeout(giAsDebounce); giAsDebounce = null; }
  }

  // groupImportInspect uploads the picked .zip to the dry-run endpoint
  // and renders the preview. Each call bumps giInspectSeq; a response is
  // applied only while it is still the latest request, so a fast re-pick
  // or an "Import as" edit can't let a stale preview win.
  async function groupImportInspect() {
    const fileEl = $('#group-import-file');
    const file = fileEl.files && fileEl.files[0];
    if (!file) {
      giLastInspection = null;
      $('#group-import-preview').style.display = 'none';
      $('#group-import-error').textContent = '';
      refreshGroupImportSubmitState();
      return;
    }
    const seq = ++giInspectSeq;
    const fd = new FormData();
    fd.append('archive', file);
    const asName = $('#group-import-as').value.trim();
    if (asName) fd.append('as', asName);

    const prev = $('#group-import-preview');
    prev.style.display = 'flex';
    prev.innerHTML = '<div class="gi-head">Inspecting archive…</div>';
    $('#group-import-error').textContent = '';
    $('#group-import-submit').disabled = true;

    let r, body;
    try {
      r = await fetch('/api/groups/import/inspect', {
        method: 'POST', credentials: 'same-origin', body: fd,
      });
      body = await r.json().catch(() => null);
    } catch (err) {
      if (seq !== giInspectSeq) return;
      giLastInspection = null;
      renderGroupImportPreviewError((err && err.message) || String(err));
      refreshGroupImportSubmitState();
      return;
    }
    if (seq !== giInspectSeq) return; // a newer inspect superseded this one

    if (!r.ok) {
      // Malformed / corrupt / unsupported-version archive — block confirm.
      giLastInspection = null;
      renderGroupImportPreviewError((body && body.error) || ('HTTP ' + r.status));
      refreshGroupImportSubmitState();
      return;
    }
    giLastInspection = body;
    renderGroupImportPreview();
  }

  function renderGroupImportPreviewError(msg) {
    const prev = $('#group-import-preview');
    prev.style.display = 'flex';
    prev.innerHTML =
      '<div class="gi-head">Archive</div>' +
      '<div class="gi-verdict gi-bad">✗ ' + esc(msg) + '</div>' +
      '<div class="gi-bad">This file is not an importable group archive — pick a .zip produced by the ⤓ export button.</div>';
  }

  // renderGroupImportPreview paints the manifest summary + collision
  // report + verdict from giLastInspection. Also re-run when "Into dir"
  // changes, since the verdict line depends on it.
  function renderGroupImportPreview() {
    const insp = giLastInspection;
    const prev = $('#group-import-preview');
    if (!insp) {
      prev.style.display = 'none';
      refreshGroupImportSubmitState();
      return;
    }
    prev.style.display = 'flex';

    const row = (k, v, cls) =>
      '<div class="gi-row"><span class="gi-k">' + esc(k) + '</span>' +
      '<span class="gi-v ' + (cls || '') + '">' + esc(v) + '</span></div>';

    let h = '<div class="gi-head">Archive contents</div>';
    h += row('Source group', insp.source_group || '(unnamed)');
    h += row('Agents', String(insp.agent_count));
    h += row('Messages', String(insp.message_count));
    let convs = insp.conv_count + ' conversation' + (insp.conv_count === 1 ? '' : 's');
    if (insp.missing_convs > 0) convs += ' (' + insp.missing_convs + ' with no .jsonl content)';
    h += row('Conversations', convs);
    if (insp.source_os || insp.source_home) {
      h += row('Source machine',
        (insp.source_os || '?') + (insp.source_home ? ', home ' + insp.source_home : ''));
    }
    if (insp.exported_at) h += row('Exported', insp.exported_at);
    h += row('Format version', 'v' + insp.format_version + ' — supported', 'gi-ok');

    h += '<div class="gi-sep gi-head">Collisions on this machine</div>';
    const collisions = insp.conv_collisions || [];
    if (collisions.length === 0) {
      h += '<div class="gi-ok">✓ No conv-id collisions — every conversation id is preserved.</div>';
    } else {
      h += '<div class="gi-warn">⚠ ' + collisions.length + ' conversation' +
        (collisions.length === 1 ? '' : 's') +
        ' already exist locally — each is imported as a fresh copy, its agent retitled “-i-N”:</div>';
      h += '<ul class="gi-collide-list">';
      collisions.forEach((c) => {
        h += '<li>' + esc(c.title || c.conv_id) +
          ' <span class="gi-k">(' + esc((c.conv_id || '').slice(0, 8)) + ')</span></li>';
      });
      h += '</ul>';
    }

    // Verdict — exactly what enables or blocks the Import button.
    h += '<div class="gi-sep"></div>';
    const into = $('#group-import-into').value.trim();
    if (!insp.target_name_valid) {
      h += '<div class="gi-verdict gi-bad">✗ Invalid group name “' + esc(insp.target_name) +
        '”: ' + esc(insp.target_name_error || '') + '</div>';
    } else if (insp.group_name_taken) {
      h += '<div class="gi-verdict gi-bad">✗ A group named “' + esc(insp.target_name) +
        '” already exists here. Fill “Import as” with a free name.</div>';
    } else if (!into) {
      h += '<div class="gi-verdict gi-warn">⚠ Fill “Into dir” with a target directory to enable the import.</div>';
    } else {
      h += '<div class="gi-verdict gi-ok">✓ Ready — ' + insp.agent_count + ' agent' +
        (insp.agent_count === 1 ? '' : 's') + ' will be imported into group “' +
        esc(insp.target_name) + '”.</div>';
    }
    prev.innerHTML = h;
    refreshGroupImportSubmitState();
  }

  // refreshGroupImportSubmitState enables Import only when the latest
  // dry run is clean: archive parsed, target name valid and free, and a
  // target directory has been entered.
  function refreshGroupImportSubmitState() {
    const insp = giLastInspection;
    const into = $('#group-import-into').value.trim();
    const ok = !!insp && insp.target_name_valid && !insp.group_name_taken && into !== '';
    $('#group-import-submit').disabled = !ok;
  }

  async function submitGroupImport() {
    const fileEl = $('#group-import-file');
    const file = fileEl.files && fileEl.files[0];
    const into = $('#group-import-into').value.trim();
    const asName = $('#group-import-as').value.trim();
    const errEl = $('#group-import-error');
    errEl.textContent = '';
    if (!file) { errEl.textContent = 'pick a .zip archive first'; return; }
    if (!into) { errEl.textContent = 'a target directory (Into dir) is required'; return; }

    const fd = new FormData();
    fd.append('archive', file);
    fd.append('into', into);
    if (asName) fd.append('as', asName);

    const submitBtn = $('#group-import-submit');
    submitBtn.disabled = true;
    submitBtn.textContent = 'Importing…';
    try {
      const r = await fetch('/api/groups/import', {
        method: 'POST', credentials: 'same-origin', body: fd,
      });
      const body = await r.json().catch(() => null);
      if (!r.ok) {
        // The import is transactional — a failure wrote nothing at all.
        errEl.textContent = 'Import failed: ' + ((body && body.error) || ('HTTP ' + r.status)) +
          ' — nothing was written. The import is all-or-nothing, so the group, its agents and' +
          ' conversations are exactly as before. Adjust the fields and try again.';
        return;
      }
      closeGroupImportModal();
      let summary = 'Imported group "' + body.group + '" — ' +
        body.agent_count + ' agent(s), ' + body.message_count + ' message(s)';
      const remaps = body.conv_remaps ? Object.keys(body.conv_remaps).length : 0;
      if (remaps > 0) summary += ' (' + remaps + ' conv-id(s) remapped to fresh copies)';
      const warnings = body.file_warnings || [];
      if (warnings.length > 0) {
        toast(summary + ' — ' + warnings.length + ' file warning(s); see the daemon log', true);
      } else {
        toast(summary);
      }
      // Show the imported group expanded on the next render.
      try { localStorage.setItem('tclaude.dash.group.' + body.group, '1'); } catch (_) {}
      refresh();
    } catch (err) {
      errEl.textContent = 'Import failed: ' + ((err && err.message) || String(err)) +
        ' — nothing was written.';
    } finally {
      submitBtn.textContent = 'Import';
      refreshGroupImportSubmitState();
    }
  }

  function bindGroupImportModal() {
    $('#group-import-open').addEventListener('click', openGroupImportModal);
    $('#group-import-cancel').addEventListener('click', closeGroupImportModal);
    $('#group-import-submit').addEventListener('click', submitGroupImport);
    // Picking (or changing) the file re-runs the dry-run preview.
    $('#group-import-file').addEventListener('change', groupImportInspect);
    // "Into dir" does not affect the archive analysis — collisions are
    // group-name + conv-id — so it only re-evaluates the verdict locally.
    $('#group-import-into').addEventListener('input', renderGroupImportPreview);
    // "Import as" DOES change the collision check (a different target
    // name), so editing it re-runs inspect — debounced so a burst of
    // keystrokes collapses into one request.
    $('#group-import-as').addEventListener('input', () => {
      if (giAsDebounce) clearTimeout(giAsDebounce);
      giAsDebounce = setTimeout(groupImportInspect, 350);
    });
    $('#group-import-modal').addEventListener('click', (e) => {
      if (e.target.id === 'group-import-modal') closeGroupImportModal();
    });
    $('#group-import-modal').addEventListener('keydown', (e) => {
      if (e.key === 'Enter' &&
          (e.target.id === 'group-import-into' || e.target.id === 'group-import-as') &&
          !$('#group-import-submit').disabled) {
        e.preventDefault();
        submitGroupImport();
      }
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && $('#group-import-modal').classList.contains('show')) {
        closeGroupImportModal();
      }
    });
  }

  // ---- Group startup-context modal ---------------------------------------
  //
  // Edits a group's default_context — the shared block of guidance
  // injected into every agent spawned into the group. The cwd chip
  // edits inline; context is multi-line so it gets a modal textarea.
  // Save PATCHes /api/groups/{name} with {default_context}.

  // groupDefaultContext looks up a group's startup context from the
  // latest snapshot. "" when the group is unknown or has none.
  function groupDefaultContext(groupName) {
    const groups = (lastSnapshot && lastSnapshot.groups) || [];
    const g = groups.find(x => x.name === groupName);
    return (g && g.default_context) || '';
  }

  // The group whose context the modal is currently editing.
  let groupContextModalGroup = '';

  function openGroupContextModal(groupName) {
    groupContextModalGroup = groupName;
    $('#group-context-text').value = groupDefaultContext(groupName);
    $('#group-context-error').textContent = '';
    const meta = $('#group-context-meta');
    meta.textContent = `group: ${groupName}`;
    meta.style.display = '';
    $('#group-context-modal').classList.add('show');
    setTimeout(() => $('#group-context-text').focus(), 0);
  }

  function closeGroupContextModal() {
    $('#group-context-modal').classList.remove('show');
    groupContextModalGroup = '';
  }

  async function submitGroupContext() {
    const group = groupContextModalGroup;
    if (!group) { closeGroupContextModal(); return; }
    const context = $('#group-context-text').value.trim();
    const errEl = $('#group-context-error');
    errEl.textContent = '';
    const submitBtn = $('#group-context-submit');
    submitBtn.disabled = true;
    try {
      const r = await fetch(`/api/groups/${encodeURIComponent(group)}`, {
        method: 'PATCH', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ default_context: context }),
      });
      if (!r.ok) {
        errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
        return;
      }
      closeGroupContextModal();
      toast(context ? `${group}: startup context updated` : `${group}: startup context cleared`);
      refresh();
    } catch (err) {
      errEl.textContent = (err && err.message) || String(err);
    } finally {
      submitBtn.disabled = false;
    }
  }

  function bindGroupContextModal() {
    $('#group-context-cancel').addEventListener('click', closeGroupContextModal);
    $('#group-context-submit').addEventListener('click', submitGroupContext);
    $('#group-context-modal').addEventListener('click', (e) => {
      if (e.target.id === 'group-context-modal') closeGroupContextModal();
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && $('#group-context-modal').classList.contains('show')) {
        closeGroupContextModal();
      }
    });
  }

  // ---- Link modal (create + edit) ----------------------------------------
  //
  // One modal, two states:
  //   openLinkModal({ mode: 'create', preset: {from?, to?, linkMode?} })
  //   openLinkModal({ mode: 'edit', linkID, preset: {from, to, linkMode} })
  //
  // Create posts to /api/groups/{from}/links with body {to, mode, bidir?};
  // edit patches /api/groups/{from-or-to}/links/{linkID} with {mode}.
  // Edit hides from/to fields (those are immutable on a link) and the
  // bidir checkbox (no meaning when editing a single existing row).

  let linkModalState = { mode: 'create', linkID: null };

  function openLinkModal(opts) {
    opts = opts || {};
    const state = opts.mode === 'edit' ? 'edit' : 'create';
    const preset = opts.preset || {};
    linkModalState = { mode: state, linkID: opts.linkID || null, fromForEdit: preset.from || '' };
    const groups = ((lastSnapshot && lastSnapshot.groups) || []).map(g => g.name);
    const fromSel = $('#link-modal-from');
    const toSel = $('#link-modal-to');
    fromSel.innerHTML = groups.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join('');
    toSel.innerHTML = groups.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join('');
    // Defensive: paste in preset values that aren't in the snapshot yet.
    for (const [sel, val] of [[fromSel, preset.from], [toSel, preset.to]]) {
      if (val && ![...sel.options].some(o => o.value === val)) {
        const opt = document.createElement('option');
        opt.value = val;
        opt.textContent = val;
        sel.appendChild(opt);
      }
    }
    if (preset.from) fromSel.value = preset.from;
    else if (groups.length) fromSel.value = groups[0];
    if (preset.to) toSel.value = preset.to;
    else if (groups.length > 1) toSel.value = groups[1];

    $('#link-modal-mode').value = preset.linkMode || 'members->members';
    $('#link-modal-bidir').checked = false;
    $('#link-modal-error').textContent = '';

    const title = $('#link-modal-title');
    const meta = $('#link-modal-meta');
    const submit = $('#link-modal-submit');
    const bidirRow = $('#link-modal-bidir-row');
    if (state === 'edit') {
      title.textContent = 'Edit link mode';
      meta.textContent = `#${opts.linkID} · ${preset.from} → ${preset.to}`;
      meta.style.display = '';
      submit.textContent = 'Save changes';
      // From/To are immutable when editing — show as disabled.
      fromSel.disabled = true;
      toSel.disabled = true;
      bidirRow.style.display = 'none';
    } else {
      title.textContent = 'Add inter-group link';
      meta.style.display = 'none';
      submit.textContent = 'Create link';
      fromSel.disabled = !!preset.from; // fix FROM when invoked from a group card
      toSel.disabled = false;
      bidirRow.style.display = '';
    }

    $('#link-modal').classList.add('show');
    setTimeout(() => {
      if (state === 'edit') $('#link-modal-mode').focus();
      else if (preset.from && !preset.to) toSel.focus();
      else fromSel.focus();
    }, 0);
  }

  function closeLinkModal() {
    $('#link-modal').classList.remove('show');
  }

  async function submitLinkModal() {
    const state = linkModalState.mode;
    const errEl = $('#link-modal-error');
    errEl.textContent = '';
    const submit = $('#link-modal-submit');
    const mode = $('#link-modal-mode').value;
    submit.disabled = true;
    try {
      if (state === 'edit') {
        const id = linkModalState.linkID;
        const scope = linkModalState.fromForEdit || $('#link-modal-from').value;
        const r = await fetch(`/api/groups/${encodeURIComponent(scope)}/links/${encodeURIComponent(id)}`, {
          method: 'PATCH', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ mode }),
        });
        if (!r.ok) {
          errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
          return;
        }
        closeLinkModal();
        toast(`link #${id} mode → ${mode}`);
      } else {
        const from = $('#link-modal-from').value;
        const to = $('#link-modal-to').value;
        if (!from || !to) {
          errEl.textContent = 'from and to are required';
          return;
        }
        if (from === to) {
          errEl.textContent = 'from and to must differ — use group membership for intra-group comm';
          return;
        }
        const bidir = $('#link-modal-bidir').checked;
        const r = await fetch(`/api/groups/${encodeURIComponent(from)}/links`, {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ to, mode, bidir }),
        });
        if (!r.ok) {
          errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
          return;
        }
        closeLinkModal();
        toast(`linked: ${from} → ${to}${bidir ? ' (+reverse)' : ''}`);
      }
      refresh();
    } catch (err) {
      errEl.textContent = (err && err.message) || String(err);
    } finally {
      submit.disabled = false;
    }
  }

  function bindLinkModal() {
    $('#link-new-open').addEventListener('click', () => openLinkModal({ mode: 'create' }));
    $('#link-modal-cancel').addEventListener('click', closeLinkModal);
    $('#link-modal-submit').addEventListener('click', submitLinkModal);
    $('#link-modal').addEventListener('click', (e) => {
      if (e.target.id === 'link-modal') closeLinkModal();
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && $('#link-modal').classList.contains('show')) {
        closeLinkModal();
      }
    });
  }

  // ---- Worktree picker (shared by spawn + clone modals) -------------------
  //
  // A worktree picker is a <select> plus two reveal-on-demand rows,
  // identified by an element-id prefix (`agent-spawn` or `clone-agent`):
  //
  //   #<prefix>-worktree      <select> — none / existing worktrees / "+ create"
  //   #<prefix>-wt-new-row    new-branch row, shown only for "+ create"
  //   #<prefix>-wt-base-row   base-branch row, same visibility
  //   #<prefix>-wt-branch     new branch name (text input)
  //   #<prefix>-wt-base       base branch (<select>)
  //
  // wtRefresh repopulates the <select> from GET /api/worktrees; on
  // submit, wtResolveCwd turns the picker state into the directory the
  // spawn/clone should run in — POSTing /api/worktrees first when the
  // human chose "+ create". The whole feature is opt-in: the default
  // "" option leaves spawn/clone behaviour exactly as before.

  const WT_NEW = '__new__';

  // wtToggleNew shows/hides the new-branch + base-branch rows.
  function wtToggleNew(prefix, show) {
    $(`#${prefix}-wt-new-row`).style.display = show ? '' : 'none';
    $(`#${prefix}-wt-base-row`).style.display = show ? '' : 'none';
  }

  // wtRefresh repopulates the worktree <select> for `repo`. A missing
  // or non-repo `repo` disables the picker (spawn/clone then just uses
  // the directory as-is). noneLabel is the text of the leading "no
  // worktree" option — phrased per modal.
  async function wtRefresh(prefix, repo, noneLabel) {
    const select = $(`#${prefix}-worktree`);
    wtToggleNew(prefix, false);
    repo = (repo || '').trim();
    select.dataset.repoRoot = '';
    if (!repo) {
      select.innerHTML = '<option value="">(enter a CWD to enable worktrees)</option>';
      select.disabled = true;
      return;
    }
    select.disabled = true;
    select.innerHTML = '<option value="">loading…</option>';
    let data = {};
    try {
      const r = await fetch(`/api/worktrees?repo=${encodeURIComponent(repo)}`, { credentials: 'same-origin' });
      data = await r.json();
    } catch (_) { data = {}; }
    // Stale-guard: if the picker was reloaded for a different repo
    // while this fetch was in flight, drop the result.
    if ((select.dataset.pendingRepo || '') !== repo) return;
    if (!data || !data.is_repo) {
      // Not a git repo. If the daemon found nested repos under it
      // (a "virtual monorepo" launch dir), point the human at the
      // Worktree-repo field — wtFillSubRepos populates its datalist.
      const hasSubs = data && Array.isArray(data.sub_repos) && data.sub_repos.length;
      select.innerHTML = hasSubs
        ? '<option value="">(not a git repo — pick a sub-repo in "Worktree repo" above)</option>'
        : '<option value="">(not a git repo — worktrees unavailable)</option>';
      select.disabled = true;
      wtFillSubRepos(prefix, data && data.sub_repos);
      return;
    }
    select.dataset.repoRoot = data.repo_root || '';
    const opts = [`<option value="">${esc(noneLabel || '(no worktree)')}</option>`];
    (data.worktrees || []).forEach(wt => {
      const br = wt.branch || '(detached)';
      const tag = wt.is_main ? ' [main]' : '';
      opts.push(`<option value="wt:${esc(wt.path)}" data-branch="${esc(wt.branch || '')}">${esc(br)}${tag} — ${esc(shortCwd(wt.path))}</option>`);
    });
    opts.push(`<option value="${WT_NEW}">+ create new worktree…</option>`);
    select.innerHTML = opts.join('');
    select.disabled = false;
    const base = $(`#${prefix}-wt-base`);
    base.innerHTML = (data.branches || []).map(b => `<option value="${esc(b)}">${esc(b)}</option>`).join('');
    if (data.default_branch) base.value = data.default_branch;
  }

  // wtLoad sets the pending-repo guard then refreshes. Call this rather
  // than wtRefresh directly so overlapping loads resolve correctly.
  function wtLoad(prefix, repo, noneLabel) {
    $(`#${prefix}-worktree`).dataset.pendingRepo = (repo || '').trim();
    return wtRefresh(prefix, repo, noneLabel);
  }

  // bindWtPicker wires the <select>'s change event once at startup.
  function bindWtPicker(prefix) {
    $(`#${prefix}-worktree`).addEventListener('change', (e) => {
      wtToggleNew(prefix, e.target.value === WT_NEW);
    });
  }

  // wtFillSubRepos populates a modal's sub-repo <datalist> from a
  // /api/worktrees response's `sub_repos`. Only the spawn modal has
  // the datalist (`<prefix>-subrepo-list`); for the clone picker this
  // is a no-op. A null/empty list is left alone rather than cleared —
  // once the human drills from the monorepo into one sub-repo the
  // suggestions stay available so they can switch to a sibling.
  function wtFillSubRepos(prefix, subRepos) {
    const list = document.getElementById(`${prefix}-subrepo-list`);
    if (!list) return;
    if (Array.isArray(subRepos) && subRepos.length) {
      list.innerHTML = subRepos
        .map(s => `<option value="${esc(s.path)}">${esc(s.rel)}</option>`)
        .join('');
    }
  }

  // wtResolve turns the current worktree-picker state into a
  // {path, branch} selection. An empty path means "no worktree
  // chosen" — the caller applies its own fallback. `repo` is the
  // directory the picker loaded against. Throws on a creation
  // failure so the caller can surface it.
  async function wtResolve(prefix, repo) {
    const select = $(`#${prefix}-worktree`);
    const val = select.value || '';
    if (!val) return { path: '', branch: '' };
    if (val.startsWith('wt:')) {
      const opt = select.selectedOptions[0];
      return { path: val.slice(3), branch: (opt && opt.dataset.branch) || '' };
    }
    if (val === WT_NEW) {
      const branch = $(`#${prefix}-wt-branch`).value.trim();
      if (!branch) throw new Error('enter a branch name for the new worktree');
      const root = select.dataset.repoRoot || repo;
      const r = await fetch('/api/worktrees', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ repo: root, branch, from_branch: $(`#${prefix}-wt-base`).value || '' }),
      });
      if (!r.ok) throw new Error((await r.text()) || `HTTP ${r.status}`);
      const j = await r.json();
      return { path: j.path || '', branch: j.branch || branch };
    }
    return { path: '', branch: '' };
  }

  // wtResolveCwd is the cwd-only view of wtResolve, kept for the clone
  // modal: it returns the worktree path or `fallback` when none is
  // selected. Throws on a worktree-creation failure.
  async function wtResolveCwd(prefix, repo, fallback) {
    const sel = await wtResolve(prefix, repo);
    return sel.path || fallback;
  }

  // ---- Agent spawn modal --------------------------------------------------
  //
  // Opens with `{groupName}` pre-filled from a group header's
  // "+ spawn agent" button — the group is fixed and the <select> stays
  // hidden. (The form still supports an empty open, showing the group
  // <select>, for any future caller.) On submit it POSTs to
  // /api/groups/{name}/spawn, which forks `tclaude session new` and waits
  // for the conv-id before returning.

  // Tracks the cwd value the spawn form last auto-filled from a group
  // default, so switching the group <select> can refresh the prefill
  // without clobbering a path the user typed by hand.
  let lastSpawnCwdPrefill = '';

  // True once the human has typed in the "Worktree repo" field. Until
  // then that field mirrors CWD; after, CWD changes leave it alone so
  // a deliberately-pointed sub-repo path isn't clobbered.
  let spawnWtRepoEdited = false;

  // groupDefaultCwd looks up a group's default spawn dir from the
  // latest snapshot. "" when the group is unknown or has no default.
  function groupDefaultCwd(groupName) {
    const groups = (lastSnapshot && lastSnapshot.groups) || [];
    const g = groups.find(x => x.name === groupName);
    return (g && g.default_cwd) || '';
  }

  // spawnAutoFocusPref reads the persisted "auto focus" checkbox state
  // for the spawn modal. Defaults to true: a freshly-spawned agent runs
  // detached with no window, so the common case is wanting one opened.
  function spawnAutoFocusPref() {
    try {
      const v = localStorage.getItem('tclaude.dash.spawn.autofocus');
      return v === null ? true : v === '1';
    } catch (_) { return true; }
  }

  // prefillSpawnCwd fills #agent-spawn-cwd with the group's default
  // dir. With force=false it leaves a user-typed value alone — it
  // only overwrites an empty field or a stale prior auto-prefill.
  function prefillSpawnCwd(groupName, force) {
    const cwdEl = $('#agent-spawn-cwd');
    if (!force && cwdEl.value.trim() !== '' && cwdEl.value !== lastSpawnCwdPrefill) {
      return;
    }
    const dflt = groupDefaultCwd(groupName);
    cwdEl.value = dflt;
    lastSpawnCwdPrefill = dflt;
  }

  // updateSpawnGroupContextRow shows the "include group default
  // context" checkbox only when the selected group actually has a
  // startup context — there's nothing to opt into otherwise. The
  // checkbox is (re)set to checked whenever the row becomes visible
  // so switching groups always lands on the opt-in default.
  function updateSpawnGroupContextRow(groupName) {
    const hasContext = groupDefaultContext(groupName).trim() !== '';
    $('#agent-spawn-group-context-row').style.display = hasContext ? '' : 'none';
    if (hasContext) $('#agent-spawn-group-context').checked = true;
  }

  // Label for the leading "no worktree" option in the spawn modal's
  // worktree picker.
  const SPAWN_WT_NONE = '(no worktree — use CWD above)';

  // applyWtSync reflects the "Sync worktree branch with name"
  // checkbox into the spawn modal's worktree picker. Call it after
  // the picker (re)loads, after the name changes, and whenever the
  // checkbox itself is toggled.
  //
  // The sync only works when the picker landed on a usable git repo —
  // wtRefresh leaves the <select> disabled in every other state ((no
  // CWD), (not a repo), still loading) — so the checkbox is disabled
  // to match. When checked with a non-empty name it forces the
  // picker into "+ create new worktree" and mirrors the name into
  // the new-branch field; clearing the name drops it back to "no
  // worktree".
  function applyWtSync() {
    const syncEl = $('#agent-spawn-wt-sync');
    const select = $('#agent-spawn-worktree');
    const usable = !select.disabled;
    syncEl.disabled = !usable;
    $('#agent-spawn-wt-sync-row').classList.toggle('disabled', !usable);
    if (!usable || !syncEl.checked) return;
    const name = $('#agent-spawn-name').value.trim();
    if (name) {
      if (select.value !== WT_NEW) select.value = WT_NEW;
      wtToggleNew('agent-spawn', true);
      $('#agent-spawn-wt-branch').value = name;
    } else if (select.value === WT_NEW) {
      // Name cleared while syncing — fall back to "no worktree".
      select.value = '';
      wtToggleNew('agent-spawn', false);
      $('#agent-spawn-wt-branch').value = '';
    }
  }

  // spawnWtLoad reloads the spawn worktree picker for `cwd`, then
  // re-applies the name-sync checkbox once the list settles (the
  // checkbox's usable state depends on whether `cwd` is a git repo).
  function spawnWtLoad(cwd) {
    return wtLoad('agent-spawn', cwd, SPAWN_WT_NONE).then(applyWtSync);
  }

  function openAgentSpawnModal(opts) {
    const groupName = (opts && opts.groupName) || '';
    const groupRow = $('#agent-spawn-group-row');
    const select = $('#agent-spawn-group');
    // Populate the <select> from the latest snapshot. The select stays
    // hidden when groupName is fixed; we still set the value so submit
    // can read it from one place.
    const groups = (lastSnapshot && lastSnapshot.groups) || [];
    select.innerHTML = groups.map(g => `<option value="${esc(g.name)}">${esc(g.name)}</option>`).join('');
    if (groupName) {
      // Pre-pinned: append/select the target group even if it isn't in
      // the snapshot yet (paranoid — the user just clicked its header
      // so it must be there, but defend anyway).
      if (![...select.options].some(o => o.value === groupName)) {
        const opt = document.createElement('option');
        opt.value = groupName;
        opt.textContent = groupName;
        select.appendChild(opt);
      }
      select.value = groupName;
      groupRow.style.display = 'none';
    } else {
      groupRow.style.display = '';
      if (!select.value && groups.length) select.value = groups[0].name;
    }
    $('#agent-spawn-name').value = '';
    $('#agent-spawn-role').value = '';
    $('#agent-spawn-descr').value = '';
    $('#agent-spawn-init-msg').value = '';
    $('#agent-spawn-cwd').value = '';
    // Restore the auto-focus checkbox from the human's last choice
    // (defaults on — see spawnAutoFocusPref).
    $('#agent-spawn-focus').checked = spawnAutoFocusPref();
    // Prefill the cwd from the selected group's default spawn dir.
    // force=true: the modal just opened fresh, so there's no
    // user-typed value to protect.
    prefillSpawnCwd(select.value, true);
    // Show the "include group default context" checkbox iff the
    // selected group carries a startup context.
    updateSpawnGroupContextRow(select.value);
    $('#agent-spawn-wt-branch').value = '';
    // The worktree picker targets a separate "Worktree repo" field.
    // It mirrors CWD until the human edits it; for a monorepo CWD the
    // field's datalist offers the nested repos to drill into.
    spawnWtRepoEdited = false;
    $('#agent-spawn-subrepo-list').innerHTML = '';
    $('#agent-spawn-wt-repo').value = $('#agent-spawn-cwd').value;
    // Restore the name→branch sync to its default-on state.
    $('#agent-spawn-wt-sync').checked = true;
    // Load the worktree picker against the Worktree-repo field, then
    // apply the name-sync checkbox once it settles.
    spawnWtLoad($('#agent-spawn-wt-repo').value.trim());
    $('#agent-spawn-error').textContent = '';
    const meta = $('#agent-spawn-meta');
    if (groupName) {
      meta.textContent = `joining group: ${groupName}`;
      meta.style.display = '';
    } else {
      meta.style.display = 'none';
    }
    $('#agent-spawn-modal').classList.add('show');
    setTimeout(() => {
      if (groupName) $('#agent-spawn-name').focus();
      else select.focus();
    }, 0);
  }

  function closeAgentSpawnModal() {
    $('#agent-spawn-modal').classList.remove('show');
  }

  async function submitAgentSpawn() {
    const group = $('#agent-spawn-group').value;
    const name = $('#agent-spawn-name').value.trim();
    const role = $('#agent-spawn-role').value.trim();
    const descr = $('#agent-spawn-descr').value.trim();
    // The initial message is delivered to the new agent's inbox (an
    // agent_messages row), not typed into its pane — so newlines are
    // preserved. Send the textarea verbatim; the daemon trims it.
    const initMsg = $('#agent-spawn-init-msg').value;
    const cwd = $('#agent-spawn-cwd').value.trim();
    const wtRepo = $('#agent-spawn-wt-repo').value.trim();
    const autoFocus = $('#agent-spawn-focus').checked;
    const includeGroupContext = $('#agent-spawn-group-context').checked;
    const errEl = $('#agent-spawn-error');
    errEl.textContent = '';
    if (!group) {
      errEl.textContent = 'group is required';
      return;
    }
    // Persist the checkbox so the human's choice sticks across spawns.
    try { localStorage.setItem('tclaude.dash.spawn.autofocus', autoFocus ? '1' : '0'); } catch (_) {}
    const submitBtn = $('#agent-spawn-submit');
    submitBtn.disabled = true;
    submitBtn.textContent = 'Spawning…';
    try {
      // Resolve the worktree picker (it targets the "Worktree repo"
      // field, which may differ from CWD). Two outcomes:
      //   • Worktree repo == CWD → the worktree becomes the spawn cwd
      //     (the long-standing single-directory behaviour).
      //   • Worktree repo is a sub-repo of a monorepo CWD → the agent
      //     still launches in CWD; the worktree path + branch ride
      //     along so the daemon's welcome points the agent at it.
      const sel = await wtResolve('agent-spawn', wtRepo);
      const body = { name, role, descr, initial_message: initMsg, auto_focus: autoFocus, include_group_context: includeGroupContext };
      if (sel.path && wtRepo && wtRepo !== cwd) {
        body.cwd = cwd;
        body.worktree_path = sel.path;
        body.worktree_branch = sel.branch;
      } else if (sel.path) {
        body.cwd = sel.path;
      } else {
        body.cwd = cwd;
      }
      const r = await fetch(`/api/groups/${encodeURIComponent(group)}/spawn`, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!r.ok) {
        errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
        return;
      }
      let payload = {};
      try { payload = await r.json(); } catch (_) {}
      closeAgentSpawnModal();
      const label = name || (payload.conv_id ? shortId(payload.conv_id) : 'agent');
      toast(`spawned ${label} → ${group}${autoFocus ? ' — opening terminal' : ''}`);
      // Keep the destination group expanded so the new member is visible.
      try { localStorage.setItem('tclaude.dash.group.' + group, '1'); } catch (_) {}
      refresh();
    } catch (err) {
      errEl.textContent = (err && err.message) || String(err);
    } finally {
      submitBtn.disabled = false;
      submitBtn.textContent = 'Spawn';
    }
  }

  function bindAgentSpawnModal() {
    // The spawn modal is opened per-group from each group's
    // "+ spawn agent" button (data-act="spawn-agent"); it has no
    // global open button. Switching the group <select> re-prefills
    // the cwd from the newly-chosen group's default, mirrors it into
    // Worktree-repo (unless the human pinned that), and reloads the
    // picker.
    $('#agent-spawn-group').addEventListener('change', (e) => {
      prefillSpawnCwd(e.target.value, false);
      updateSpawnGroupContextRow(e.target.value);
      if (!spawnWtRepoEdited) $('#agent-spawn-wt-repo').value = $('#agent-spawn-cwd').value;
      spawnWtLoad($('#agent-spawn-wt-repo').value.trim());
    });
    $('#agent-spawn-cancel').addEventListener('click', closeAgentSpawnModal);
    $('#agent-spawn-submit').addEventListener('click', submitAgentSpawn);
    bindWtPicker('agent-spawn');
    // Name-sync wiring: typing in the name mirrors into the
    // worktree branch; toggling the checkbox re-applies the sync;
    // hand-editing the branch or picking a worktree by hand turns the
    // sync off so it stops fighting the human.
    $('#agent-spawn-name').addEventListener('input', applyWtSync);
    $('#agent-spawn-wt-sync').addEventListener('change', applyWtSync);
    $('#agent-spawn-wt-branch').addEventListener('input', () => {
      $('#agent-spawn-wt-sync').checked = false;
    });
    $('#agent-spawn-worktree').addEventListener('change', (e) => {
      if (e.target.value !== WT_NEW) $('#agent-spawn-wt-sync').checked = false;
    });
    // Re-list worktrees when the CWD field settles (debounced). CWD
    // mirrors into Worktree-repo until the human edits the latter.
    let spawnCwdTimer;
    $('#agent-spawn-cwd').addEventListener('input', () => {
      clearTimeout(spawnCwdTimer);
      spawnCwdTimer = setTimeout(() => {
        if (!spawnWtRepoEdited) $('#agent-spawn-wt-repo').value = $('#agent-spawn-cwd').value;
        spawnWtLoad($('#agent-spawn-wt-repo').value.trim());
      }, 350);
    });
    // Editing "Worktree repo" detaches it from CWD and reloads the
    // picker against the typed/picked repo (e.g. a monorepo sub-repo).
    let spawnWtRepoTimer;
    $('#agent-spawn-wt-repo').addEventListener('input', () => {
      spawnWtRepoEdited = true;
      clearTimeout(spawnWtRepoTimer);
      spawnWtRepoTimer = setTimeout(() => {
        spawnWtLoad($('#agent-spawn-wt-repo').value.trim());
      }, 350);
    });
    $('#agent-spawn-modal').addEventListener('click', (e) => {
      if (e.target.id === 'agent-spawn-modal') closeAgentSpawnModal();
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && $('#agent-spawn-modal').classList.contains('show')) {
        closeAgentSpawnModal();
      }
    });
  }

  // ---- Clone agent modal --------------------------------------------------
  //
  // Submit POSTs to /api/agents/{conv}/clone with `{follow_up, no_copy_conv}`.
  // Follow-up is optional; newlines are stripped client-side because the
  // server rejects them (tmux send-keys would split them into multiple
  // submits).

  function openCloneAgentModal(conv, label, cwd) {
    cwd = cwd || '';
    const meta = $('#clone-agent-meta');
    const src = label || shortId(conv);
    meta.textContent = cwd ? `source: ${src}  ·  ${cwd}` : `source: ${src}`;
    $('#clone-agent-followup').value = '';
    $('#clone-agent-copy-conv').checked = true;
    $('#clone-agent-wt-branch').value = '';
    $('#clone-agent-error').textContent = '';
    $('#clone-agent-modal').dataset.conv = conv;
    $('#clone-agent-modal').dataset.label = label || '';
    $('#clone-agent-modal').dataset.cwd = cwd;
    // The picker lists worktrees of the source agent's repo; "+ create"
    // forks a new one and the clone spawns there.
    wtLoad('clone-agent', cwd, '(no worktree — same directory as source)');
    $('#clone-agent-modal').classList.add('show');
    setTimeout(() => $('#clone-agent-followup').focus(), 0);
  }

  function closeCloneAgentModal() {
    $('#clone-agent-modal').classList.remove('show');
  }

  // normaliseFollowUp collapses newlines/tabs/runs-of-whitespace to a
  // single space and trims. Server rejects newlines outright; this
  // keeps the textarea ergonomic while staying safe.
  function normaliseFollowUp(s) {
    return String(s || '').replace(/[\r\n\t]+/g, ' ').replace(/\s+/g, ' ').trim();
  }

  async function submitCloneAgent() {
    const modal = $('#clone-agent-modal');
    const conv = modal.dataset.conv;
    const label = modal.dataset.label || shortId(conv);
    const followUp = normaliseFollowUp($('#clone-agent-followup').value);
    const copyConv = $('#clone-agent-copy-conv').checked;
    const errEl = $('#clone-agent-error');
    errEl.textContent = '';
    const submitBtn = $('#clone-agent-submit');
    submitBtn.disabled = true;
    submitBtn.textContent = 'Cloning…';
    try {
      // Resolve the worktree picker → optional cwd override. An empty
      // result means "inherit the source's cwd" (historical behaviour).
      const cwd = await wtResolveCwd('clone-agent', modal.dataset.cwd || '', '');
      const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/clone`, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ follow_up: followUp, no_copy_conv: !copyConv, cwd }),
      });
      if (!r.ok) {
        errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
        return;
      }
      let payload = {};
      try { payload = await r.json(); } catch (_) {}
      closeCloneAgentModal();
      toast(`cloned ${label}${payload.new_conv ? ' → ' + shortId(payload.new_conv) : ''}`);
      refresh();
    } catch (err) {
      errEl.textContent = (err && err.message) || String(err);
    } finally {
      submitBtn.disabled = false;
      submitBtn.textContent = 'Clone';
    }
  }

  function bindCloneAgentModal() {
    $('#clone-agent-cancel').addEventListener('click', closeCloneAgentModal);
    $('#clone-agent-submit').addEventListener('click', submitCloneAgent);
    bindWtPicker('clone-agent');
    $('#clone-agent-modal').addEventListener('click', (e) => {
      if (e.target.id === 'clone-agent-modal') closeCloneAgentModal();
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && $('#clone-agent-modal').classList.contains('show')) {
        closeCloneAgentModal();
      }
    });
  }

  // ---- Reincarnate agent modal --------------------------------------------
  //
  // Two modes, chosen by the radiogroup; both POST to
  // /api/agents/{conv}/reincarnate:
  //   - "self" (the DEFAULT): POST {mode:'self', focus_hint?} — the
  //     daemon messages the agent to reincarnate itself. focus_hint is
  //     OPTIONAL, so Submit is always enabled.
  //   - "force": POST {mode:'force', follow_up} — the immediate
  //     daemon-driven reincarnation. follow_up is REQUIRED, so Submit
  //     is disabled until the follow-up textarea has content.

  function reincarnateMode() {
    const checked = $('input[name=reincarnate-mode]:checked');
    return (checked && checked.value) || 'self';
  }

  // updateReincarnateMode shows the fields for the selected mode,
  // relabels Submit, and recomputes its disabled state. Self-mode's
  // Submit is always enabled (the focus hint is optional); force-mode's
  // is gated on a non-empty follow-up.
  function updateReincarnateMode() {
    const isForce = reincarnateMode() === 'force';
    $('#reincarnate-self-fields').hidden = isForce;
    $('#reincarnate-force-fields').hidden = !isForce;
    const submitBtn = $('#reincarnate-agent-submit');
    submitBtn.textContent = isForce ? 'Force reincarnate' : 'Ask agent';
    submitBtn.disabled = isForce && !normaliseFollowUp($('#reincarnate-agent-followup').value);
  }

  function openReincarnateAgentModal(conv, label) {
    const meta = $('#reincarnate-agent-meta');
    meta.textContent = label ? `target: ${label}` : `target: ${shortId(conv)}`;
    $('#reincarnate-agent-followup').value = '';
    $('#reincarnate-agent-focus').value = '';
    $('#reincarnate-agent-error').textContent = '';
    // Every open resets to the self-reincarnate default.
    const selfRadio = $('input[name=reincarnate-mode][value=self]');
    if (selfRadio) selfRadio.checked = true;
    updateReincarnateMode();
    $('#reincarnate-agent-modal').dataset.conv = conv;
    $('#reincarnate-agent-modal').dataset.label = label || '';
    $('#reincarnate-agent-modal').classList.add('show');
    setTimeout(() => $('#reincarnate-agent-focus').focus(), 0);
  }

  function closeReincarnateAgentModal() {
    $('#reincarnate-agent-modal').classList.remove('show');
  }

  async function submitReincarnateAgent() {
    const modal = $('#reincarnate-agent-modal');
    const conv = modal.dataset.conv;
    const label = modal.dataset.label || shortId(conv);
    const errEl = $('#reincarnate-agent-error');
    errEl.textContent = '';
    const mode = reincarnateMode();
    let body;
    if (mode === 'force') {
      const followUp = normaliseFollowUp($('#reincarnate-agent-followup').value);
      if (!followUp) {
        errEl.textContent = 'follow-up is required for force reincarnate';
        return;
      }
      body = { mode: 'force', follow_up: followUp };
    } else {
      // Focus hint is optional — send it trimmed, or omit when blank.
      const hint = $('#reincarnate-agent-focus').value.trim();
      body = { mode: 'self' };
      if (hint) body.focus_hint = hint;
    }
    const submitBtn = $('#reincarnate-agent-submit');
    submitBtn.disabled = true;
    submitBtn.textContent = mode === 'force' ? 'Reincarnating…' : 'Asking…';
    try {
      const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/reincarnate`, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!r.ok) {
        errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
        return;
      }
      let payload = {};
      try { payload = await r.json(); } catch (_) {}
      closeReincarnateAgentModal();
      if (mode === 'force') {
        const suffix = payload.new_title ? ' → ' + payload.new_title : (payload.new_conv ? ' → ' + shortId(payload.new_conv) : '');
        toast(`reincarnated ${label}${suffix}`);
      } else {
        toast(`asked ${label} to reincarnate itself`);
      }
      refresh();
    } catch (err) {
      errEl.textContent = (err && err.message) || String(err);
    } finally {
      // Recompute label + disabled state for whatever mode is selected
      // (relevant only on the error path — success closed the modal).
      updateReincarnateMode();
    }
  }

  function bindReincarnateAgentModal() {
    $('#reincarnate-agent-cancel').addEventListener('click', closeReincarnateAgentModal);
    $('#reincarnate-agent-submit').addEventListener('click', submitReincarnateAgent);
    $('#reincarnate-agent-followup').addEventListener('input', updateReincarnateMode);
    $$('input[name=reincarnate-mode]').forEach(rdo => {
      rdo.addEventListener('change', () => {
        updateReincarnateMode();
        const focusEl = reincarnateMode() === 'force'
          ? $('#reincarnate-agent-followup') : $('#reincarnate-agent-focus');
        setTimeout(() => focusEl.focus(), 0);
      });
    });
    $('#reincarnate-agent-modal').addEventListener('click', (e) => {
      if (e.target.id === 'reincarnate-agent-modal') closeReincarnateAgentModal();
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && $('#reincarnate-agent-modal').classList.contains('show')) {
        closeReincarnateAgentModal();
      }
    });
  }

  // ---- Rename-agent modal --------------------------------------------------
  //
  // Opens with `{conv, label, currentTitle}`. Two submit paths:
  //   - manual: type a title → POST /api/agents/{conv}/rename {title}
  //   - auto:   check the box → POST /api/agents/{conv}/rename {auto: true}
  //             daemon injects a [system: ...] nudge asking the agent to
  //             pick its own title via the agent-rename skill.
  // The title field is disabled when auto is checked so the two paths
  // can't be ambiguous.

  function openRenameAgentModal(conv, label, currentTitle) {
    const meta = $('#rename-agent-meta');
    meta.textContent = label ? `target: ${label}` : `target: ${shortId(conv)}`;
    const titleInput = $('#rename-agent-title-input');
    titleInput.value = currentTitle || '';
    titleInput.disabled = false;
    $('#rename-agent-auto').checked = false;
    $('#rename-agent-error').textContent = '';
    $('#rename-agent-submit').textContent = 'Rename';
    $('#rename-agent-modal').dataset.conv = conv;
    $('#rename-agent-modal').dataset.label = label || '';
    $('#rename-agent-modal').classList.add('show');
    setTimeout(() => titleInput.focus(), 0);
  }

  function closeRenameAgentModal() {
    $('#rename-agent-modal').classList.remove('show');
  }

  async function submitRenameAgent() {
    const modal = $('#rename-agent-modal');
    const conv = modal.dataset.conv;
    const label = modal.dataset.label || shortId(conv);
    const auto = $('#rename-agent-auto').checked;
    const title = $('#rename-agent-title-input').value.trim();
    const errEl = $('#rename-agent-error');
    errEl.textContent = '';
    if (!auto && !title) {
      errEl.textContent = 'title is required (or check "auto" to let the agent choose)';
      return;
    }
    const submitBtn = $('#rename-agent-submit');
    const origLabel = submitBtn.textContent;
    submitBtn.disabled = true;
    submitBtn.textContent = auto ? 'Sending nudge…' : 'Renaming…';
    try {
      const body = auto ? { auto: true } : { title };
      const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/rename`, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!r.ok) {
        errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
        return;
      }
      closeRenameAgentModal();
      if (auto) {
        toast(`auto-rename nudge sent: ${label}`);
      } else {
        toast(`renaming ${label} → ${title}`);
      }
      refresh();
    } catch (err) {
      errEl.textContent = (err && err.message) || String(err);
    } finally {
      submitBtn.disabled = false;
      submitBtn.textContent = origLabel;
    }
  }

  function bindRenameAgentModal() {
    $('#rename-agent-cancel').addEventListener('click', closeRenameAgentModal);
    $('#rename-agent-submit').addEventListener('click', submitRenameAgent);
    $('#rename-agent-auto').addEventListener('change', (e) => {
      const auto = e.target.checked;
      $('#rename-agent-title-input').disabled = auto;
      $('#rename-agent-submit').textContent = auto ? 'Send auto-rename nudge' : 'Rename';
    });
    $('#rename-agent-title-input').addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !$('#rename-agent-auto').checked) {
        e.preventDefault();
        submitRenameAgent();
      }
    });
    $('#rename-agent-modal').addEventListener('click', (e) => {
      if (e.target.id === 'rename-agent-modal') closeRenameAgentModal();
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && $('#rename-agent-modal').classList.contains('show')) {
        closeRenameAgentModal();
      }
    });
  }

  // sudoBadge renders the per-row 🔓 indicator when an agent currently
  // holds ≥1 active grant. Tooltip lists the slugs + soonest expiry so
  // hovering tells the human everything they'd want to know without a
  // tab switch.
  function sudoBadge(activeSudo, fallbackConvID) {
    if (!activeSudo || !activeSudo.length) return '';
    const lines = activeSudo.map(g => `${g.slug} (expires in ${fmtRemaining(g.remaining_seconds)})`);
    const title = `${activeSudo.length} active sudo grant${activeSudo.length === 1 ? '' : 's'} — click to manage:\n` + lines.join('\n');
    // sudoByConv entries carry their own conv_id; the caller-supplied
    // fallback (and finally '') just guarantees the badge always has a
    // click target even on an unexpected entry shape.
    const convID = activeSudo[0].conv_id || fallbackConvID || '';
    return `<span class="sudo-badge" data-act="sudo-manage" data-conv="${esc(convID)}" title="${esc(title)}">🔓</span>`;
  }

  function bindFilter(tab) {
    const input = $(`#filter-${tab}`);
    const clear = $(`#filter-${tab}-clear`);
    const key = `tclaude.dash.filter.${tab}`;
    input.value = localStorage.getItem(key) || '';
    const rerender = () => {
      if (tab === 'groups') renderGroupsTab();
      else if (tab === 'templates') renderTemplatesTab();
      else if (tab === 'cron') renderCronTab();
      else if (tab === 'sudo') renderSudoTab();
      else if (tab === 'links') renderLinksTab();
      else if (tab === 'messages') renderMessagesTab();
    };
    const onChange = () => {
      const v = input.value;
      if (v) localStorage.setItem(key, v); else localStorage.removeItem(key);
      rerender();
    };
    input.addEventListener('input', onChange);
    clear.addEventListener('click', () => { input.value = ''; onChange(); input.focus(); });
    // Optional per-tab "show offline" checkbox (the 'groups' tab only).
    // Restore its persisted state — defaults to checked (show all)
    // when the user has never touched it.
    const offline = $(`#filter-${tab}-offline`);
    if (offline) {
      const okey = `tclaude.dash.offline.${tab}`;
      const saved = localStorage.getItem(okey);
      offline.checked = saved === null ? true : saved === '1';
      offline.addEventListener('change', () => {
        localStorage.setItem(okey, offline.checked ? '1' : '0');
        rerender();
      });
    }
    // Optional "show ungrouped" checkbox (groups tab only) — toggles
    // the virtual Ungrouped group. Persisted like the offline toggle;
    // defaults to checked when the user has never touched it.
    const ungrouped = $(`#filter-${tab}-ungrouped`);
    if (ungrouped) {
      const ukey = `tclaude.dash.ungrouped.${tab}`;
      const saved = localStorage.getItem(ukey);
      ungrouped.checked = saved === null ? true : saved === '1';
      ungrouped.addEventListener('change', () => {
        localStorage.setItem(ukey, ungrouped.checked ? '1' : '0');
        rerender();
      });
    }
    // Optional "show conversations" checkbox (groups tab only) —
    // toggles the virtual Conversations group. Defaults OFF (there can
    // be many conversations) when the user has never touched it.
    const conversations = $(`#filter-${tab}-conversations`);
    if (conversations) {
      const ckey = `tclaude.dash.conversations.${tab}`;
      const saved = localStorage.getItem(ckey);
      conversations.checked = saved === '1';
      conversations.addEventListener('change', () => {
        localStorage.setItem(ckey, conversations.checked ? '1' : '0');
        rerender();
      });
    }
    // Optional "show retired" checkbox (groups tab only) — toggles the
    // virtual Retired group. Defaults ON: a retired agent must stay
    // visible somewhere on the tab rather than silently disappearing.
    const retired = $(`#filter-${tab}-retired`);
    if (retired) {
      const rkey = `tclaude.dash.retired.${tab}`;
      const saved = localStorage.getItem(rkey);
      retired.checked = saved === null ? true : saved === '1';
      retired.addEventListener('change', () => {
        localStorage.setItem(rkey, retired.checked ? '1' : '0');
        rerender();
      });
    }
  }

  async function refresh() {
    if (refreshSuspended()) {
      // An inline-edit input, a modal, or a drag is in progress;
      // re-rendering now would blow the input away mid-keystroke,
      // disrupt the modal, or detach the dragged row. Skip this tick —
      // the commit / cancel / dragend handlers each re-trigger
      // refresh() once the user is done.
      return;
    }
    try {
      const r = await fetch('/api/snapshot', { credentials: 'same-origin' });
      if (!r.ok) {
        showStatus('snapshot failed: HTTP ' + r.status, true);
        return;
      }
      const data = await r.json();
      // The guard above was sampled BEFORE the fetch. A drag or a modal
      // may have opened while it was in flight — re-check now, before
      // touching the DOM. Bailing here (ahead of the lastSnapshot
      // assignment) also preserves any optimistic drag mutation already
      // applied to the old snapshot; the drag/modal teardown re-runs
      // refresh() when it finishes.
      if (refreshSuspended()) return;
      lastSnapshot = data;
      $('#meta').textContent = data.popup_base + ' · refreshed ' + new Date(data.generated_at).toLocaleTimeString();
      // Refresh the proactive-grant blocklist hint from the snapshot
      // when present; falls back to the v1 hardcoded pair otherwise.
      // (Snapshot doesn't carry the resolved blocklist directly; the
      // server returns 403 on submit if the picker missed one — the
      // UI just dims the well-known pair so the common case is
      // self-explanatory.)
      sudoGrantBlocklist = ['permissions.grant', 'permissions.revoke'];
      sudoByConv = {};
      (data.sudo || []).forEach(g => {
        if (!sudoByConv[g.conv_id]) sudoByConv[g.conv_id] = [];
        sudoByConv[g.conv_id].push(g);
      });
      renderGroupsTab();
      renderTemplatesTab();
      renderCronTab();
      renderSudoTab();
      renderLinksTab();
      $('#tab-permissions').innerHTML = renderPermissions(data.permissions, data.agents);
      $('#tab-slugs').innerHTML = renderSlugs(data.slugs);
      renderMessagesTab();
      renderMessagesBadge(data.messages_unread || 0);
      renderUsage(data.usage);
      showStatus('● live', false);
    } catch (e) {
      showStatus('snapshot failed: ' + (e.message || e), true);
    }
  }

  function bindTabs() {
    $$('nav button').forEach(b => {
      b.addEventListener('click', () => {
        $$('nav button').forEach(x => x.classList.toggle('active', x === b));
        $$('main section').forEach(s => {
          s.classList.toggle('active', s.id === 'tab-' + b.dataset.tab);
        });
      });
    });
  }

  function bindCopy() {
    document.addEventListener('click', e => {
      const t = e.target.closest('[data-copy]');
      if (!t) return;
      const cmd = t.getAttribute('data-copy');
      navigator.clipboard?.writeText(cmd).then(() => {
        const orig = t.textContent;
        t.textContent = '✓ copied: ' + cmd;
        setTimeout(() => { t.textContent = orig; }, 1200);
      }).catch(() => {});
    });
  }

  // <details> only fires `toggle` on the element itself (not bubbling),
  // so use a capturing listener at the document level rather than
  // re-binding per-element after every render.
  function bindDetailsPersistence() {
    document.addEventListener('toggle', e => {
      const d = e.target;
      if (!(d instanceof HTMLDetailsElement)) return;
      const key = d.getAttribute('data-group-key');
      if (!key) return;
      if (d.open) {
        localStorage.setItem('tclaude.dash.group.' + key, '1');
      } else {
        localStorage.removeItem('tclaude.dash.group.' + key);
      }
    }, true);
  }

  // bindSortHeaders delegates clicks on sortable <th> cells. Headers
  // are re-rendered on every 5s refresh, so a single document-level
  // listener is simpler than re-binding per render (same approach as
  // bindCopy / bindDetailsPersistence). Clicking re-renders just the
  // affected tab so the new ordering — and the header arrow — show
  // immediately, without waiting for the next poll.
  function bindSortHeaders() {
    document.addEventListener('click', e => {
      const th = e.target.closest('th[data-sort-table]');
      if (!th) return;
      const tableKey = th.dataset.sortTable;
      cycleSort(tableKey, th.dataset.sortCol);
      if (tableKey === 'members') renderGroupsTab();
      else if (tableKey === 'cron') renderCronTab();
      else if (tableKey === 'sudo') renderSudoTab();
      else if (tableKey === 'links') renderLinksTab();
    });
  }

  // --- inline mutations: action buttons + confirm modal + toast ---

  // confirmModal pops the confirmation overlay; resolves true on
  // OK, false on Cancel / outside-click / Escape.
  function confirmModal({title, body, meta, okLabel}) {
    return new Promise(resolve => {
      const overlay = $('#confirm-modal');
      $('#confirm-title').textContent = title;
      $('#confirm-body').textContent = body;
      $('#confirm-meta').textContent = meta || '';
      $('#confirm-meta').style.display = meta ? 'block' : 'none';
      const okBtn = $('#confirm-ok');
      okBtn.textContent = okLabel || 'Confirm';
      const cancelBtn = $('#confirm-cancel');
      const cleanup = (result) => {
        overlay.classList.remove('show');
        okBtn.removeEventListener('click', onOk);
        cancelBtn.removeEventListener('click', onCancel);
        overlay.removeEventListener('click', onOverlay);
        document.removeEventListener('keydown', onKey);
        resolve(result);
      };
      const onOk = () => cleanup(true);
      const onCancel = () => cleanup(false);
      const onOverlay = (e) => { if (e.target === overlay) cleanup(false); };
      const onKey = (e) => { if (e.key === 'Escape') cleanup(false); };
      okBtn.addEventListener('click', onOk);
      cancelBtn.addEventListener('click', onCancel);
      overlay.addEventListener('click', onOverlay);
      document.addEventListener('keydown', onKey);
      overlay.classList.add('show');
      okBtn.focus();
    });
  }

  // emergencyShutdown drives the group-level and whole-dashboard
  // emergency-shutdown buttons. It counts the running agents in scope
  // from the last snapshot, pops a confirm modal that states the
  // count and spells out that this is stop-only (no data deleted),
  // POSTs /api/emergency-shutdown, then toasts the outcome summary.
  // scope is "group" (groupName set) or "all" (groupName ignored).
  async function emergencyShutdown(scope, groupName) {
    const snap = lastSnapshot || {};
    let running = 0;
    let where = '';
    let metaLine = '';
    if (scope === 'group') {
      const g = (snap.groups || []).find(x => x.name === groupName);
      running = g ? (g.online || 0) : 0;
      where = `group "${groupName}"`;
      metaLine = groupName;
    } else {
      running = (snap.agents || []).filter(a => a.online).length;
      where = 'the whole dashboard';
      metaLine = 'every group + ungrouped agents';
    }
    if (running === 0) {
      toast(`emergency shutdown: no running agents in ${where}`);
      return;
    }
    const n = running === 1 ? '1 running agent' : `${running} running agents`;
    const confirmed = await confirmModal({
      title: 'Emergency shutdown?',
      body: `This stops ${n} in ${where}. Each agent is sent /exit, then `
        + `force-killed only if it has not exited within the grace period. `
        + `Stop only — no conversations, group memberships, enrollment or `
        + `permissions are deleted. Resume any session to bring that agent back.`,
      meta: metaLine,
      okLabel: `Shut down ${running === 1 ? '1 agent' : running + ' agents'}`,
    });
    if (!confirmed) return;
    const payload = scope === 'group' ? {scope: 'group', group: groupName} : {scope: 'all'};
    let r;
    try {
      r = await fetch('/api/emergency-shutdown', {
        method: 'POST', credentials: 'same-origin',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(payload),
      });
    } catch (e) {
      toast(`emergency shutdown failed: ${e && e.message || e}`, true);
      return;
    }
    if (!r.ok) {
      toast(`emergency shutdown failed: ${await r.text()}`, true);
      return;
    }
    const out = await r.json().catch(() => null);
    if (!out) {
      toast('emergency shutdown: done');
      refresh();
      return;
    }
    const parts = [`${out.exited_gracefully} exited gracefully`, `${out.force_killed} force-killed`];
    if (out.already_offline) parts.push(`${out.already_offline} already offline`);
    if (out.failed) parts.push(`${out.failed} failed`);
    toast(`emergency shutdown (${out.targeted} targeted): ${parts.join(', ')}`, out.failed > 0);
    refresh();
  }

  // openWindowModal drives the bulk window focus/unfocus feature. One
  // trigger per scope — a group-level button and the top-bar button —
  // opens this modal. Inside it the human picks the DIRECTION (focus
  // vs unfocus) and the agent SELECTION: every running agent in scope
  // is listed and ticked by default, and can be narrowed by role chip,
  // by individual checkbox, or by the text filter. Submit POSTs the
  // explicit conv-id list to /api/agent-windows.
  //
  // It is window-only: focus opens/raises terminal windows, unfocus
  // detaches them. Neither touches an agent process — the agents keep
  // running. scope is "group" (groupName set) or "all".
  function openWindowModal(scope, groupName) {
    const snap = lastSnapshot || {};
    const where = scope === 'group' ? `group "${groupName}"` : 'the dashboard';
    const NO_ROLE = '(no role)';

    // An agent's roles come from its group memberships — a top-level
    // agent row carries no role of its own, so the all-scope modal
    // collects them across every group.
    const rolesByConv = {};
    for (const g of (snap.groups || [])) {
      for (const m of (g.members || [])) {
        if (!m.role) continue;
        const rs = rolesByConv[m.conv_id] || (rolesByConv[m.conv_id] = []);
        if (!rs.includes(m.role)) rs.push(m.role);
      }
    }
    // Candidates — RUNNING agents only: an offline agent has no window
    // to focus or detach. Each carries its own `checked` flag so the
    // text filter can re-render the list without losing the selection.
    const candidates = [];
    if (scope === 'group') {
      const g = (snap.groups || []).find(x => x.name === groupName);
      for (const m of (g && g.members || [])) {
        if (!m.online) continue;
        candidates.push({ conv_id: m.conv_id, title: m.title || '',
          roles: m.role ? [m.role] : [], checked: true });
      }
    } else {
      for (const a of (snap.agents || [])) {
        if (!a.online) continue;
        candidates.push({ conv_id: a.conv_id, title: a.title || '',
          roles: rolesByConv[a.conv_id] || [], checked: true });
      }
    }
    if (candidates.length === 0) {
      toast(`agent windows: no running agents in ${where}`);
      return;
    }
    // roleKeys(c) — the role buckets a candidate belongs to (for the
    // chips). An agent with no role lands in the synthetic NO_ROLE
    // bucket so it stays reachable by a chip.
    const roleKeys = (c) => c.roles.length ? c.roles : [NO_ROLE];
    const allRoleKeys = [];
    for (const c of candidates) {
      for (const k of roleKeys(c)) {
        if (!allRoleKeys.includes(k)) allRoleKeys.push(k);
      }
    }
    allRoleKeys.sort((a, b) => (a === NO_ROLE) - (b === NO_ROLE) || a.localeCompare(b));

    const overlay = $('#window-modal');
    const hintEl = $('#window-hint');
    const rolesEl = $('#window-roles');
    const listEl = $('#window-list');
    const countEl = $('#window-count');
    const errEl = $('#window-error');
    const searchEl = $('#window-search');
    const submitBtn = $('#window-submit');
    const cancelBtn = $('#window-cancel');
    const selAllBtn = $('#window-select-all');
    const selNoneBtn = $('#window-select-none');
    const dirRadios = overlay.querySelectorAll('input[name=window-direction]');

    // Reset transient state on every open.
    errEl.textContent = '';
    searchEl.value = '';
    for (const r of dirRadios) r.checked = (r.value === 'focus');
    for (const c of candidates) c.checked = true;

    const direction = () => {
      for (const r of dirRadios) if (r.checked) return r.value;
      return 'focus';
    };
    const checkedCount = () => candidates.filter(c => c.checked).length;
    const matchesFilter = (c) => {
      const q = searchEl.value.trim().toLowerCase();
      if (!q) return true;
      return c.title.toLowerCase().includes(q) || c.conv_id.toLowerCase().includes(q);
    };

    function renderHint() {
      hintEl.textContent = direction() === 'focus'
        ? `Open or raise a terminal window for each selected running agent in ${where}.`
        : `Detach the terminal windows of the selected running agents in ${where} so the `
          + `desktop is decluttered. The agents keep running — only the windows are dismissed.`;
    }
    function renderRoles() {
      // Chips only earn their space when there is more than one bucket.
      if (allRoleKeys.length < 2) { rolesEl.innerHTML = ''; return; }
      let html = '<span class="roles-label">roles</span>';
      for (const k of allRoleKeys) {
        const inK = candidates.filter(c => roleKeys(c).includes(k));
        const on = inK.filter(c => c.checked).length;
        const cls = on === 0 ? '' : (on === inK.length ? ' on' : ' partial');
        html += `<button type="button" class="window-role-chip${cls}" data-role="${esc(k)}">`
          + `${esc(k)} (${on}/${inK.length})</button>`;
      }
      rolesEl.innerHTML = html;
    }
    function renderList() {
      const rows = candidates.filter(matchesFilter);
      if (rows.length === 0) {
        listEl.innerHTML = '<div class="cleanup-empty">no agents match the filter</div>';
        return;
      }
      listEl.innerHTML = rows.map(c => {
        const badges = c.roles.map(r => `<span class="cleanup-badge">${esc(r)}</span>`).join('');
        return `<div class="cleanup-row"><label>`
          + `<input type="checkbox" data-conv="${esc(c.conv_id)}"${c.checked ? ' checked' : ''} />`
          + `<span class="title">${esc(c.title || '(untitled)')}</span>`
          + `<span class="id">${esc(c.conv_id.slice(0, 8))}</span>`
          + `${badges}</label></div>`;
      }).join('');
    }
    function renderFooter() {
      const n = checkedCount();
      countEl.textContent = `${n} of ${candidates.length} selected`;
      const verb = direction() === 'focus' ? 'Focus' : 'Unfocus';
      submitBtn.textContent = n === 1 ? `${verb} 1 agent` : `${verb} ${n} agents`;
      submitBtn.disabled = n === 0;
    }
    function render() { renderHint(); renderRoles(); renderList(); renderFooter(); }

    const findCandidate = (conv) => candidates.find(c => c.conv_id === conv);

    const onListChange = (e) => {
      const cb = e.target.closest('input[type=checkbox]');
      if (!cb) return;
      const c = findCandidate(cb.getAttribute('data-conv'));
      if (c) c.checked = cb.checked;
      renderRoles(); renderFooter();
    };
    const onRolesClick = (e) => {
      const chip = e.target.closest('.window-role-chip');
      if (!chip) return;
      const k = chip.getAttribute('data-role');
      const inK = candidates.filter(c => roleKeys(c).includes(k));
      // Toggle: if every agent in this role is already selected, clear
      // them; otherwise select them all.
      const allOn = inK.every(c => c.checked);
      for (const c of inK) c.checked = !allOn;
      render();
    };
    const onDirChange = () => { renderHint(); renderFooter(); };
    const onSearch = () => renderList();
    const onSelectAll = () => { for (const c of candidates) c.checked = true; render(); };
    const onSelectNone = () => { for (const c of candidates) c.checked = false; render(); };

    const cleanup = () => {
      overlay.classList.remove('show');
      listEl.removeEventListener('change', onListChange);
      rolesEl.removeEventListener('click', onRolesClick);
      for (const r of dirRadios) r.removeEventListener('change', onDirChange);
      searchEl.removeEventListener('input', onSearch);
      selAllBtn.removeEventListener('click', onSelectAll);
      selNoneBtn.removeEventListener('click', onSelectNone);
      submitBtn.removeEventListener('click', onSubmit);
      cancelBtn.removeEventListener('click', cleanup);
      overlay.removeEventListener('click', onOverlay);
      document.removeEventListener('keydown', onKey);
    };
    const onOverlay = (e) => { if (e.target === overlay) cleanup(); };
    const onKey = (e) => { if (e.key === 'Escape') cleanup(); };

    async function onSubmit() {
      const convs = candidates.filter(c => c.checked).map(c => c.conv_id);
      if (convs.length === 0) return;
      const dir = direction();
      const payload = { direction: dir, scope, convs };
      if (scope === 'group') payload.group = groupName;
      submitBtn.disabled = true;
      errEl.textContent = '';
      let r;
      try {
        r = await fetch('/api/agent-windows', {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload),
        });
      } catch (e) {
        errEl.textContent = `request failed: ${e && e.message || e}`;
        renderFooter();
        return;
      }
      if (!r.ok) {
        errEl.textContent = await r.text();
        renderFooter();
        return;
      }
      const out = await r.json().catch(() => null);
      cleanup();
      if (!out) { toast('agent windows: done'); return; }
      if (dir === 'focus') {
        const extra = out.failed ? `, ${out.failed} failed` : '';
        toast(`focus windows (${out.targeted} targeted): ${out.focused} focused${extra}`, out.failed > 0);
      } else {
        const parts = [`${out.detached} detached`];
        if (out.no_window) parts.push(`${out.no_window} had no window`);
        if (out.failed) parts.push(`${out.failed} failed`);
        toast(`unfocus windows (${out.targeted} targeted): ${parts.join(', ')}`, out.failed > 0);
      }
    }

    listEl.addEventListener('change', onListChange);
    rolesEl.addEventListener('click', onRolesClick);
    for (const r of dirRadios) r.addEventListener('change', onDirChange);
    searchEl.addEventListener('input', onSearch);
    selAllBtn.addEventListener('click', onSelectAll);
    selNoneBtn.addEventListener('click', onSelectNone);
    submitBtn.addEventListener('click', onSubmit);
    cancelBtn.addEventListener('click', cleanup);
    overlay.addEventListener('click', onOverlay);
    document.addEventListener('keydown', onKey);

    render();
    overlay.classList.add('show');
    setTimeout(() => submitBtn.focus(), 0);
  }

  // retireConfirm pops the retire confirmation: the same explanatory
  // copy as the old confirmModal-based prompt, plus an "also shut down
  // the running session" checkbox (checked by default). Resolves to
  // {shutdown: bool} on Retire, null on Cancel / outside-click /
  // Escape. Shared by the per-row retire button and the drag-onto-
  // Retired gesture so both ask the same question.
  function retireConfirm({label}) {
    return new Promise(resolve => {
      const overlay = $('#retire-modal');
      const okBtn = $('#retire-ok');
      const cancelBtn = $('#retire-cancel');
      const shutdownCb = $('#retire-shutdown');
      $('#retire-meta').textContent = label || '';
      $('#retire-meta').style.display = label ? 'block' : 'none';
      shutdownCb.checked = true; // default ON on every open
      const cleanup = (result) => {
        overlay.classList.remove('show');
        okBtn.removeEventListener('click', onOk);
        cancelBtn.removeEventListener('click', onCancel);
        overlay.removeEventListener('click', onOverlay);
        document.removeEventListener('keydown', onKey);
        resolve(result);
      };
      const onOk = () => cleanup({ shutdown: shutdownCb.checked });
      const onCancel = () => cleanup(null);
      const onOverlay = (e) => { if (e.target === overlay) cleanup(null); };
      const onKey = (e) => { if (e.key === 'Escape') cleanup(null); };
      okBtn.addEventListener('click', onOk);
      cancelBtn.addEventListener('click', onCancel);
      overlay.addEventListener('click', onOverlay);
      document.addEventListener('keydown', onKey);
      overlay.classList.add('show');
      okBtn.focus();
    });
  }

  // shutdownConfirm pops a 3-button confirm: Soft exit (default),
  // Force kill (destructive), Cancel. Resolves to "soft" / "force" /
  // null. Mirrors the existing confirmModal but with two distinct
  // confirm paths so the human can pick blast radius.
  function shutdownConfirm({label}) {
    return new Promise(resolve => {
      const overlay = $('#shutdown-modal');
      $('#shutdown-meta').textContent = label || '';
      $('#shutdown-meta').style.display = label ? 'block' : 'none';
      const softBtn = $('#shutdown-soft');
      const forceBtn = $('#shutdown-force');
      const cancelBtn = $('#shutdown-cancel');
      const cleanup = (result) => {
        overlay.classList.remove('show');
        softBtn.removeEventListener('click', onSoft);
        forceBtn.removeEventListener('click', onForce);
        cancelBtn.removeEventListener('click', onCancel);
        overlay.removeEventListener('click', onOverlay);
        document.removeEventListener('keydown', onKey);
        resolve(result);
      };
      const onSoft = () => cleanup('soft');
      const onForce = () => cleanup('force');
      const onCancel = () => cleanup(null);
      const onOverlay = (e) => { if (e.target === overlay) cleanup(null); };
      const onKey = (e) => { if (e.key === 'Escape') cleanup(null); };
      softBtn.addEventListener('click', onSoft);
      forceBtn.addEventListener('click', onForce);
      cancelBtn.addEventListener('click', onCancel);
      overlay.addEventListener('click', onOverlay);
      document.addEventListener('keydown', onKey);
      overlay.classList.add('show');
      softBtn.focus();
    });
  }

  // termDirModal pops a 4-button picker: Current dir (default),
  // Worktree dir, Launch dir, Cancel. Resolves to
  // "current" / "worktree" / "start" / null. The caller POSTs the
  // choice to /api/term/{conv}; the daemon opens the terminal window
  // out-of-sandbox via terminal.OpenWithCommand.
  function termDirModal({label}) {
    return new Promise(resolve => {
      const overlay = $('#term-modal');
      $('#term-meta').textContent = label || '';
      $('#term-meta').style.display = label ? 'block' : 'none';
      const currentBtn = $('#term-current');
      const worktreeBtn = $('#term-worktree');
      const startBtn = $('#term-start');
      const cancelBtn = $('#term-cancel');
      const cleanup = (result) => {
        overlay.classList.remove('show');
        currentBtn.removeEventListener('click', onCurrent);
        worktreeBtn.removeEventListener('click', onWorktree);
        startBtn.removeEventListener('click', onStart);
        cancelBtn.removeEventListener('click', onCancel);
        overlay.removeEventListener('click', onOverlay);
        document.removeEventListener('keydown', onKey);
        resolve(result);
      };
      const onCurrent = () => cleanup('current');
      const onWorktree = () => cleanup('worktree');
      const onStart = () => cleanup('start');
      const onCancel = () => cleanup(null);
      const onOverlay = (e) => { if (e.target === overlay) cleanup(null); };
      const onKey = (e) => { if (e.key === 'Escape') cleanup(null); };
      currentBtn.addEventListener('click', onCurrent);
      worktreeBtn.addEventListener('click', onWorktree);
      startBtn.addEventListener('click', onStart);
      cancelBtn.addEventListener('click', onCancel);
      overlay.addEventListener('click', onOverlay);
      document.addEventListener('keydown', onKey);
      overlay.classList.add('show');
      currentBtn.focus();
    });
  }

  // editMemberModal pops the role/descr editor pre-filled with
  // current values. Resolves to the new {role, descr} object on
  // Save (only fields that actually changed are kept; unchanged fields
  // are sent as null so the daemon leaves them alone), or null on
  // Cancel / outside-click / Escape. Auto-refresh suspends while the
  // modal is open — refreshSuspended() sees its .modal-overlay.show.
  function editMemberModal({label, role, descr}) {
    return new Promise(resolve => {
      const overlay = $('#edit-member-modal');
      $('#edit-member-meta').textContent = label || '';
      $('#edit-member-meta').style.display = label ? 'block' : 'none';
      const roleEl = $('#edit-member-role');
      const descrEl = $('#edit-member-descr');
      roleEl.value = role || '';
      descrEl.value = descr || '';
      const saveBtn = $('#edit-member-save');
      const cancelBtn = $('#edit-member-cancel');
      const cleanup = (result) => {
        overlay.classList.remove('show');
        saveBtn.removeEventListener('click', onSave);
        cancelBtn.removeEventListener('click', onCancel);
        overlay.removeEventListener('click', onOverlay);
        document.removeEventListener('keydown', onKey);
        resolve(result);
      };
      const onSave = () => {
        // Only send fields that changed; unchanged fields go as null
        // so the daemon's PATCH leaves them untouched. Each field
        // either differs from the original (send the new value, even
        // if empty) or is unchanged (send null).
        const out = {};
        const newRole = roleEl.value;
        const newDescr = descrEl.value;
        if (newRole !== (role || '')) out.role = newRole;
        if (newDescr !== (descr || '')) out.descr = newDescr;
        cleanup(Object.keys(out).length === 0 ? 'noop' : out);
      };
      const onCancel = () => cleanup(null);
      const onOverlay = (e) => { if (e.target === overlay) cleanup(null); };
      const onKey = (e) => {
        if (e.key === 'Escape') { e.preventDefault(); cleanup(null); }
        // Ctrl/Cmd+Enter saves from anywhere in the modal so power
        // users don't have to mouse over to the Save button.
        if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
          e.preventDefault(); onSave();
        }
      };
      saveBtn.addEventListener('click', onSave);
      cancelBtn.addEventListener('click', onCancel);
      overlay.addEventListener('click', onOverlay);
      document.addEventListener('keydown', onKey);
      overlay.classList.add('show');
      roleEl.focus();
      roleEl.select();
    });
  }

  // addMemberModal opens an overlay anchored conceptually to a group's
  // header, with a live-filtered candidate list. Returns when the user
  // closes (Esc / click-outside / X). The overlay STAYS OPEN after a
  // successful add — close-on-add is the pain we're fixing here.
  // Uses /api/snapshot directly (no second endpoint) since both the
  // ungrouped[] and agents[] arrays already ship.
  function addMemberModal(groupName) {
    return new Promise(resolve => {
      const overlay = $('#add-member-modal');
      const groupLabel = $('#add-member-group');
      const search = $('#add-member-search');
      const list = $('#add-member-list');
      const includeAll = $('#add-member-all');
      groupLabel.textContent = groupName;
      search.value = '';
      includeAll.checked = false;

      // Highlighted row index (in the currently-rendered candidate
      // list). Reset when the candidate set changes; clamped on render.
      let highlight = 0;
      let candidates = [];

      // Members already in this group — exclude from candidates so the
      // list shows ONLY rows the user can actually add. Looked up from
      // lastSnapshot once at open time + refreshed on each render so
      // a successful add immediately removes the row without waiting
      // for the 5s poll.
      function existingMembers() {
        const g = (lastSnapshot?.groups || []).find(gr => gr.name === groupName);
        return new Set((g?.members || []).map(m => m.conv_id));
      }

      // Build the candidate list from the snapshot. Default pool is
      // (agents ∪ ungrouped) — the agents list covers anyone in any
      // group, and ungrouped covers fresh-spawned online convs that
      // aren't in any group yet. With "Include offline / archived"
      // ticked, the snapshot's whole `agents` set is unioned in even
      // when its rows are offline.
      function buildCandidates() {
        const seen = new Set();
        const out = [];
        const exclude = existingMembers();
        const push = (a) => {
          if (!a || !a.conv_id) return;
          if (seen.has(a.conv_id) || exclude.has(a.conv_id)) return;
          if (!includeAll.checked && !a.online) {
            // Default pool: only currently-online convs (matches the
            // ungrouped + active-pool intuition). The "include all"
            // checkbox lifts this gate.
            // Ungrouped[] is online-only by daemon construction, but
            // agents[] can carry offline rows for previously-grouped
            // convs.
            return;
          }
          seen.add(a.conv_id);
          out.push(a);
        };
        for (const a of lastSnapshot?.ungrouped || []) push(a);
        for (const a of lastSnapshot?.agents   || []) push(a);
        // Non-agent conversations too: adding one to a group promotes
        // it to an agent (the daemon enrolls it on the membership
        // write). Tagged with _promote so the row flags the
        // side-effect. Same online-gating as everything else.
        for (const a of lastSnapshot?.conversations || []) push({ ...a, _promote: true });
        // Sort: online first, then by title.
        out.sort((a, b) => {
          if (!!b.online !== !!a.online) return (b.online ? 1 : 0) - (a.online ? 1 : 0);
          return (a.title || '').localeCompare(b.title || '');
        });
        return out;
      }

      // Pull role / descr off the per-group member row in any
      // group the agent already belongs to. Lets the search match on
      // human-meaningful fields the snapshot's `agents[]` view doesn't
      // surface (it dedupes across groups). A conv that's a member of
      // two groups uses the first-seen row.
      function memberMetaForConv(convID) {
        for (const g of lastSnapshot?.groups || []) {
          for (const m of g.members || []) {
            if (m.conv_id === convID) {
              return {role: m.role || '', descr: m.descr || ''};
            }
          }
        }
        return {role: '', descr: ''};
      }

      function applyFilter(list, q) {
        if (!q) return list;
        const needle = q.toLowerCase();
        return list.filter(a => {
          const meta = memberMetaForConv(a.conv_id);
          return ((a.title || '').toLowerCase().includes(needle)) ||
                 ((a.conv_id || '').toLowerCase().includes(needle)) ||
                 ((meta.role  || '').toLowerCase().includes(needle)) ||
                 ((meta.descr || '').toLowerCase().includes(needle)) ||
                 (a.groups || []).some(g => g.toLowerCase().includes(needle));
        });
      }

      function render() {
        candidates = applyFilter(buildCandidates(), search.value);
        if (highlight >= candidates.length) highlight = Math.max(0, candidates.length - 1);
        if (highlight < 0) highlight = 0;
        if (!candidates.length) {
          list.innerHTML = '<div class="add-member-empty">No matching conversations. ' +
            (includeAll.checked
              ? '(Try a different filter.)'
              : '(Try ticking "Include offline / archived" for a wider pool.)') +
            '</div>';
          return;
        }
        list.innerHTML = candidates.map((a, i) => {
          const meta = memberMetaForConv(a.conv_id);
          const display = a.title || '(unnamed)';
          const dot = a.online
            ? '<span class="online" title="online">●</span>'
            : '<span class="offline" title="offline">○</span>';
          const role = meta.role ? `<span class="role">${esc(meta.role)}</span>` : '';
          const descr = meta.descr ? `<span class="descr">${esc(meta.descr)}</span>` : '';
          const groups = (a.groups || []).length
            ? `<span class="groups-tag">in: ${esc((a.groups || []).join(', '))}</span>`
            : '';
          const promote = a._promote
            ? '<span class="groups-tag promote-tag" title="Not an agent yet — adding it here promotes it">promotes to agent</span>'
            : '';
          return `<div class="add-member-row${i === highlight ? ' highlighted' : ''}" data-i="${i}">` +
                 `${dot}<span class="rowname">${esc(display)}</span>` +
                 `<span class="id">${esc(shortId(a.conv_id))}</span>` +
                 `${role}${descr}${groups}${promote}` +
                 `</div>`;
        }).join('');
        // Scroll the highlighted row into view.
        const hl = list.querySelector('.add-member-row.highlighted');
        if (hl) hl.scrollIntoView({block: 'nearest'});
      }

      async function addOne(idx) {
        const cand = candidates[idx];
        if (!cand) return;
        const r = await fetch(`/api/groups/${encodeURIComponent(groupName)}/members`, {
          method: 'POST', credentials: 'same-origin',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({conv: cand.conv_id}),
        });
        if (!r.ok) {
          toast(`add failed: ${await r.text()}`, true);
          return;
        }
        const label = cand.title || cand.conv_id;
        toast(`added ${label} to ${groupName}`);
        // Optimistic local mutation: append to lastSnapshot's group so
        // the next render filters this row out without waiting for the
        // 5s poll. The poll will overwrite with the canonical state.
        const grp = (lastSnapshot?.groups || []).find(g => g.name === groupName);
        if (grp) {
          grp.members = grp.members || [];
          grp.members.push({conv_id: cand.conv_id, title: cand.title, online: cand.online});
        }
        // Re-render the dashboard groups tab so the just-added row
        // appears under the group header without a poll round-trip.
        renderGroupsTab();
        render();
      }

      const cleanup = () => {
        overlay.classList.remove('show');
        search.removeEventListener('input', onInput);
        includeAll.removeEventListener('change', onInput);
        list.removeEventListener('click', onListClick);
        list.removeEventListener('mousemove', onListMouseMove);
        document.removeEventListener('keydown', onKey, true);
        overlay.removeEventListener('click', onOverlay);
        resolve();
      };
      const onInput = () => { highlight = 0; render(); };
      const onListClick = (e) => {
        const row = e.target.closest('.add-member-row');
        if (!row) return;
        const i = parseInt(row.getAttribute('data-i'), 10);
        if (Number.isFinite(i)) addOne(i);
      };
      const onListMouseMove = (e) => {
        const row = e.target.closest('.add-member-row');
        if (!row) return;
        const i = parseInt(row.getAttribute('data-i'), 10);
        if (Number.isFinite(i) && i !== highlight) {
          highlight = i;
          render();
        }
      };
      const onKey = (e) => {
        if (e.key === 'Escape') { e.preventDefault(); cleanup(); return; }
        if (e.key === 'ArrowDown') {
          e.preventDefault();
          if (candidates.length) { highlight = (highlight + 1) % candidates.length; render(); }
          return;
        }
        if (e.key === 'ArrowUp') {
          e.preventDefault();
          if (candidates.length) { highlight = (highlight - 1 + candidates.length) % candidates.length; render(); }
          return;
        }
        if (e.key === 'Enter') {
          e.preventDefault();
          if (candidates.length) addOne(highlight);
          return;
        }
      };
      const onOverlay = (e) => { if (e.target === overlay) cleanup(); };

      search.addEventListener('input', onInput);
      includeAll.addEventListener('change', onInput);
      list.addEventListener('click', onListClick);
      list.addEventListener('mousemove', onListMouseMove);
      document.addEventListener('keydown', onKey, true);
      overlay.addEventListener('click', onOverlay);
      overlay.classList.add('show');
      render();
      search.focus();
    });
  }

  // toast shows a transient message in the bottom-right. error=true
  // makes the left border red. Auto-dismisses after 3s.
  let toastTimer = null;
  function toast(message, error) {
    const el = $('#toast');
    el.textContent = message;
    el.classList.toggle('error', !!error);
    el.classList.add('show');
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.classList.remove('show'), 3000);
  }

  // deleteAgentModal is the per-row "delete forever" confirm. Beyond
  // confirm/cancel it offers an opt-in to also remove the agent's git
  // worktree. The worktree's status is fetched async from
  // /api/agents/{conv}/worktree: a removable worktree gets a checked,
  // enabled checkbox; a main-repo or shared worktree gets a disabled,
  // greyed one explaining why it's kept; an agent with no worktree
  // shows no row at all. Resolves to null (cancelled) or
  // {deleteWorktree: bool}.
  function deleteAgentModal(conv, label) {
    return new Promise(resolve => {
      const overlay = $('#delete-agent-modal');
      const wtRow = $('#delete-agent-wt-row');
      const wtCb = $('#delete-agent-wt');
      const wtLabel = $('#delete-agent-wt-label');
      const okBtn = $('#delete-agent-ok');
      const cancelBtn = $('#delete-agent-cancel');
      $('#delete-agent-body').textContent =
        'Wipes the conversation history (.jsonl) from disk and drops every group / '
        + 'membership / ownership / permission row for this agent. This cannot be undone.';
      $('#delete-agent-meta').textContent = label || conv;
      // Worktree row hidden until the fetch tells us there is one.
      wtRow.style.display = 'none';
      wtRow.classList.remove('disabled');
      wtCb.checked = false;
      wtCb.disabled = false;

      const cleanup = (result) => {
        overlay.classList.remove('show');
        okBtn.removeEventListener('click', onOk);
        cancelBtn.removeEventListener('click', onCancel);
        overlay.removeEventListener('click', onOverlay);
        document.removeEventListener('keydown', onKey);
        resolve(result);
      };
      const onOk = () => cleanup({ deleteWorktree: wtCb.checked && !wtCb.disabled });
      const onCancel = () => cleanup(null);
      const onOverlay = (e) => { if (e.target === overlay) cleanup(null); };
      const onKey = (e) => { if (e.key === 'Escape') cleanup(null); };
      okBtn.addEventListener('click', onOk);
      cancelBtn.addEventListener('click', onCancel);
      overlay.addEventListener('click', onOverlay);
      document.addEventListener('keydown', onKey);
      overlay.classList.add('show');
      okBtn.focus();

      // Resolve the worktree in the background — the modal is already
      // usable (delete works) before this lands. If the human clicks
      // through before it resolves the worktree is simply kept, the
      // safe default.
      fetch(`/api/agents/${encodeURIComponent(conv)}/worktree`, { credentials: 'same-origin' })
        .then(r => r.ok ? r.json() : null)
        .then(wt => {
          if (!wt || wt.kind === 'none' || !wt.path) return;
          wtRow.style.display = '';
          const pathTxt = wt.path + (wt.branch ? ' · ' + wt.branch : '');
          if (wt.removable) {
            wtCb.checked = true;
            wtCb.disabled = false;
            wtRow.classList.remove('disabled');
            wtLabel.innerHTML = 'Also delete the git worktree '
              + `<span class="wt-note">${esc(pathTxt)} — directory removed, branch kept</span>`;
          } else {
            wtCb.checked = false;
            wtCb.disabled = true;
            wtRow.classList.add('disabled');
            const why = wt.kind === 'main' ? 'the repo’s main worktree, never removed'
              : wt.shared ? 'shared with another agent'
              : 'not removable';
            wtLabel.innerHTML = 'Git worktree kept '
              + `<span class="wt-note">${esc(pathTxt)} — ${esc(why)}</span>`;
          }
        })
        .catch(() => {});
    });
  }

  // ---- 🧹 Cleanup modal ---------------------------------------------
  //
  // CLEANUP_CATS — the three conversation categories the 'agents'-mode
  // cleanup modal spans, in display order. Each maps to a disjoint
  // snapshot list (agents / retired / conversations).
  const CLEANUP_CATS = ['agent', 'retired', 'conversation'];
  const CLEANUP_CAT_LABEL = {
    agent: 'Active agents', retired: 'Retired agents', conversation: 'Conversations',
  };

  // openCleanupModal drives the bulk-cleanup overlay. opts.mode:
  //   'group'      — remove confirmed-offline members from opts.group.
  //   'agents'     — the rich multi-category tool: spans all three
  //                  categories (active agents, retired agents, plain
  //                  conversations) with category / online / search
  //                  filters and four tiers (unjoin, retire, delete,
  //                  reinstate). opts.categories pre-scopes the
  //                  category filter; opts.tier pre-selects the tier.
  //
  // The overlay builds its candidate list from the current snapshot,
  // lets the human edit the include/exclude selection (and bulk-pick
  // by inactivity age), POSTs the explicit conv-id list to
  // /api/cleanup/… and renders the per-item result back. The daemon
  // re-checks tmux liveness for every conv-id, so a conv that came
  // back online between snapshot and submit is reported skipped unless
  // "include online sessions" was opted into.
  function openCleanupModal(opts) {
    const overlay = $('#cleanup-modal');
    const listEl = $('#cleanup-list');
    const optsEl = $('#cleanup-options');
    const catsEl = $('#cleanup-cats');
    const hintEl = $('#cleanup-hint');
    const warnEl = $('#cleanup-warn');
    const errEl = $('#cleanup-error');
    const countEl = $('#cleanup-count');
    const toolbar = $('#cleanup-toolbar');
    const ageInput = $('#cleanup-age');
    const searchInput = $('#cleanup-search');
    const submitBtn = $('#cleanup-submit');
    const cancelBtn = $('#cleanup-cancel');
    const mode = opts.mode;
    const groupName = opts.group || '';
    let phase = 'select';
    // multiCat — only 'agents' mode spans categories and gets
    // the category / search filters and the reinstate tier.
    const multiCat = mode === 'agents';
    // The cleanup tier: unjoin | retire | delete | reinstate.
    // 'agents' mode defaults to delete so every category is visible on
    // open (retire/reinstate would hide other categories); 'group'
    // mode always unjoins.
    let tier = multiCat ? 'delete' : 'unjoin';
    if (opts.tier) tier = opts.tier;
    // Category filter for 'agents' mode. opts.categories, when
    // supplied by a caller, pre-scopes which categories start ticked.
    const catOn = {};
    for (const k of CLEANUP_CATS) {
      catOn[k] = opts.categories ? opts.categories.includes(k) : true;
    }
    // includeOnline — opt-in that lets a tier act on still-running
    // sessions. Off by default: the offline-only safety stance.
    let includeOnline = false;
    let searchText = '';

    // Build the candidate list from the current snapshot. Each entry
    // carries its own `checked` flag so re-renders (filter changes)
    // preserve the human's hand-tuned selection. `category` tags which
    // snapshot list it came from; `lastActivity` is the per-category
    // recency stamp (last_hook / retired_at / modified).
    function buildCandidates() {
      const out = [];
      if (mode === 'group') {
        const g = (lastSnapshot?.groups || []).find(gr => gr.name === groupName);
        for (const m of (g?.members || [])) {
          if (m.online) continue;
          out.push({
            conv_id: m.conv_id, title: m.title || '', category: 'agent',
            online: false, lastActivity: (m.state || {}).last_hook || '',
            owner: !!m.owner, groups: [],
            checked: !m.owner, // owners excluded by default
          });
        }
      } else {
        // agents mode — all three categories, online + offline alike.
        // Nothing is pre-checked: with delete as the default tier,
        // auto-selection would be a footgun.
        for (const a of (lastSnapshot?.agents || [])) {
          out.push({
            conv_id: a.conv_id, title: a.title || '', category: 'agent',
            online: !!a.online, lastActivity: (a.state || {}).last_hook || '',
            owner: !!(a.owned_groups || []).length,
            groups: a.groups || [], checked: false,
          });
        }
        for (const r of (lastSnapshot?.retired || [])) {
          out.push({
            conv_id: r.conv_id, title: r.title || '', category: 'retired',
            online: !!r.online, lastActivity: r.retired_at || '',
            owner: false, groups: [], checked: false,
          });
        }
        for (const c of (lastSnapshot?.conversations || [])) {
          out.push({
            conv_id: c.conv_id, title: c.title || '', category: 'conversation',
            online: !!c.online, lastActivity: c.modified || '',
            owner: false, groups: [], checked: false,
          });
        }
      }
      // Longest-inactive first — what a human cleaning up wants at the
      // top. Missing stamp (orphan / never had a session) sorts oldest.
      out.sort((x, y) => {
        const tx = x.lastActivity ? Date.parse(x.lastActivity) : 0;
        const ty = y.lastActivity ? Date.parse(y.lastActivity) : 0;
        return tx - ty;
      });
      return out;
    }
    const candidates = buildCandidates();

    function inactivityHours(c) {
      if (!c.lastActivity) return Infinity;
      const t = Date.parse(c.lastActivity);
      if (isNaN(t)) return Infinity;
      return (Date.now() - t) / 3600000;
    }
    // activityLabel — the per-category recency line shown on each row.
    function activityLabel(c) {
      if (!c.lastActivity) return 'no recent activity';
      const rel = relTime(c.lastActivity);
      if (c.category === 'retired') return 'retired ' + rel;
      if (c.category === 'conversation') return 'last activity ' + rel;
      return 'last seen ' + rel;
    }

    // cleanupTier is the effective tier for the current mode: group
    // mode is hardwired to unjoin (it hits the single-group endpoint);
    // agents mode reads the radio-backed `tier` variable.
    function cleanupTier() {
      return mode === 'group' ? 'unjoin' : tier;
    }
    // tierCategories — which categories the current tier can act on.
    // delete is universal; reinstate is retired-only; retire / unjoin
    // are agent-only. The tier therefore doubles as a category gate.
    function tierCategories() {
      const t = cleanupTier();
      if (t === 'delete') return CLEANUP_CATS;
      if (t === 'reinstate') return ['retired'];
      return ['agent'];
    }
    function tierRadio(val, label, note) {
      return '<label><input type="radio" name="cleanup-tier" value="' + val + '"' +
        (val === tier ? ' checked' : '') + ' /> ' + label +
        ' <span class="opt-note">— ' + note + '</span></label>';
    }
    function renderOptions() {
      if (mode === 'group') {
        optsEl.innerHTML =
          '<label><input type="checkbox" id="cleanup-opt-owners" /> ' +
          'Include offline owners <span class="opt-note">— also strips their owner status</span></label>';
        return;
      }
      // agents mode: the tier selector (group mode returned above).
      // The reinstate tier has no meaning for a single-group
      // membership cleanup, so it only appears here.
      let radios =
        tierRadio('unjoin', 'Unjoin from groups',
          'stays an agent — only its group memberships are dropped') +
        tierRadio('retire', 'Retire (soft-delete)',
          'demote to a plain conversation: revokes groups + permissions, keeps the .jsonl, reinstatable') +
        tierRadio('delete', 'Delete permanently',
          'wipes the conversation from disk and every agent row — cannot be undone');
      if (multiCat) {
        radios += tierRadio('reinstate', 'Reinstate',
          'return a retired agent to the active roster — groups and permissions are not restored');
      }
      const ownersOpt =
        '<label id="cleanup-opt-owners-row"><input type="checkbox" id="cleanup-opt-owners" /> ' +
        'Include offline owners <span class="opt-note">— unjoin tier only; retire and delete drop owner rows anyway</span></label>';
      const onlineOpt = multiCat
        ? '<label id="cleanup-opt-online-row"><input type="checkbox" id="cleanup-opt-online"' +
          (includeOnline ? ' checked' : '') + ' /> ' +
          'Include online sessions <span class="opt-note">— also act on conversations whose tmux ' +
          'session is still running. Delete force-stops them first; retire / unjoin leave the process ' +
          'running. Reinstate ignores liveness either way.</span></label>'
        : '';
      const wtOpt =
        '<label id="cleanup-opt-wt-row"><input type="checkbox" id="cleanup-opt-wt" checked /> ' +
        'Delete associated git worktrees <span class="opt-note">— removes the worktree directory; ' +
        'the branch and its commits are kept. The main repo and worktrees shared with another ' +
        'agent are always skipped.</span></label>';
      const shutdownOpt =
        '<label id="cleanup-opt-shutdown-row"><input type="checkbox" id="cleanup-opt-shutdown" checked /> ' +
        'Also shut down running sessions <span class="opt-note">— retire tier only; soft-exits ' +
        '(/exit) the tmux pane of each retired agent that is still running. The conversation is ' +
        'kept and reinstatable either way.</span></label>';
      optsEl.innerHTML =
        '<div class="cleanup-tier">' + radios + '</div>' +
        ownersOpt + onlineOpt + shutdownOpt + wtOpt;
      syncTierLocks();
    }
    // syncTierLocks enables each sub-option only for the tier it
    // applies to: owners → unjoin, worktrees → delete, shutdown →
    // retire, include-online → every tier except reinstate (which
    // ignores liveness).
    function syncTierLocks() {
      if (mode === 'group') return;
      const tr = cleanupTier();
      const lock = (id, rowId, enabledWhen) => {
        const cb = $(id), row = $(rowId);
        if (!cb || !row) return;
        cb.disabled = !enabledWhen;
        row.classList.toggle('disabled', !enabledWhen);
      };
      lock('#cleanup-opt-owners', '#cleanup-opt-owners-row', tr === 'unjoin');
      lock('#cleanup-opt-wt', '#cleanup-opt-wt-row', tr === 'delete');
      lock('#cleanup-opt-shutdown', '#cleanup-opt-shutdown-row', tr === 'retire');
      lock('#cleanup-opt-online', '#cleanup-opt-online-row', tr !== 'reinstate');
    }
    function optInclOwners() {
      const cb = $('#cleanup-opt-owners');
      return !!(cb && cb.checked && !cb.disabled);
    }
    function optDeleteWorktrees() {
      const cb = $('#cleanup-opt-wt');
      return !!(cb && cb.checked && !cb.disabled);
    }
    function optIncludeOnline() {
      const cb = $('#cleanup-opt-online');
      return !!(cb && cb.checked && !cb.disabled);
    }
    function optShutdown() {
      const cb = $('#cleanup-opt-shutdown');
      return !!(cb && cb.checked && !cb.disabled);
    }

    // matchesSearch / rowVisible / rowEnabled compose the filter
    // pipeline. A row is visible when it passes the search box, the
    // category checkboxes, the current tier's category gate and the
    // online filter; it is selectable when, additionally, it is not a
    // locked group-mode owner row.
    function matchesSearch(c) {
      if (!searchText) return true;
      const q = searchText.toLowerCase();
      return (c.title || '').toLowerCase().includes(q) ||
             c.conv_id.toLowerCase().includes(q);
    }
    function rowVisible(c) {
      if (!matchesSearch(c)) return false;
      if (!multiCat) return true;
      if (!catOn[c.category]) return false;
      if (!tierCategories().includes(c.category)) return false;
      // Online rows are hidden unless opted in — except under
      // reinstate, which is non-destructive and ignores liveness.
      if (c.online && !includeOnline && cleanupTier() !== 'reinstate') return false;
      return true;
    }
    function rowEnabled(c) {
      if (mode === 'group' && c.owner) return optInclOwners();
      return true;
    }
    // selected() only counts rows the human can currently see — a row
    // checked then hidden by a filter change is not submitted.
    function selected() {
      return candidates.filter(c => rowVisible(c) && rowEnabled(c) && c.checked);
    }

    // renderCategories draws the category-filter row ('agents' mode only).
    function renderCategories() {
      if (!multiCat) { catsEl.style.display = 'none'; return; }
      catsEl.style.display = '';
      catsEl.innerHTML = '<span class="cleanup-cats-label">categories:</span>' +
        CLEANUP_CATS.map(cat => {
          const n = candidates.filter(c => c.category === cat).length;
          return `<label class="cleanup-cat-toggle">
            <input type="checkbox" data-cat="${cat}"${catOn[cat] ? ' checked' : ''} />
            ${esc(CLEANUP_CAT_LABEL[cat])} <span class="muted">(${n})</span>
          </label>`;
        }).join('');
    }

    function rowHTML(c) {
      const enabled = rowEnabled(c);
      const checked = enabled && c.checked;
      const ownerBadge = c.owner ? '<span class="cleanup-badge owner">owner</span>' : '';
      const onlineBadge = c.online ? '<span class="cleanup-badge online">online</span>' : '';
      const metaText = (c.groups && c.groups.length) ? 'in: ' + c.groups.join(', ') : '';
      return `<div class="cleanup-row${enabled ? '' : ' disabled'}">
        <label>
          <input type="checkbox" data-conv="${esc(c.conv_id)}"${checked ? ' checked' : ''}${enabled ? '' : ' disabled'} />
          <span class="title">${esc(c.title || shortId(c.conv_id))}</span>
          <span class="id">${esc(shortId(c.conv_id))}</span>
          ${ownerBadge}${onlineBadge}
          <span class="meta">${esc(metaText)}</span>
          <span class="seen">${esc(activityLabel(c))}</span>
        </label>
      </div>`;
    }
    function renderList() {
      const vis = candidates.filter(rowVisible);
      if (!vis.length) {
        listEl.innerHTML = '<div class="cleanup-empty">' +
          (candidates.length ? 'No conversations match the current filters.'
                             : 'Nothing to clean up.') + '</div>';
        return;
      }
      if (!multiCat) {
        listEl.innerHTML = vis.map(rowHTML).join('');
        return;
      }
      // 'agents' mode: group the visible rows under category sub-headers.
      let html = '';
      for (const cat of CLEANUP_CATS) {
        const rows = vis.filter(c => c.category === cat);
        if (!rows.length) continue;
        html += `<div class="cleanup-cat-head">${esc(CLEANUP_CAT_LABEL[cat])} <span>(${rows.length})</span></div>`;
        html += rows.map(rowHTML).join('');
      }
      listEl.innerHTML = html;
    }

    function recompute() {
      const n = selected().length;
      const tr = cleanupTier();
      countEl.textContent = n + ' selected';
      let label;
      if (mode === 'group') {
        label = n ? `Remove ${n} from ${groupName}` : 'Remove from group';
      } else if (tr === 'delete') {
        label = n ? `Delete ${n} conversation${n === 1 ? '' : 's'} permanently` : 'Delete conversations';
      } else if (tr === 'retire') {
        label = n ? `Retire ${n} agent${n === 1 ? '' : 's'}` : 'Retire agents';
      } else if (tr === 'reinstate') {
        label = n ? `Reinstate ${n} agent${n === 1 ? '' : 's'}` : 'Reinstate agents';
      } else {
        label = n ? `Remove ${n} agent${n === 1 ? '' : 's'} from all groups` : 'Remove from groups';
      }
      submitBtn.textContent = label;
      submitBtn.disabled = n === 0;
      submitBtn.classList.toggle('danger', tr === 'delete');
      applyHint();
    }

    // Bulk-select every visible, selectable row whose inactivity meets
    // the age threshold (0 h selects all visible). A convenience on top
    // of the per-row checkboxes the human can still hand-tune.
    function applyAge() {
      const h = Math.max(0, parseFloat(ageInput.value) || 0);
      for (const c of candidates) {
        if (!rowVisible(c) || !rowEnabled(c)) continue;
        c.checked = inactivityHours(c) >= h;
      }
      renderList();
      recompute();
    }

    function applyChrome() {
      const titleEl = $('#cleanup-title');
      if (mode === 'group') {
        titleEl.textContent = '🧹 Clean up group: ' + groupName;
      } else {
        titleEl.textContent = '🧹 Clean up agents and conversations';
      }
      applyHint();
    }
    // applyHint sets the modal's explanatory line. For group mode it is
    // static; otherwise it tracks the selected tier so the human always
    // sees exactly what the action will do.
    function applyHint() {
      if (phase === 'result') return;
      if (mode === 'group') {
        hintEl.className = 'cleanup-hint';
        hintEl.textContent = 'Removes the selected confirmed-offline members from this group. '
          + 'Their conversations keep running and stay on disk — only the membership is dropped. '
          + 'Owners are excluded unless you opt in below.';
        return;
      }
      const tr = cleanupTier();
      if (tr === 'delete') {
        hintEl.className = 'cleanup-hint danger';
        hintEl.textContent = 'Permanently deletes the selected conversations — wipes the history from '
          + 'disk and drops every group / owner / permission row. Works on active agents, retired '
          + 'agents and plain conversations alike. Cannot be undone.';
      } else if (tr === 'retire') {
        hintEl.className = 'cleanup-hint';
        hintEl.textContent = 'Retires the selected agents: revokes their group memberships and '
          + 'permission grants so they stop being agents — the conversations stay on disk and can '
          + 'be reinstated later. The non-destructive soft-delete. Running sessions are also '
          + 'soft-stopped unless you untick the option below.';
      } else if (tr === 'reinstate') {
        hintEl.className = 'cleanup-hint';
        hintEl.textContent = 'Reinstates the selected retired agents — returns them to the active '
          + 'roster. Their former groups and permissions are not restored; they start fresh.';
      } else {
        hintEl.className = 'cleanup-hint';
        hintEl.textContent = 'Removes the selected agents from every group they belong to. '
          + 'They stay agents (and stay on disk) — only the group memberships are dropped.';
      }
    }

    async function submit() {
      const picks = selected().map(c => c.conv_id);
      if (!picks.length) return;
      errEl.textContent = '';
      submitBtn.disabled = true;
      let url, payload;
      if (mode === 'group') {
        url = '/api/cleanup/group';
        payload = { group: groupName, members: picks, include_owners: optInclOwners() };
      } else {
        url = '/api/cleanup/agents';
        payload = {
          agents: picks,
          mode: cleanupTier(),
          include_owners: optInclOwners(),
          include_online: optIncludeOnline(),
          delete_worktrees: optDeleteWorktrees(),
          shutdown: optShutdown(),
        };
      }
      try {
        const r = await fetch(url, {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload),
        });
        if (!r.ok) {
          errEl.textContent = (await r.text()) || ('HTTP ' + r.status);
          recompute();
          return;
        }
        renderResult(await r.json());
        refresh();
      } catch (err) {
        errEl.textContent = 'Request failed: ' + (err && err.message || err);
        recompute();
      }
    }

    // renderResult swaps the modal into its read-only result phase:
    // the editable list becomes a per-conv outcome log, the action
    // button becomes "Done".
    function renderResult(resp) {
      phase = 'result';
      toolbar.style.display = 'none';
      optsEl.style.display = 'none';
      catsEl.style.display = 'none';
      const outcomes = resp.outcomes || [];
      listEl.innerHTML = outcomes.length
        ? outcomes.map(o => `<div class="cleanup-row">
            <span class="cleanup-badge ${esc(o.result)}">${esc(o.result)}</span>
            <span class="title">${esc(o.title || shortId(o.conv_id))}</span>
            <span class="id">${esc(shortId(o.conv_id))}</span>
            <span class="meta">${esc(o.detail || '')}</span>
          </div>`).join('')
        : '<div class="cleanup-empty">Nothing to do.</div>';
      const bits = [];
      if (resp.removed) bits.push(resp.removed + ' removed');
      if (resp.retired) bits.push(resp.retired + ' retired');
      if (resp.reinstated) bits.push(resp.reinstated + ' reinstated');
      if (resp.deleted) bits.push(resp.deleted + ' deleted');
      if (resp.skipped) bits.push(resp.skipped + ' skipped');
      if (resp.failed) bits.push(resp.failed + ' failed');
      hintEl.className = 'cleanup-hint';
      hintEl.textContent = 'Cleanup complete — ' + (bits.join(' · ') || 'nothing to do') + '.';
      if ((resp.warnings || []).length) {
        warnEl.style.display = 'block';
        warnEl.textContent = '⚠ ' + resp.warnings.join('\n⚠ ');
      }
      errEl.textContent = '';
      submitBtn.textContent = 'Done';
      submitBtn.disabled = false;
      submitBtn.classList.remove('danger');
      cancelBtn.style.display = 'none';
    }

    function close() {
      overlay.classList.remove('show');
      cancelBtn.removeEventListener('click', onCancel);
      submitBtn.removeEventListener('click', onSubmit);
      overlay.removeEventListener('click', onOverlay);
      document.removeEventListener('keydown', onKey);
      $('#cleanup-select-all').removeEventListener('click', onSelectAll);
      $('#cleanup-select-none').removeEventListener('click', onSelectNone);
      ageInput.removeEventListener('input', applyAge);
      searchInput.removeEventListener('input', onSearch);
      catsEl.removeEventListener('change', onCatChange);
      optsEl.removeEventListener('change', onOptChange);
      listEl.removeEventListener('change', onListChange);
    }
    function onCancel() { close(); }
    function onSubmit() { if (phase === 'result') close(); else submit(); }
    function onOverlay(e) { if (e.target === overlay) close(); }
    function onKey(e) { if (e.key === 'Escape') close(); }
    function onSelectAll() {
      for (const c of candidates) { if (rowVisible(c) && rowEnabled(c)) c.checked = true; }
      renderList(); recompute();
    }
    function onSelectNone() {
      for (const c of candidates) c.checked = false;
      renderList(); recompute();
    }
    function onSearch() {
      searchText = searchInput.value.trim();
      renderList(); recompute();
    }
    function onCatChange(e) {
      const cb = e.target.closest('input[type=checkbox]');
      if (!cb) return;
      catOn[cb.getAttribute('data-cat')] = cb.checked;
      renderList(); recompute();
    }
    function onOptChange(e) {
      // Group mode: toggling "include owners" unlocks owner rows and
      // pre-selects them (the human can still hand-uncheck any).
      if (e.target.id === 'cleanup-opt-owners' && mode === 'group') {
        for (const c of candidates) { if (c.owner) c.checked = e.target.checked; }
      }
      // "Include online sessions" toggled — reveal / hide online rows.
      if (e.target.id === 'cleanup-opt-online') {
        includeOnline = e.target.checked;
      }
      // agents mode: a tier radio changed — update the tier and
      // re-lock the dependent sub-options.
      if (e.target.name === 'cleanup-tier') {
        tier = e.target.value;
        syncTierLocks();
      }
      renderList();
      recompute();
    }
    function onListChange(e) {
      const cb = e.target.closest('input[type=checkbox]');
      if (!cb) return;
      const c = candidates.find(x => x.conv_id === cb.getAttribute('data-conv'));
      if (c) c.checked = cb.checked;
      recompute();
    }

    // Reset chrome — a prior result-phase render may have hidden bits.
    toolbar.style.display = '';
    optsEl.style.display = '';
    cancelBtn.style.display = '';
    cancelBtn.textContent = 'Cancel';
    warnEl.style.display = 'none';
    warnEl.textContent = '';
    errEl.textContent = '';
    ageInput.value = '0';
    searchInput.value = '';
    searchInput.style.display = multiCat ? '' : 'none';
    submitBtn.classList.remove('danger');

    applyChrome();
    renderOptions();
    renderCategories();
    renderList();
    recompute();

    cancelBtn.addEventListener('click', onCancel);
    submitBtn.addEventListener('click', onSubmit);
    overlay.addEventListener('click', onOverlay);
    document.addEventListener('keydown', onKey);
    $('#cleanup-select-all').addEventListener('click', onSelectAll);
    $('#cleanup-select-none').addEventListener('click', onSelectNone);
    ageInput.addEventListener('input', applyAge);
    searchInput.addEventListener('input', onSearch);
    catsEl.addEventListener('change', onCatChange);
    optsEl.addEventListener('change', onOptChange);
    listEl.addEventListener('change', onListChange);
    overlay.classList.add('show');
  }

  // resumeAgentReq POSTs the resume endpoint, toasts the per-conv
  // outcome, and refreshes on success. Shared by the "wake" row button
  // and the offline status-dot click — both wake an agent the exact
  // same way. Returns true on success.
  async function resumeAgentReq(conv, label) {
    const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/resume`, {
      method: 'POST', credentials: 'same-origin',
    });
    if (!r.ok) {
      toast(`wake failed: ${await r.text()}`, true);
      return false;
    }
    // Surface the daemon's per-conv result so an "already-online" no-op
    // shows up distinctly from a real wake. The body is JSON shaped
    // like {action: "resumed" | "skipped:already_online" | ...}.
    try {
      const out = await r.json();
      toast(`wake ${label}: ${out.action || 'ok'}`);
    } catch (_) {
      toast(`wake ${label}: ok`);
    }
    refresh();
    return true;
  }

  // stopAgentReq POSTs the stop endpoint with the given blast radius
  // (force=false → soft /exit, force=true → tmux kill), toasts the
  // outcome, and refreshes on success. Shared by the "shut down" row
  // button and the online status-dot click. Returns true on success.
  async function stopAgentReq(conv, label, force) {
    const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/stop`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({force: !!force}),
    });
    if (!r.ok) {
      toast(`shutdown failed: ${await r.text()}`, true);
      return false;
    }
    try {
      const out = await r.json();
      toast(`shutdown ${label}: ${out.action || 'ok'}`);
    } catch (_) {
      toast(`shutdown ${label}: ok`);
    }
    refresh();
    return true;
  }

  // bindRowActions delegates clicks on row-action buttons to the
  // appropriate /api/groups/... call. After a successful mutation we
  // re-fetch the snapshot so the badge / button state updates.
  function bindRowActions() {
    document.addEventListener('click', async (e) => {
      const btn = e.target.closest('[data-act]');
      if (!btn) return;
      // Buttons may live inside <summary>, where the default click
      // action is to toggle the details. Stop that.
      e.preventDefault();
      const act = btn.getAttribute('data-act');
      const group = btn.getAttribute('data-group');
      const conv = btn.getAttribute('data-conv');
      const label = btn.getAttribute('data-label') || conv;
      try {
        let ok = false;
        switch (act) {
          case 'cycle-group-offline': {
            // Pure client-side view state — cycle the per-group
            // offline override inherit → show → hide and re-render.
            // No daemon round-trip.
            const okey = 'tclaude.dash.group.offline.' + group;
            const cur = groupOfflineOverride(group);
            const next = cur === 'inherit' ? 'show' : cur === 'show' ? 'hide' : 'inherit';
            if (next === 'inherit') localStorage.removeItem(okey);
            else localStorage.setItem(okey, next);
            renderGroupsTab();
            return;
          }
          case 'remove-member': {
            const confirmed = await confirmModal({
              title: 'Remove member from group?',
              body: 'This unsubscribes them from group messages and severs the manager-pattern path. Their conv keeps running.',
              meta: `${label} → ${group}`,
              okLabel: 'Remove',
            });
            if (!confirmed) return;
            const r = await fetch(`/api/groups/${encodeURIComponent(group)}/members/${encodeURIComponent(conv)}`, {
              method: 'DELETE', credentials: 'same-origin',
            });
            ok = r.ok;
            if (!ok) toast(`Remove failed: ${await r.text()}`, true);
            break;
          }
          case 'grant-owner': {
            // Granting owner is non-destructive; skip the confirm
            // modal but still re-fetch on success.
            const r = await fetch(`/api/groups/${encodeURIComponent(group)}/owners`, {
              method: 'POST', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ conv }),
            });
            ok = r.ok;
            if (!ok) toast(`Grant owner failed: ${await r.text()}`, true);
            break;
          }
          case 'jump': {
            // Non-destructive; no confirm modal, just fire-and-toast.
            const r = await fetch(`/api/jump/${encodeURIComponent(conv)}`, {
              method: 'POST', credentials: 'same-origin',
            });
            ok = r.ok;
            if (!ok) toast(`Jump failed: ${await r.text()}`, true);
            // Skip the default refresh — focusing doesn't change any
            // dashboard state and the user just left the window.
            if (ok) toast(`focused: ${label}`);
            return;
          }
          case 'term': {
            // Pick which directory, then ask the daemon to spawn a
            // terminal window there. Non-destructive and changes no
            // dashboard state, so skip the refresh.
            const which = await termDirModal({ label });
            if (!which) return;
            const r = await fetch(`/api/term/${encodeURIComponent(conv)}`, {
              method: 'POST', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ which }),
            });
            if (!r.ok) { toast(`Open terminal failed: ${await r.text()}`, true); return; }
            const info = await r.json().catch(() => ({}));
            toast(`terminal opened: ${info.dir || label}`);
            return;
          }
          case 'term-dir': {
            // Click on a CWD path cell — the cell already names one
            // specific directory, so open a terminal there straight
            // away, skipping the term button's 3-way picker modal.
            const which = btn.getAttribute('data-which') || 'current';
            const r = await fetch(`/api/term/${encodeURIComponent(conv)}`, {
              method: 'POST', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ which }),
            });
            if (!r.ok) { toast(`Open terminal failed: ${await r.text()}`, true); return; }
            const info = await r.json().catch(() => ({}));
            toast(`terminal opened: ${info.dir || which}`);
            return;
          }
          case 'sudo-grant': {
            // Per-row affordance: open the same modal the Sudo tab's
            // "+ Grant sudo" button uses, pre-filled with this conv.
            // Modal handles the rest (validation, POST /api/sudo,
            // refresh).
            openSudoGrantModal(conv);
            return;
          }
          case 'perm-edit': {
            // Per-row affordance: open the permanent-permission editor
            // for this agent. Distinct from sudo-grant — that elevation
            // is time-bounded, these overrides persist.
            openPermEditModal(conv, label);
            return;
          }
          case 'sudo-manage': {
            // Click on the 🔓 badge: switch to the Sudo tab pre-
            // filtered to this agent so the human can revoke specific
            // grants without scrolling through unrelated rows.
            const filterInput = $('#filter-sudo');
            filterInput.value = shortId(conv);
            try { localStorage.setItem('tclaude.dash.filter.sudo', filterInput.value); } catch (_) {}
            $$('nav button').forEach(x => x.classList.toggle('active', x.dataset.tab === 'sudo'));
            $$('main section').forEach(s => s.classList.toggle('active', s.id === 'tab-sudo'));
            renderSudoTab();
            return;
          }
          case 'sudo-revoke': {
            const id = btn.getAttribute('data-id');
            const slug = btn.getAttribute('data-slug') || '';
            const confirmed = await confirmModal({
              title: 'Revoke sudo grant?',
              body: 'The agent loses access to this slug immediately. They can request again if needed.',
              meta: `#${id} ${slug ? '· ' + slug : ''}${label ? ' · ' + label : ''}`,
              okLabel: 'Revoke',
            });
            if (!confirmed) return;
            const r = await fetch('/api/sudo/' + encodeURIComponent(id), {
              method: 'DELETE', credentials: 'same-origin',
            });
            ok = r.ok;
            if (!ok) toast(`Revoke failed: ${await r.text()}`, true);
            break;
          }
          case 'promote-agent': {
            // Conversations list → roster. Backend PromoteAgent also
            // reinstates a retired conv, so this one button covers
            // both "never an agent" and "was retired".
            const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/promote`, {
              method: 'POST', credentials: 'same-origin',
            });
            ok = r.ok;
            if (!ok) { toast(`Promote failed: ${await r.text()}`, true); break; }
            toast(`promoted to agent: ${label}`);
            break;
          }
          case 'retire-agent': {
            const choice = await retireConfirm({ label });
            if (!choice) return;
            const q = choice.shutdown ? '?shutdown=1' : '?shutdown=0';
            const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/retire${q}`, {
              method: 'POST', credentials: 'same-origin',
            });
            ok = r.ok;
            if (!ok) { toast(`Retire failed: ${await r.text()}`, true); break; }
            toast(choice.shutdown ? `retired + session stopped: ${label}` : `retired: ${label}`);
            break;
          }
          case 'reinstate-agent': {
            const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/reinstate`, {
              method: 'POST', credentials: 'same-origin',
            });
            ok = r.ok;
            if (!ok) { toast(`Reinstate failed: ${await r.text()}`, true); break; }
            toast(`reinstated: ${label}`);
            break;
          }
          case 'delete-agent': {
            const choice = await deleteAgentModal(conv, label);
            if (!choice) return;
            const q = choice.deleteWorktree ? '?delete_worktree=1' : '';
            const r = await fetch(`/api/agents/${encodeURIComponent(conv)}${q}`, {
              method: 'DELETE', credentials: 'same-origin',
            });
            ok = r.ok;
            if (!ok) {
              toast(`Delete failed: ${await r.text()}`, true);
              break;
            }
            // Surface the worktree outcome when one was requested —
            // the DELETE returns 200 + JSON in that case.
            try {
              const out = await r.json();
              toast(out.worktree ? `deleted ${label} · ${out.worktree}` : `deleted ${label}`);
            } catch (_) {
              toast(`deleted ${label}`);
            }
            refresh();
            return;
          }
          case 'edit-member': {
            const cur = {
              role: btn.getAttribute('data-role') || '',
              descr: btn.getAttribute('data-descr') || '',
            };
            const result = await editMemberModal({
              label: `${label} → ${group}`,
              role: cur.role, descr: cur.descr,
            });
            if (result === null) return; // cancelled
            if (result === 'noop') {
              toast('no changes');
              return;
            }
            const r = await fetch(`/api/groups/${encodeURIComponent(group)}/members/${encodeURIComponent(conv)}`, {
              method: 'PATCH', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify(result),
            });
            ok = r.ok;
            if (!ok) toast(`edit failed: ${await r.text()}`, true);
            break;
          }
          case 'wake-agent': {
            // Resume is non-destructive (only spawns a tmux session;
            // the conv jsonl is unchanged). No confirm modal — fire +
            // toast + refresh on success. Idempotent server-side.
            await resumeAgentReq(conv, label);
            return; // Skip the default toast — resumeAgentReq toasted.
          }
          case 'shutdown-agent': {
            const choice = await shutdownConfirm({label});
            if (!choice) return;
            await stopAgentReq(conv, label, choice === 'force');
            return; // Skip the default toast — stopAgentReq toasted.
          }
          case 'dot-toggle': {
            // The per-agent status light doubles as an on/off toggle.
            // It reuses the same resume / stop endpoints the "wake" and
            // "shut down" row buttons hit — no parallel endpoint.
            //   - offline dot → wake (resume). Non-destructive; starting
            //     a session never needs a confirm.
            //   - online dot → confirm first, then soft-stop. The
            //     confirm fires for EVERY online click, idle or busy.
            //     The dot's rendered state can be stale by click time
            //     (the snapshot refreshes asynchronously), so a dot
            //     that looks idle may front an agent that has since
            //     started working — skipping the confirm there would
            //     silently interrupt it. Always asking closes that race
            //     and keeps every green-dot click behaving identically.
            // online is read from data-* set by agentStatusDot.
            const online = btn.getAttribute('data-online') === '1';
            if (!online) {
              await resumeAgentReq(conv, label);
              return;
            }
            const confirmed = await confirmModal({
              title: 'Turn off this agent?',
              body: 'Turning this agent off injects /exit into its pane. If it is mid-task, any in-flight tool call is interrupted. The conversation is preserved and the agent can be turned back on (resumed) later — nothing is deleted or retired.',
              meta: label,
              okLabel: 'Turn off',
            });
            if (!confirmed) return;
            await stopAgentReq(conv, label, false);
            return;
          }
          case 'add-member': {
            // Pop the candidate-list overlay. The overlay manages its
            // own POSTs + optimistic refresh; we just await its
            // close so the trailing toast/refresh logic doesn't fire
            // (the overlay already handled that per-add).
            await addMemberModal(group);
            return;
          }
          case 'spawn-agent': {
            // Open the spawn modal pre-pinned to this group. The
            // modal manages its own POST + refresh on success.
            openAgentSpawnModal({groupName: group});
            return;
          }
          case 'clone': {
            // Open the clone modal pre-populated with this agent. The
            // modal handles the POST + refresh. data-cwd seeds the
            // worktree picker with the source agent's repo.
            openCloneAgentModal(conv, label, btn.getAttribute('data-cwd') || '');
            return;
          }
          case 'reincarnate': {
            // Open the reincarnate modal pre-populated with this
            // agent. The modal enforces the required follow_up and
            // handles the POST + refresh.
            openReincarnateAgentModal(conv, label);
            return;
          }
          case 'rename-agent': {
            const current = btn.getAttribute('data-current') || '';
            openRenameAgentModal(conv, label, current);
            return;
          }
          case 'rename-group': {
            // Inline edit: replace the group's <strong> label with an
            // <input>. Enter saves (POST /api/groups/{old}/rename),
            // Esc cancels (revert without touching the daemon).
            // Background poll is suspended while editing so a 5s
            // refresh doesn't blow the input away mid-type.
            const summary = btn.closest('summary');
            const nameEl = summary && summary.querySelector('.group-name');
            if (!nameEl) {
              toast('rename: could not locate group name element', true);
              return;
            }
            // Suspend the auto-refresh while the input is open. The
            // refresh re-runs renderGroups which would replace our
            // input back with the static strong, losing keystrokes.
            const prevSnapshot = lastSnapshot;
            renameEditing = true;
            const oldName = group;
            const input = document.createElement('input');
            input.type = 'text';
            input.className = 'group-rename-input';
            input.value = oldName;
            input.spellcheck = false;
            input.autocomplete = 'off';
            // Replace + focus + select.
            nameEl.replaceWith(input);
            input.focus();
            input.select();
            const restore = () => {
              const restored = document.createElement('strong');
              restored.className = 'group-name';
              restored.dataset.groupName = oldName;
              restored.textContent = oldName;
              if (input.parentNode) input.replaceWith(restored);
              renameEditing = false;
              lastSnapshot = prevSnapshot;
            };
            const commit = async () => {
              const newName = input.value;
              if (newName === oldName || newName.trim() === '') {
                restore();
                return;
              }
              const r = await fetch(`/api/groups/${encodeURIComponent(oldName)}/rename`, {
                method: 'POST', credentials: 'same-origin',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ new_name: newName }),
              });
              if (!r.ok) {
                toast(`rename failed: ${await r.text()}`, true);
                restore();
                return;
              }
              // Move the persisted "is open" flag onto the new key so
              // the details stays in the state the user left it in.
              const wasOpen = localStorage.getItem('tclaude.dash.group.' + oldName) === '1';
              localStorage.removeItem('tclaude.dash.group.' + oldName);
              if (wasOpen) localStorage.setItem('tclaude.dash.group.' + newName, '1');
              renameEditing = false;
              toast(`renamed: ${oldName} → ${newName}`);
              refresh();
            };
            input.addEventListener('keydown', (ev) => {
              if (ev.key === 'Enter') { ev.preventDefault(); commit(); }
              else if (ev.key === 'Escape') { ev.preventDefault(); restore(); }
            });
            input.addEventListener('blur', () => {
              // Blur cancels rather than commits — avoids accidentally
              // posting a stale name when the user clicks elsewhere
              // mid-edit. They have to commit explicitly with Enter.
              if (renameEditing) restore();
            });
            return; // Skip the default refresh; commit() / restore() handle it.
          }
          case 'set-group-dir': {
            // Inline edit of the group's default spawn directory.
            // The 📁 chip itself is the click target (data-act lives
            // on the .group-default-cwd span), so btn IS the chip:
            // replace it with an <input>, Enter saves (PATCH
            // /api/groups/{name}), Esc / blur cancels. Auto-refresh
            // suspended via renameEditing so the 5s tick can't drop
            // the input. Fall back to a summary lookup in case the
            // click landed on a descendant rather than the span.
            const cwdEl = btn.classList.contains('group-default-cwd')
              ? btn
              : (btn.closest('summary') && btn.closest('summary').querySelector('.group-default-cwd'));
            if (!cwdEl) {
              toast('start dir: could not locate the dir element', true);
              return;
            }
            const prevSnapshot = lastSnapshot;
            renameEditing = true;
            const origEl = cwdEl.cloneNode(true);
            const oldCwd = cwdEl.getAttribute('data-cwd') || '';
            const input = document.createElement('input');
            input.type = 'text';
            input.className = 'group-default-cwd-input';
            input.value = oldCwd;
            input.placeholder = 'absolute path — empty clears the default';
            input.spellcheck = false;
            input.autocomplete = 'off';
            cwdEl.replaceWith(input);
            input.focus();
            input.select();
            const restore = () => {
              if (input.parentNode) input.replaceWith(origEl);
              renameEditing = false;
              lastSnapshot = prevSnapshot;
            };
            const commit = async () => {
              const newCwd = input.value.trim();
              if (newCwd === oldCwd) {
                restore();
                return;
              }
              const r = await fetch(`/api/groups/${encodeURIComponent(group)}`, {
                method: 'PATCH', credentials: 'same-origin',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ default_cwd: newCwd }),
              });
              if (!r.ok) {
                toast(`set dir failed: ${await r.text()}`, true);
                restore();
                return;
              }
              renameEditing = false;
              toast(newCwd ? `${group}: default dir → ${newCwd}` : `${group}: default dir cleared`);
              refresh();
            };
            input.addEventListener('keydown', (ev) => {
              if (ev.key === 'Enter') { ev.preventDefault(); commit(); }
              else if (ev.key === 'Escape') { ev.preventDefault(); restore(); }
            });
            input.addEventListener('blur', () => {
              // Blur cancels (like rename) — explicit Enter to save.
              if (renameEditing) restore();
            });
            return; // Skip the default refresh; commit() / restore() handle it.
          }
          case 'set-group-max-members': {
            // Inline edit of the group's hard member cap (the 👥
            // chip). Mirrors set-group-dir: swap the chip for a number
            // <input>, Enter PATCHes /api/groups/{name}, Esc / blur
            // cancels. Auto-refresh suspended via renameEditing so the
            // 5s tick can't drop the input mid-edit.
            const capEl = btn.classList.contains('group-max-members')
              ? btn
              : (btn.closest('summary') && btn.closest('summary').querySelector('.group-max-members'));
            if (!capEl) {
              toast('max members: could not locate the cap element', true);
              return;
            }
            const prevSnapshot = lastSnapshot;
            renameEditing = true;
            const origEl = capEl.cloneNode(true);
            const oldMax = parseInt(capEl.getAttribute('data-max') || '0', 10) || 0;
            const input = document.createElement('input');
            input.type = 'number';
            input.min = '0';
            input.step = '1';
            input.className = 'group-max-members-input';
            input.value = String(oldMax);
            input.title = '0 clears the cap (unlimited)';
            capEl.replaceWith(input);
            input.focus();
            input.select();
            const restore = () => {
              if (input.parentNode) input.replaceWith(origEl);
              renameEditing = false;
              lastSnapshot = prevSnapshot;
            };
            const commit = async () => {
              const newMax = parseInt(input.value, 10);
              if (!Number.isInteger(newMax) || newMax < 0) {
                toast('max members must be a non-negative integer (0 = unlimited)', true);
                restore();
                return;
              }
              if (newMax === oldMax) {
                restore();
                return;
              }
              const r = await fetch(`/api/groups/${encodeURIComponent(group)}`, {
                method: 'PATCH', credentials: 'same-origin',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ max_members: newMax }),
              });
              if (!r.ok) {
                toast(`set max members failed: ${await r.text()}`, true);
                restore();
                return;
              }
              renameEditing = false;
              toast(newMax > 0 ? `${group}: member cap → ${newMax}` : `${group}: member cap cleared`);
              refresh();
            };
            input.addEventListener('keydown', (ev) => {
              if (ev.key === 'Enter') { ev.preventDefault(); commit(); }
              else if (ev.key === 'Escape') { ev.preventDefault(); restore(); }
            });
            input.addEventListener('blur', () => {
              if (renameEditing) restore();
            });
            return; // Skip the default refresh; commit() / restore() handle it.
          }
          case 'export-group': {
            // Export is a file download, not a mutation. Trigger it via
            // a transient anchor so the browser saves the .zip (the
            // endpoint sets Content-Disposition); the cookie rides along
            // on the same-origin GET. Return so the default toast +
            // refresh do not fire — nothing changed.
            const a = document.createElement('a');
            a.href = `/api/groups/${encodeURIComponent(group)}/export`;
            a.download = '';
            document.body.appendChild(a);
            a.click();
            a.remove();
            toast(`Exporting group "${group}"…`);
            return;
          }
          case 'cleanup-group': {
            // Open the bulk-cleanup overlay scoped to this group. The
            // modal manages its own POST + refresh on success.
            openCleanupModal({ mode: 'group', group });
            return;
          }
          case 'emergency-shutdown-group': {
            // emergencyShutdown owns its confirm modal, POST, toast
            // and refresh — return so the default toast doesn't fire.
            await emergencyShutdown('group', group);
            return;
          }
          case 'emergency-shutdown-all': {
            // The top-bar button: shut down every running agent the
            // dashboard shows. No group context.
            await emergencyShutdown('all', null);
            return;
          }
          case 'window-modal-group': {
            // openWindowModal owns its modal, POST and toast — return
            // so the default toast/refresh doesn't fire.
            openWindowModal('group', group);
            return;
          }
          case 'window-modal-all': {
            // The top-bar button: focus/unfocus windows across every
            // agent on the dashboard. No group context.
            openWindowModal('all', null);
            return;
          }
          case 'set-group-context': {
            // Open the group startup-context editor. Unlike the cwd
            // chip's inline <input>, the context is multi-line, so it
            // gets its own modal with a <textarea>.
            openGroupContextModal(group);
            return; // Modal owns the save + refresh.
          }
          case 'delete-group': {
            const memberCount = parseInt(btn.getAttribute('data-members') || '0', 10);
            const confirmed = await confirmModal({
              title: 'Delete group?',
              body: `This drops the group plus all ${memberCount} membership row(s), any owner grants, and the entire group message history. The conversations themselves keep running.`,
              meta: group,
              okLabel: 'Delete group',
            });
            if (!confirmed) return;
            const r = await fetch(`/api/groups/${encodeURIComponent(group)}`, {
              method: 'DELETE', credentials: 'same-origin',
            });
            ok = r.ok;
            if (!ok) toast(`Delete failed: ${await r.text()}`, true);
            break;
          }
          case 'revoke-owner': {
            const confirmed = await confirmModal({
              title: 'Revoke owner status?',
              body: 'They will lose the implicit power to manage other members of this group (message, reincarnate, compact, rename, clone). The membership row stays.',
              meta: `${label} → ${group}`,
              okLabel: 'Revoke',
            });
            if (!confirmed) return;
            const r = await fetch(`/api/groups/${encodeURIComponent(group)}/owners/${encodeURIComponent(conv)}`, {
              method: 'DELETE', credentials: 'same-origin',
            });
            ok = r.ok;
            if (!ok) toast(`Revoke failed: ${await r.text()}`, true);
            break;
          }
          case 'cron-enable':
          case 'cron-disable': {
            const id = btn.getAttribute('data-id');
            const verb = act === 'cron-enable' ? 'enable' : 'disable';
            const r = await fetch(`/api/cron/${encodeURIComponent(id)}/${verb}`, {
              method: 'POST', credentials: 'same-origin',
            });
            ok = r.ok;
            if (!ok) toast(`${verb} failed: ${await r.text()}`, true);
            break;
          }
          case 'cron-run-now': {
            const id = btn.getAttribute('data-id');
            // Run-now is non-destructive (it just fires the job once)
            // but it does send a real message to the target — confirm
            // so a stray click doesn't paste into someone's pane.
            const confirmed = await confirmModal({
              title: 'Fire this cron job now?',
              body: 'Sends the job\'s message to its target immediately. Stamps last_run_at so the regular cadence resumes from now.',
              meta: label,
              okLabel: 'Fire now',
            });
            if (!confirmed) return;
            const r = await fetch(`/api/cron/${encodeURIComponent(id)}/run-now`, {
              method: 'POST', credentials: 'same-origin',
            });
            ok = r.ok;
            if (!ok) toast(`run-now failed: ${await r.text()}`, true);
            break;
          }
          case 'cron-delete': {
            const id = btn.getAttribute('data-id');
            const confirmed = await confirmModal({
              title: 'Delete cron job?',
              body: 'Removes the job and its run history. The target itself is unaffected; you can re-create the job with `tclaude agent cron add`.',
              meta: label,
              okLabel: 'Delete job',
            });
            if (!confirmed) return;
            const r = await fetch(`/api/cron/${encodeURIComponent(id)}`, {
              method: 'DELETE', credentials: 'same-origin',
            });
            ok = r.ok;
            if (!ok) toast(`delete failed: ${await r.text()}`, true);
            break;
          }
          case 'cron-edit': {
            const id = parseInt(btn.getAttribute('data-id'), 10);
            const job = (lastSnapshot?.cron || []).find(j => j.id === id);
            if (!job) {
              toast(`edit: job #${id} not in current snapshot`, true);
              return;
            }
            openCronEditModal(job);
            return;
          }
          case 'cron-new': {
            // Context-aware "+ new cron job" buttons from the Agents
            // tab (per-agent + per-group) land here. data-prefill is a JSON blob
            // describing the prefill state (targetMode, target,
            // groupName, owner). Empty / missing → default form.
            let prefill = {};
            const raw = btn.getAttribute('data-prefill');
            if (raw) {
              try { prefill = JSON.parse(raw); } catch (_) {}
            }
            openCronCreateModal(prefill);
            return;
          }
          case 'message-new': {
            // Context-aware "send message" buttons from the Agents row
            // (✉, solo) and group headers (✉ message, group multicast).
            // data-prefill is a JSON blob: { from, targetMode, target,
            // groupName }. Empty / missing → default form.
            let prefill = {};
            const raw = btn.getAttribute('data-prefill');
            if (raw) {
              try { prefill = JSON.parse(raw); } catch (_) {}
            }
            openMessageCreateModal(prefill);
            return;
          }
          case 'link-new': {
            // From per-group "+ link" button: preset FROM to the
            // current group so the user only has to pick TO.
            const from = btn.getAttribute('data-from') || '';
            openLinkModal({ mode: 'create', preset: { from } });
            return;
          }
          case 'link-edit': {
            const id = btn.getAttribute('data-id');
            const from = btn.getAttribute('data-from') || '';
            const to = btn.getAttribute('data-to') || '';
            const linkMode = btn.getAttribute('data-mode') || '';
            openLinkModal({ mode: 'edit', linkID: id, preset: { from, to, linkMode } });
            return;
          }
          case 'link-delete': {
            const id = btn.getAttribute('data-id');
            const from = btn.getAttribute('data-from') || '';
            const to = btn.getAttribute('data-to') || '';
            // The dashboard's DELETE endpoint requires the link to
            // touch the group in the URL — pass the from group when
            // available, otherwise fall back to the explicit data-group
            // attribute.
            const scope = btn.getAttribute('data-group') || from || to;
            const confirmed = await confirmModal({
              title: 'Remove this link?',
              body: 'Members of FROM lose the ability to message members of TO via this edge. Other groups / links are unaffected. This can\'t be undone — recreate to restore.',
              meta: `#${id} · ${from} → ${to}`,
              okLabel: 'Remove link',
            });
            if (!confirmed) return;
            const r = await fetch(`/api/groups/${encodeURIComponent(scope)}/links/${encodeURIComponent(id)}`, {
              method: 'DELETE', credentials: 'same-origin',
            });
            ok = r.ok;
            if (!ok) toast(`Remove link failed: ${await r.text()}`, true);
            break;
          }
          case 'msg-focus': {
            // Raise the sending agent's window AND mark the message
            // read — the human is acting on it. Both are non-destructive;
            // toast each, then refresh so the read state + badge update.
            const id = btn.getAttribute('data-id');
            const jr = await fetch(`/api/jump/${encodeURIComponent(conv)}`, {
              method: 'POST', credentials: 'same-origin',
            });
            if (jr.ok) toast(`focused: ${label}`);
            else toast(`Focus failed: ${await jr.text()}`, true);
            const rr = await fetch('/api/human-messages/read', {
              method: 'POST', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ id: parseInt(id, 10) }),
            });
            // Surface a read failure rather than swallow it (parity with
            // msg-mark-read); the jump already happened, so still refresh.
            if (!rr.ok) toast(`Mark read failed: ${await rr.text()}`, true);
            refresh();
            return;
          }
          case 'msg-mark-read': {
            const id = btn.getAttribute('data-id');
            const r = await fetch('/api/human-messages/read', {
              method: 'POST', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ id: parseInt(id, 10) }),
            });
            if (!r.ok) { toast(`Mark read failed: ${await r.text()}`, true); return; }
            toast('message marked read');
            refresh();
            return;
          }
          case 'msg-mark-all-read': {
            const r = await fetch('/api/human-messages/read', {
              method: 'POST', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ all: true }),
            });
            if (!r.ok) { toast(`Mark all read failed: ${await r.text()}`, true); return; }
            const res = await r.json().catch(() => ({}));
            toast(`marked ${res.marked || 0} message(s) read`);
            refresh();
            return;
          }
          case 'msg-clear': {
            const confirmed = await confirmModal({
              title: 'Clear read messages?',
              body: 'Permanently deletes every message that has been marked read. Unread messages are kept.',
              okLabel: 'Clear read',
            });
            if (!confirmed) return;
            const r = await fetch('/api/human-messages/clear', {
              method: 'POST', credentials: 'same-origin',
            });
            if (!r.ok) { toast(`Clear failed: ${await r.text()}`, true); return; }
            const res = await r.json().catch(() => ({}));
            toast(`cleared ${res.deleted || 0} read message(s)`);
            refresh();
            return;
          }
          default:
            return;
        }
        if (ok) {
          toast(`${act.replace('-', ' ')}: ${label}`);
          refresh();
        }
      } catch (err) {
        toast(`Request failed: ${err && err.message || err}`, true);
      }
    });
  }

  // Drag-and-drop: move a member row from group A onto group B's
  // <summary> header to migrate. Optimistic local mutation runs first
  // so the user sees the move immediately; the daemon round-trip
  // confirms (or snaps back on failure).
  //
  // Order on success: POST /api/groups/B/members → DELETE
  // /api/groups/A/members/{conv}. POST first guarantees the conv is
  // never groupless mid-drag — on a failed delete it ends up in both
  // groups (visible, recoverable) instead of nowhere (silently lost).
  //
  // Auto-refresh suspends while a drag is in flight via the
  // dndDragActive flag below — refreshSuspended() checks it — so a 5s
  // tick doesn't blow our optimistic mutation away while the
  // round-trip is mid-air. The drag deliberately does NOT share the
  // modal suspension: a single shared boolean let a drag and a modal
  // clobber each other's reset, which is how auto-refresh used to
  // wedge after a drag-and-drop retire.
  let dndDragActive = false;
  // dndSourceUngrouped / dndSourceConversation / dndSourceRetired: which
  // virtual group the dragged row comes from. Set in dragstart, cleared
  // in dragend. dragover can't read the DataTransfer payload (browsers
  // gate getData to the drop event), so these module-level flags are
  // how the hover handlers tell an inert no-op (e.g. an ungrouped row
  // onto Ungrouped, or a retired row onto Retired) from a real op.
  let dndSourceUngrouped = false;
  let dndSourceConversation = false;
  let dndSourceRetired = false;
  // Every droppable summary — real group headers AND the two droppable
  // virtual group headers (Ungrouped, Retired). The DnD listeners share
  // this selector. The Conversations header is a drag SOURCE only.
  const DND_TARGET_SEL = 'summary[data-dnd-target-group],summary[data-dnd-target-ungrouped],summary[data-dnd-target-retired]';
  // updateDndPill positions + labels the hint pill that tracks the
  // cursor during a drag. `info` is null to hide the pill, else
  // {text, clone} — text is the action label, clone tints it green.
  function updateDndPill(e, info) {
    const pill = $('#dnd-pill');
    if (!info) {
      pill.classList.remove('show', 'clone');
      return;
    }
    pill.textContent = info.text;
    pill.classList.toggle('clone', !!info.clone);
    pill.classList.add('show');
    // Offset slightly from the cursor so the pill doesn't sit on top
    // of the user's pointer. clientX/clientY on `dragover` events
    // jitter on some browsers; the offset masks that.
    pill.style.transform = `translate(${e.clientX + 12}px, ${e.clientY + 12}px)`;
  }
  function bindDnd() {
    document.addEventListener('dragstart', (e) => {
      const row = e.target.closest('.dnd-draggable');
      if (!row) return;
      const conv = row.getAttribute('data-dnd-conv');
      const sourceGroup = row.getAttribute('data-dnd-source-group');
      const sourceUngrouped = row.hasAttribute('data-dnd-source-ungrouped');
      const sourceConversation = row.hasAttribute('data-dnd-source-conversation');
      const sourceRetired = row.hasAttribute('data-dnd-source-retired');
      const label = row.getAttribute('data-dnd-label') || conv;
      // A draggable row is a real-group member (has a source group), a
      // virtual-Ungrouped row, a virtual-Conversations row, or a
      // virtual-Retired row. Anything else isn't a valid drag.
      if (!conv || (!sourceGroup && !sourceUngrouped && !sourceConversation && !sourceRetired)) return;
      // Stash the payload on the DataTransfer so the eventual drop can
      // read it without globals. The MIME type 'text/plain' is the
      // most-supported channel; the JSON body keeps the encoding
      // self-describing. We allow both move (default) and copy effects
      // so Ctrl-drag can flip the cursor hint via dropEffect.
      const payload = JSON.stringify({conv, sourceGroup: sourceGroup || '', sourceUngrouped, sourceConversation, sourceRetired, label});
      e.dataTransfer.setData('application/x-tclaude-member', payload);
      e.dataTransfer.setData('text/plain', payload);
      e.dataTransfer.effectAllowed = 'copyMove';
      row.classList.add('dnd-source-row');
      dndDragActive = true;
      dndSourceUngrouped = sourceUngrouped;
      dndSourceConversation = sourceConversation;
      dndSourceRetired = sourceRetired;
      // dndDragActive (set above) is what suspends auto-refresh for the
      // duration of the drag — see refreshSuspended().
    });
    document.addEventListener('dragend', (e) => {
      // Clear the drag state FIRST, ahead of any DOM cleanup below: if
      // a classList / query call here ever threw, auto-refresh must
      // still come back. dragend fires for every drag that had a
      // dragstart — a successful drop, an Escape-cancel, or a release
      // over nothing — so this is the one guaranteed reset covering
      // every drag-end outcome (join, leave, retire, reinstate,
      // promote, clone, cancelled drop, error path).
      dndDragActive = false;
      dndSourceUngrouped = false;
      dndSourceConversation = false;
      dndSourceRetired = false;
      const row = e.target.closest('.dnd-draggable');
      if (row) row.classList.remove('dnd-source-row');
      // Clear any lingering hover highlight (Firefox sometimes fires
      // dragend without a final dragleave on the target).
      $$('summary.dnd-drop-over').forEach(s => s.classList.remove('dnd-drop-over', 'dnd-effect-clone'));
      $('#dnd-pill').classList.remove('show', 'clone');
      refresh();
    });
    document.addEventListener('dragover', (e) => {
      if (!dndDragActive) return;
      const summary = e.target.closest(DND_TARGET_SEL);
      if (!summary) {
        updateDndPill(e, null);
        return;
      }
      const targetUngrouped = summary.hasAttribute('data-dnd-target-ungrouped');
      const targetRetired = summary.hasAttribute('data-dnd-target-retired');
      // No-op drops — don't preventDefault (so `drop` never fires) and
      // don't show a hint:
      //   - a row onto the virtual group it already lives in;
      //   - a plain conversation onto Retired (only agents can retire).
      if ((targetUngrouped && dndSourceUngrouped) ||
          (targetRetired && (dndSourceRetired || dndSourceConversation))) {
        updateDndPill(e, null);
        return;
      }
      e.preventDefault(); // required for drop to fire on this element
      // Clone is meaningful only for a real-group target, and never for
      // a retired source (that path reinstates, it doesn't clone).
      const isClone = (!!e.ctrlKey || !!e.metaKey) && !targetUngrouped && !targetRetired && !dndSourceRetired;
      e.dataTransfer.dropEffect = isClone ? 'copy' : 'move';
      summary.classList.toggle('dnd-effect-clone', isClone);
      let text;
      if (targetRetired) text = '↓ retire — demote to conversation';
      else if (targetUngrouped) text = dndSourceRetired ? '↓ reinstate (no group)' : dndSourceConversation ? '↓ promote (no group)' : '↓ remove from group';
      else if (isClone) text = '→ clone into group';
      else if (dndSourceRetired) text = '→ reinstate + join group';
      else if (dndSourceConversation) text = '→ promote into group';
      else if (dndSourceUngrouped) text = '→ add to group';
      else text = '→ move to group';
      updateDndPill(e, {text, clone: isClone});
    });
    document.addEventListener('dragenter', (e) => {
      if (!dndDragActive) return;
      const summary = e.target.closest(DND_TARGET_SEL);
      if (!summary) return;
      // No highlight for the inert no-ops — mirror the dragover guard.
      if ((summary.hasAttribute('data-dnd-target-ungrouped') && dndSourceUngrouped) ||
          (summary.hasAttribute('data-dnd-target-retired') && (dndSourceRetired || dndSourceConversation))) return;
      summary.classList.add('dnd-drop-over');
    });
    document.addEventListener('dragleave', (e) => {
      const summary = e.target.closest(DND_TARGET_SEL);
      if (!summary) return;
      // dragleave fires when the cursor enters a child element too;
      // only remove the highlight when the cursor has actually left
      // the summary.
      if (summary.contains(e.relatedTarget)) return;
      summary.classList.remove('dnd-drop-over', 'dnd-effect-clone');
    });
    document.addEventListener('drop', async (e) => {
      const summary = e.target.closest(DND_TARGET_SEL);
      if (!summary) return;
      e.preventDefault();
      summary.classList.remove('dnd-drop-over', 'dnd-effect-clone');
      $('#dnd-pill').classList.remove('show', 'clone');
      const raw = e.dataTransfer.getData('application/x-tclaude-member')
        || e.dataTransfer.getData('text/plain');
      let payload;
      try { payload = JSON.parse(raw); } catch (_) { return; }
      if (!payload || !payload.conv) return;
      const targetUngrouped = summary.hasAttribute('data-dnd-target-ungrouped');
      const targetRetired = summary.hasAttribute('data-dnd-target-retired');
      const targetGroup = summary.getAttribute('data-dnd-target-group');
      const sourceUngrouped = !!payload.sourceUngrouped;
      const sourceConversation = !!payload.sourceConversation;
      const sourceRetired = !!payload.sourceRetired;
      // Clone applies only to a real-group target, never to a retired
      // source (that path reinstates).
      const isClone = (!!e.ctrlKey || !!e.metaKey) && !targetUngrouped && !targetRetired && !sourceRetired;

      // Confirmation gate. Each runDnd* function below opens its own
      // tailored confirmation modal as its first step, BEFORE any
      // daemon call or optimistic snapshot mutation. The no-op short-
      // circuits above have already returned, so a modal is only ever
      // shown for a gesture that would really change something — an
      // inert drop never reaches a runDnd* function and never prompts.
      // On Cancel / Escape / outside-click the runDnd* function calls
      // refresh() (the modal suspended auto-refresh while it was open)
      // and returns without touching the daemon or lastSnapshot.
      // runDndRetire uses the richer retireConfirm modal — shutdown
      // checkbox and all — so a retire-by-drag and the per-row retire
      // button ask the identical question.

      // Target = the virtual Retired group → retire the agent,
      // demoting it back to a plain conversation.
      if (targetRetired) {
        if (sourceRetired || sourceConversation) return; // no-op (see dragover)
        await runDndRetire(payload);
        return;
      }
      // Target = the virtual Ungrouped group.
      if (targetUngrouped) {
        if (sourceUngrouped) return; // already ungrouped — no-op
        if (sourceRetired) {
          // A retired agent dropped here → reinstate to an active
          // agent, joining no group.
          await runDndReinstate(payload, null);
          return;
        }
        if (sourceConversation) {
          // A conversation dropped here → promote to agent, no group.
          await runDndPromoteToUngrouped(payload);
          return;
        }
        // A real-group member → remove from that group.
        await runDndRemoveFromGroup(payload);
        return;
      }
      // Target = a real group.
      if (sourceRetired) {
        // A retired agent dragged onto a group → reinstate + join.
        await runDndReinstate(payload, targetGroup);
        return;
      }
      if (isClone) {
        // Clone forks a sibling into the target group. Works whether
        // the source is grouped or ungrouped — runDndClone clones the
        // conv then POSTs the clone into the drop-target group.
        await runDndClone(payload, targetGroup);
        return;
      }
      if (sourceUngrouped || sourceConversation) {
        // An ungrouped agent OR a conversation dragged onto a group →
        // pure add. The membership write promotes a conversation.
        await runDndAddToGroup(payload, targetGroup);
        return;
      }
      // Real group → real group move. Move-onto-self is a no-op.
      if (payload.sourceGroup === targetGroup) return;
      await runDndMove(payload, targetGroup);
    });
  }

  // runDndClone forks the source conv via POST /api/agents/{conv}/clone,
  // then adds the new conv to the target group with POST
  // /api/groups/{target}/members. The clone inherits all source
  // memberships (including the source group) — the target-group POST
  // is the differentiator: it ensures the clone is in the dropped-on
  // group even when source wasn't already there.
  //
  // No optimistic UI: the new conv-id isn't known until the response
  // lands, and inventing a placeholder row would confuse the user
  // when the real conv-id replaces it on the next poll. Just await
  // both calls and refresh.
  async function runDndClone(payload, targetGroup) {
    const {conv, label} = payload;
    const confirmed = await confirmModal({
      title: 'Clone agent into group?',
      body: `Fork a new sibling agent from "${label}" and add the clone to `
        + `group "${targetGroup}". The original keeps running; the clone is a `
        + `sibling conversation that inherits the original's identity and a `
        + `copy of its conversation history.`,
      meta: label,
      okLabel: 'Clone',
    });
    if (!confirmed) { await refresh(); return; }
    try {
      const cloneRes = await fetch(`/api/agents/${encodeURIComponent(conv)}/clone`, {
        method: 'POST', credentials: 'same-origin',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({}),
      });
      if (!cloneRes.ok) {
        toast(`clone failed: ${await cloneRes.text()}`, true);
        return;
      }
      const out = await cloneRes.json();
      const newConv = out.new_conv;
      if (!newConv) {
        toast(`clone: response missing new_conv`, true);
        return;
      }
      // Add the new conv to the drop target group. Idempotent if the
      // clone already inherited that group from the source's
      // memberships.
      const addRes = await fetch(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
        method: 'POST', credentials: 'same-origin',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({conv: newConv}),
      });
      if (!addRes.ok) {
        toast(`clone add-to-${targetGroup} failed: ${await addRes.text()}`, true);
        return;
      }
      toast(`cloned ${label} → ${targetGroup} (new ${newConv.slice(0,8)})`);
    } catch (err) {
      toast(`clone failed: ${err && err.message || err}`, true);
    } finally {
      // The confirm modal suspended auto-refresh while it was open, and
      // the dragend-fired refresh() bailed for the same reason — so the
      // dashboard has not re-rendered since before the drag. Sync now.
      await refresh();
    }
  }

  // runDndMove performs the optimistic local mutation, then the
  // POST B → DELETE A sequence. Failure of either step rolls back
  // the local mutation and surfaces a toast.
  async function runDndMove(payload, targetGroup) {
    const {conv, sourceGroup, label} = payload;
    // Confirm BEFORE the lastSnapshot read + optimistic splice below,
    // so a cancelled move leaves the snapshot — and the render —
    // completely untouched.
    const confirmed = await confirmModal({
      title: 'Move agent to another group?',
      body: `Move "${label}" out of group "${sourceGroup}" and into group `
        + `"${targetGroup}". Its membership of "${sourceGroup}" is removed and a `
        + `membership of "${targetGroup}" is added.`,
      meta: label,
      okLabel: 'Move',
    });
    if (!confirmed) { await refresh(); return; }
    // Every post-confirm exit — a guard-clause return, the partial-
    // failure return, success, or an error — funnels through the
    // finally so the dashboard re-syncs. The dragend-fired refresh()
    // bailed while the confirm modal was open (refreshSuspended() saw
    // it), so without this a confirmed-then-aborted move would leave
    // the dashboard showing stale state until the next 5s tick.
    try {
      if (!lastSnapshot || !Array.isArray(lastSnapshot.groups)) {
        toast(`move: dashboard snapshot not loaded`, true);
        return;
      }
      // Snapshot the source row so we can restore it on rollback +
      // append it to the target so the optimistic render is correct.
      const source = lastSnapshot.groups.find(g => g.name === sourceGroup);
      const target = lastSnapshot.groups.find(g => g.name === targetGroup);
      if (!source || !target) {
        toast(`move: group not found in snapshot`, true);
        return;
      }
      const idx = (source.members || []).findIndex(m => m.conv_id === conv);
      if (idx < 0) {
        toast(`move: member not found in source group`, true);
        return;
      }
      const memberSnapshot = source.members[idx];
      // Optimistic mutation: pull from source, push onto target.
      source.members.splice(idx, 1);
      target.members = target.members || [];
      target.members.push(memberSnapshot);
      renderGroupsTab();

      const rollback = () => {
        // Re-insert at the original position so the visible ordering
        // doesn't drift mid-failure.
        source.members.splice(idx, 0, memberSnapshot);
        target.members = (target.members || []).filter(m => m.conv_id !== conv);
        renderGroupsTab();
      };

      try {
        const addRes = await fetch(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
          method: 'POST', credentials: 'same-origin',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({conv}),
        });
        if (!addRes.ok) {
          toast(`move add failed: ${await addRes.text()}`, true);
          rollback();
          return;
        }
        const delRes = await fetch(`/api/groups/${encodeURIComponent(sourceGroup)}/members/${encodeURIComponent(conv)}`, {
          method: 'DELETE', credentials: 'same-origin',
        });
        if (!delRes.ok) {
          // Add succeeded but remove failed: the conv is now in BOTH
          // groups. Report it so the human can manually clean up; do
          // NOT roll the optimistic mutation back, because the daemon
          // really did add it to the target.
          toast(`move partial: added to ${targetGroup} but failed to remove from ${sourceGroup}: ${await delRes.text()}`, true);
          return;
        }
        toast(`moved ${label}: ${sourceGroup} → ${targetGroup}`);
      } catch (err) {
        toast(`move failed: ${err && err.message || err}`, true);
        rollback();
      }
    } finally {
      await refresh();
    }
  }

  // runDndAddToGroup handles a drag FROM the virtual Ungrouped group
  // ONTO a real group's header — the agent joins that group. Pure add:
  // POST /api/groups/{B}/members. The agent was in no group, so there
  // is nothing to remove; on success it drops out of the Ungrouped
  // virtual group on the next snapshot.
  //
  // Non-optimistic (one round-trip, then refresh): the source isn't a
  // real group in lastSnapshot.groups, so the optimistic splice
  // runDndMove relies on doesn't apply. A single fast call + refresh
  // keeps the code simple and the failure mode obvious.
  async function runDndAddToGroup(payload, targetGroup) {
    const {conv, label} = payload;
    // The source is either an ungrouped agent or a plain conversation;
    // for a conversation the membership write also promotes it to an
    // agent, so the modal says so.
    const isConv = !!payload.sourceConversation;
    const confirmed = await confirmModal({
      title: isConv ? 'Promote conversation into group?' : 'Add agent to group?',
      body: isConv
        ? `Promote the conversation "${label}" to an agent and add it to group `
          + `"${targetGroup}".`
        : `Add the agent "${label}" to group "${targetGroup}". It keeps every `
          + `other group it already belongs to.`,
      meta: label,
      okLabel: isConv ? 'Promote' : 'Add',
    });
    if (!confirmed) { await refresh(); return; }
    try {
      const r = await fetch(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
        method: 'POST', credentials: 'same-origin',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({conv}),
      });
      if (!r.ok) {
        toast(`add to ${targetGroup} failed: ${await r.text()}`, true);
        return;
      }
      toast(`added ${label} → ${targetGroup}`);
    } catch (err) {
      toast(`add failed: ${err && err.message || err}`, true);
    } finally {
      // The dragend handler's refresh() can race ahead of this
      // round-trip; refresh again once it has landed so the final
      // render reflects the mutation.
      await refresh();
    }
  }

  // runDndRemoveFromGroup handles a drag FROM a real group's member
  // row ONTO the virtual Ungrouped group — the agent leaves that
  // group. Pure remove: DELETE /api/groups/{A}/members/{conv}. If A
  // was the agent's only group it reappears in the Ungrouped virtual
  // group on the next snapshot; if it was in other groups too it
  // simply stays in those. Non-optimistic, same rationale as
  // runDndAddToGroup.
  async function runDndRemoveFromGroup(payload) {
    const {conv, sourceGroup, label} = payload;
    if (!sourceGroup) return; // not a real-group member — nothing to do
    const confirmed = await confirmModal({
      title: 'Remove agent from group?',
      body: `Remove "${label}" from group "${sourceGroup}". If this is its only `
        + `group it becomes an ungrouped agent; otherwise it stays in its other `
        + `groups.`,
      meta: label,
      okLabel: 'Remove',
    });
    if (!confirmed) { await refresh(); return; }
    try {
      const r = await fetch(`/api/groups/${encodeURIComponent(sourceGroup)}/members/${encodeURIComponent(conv)}`, {
        method: 'DELETE', credentials: 'same-origin',
      });
      if (!r.ok) {
        toast(`remove from ${sourceGroup} failed: ${await r.text()}`, true);
        return;
      }
      toast(`removed ${label} from ${sourceGroup}`);
    } catch (err) {
      toast(`remove failed: ${err && err.message || err}`, true);
    } finally {
      await refresh();
    }
  }

  // runDndRetire handles a drag of an AGENT row (a real-group member or
  // a virtual-Ungrouped row) ONTO the virtual Retired group — the agent
  // is retired, demoting it back to a plain conversation. Retire
  // revokes group memberships + grants, so it gets the same
  // retireConfirm modal — checkbox and all — as the per-row retire
  // button.
  async function runDndRetire(payload) {
    const {conv, label} = payload;
    const choice = await retireConfirm({ label });
    if (!choice) {
      await refresh(); // undo the optimistic dragend state
      return;
    }
    try {
      const q = choice.shutdown ? '?shutdown=1' : '?shutdown=0';
      const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/retire${q}`, {
        method: 'POST', credentials: 'same-origin',
      });
      if (!r.ok) {
        toast(`retire ${label} failed: ${await r.text()}`, true);
        return;
      }
      toast(choice.shutdown
        ? `retired ${label} — demoted + session stopped`
        : `retired ${label} — demoted to a conversation`);
    } catch (err) {
      toast(`retire failed: ${err && err.message || err}`, true);
    } finally {
      await refresh();
    }
  }

  // runDndPromoteToUngrouped handles a drag of a CONVERSATION row ONTO
  // the virtual Ungrouped group — the conversation is promoted to an
  // agent but joins no group, so it lands directly in the Ungrouped
  // virtual group on the next snapshot.
  async function runDndPromoteToUngrouped(payload) {
    const {conv, label} = payload;
    const confirmed = await confirmModal({
      title: 'Promote conversation to an agent?',
      body: `Promote the conversation "${label}" to an agent. It joins no group `
        + `and appears under Ungrouped.`,
      meta: label,
      okLabel: 'Promote',
    });
    if (!confirmed) { await refresh(); return; }
    try {
      const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/promote`, {
        method: 'POST', credentials: 'same-origin',
      });
      if (!r.ok) {
        toast(`promote ${label} failed: ${await r.text()}`, true);
        return;
      }
      toast(`promoted ${label} → agent (no group)`);
    } catch (err) {
      toast(`promote failed: ${err && err.message || err}`, true);
    } finally {
      await refresh();
    }
  }

  // runDndReinstate handles a drag of a RETIRED agent row OUT of the
  // virtual Retired group. The agent is reinstated — its retired flag
  // is cleared, making it an active agent again. Retire stripped the
  // agent's old group memberships and grants and reinstate does not
  // restore them, so the agent starts fresh: when targetGroup is given
  // (dropped onto a real group) it is then added to that group; when
  // null (dropped onto Ungrouped) it joins no group and lands in the
  // Ungrouped virtual group on the next snapshot.
  async function runDndReinstate(payload, targetGroup) {
    const {conv, label} = payload;
    const confirmed = await confirmModal({
      title: 'Reinstate retired agent?',
      body: targetGroup
        ? `Reinstate the retired agent "${label}" and add it to group `
          + `"${targetGroup}". Group memberships and permission grants stripped `
          + `when it was retired are NOT restored — it starts fresh.`
        : `Reinstate the retired agent "${label}" as an active, ungrouped agent. `
          + `Group memberships and permission grants stripped when it was retired `
          + `are NOT restored — it starts fresh.`,
      meta: label,
      okLabel: 'Reinstate',
    });
    if (!confirmed) { await refresh(); return; }
    try {
      const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/reinstate`, {
        method: 'POST', credentials: 'same-origin',
      });
      if (!r.ok) {
        toast(`reinstate ${label} failed: ${await r.text()}`, true);
        return;
      }
      if (targetGroup) {
        const addRes = await fetch(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
          method: 'POST', credentials: 'same-origin',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({conv}),
        });
        if (!addRes.ok) {
          toast(`reinstated ${label}, but join ${targetGroup} failed: ${await addRes.text()}`, true);
          return;
        }
        toast(`reinstated ${label} → ${targetGroup}`);
      } else {
        toast(`reinstated ${label} → agent (no group)`);
      }
    } catch (err) {
      toast(`reinstate failed: ${err && err.message || err}`, true);
    } finally {
      await refresh();
    }
  }

  // ===================================================================
  // Config tab — visual editor for ~/.tclaude/config.json
  //
  // The form binds to a deep clone of the loaded config, so a Config
  // field with no dedicated widget still round-trips. A JSON key absent
  // from tclaude's config schema is dropped by the server's typed
  // decode (pre-existing config.Load behaviour) — the server reports
  // such keys and the tab shows a warning on load. Save is two-phase: a
  // dry-run POST validates server-side and returns the canonical
  // "after" JSON, diffed against the on-disk baseline and shown in a
  // confirmation modal before the real write. The POST carries that
  // baseline so the server can 409 if the file drifted underneath.
  // ===================================================================
  let configObj = null;     // last loaded full config object (clone source)
  let configBaseRaw = '';   // canonical JSON of the config currently on disk
  let configLoaded = false;
  let configFileMalformed = false; // on-disk file is corrupt → form shows defaults

  function cfgInt(id, fallback) {
    const v = parseInt($('#' + id).value, 10);
    return Number.isFinite(v) ? v : fallback;
  }
  function cfgFloat(id, fallback) {
    const v = parseFloat($('#' + id).value);
    return Number.isFinite(v) ? v : fallback;
  }

  // cfgStringRow / cfgTransitionRow build one removable row of a list
  // editor. renderCfg*List (re)populates a container with rows + an
  // "+ add" button; readCfg*List collects the non-blank values back.
  function cfgStringRow(value, datalistId, placeholder) {
    const row = document.createElement('div');
    row.className = 'cfg-list-row';
    const inp = document.createElement('input');
    inp.type = 'text';
    inp.value = value || '';
    inp.placeholder = placeholder || '';
    if (placeholder) inp.setAttribute('aria-label', placeholder);
    inp.autocomplete = 'off';
    inp.spellcheck = false;
    if (datalistId) inp.setAttribute('list', datalistId);
    const del = document.createElement('button');
    del.type = 'button';
    del.className = 'cfg-row-del';
    del.textContent = '×';
    del.title = 'Remove';
    del.addEventListener('click', () => row.remove());
    row.appendChild(inp);
    row.appendChild(del);
    return row;
  }
  function cfgTransitionRow(from, to) {
    const row = document.createElement('div');
    row.className = 'cfg-list-row';
    const mk = (val, ph, role) => {
      const i = document.createElement('input');
      i.type = 'text';
      i.value = val || '';
      i.placeholder = ph;
      i.setAttribute('aria-label', ph);
      i.autocomplete = 'off';
      i.spellcheck = false;
      i.dataset.role = role;
      i.setAttribute('list', 'cfg-state-list');
      return i;
    };
    const arrow = document.createElement('span');
    arrow.className = 'cfg-arrow';
    arrow.textContent = '→';
    const del = document.createElement('button');
    del.type = 'button';
    del.className = 'cfg-row-del';
    del.textContent = '×';
    del.title = 'Remove';
    del.addEventListener('click', () => row.remove());
    row.appendChild(mk(from, 'from state', 'from'));
    row.appendChild(arrow);
    row.appendChild(mk(to, 'to state', 'to'));
    row.appendChild(del);
    return row;
  }
  function renderCfgStringList(containerId, values, datalistId, placeholder) {
    const c = $('#' + containerId);
    c.innerHTML = '';
    (values || []).forEach(v => c.appendChild(cfgStringRow(v, datalistId, placeholder)));
    const add = document.createElement('button');
    add.type = 'button';
    add.className = 'cfg-list-add';
    add.textContent = '+ add';
    add.addEventListener('click', () => {
      const row = cfgStringRow('', datalistId, placeholder);
      c.insertBefore(row, add);
      row.querySelector('input').focus();
    });
    c.appendChild(add);
  }
  function renderCfgTransitionList(values) {
    const c = $('#cfg-notif-transitions');
    c.innerHTML = '';
    (values || []).forEach(t => c.appendChild(cfgTransitionRow(t.from, t.to)));
    const add = document.createElement('button');
    add.type = 'button';
    add.className = 'cfg-list-add';
    add.textContent = '+ add transition';
    add.addEventListener('click', () => {
      const row = cfgTransitionRow('', '');
      c.insertBefore(row, add);
      row.querySelector('input').focus();
    });
    c.appendChild(add);
  }
  function readCfgStringList(containerId) {
    return $$('#' + containerId + ' .cfg-list-row input')
      .map(i => i.value.trim()).filter(Boolean);
  }
  function readCfgTransitionList() {
    const out = [];
    $$('#cfg-notif-transitions .cfg-list-row').forEach(row => {
      const from = row.querySelector('input[data-role=from]').value.trim();
      const to = row.querySelector('input[data-role=to]').value.trim();
      // A row with either side filled is kept (a half-filled row then
      // surfaces as a server validation error rather than silently
      // vanishing); a fully blank row is dropped.
      if (from || to) out.push({ from, to });
    });
    return out;
  }

  // syncCfgEnables greys out the companion inputs of any unchecked
  // enable toggle, so the form reads the way it behaves.
  function syncCfgEnables() {
    $('#cfg-autocompact-pct').disabled = !$('#cfg-autocompact-enabled').checked;
    const rl = $('#cfg-ratelimit-enabled').checked;
    $('#cfg-ratelimit-5h').disabled = !rl;
    $('#cfg-ratelimit-7d').disabled = !rl;
    $('#cfg-agent-spawnmax').disabled = !$('#cfg-agent-spawnmax-enabled').checked;
    const nudge = $('#cfg-nudge-enabled').checked;
    $('#cfg-nudge-min').disabled = !nudge;
    $('#cfg-nudge-interval').disabled = !nudge;
  }

  function populateConfigForm(cfg) {
    cfg = cfg || {};
    $('#cfg-log-level').value = cfg.log_level || 'info';
    $('#cfg-terminal').value = cfg.terminal || '';
    const acp = cfg.auto_compact_percent;
    $('#cfg-autocompact-enabled').checked = acp != null;
    $('#cfg-autocompact-pct').value = acp != null ? acp : '';
    $('#cfg-record-hooks').checked = !!cfg.record_hooks;

    const n = cfg.notifications || {};
    $('#cfg-notif-enabled').checked = !!n.enabled;
    $('#cfg-notif-cooldown').value = n.cooldown_seconds != null ? n.cooldown_seconds : '';
    renderCfgTransitionList(n.transitions || []);
    renderCfgStringList('cfg-notif-command', n.notification_command || [], null, 'argument');

    const rl = cfg.ratelimit;
    $('#cfg-ratelimit-enabled').checked = !!rl;
    $('#cfg-ratelimit-5h').value = rl ? rl.five_hour_percent_max_used : '';
    $('#cfg-ratelimit-7d').value = rl ? rl.seven_day_percent_max_used : '';

    const a = cfg.agent || {};
    $('#cfg-agent-autolaunch').checked = !!a.auto_launch_dashboard;
    $('#cfg-agent-clonecooldown').value = a.clone_cooldown || '';
    // nil / true both mean "on" (the default); only an explicit false is off.
    $('#cfg-agent-spawnrestrict').checked = a.spawn_group_restriction !== false;
    const smph = a.spawn_max_per_hour;
    $('#cfg-agent-spawnmax-enabled').checked = smph != null;
    $('#cfg-agent-spawnmax').value = smph != null ? smph : '';
    const cn = a.context_nudge || {};
    $('#cfg-nudge-enabled').checked = !!cn.enabled;
    // != null (not ||) so a stored 0 shows as 0, not blank — a 0 ladder
    // value while the nudge is enabled is a config Validate flags.
    $('#cfg-nudge-min').value = cn.min_pct != null ? cn.min_pct : '';
    $('#cfg-nudge-interval').value = cn.interval_pct != null ? cn.interval_pct : '';
    renderCfgStringList('cfg-agent-permissions', a.default_permissions || [], 'cfg-slug-list', 'permission slug');
    renderCfgStringList('cfg-agent-allowedgroups', a.spawn_allowed_groups || [], 'cfg-group-list', 'group name');

    $('#cfg-sudo-json').value = a.sudo ? JSON.stringify(a.sudo, null, 2) : '';
    syncCfgEnables();
  }

  // assembleConfig builds the config object to submit. It starts from a
  // deep clone of the loaded config so Config fields with no dedicated
  // widget still round-trip, then the form widgets overwrite the paths
  // they own. Throws on unparseable advanced sudo JSON — the caller
  // surfaces that as a save error.
  function assembleConfig() {
    const cfg = JSON.parse(JSON.stringify(configObj || {}));

    cfg.log_level = $('#cfg-log-level').value;
    const term = $('#cfg-terminal').value.trim();
    if (term) cfg.terminal = term; else delete cfg.terminal;
    if ($('#cfg-autocompact-enabled').checked) cfg.auto_compact_percent = cfgInt('cfg-autocompact-pct', 80);
    else delete cfg.auto_compact_percent;
    cfg.record_hooks = $('#cfg-record-hooks').checked;

    const n = (cfg.notifications && typeof cfg.notifications === 'object') ? cfg.notifications : {};
    n.enabled = $('#cfg-notif-enabled').checked;
    n.cooldown_seconds = cfgInt('cfg-notif-cooldown', 5);
    const trans = readCfgTransitionList();
    if (trans.length) n.transitions = trans; else delete n.transitions;
    const cmd = readCfgStringList('cfg-notif-command');
    if (cmd.length) n.notification_command = cmd; else delete n.notification_command;
    cfg.notifications = n;

    if ($('#cfg-ratelimit-enabled').checked) {
      // Clone the existing block rather than build a fresh one, so a
      // future ratelimit sub-field with no widget still round-trips.
      const rl = (cfg.ratelimit && typeof cfg.ratelimit === 'object') ? cfg.ratelimit : {};
      rl.five_hour_percent_max_used = cfgFloat('cfg-ratelimit-5h', 99);
      rl.seven_day_percent_max_used = cfgFloat('cfg-ratelimit-7d', 99.9);
      cfg.ratelimit = rl;
    } else {
      // The whole section is switched off — the human chose to drop it.
      delete cfg.ratelimit;
    }

    const a = (cfg.agent && typeof cfg.agent === 'object') ? cfg.agent : {};
    // Set optional keys only when meaningful so an all-default agent
    // block stays genuinely empty (see the empty-agent drop below).
    if ($('#cfg-agent-autolaunch').checked) a.auto_launch_dashboard = true;
    else delete a.auto_launch_dashboard;
    const cc = $('#cfg-agent-clonecooldown').value.trim();
    if (cc) a.clone_cooldown = cc; else delete a.clone_cooldown;
    // Checked = "on" = also the default (nil): preserve an existing nil
    // or true rather than introducing a redundant explicit `true`.
    if ($('#cfg-agent-spawnrestrict').checked) {
      if (a.spawn_group_restriction === false) delete a.spawn_group_restriction;
    } else {
      a.spawn_group_restriction = false;
    }
    if ($('#cfg-agent-spawnmax-enabled').checked) a.spawn_max_per_hour = cfgInt('cfg-agent-spawnmax', 10);
    else delete a.spawn_max_per_hour;
    // Clone the existing context_nudge block so a future sub-field with
    // no widget round-trips, then set the ladder from the form.
    const cn = (a.context_nudge && typeof a.context_nudge === 'object') ? a.context_nudge : {};
    if ($('#cfg-nudge-enabled').checked) {
      cn.enabled = true;
      cn.min_pct = cfgInt('cfg-nudge-min', 30);
      cn.interval_pct = cfgInt('cfg-nudge-interval', 10);
      a.context_nudge = cn;
    } else {
      // Disabled: drop the enabled flag (false is the omitempty default)
      // but keep the ladder values the user entered so toggling off then
      // on round-trips. Drop the block only when nothing is left to keep.
      delete cn.enabled;
      const minRaw = $('#cfg-nudge-min').value.trim();
      const ivRaw = $('#cfg-nudge-interval').value.trim();
      if (minRaw) cn.min_pct = cfgInt('cfg-nudge-min', 0); else delete cn.min_pct;
      if (ivRaw) cn.interval_pct = cfgInt('cfg-nudge-interval', 0); else delete cn.interval_pct;
      if (Object.keys(cn).length) a.context_nudge = cn;
      else delete a.context_nudge;
    }
    const perms = readCfgStringList('cfg-agent-permissions');
    if (perms.length) a.default_permissions = perms; else delete a.default_permissions;
    const grps = readCfgStringList('cfg-agent-allowedgroups');
    if (grps.length) a.spawn_allowed_groups = grps; else delete a.spawn_allowed_groups;

    const sudoRaw = $('#cfg-sudo-json').value.trim();
    if (sudoRaw) {
      let parsed;
      try {
        parsed = JSON.parse(sudoRaw);
      } catch (e) {
        throw new Error('Advanced sudo JSON is not valid JSON: ' + (e.message || e));
      }
      if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
        throw new Error('Advanced sudo JSON must be a JSON object (or left blank).');
      }
      a.sudo = parsed;
    } else {
      delete a.sudo;
    }
    // An all-default agent block marshals to "agent": {} on the server,
    // which would show as a spurious diff against a config that simply
    // had no agent key. Drop it when nothing is set.
    if (Object.keys(a).length) cfg.agent = a;
    else delete cfg.agent;
    return cfg;
  }

  function clearConfigErrors() {
    const el = $('#cfg-errors');
    el.style.display = 'none';
    el.innerHTML = '';
  }
  function showConfigErrors(errs) {
    const el = $('#cfg-errors');
    el.innerHTML = '<strong>Cannot save — fix these first:</strong><ul>' +
      errs.map(e => `<li>${esc(e)}</li>`).join('') + '</ul>';
    el.style.display = 'block';
    el.scrollIntoView({ block: 'nearest' });
  }

  // The notice box (amber) carries load-time facts about the file the
  // form cannot represent: a malformed file shown as defaults, or
  // keys the running tclaude does not model and a save would drop.
  function renderConfigNotice(messages) {
    const el = $('#cfg-notice');
    if (!messages.length) {
      el.style.display = 'none';
      el.innerHTML = '';
      return;
    }
    el.innerHTML = '<strong>Heads up:</strong><ul>' +
      messages.map(m => `<li>${esc(m)}</li>`).join('') + '</ul>';
    el.style.display = 'block';
  }

  async function loadConfigTab() {
    // Refresh the slug / group datalists from the latest snapshot.
    const snap = lastSnapshot || {};
    $('#cfg-slug-list').innerHTML = (snap.slugs || [])
      .map(s => `<option value="${esc(s.slug)}"></option>`).join('');
    $('#cfg-group-list').innerHTML = (snap.groups || [])
      .map(g => `<option value="${esc(g.name)}"></option>`).join('');
    $('#cfg-status').textContent = 'loading…';
    clearConfigErrors();
    renderConfigNotice([]);
    try {
      const r = await fetch('/api/config', { credentials: 'same-origin' });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const data = await r.json();
      configBaseRaw = data.raw || '{}';
      configObj = JSON.parse(configBaseRaw);
      configFileMalformed = !!data.malformed;
      if (data.path) $('#cfg-path').textContent = data.path;
      populateConfigForm(configObj);
      configLoaded = true;
      const notices = [];
      if (data.warning) notices.push(data.warning);
      if (data.unknown_keys && data.unknown_keys.length) {
        notices.push('config.json also contains key(s) this version of tclaude does not ' +
          'model: ' + data.unknown_keys.join(', ') + '. They are not shown here, and ' +
          'saving from this tab will remove them.');
      }
      renderConfigNotice(notices);
      $('#cfg-status').textContent = notices.length
        ? 'loaded with a notice — see above'
        : 'loaded — edits stay in this form until you Save';
    } catch (e) {
      configLoaded = false;
      $('#cfg-status').textContent = 'failed to load';
      showConfigErrors(['Could not load config: ' + (e.message || e)]);
    }
  }

  // cfgLineDiff returns an LCS-based line diff of two strings. Config
  // JSON is tiny (tens of lines) so the O(n·m) table is trivial.
  function cfgLineDiff(aStr, bStr) {
    const a = aStr.split('\n'), b = bStr.split('\n');
    const n = a.length, m = b.length;
    const dp = [];
    for (let i = 0; i <= n; i++) dp.push(new Array(m + 1).fill(0));
    for (let i = n - 1; i >= 0; i--) {
      for (let j = m - 1; j >= 0; j--) {
        dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
      }
    }
    const out = [];
    let i = 0, j = 0;
    while (i < n && j < m) {
      if (a[i] === b[j]) { out.push({ t: 'ctx', s: a[i] }); i++; j++; }
      else if (dp[i + 1][j] >= dp[i][j + 1]) { out.push({ t: 'del', s: a[i] }); i++; }
      else { out.push({ t: 'add', s: b[j] }); j++; }
    }
    while (i < n) out.push({ t: 'del', s: a[i++] });
    while (j < m) out.push({ t: 'add', s: b[j++] });
    return out;
  }

  // configDiffModal renders the before/after diff and resolves true on
  // confirm, false on cancel / outside-click / Escape. When malformed
  // is set the on-disk file is corrupt: a red banner spells out that
  // the whole file is being replaced and the diff is only against
  // defaults, so the human cannot wipe a corrupt config unawares.
  function configDiffModal(beforeRaw, afterRaw, malformed) {
    return new Promise(resolve => {
      const overlay = $('#config-diff-modal');
      const diff = cfgLineDiff(beforeRaw, afterRaw);
      const adds = diff.filter(d => d.t === 'add').length;
      const dels = diff.filter(d => d.t === 'del').length;
      const warnEl = $('#config-diff-warn');
      if (malformed) {
        warnEl.textContent = '⚠ config.json on disk is corrupt and could not be parsed. ' +
          'The form shows DEFAULT values, not your previous settings. Saving replaces the ' +
          'corrupt file entirely — anything it contained is lost. The diff below is against defaults.';
        warnEl.style.display = 'block';
      } else {
        warnEl.style.display = 'none';
      }
      $('#config-diff-confirm').textContent = malformed
        ? 'Replace corrupt config.json' : 'Save to config.json';
      $('#config-diff-sub').textContent =
        `${adds} line(s) added, ${dels} removed — writing to ${$('#cfg-path').textContent}`;
      const sign = { add: '+', del: '-', ctx: ' ' };
      $('#config-diff-body').innerHTML = diff
        .map(d => `<span class="dl ${d.t}">${esc(sign[d.t] + ' ' + d.s)}</span>`).join('');
      const okBtn = $('#config-diff-confirm');
      const cancelBtn = $('#config-diff-cancel');
      const cleanup = (result) => {
        overlay.classList.remove('show');
        okBtn.removeEventListener('click', onOk);
        cancelBtn.removeEventListener('click', onCancel);
        overlay.removeEventListener('click', onOverlay);
        document.removeEventListener('keydown', onKey);
        resolve(result);
      };
      const onOk = () => cleanup(true);
      const onCancel = () => cleanup(false);
      const onOverlay = (e) => { if (e.target === overlay) cleanup(false); };
      const onKey = (e) => { if (e.key === 'Escape') cleanup(false); };
      okBtn.addEventListener('click', onOk);
      cancelBtn.addEventListener('click', onCancel);
      overlay.addEventListener('click', onOverlay);
      document.addEventListener('keydown', onKey);
      overlay.classList.add('show');
      okBtn.focus();
    });
  }

  // reportConfigHTTPError surfaces a non-OK /api/config response and
  // returns true when it handled one — the caller then aborts. 400 is
  // the structured validation contract; 409 is the drift guard (the
  // file changed under the editor).
  async function reportConfigHTTPError(resp) {
    if (resp.status === 400) {
      const d = await resp.json().catch(() => ({}));
      showConfigErrors(d.errors && d.errors.length ? d.errors : ['Config rejected by the server.']);
      return true;
    }
    if (resp.status === 409) {
      const d = await resp.json().catch(() => ({}));
      showConfigErrors([(d.error || 'config.json changed on disk') +
        ' — press Reload to pick up the current file, then re-apply your edits.']);
      return true;
    }
    if (!resp.ok) {
      showConfigErrors(['Server error: HTTP ' + resp.status]);
      return true;
    }
    return false;
  }

  async function saveConfig() {
    if (!configLoaded) { toast('Config not loaded yet', true); return; }
    clearConfigErrors();
    let edited;
    try {
      edited = assembleConfig();
    } catch (e) {
      showConfigErrors([e.message || String(e)]);
      return;
    }
    // The body carries the edited config plus the canonical baseline
    // the form loaded — the server 409s if the file drifted since.
    const body = JSON.stringify({ config: edited, base: configBaseRaw });
    const post = (query) => fetch('/api/config' + query, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' }, body,
    });
    const saveBtn = $('#cfg-save');
    saveBtn.disabled = true;
    try {
      // Phase 1: dry-run — server validates and returns the canonical
      // "after" without writing anything.
      const dry = await post('?dry_run=1');
      if (await reportConfigHTTPError(dry)) return;
      const after = (await dry.json()).raw || '';
      // When the on-disk file is corrupt the diff baseline is "defaults",
      // so after===base can hold even though the save still meaningfully
      // replaces the corrupt file — don't skip it then.
      if (after === configBaseRaw && !configFileMalformed) { toast('No changes to save'); return; }

      // Phase 2: human confirms the diff before the real write.
      const ok = await configDiffModal(configBaseRaw, after, configFileMalformed);
      if (!ok) { toast('Save cancelled'); return; }

      // replace_malformed acknowledges wiping a corrupt on-disk file.
      const res = await post(configFileMalformed ? '?replace_malformed=1' : '');
      if (await reportConfigHTTPError(res)) return;
      const data = await res.json();
      configBaseRaw = data.raw || configBaseRaw;
      configObj = JSON.parse(configBaseRaw);
      configFileMalformed = false; // the file is canonical after a save
      populateConfigForm(configObj);
      // The saved file is canonical now — any load-time notice (a
      // malformed file, or unknown keys that this save dropped) is
      // stale, so clear it.
      renderConfigNotice([]);
      $('#cfg-status').textContent = 'saved · ' + new Date().toLocaleTimeString();
      toast('Config saved to ' + $('#cfg-path').textContent);
    } catch (e) {
      showConfigErrors(['Save failed: ' + (e.message || e)]);
    } finally {
      saveBtn.disabled = false;
    }
  }

  function bindConfigTab() {
    // Lazy-load on the first activation of the Config tab.
    const navBtn = $('nav button[data-tab="config"]');
    if (navBtn) navBtn.addEventListener('click', () => {
      if (!configLoaded) loadConfigTab();
    });
    $('#cfg-reload').addEventListener('click', loadConfigTab);
    $('#cfg-save').addEventListener('click', saveConfig);
    ['cfg-autocompact-enabled', 'cfg-ratelimit-enabled',
      'cfg-agent-spawnmax-enabled', 'cfg-nudge-enabled'].forEach(id => {
      $('#' + id).addEventListener('change', syncCfgEnables);
    });
  }

  bindTabs();
  bindCopy();
  bindDetailsPersistence();
  bindSortHeaders();
  bindRowActions();
  bindDnd();
  bindFilter('groups');
  bindFilter('templates');
  bindFilter('cron');
  bindFilter('sudo');
  bindFilter('links');
  bindFilter('messages');
  bindSudoModal();
  bindPermEditModal();
  bindCronModal();
  bindMessageModal();
  bindGroupCreateModal();
  bindTemplatesUI();
  bindGroupImportModal();
  bindGroupContextModal();
  bindLinkModal();
  bindAgentSpawnModal();
  bindCloneAgentModal();
  bindReincarnateAgentModal();
  bindRenameAgentModal();
  bindConfigTab();
  refresh();
  setInterval(refresh, 5000);
