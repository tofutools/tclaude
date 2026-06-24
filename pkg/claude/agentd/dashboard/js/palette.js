// palette.js — the Ctrl/Cmd-K command palette ("spotlight").
//
// A keyboard-first overlay that searches across the dashboard's
// EXISTING operations and runs the picked one on Enter. It is a thin
// SURFACE, not new behaviour: every command it lists delegates to a
// function or endpoint that already exists —
//
//   - navigation        → clicks the matching <nav> button, reusing
//                          bindTabs() plus each tab's own load-on-click
//                          handler (audit/costs/messages fetch on click)
//   - window focus/hide  → POST /api/agent-windows (bulk, all or one
//                          group) or POST /api/jump|/api/hide (per
//                          agent) — the very calls the 🪟 windows… modal
//                          and the per-row eye buttons make
//   - pick-a-subset      → opens the existing window modal
//   - spawn              → opens the existing spawn modal
//
// So the palette only adds a fast keyboard entry point to the window
// hide/show + navigation the dashboard already does; it owns no agent
// state of its own and reads the live roster fresh from lastSnapshot
// each time it opens.
//
// It is a .modal-overlay so it picks up the shared backdrop AND pauses
// the 2s auto-refresh while open (refreshSuspended() keys on
// .modal-overlay.show), which keeps a re-render from yanking focus out
// of the search box mid-type.
//
// Trigger: Ctrl/Cmd-K (claimed with preventDefault; pressing it again
// closes). Esc or a backdrop click closes. ↑/↓ move the selection,
// Enter runs it, typing filters.

import { $, $$, esc } from './helpers.js';
import { lastSnapshot } from './dashboard.js';
import { toast, openWindowModal } from './refresh.js';
import { openAgentSpawnModal } from './modal-spawn.js';
import { toggleSlop, isSlopActive } from './slop.js';

const MODAL_ID = 'command-palette-modal';

// Module state for the current open. commands is the full list built at
// open time; filtered is the current query's subset; selected indexes
// into filtered. Cached element refs are filled in bindCommandPalette.
let commands = [];
let filtered = [];
let selected = 0;
let overlay = null;
let input = null;
let list = null;

function isOpen() {
  return overlay !== null && overlay.classList.contains('show');
}

// -- POST helpers — the same endpoints the windows modal / eye buttons
//    hit. Each toasts its own outcome (matching the existing modal's
//    wording) and never touches an agent process: window-only.

async function bulkWindowOp(payload, what) {
  let r;
  try {
    r = await fetch('/api/agent-windows', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
  } catch (e) {
    toast(`${what}: request failed: ${(e && e.message) || e}`, true);
    return;
  }
  if (!r.ok) { toast(`${what}: ${await r.text()}`, true); return; }
  const out = await r.json().catch(() => null);
  if (!out) { toast(`${what}: done`); return; }
  if (payload.direction === 'focus') {
    const extra = out.failed ? `, ${out.failed} failed` : '';
    toast(`${what}: ${out.focused} focused${extra}`, out.failed > 0);
  } else {
    const extra = out.failed ? `, ${out.failed} failed` : '';
    toast(`${what}: ${out.detached} detached${extra}`, out.failed > 0);
  }
}

async function jumpAgent(conv, label) {
  try {
    const r = await fetch(`/api/jump/${encodeURIComponent(conv)}`, {
      method: 'POST', credentials: 'same-origin',
    });
    if (!r.ok) { toast(`focus ${label}: ${await r.text()}`, true); return; }
    toast(`focused: ${label}`);
  } catch (e) {
    toast(`focus ${label}: ${(e && e.message) || e}`, true);
  }
}

async function hideAgent(conv, label) {
  try {
    const r = await fetch(`/api/hide/${encodeURIComponent(conv)}`, {
      method: 'POST', credentials: 'same-origin',
    });
    if (!r.ok) { toast(`hide ${label}: ${await r.text()}`, true); return; }
    const out = await r.json().catch(() => ({}));
    toast(out.detached > 0 ? `hidden: ${label}` : `already hidden: ${label}`);
  } catch (e) {
    toast(`hide ${label}: ${(e && e.message) || e}`, true);
  }
}

// -- Command list ------------------------------------------------------

// buildCommands assembles the command list from the live snapshot plus
// the current <nav>. Order is "headline first": the global hide/show
// the branch is named for, then spawn, then tab navigation, then the
// per-group and per-agent window ops. Filtering re-ranks by query, so
// this order only governs the empty-query view.
function buildCommands() {
  const snap = lastSnapshot || {};
  const cmds = [];

  // 1) Global window ops — "hide all windows" (and its inverse), plus
  //    the modal for picking an arbitrary subset.
  cmds.push({
    icon: '⏏', label: 'Unfocus all windows',
    hint: 'detach every agent terminal window (agents keep running)',
    keywords: 'hide all windows declutter detach panic minimize',
    run: () => bulkWindowOp({ direction: 'unfocus', scope: 'all' },
      'unfocus all windows'),
  });
  cmds.push({
    icon: '◎', label: 'Focus all windows',
    hint: 'raise / open a terminal window for every running agent',
    keywords: 'show all windows raise focus bring up',
    run: () => bulkWindowOp({ direction: 'focus', scope: 'all' },
      'focus all windows'),
  });
  cmds.push({
    icon: '▦', label: 'Pick windows to focus / hide…',
    hint: 'open the window modal to choose a subset',
    keywords: 'windows subset choose select modal some',
    run: () => openWindowModal('all', null),
  });

  // 2) Spawn a new agent.
  cmds.push({
    icon: '＋', label: 'Spawn agent…',
    hint: 'open the spawn dialog',
    keywords: 'new agent create spawn launch start',
    run: () => openAgentSpawnModal({}),
  });

  // 3) Theme toggle — regular ↔ slop, the only two themes today (the
  //    header 🤝/🎰 icon does the same). Labelled by the DESTINATION so
  //    it reads as an action, not a state. When more themes arrive this
  //    becomes a picker; a two-state toggle is enough for now.
  const slopOn = isSlopActive();
  cmds.push({
    icon: slopOn ? '🤝' : '🎰',
    label: slopOn ? 'Switch to regular theme' : 'Switch to slop theme',
    hint: 'toggle the dashboard theme',
    keywords: 'toggle switch theme slop regular vegas casino mode appearance',
    run: () => toggleSlop(),
  });

  // 4) Navigation — one command per VISIBLE nav button, reusing its own
  //    click handler (which also triggers each tab's data load). A
  //    CSS-hidden tab (Costs auto-hidden, Vegas off-slop) has no
  //    offsetParent, so it isn't a place the human can currently go.
  for (const btn of $$('nav button')) {
    if (btn.offsetParent === null) continue;
    // Strip a trailing badge count ("Messages3" → "Messages").
    const name = (btn.textContent || '').replace(/\s*\d+\s*$/, '').trim();
    if (!name) continue;
    cmds.push({
      icon: '⤢', label: `Go to ${name}`,
      hint: 'switch tab',
      keywords: 'tab navigate go open ' + (btn.dataset.tab || ''),
      run: () => btn.click(),
    });
  }

  // 5) Per-group window ops — only groups with at least one running
  //    member (an idle group has no window to focus or hide).
  for (const g of (snap.groups || [])) {
    const online = (g.members || []).filter(m => m.online).length;
    if (!online) continue;
    const n = `${online} window${online === 1 ? '' : 's'}`;
    cmds.push({
      icon: '⏏', label: `Hide group: ${g.name}`,
      hint: `unfocus ${n}`,
      keywords: 'hide unfocus group windows ' + g.name,
      run: () => bulkWindowOp(
        { direction: 'unfocus', scope: 'group', group: g.name },
        `hide group ${g.name}`),
    });
    cmds.push({
      icon: '◎', label: `Focus group: ${g.name}`,
      hint: `raise ${n}`,
      keywords: 'focus show group windows ' + g.name,
      run: () => bulkWindowOp(
        { direction: 'focus', scope: 'group', group: g.name },
        `focus group ${g.name}`),
    });
  }

  // 6) Per-agent window ops — RUNNING agents only.
  for (const a of (snap.agents || [])) {
    if (!a.online) continue;
    const label = a.title || (a.conv_id || '').slice(0, 8);
    cmds.push({
      icon: '◉', label: `Focus window: ${label}`,
      hint: "raise / open this agent's terminal",
      keywords: 'focus jump bring up window agent ' + label + ' ' + (a.conv_id || ''),
      run: () => jumpAgent(a.conv_id, label),
    });
    cmds.push({
      icon: '⊘', label: `Hide window: ${label}`,
      hint: "detach this agent's terminal",
      keywords: 'hide detach window agent ' + label + ' ' + (a.conv_id || ''),
      run: () => hideAgent(a.conv_id, label),
    });
  }

  return cmds;
}

// filterCommands narrows the command list to those whose searchable
// text contains every whitespace-token of the query (AND match). A
// match in the label outranks one only in the hint/keywords, so the
// most direct command floats to the top. Empty query → the whole list
// in build order.
function filterCommands(q) {
  q = q.trim().toLowerCase();
  if (!q) return commands.slice();
  const tokens = q.split(/\s+/);
  const scored = [];
  for (const cmd of commands) {
    const label = cmd.label.toLowerCase();
    const hay = (cmd.label + ' ' + (cmd.hint || '') + ' ' + (cmd.keywords || '')).toLowerCase();
    if (!tokens.every(t => hay.includes(t))) continue;
    const score = tokens.every(t => label.includes(t)) ? 2 : 1;
    scored.push({ cmd, score });
  }
  // Stable sort (modern engines) keeps build order within a score band.
  scored.sort((a, b) => b.score - a.score);
  return scored.map(s => s.cmd);
}

// -- Rendering ---------------------------------------------------------

function render(q) {
  filtered = filterCommands(q);
  if (selected >= filtered.length) selected = filtered.length - 1;
  if (selected < 0) selected = 0;
  if (!filtered.length) {
    list.innerHTML = '<div class="palette-empty">No matching commands</div>';
    return;
  }
  list.innerHTML = filtered.map((c, i) => `
    <div class="palette-item${i === selected ? ' selected' : ''}" data-idx="${i}"
         role="option" aria-selected="${i === selected ? 'true' : 'false'}">
      <span class="palette-ico">${esc(c.icon || '•')}</span>
      <span class="palette-label">${esc(c.label)}</span>
      ${c.hint ? `<span class="palette-hint">${esc(c.hint)}</span>` : ''}
    </div>`).join('');
  paintSelection();
}

// paintSelection updates the highlight + ARIA without a full re-render
// and scrolls the active row into view — used by ↑/↓ and hover.
function paintSelection() {
  const items = list.querySelectorAll('.palette-item');
  items.forEach((el, i) => {
    const on = i === selected;
    el.classList.toggle('selected', on);
    el.setAttribute('aria-selected', on ? 'true' : 'false');
    if (on) el.scrollIntoView({ block: 'nearest' });
  });
}

function move(d) {
  if (!filtered.length) return;
  selected = (selected + d + filtered.length) % filtered.length;
  paintSelection();
}

function runSelected() {
  const cmd = filtered[selected];
  if (!cmd) return;
  // Close first so a command that opens its OWN modal (windows / spawn)
  // isn't stacked under our overlay.
  closePalette();
  try {
    cmd.run();
  } catch (e) {
    toast('command failed: ' + ((e && e.message) || e), true);
  }
}

// -- Open / close ------------------------------------------------------

function openPalette() {
  commands = buildCommands();
  selected = 0;
  input.value = '';
  overlay.classList.add('show');
  render('');
  // Focus after the show so the box is laid out; select-all is moot on
  // an empty field but keeps the behaviour obvious if reopened dirty.
  input.focus();
}

function closePalette() {
  overlay.classList.remove('show');
}

// bindCommandPalette wires the global Ctrl/Cmd-K trigger and the
// in-overlay interactions. Called once from dashboard.js boot.
export function bindCommandPalette() {
  overlay = $('#' + MODAL_ID);
  input = $('#palette-input');
  list = $('#palette-list');
  // Defensive: if the markup ever goes missing, do nothing rather than
  // throw and break the rest of boot.
  if (!overlay || !input || !list) return;

  // Global trigger. e.key is layout-stable for a plain letter; lower-
  // case both so Shift+Ctrl+K (some setups) still matches. e.repeat is
  // ignored so holding the chord doesn't strobe open/close.
  document.addEventListener('keydown', (e) => {
    if (e.repeat) return;
    if (!(e.ctrlKey || e.metaKey)) return;
    if ((e.key || '').toLowerCase() !== 'k') return;
    e.preventDefault();
    if (isOpen()) closePalette();
    else openPalette();
  });

  // Typing filters; ↑/↓ move; Enter runs; Esc closes. Bound to the
  // input (which holds focus the whole time the palette is open).
  input.addEventListener('input', () => { selected = 0; render(input.value); });
  input.addEventListener('keydown', (e) => {
    switch (e.key) {
      case 'ArrowDown': e.preventDefault(); move(1); break;
      case 'ArrowUp': e.preventDefault(); move(-1); break;
      case 'Enter': e.preventDefault(); runSelected(); break;
      case 'Escape': e.preventDefault(); closePalette(); break;
      default: break;
    }
  });

  // Mouse: hover selects, click runs.
  list.addEventListener('mousemove', (e) => {
    const item = e.target.closest('.palette-item');
    if (!item) return;
    const idx = Number(item.dataset.idx);
    if (idx !== selected) { selected = idx; paintSelection(); }
  });
  list.addEventListener('click', (e) => {
    const item = e.target.closest('.palette-item');
    if (!item) return;
    selected = Number(item.dataset.idx);
    runSelected();
  });

  // Backdrop click closes (a click on the box itself does not).
  overlay.addEventListener('click', (e) => {
    if (e.target === overlay) closePalette();
  });

  // The header 🔍 button is the discoverable entry point for anyone who
  // doesn't know the hotkey.
  const btn = $('#command-palette-btn');
  if (btn) btn.addEventListener('click', openPalette);
}
