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
  // Surface the harness + model on hover (the brief's second ask). The
  // visible harness line under the controls already shows it, but the
  // dot's tooltip is the natural "what is this running on?" probe.
  const hm = harnessModel(m);
  if (hm) tip += ` · running on ${hm}`;
  let cls;
  if (errored) cls = 'status-dot status-dot-error';
  else if (online) cls = 'status-dot status-dot-online';
  else cls = 'status-dot status-dot-offline';
  const glyph = online ? '●' : '○';
  return `<button type="button" class="${cls}" data-act="dot-toggle"` +
    ` data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(label)}"` +
    ` data-online="${online ? '1' : '0'}"` +
    ` title="${esc(tip)}" aria-label="${esc(tip)}">${glyph}</button>`;
}

// The harness an agent runs under, now a real per-agent value: the
// dashboard drives more than Claude Code (Codex too, JOH-162). state.harness
// carries the tag ("claude", "codex"); empty means the default (Claude
// Code), the value a row written before the harness column existed reports.
// HARNESS_LABELS maps a known tag to its compact row chip (short) and the
// spelled-out tooltip label (long); an unknown tag falls back to its raw
// name so a future harness still shows something legible.
const HARNESS_LABELS = {
  claude: { short: 'CC', long: 'Claude Code' },
  codex: { short: 'Codex', long: 'Codex CLI' },
};

// isDefaultHarness reports whether a harness tag is the default (Claude
// Code) — '' (untagged / pre-column row) or the explicit 'claude'. Used to
// keep the common case visually quiet (no badge until a model is known)
// while a non-default harness like Codex is flagged immediately.
function isDefaultHarness(name) {
  return !name || name === 'claude';
}

// harnessLabels returns the {short, long} display labels for a harness
// tag, falling back to the default (Claude Code) for an empty tag and to
// the raw name for an unknown one.
function harnessLabels(name) {
  if (!name) return HARNESS_LABELS.claude;
  return HARNESS_LABELS[name] || { short: name, long: name };
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

// harnessModel returns "Claude Code · Opus 4.8" / "Codex CLI · gpt-5" for
// tooltips, or '' when the model isn't known yet (the statusbar hook /
// Codex telemetry hasn't ticked for this agent). The model comes from
// state.model; the harness label from state.harness. Uses the FULL model
// name (tooltips have room); shortModel() is for the visible chip.
function harnessModel(m) {
  const model = (m && m.state && m.state.model) || '';
  if (!model) return '';
  const labels = harnessLabels((m && m.state && m.state.harness) || '');
  return `${labels.long} · ${model}`;
}

// shortModel compresses a model display name for the always-visible row
// label, where horizontal space is tight. The full name stays in the
// tooltip (harnessLine's title / the status-dot tip), so nothing is lost.
//
// Rules (CC names are "<Family> <version> [(<window> context)]"):
//   1. Family → its capitalised initial, glued straight onto the version
//      with no space:  "Opus 4.8" → "O4.8", "Sonnet 4.6" → "S4.6".
//   2. A trailing "(… context)" parenthetical → just its size token
//      ("1M", "200K"), appended after a space:
//        "Opus 4.8 (1M context)" → "O4.8 1M".
//   3. Graceful degrade: a single-word name is left as-is; extra version
//      words are kept after the initial; a parenthetical with no size
//      token is dropped from the chip (it survives in the tooltip).
//
// Examples:
//   "Opus 4.8 (1M context)" → "O4.8 1M"
//   "Opus 4.8"              → "O4.8"
//   "Sonnet 4.6"            → "S4.6"
//   "Haiku 4.5"             → "H4.5"
function shortModel(model) {
  let main = (model || '').trim();
  if (!main) return '';
  // Peel off a trailing parenthetical and pull a size token out of it.
  let size = '';
  const paren = main.match(/\(([^)]*)\)\s*$/);
  if (paren) {
    main = main.slice(0, paren.index).trim();
    const m = paren[1].match(/\d+\s*[KMBkmb]/);
    if (m) size = m[0].replace(/\s+/g, '').toUpperCase();
  }
  // Family initial + version, no space between them.
  const parts = main.split(/\s+/);
  const core = parts.length >= 2
    ? parts[0].charAt(0).toUpperCase() + parts.slice(1).join(' ')
    : main;
  return size ? `${core} ${size}` : core;
}

// harnessLine renders the small muted "CC · O4.8 1M" line that sits
// UNDER the status-dot / focus / cog cluster in the same column (the
// brief's primary ask — no new table column). Returns '' when the model
// isn't known yet, so freshly-spawned / never-ticked rows stay clean
// rather than showing a bare harness with no model. The harness chip is
// dimmer than the model so the eye lands on the model first. The visible
// model is shortModel()-compressed; the full name rides in the title.
function harnessLine(m) {
  const harness = (m && m.state && m.state.harness) || '';
  const labels = harnessLabels(harness);
  const model = (m && m.state && m.state.model) || '';
  // Remote-access indicator — a bare 📱 glyph appended to the END of the
  // harness line when Remote Access is armed (best-known, JOH-256). Trails
  // the effort/cost tokens so the line reads "CC · O4.8 1M high 📱"; empty
  // when off. Computed up front so every return path can append it,
  // including the pre-first-tick (no-model) rows below — an armed agent
  // shouldn't be invisible just because its model hasn't landed yet.
  const remoteEl = remoteControlBadge(m);
  if (!model) {
    // No model reported yet. Keep Claude Code (the default) rows clean —
    // a freshly-spawned CC agent shows no line until its first tick — but
    // still flag a non-default harness (Codex) right away so a mixed group
    // is legible the moment an agent appears, not only after a model lands.
    // An armed remote indicator still earns a (minimal) line either way.
    if (isDefaultHarness(harness)) {
      return remoteEl ? `<div class="agent-harness">${remoteEl}</div>` : '';
    }
    return `<div class="agent-harness" title="Harness: ${esc(labels.long)}">`
      + `<span class="harness-name">${esc(labels.short)}</span>${remoteEl}</div>`;
  }
  // Reasoning-effort level (low…max), recorded by the statusline hook on
  // the same row as the model. Trails the model — "CC · O4.8 1M high" —
  // and is omitted entirely when absent (model without effort support, or
  // not ticked yet) so the line degrades to just "CC · O4.8 1M".
  const effort = (m && m.state && m.state.effort_level) || '';
  // Cumulative API cost, recorded by the statusline hook on the same row
  // — but only for sessions on API/enterprise pricing (the hook writes it
  // solely when the input carries no subscription rate-limit buckets), so
  // subscription agents stay at 0 and show no cost token. Trails the
  // effort — "CC · O4.8 1M high $0.42". Costs that round below a cent
  // show as "<1¢" rather than a lying "$0.00". A nonzero cost implies a
  // statusline tick, which always records the model too — so the
  // model-gate above never hides a real cost. Like the model, the cost
  // survives an agent's exit — what a dead agent cost is still useful.
  const cost = Number((m && m.state && m.state.cost_usd) || 0);
  // WHAT-IF sibling of cost: the pay-per-token-EQUIVALENT cost of a
  // subscription session (virtual_cost_usd). Rendered as a separate span
  // flagged hypothetical (≈) and CSS-hidden unless body.cost-whatif — the
  // dashboard is in WHAT-IF mode (the cost.show_on_subscription opt-in is
  // on). Real and virtual are normally exclusive per agent, so at most one
  // of the two cost spans below carries a value. body.agent-cost-hidden
  // (the Groups-tab 💲 toggle) suppresses both via CSS.
  const vcost = Number((m && m.state && m.state.virtual_cost_usd) || 0);
  let tip = `Harness: ${labels.long} — Model: ${model}`;
  if (effort) tip += ` — Effort: ${effort}`;
  if (cost > 0) tip += ` — API cost this session: $${cost.toFixed(4)} (API/enterprise pricing — no subscription limits)`;
  if (vcost > 0) tip += ` — WHAT-IF cost this session: $${vcost.toFixed(4)} (estimated if billed pay-per-token — you're on a subscription, so this is hypothetical, not a real charge)`;
  // One continuous string — "CC · O4.8 1M high $0.42" — no chip/box
  // around the harness. The spans exist only for typographic emphasis
  // (the harness prefix and the middot sit a shade dimmer than the
  // model; the effort and cost tokens a shade brighter).
  const effortEl = effort
    ? `<span class="harness-effort">${esc(effort)}</span>`
    : '';
  const costEl = cost > 0
    ? `<span class="harness-cost">${esc(cost >= 0.005 ? '$' + cost.toFixed(2) : '<1¢')}</span>`
    : '';
  const whatifEl = vcost > 0
    ? `<span class="harness-cost harness-cost-whatif" title="Estimated pay-per-token-equivalent cost this session — hypothetical, not a real charge (subscription)">${esc(vcost >= 0.005 ? '≈$' + vcost.toFixed(2) : '≈<1¢')}</span>`
    : '';
  return `<div class="agent-harness" title="${esc(tip)}">`
    + `<span class="harness-name">${esc(labels.short)}</span>`
    + `<span class="harness-sep">·</span>`
    + `<span class="harness-model">${esc(shortModel(model))}</span>`
    + effortEl + costEl + whatifEl + remoteEl + `</div>`;
}

// sandboxBadge renders the per-agent launch-sandbox chip — "🔒 workspace-
// write" — from state.sandbox_mode (Codex's --sandbox, recorded on the
// session row at spawn). Returns '' when no override is in effect: a row
// from before the column existed, or a Claude Code agent on 'inherit' (the
// default — sandbox configured out of band via settings.json, no per-session
// override), both show no badge. Note a Claude spawn now records the literal
// 'inherit' rather than '' (the tri-state carry that keeps an explicit inherit
// from being overridden), so 'inherit' is treated the same as unset here.
// read-only / workspace-write carry a lock; danger-full-access carries a
// warning glyph + a distinct class so a sandbox-OFF agent stands out. The full
// mode rides in the tooltip; the chip text is the bare mode.
function sandboxBadge(m) {
  const mode = (m && m.state && m.state.sandbox_mode) || '';
  if (!mode || mode === 'inherit') return '';
  const danger = mode === 'danger-full-access';
  const glyph = danger ? '⚠' : '🔒';
  const cls = danger ? 'sandbox-badge sandbox-danger' : 'sandbox-badge';
  const tip = danger
    ? `Sandbox: ${mode} — the OS sandbox is OFF (full access). Explicit opt-in.`
    : `Sandbox: ${mode} — launch-time OS sandbox confining the agent's writes`;
  return `<span class="${cls}" title="${esc(tip)}">${glyph} ${esc(mode)}</span>`;
}

// remoteControlBadge renders the at-a-glance "remote on" indicator — a bare
// 📱 glyph (no text label) — from state.remote_control (tclaude's best-known
// Remote Access flag, JOH-256). harnessLine appends it to the END of the
// harness line ("CC · O4.8 1M high 📱"), so the glyph alone carries the
// signal and the explanation rides in the title on hover. It is shown ONLY
// when remote control is on, mirroring sandboxBadge: a clean row carries no
// indicator, so an armed agent stands out as reachable from the Claude
// app/phone. There is no "off" indicator — off is the silent default.
// Best-known: the harness has no readback, so this reflects the last
// recorded intent and reconciles on the next refresh.
//
// The glyph is also a click affordance: it carries data-act="web-open-window"
// with the same conv/agent/label the ⚙-menu "web window" button uses, so
// clicking it opens the agent's live session (its Claude Code TUI) in a
// browser terminal — the natural "reach this reachable agent now" action.
// It is a clickable <span> (no <button>) to match the .cwd-link precedent and
// stay inline in the harness line; the delegated row-actions router dispatches
// any [data-act] element, so the span routes identically to the menu button.
function remoteControlBadge(m) {
  const on = !!(m && m.state && m.state.remote_control);
  if (!on) return '';
  const label = m.title || m.conv_id;
  const tip = 'Remote Access is ON — this agent is reachable from the Claude app/phone. '
    + 'Click to open its live session (Claude Code TUI) in a web terminal. '
    + 'Best-known state (the harness has no readback); toggle it from the row’s ⚙ menu.';
  return `<span class="remote-badge" data-act="web-open-window" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(label)}" title="${esc(tip)}">📱</span>`;
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

// === Slop slot-machine widget — the slop-mode replacement for statePill ===
//
// In slop mode (body.slop) the state pill is hidden via CSS and this
// three-reel slot machine appears in its place. It's pure CSS animation —
// no GIFs, no JS frame loop — so it costs nothing when slop is off
// (display:none, never reflowed) and the compositor handles the spin
// when slop is on. The widget is always emitted; the CSS swap means
// toggling slop in-place flips between the two without a re-render.
//
// State mapping:
//   working / main_agent_idle    → all 3 reels spin (per-reel duration)
//   idle                         → 7️⃣ 7️⃣ 7️⃣ (jackpot, gold pulse)
//   awaiting_permission / input  → ⏳ ❓ ⏳ (red/gold flicker)
//   error                        → 💥 ❌ 💥 (red glow)
//   crashed                      → 💀 💀 💀 (red frame)
//   exited / offline             → — — — (dim)
//
// Per-agent identity: a djb2 hash of conv_id picks three rotation
// offsets into SLOP_SYMBOLS so each agent's spinning reels show a
// different sequence — the machine "belongs" to that agent.
const SLOP_SYMBOLS = ['🍒', '🍋', '🍇', '🍊', '🔔', '⭐', '💎', '7️⃣'];
const SLOP_STOPPED = {
  idle:                ['7️⃣', '7️⃣', '7️⃣'],
  awaiting_permission: ['⏳', '❓', '⏳'],
  awaiting_input:      ['⏳', '❓', '⏳'],
  error:               ['💥', '❌', '💥'],
  crashed:             ['💀', '💀', '💀'],
  exited:              ['—', '—', '—'],
  offline:             ['—', '—', '—'],
};

// slopHash: djb2 over the conv-id string. The output is reduced mod
// SLOP_SYMBOLS.length per reel, so even a small hash space gives a
// good visual spread across the eight symbols.
function slopHash(s) {
  let h = 5381;
  for (let i = 0; i < s.length; i++) h = ((h << 5) + h + s.charCodeAt(i)) >>> 0;
  return h;
}

// slopReelStripHTML builds one reel's vertical strip: the 8-symbol set
// starting at `offset`, plus the starting symbol repeated at the end so
// the CSS spin animation (translateY from 0 to -8 cells) loops without
// a visible seam. The CSS keyframes are pinned to exactly 8 cells, so
// the strip length must match — keep them in sync if either changes.
function slopReelStripHTML(offset) {
  const n = SLOP_SYMBOLS.length;
  let html = '';
  for (let i = 0; i < n; i++) {
    html += '<span>' + SLOP_SYMBOLS[(i + offset) % n] + '</span>';
  }
  html += '<span>' + SLOP_SYMBOLS[offset % n] + '</span>';
  return '<span class="slop-strip">' + html + '</span>';
}

// slopMachine renders the per-row slot machine. Tooltip mirrors
// statePill so accessibility/discoverability stay equivalent.
function slopMachine(state, online, convID) {
  let status;
  if (!online) {
    status = ((state && state.exit_reason) || '') === 'unexpected' ? 'crashed' : 'offline';
  } else {
    status = (state && state.status) || 'idle';
  }
  const detail = (state && state.status_detail) || '';
  const tip = detail ? `${status}: ${detail}` : status;
  // data-conv tags the cell so slop-fx.js can correlate refresh-tick
  // re-renders (status transitions for the celebration) and route a
  // manual click back to the right row. The conv-id is already public
  // (it appears in many places on the page) so emitting it here adds
  // no new exposure.
  const conv = esc(convID || '');
  const stopped = SLOP_STOPPED[status];
  if (stopped) {
    const reels = stopped.map(g => `<span class="slop-reel slop-static">${g}</span>`).join('');
    return `<span class="slop-machine" data-status="${esc(status)}" data-conv="${conv}" title="${esc(tip)}" aria-label="${esc(tip)}">${reels}</span>`;
  }
  // Spinning state: a per-agent permutation of the symbol set on each
  // reel. Three offsets carved from one hash — independent enough to
  // look distinct, deterministic so the same agent keeps "its" machine
  // across reloads.
  const h = slopHash(convID || '');
  const n = SLOP_SYMBOLS.length;
  const offsets = [h % n, (h >>> 3) % n, (h >>> 7) % n];
  const reels = offsets.map(o => `<span class="slop-reel">${slopReelStripHTML(o)}</span>`).join('');
  return `<span class="slop-machine" data-status="${esc(status)}" data-conv="${conv}" title="${esc(tip)}" aria-label="${esc(tip)}">${reels}</span>`;
}

// === Wizard state pill — the wizard-mode replacement for statePill ===
//
// In wizard mode (body.wizard) the plain state pill is hidden via CSS and
// this arcane-flavored pill appears in its place (same swap trick as the
// slop machine: always emitted, CSS shows the one for the active theme, so
// toggling in-place needs no re-render). Each agent status maps to a
// sarcastic Dungeons-&-Dragons label + glyph; the REAL status (plus any
// detail) stays in the title/aria-label so hovering and screen readers get
// the honest state. data-conv + data-status let wizard-fx.js watch for the
// working→idle "spell resolved" sparkle, mirroring slop-fx's slot-machine
// watch.
const WIZARD_STATE = {
  working:             { glyph: '⚗️', label: 'Channeling' },
  main_agent_idle:     { glyph: '⚗️', label: 'Channeling' },
  idle:                { glyph: '🕯️', label: 'Meditating' },
  awaiting_permission: { glyph: '📜', label: 'Awaiting decree' },
  awaiting_input:      { glyph: '🗝️', label: 'Awaiting a key' },
  error:               { glyph: '💥', label: 'Spell backfired' },
  crashed:             { glyph: '💀', label: 'Slain by a grue' },
  exited:              { glyph: '🪦', label: 'Departed' },
  offline:             { glyph: '🪦', label: 'Departed' },
};

function wizardPill(state, online, convID) {
  let status;
  if (!online) {
    status = ((state && state.exit_reason) || '') === 'unexpected' ? 'crashed' : 'offline';
  } else {
    status = (state && state.status) || 'idle';
  }
  const detail = (state && state.status_detail) || '';
  // Honest tooltip: the real status (and detail), same as statePill — the
  // sarcastic label is flair, not a replacement for the truth.
  const tip = detail ? `${status}: ${detail}` : status;
  const conv = esc(convID || '');
  const m = WIZARD_STATE[status] || { glyph: '✨', label: status };
  return `<span class="wizard-pill" data-status="${esc(status)}" data-conv="${conv}" title="${esc(tip)}" aria-label="${esc(tip)}"><span class="wizard-pill-glyph">${m.glyph}</span> ${esc(m.label)}</span>`;
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

// contextManaTooltip is the 🧙 wizard-theme twin of contextMeterTooltip —
// the same three cases (real tokens / percent-only / not-yet-reported)
// re-flavoured as a wizard's mana reserve. The context window is the pool the
// agent channels from; a fuller meter means more of that mana has been spent.
// Purely cosmetic: the honest figure still rides in the regular meter's own
// tooltip (one theme-flip away) and `tclaude agent context-info`.
function contextManaTooltip(state, pct, known) {
  if (!known) return '🔮 Mana reserves: not yet divined';
  const tin = Number((state && state.tokens_input) || 0);
  const tout = Number((state && state.tokens_output) || 0);
  const win = Number((state && state.context_window_size) || 0);
  const total = tin + tout;
  if (win > 0 && total > 0) {
    return `🔮 Mana: ${fmtTokens(total)} / ${fmtTokens(win)} channeled — ${Math.round(pct)}%`;
  }
  return `🔮 Mana: ${Math.round(pct)}% channeled`;
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
  // Regular + wizard ("mana") twins — both always emitted, CSS reveals the one
  // for the active theme (body.wizard). A theme flip swaps the meter's colours
  // AND its tooltip with no re-render, the same "always emit, theme picks"
  // trick as the slot machine / wizard state pill. The segments (fill level +
  // traffic-light band classes) are shared verbatim; only the tooltip wording
  // and — via CSS — the lit-segment colours differ, so the honest "context
  // filling up" signal survives the re-skin as glowing mana crystals.
  const unk = known ? '' : ' ctx-unknown';
  const tip = contextMeterTooltip(state, pct, known);
  const manaTip = contextManaTooltip(state, pct, known);
  return `<span class="ctx-meter ctx-regular${unk}" title="${esc(tip)}">${segs}</span>`
    + `<span class="ctx-meter ctx-mana${unk}" title="${esc(manaTip)}">${segs}</span>`;
}

// activityBadges renders the small "background work still running" marker
// in an agent's state cell:
//   🤖+N  — N sub-agents spawned by this agent are still running
//
// It is deliberately NOT gated on the agent being "busy". The whole point
// is to flag work that OUTLIVES the parent's turn: an agent whose own turn
// has ended reads as idle, but if it left a sub-agent running that should
// be visible at a glance rather than blank. The badge carries a hover
// tooltip naming what it is, and sits in a column-flex container so any
// future per-agent activity markers can stack vertically beside it without
// the row growing wide. Returns '' when there is nothing to show.
//
// NOTE — there is intentionally NO background-shell ("🐚+M") badge. We
// considered counting Bash run_in_background commands, but dropped it:
// Claude Code fires no hook when a background shell EXITS (only a
// PreToolUse when it launches), so a hook-based count could never
// decrement — it would show "ghost" shells (already finished) for the
// whole idle window, which is exactly when the badge is read. Sub-agents
// have BOTH SubagentStart and SubagentStop hooks, but even that pair is
// lossy (no hooks fire on a user interrupt, for one), so the +N here is
// not the raw event tally: the backend keeps a self-healing per-agent_id
// ledger with a staleness TTL and known-zero resets, and subagent_count
// is its TTL-filtered live view (see db.SubagentSet in
// pkg/claude/common/db/subagents.go). Codex additionally reconciles its
// authoritative rollout sub_agent_activity stream on dashboard reads, which
// repairs a lost SubagentStop immediately. (A process-tree liveness reconcile
// in agentd could revive a trustworthy +M later — see the Groups section of
// docs/dashboard.md.)
function activityBadges(state) {
  const subagents = Number((state && state.subagent_count) || 0);
  if (subagents <= 0) return '';
  const tip = `${subagents} sub-agent${subagents === 1 ? '' : 's'} still running under this agent`;
  return `<span class="activity-badges"><span class="activity-badge badge-subagents" title="${esc(tip)}">🤖+${subagents}</span></span>`;
}

// roleCell renders the role column for a member row. Mirrors the CLI:
// members who are also owners get an "owner" badge; pure-owners
// (role==="owner" set by the daemon) show the badge alone.
//
// In a REAL group (g passed) the whole cell is a click-to-edit
// affordance (data-act="edit-role") — clicking opens the edit-member
// modal focused on the role field, where the role text, the
// group-owner checkbox and a Permissions… button all live. An empty
// role renders a discrete "+ role" hint rather than a blank cell, so
// the affordance stays discoverable. In the virtual Ungrouped group
// (no g) the cell is non-interactive — an ungrouped agent has no
// group-membership row to carry a role.
function roleCell(m, g) {
  const ownerBadge = '<span class="owner-badge">owner</span>';
  const hasRole = m.role && m.role !== 'owner';
  // A pure-owner row — an owner of the group that is NOT a member — is
  // surfaced by the daemon with the literal role sentinel "owner" and
  // has no membership row, so it carries no editable role/descr (a
  // PATCH /members/{conv} would 404). Render the badge alone,
  // non-interactive, exactly as before; ownership/perms for such a row
  // stay reachable via the ⚙ menu. (A real member who is also an owner
  // keeps their own role — empty or otherwise — and IS editable.)
  const pureOwner = m.owner && m.role === 'owner';
  if (!g || pureOwner) {
    if (m.owner) return hasRole ? `${esc(m.role)} ${ownerBadge}` : ownerBadge;
    return esc(m.role || '');
  }
  const attrs = `data-act="edit-role" data-group="${esc(g.name)}"`
    + ` data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(m.title || m.conv_id)}"`
    + ` data-current="${esc(m.title || '')}" data-role="${esc(m.role || '')}"`
    + ` data-descr="${esc(m.descr || '')}" data-tags="${esc(tagsAttr(m.tags))}" data-owner="${m.owner ? '1' : '0'}"`;
  const inner = hasRole
    ? `${esc(m.role)}${m.owner ? ' ' + ownerBadge : ''}`
    : (m.owner ? ownerBadge : '<span class="role-add">+ role</span>');
  return `<span class="role-edit" ${attrs} title="Edit role, ownership and permissions">${inner}</span>`;
}

// notifyMenuItem renders the per-agent OS-notification control as a ⚙
// options-menu row. It used to be an always-visible 🔔/🔕 bell beside
// the status dot, but the agent-ctl cluster was getting crowded, so the
// control now lives in the menu. One click still cycles the override
// inherit → off → on → inherit; the data-act / data-mode the row-action
// dispatcher reads are unchanged — only the presentation moved. The
// label states the current mode (and, for inherit, the effective
// on/off after group mutes) so the menu row is self-describing; the
// title keeps the full explanation. The global master switch (top-bar
// bell) still sits above all of this.
function notifyMenuItem(m) {
  const label = m.title || m.conv_id;
  const mode = m.notify || 'inherit';
  const effective = !!m.notify_effective;
  const glyph = (mode === 'off' || (mode === 'inherit' && !effective)) ? '🔕' : '🔔';
  let text, wizardText, tip;
  if (mode === 'off') {
    text = `${glyph} notify: off`;
    wizardText = `${glyph} omens: silent`;
    tip = wizWord(
      `notifications muted for ${label} — click to force ON (overrides a group mute)`,
      `omens silenced for familiar ${label} — click to restore them (overrides a party silence)`,
    );
  } else if (mode === 'on') {
    text = `${glyph} notify: on`;
    wizardText = `${glyph} omens: on`;
    tip = wizWord(
      `notifications forced ON for ${label} (overrides a group mute) — click to inherit from group`,
      `omens forced ON for familiar ${label} (overrides a party silence) — click to inherit from the party`,
    );
  } else {
    text = `${glyph} notify: inherit (${effective ? 'on' : 'off'})`;
    wizardText = `${glyph} omens: inherit (${effective ? 'on' : 'silent'})`;
    tip = wizWord(
      `notifications inherit (currently ${effective ? 'on' : 'off — a group is muted'}) for ${label} — click to mute`,
      `omens inherit from the party (currently ${effective ? 'on' : 'silent'}) for familiar ${label} — click to silence`,
    );
  }
  return `<button data-act="toggle-agent-notify" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-mode="${esc(mode)}" data-label="${esc(label)}" title="${esc(tip)}">${themeWords(text, wizardText)}</button>`;
}

// remoteControlMenuItem renders the ⚙-menu "toggle Remote Access" item — the
// per-agent twin of the harness's `/remote-control`. It carries data-intent
// = the OPPOSITE of the current best-known state (state.remote_control), so
// one click flips it: an off agent's button sends intent "on", an on
// agent's sends "off". The handler (row-actions.js, toggle-remote-control)
// POSTs /api/agents/{conv}/remote-control {intent} and refreshes; the server
// owns the toggle direction + the disable confirm-Enter, the UI only sends
// intent (JOH-259). Returns '' when the agent's harness has no Remote Access
// (canRemote=false, e.g. Codex), so the affordance is hidden exactly the way
// the rename control hides for a harness that can't deliver one. The phone
// glyph differs on/off (📱 reachable / 📴 off) to read at a glance in the menu.
function remoteControlMenuItem(m, canRemote) {
  if (!canRemote) return '';
  const label = m.title || m.conv_id;
  const on = !!(m && m.state && m.state.remote_control);
  const glyph = on ? '📱' : '📴';
  const intent = on ? 'off' : 'on';
  const text = on ? `${glyph} remote: on` : `${glyph} remote: off`;
  const wizardText = on ? `${glyph} remote scrying: on` : `${glyph} remote scrying: off`;
  const tip = on
    ? wizWord(
        `Remote Access is ON for ${label} — reachable from the Claude app/phone. Click to turn it OFF.`,
        `Remote scrying is ON for familiar ${label} — reachable from the Claude app/phone. Click to close it.`,
      )
    : wizWord(
        `Remote Access is OFF for ${label}. Click to turn it ON — expose this agent to the Claude app/phone.`,
        `Remote scrying is OFF for familiar ${label}. Click to open it to the Claude app/phone.`,
      );
  return `<button data-act="toggle-remote-control" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-intent="${esc(intent)}" data-label="${esc(label)}" title="${esc(tip)}">${themeWords(text, wizardText)}</button>`;
}

// MENU_SEP is the hairline divider between semantic groups of cog-menu
// items. It's an inert flex row — no data-act and not a <button>, so the
// row-actions.js click router ignores it (a click on it is treated like a
// click on the menu's padding: menu stays open, nothing dispatched).
const MENU_SEP = '<div class="menu-sep" role="separator"></div>';

// joinMenuGroups concatenates the per-group HTML runs with a MENU_SEP
// between them, dropping any empty group first so a group that renders to
// nothing (e.g. a Configure group whose only item is a hidden
// remote-control toggle) never leaves a dangling / doubled divider.
function joinMenuGroups(groups) {
  return groups.filter((g) => g && g.trim() !== '').join(MENU_SEP);
}

// memberActions renders the per-row action cell for a real group member.
// focus + hide stay at the TOP LEVEL — the window pair, disabled when the
// agent is offline. Everything heavier is collected behind the ⚙ options
// cog so the row stays uncluttered, ordered into four semantic groups
// (light/frequent → heavy/destructive), divider-separated:
//   1. Inspect & reach   — view messages, term, open window, summary…
//   2. Configure         — edit, owner, permissions, sudo, notify, remote, schedule
//   3. Lifecycle         — clone, reincarnate
//   4. Remove / destroy  — remove-from-group, retire
// The cog is always present and enabled.
function memberActions(g, m, canRemote) {
  const menu = joinMenuGroups([
    viewMessagesButton(m) + termButton(m) + webTermButton(m) + openWindowButton(m) + webOpenWindowButton(m) + exportAgentButton(m),
    editMemberButton(g, m) + ownerToggleButton(g, m) + permMemberButton(m) + sudoMemberButton(m)
      + notifyMenuItem(m) + remoteControlMenuItem(m, canRemote) + cronMemberButton(m),
    cloneAgentButton(m) + reincarnateAgentButton(m),
    removeMemberButton(g, m) + retireMemberButton(m),
  ]);
  return `<div class="row-actions">${focusHideButtons(m)}${retireIconButton(m)}${actionCog('row-menu', menu)}</div>`;
}
// cloneAgentButton renders a "clone" button for any row that
// represents a single agent. Clone forks a sibling that inherits the
// source's identity (groups / perms / ownership). The original keeps
// running.
function cloneAgentButton(m) {
  const label = m.title || m.conv_id;
  const cwd = (m.state && m.state.cwd) || m.cwd || '';
  const tip = wizWord(
    'Fork a sibling agent that inherits identity (groups, perms, ownership). The original keeps running.',
    'Mirror this familiar into a sibling that inherits its parties, boons, and ownership. The original keeps channeling.',
  );
  return `<button data-act="clone" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(label)}" data-cwd="${esc(cwd)}" title="${esc(tip)}">${themeWords('clone', 'mirror familiar')}</button>`;
}
// reincarnateAgentButton renders a "reincarnate" button for any row
// that represents a single agent. The modal it opens defaults to
// asking the agent to reincarnate ITSELF (it writes its own handoff);
// a force mode does the immediate daemon-driven reincarnation.
function reincarnateAgentButton(m) {
  const label = m.title || m.conv_id;
  const tip = wizWord(
    'Reincarnate this agent — by default ask it to do so itself (it writes its own handoff); or force an immediate daemon-driven reincarnation.',
    'Reincarnate this familiar — by default ask it to write its own handoff; or force its immediate return in a fresh vessel.',
  );
  return `<button data-act="reincarnate" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(label)}" title="${esc(tip)}">${themeWords('reincarnate', 'reincarnate familiar')}</button>`;
}
function sudoMemberButton(m) {
  return `<button data-act="sudo-grant" data-conv="${esc(m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" title="${esc(wizWord('Grant a time-bounded sudo elevation to this agent', 'Grant this familiar a time-bounded sudo boon'))}">+ sudo</button>`;
}
// exportAgentButton renders the ⚙-menu "📋 summary…" item — opens the export
// modal that asks the LIVE agent to consolidate a shareable artifact (a
// summary / report, one or more files auto-zipped), which the browser then
// downloads. Disabled while the agent is offline: the export runs in the
// agent's own session, so it needs a running pane (the daemon fast-fails an
// offline target anyway). The async, agent-produced twin of the group's
// mechanical "⤓ export".
function exportAgentButton(m) {
  const label = m.title || m.conv_id;
  const dis = m.online ? '' : ' disabled';
  const why = m.online
    ? wizWord(
        'Ask this agent to produce a shareable export of the conversation (a summary / report) and download it here. Multiple files are zipped automatically.',
        'Ask this familiar to inscribe a shareable account of its conversation and bring it here. Multiple scrolls are bundled automatically.',
      )
    : wizWord(
        'Export needs a running agent — it produces the file in its own session. Unavailable while the agent is offline.',
        'The familiar must be channeling to inscribe an export. Unavailable while it slumbers.',
      );
  return `<button data-act="export-summary" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(label)}"${dis} title="${esc(why)}">${themeWords('summary…', 'inscribe scroll…')}</button>`;
}
// permMemberButton renders the per-row "permissions" affordance —
// opens the permanent-permission editor (grant / deny / default per
// slug). The permanent twin of "+ sudo" right beside it.
function permMemberButton(m) {
  const tip = wizWord(
    "Edit this agent's permanent permissions (grant / deny / inherit-default)",
    "Open this familiar's grimoire of permanent boons and bindings",
  );
  return `<button data-act="perm-edit" data-conv="${esc(m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" title="${esc(tip)}">${themeWords('permissions', 'grimoire')}</button>`;
}
// cronMemberButton renders the ⏰ "schedule a nudge for this member"
// button. Opens the cron-create modal prefilled with Solo target =
// this member's stable agent_id, and Owner = the same (self-nudge is
// the common case from member rows). conv-id is the fallback for a
// pre-identity member (JOH-312).
function cronMemberButton(m) {
  const label = m.title || m.conv_id;
  const prefill = JSON.stringify({
    targetMode: 'solo',
    target: m.agent_id || m.conv_id,
    owner: m.agent_id || m.conv_id,
  });
  return `<button data-act="cron-new" data-prefill="${esc(prefill)}" data-label="${esc(label)}" title="${esc(wizWord(`Schedule a recurring nudge for ${label}`, `Bind a recurring ritual for familiar ${label}`))}">${themeWords('schedule…', 'bind ritual…')}</button>`;
}

// viewMessagesButton renders the ⚙-menu "view messages" item — a deep
// link that opens the Messages tab filtered to this agent's mailbox (its
// existing per-agent folder). Dispatched by row-actions.js
// (view-agent-messages → openMailbox(conv)).
function viewMessagesButton(m) {
  const label = m.title || m.conv_id;
  return `<button data-act="view-agent-messages" data-conv="${esc(m.conv_id)}" data-label="${esc(label)}" title="${esc(wizWord("Open this agent's messages in the Messages tab", "Open this familiar's missives in the Messages tab"))}">${themeWords('view messages', 'view missives')}</button>`;
}

// termButton renders the "open a terminal in this agent's working
// directory" affordance. Shown whether the agent is online or not —
// the directory is known from the DB regardless of whether the
// agent's tmux pane is currently alive.
function termButton(m) {
  const label = m.title || m.conv_id;
  return `<button data-act="term" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(label)}" title="${esc(wizWord("Open a terminal in this agent's working directory", "Open a scrying portal in this familiar's working directory"))}">${themeWords('term', 'scrying portal')}</button>`;
}

// webTermButton renders the "web term" affordance — the same shell-in-the-
// working-directory as `term`, but ALWAYS streamed into an in-browser xterm
// (modal-term.js) instead of a native window. `term` only falls back to the
// browser when the daemon host can't pop a native window; this button forces
// it, which is what you want when reaching the dashboard remotely (e.g. from a
// phone) even though the host itself has a display.
function webTermButton(m) {
  const label = m.title || m.conv_id;
  return `<button data-act="web-term" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(label)}" title="${esc(wizWord("Open a terminal in this agent's working directory, in the browser (always a web terminal — never a native window)", "Open a browser scrying portal in this familiar's working directory"))}">${themeWords('web term', 'web scrying portal')}</button>`;
}

// openWindowButton renders the explicit "open a terminal attached to
// this agent's live session" affordance — distinct from `term` (a shell
// in the working dir), this lands you inside the agent's running Claude
// Code TUI. The explicit way to get a console — works regardless of the
// focus.raise_only config (which, when on, makes plain focus a no-op for
// a windowless agent). Needs the agent online (404s without a live session).
function openWindowButton(m) {
  const label = m.title || m.conv_id;
  return `<button data-act="open-window" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(label)}" title="${esc(wizWord("Open a terminal window attached to this agent's live session (its Claude Code TUI)", "Reveal a scrying portal onto this familiar's live session"))}">${themeWords('open window', 'reveal portal')}</button>`;
}

// webOpenWindowButton renders the "web window" affordance — the same
// attach-to-the-live-session as `open window`, but ALWAYS streamed into an
// in-browser xterm (modal-term.js) instead of a native window. `open window`
// only falls back to the browser when the daemon host can't pop a native
// window; this button forces it, for reaching a live agent's TUI from a
// remote dashboard (e.g. a phone) even though the host itself has a display.
function webOpenWindowButton(m) {
  const label = m.title || m.conv_id;
  return `<button data-act="web-open-window" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(label)}" title="${esc(wizWord("Open a terminal attached to this agent's live session (its Claude Code TUI), in the browser (always a web terminal — never a native window)", "Reveal a browser scrying portal onto this familiar's live session"))}">${themeWords('web window', 'web portal')}</button>`;
}

// Eye glyphs for the focus / hide window buttons — an open eye for
// "show this window" (focus) and an eye with a slash for "hide it".
// Inline Feather-style SVG (MIT line icons): monochrome, and they
// inherit the button's text colour via stroke="currentColor", so they
// dim and brighten with the rest of the row-action cluster. aria-hidden
// because the host <button> carries the accessible name (aria-label).
const EYE_OPEN_SVG = '<svg class="eye-ico" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>';
const EYE_OFF_SVG = '<svg class="eye-ico" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24"/><line x1="1" y1="1" x2="23" y2="23"/></svg>';

// Trash-can glyph for the top-level "retire this agent" quick control —
// the icon twin of the ⚙-menu "retire" item (retireMemberButton). Feather-
// style inline SVG (MIT line icons) matching the eye pair: monochrome, it
// inherits the button's text colour via stroke="currentColor", so it dims
// and brightens with the rest of the row-action cluster. aria-hidden
// because the host <button> carries the accessible name (aria-label).
const TRASH_SVG = '<svg class="trash-ico" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><line x1="10" y1="11" x2="10" y2="17"/><line x1="14" y1="11" x2="14" y2="17"/></svg>';

// focusHideButtons renders the window pair kept at the TOP LEVEL of a
// member row: focus raises the agent's terminal window, hide detaches
// it (the per-agent twin of the group "windows" bulk unfocus). They
// render as eye icons — open eye = show, slashed eye = hide — rather
// than text labels. An offline agent has no window, so the pair
// renders DISABLED rather than vanishing — the row's control cluster
// keeps a stable shape whether the agent is on or off. Powering the
// agent up/down has no button here: the status dot (agentStatusDot)
// is the power control. term and every heavier action live in the
// per-row ⚙ options menu (see actionCog). Used by both real-group
// member rows and the virtual Ungrouped group's rows so the surface
// is identical.
function focusHideButtons(m) {
  const label = m.title || m.conv_id;
  // A disabled <button> fires no click event, so the delegated
  // dispatcher never sees an offline focus/hide — no extra guard
  // needed in row-actions.js.
  const dis = m.online ? '' : ' disabled';
  const why = m.online ? '' : ' — unavailable while the agent is offline';
  return `<button class="icon-btn" data-act="jump" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(label)}" title="Focus this agent's terminal window${why}" aria-label="Focus window"${dis}>${EYE_OPEN_SVG}</button>`
    + `<button class="icon-btn" data-act="hide" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(label)}" title="Hide this agent's terminal window — detaches its tmux client. The agent keeps running.${why}" aria-label="Hide window"${dis}>${EYE_OFF_SVG}</button>`;
}

// retireIconButton renders the top-level 🗑 "retire this agent" quick
// control that rides beside the ⚙ cog in the row-action cluster — the icon
// twin of the ⚙-menu retire item (retireMemberButton). It carries the SAME
// data-act="retire-agent" and the SAME conv-keyed selector (data-conv +
// data-label, deliberately NO data-agent — see retireMemberButton for why
// retire must stay conv-keyed, JOH-322), so the delegated dispatcher routes
// it through the identical retireAgentInteractive → retireConfirm flow. It
// exists to promote retire from a buried menu item to a one-click icon,
// sparing the operator the menu (or the long drag-onto-Retired) for the
// common "send this agent to the bin" action — the menu item stays for
// discoverability, exactly as focus/hide live top-level while heavier
// actions collect in the cog.
//
// Styled `warn` (not `danger`) to match the menu item's semantics: retire
// is a REVERSIBLE demotion (reinstate), heavier than the group actions yet
// lighter than a permanent delete — so on hover it lights amber, not red.
// Always present and enabled: retiring an offline agent is valid (its
// shutdown is then a no-op), matching both the menu item and the drag.
function retireIconButton(m) {
  const label = m.title || m.conv_id;
  return `<button class="icon-btn warn" data-act="retire-agent" data-conv="${esc(m.conv_id)}" data-label="${esc(label)}" title="Retire this agent — demote it back to a plain conversation, revoking its group memberships and permission grants. Reversible via reinstate (stripped grants are not restored)." aria-label="Retire agent">${TRASH_SVG}</button>`;
}

// actionCog renders the ⚙ "more actions" cog and its collapsed
// dropdown — the surface that collects a row's / group's less-used
// buttons so the table stays uncluttered. `act` is the cog's data-act
// ('row-menu' for an agent row, 'group-menu' for a group header); the
// delegated handler in row-actions.js toggles the sibling .action-menu
// and closes any other open one. `items` is the pre-built HTML of the
// menu's buttons — each keeps its own data-act and every data-*
// untouched, so the existing dispatcher handles them unchanged; only
// their position in the DOM moves.
//
// Cog and menu are emitted as siblings, inside the same .row-actions /
// .group-actions container as the buttons kept top-level — NOT floated
// to document.body — so a menu item's handler that walks up the DOM to
// its enclosing group <details> (rename-group, to find .group-name)
// still resolves. (The .group-actions cluster is no longer inside the
// <summary> itself — #212 moved it into the .subtable — so a
// closest('summary') walk from here would miss; closest('details') is
// the reliable anchor.)
function actionCog(act, items) {
  // U+FE0E (text variation selector) pins the gear to its monochrome
  // text glyph so the CSS amber colour applies — without it some
  // platforms render U+2699 as a colour emoji that ignores `color`.
  // The glyph rides a .cog-glyph span so the wizard theme can spin just
  // the gear (an enchanted cogwheel) without rotating the bordered box.
  //
  // ARIA menu-button pattern: the cog is aria-haspopup="menu" with an
  // aria-expanded the toggle handler / closeAllActionMenus keep in
  // sync; the dropdown is role="menu". role="menuitem" is stamped onto
  // every collected button HERE, at the menu-construction site —
  // `items` is HTML we built ourselves (a flat run of <button …>
  // elements), so the literal substring insert is safe and can't miss
  // one of the ~15 button templates the way per-template edits could.
  const menuItems = items.replaceAll('<button ', '<button role="menuitem" ');
  return `<button type="button" class="cog-btn" data-act="${esc(act)}"`
    + ` aria-haspopup="menu" aria-expanded="false"`
    + ` title="More actions" aria-label="More actions"><span class="cog-glyph">⚙︎</span></button>`
    + `<div class="action-menu" role="menu">${menuItems}</div>`;
}

// editMemberButton renders the per-agent "edit" button — the single
// panel for editing an agent: its title (incl. the "auto" self-rename),
// its group role and its group description. data-current carries the
// title so the modal opens pre-filled.
function editMemberButton(g, m) {
  const tip = wizWord(
    'Edit this agent — title, role, description, ownership, permissions',
    'Enchant this familiar — title, class, description, party ownership, and grimoire',
  );
  return `<button data-act="edit-member" data-group="${esc(g.name)}" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" data-current="${esc(m.title || '')}" data-role="${esc(m.role || '')}" data-descr="${esc(m.descr || '')}" data-tags="${esc(tagsAttr(m.tags))}" data-owner="${m.owner ? '1' : '0'}" title="${esc(tip)}">${themeWords('edit', 'enchant')}</button>`;
}
function ownerToggleButton(g, m) {
  return m.owner
    ? `<button class="warn" data-act="revoke-owner" data-group="${esc(g.name)}" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" title="${esc(wizWord('Revoke group owner status', 'Revoke party owner status'))}">${themeWords('revoke owner', 'revoke party owner')}</button>`
    : `<button data-act="grant-owner" data-group="${esc(g.name)}" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" title="${esc(wizWord('Make this agent an owner of the group', 'Make this familiar an owner of the party'))}">${themeWords('make owner', 'make party owner')}</button>`;
}
function removeMemberButton(g, m) {
  return `<button class="danger" data-act="remove-member" data-group="${esc(g.name)}" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" title="${esc(wizWord('Remove this agent from the group', 'Remove this familiar from the party'))}">${themeWords('remove', 'dismiss from party')}</button>`;
}
// retireMemberButton renders the "retire" lifecycle action — the
// ⚙-menu twin of dragging the row onto the virtual Retired group.
// Both dispatch the same retire-agent path (case 'retire-agent' in
// row-actions.js) and the same retireConfirm modal (shutdown + worktree
// checkboxes), so they ask the identical question; the button just
// spares the operator the long drag-to-Retired when many groups and
// agents are on screen. Retire demotes the agent back to a plain
// conversation, revoking its group memberships and permission grants —
// reversible via reinstate, though stripped grants are not restored.
// Styled `warn` (a reversible demotion) so it reads as heavier than the
// reversible group actions above it yet lighter than the permanent
// `danger` delete. Always present and enabled — retiring an offline
// agent is valid (shutdown is then a no-op), matching the drag gesture.
function retireMemberButton(m) {
  // NB: retire intentionally stays conv-keyed (no data-agent). The retire
  // endpoint resolves a UUID-shaped conv-id that FAILS to resolve into the
  // "dangling agent entry" 409-recovery path (enrollment_handlers.go); a
  // stable agent_id resolves successfully even when the conversation is
  // gone, which would silently demote the orphan instead of offering to
  // remove it. So retire is a conv-keyed KEEP — see row-actions.js (JOH-322).
  const tip = wizWord(
    'Retire this agent — demote it back to a plain conversation, revoking its group memberships and permission grants. Reversible via reinstate (stripped grants are not restored).',
    'Banish this familiar — return it to a plain conversation, revoking its party memberships and boons. Reversible via reinstate.',
  );
  return `<button class="warn" data-act="retire-agent" data-conv="${esc(m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" title="${esc(tip)}">${themeWords('retire', 'banish')}</button>`;
}

// ungroupedMemberActions renders the per-row action cell for a row in
// the virtual "Ungrouped" group. Like memberActions it keeps focus +
// hide at the top level and collects the rest behind the ⚙ options
// cog, but it deliberately OMITS every group-affecting button (the
// edit panel, owner toggle, remove-from-group) — the agent belongs to
// no group. It keeps retire (demote to a plain conversation) and its
// destructive action is delete-agent rather than remove-from-group.
// Powering the agent up/down is the status
// dot's job; renaming is the click-to-edit name cell. To put an
// ungrouped agent INTO a group, drag its row onto a group header.
function ungroupedMemberActions(m, canRemote) {
  const menu = joinMenuGroups([
    viewMessagesButton(m) + termButton(m) + webTermButton(m) + openWindowButton(m) + webOpenWindowButton(m) + exportAgentButton(m),
    permMemberButton(m) + sudoMemberButton(m)
      + notifyMenuItem(m) + remoteControlMenuItem(m, canRemote) + cronMemberButton(m),
    cloneAgentButton(m) + reincarnateAgentButton(m),
    retireMemberButton(m)
      + `<button class="danger" data-act="delete-agent" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(m.title || m.conv_id)}" title="${esc(wizWord('Permanently delete this agent and conversation', 'Permanently erase this familiar and its conversation scroll'))}">${themeWords('delete', 'erase familiar')}</button>`,
  ]);
  return `<div class="row-actions">${focusHideButtons(m)}${retireIconButton(m)}${actionCog('row-menu', menu)}</div>`;
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
  const agent = m.agent_id || m.conv_id || '';
  const fmt = (d, which) => {
    if (!d) return '<span class="cwd">—</span>';
    return `<span class="cwd cwd-link" data-act="term-dir" data-conv="${esc(conv)}" data-agent="${esc(agent)}" data-which="${which}" title="Open a terminal here — ${esc(d)}">${esc(shortCwd(d))}</span>`;
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
  const fmt = (branch, url, prNum, prURL, prState) => {
    if (!branch) return '<span class="muted">—</span>';
    const inner = `⎇ ${esc(branch)}`;
    const branchEl = url
      ? `<a class="branch branch-link" href="${esc(url)}" target="_blank" rel="noopener noreferrer" draggable="false" title="Open branch on GitHub — ${esc(branch)}">${inner}</a>`
      : `<span class="branch" title="git branch: ${esc(branch)}">${inner}</span>`;
    // State drives the PR link color: green=open, purple=merged, red=closed,
    // grey=unknown. Unknown covers older cache entries written before the
    // state field landed — the badge stays clickable, just not coloured.
    const stateClass = ['open', 'merged', 'closed'].includes(prState) ? `pr-state-${prState}` : 'pr-state-unknown';
    const stateLabel = prState ? prState.charAt(0).toUpperCase() + prState.slice(1) : 'Pull request';
    const prEl = (prNum && prURL)
      ? ` <a class="pr-link ${stateClass}" href="${esc(prURL)}" target="_blank" rel="noopener noreferrer" draggable="false" title="${esc(stateLabel)} pull request #${prNum}">#${prNum}</a>`
      : '';
    return branchEl + prEl;
  };
  const startupEl = fmt(m.startup_branch || '', m.startup_branch_url || '', m.startup_pr_number || 0, m.startup_pr_url || '', m.startup_pr_state || '');
  const currentEl = fmt(m.branch || '', m.branch_url || '', m.branch_pr_number || 0, m.branch_pr_url || '', m.branch_pr_state || '');
  const seenPRs = new Set([m.startup_pr_url || '', m.branch_pr_url || ''].filter(Boolean));
  const extraPRs = Array.isArray(m.presented_prs) ? m.presented_prs : [];
  const presented = extraPRs.map((pr) => {
    const url = (pr.url || '').trim();
    if (!url || seenPRs.has(url) || !/^https?:\/\//i.test(url)) return '';
    seenPRs.add(url);
    const state = pr.state || '';
    const stateClass = ['open', 'merged', 'closed'].includes(state) ? `pr-state-${state}` : 'pr-state-unknown';
    const label = pr.number ? `#${pr.number}` : (pr.summary || 'PR');
    const title = pr.summary ? `${pr.summary} — ${url}` : `Presented pull request — ${url}`;
    return `<a class="pr-link ${stateClass}" href="${esc(url)}" target="_blank" rel="noopener noreferrer" draggable="false" title="${esc(title)}">${esc(label)}</a>`;
  }).filter(Boolean).join(' ');
  const body = stackedLoc(startupEl, currentEl, (m.startup_branch || '') !== (m.branch || ''));
  return presented ? `${body} <span class="presented-prs">${presented}</span>` : body;
}

// taskCell renders the Task column: the agent's task-reference link
// (a Linear issue, GitHub issue/PR, ticket, …). `m.task_ref_url` is the
// raw URL; `m.task_ref_label` is the display label the daemon already
// derived (Linear→JOH-xxx, GitHub→#nnn, else host) or the human's
// explicit override. Empty renders an em dash so the column stays
// aligned.
//
// Defence in depth: the daemon only ever stores an http(s) task URL (the
// write path scheme-validates it), but an href is a stored-XSS sink, so
// this NEVER puts a non-http(s) value in one — a `javascript:`/`data:`
// URL survives HTML-escaping. Anything that isn't http(s) renders as
// inert text instead of a link.
function taskCell(m) {
  const url = (m.task_ref_url || '').trim();
  if (!url) return '<span class="muted">—</span>';
  const label = m.task_ref_label || url;
  if (!/^https?:\/\//i.test(url)) {
    return `<span class="task-ref muted" title="${esc(url)}">🔗 ${esc(label)}</span>`;
  }
  return `<a class="task-ref task-link" href="${esc(url)}" target="_blank" rel="noopener noreferrer" draggable="false" title="Open task reference — ${esc(url)}">🔗 ${esc(label)}</a>`;
}

// tagChips renders an agent's tags as compact pill chips (the same
// visual idiom as the status pills / owner badge). Tags arrive from the
// snapshot already sorted (alphabetical, server-side) so the chip order
// is stable across the 2s poll — a keyed morph re-renders this cell in
// place without reordering. Every tag is esc()'d — a tag is agent- or
// operator-authored free text. Returns '' for no tags (the descr cell
// then shows only its text / empty-state hint). The tf:<template>
// task-force marker gets a distinct class so it reads apart from a
// free-form operator tag.
function tagChips(tags) {
  if (!Array.isArray(tags) || tags.length === 0) return '';
  const chip = (t) => {
    const isTaskForce = t.slice(0, 3) === 'tf:';
    const cls = isTaskForce ? 'agent-tag agent-tag-tf' : 'agent-tag';
    const tip = isTaskForce ? `task force: ${t.slice(3)}` : `tag: ${t}`;
    return `<span class="${cls}" title="${esc(tip)}">${esc(t)}</span>`;
  };
  return `<span class="agent-tags">${tags.map(chip).join('')}</span>`;
}

// descrCell renders the Description column for a member row: the
// description text plus the agent's tag chips after it. It mirrors
// roleCell — in a REAL group (g passed) the whole cell is a
// click-to-edit affordance (data-act="edit-descr") that opens the same
// edit-member modal, focused on the Description / Tags fields; an empty
// descr + no tags renders a discrete "+ descr / tags" hint rather than a
// blank cell so the affordance stays discoverable. In the virtual
// Ungrouped group (no g) — and for a pure-owner row, which has no
// membership row to PATCH — the cell is non-interactive and just shows
// the text + chips.
function descrCell(m, g) {
  const text = (m.descr || '').trim();
  const chips = tagChips(m.tags);
  const pureOwner = m.owner && m.role === 'owner';
  const inertBody = (text ? `<span class="descr-text">${esc(text)}</span>` : '') + chips;
  if (!g || pureOwner) {
    return inertBody || '<span class="muted">—</span>';
  }
  const attrs = `data-act="edit-descr" data-group="${esc(g.name)}"`
    + ` data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-label="${esc(m.title || m.conv_id)}"`
    + ` data-current="${esc(m.title || '')}" data-role="${esc(m.role || '')}"`
    + ` data-descr="${esc(m.descr || '')}" data-tags="${esc(tagsAttr(m.tags))}" data-owner="${m.owner ? '1' : '0'}"`;
  const inner = inertBody || '<span class="descr-add">+ descr / tags</span>';
  return `<span class="descr-edit" ${attrs} title="Edit description and tags">${inner}</span>`;
}

// tagsAttr renders an agent's tag set as the comma-joined string the
// edit-member modal's Tags field pre-fills from (and the CLI prints). It
// is the round-trip form of the chip input — sorted already by the
// server. Empty for no tags.
function tagsAttr(tags) {
  return Array.isArray(tags) ? tags.join(', ') : '';
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

// Public API — the helpers used outside this module. actionCog is
// exported because render.js builds the group header's ⚙ menu with it.
// The rest (statusPillClass, fmtTokens, contextMeterTooltip, the
// per-row button builders, focusHideButtons, stackedLoc) are internal
// composition details of the exported builders above.
export {
  syncBotAnimations,
  syncWizardOrbit,
  $, $$, isModifiedClick, esc, themeWords, linkify, shortId, shortAgentId, idTooltip, syncSelectTitle, populateModelSelect, setModelSelectValue, MODEL_CUSTOM_VALUE, syncCustomModelRow, bindSelectTitles, makeModalResizable, bindModalSubmitHotkey, showModalError, onlineDot, agentStatusDot, harnessLine, sandboxBadge, remoteControlBadge, statePill, slopMachine, wizardPill, contextMeter, activityBadges,
  harnessCanRename, harnessCanRemoteControl,
  roleCell, descrCell, tagChips, memberActions, ungroupedMemberActions, actionCog, relTime, shortCwd,
  cwdCell, branchCell, taskCell, offlineDefault, groupOfflineOverride, groupShowOffline,
  groupOfflineToggleHTML,
  // slop-fx.js re-uses these for the manual-pull animation and the
  // 7-7-7 win detection — single source of truth.
  SLOP_SYMBOLS, SLOP_STOPPED,
  // Shared native/web directory-picker boundary. The Preact island registers
  // its web implementation without pulling Preact into this legacy module.
  pickDirectory, configureDirectoryPickerBridge, isLoopbackDashboard,
};
