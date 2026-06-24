// Slop theme — a purely cosmetic re-skin of the agent dashboard,
// tagged onto the URL with ?slop=1. Same data, same routes, same
// auth; just a different paint job. The server preserves the param
// through the auth redirect (see handleDashboardRoot in agentd/dashboard.go)
// so the bare-path URL still carries it.
//
// Click the header icon (🤝 / 🎰) — or hit the global hotkey
// Ctrl/⌘ + Alt/⌥ + Shift + S (see bindSlopHotkey) — to toggle slop mode
// at runtime. The URL is rewritten in place via history.replaceState so
// a refresh preserves the chosen state without leaving an extra history
// entry.

const SLOP_FAVICON =
  'data:image/svg+xml,<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">' +
  '<text x="50" y="52" font-size="78" text-anchor="middle" dominant-baseline="central">🎰</text></svg>';
const SLOP_EMOJI = '🎰';
const SLOP_REST = 'The slop machine';
// The slop favicon is itself a 🎰 (see SLOP_FAVICON), and the browser
// renders the favicon to the left of the tab title — so prefixing the
// title with a 🎰 too gave every tab two slot machines side-by-side.
// Drop the leading emoji here; the favicon is enough.
const SLOP_TITLE = 'The slop machine';

// Captured once on first apply so a click can restore the page to its
// pre-slop state. Reading from the DOM rather than hard-coding the
// strings keeps slop.js in sync with whatever dashboard.html ships.
const original = {
  emoji: '',
  rest: '',
  title: '',
  favicon: '',
  captured: false,
};

let iconSpan = null;
let restNode = null;

function captureOriginal() {
  if (original.captured) return;
  const h1 = document.querySelector('header h1');
  const link = document.querySelector('link[rel="icon"]');
  const text = h1 ? h1.textContent.trim() : '';
  // Split "🤝 tclaude agent dashboard" into [emoji, rest]. The leading
  // glyph may be a multi-codepoint emoji, so we slice on the first space
  // rather than the first character.
  const idx = text.indexOf(' ');
  original.emoji = idx > 0 ? text.slice(0, idx) : text;
  original.rest = idx > 0 ? text.slice(idx + 1) : '';
  original.title = document.title;
  original.favicon = link ? link.getAttribute('href') || '' : '';
  original.captured = true;
}

// Replace the h1's text with `<span class="slop-icon">…</span> rest`
// so we have a stable click target for the toggle. The span has no
// visual treatment beyond `cursor: pointer` — the easter egg lives
// or dies by the curious user hovering the header icon.
function ensureH1Structure() {
  const h1 = document.querySelector('header h1');
  if (!h1 || iconSpan) return;
  captureOriginal();
  h1.textContent = '';
  iconSpan = document.createElement('span');
  iconSpan.className = 'slop-icon';
  iconSpan.textContent = original.emoji;
  iconSpan.addEventListener('click', toggleSlop);
  restNode = document.createTextNode(' ' + original.rest);
  h1.appendChild(iconSpan);
  h1.appendChild(restNode);
}

function renderState() {
  const isSlop = document.body.classList.contains('slop');
  document.title = isSlop ? SLOP_TITLE : original.title;
  if (iconSpan) iconSpan.textContent = isSlop ? SLOP_EMOJI : original.emoji;
  if (restNode) restNode.nodeValue = ' ' + (isSlop ? SLOP_REST : original.rest);
  const link = document.querySelector('link[rel="icon"]');
  if (link) link.setAttribute('href', isSlop ? SLOP_FAVICON : original.favicon);
  // Broadcast the current slop state so feature modules can react without
  // importing slop.js internals — slop-audio.js listens here to suspend /
  // resume its FX AudioContext. (slop-fx and the marquee don't subscribe;
  // they read isSlopActive() inside their own click/fx/timer handlers.)
  // Fired on every apply/toggle; listeners that care about edges diff for
  // themselves. One-way, like the `tclaude:snapshot` event refresh.js emits.
  document.dispatchEvent(new CustomEvent('tclaude:slop', { detail: { active: isSlop } }));
  // The Vegas music features (tab / volume mixer / radio) are live in slop
  // mode too, so re-sync them whenever slop flips. They read the combined
  // isVegasActive() state (slop OR the regular-mode opt-in), not this
  // event's detail — see vegas.js / slop-volume.js.
  document.dispatchEvent(new CustomEvent('tclaude:vegas', { detail: { active: isVegasActive() } }));
}

// toggleSlop flips between the regular and slop themes — the same thing
// the header 🤝/🎰 icon click and the global hotkey do. Exported so the
// command palette can offer it as a "Switch theme" command.
export function toggleSlop() {
  document.body.classList.toggle('slop');
  renderState();
  const u = new URL(window.location.href);
  if (document.body.classList.contains('slop')) {
    u.searchParams.set('slop', '1');
  } else {
    u.searchParams.delete('slop');
  }
  // replaceState (not pushState) so the toggle doesn't litter back-button
  // history; the URL still reflects state so a refresh stays consistent.
  window.history.replaceState({}, '', u.toString());
}

export function applySlopThemeIfRequested() {
  ensureH1Structure();
  const params = new URLSearchParams(window.location.search);
  if (params.get('slop') === '1') {
    document.body.classList.add('slop');
    renderState();
  }
}

// isSlopActive checks the live body class instead of caching the URL
// param at load time — slop mode can flip mid-session via the header
// icon, and consumers (slop-fx.js) re-check on every click.
export function isSlopActive() {
  return document.body.classList.contains('slop');
}

// isVegasActive reports whether the Vegas music features — the Vegas tab,
// the header volume mixer + sound switch, and the lounge radio — should
// be live. That's true in slop ("casino") mode OR when the regular-mode
// opt-in (config slop.vegas_in_regular_mode → body.vegas, applied from the
// snapshot by refresh.js) is on. The music/volume modules gate on this
// instead of isSlopActive so they light up in both modes; the casino
// flair (slop-fx, slot machines, marquee) stays keyed on body.slop alone.
export function isVegasActive() {
  return document.body.classList.contains('slop') ||
    document.body.classList.contains('vegas');
}

// setVegasRegularMode toggles the regular-mode Vegas opt-in (body.vegas) —
// the config-driven twin of the slop theme that reveals the Vegas tab, the
// volume HUD and the radio WITHOUT the casino re-skin. refresh.js calls
// this from every snapshot with config slop.vegas_in_regular_mode. It only
// dispatches tclaude:vegas (which vegas.js / slop-volume.js sync off) when
// the state actually changes, so the 2s poll doesn't churn the player.
export function setVegasRegularMode(on) {
  on = !!on;
  if (on === document.body.classList.contains('vegas')) return;
  document.body.classList.toggle('vegas', on);
  document.dispatchEvent(new CustomEvent('tclaude:vegas', { detail: { active: isVegasActive() } }));
}

// bindSlopHotkey wires a single global keyboard shortcut that toggles
// slop mode from anywhere in the dashboard:
//
//   Ctrl + Alt + Shift + S   (Windows / Linux)
//   ⌘   + ⌥   + Shift + S   (macOS — Cmd substitutes for Ctrl)
//
// Three modifiers is deliberate: the easter egg must never fire by
// accident during normal work, and the trio dodges every default we
// could collide with — Ctrl+Shift+S is Firefox "Save As", Win+Shift+S is
// the Windows snip tool, Alt+Shift switches the keyboard layout. Nothing
// owns Ctrl/⌘+Alt+Shift+S, so we claim it.
//
// We match on e.code === 'KeyS' rather than e.key because e.key is
// keyboard-layout *and* modifier dependent: on macOS Option+S emits 'ß',
// and Ctrl+Alt (AltGr on Windows/EU layouts) can remap the produced
// character too. e.code is the physical key, so the shortcut behaves the
// same on every layout. e.repeat is ignored so holding the keys down
// doesn't strobe the toggle.
export function bindSlopHotkey() {
  document.addEventListener('keydown', (e) => {
    if (e.repeat) return;
    if (e.code !== 'KeyS') return;
    if (!e.shiftKey || !e.altKey) return;
    if (!e.ctrlKey && !e.metaKey) return;
    e.preventDefault();
    toggleSlop();
  });
}
