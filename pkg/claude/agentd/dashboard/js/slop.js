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

// Wizard theme chrome — the 🧙 "it's wizard time" re-skin, sibling of the
// slop constants above, tagged onto the URL with ?wizard=1. Same data, same
// routes; a sarcastic over-the-top DnD paint job. Mutually exclusive with
// slop (the header icon cycles regular → slop → wizard). The favicon carries
// the 🧙 so the title, like SLOP_TITLE, doesn't repeat it.
const WIZARD_FAVICON =
  'data:image/svg+xml,<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">' +
  '<text x="50" y="52" font-size="78" text-anchor="middle" dominant-baseline="central">🧙</text></svg>';
const WIZARD_EMOJI = '🧙';
const WIZARD_REST = "The Wizard's Tower";
const WIZARD_TITLE = "The Wizard's Tower";

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
  // The header icon cycles regular → slop → wizard → regular. It's the
  // discoverable easter-egg toggle; a single click steps to the next theme.
  iconSpan.addEventListener('click', cycleTheme);
  restNode = document.createTextNode(' ' + original.rest);
  h1.appendChild(iconSpan);
  h1.appendChild(restNode);
}

// currentTheme reads the live cosmetic theme off the body class. slop and
// wizard are mutually exclusive full re-skins; anything else is 'regular'.
function currentTheme() {
  if (document.body.classList.contains('slop')) return 'slop';
  if (document.body.classList.contains('wizard')) return 'wizard';
  return 'regular';
}

function renderState() {
  const theme = currentTheme();
  const isSlop = theme === 'slop';
  const isWizard = theme === 'wizard';
  document.title = isSlop ? SLOP_TITLE : isWizard ? WIZARD_TITLE : original.title;
  if (iconSpan) iconSpan.textContent = isSlop ? SLOP_EMOJI : isWizard ? WIZARD_EMOJI : original.emoji;
  if (restNode) restNode.nodeValue = ' ' + (isSlop ? SLOP_REST : isWizard ? WIZARD_REST : original.rest);
  const link = document.querySelector('link[rel="icon"]');
  if (link) link.setAttribute('href', isSlop ? SLOP_FAVICON : isWizard ? WIZARD_FAVICON : original.favicon);
  // Broadcast the current slop state so feature modules can react without
  // importing slop.js internals — slop-audio.js listens here to suspend /
  // resume its FX AudioContext. (slop-fx and the marquee don't subscribe;
  // they read isSlopActive() inside their own click/fx/timer handlers.)
  // Fired on every apply/toggle; listeners that care about edges diff for
  // themselves. One-way, like the `tclaude:snapshot` event refresh.js emits.
  document.dispatchEvent(new CustomEvent('tclaude:slop', { detail: { active: isSlop } }));
  // The wizard twin — wizard-fx.js listens here to reset its marquee on a
  // theme flip. Its per-frame visual FX self-gate on body.wizard, same as
  // slop-fx reads isSlopActive(), so this is just the edge signal.
  document.dispatchEvent(new CustomEvent('tclaude:wizard', { detail: { active: isWizard } }));
  // The Vegas music features (tab / volume mixer / radio) are live in slop
  // AND wizard mode, so re-sync them whenever the theme flips. They read the
  // combined isVegasActive() state (slop OR wizard OR the regular-mode
  // opt-in), not this event's detail — see vegas.js / slop-volume.js.
  document.dispatchEvent(new CustomEvent('tclaude:vegas', { detail: { active: isVegasActive() } }));
}

// syncThemeParams rewrites the URL query to match the live theme classes:
// at most one of ?slop=1 / ?wizard=1 (or neither). replaceState (not
// pushState) so a toggle doesn't litter back-button history; the URL still
// reflects state so a refresh stays consistent.
function syncThemeParams() {
  const u = new URL(window.location.href);
  u.searchParams.delete('slop');
  u.searchParams.delete('wizard');
  const theme = currentTheme();
  if (theme === 'slop') u.searchParams.set('slop', '1');
  else if (theme === 'wizard') u.searchParams.set('wizard', '1');
  window.history.replaceState({}, '', u.toString());
}

// toggleSlop flips slop mode on or off — the global hotkey does this too.
// Enabling it clears wizard (the two re-skins are mutually exclusive).
// Exported so the command palette can offer it as a "Switch theme" command.
export function toggleSlop() {
  const on = !document.body.classList.contains('slop');
  document.body.classList.toggle('slop', on);
  if (on) document.body.classList.remove('wizard');
  renderState();
  syncThemeParams();
}

// toggleWizard flips wizard mode on or off — the +W hotkey and a palette
// command do this. Enabling it clears slop.
export function toggleWizard() {
  const on = !document.body.classList.contains('wizard');
  document.body.classList.toggle('wizard', on);
  if (on) document.body.classList.remove('slop');
  renderState();
  syncThemeParams();
}

// cycleTheme steps the header icon through the three themes in order:
// regular → slop → wizard → regular. The one-click discoverable path; the
// hotkeys and palette commands target a specific theme instead.
export function cycleTheme() {
  const theme = currentTheme();
  document.body.classList.remove('slop', 'wizard');
  if (theme === 'regular') document.body.classList.add('slop');
  else if (theme === 'slop') document.body.classList.add('wizard');
  // wizard → regular leaves both cleared.
  renderState();
  syncThemeParams();
}

export function applySlopThemeIfRequested() {
  ensureH1Structure();
  const params = new URLSearchParams(window.location.search);
  // slop and wizard are mutually exclusive; slop wins if both are somehow
  // present so the URL can never paint two re-skins at once.
  const wantSlop = params.get('slop') === '1';
  const wantWizard = params.get('wizard') === '1';
  if (wantSlop) {
    document.body.classList.add('slop');
    renderState();
  } else if (wantWizard) {
    document.body.classList.add('wizard');
    renderState();
  }
  // Canonicalise a hand-crafted ?slop=1&wizard=1 down to the single resolved
  // theme so the address bar doesn't linger with both params. The body class
  // is already exclusive above; this only tidies the URL, so it's a no-op for
  // the normal single-param load.
  if (wantSlop && wantWizard) syncThemeParams();
}

// isSlopActive checks the live body class instead of caching the URL
// param at load time — slop mode can flip mid-session via the header
// icon, and consumers (slop-fx.js) re-check on every click.
export function isSlopActive() {
  return document.body.classList.contains('slop');
}

// isWizardActive is the wizard twin of isSlopActive — wizard-fx.js re-checks
// it on every click/timer so a mid-session toggle needs no re-binding.
export function isWizardActive() {
  return document.body.classList.contains('wizard');
}

// wizWord picks the wizard-mode wording when wizard mode is live, else the
// regular wording. The shared home for the profile-vocabulary swaps that are
// JS-rendered (the profile editor title / empty-state in modal-profiles.js, the
// "+ new pattern…" picker option in row-actions.js) — the static-copy spots use
// the pure-CSS .profiles-word span pair instead. Re-checks live, like
// isWizardActive, so a mid-session toggle needs no re-binding.
export function wizWord(regular, wizard) { return isWizardActive() ? wizard : regular; }

// isVegasActive reports whether the Vegas music features — the Vegas tab,
// the header volume mixer + sound switch, and the lounge radio — should
// be live. That's true in slop ("casino") mode OR when the regular-mode
// opt-in (config slop.vegas_in_regular_mode → body.vegas, applied from the
// snapshot by refresh.js) is on. The music/volume modules gate on this
// instead of isSlopActive so they light up in both modes; the casino
// flair (slop-fx, slot machines, marquee) stays keyed on body.slop alone.
export function isVegasActive() {
  return document.body.classList.contains('slop') ||
    document.body.classList.contains('wizard') ||
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

// bindWizardHotkey is the wizard twin of bindSlopHotkey — the same three-
// modifier chord on the physical W key instead of S:
//
//   Ctrl + Alt + Shift + W   (Windows / Linux)
//   ⌘   + ⌥   + Shift + W   (macOS)
//
// The same rationale applies (see bindSlopHotkey): three modifiers so it
// never fires during normal work, e.code so it's layout-independent (⌥W
// emits '∑' on macOS), e.repeat ignored so a held key doesn't strobe. W is
// unclaimed with this trio (Ctrl+W closes a tab, but Ctrl+Alt+Shift+W is
// free), so we take it for wizard mode.
export function bindWizardHotkey() {
  document.addEventListener('keydown', (e) => {
    if (e.repeat) return;
    if (e.code !== 'KeyW') return;
    if (!e.shiftKey || !e.altKey) return;
    if (!e.ctrlKey && !e.metaKey) return;
    e.preventDefault();
    toggleWizard();
  });
}
