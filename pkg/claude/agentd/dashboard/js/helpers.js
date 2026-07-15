// helpers.js — dashboard leaf module.
//
// DOM shortcuts ($/$$), HTML escaping (esc), relative-time and path
// formatting, and the small pure-ish cell / pill / status-dot / row-
// button builders the dashboard render code shares. Extracted verbatim
// from dashboard.js as the first step of the Stage 2 module split.
// Near-leaf: it imports the prefs store for per-group offline overrides and
// the dependency-free theme helper used by shared presentation-copy builders.
import { dashPrefs } from './prefs.js';
import { wizWord } from './slop.js';

const $ = (sel, root) => (root || document).querySelector(sel);
const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));

// isModifiedClick reports whether a click event should be left to the browser
// rather than handled in-SPA. The dashboard's nav tabs/subtabs are real
// <a href> anchors (so hovering previews the URL and Cmd/Ctrl/middle-click open
// a new tab); their click handlers call this to bail out of the in-page switch
// on a modified or non-primary click, letting the anchor's native navigation
// open the location in a new tab / window. A plain left-click returns false, so
// the handler preventDefaults and switches in place. A synthetic
// element.click() (command palette, [/] tab-cycle, deep links) reports button 0
// with no modifiers, so it too stays in-SPA — no reload.
function isModifiedClick(e) {
  return e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0;
}
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

// themeWords emits both ordinary and wizard-mode copy and lets CSS reveal the
// active voice. Unlike wizWord(), this swaps immediately when the theme flips
// without waiting for the Groups list's next snapshot render. Keep it for
// visible copy; title/aria attributes still use wizWord() at render time.
function themeWords(plain, wizard) {
  return `<span class="theme-copy-regular">${esc(plain)}</span>`
    + `<span class="theme-copy-wizard">${esc(wizard)}</span>`;
}
function shortId(id) { return (id || '').slice(0, 8); }
// shortAgentId is the narrow-table form of a stable agent_id: the `agt_` tag
// plus the first 8 hex of the suffix (12 chars) — the rotation-immune handle
// the roster + audit/sudo/cron surfaces lead with. Falls back to the conv-id
// prefix when the row carries no agent_id (a plain conversation, a group
// target, or an older daemon that didn't send the field).
function shortAgentId(agentId, convId) { return agentId ? agentId.slice(0, 12) : shortId(convId); }
// idTooltip is the hover companion to shortAgentId: it expands the truncated
// handle to its full form for a cell's title. With both a stable agent_id and
// a conv-id present it shows "<agent_id> / <conv-id>" so either identifier can
// be read/copied off the hover (the agent_id is the rotation-immune handle;
// the conv-id is what resumes/inspects the underlying conversation). Falls
// back to whichever single id exists — a plain conversation, a group target,
// or a pre-enrollment row carries only a conv-id.
function idTooltip(agentId, convId) {
  if (agentId && convId) return `${agentId} / ${convId}`;
  return agentId || convId || '';
}

// linkify turns bare http(s) URLs in a plain-text string into clickable
// <a> links and escapes everything else, so the result is safe to drop
// straight into innerHTML. Used by the Messages-tab reader so a URL an
// agent pasted into a message — e.g. a GitHub PR link — renders as a real
// link instead of dead text.
//
// XSS-safe by construction: the non-URL runs go through esc(), and each
// URL's href AND visible text are esc()'d too. (The URL pattern already
// excludes <, >, ", ' and whitespace, so a matched URL can't break out of
// the attribute; escaping is belt-and-suspenders and keeps & correct.)
// Links open in a new tab with rel="noopener noreferrer", matching the
// branch / PR link convention elsewhere in this module.
//
// Only the http:// and https:// schemes are linkified — a deliberate
// allowlist, so a "javascript:" / "data:" string can never become a live
// link, and bare words like "github.com" aren't falsely linked. Trailing
// characters that belong to the surrounding prose rather than the URL are
// peeled back out: sentence punctuation (.,;:!?'") and an unbalanced
// closing ) or ] (the markdown "(url)" wrapper). The peel loops until
// stable so any order — "url.", "url)", "url)." — is handled.
function linkify(text) {
  const s = String(text == null ? '' : text);
  const urlRe = /https?:\/\/[^\s<>"']+/g;
  let out = '';
  let last = 0;
  let m;
  while ((m = urlRe.exec(s)) !== null) {
    out += esc(s.slice(last, m.index));
    let url = m[0];
    let trail = '';
    for (;;) {
      const ch = url[url.length - 1];
      if ('.,;:!?\'"'.includes(ch)) {
        trail = ch + trail;
        url = url.slice(0, -1);
        continue;
      }
      if (ch === ')' || ch === ']') {
        const open = ch === ')' ? '(' : '[';
        const opens = url.split(open).length - 1;
        const closes = url.split(ch).length - 1;
        if (closes > opens) {
          trail = ch + trail;
          url = url.slice(0, -1);
          continue;
        }
      }
      break;
    }
    if (url) {
      out += `<a href="${esc(url)}" target="_blank" rel="noopener noreferrer">${esc(url)}</a>`;
    }
    out += esc(trail);
    last = m.index + m[0].length;
  }
  out += esc(s.slice(last));
  return out;
}

// syncSelectTitle mirrors a <select>'s currently-selected option text
// into its `title` attribute. The modal form controls shrink to the
// available width (min-width:0 in dashboard.css), so a long option label
// — e.g. a worktree's "branch — ~/long/path" — is clipped in the closed
// box; the tooltip makes the full label readable on hover without having
// to open the dropdown. Safe to call repeatedly and after every
// (re)population; a short/blank label just yields a short/blank tooltip.
function syncSelectTitle(sel) {
  if (!sel) return;
  const opt = sel.selectedOptions && sel.selectedOptions[0];
  // Prefer an option's own `title` when it carries one — the worktree
  // options set it to the full, untruncated path (their visible label
  // shortens the path), so the tooltip can show more than the box does.
  sel.title = (opt ? (opt.title || opt.textContent) : '').trim();
}

// MODEL_CUSTOM_VALUE is the sentinel <option> value the curated Model
// <select>s end with ("Custom model id…"). Picking it reveals a free-text input
// (id `${base}-custom`) so a human can type ANY model id/alias, not just the
// curated presets — the daemon validates it at spawn (ValidateModel). It is not
// a real model, so submit/seed treat it specially (see the active-model-element
// resolvers + syncCustomModelRow). Kept distinct from "" (Default/unset).
const MODEL_CUSTOM_VALUE = '__custom__';

// populateModelSelect rebuilds a curated Model <select> from the selected
// harness's snapshot catalog. The catalog is a suggestion list rather than an
// allow-list, so every harness gets the same trailing Custom model id… entry.
// Callers seed an existing model afterwards through setModelSelectValue(),
// which keeps an out-of-catalog value selectable as an exact ID.
function populateModelSelect(sel, models, defaultLabel = 'Default (unset)') {
  if (!sel) return;
  sel.replaceChildren();
  const add = (value, label) => {
    const opt = document.createElement('option');
    opt.value = value;
    opt.textContent = label;
    sel.appendChild(opt);
  };
  add('', defaultLabel);
  for (const model of (models || [])) add(model, model);
  add(MODEL_CUSTOM_VALUE, 'Custom model id…');
  sel.value = '';
  syncSelectTitle(sel);
}

// setModelSelectValue sets a model id into a Model control, the way the spawn
// dialog and profile editor seed one from a saved profile / captured live
// agent. A curated Model control is a <select> whose <option>s are a preset
// list of aliases; assigning `.value` a
// model that isn't one of those options is a silent no-op — the box just keeps
// its prior pick — so a full model id captured from a running agent (e.g.
// "claude-opus-4-8[1m]", which ValidateModel accepts but the alias list never
// contains) would be dropped. To keep it, we inject the exact id as a
// selectable option (flagged "(exact id)" so it reads as an out-of-catalog
// value, not a curated preset) before selecting it — mirroring the
// profileRef/roleRef "keep the current value selectable" pattern. The injected
// option is placed before the trailing "Custom model id…" sentinel so that
// stays last. A previously injected option is removed on each call so re-opening
// the form with a different model doesn't stack stale options. For a free-text
// <input> (a harness with no suggestion list) any value is valid, so we set it
// directly.
function setModelSelectValue(el, value) {
  if (!el) return;
  value = (value || '').trim();
  if (el.tagName === 'SELECT') {
    // Drop a stale injected option from a prior open so they never accumulate.
    for (const o of [...el.options]) {
      if (o.dataset.dynamicModel && o.value !== value) o.remove();
    }
    if (value && ![...el.options].some(o => o.value === value)) {
      const opt = document.createElement('option');
      opt.value = value;
      opt.textContent = `${value} (exact id)`;
      opt.dataset.dynamicModel = '1';
      // Keep the "Custom model id…" sentinel last (insertBefore(…, null) just
      // appends when the select carries no sentinel).
      el.insertBefore(opt, el.querySelector(`option[value="${MODEL_CUSTOM_VALUE}"]`));
    }
  }
  el.value = value;
}

// syncCustomModelRow reconciles a Model field group's free-text "Custom…" row
// with its curated <select>. The row (id `${base}-custom-row`) and its input
// (id `${base}-custom`) are shown iff the select (id `${base}`) sits on the
// MODEL_CUSTOM_VALUE sentinel; the input is cleared when hidden so a stale typed
// id can't leak into a later read. Pass {focus:true} to move the caret into the
// input the moment it appears (a human picking "Custom…"). Call after any change
// to the select — a user pick, a programmatic seed, or a harness switch.
function syncCustomModelRow(base, { focus = false } = {}) {
  const sel = $('#' + base);
  const row = $('#' + base + '-custom-row');
  const input = $('#' + base + '-custom');
  if (!sel || !row || !input) return;
  const on = sel.value === MODEL_CUSTOM_VALUE;
  row.style.display = on ? '' : 'none';
  if (!on) input.value = '';
  else if (focus) input.focus();
}

// refreshModalMinSize pins a resizable modal's minimum size to its natural
// "at rest" size — the size it renders at with no user resize: the default
// width and the content height (the latter already capped by max-height in
// CSS). That stops the resize grip from shrinking the dialog below where
// its fields fit, and it's the previous (pre-resize) default size, not a
// hardcoded number — measured live each open, so it tracks the viewport and
// the current content. The box still auto-grows above this floor to fit
// taller content (a fixed min-height isn't imposed), so this only sets the
// drag floor. No-op while the modal is hidden (it can't be measured then).
//
// Measurement drops any applied size + prior min so the box falls back to
// its content-driven natural size, reads that, pins it as the min, and
// restores the applied size — all synchronously, so the cleared state never
// paints (no flicker). box-sizing is border-box globally, so the measured
// offsetWidth/Height line up with the width/height we restore.
function refreshModalMinSize(modalEl) {
  if (!modalEl || !modalEl.offsetParent) return; // hidden → can't measure
  const { width, height } = modalEl.style;
  modalEl.style.minWidth = '';
  modalEl.style.minHeight = '';
  modalEl.style.width = '';
  modalEl.style.height = '';
  const natW = modalEl.offsetWidth;
  const natH = modalEl.offsetHeight;
  modalEl.style.minWidth = natW + 'px';
  modalEl.style.minHeight = natH + 'px';
  modalEl.style.width = width;
  modalEl.style.height = height;
}

// growModalToFitContent expands a resizable modal whose *applied* (saved or
// dragged) height has been outgrown by content that appeared after the drag
// — e.g. the spawn dialog revealing the worktree branch field on name entry,
// or the Codex Model / Sandbox / trust-dir rows on a harness switch. Without
// an applied inline height the box is content-driven and CSS already grows it
// (overflow:auto + max-height), so this is a no-op then; it only kicks in once
// a fixed height is pinned, where the extra rows would otherwise scroll inside
// the box instead of enlarging it.
//
// Grow-only — it never shrinks, so it can't undo a deliberate drag (switching
// back to a shorter layout just leaves the roomier box, footer bottom-stuck by
// the margin-top:auto rule, exactly the look #398 already settled on). The new
// height adds the chrome the content height excludes (border + any horizontal
// scrollbar, = offsetHeight − clientHeight) so the grown box exactly contains
// the content. CSS max-height:86vh still caps it: past the cap the browser
// clamps the applied height and overflow:auto restores the scrollbar.
function growModalToFitContent(modalEl) {
  if (!modalEl || !modalEl.style.height) return; // content-driven; CSS grows it
  if (modalEl.scrollHeight - modalEl.clientHeight <= 1) return; // fits already (1px rounding slack)
  modalEl.style.height = modalEl.scrollHeight + (modalEl.offsetHeight - modalEl.clientHeight) + 'px';
}

// makeModalResizable persists a CSS-`resize`-enabled modal's dragged size
// (width + height) in dashPrefs, keyed by `key`, so it survives reopen,
// daemon restart and tab — dashPrefs lives server-side, unlike
// localStorage which the random loopback port would partition away.
// `modalEl` is the element that carries `resize` in CSS (the inner card,
// not the overlay). Restores any saved size up front, then writes the new
// size only on a genuine resize: it brackets each pointer gesture
// (pointerdown→pointerup) and persists only when the box actually changed
// between them, so plain clicks and content-driven reflows (rows showing/
// hiding, error text) never get mistaken for a user resize. box-sizing is
// border-box globally, so offsetWidth/Height match the inline width/height
// we restore; CSS min/max-width + max-height still clamp the applied size.
//
// It also pins the modal's minimum size to its natural default each time it
// opens (refreshModalMinSize), so the grip can't shrink it below where the
// fields fit. The open trigger is the overlay gaining its `show` class —
// watched here so the caller needn't thread a hook through every open path.
//
// Finally it auto-grows a pinned height to fit content that appears after a
// drag (growModalToFitContent), watching the card's own subtree for the
// row-reveal mutations the spawn/clone forms make (style/class/hidden flips +
// option repopulation). Centralising it here means every resizable modal —
// and any field a future change adds — gets the behaviour without threading a
// hook through each reshape call site. It reacts ONLY to mutations of a
// *descendant*, never of the card itself: a content reveal always flips a
// descendant's style/display or repopulates a descendant <select>, whereas the
// card's own width/height changes are the user's resize drag and our own
// grow-write. Filtering those out means auto-grow never fights a deliberate
// drag-shrink and our height write can't recurse — no re-entrancy guard needed.
//
// Those last two behaviours (content-tracking min-size + auto-grow) suit FORM
// dialogs, whose whole body should stay visible. A LIST panel — the templates-
// manage overlay, whose body is a scroll region — opts out with
// `{ fitContent: false }`: content-tracking would pin the min-height at the
// max-height cap (making a long list un-shrinkable) and auto-grow would re-
// expand a deliberately-shortened box on the panel's 2s live refresh. Opting
// out keeps only the persist/restore + pointer-bracketed save; the resize floor
// is then a fixed CSS min-height on the card.
function makeModalResizable(modalEl, key, opts = {}) {
  if (!modalEl) return;
  let saved = { w: 0, h: 0 };
  try {
    const s = JSON.parse(dashPrefs.getItem(key));
    if (s && typeof s === 'object') saved = { w: +s.w || 0, h: +s.h || 0 };
  } catch (_) { /* missing / corrupt — fall back to the CSS default size */ }
  if (saved.w) modalEl.style.width = saved.w + 'px';
  if (saved.h) modalEl.style.height = saved.h + 'px';
  let downW = 0, downH = 0;
  const onPointerDown = (event) => {
    // Descendants may own independent resize handles (notably textareas).
    // Their events bubble through the card, but only a gesture that starts on
    // the card itself can be the modal's native bottom-right resize grip.
    if (event.target !== modalEl) {
      downW = 0; downH = 0;
      return;
    }
    downW = modalEl.offsetWidth; downH = modalEl.offsetHeight;
  };
  const onPointerUp = () => {
    if (!downW && !downH) return;
    const w = modalEl.offsetWidth, h = modalEl.offsetHeight;
    if (w === downW && h === downH) return;     // a click, not a resize
    if (w === saved.w && h === saved.h) return; // already the stored size
    saved = { w, h };
    try { dashPrefs.setItem(key, JSON.stringify(saved)); } catch (_) {}
  };
  modalEl.addEventListener('pointerdown', onPointerDown);
  modalEl.addEventListener('pointerup', onPointerUp);
  const observers = [];
  const cleanup = () => {
    modalEl.removeEventListener('pointerdown', onPointerDown);
    modalEl.removeEventListener('pointerup', onPointerUp);
    observers.forEach(observer => observer.disconnect());
  };
  // List panels stop here (see the header note): no content-tracking min-size,
  // no auto-grow — just the persist/restore above, with a fixed CSS floor.
  if (opts.fitContent === false) return cleanup;
  // Re-measure the min size whenever the modal becomes visible (its overlay
  // gains `show`) — content and viewport can differ per open. Observing the
  // class avoids editing every open*Modal call site, and only fires on the
  // overlay's own class changes, so there's no measure/observe feedback loop
  // (refreshModalMinSize mutates modalEl, not the overlay).
  const overlay = modalEl.closest('.modal-overlay');
  if (overlay) {
    const overlayObserver = new MutationObserver(() => {
      if (overlay.classList.contains('show')) refreshModalMinSize(modalEl);
    });
    overlayObserver.observe(overlay, { attributes: true, attributeFilter: ['class'] });
    observers.push(overlayObserver);
  }
  // Auto-grow a pinned height to fit content revealed after a drag. The
  // attributeFilter keeps this to the structural changes that move the
  // content height (row display/hidden flips), not every title/value tweak;
  // childList catches option repopulation (the worktree picker reload). The
  // descendant-only guard (target !== modalEl) skips the card's own size
  // changes — the resize drag and our grow-write — so auto-grow neither fights
  // a drag nor recurses on itself.
  const contentObserver = new MutationObserver((records) => {
    if (records.some(r => r.target !== modalEl)) growModalToFitContent(modalEl);
  });
  contentObserver.observe(modalEl, {
    childList: true, subtree: true,
    attributes: true, attributeFilter: ['style', 'class', 'hidden'],
  });
  observers.push(contentObserver);
  return cleanup;
}

// bindSelectTitles keeps every <select> under `root` tooltip-synced: an
// initial pass over the current selections plus one delegated `change`
// listener so user picks update the tooltip live. Programmatic
// repopulation (e.g. the worktree reload) doesn't fire `change`, so those
// call sites sync explicitly via syncSelectTitle. Idempotent per root via
// a data-flag so re-binding (modules can bind on open) won't stack
// listeners.
function bindSelectTitles(root) {
  if (!root) return;
  $$('select', root).forEach(syncSelectTitle);
  if (root.dataset.selectTitlesBound === '1') return;
  root.dataset.selectTitlesBound = '1';
  root.addEventListener('change', (e) => {
    if (e.target && e.target.tagName === 'SELECT') syncSelectTitle(e.target);
  });
}
// bindModalSubmitHotkey makes Ctrl/Cmd+Enter anywhere inside a modal
// fire its primary submit button — a keyboard alternative to mousing over
// to click it. Both modifiers are accepted on every platform (Cmd+Enter
// on macOS, Ctrl+Enter elsewhere), so it just works without sniffing the
// OS. It clicks the real <button>, so a disabled submit — an in-flight
// request, or a form that isn't valid yet (e.g. reincarnate force-mode
// with no follow-up) — is a no-op, the same guard the mouse path already
// respects. Plain Enter is left alone so the multi-line textareas (the
// init-message / follow-up fields) keep inserting newlines. Scoped to the
// modal element so it only fires while focus is inside the open dialog.
// Matches the existing Ctrl/Cmd+Enter convention in the edit-member modal.
function bindModalSubmitHotkey(modalEl, submitBtn) {
  if (!modalEl || !submitBtn) return;
  modalEl.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter' || !(e.ctrlKey || e.metaKey)) return;
    e.preventDefault();
    if (!submitBtn.disabled) submitBtn.click();
  });
}

// showModalError renders a modal's inline error line (a .cron-create-error
// element) as a dismissible banner: the message plus an ✕ that clears it.
// When `msg` is non-empty it also scrolls the banner into view and re-triggers
// its attention flash so it can't be missed — the dialogs that use it (spawn /
// clone / reincarnate, …) are tall and scroll inside a max-height cap, and
// Ctrl/Cmd+Enter can submit while the line sits below the fold, so a bare
// textContent write would leave a failed submit looking like nothing happened.
// An empty/falsy `msg` clears it (the .cron-create-error `:empty` rule then
// collapses the banner). Accepts an element or an id (no '#').
function showModalError(elOrId, msg) {
  const el = typeof elOrId === 'string' ? $('#' + elOrId) : elOrId;
  if (!el) return;
  if (!msg) {
    // Cleared → empty the line so `:empty` collapses it, and drop the
    // banner-mode classes so no flex/flash state lingers for the next show.
    el.textContent = '';
    el.classList.remove('flash', 'dismissible');
    return;
  }
  // Rebuilt on every show (a textContent write would wipe a prior ✕ anyway),
  // so the dismiss handler never accumulates across resubmits.
  el.textContent = '';
  const span = document.createElement('span');
  span.className = 'cron-create-error-msg';
  span.textContent = msg;
  const x = document.createElement('button');
  x.type = 'button';
  x.className = 'cron-create-error-x';
  x.setAttribute('aria-label', 'Dismiss error');
  x.title = 'Dismiss';
  x.textContent = '✕';
  // The error already self-clears on the next submit and on close/reopen; the
  // ✕ is just "I've read it, hide it now" — so it only clears the line.
  x.addEventListener('click', () => showModalError(el, ''));
  el.append(span, x);
  el.classList.add('dismissible');
  // Remove → force a reflow → re-add so an identical, resubmitted message
  // still restarts the CSS flash animation (the standard restart trick).
  el.classList.remove('flash');
  void el.offsetWidth;
  el.classList.add('flash');
  // block:'nearest' is a no-op when already visible, so this never yanks the
  // dialog around for an error the human is already looking at.
  el.scrollIntoView({ block: 'nearest' });
}
// harnessCanRename reports whether an agent on harness `name` can be
// renamed, per the snapshot's harness catalog (dashboardHarness.can_rename).
// can_rename is true whenever a rename is DELIVERABLE — by an in-pane
// command (Claude Code's /rename) OR an out-of-band ConvStore write
// (Codex's title store) — so Codex stays renameable even without a TUI
// rename command; only a harness that supports NEITHER reports false.
//
// Fail-OPEN: an unknown harness, or a snapshot whose catalog hasn't loaded
// yet, returns true so the rename affordance is never hidden on incomplete
// data. Only an explicit can_rename:false hides it.
function harnessCanRename(snapshot, name) {
  const list = (snapshot && snapshot.harnesses) || [];
  const h = list.find(x => x.name === (name || 'claude'));
  return h ? !!h.can_rename : true;
}

// harnessCanRemoteControl reports whether an agent on harness `name` can
// have its built-in Remote Access toggled, per the snapshot's harness
// catalog (dashboardHarness.can_remote_control). True for Claude Code (the
// `/remote-control` toggle), false for Codex (no Remote Access) — so the
// per-row remote-control control is hidden for a harness that has none,
// exactly as the rename control gates on can_rename (JOH-259).
//
// Fail-OPEN like harnessCanRename: an unknown harness, or a snapshot whose
// catalog hasn't loaded yet, returns true — the only harness that ever
// reports false is one the catalog explicitly marks can_remote_control:false
// (Codex). Briefly showing the control on an incomplete snapshot is
// harmless; the server still re-gates on the harness capability.
function harnessCanRemoteControl(snapshot, name) {
  const list = (snapshot && snapshot.harnesses) || [];
  const h = list.find(x => x.name === (name || 'claude'));
  return h ? !!h.can_remote_control : true;
}

const SLOP_SYMBOLS = ['🍒', '🍋', '🍇', '🍊', '🔔', '⭐', '💎', '7️⃣'];
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

// offlineDefault returns the tab-wide "show offline" checkbox state
// for the 'groups' tab. Defaults to true (show everything) when
// the checkbox isn't in the DOM yet / the user hasn't touched it.
function offlineDefault(tab) {
  const el = $(`#filter-${tab}-offline`);
  return el ? el.checked : true;
}

// groupOfflineOverride: per-group override — 'show', 'hide', or
// 'inherit' (no override; follows the tab-wide checkbox). Persisted
// in dashPrefs keyed by group name so it survives reloads and restarts.
function groupOfflineOverride(name) {
  const v = dashPrefs.getItem('tclaude.dash.group.offline.' + name);
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

let webDirectoryPickerBridge = null;

// configureDirectoryPickerBridge lets the optional Preact island claim the
// web-picker path without making this legacy helper module import Preact. A
// broken optional runtime therefore cannot stop the rest of the dashboard from
// linking; remote callers receive a visible error instead of popping a dialog
// on the daemon host that they cannot operate.
function configureDirectoryPickerBridge(bridge) {
  webDirectoryPickerBridge = bridge || null;
}

function isLoopbackDashboard(value = window.location?.hostname) {
  let hostname = String(value || '').trim().toLowerCase().replace(/\.+$/, '');
  if (hostname.startsWith('[') && hostname.endsWith(']')) hostname = hostname.slice(1, -1);
  if (!hostname || hostname === 'localhost' || hostname === '::1') return true;
  const octets = hostname.split('.');
  return octets.length === 4 && octets[0] === '127' && octets.every((octet) =>
    /^\d{1,3}$/.test(octet) && Number(octet) <= 255);
}

// pickDirectory resolves through the browser-rendered Preact picker for every
// remote dashboard connection and when local web-picker mode is configured.
// Local dashboards otherwise preserve the native host OS chooser. Returns:
//   { path }            — a directory was chosen
//   { canceled: true }  — the human dismissed the dialog (no change)
//   { error: <message> } — no picker available / busy / failed
async function pickDirectory({ startDir = '', title = 'Select a directory' } = {}) {
  const useWeb = !isLoopbackDashboard() || !!webDirectoryPickerBridge?.prefersWeb?.();
  if (useWeb) {
    if (!webDirectoryPickerBridge?.open) {
      return { error: 'web directory picker unavailable — type the host path instead' };
    }
    return webDirectoryPickerBridge.open({ startDir, title });
  }
  let r;
  try {
    r = await fetch('/api/pick-directory', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ start_dir: startDir, title }),
    });
  } catch (err) {
    return { error: (err && err.message) || String(err) };
  }
  let data = {};
  try { data = await r.json(); } catch (_) {}
  if (!r.ok) return { error: data.error || `HTTP ${r.status}` };
  if (data.canceled) return { canceled: true };
  if (data.path) return { path: data.path };
  return { canceled: true }; // empty result — treat as a no-op cancel
}

// botStampSig / orbitStampSig record the animation identity ("name period") a
// node was last wall-clock-stamped for, so the phasers below re-stamp EXACTLY
// when the animation (re)started and never otherwise. WeakMaps keyed on the live
// element, so a recreated/detached node drops out on its own (GC).
const botStampSig = new WeakMap();
const orbitStampSig = new WeakMap();

// syncBotAnimations phases an activity-bot's CSS animation to a shared wall-clock
// so bots that started at different times still animate in lock-step. Setting
// `animation-delay = -(now % period)` at the animation's start makes the
// element's displayed phase a function of wall-clock ALONE (its own start time
// cancels out: with start C and delay -(C % P), phase at time t is
// ((t - C) + (C % P)) % P = t % P), so every bot sits at phase `t % P`, together.
//
// STAMP ON (RE)START, NOT EVERY TICK. Re-stamping a node that is already running
// the same animation with a fresh `now` does NOT restart it (its start time is
// unchanged), so the new delay just shifts the phase ~(elapsed) — a visible jump
// every tick. Keyed Preact bot nodes persist across the 2s poll, so we re-stamp
// only when the animation itself changed identity — a brand-new/recreated node, or a status change that
// swaps the keyframes/period (e.g. working `actbot-dance 0.45s` → idle
// `actbot-breathe 1.7s`, which restarts the animation at a DIFFERENT period so
// the old stamp is stale). A node still running the same animation keeps its
// one-time stamp. Existing group bots keep their stamp while a transitioned or
// newly-inserted bot re-locks. The
// period is read from computed style (tracks the CSS with no duplication);
// `alternate` doubles it. Called right after each bot-bearing re-render.
function syncBotAnimations() {
  const now = (typeof performance !== 'undefined' ? performance.now() : 0);
  for (const el of $$('.actbot-face, .actbot-tag, .actbot-spr')) {
    const cs = getComputedStyle(el);
    const dur = parseFloat(cs.animationDuration) || 0; // seconds; 0 when none
    if (!dur) continue;
    const period = dur * 1000 * (cs.animationDirection.startsWith('alternate') ? 2 : 1);
    const sig = cs.animationName + ' ' + period;
    // Same animation still running + still stamped → leave it (no re-jolt).
    if (botStampSig.get(el) === sig && el.style.animationDelay) continue;
    botStampSig.set(el, sig);
    el.style.animationDelay = (-(now % period)) + 'ms';
  }
}

// syncWizardOrbit phases the wizard "Channeling" pill's orbiting mote to the
// same shared wall-clock as syncBotAnimations, on the same (re)start rule. The
// mote animates on the pill's ::before, and a pseudo-element takes no inline
// style — but it inherits custom properties from its originating element, so we
// set -(now % period) on the PILL via --wizard-orbit-delay and the ::before's
// `animation-delay: var(...)` picks it up. Phase then depends on wall-clock
// alone (see syncBotAnimations for the algebra), so every channeling pill orbits
// in lock-step. Period is read from the pseudo's computed animationDuration.
//
// We iterate EVERY pill, not just the channeling ones, so a pill that has left a
// channeling status has its stamp cleared: the orbit ::before stops matching in
// non-channeling statuses (asking / offline / non-wizard theme) → duration 0.
// Clearing then means a later RETURN to channeling (working / main_agent_idle)
// finds no stamp and re-phases from that restart, instead of resuming the mote
// at a stale orbital angle (the pill node persists across the poll — it lives in
// a keyed row — and Preact preserves the property, so without this the
// mote would freeze at the wrong angle after asking→working). A channeling pill
// still running the same orbit keeps its stamp (no per-tick re-jolt). Called
// right after each group-row re-render.
function syncWizardOrbit() {
  const now = (typeof performance !== 'undefined' ? performance.now() : 0);
  for (const pill of $$('.wizard-pill')) {
    const cs = getComputedStyle(pill, '::before');
    const dur = parseFloat(cs.animationDuration) || 0; // seconds; 0 when the ::before has no orbit
    if (!dur) {
      // Not channeling now — drop any stamp so a later return re-phases fresh.
      if (pill.style.getPropertyValue('--wizard-orbit-delay')) pill.style.removeProperty('--wizard-orbit-delay');
      orbitStampSig.delete(pill);
      continue;
    }
    const period = dur * 1000; // linear, non-alternating orbit
    const sig = cs.animationName + ' ' + period;
    if (orbitStampSig.get(pill) === sig && pill.style.getPropertyValue('--wizard-orbit-delay')) continue;
    orbitStampSig.set(pill, sig);
    pill.style.setProperty('--wizard-orbit-delay', (-(now % period)) + 'ms');
  }
}

// Public API — shared DOM, formatting, capability, picker and animation helpers.
export {
  syncBotAnimations,
  syncWizardOrbit,
  $, $$, isModifiedClick, esc, themeWords, linkify, shortId, shortAgentId, idTooltip,
  syncSelectTitle, populateModelSelect, setModelSelectValue, MODEL_CUSTOM_VALUE,
  syncCustomModelRow, bindSelectTitles, makeModalResizable, bindModalSubmitHotkey,
  showModalError,
  harnessCanRename, harnessCanRemoteControl,
  relTime, shortCwd, offlineDefault, groupOfflineOverride, groupShowOffline,
  // slop-fx.js and the native member reels share one symbol sequence.
  SLOP_SYMBOLS,
  // Shared native/web directory-picker boundary. The Preact island registers
  // its web implementation without pulling Preact into this legacy module.
  pickDirectory, configureDirectoryPickerBridge, isLoopbackDashboard,
};
