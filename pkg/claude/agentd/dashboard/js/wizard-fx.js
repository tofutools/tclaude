// Wizard-mode visual feedback — the 🧙 twin of slop-fx.js / slop-spectacle.js.
// Five behaviors, all gated on body.wizard (the toggle in slop.js) and
// prefers-reduced-motion:
//
//   1. Cast burst — a spark/rune spray on every interactive button press.
//   2. Cursor spark trail — fading arcane sparks follow the pointer.
//   3. Spell-resolved sparkle — when an agent's status actually transitions
//      from channeling (working / main_agent_idle) to meditating (idle) the
//      row's wizard pill flashes and bursts sparks.
//   4. Marquee ticker — sarcastic DnD one-liners under the header (reuses the
//      shared #slop-marquee node; CSS shows it in wizard mode too).
//   5. Meteor Swarm — the Konami code (↑↑↓↓←→←→ B A) erupts a banner, a
//      screen shake and an ember storm.
//
// Companion to slop.js (the theme toggle + chrome re-skin) and the CSS
// body.wizard re-skin. Like slop-fx, all bind functions install listeners at
// the document root once at boot; the body.wizard check inside each handler is
// what gates the effect, so toggling the theme mid-session needs no
// re-binding. Deliberately self-contained (its own reducedMotion/spawn
// helpers) rather than importing slop-fx internals — the two themes share
// mechanics, not code, and slop's coin visuals don't fit here.

import { isWizardActive } from './slop.js';
import { lastSnapshot } from './dashboard.js';

// Arcane sparks for the cursor trail and cast bursts…
const SPARK_EMOJIS = ['✨', '⭐', '💫', '🪄', '🔥', '🌟'];
// …and the louder set the Meteor Swarm + summon shower rain from the top edge.
const METEOR_EMOJIS = ['☄️', '🔥', '✨', '💥', '⭐', '🌟'];

// Silly spell-flavoured one-liners the summon banner flashes when a new
// familiar is conjured — the 🧙 twin of slop's "🎰 JACKPOT! 🎰". One is picked
// at random per spawn so a busy operator sees variety. Kept short so the
// banner reads at a glance; the CSS clamps the font on narrow viewports.
const SUMMON_QUOTES = [
  '🧙 It\'s wizard time!',
  '🔥 Fireball!',
  '✨ You shall not pass!',
  '🪄 Abracadabra!',
  '⚡ Lightning bolt! Lightning bolt!',
  '📜 A new familiar answers the call',
  '🔮 By the arcane, it lives!',
  '🐉 The summoning circle glows',
  '🎲 Natural 20 on the summon check',
  '🧪 It\'s alive… mostly',
  '👁️ The tower gains an apprentice',
  '☄️ Rise, familiar, rise!',
];

// reducedMotion gates every effect so a user with
// `prefers-reduced-motion: reduce` sees nothing extra. Read at call time
// (not module init) because the OS preference can change at runtime.
function reducedMotion() {
  return window.matchMedia
    && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
}

// spawnSpark drops a single emoji at (x,y) and animates it via the CSS
// keyframes `wizard-spark-arc` (see dashboard.css). Random arc/spin per
// spark so a burst looks unstructured; animationend self-cleans so the DOM
// never accumulates. `dy` is signed (negative arcs upward); `spread` is the
// half-width of the horizontal scatter.
function spawnSpark(x, y, spread, dy) {
  const el = document.createElement('span');
  el.className = 'wizard-spark';
  el.textContent = SPARK_EMOJIS[Math.floor(Math.random() * SPARK_EMOJIS.length)];
  el.style.left = x + 'px';
  el.style.top = y + 'px';
  el.style.setProperty('--dx', ((Math.random() - 0.5) * 2 * spread) + 'px');
  el.style.setProperty('--dy', (dy + dy * 0.6 * Math.random()) + 'px');
  el.style.setProperty('--rot', (Math.random() * 720 - 360) + 'deg');
  el.style.animationDuration = (0.7 + Math.random() * 0.5) + 's';
  document.body.appendChild(el);
  el.addEventListener('animationend', () => el.remove(), { once: true });
}

// sparkBurst erupts `n` sparks upward from (x, y) — the cast-spray and the
// spell-resolved celebration share this look.
function sparkBurst(x, y, n) {
  const count = n || 8;
  for (let i = 0; i < count; i++) spawnSpark(x, y, 90, -90);
}

// ─── Cast burst ────────────────────────────────────────────────────────
// A spark spray on real interactive controls — the wizard twin of slop-fx's
// coin spray. Same targeting rules: real buttons / data-act handles / the
// spawn chip / <summary>, but never the theme-toggle icon (sparking the very
// click that leaves wizard mode looks broken) or modal backdrops.

function shouldEmitFor(target) {
  if (!target || target.nodeType !== 1) return false;
  if (target.closest('.slop-icon')) return false;
  if (target.classList && target.classList.contains('modal-overlay')) return false;
  return !!target.closest('button, [data-act], .spawn-btn, summary');
}

export function bindWizardCastFx() {
  // Capture phase so a button's own stopPropagation doesn't suppress the spray.
  document.addEventListener('click', (e) => {
    if (!isWizardActive()) return;
    if (reducedMotion()) return;
    if (!shouldEmitFor(e.target)) return;
    // Actual pointer location when available; the target's centre for
    // keyboard-triggered clicks (Enter fires click with clientX/Y == 0).
    let x = e.clientX;
    let y = e.clientY;
    if (!x && !y) {
      const r = e.target.getBoundingClientRect();
      x = r.left + r.width / 2;
      y = r.top + r.height / 2;
    }
    const n = 4 + Math.floor(Math.random() * 3);
    for (let i = 0; i < n; i++) spawnSpark(x, y, 80, -70);
  }, true);
}

// ─── Cursor spark trail ──────────────────────────────────────────────────
// In wizard mode the cursor leaves a fading spark trail. Throttled so even a
// frantic mouse-shake doesn't flood the DOM; reduced-motion suppresses it.
const CURSOR_TRAIL_THROTTLE_MS = 70;
let cursorTrailLast = 0;

export function bindWizardCursorTrail() {
  document.addEventListener('mousemove', (e) => {
    if (!isWizardActive()) return;
    if (reducedMotion()) return;
    const now = performance.now();
    if (now - cursorTrailLast < CURSOR_TRAIL_THROTTLE_MS) return;
    cursorTrailLast = now;
    const el = document.createElement('span');
    el.className = 'wizard-trail-spark';
    el.textContent = SPARK_EMOJIS[Math.floor(Math.random() * SPARK_EMOJIS.length)];
    el.style.left = e.clientX + 'px';
    el.style.top = e.clientY + 'px';
    el.style.setProperty('--dx', ((Math.random() - 0.5) * 24) + 'px');
    el.style.setProperty('--dy', (10 + Math.random() * 16) + 'px');
    document.body.appendChild(el);
    el.addEventListener('animationend', () => el.remove(), { once: true });
  });
}

// ─── Spell-resolved sparkle ──────────────────────────────────────────────
// When an agent's status changes from a channeling state (working /
// main_agent_idle) to idle (meditating — a turn genuinely finished) we
// celebrate on that row's wizard pill. refresh() rebuilds the DOM every 2 s
// so an attribute observer wouldn't fire — instead we scan
// .wizard-pill[data-conv] every 1 s and diff against the previous tick.
// Mirrors slop-fx's slot-machine watch exactly (same statuses, same map
// hygiene) so the two themes celebrate the same real event.
const CHANNELING_STATUSES = new Set(['working', 'main_agent_idle']);
const prevStatusByConv = new Map();

function scanStatusTransitions() {
  if (!isWizardActive() || reducedMotion()) {
    // Wizard off — drop the map so the first tick after re-enable doesn't
    // fire a flood of false transitions for every now-idle agent.
    prevStatusByConv.clear();
    return;
  }
  const seen = new Set();
  const pills = document.querySelectorAll('.wizard-pill[data-conv]');
  for (const p of pills) {
    const conv = p.getAttribute('data-conv');
    if (!conv) continue;
    seen.add(conv);
    const status = p.getAttribute('data-status') || '';
    const prev = prevStatusByConv.get(conv);
    if (prev && CHANNELING_STATUSES.has(prev) && status === 'idle') {
      const r = p.getBoundingClientRect();
      sparkBurst(r.left + r.width / 2, r.top + r.height / 2, 8);
      p.classList.add('wizard-pill-flash');
      setTimeout(() => {
        if (p.isConnected) p.classList.remove('wizard-pill-flash');
      }, 1400);
    }
    prevStatusByConv.set(conv, status);
  }
  // Drop entries for agents that vanished from the DOM so the map doesn't
  // grow forever.
  for (const k of prevStatusByConv.keys()) {
    if (!seen.has(k)) prevStatusByConv.delete(k);
  }
}

export function bindWizardStatusWatch() {
  // 1 s tick — refresh() paints every 2 s, so a 2 s tick risks landing in the
  // gap and missing a fast transition. Matches slop-fx's cadence.
  setInterval(scanStatusTransitions, 1000);
}

// ─── Marquee ticker ──────────────────────────────────────────────────────
// Reuses the shared #slop-marquee node (CSS shows it in wizard mode too).
// slop-fx.js's marquee writer gates on isSlopActive() and this one on
// isWizardActive(); the two themes are mutually exclusive, so only the active
// theme ever writes — no contention on the shared node.
//
// Each scroll pass shows the live familiar-count line plus a small random
// sample from TICKER_QUOTES, joined with ✦ — short enough to actually read
// before it slides off (the old design joined the whole pool into one wall of
// text). The sample is re-rolled ONLY at scroll-loop boundaries
// (animationiteration) and on a theme flip into wizard mode; snapshot ticks
// (refresh.js dispatches `tclaude:snapshot`) re-render the same sample so a
// live count change can land without shuffling the quotes mid-scroll.

// The quote pool the ticker samples from — short, arcane, and sarcastic; one
// gag per line so a pass reads at a glance. Add freely: a bigger pool just
// means more variety, no pass gets longer.
const TICKER_QUOTES = [
  '🎲 Rolling for initiative…',
  '📜 The dice gods demand a rebase',
  '⚗️ A wild segfault appears — it was super effective',
  '🔮 +2 to your saving throw vs. merge conflicts',
  '🍺 The Tavern radio never closes',
  '🗡️ Choose wisely: some spells cannot be un-cast',
  '⏳ A wizard is never late — his CI is',
  '🐉 Here be dragons (see: legacy code)',
  '📖 The spellbook is just Stack Overflow with a leather cover',
  '🧪 Do not taste the potions in prod',
  '⚔️ Roll a d20 to approve this PR',
  '🔥 Fireball fixes most problems. Not this one.',
  '🏰 The dungeon is fully containerized',
  '👻 A ghost process haunts port 8080',
  '🦉 The owl delivers your scrolls at 2am',
  '🗝️ The key was in .env the whole time',
  '🍄 You found a mushroom. It does nothing.',
  '📜 Scroll of Undo: single use only',
  '🐸 The familiar got turned into a frog. Still passes CI.',
  '🎩 Pulling a hotfix out of the hat',
  '🕸️ The crypt has excellent Wi-Fi',
  '⚗️ Alchemy is just chemistry without unit tests',
  '🗡️ Critical hit! The bug takes 2d6 damage',
  '🎲 The DM says your deploy succeeds. Roll anyway.',
  '🔮 The crystal ball shows… another meeting',
  '🧟 The zombie process shambles on',
  '🐲 Feed the dragon or it eats your RAM',
  '🪶 Quill and parchment, but make it Markdown',
  '🌋 The volcano accepts YAML sacrifices',
  '🏹 You have 99 arrows. The bug needs 100.',
  '🧹 The broom sweeps the cache at midnight',
  '📿 Chanting "works on my machine" grants no protection',
  '🕯️ Rubber duck of True Seeing equipped',
  '🗿 The golem compiles… slowly',
  '⛲ Mana potion = coffee. This is canon.',
  '🃏 The jester rewrote the tests to always pass',
  '🧭 A maze of twisty little passages, all alike',
  '🪙 A copper piece for your stand-up update',
  '🐺 Werewolves only attack during full-moon deploys',
  '🏺 Ancient artifact detected: jQuery',
  '✨ Sparkles are 90% of wizardry',
  '📚 RTFM: Read The Forbidden Manuscript',
  '🧊 Frozen dependencies keep the lich fresh',
  '🚪 That settings dialog might be a mimic',
  '⚖️ Lawful neutral: obeys the linter without question',
  '🦴 The skeleton crew ships on weekends',
  '🌀 Teleportation is just SSH with extra steps',
  '👑 The lich king demands 2FA',
];

// How many quotes ride along with the count line each pass. Two keeps a pass
// readable in one 18 s scroll; the pool above provides the variety.
const TICKER_QUOTES_PER_PASS = 2;
let currentTickerQuotes = [];

// rollTickerQuotes re-samples the per-pass selection — distinct picks so a
// pass never repeats itself.
function rollTickerQuotes() {
  const pool = TICKER_QUOTES.slice();
  const picks = [];
  for (let i = 0; i < TICKER_QUOTES_PER_PASS && pool.length; i++) {
    picks.push(pool.splice(Math.floor(Math.random() * pool.length), 1)[0]);
  }
  currentTickerQuotes = picks;
}

function tickerLines() {
  const snap = lastSnapshot || {};
  const onlineCount = (snap.agents || []).filter(a => a.online).length;
  if (!currentTickerQuotes.length) rollTickerQuotes();
  return [
    `🧙 ${onlineCount} familiar${onlineCount === 1 ? '' : 's'} channeling the arcane`,
    ...currentTickerQuotes,
  ];
}

function tickerString() {
  return tickerLines().join('   ✦   ');
}

// updateMarqueeText writes the current ticker string IFF it changed — the
// cheap guard against snapshot ticks causing pointless mid-scroll jumps.
function updateMarqueeText(text) {
  const next = tickerString();
  if (text.textContent !== next) text.textContent = next;
}

export function bindWizardMarquee() {
  const text = document.getElementById('slop-marquee-text');
  const track = document.getElementById('slop-marquee-track');
  if (!text || !track) return;
  // If the page loaded already in wizard mode, paint live text now (the HTML
  // placeholder is the slop string). On a fresh load without a snapshot the
  // count reads 0 for one poll round-trip, then the snapshot event corrects
  // it — the same trade slop-fx makes.
  if (isWizardActive()) updateMarqueeText(text);
  // Snapshot ticks re-render the CURRENT sample (live count refresh) — no
  // re-roll, so the quotes never shuffle mid-scroll.
  document.addEventListener('tclaude:snapshot', () => {
    if (!isWizardActive()) return;
    updateMarqueeText(text);
  });
  // Scroll-loop boundary: the one visually-quiet moment to swap quotes.
  // Under prefers-reduced-motion the CSS kills the scroll animation, so this
  // never fires and the current sample stays frozen — intended: reduced
  // motion means static content, and a theme flip still refreshes it.
  track.addEventListener('animationiteration', () => {
    if (!isWizardActive()) return;
    rollTickerQuotes();
    updateMarqueeText(text);
  });
  // A theme flip into wizard mode should repaint immediately rather than wait
  // for the next snapshot — slop.js dispatches tclaude:wizard on every toggle.
  // Fresh roll too, so re-entering the theme doesn't replay the last pass.
  document.addEventListener('tclaude:wizard', (e) => {
    if (e.detail && e.detail.active) {
      rollTickerQuotes();
      updateMarqueeText(text);
    }
  });
}

// ─── Summon celebration ──────────────────────────────────────────────────
// The 🧙 twin of slop-fx's slopJackpot(): fired by the spawn modal on a
// successful POST. Flashes a random silly spell quote and rains an arcane
// shower from the top edge — the wizard-mode "It's wizard time!" moment when a
// new familiar is conjured. Silently no-ops when wizard mode is off (mirroring
// slopJackpot's slop-off no-op) so the spawn island can call it unconditionally
// next to slopJackpot; the two themes are mutually exclusive, so only the
// active one ever paints. reduced-motion suppresses it like every other
// wizard effect.
export function wizardSummon() {
  if (!isWizardActive() || reducedMotion()) return;
  const quote = SUMMON_QUOTES[Math.floor(Math.random() * SUMMON_QUOTES.length)];
  showBanner(quote, 'wizard-summon-banner');
  // A lighter shower than the Konami Meteor Swarm (70) — celebratory, not the
  // full spectacle, reusing the same top-edge fall so the two share a look.
  meteorStorm(34);
}

// ─── Enter-wizard-mode banner ────────────────────────────────────────────
// The "It's wizard time!" the whole theme is named for: flipping *into* wizard
// mode from another theme — via the +W hotkey, the palette's "Switch to wizard
// theme" command, or the header cycle regular→slop→wizard — flashes the same
// banner + arcane shower as a summon. Unlike wizardSummon()'s random spell
// quote this is one fixed line: entering the mode is a single event with a
// single greeting, where each summon wants variety, so no rotation here.
const ENTER_WIZARD_QUOTE = '🧙 It\'s wizard time!';

export function wizardEnter() {
  if (!isWizardActive() || reducedMotion()) return;
  showBanner(ENTER_WIZARD_QUOTE, 'wizard-summon-banner');
  meteorStorm(34);
}

// bindWizardEnterBanner flashes the enter banner on slop.js's tclaude:wizard
// edge event (detail.active === true means the theme just flipped INTO wizard
// mode). dashboard.js installs it inside its bootstrap IIFE, which runs after
// the top-level applySlopThemeIfRequested() — so a page that *loads* already in
// wizard mode (?wizard=1) dispatches its initial event before this listener
// exists and stays silent. A direct wizard-URL load isn't "entering from
// another mode", so it earns no banner; only a live, user-driven flip does.
export function bindWizardEnterBanner() {
  document.addEventListener('tclaude:wizard', (e) => {
    if (e.detail && e.detail.active) wizardEnter();
  });
}

// ─── Meteor Swarm (Konami) ───────────────────────────────────────────────
// Type ↑↑↓↓←→←→ B A and the page erupts: a banner, an ember storm from the
// top edge, and a screen shake. Self-contained spectacle (wizard mode has no
// audio/credits bus to route through, unlike slop's mega-jackpot).
const KONAMI = [
  'ArrowUp', 'ArrowUp', 'ArrowDown', 'ArrowDown',
  'ArrowLeft', 'ArrowRight', 'ArrowLeft', 'ArrowRight', 'b', 'a',
];
let konamiProgress = 0;

// showBanner is a centred flash banner (see .wizard-banner in dashboard.css),
// self-cleaning on animationend. `extraClass` (optional) adds a variant class
// — the summon banner uses it to clamp the font so a longer spell quote fits.
// The text rides an inner .wizard-banner-text span so a variant can cap its
// width and let it wrap while the full-viewport flex container keeps centring
// it (the Meteor Swarm's short text is unaffected).
function showBanner(str, extraClass) {
  const banner = document.createElement('div');
  banner.className = extraClass ? 'wizard-banner ' + extraClass : 'wizard-banner';
  banner.setAttribute('aria-hidden', 'true');
  const text = document.createElement('span');
  text.className = 'wizard-banner-text';
  text.textContent = str;
  banner.appendChild(text);
  document.body.appendChild(banner);
  banner.addEventListener('animationend', () => banner.remove(), { once: true });
}

// meteorStorm rains `count` emoji from random points along the top edge,
// staggered so it falls like a storm rather than dropping as one sheet.
function meteorStorm(count) {
  const vw = window.innerWidth || document.documentElement.clientWidth;
  const vh = window.innerHeight || document.documentElement.clientHeight;
  for (let i = 0; i < count; i++) {
    const el = document.createElement('span');
    el.className = 'wizard-meteor';
    el.textContent = METEOR_EMOJIS[Math.floor(Math.random() * METEOR_EMOJIS.length)];
    el.style.left = (Math.random() * vw) + 'px';
    el.style.top = '-24px';
    el.style.setProperty('--dx', ((Math.random() - 0.5) * 220) + 'px');
    el.style.setProperty('--dy', (vh * (0.8 + Math.random() * 0.4)) + 'px');
    el.style.setProperty('--rot', (Math.random() * 1080 - 540) + 'deg');
    el.style.animationDelay = (Math.random() * 0.4) + 's';
    el.style.animationDuration = (1.4 + Math.random() * 1.0) + 's';
    document.body.appendChild(el);
    el.addEventListener('animationend', () => el.remove(), { once: true });
  }
}

function meteorSwarm() {
  if (!isWizardActive() || reducedMotion()) return;
  showBanner('☄️ METEOR SWARM ☄️');
  meteorStorm(70);
  // Screen shake — a body class drives the keyframes; removed after the
  // animation so a later cast can re-trigger it.
  document.body.classList.add('wizard-shake');
  setTimeout(() => document.body.classList.remove('wizard-shake'), 800);
}

export function bindWizardSpectacle() {
  document.addEventListener('keydown', (e) => {
    if (!isWizardActive()) return;
    const key = e.key.length === 1 ? e.key.toLowerCase() : e.key;
    konamiProgress = key === KONAMI[konamiProgress] ? konamiProgress + 1 : (key === KONAMI[0] ? 1 : 0);
    if (konamiProgress === KONAMI.length) {
      konamiProgress = 0;
      meteorSwarm();
    }
  });
}
