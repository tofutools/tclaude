// Slop-mode visual feedback. Five behaviors, all gated on body.slop
// (the toggle in slop.js) and prefers-reduced-motion:
//
//   1. Click-spray — coin burst on every interactive button press.
//   2. Spawn jackpot — banner + coin shower from the spawn modal.
//   3. Manual pull — click a row's slot machine for a fake spin that
//      lands on a random combo (7-7-7 → per-row mini jackpot).
//   4. Working→idle celebration — when an agent's status actually
//      transitions from spinning to idle (a real jackpot moment) the
//      row's machine bursts coins of its own.
//   5. Cursor coin trail — fading coins follow the pointer.
//   6. Marquee ticker — rotating one-liner under the header.
//
// Companion to slop.js (the theme toggle + chrome re-skin). All bind
// functions install listeners at the document root once at boot; the
// body-class check inside each handler is what actually gates the
// effect, so toggling slop mid-session needs no re-binding.

import { isSlopActive } from './slop.js';
import { SLOP_SYMBOLS } from './helpers.js';
import { lastSnapshot } from './dashboard.js';

const COIN_EMOJIS = ['🪙', '💰', '⭐', '🍒', '🔔', '7️⃣'];
const JACKPOT_EMOJIS = ['🪙', '💰', '⭐', '🍒', '🔔', '7️⃣', '🎰', '💎'];

// reducedMotion gates every effect — both the per-click spray and the
// jackpot banner — so a user with `prefers-reduced-motion: reduce` sees
// nothing extra. Read at call time (not module init) because the OS
// preference can change at runtime.
function reducedMotion() {
  return window.matchMedia
    && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
}

// spawnCoin drops a single emoji at (x,y), then animates it via the
// CSS keyframes `slop-coin-arc` (see dashboard.css). Random arc/spin
// per coin so a burst looks unstructured. animationend self-cleans.
//
// `dy` is signed: negative = the coin arcs upward (click bursts),
// positive = the coin falls down (jackpot rain). `spread` is the
// half-width of the horizontal scatter.
function spawnCoin(x, y, fromEmojis, spread, dy) {
  const el = document.createElement('span');
  el.className = 'slop-coin';
  el.textContent = fromEmojis[Math.floor(Math.random() * fromEmojis.length)];
  el.style.left = x + 'px';
  el.style.top = y + 'px';
  // --dx / --dy feed into the keyframes; randomised so each coin
  // takes its own arc instead of marching in formation. The 0.6
  // random factor keeps the same sign as `dy` so an upward arc
  // doesn't accidentally bias downward (or vice versa).
  el.style.setProperty('--dx', ((Math.random() - 0.5) * 2 * spread) + 'px');
  el.style.setProperty('--dy', (dy + dy * 0.6 * Math.random()) + 'px');
  el.style.setProperty('--rot', (Math.random() * 720 - 360) + 'deg');
  el.style.animationDuration = (0.7 + Math.random() * 0.5) + 's';
  document.body.appendChild(el);
  el.addEventListener('animationend', () => el.remove(), { once: true });
}

// shouldEmitFor decides whether a click should trigger a coin spray.
// We want the spray on actual interactive controls (real <button>s,
// data-act handles, the spawn chip) and NOT on modal backdrops, raw
// text, anchors that just toggle <details>, or the slop icon itself
// (it has its own jiggle and toggles the theme — adding a spray on
// the very click that turns slop off looks broken).
function shouldEmitFor(target) {
  if (!target || target.nodeType !== 1) return false;
  if (target.closest('.slop-icon')) return false;
  // The slot machine has its own manual-pull burst — let that handler
  // own the visuals, otherwise we'd double-coin every click.
  if (target.closest('.slop-machine')) return false;
  // Inert chrome — clicks on modal overlays/backdrops are dismissals,
  // not real button presses. Don't decorate them.
  if (target.classList && target.classList.contains('modal-overlay')) return false;
  return !!target.closest('button, [data-act], .spawn-btn, summary');
}

// bindSlopClickFx attaches a single delegated click listener at the
// document root. Capture phase so a button's own handler calling
// stopPropagation/preventDefault doesn't suppress our spray.
//
// The check happens on every click rather than at bind time so the
// listener can stay registered for the lifetime of the page and
// merely no-op while slop is off.
export function bindSlopClickFx() {
  document.addEventListener('click', (e) => {
    if (!isSlopActive()) return;
    if (reducedMotion()) return;
    if (!shouldEmitFor(e.target)) return;
    // Use the actual pointer location when available; fall back to
    // the target's center for keyboard-triggered clicks (Enter on a
    // focused button fires "click" with clientX/Y == 0).
    let x = e.clientX;
    let y = e.clientY;
    if (!x && !y) {
      const r = e.target.getBoundingClientRect();
      x = r.left + r.width / 2;
      y = r.top + r.height / 2;
    }
    const n = 4 + Math.floor(Math.random() * 3);
    // Negative dy → coins arc upward out of the click point.
    for (let i = 0; i < n; i++) spawnCoin(x, y, COIN_EMOJIS, 80, -70);
  }, true);
}

// slopJackpot is the bigger celebration: a centered "JACKPOT" banner
// plus a wider coin shower from the top of the viewport. Used by the
// spawn modal on a successful POST. Silently no-ops when slop is off
// so callers don't need their own conditional.
export function slopJackpot() {
  if (!isSlopActive()) return;
  if (reducedMotion()) return;
  // Mark the jackpot timestamp for the marquee — a banner-level
  // jackpot doesn't carry a conv-id (spawn isn't tied to one until
  // the new conv lands in the next refresh), so we record only the
  // time. The status-transition path notes a per-agent jackpot.
  lastJackpotAt = Date.now();
  const banner = document.createElement('div');
  banner.className = 'slop-jackpot';
  banner.textContent = '🎰 JACKPOT! 🎰';
  banner.setAttribute('aria-hidden', 'true');
  document.body.appendChild(banner);
  banner.addEventListener('animationend', () => banner.remove(), { once: true });
  // Shower coins from random points along the top edge so the burst
  // feels like it rained down on the page rather than erupting from
  // the banner itself.
  const vw = window.innerWidth || document.documentElement.clientWidth;
  for (let i = 0; i < 28; i++) {
    const x = Math.random() * vw;
    const y = -20;
    setTimeout(() => spawnCoin(x, y, JACKPOT_EMOJIS, 40, vw * 0.6),
      Math.random() * 600);
  }
}

// rowBurst is a smaller, localised version of slopJackpot — a 6-coin
// upward burst centred on a single row machine. Used both by the
// working→idle celebration and by the manual-pull non-jackpot path
// so they share one look.
function rowBurst(el) {
  const r = el.getBoundingClientRect();
  const x = r.left + r.width / 2;
  const y = r.top + r.height / 2;
  const n = 6 + Math.floor(Math.random() * 4);
  for (let i = 0; i < n; i++) spawnCoin(x, y, COIN_EMOJIS, 90, -90);
}

// ─── Manual pull ───────────────────────────────────────────────────
// Click any row's .slop-machine to give it a fake "pull": the cell
// spins fast for ~900 ms, then snaps to a random 3-symbol combo and
// holds it for ~1.8 s. Pure cosmetic — no agent state is touched.
// The next refresh() tick (or our restore timer) rebuilds the cell.

const PULL_SPIN_MS = 900;
const PULL_HOLD_MS = 1800;
// Track pull-in-progress per machine so a rapid double-click doesn't
// kick off two overlapping animations on the same cell.
const pullingNodes = new WeakSet();

function pullReelHTML() {
  // One reel's strip — the full SLOP_SYMBOLS set plus the first symbol
  // repeated at the tail so the seamless-loop math in dashboard.css's
  // slop-spin keyframes still applies (-10.4em = 8 cells * 1.3em).
  let inner = '';
  for (const s of SLOP_SYMBOLS) inner += `<span>${s}</span>`;
  inner += `<span>${SLOP_SYMBOLS[0]}</span>`;
  return `<span class="slop-reel slop-pull-reel"><span class="slop-strip">${inner}</span></span>`;
}

function manualPull(machine) {
  if (pullingNodes.has(machine)) return;
  pullingNodes.add(machine);
  // Stash the original state so we can restore the live cell after
  // the pull. If refresh() rebuilds the whole row before we get there
  // the restore is a no-op (the node is detached).
  const originalHTML = machine.innerHTML;
  const originalStatus = machine.getAttribute('data-status') || '';
  // Pick a random ending combo. Bias slightly toward 7-7-7 for fun.
  // 1-in-12 (≈8 %) is high enough that a curious user who pulls a
  // few times will eventually win, but low enough that the
  // celebration doesn't become routine.
  let combo;
  if (Math.random() < 1 / 12) {
    combo = ['7️⃣', '7️⃣', '7️⃣'];
  } else {
    combo = [0, 1, 2].map(() => SLOP_SYMBOLS[Math.floor(Math.random() * SLOP_SYMBOLS.length)]);
  }
  // Rebuild the cell as three independently-spinning reels. We can't
  // tween a CSS keyframe to a stop on a chosen offset, so the
  // animation is a fast spin followed by a snap-replace.
  machine.setAttribute('data-status', 'pull-spinning');
  machine.innerHTML = pullReelHTML() + pullReelHTML() + pullReelHTML();
  setTimeout(() => {
    if (!machine.isConnected) { pullingNodes.delete(machine); return; }
    machine.setAttribute('data-status', 'pull-stopped');
    machine.innerHTML = combo.map(g => `<span class="slop-reel slop-static">${g}</span>`).join('');
    const isJackpot = combo.every(g => g === '7️⃣');
    if (isJackpot) {
      machine.classList.add('slop-pull-win');
      slopJackpot();
    } else {
      rowBurst(machine);
    }
    setTimeout(() => {
      if (machine.isConnected) {
        machine.classList.remove('slop-pull-win');
        machine.setAttribute('data-status', originalStatus);
        machine.innerHTML = originalHTML;
      }
      pullingNodes.delete(machine);
    }, PULL_HOLD_MS);
  }, PULL_SPIN_MS);
}

// bindSlopMachineClicks — delegated click listener for manual pulls.
// Bound once at boot; no-ops while slop is off.
export function bindSlopMachineClicks() {
  document.addEventListener('click', (e) => {
    if (!isSlopActive()) return;
    if (reducedMotion()) return;
    const machine = e.target.closest('.slop-machine');
    if (!machine) return;
    // Don't pull on an offline / crashed / exited row — the cell
    // means "this agent isn't there", and respinning it would
    // misrepresent state at a glance.
    const status = machine.getAttribute('data-status') || '';
    if (status === 'offline' || status === 'crashed' || status === 'exited') return;
    manualPull(machine);
  });
}

// ─── Working → idle celebration ───────────────────────────────────
// When an agent's status changes from a spinning state (working /
// main_agent_idle) to idle (the jackpot resting state) we want to
// celebrate with a row-level coin burst. refresh() rebuilds the DOM
// every 5 s so an attribute observer wouldn't fire — instead we scan
// .slop-machine[data-conv] elements every 1 s and diff against the
// previous tick. Cheap (a few dozen nodes, attribute reads only) and
// decoupled from the refresh path so refresh.js doesn't have to know
// about us.

const SPINNING_STATUSES = new Set(['working', 'main_agent_idle']);
const prevStatusByConv = new Map();

function scanStatusTransitions() {
  if (!isSlopActive() || reducedMotion()) {
    // Slop off — drop the map so the first tick after re-enable
    // doesn't fire a flood of false transitions for every now-idle
    // agent that was working when slop turned off.
    prevStatusByConv.clear();
    return;
  }
  const seen = new Set();
  const machines = document.querySelectorAll('.slop-machine[data-conv]');
  for (const m of machines) {
    const conv = m.getAttribute('data-conv');
    if (!conv) continue;
    seen.add(conv);
    const status = m.getAttribute('data-status') || '';
    const prev = prevStatusByConv.get(conv);
    if (prev && SPINNING_STATUSES.has(prev) && status === 'idle') {
      // Real transition: this agent just finished spinning.
      // Skip if the cell is currently mid-manual-pull — it's not a
      // live status change, it's the pull's reveal landing on idle's
      // resting symbols.
      if (!pullingNodes.has(m)) {
        rowBurst(m);
        m.classList.add('slop-transition-flash');
        setTimeout(() => {
          if (m.isConnected) m.classList.remove('slop-transition-flash');
        }, 1400);
        noteAgentJackpot(conv);
      }
    }
    prevStatusByConv.set(conv, status);
  }
  // Drop entries for agents that vanished from the DOM (deleted /
  // retired) so the map doesn't grow forever.
  for (const k of prevStatusByConv.keys()) {
    if (!seen.has(k)) prevStatusByConv.delete(k);
  }
}

export function bindSlopStatusWatch() {
  // 1-second tick. refresh() runs every 5 s but a status change is
  // visible the moment refresh paints — a 5 s tick risks landing in
  // the gap and missing a fast transition. 1 s is the cheap-enough
  // compromise and still feels live.
  setInterval(scanStatusTransitions, 1000);
}

// ─── Cursor coin trail ────────────────────────────────────────────
// In slop mode the cursor leaves a fading coin trail. Throttled so
// even a frantic mouse-shake doesn't flood the DOM. Reduced-motion
// suppresses it entirely.

const CURSOR_TRAIL_THROTTLE_MS = 70;
let cursorTrailLast = 0;

export function bindSlopCursorTrail() {
  document.addEventListener('mousemove', (e) => {
    if (!isSlopActive()) return;
    if (reducedMotion()) return;
    const now = performance.now();
    if (now - cursorTrailLast < CURSOR_TRAIL_THROTTLE_MS) return;
    cursorTrailLast = now;
    // Small downward drift so each coin slips off the cursor rather
    // than floating in place. Spread tight — this is a trail, not a
    // burst.
    const el = document.createElement('span');
    el.className = 'slop-trail-coin';
    el.textContent = COIN_EMOJIS[Math.floor(Math.random() * COIN_EMOJIS.length)];
    el.style.left = e.clientX + 'px';
    el.style.top = e.clientY + 'px';
    el.style.setProperty('--dx', ((Math.random() - 0.5) * 24) + 'px');
    el.style.setProperty('--dy', (10 + Math.random() * 16) + 'px');
    document.body.appendChild(el);
    el.addEventListener('animationend', () => el.remove(), { once: true });
  });
}

// ─── Marquee ticker ───────────────────────────────────────────────
// Casino-style scrolling banner under the header (slop-only). All
// lines are joined into one long string separated by ✦ so a single
// CSS scroll animation does the whole work; the JS rewrites the
// joined string whenever fresh snapshot data lands (refresh.js
// dispatches a `tclaude:snapshot` event after each successful poll),
// AND on every animationiteration as a backstop for the lucky-symbol
// minute rollover — the snapshot tick is the primary driver.
//
// Why both: the initial paint happens before the first /api/snapshot
// returns, so lastSnapshot is null and tickerString() would read "0
// agents online". The snapshot event refreshes that within the first
// poll round-trip instead of waiting ~18 s for the first
// animationiteration. To minimise visible mid-scroll jumps we only
// rewrite the text when the computed string actually changed.
//
// "Last jackpot" lives in module state — the snapshot doesn't carry
// jackpots (they're a UI concept) so we remember the most recent
// transition / spawn here.

let lastJackpotAgent = '';
let lastJackpotAt = 0;

function noteAgentJackpot(conv) {
  lastJackpotAt = Date.now();
  if (!conv) return;
  // Resolve the conv-id to a friendly title so the marquee reads
  // "Last jackpot: foo" rather than a hex slice. Fall back to a
  // short id when the snapshot lookup misses.
  let label = conv.slice(0, 8);
  const snap = lastSnapshot;
  if (snap) {
    const a = (snap.agents || []).find(x => x.conv_id === conv);
    if (a && a.title) label = a.title;
  }
  lastJackpotAgent = label;
}

function tickerLines() {
  const snap = lastSnapshot || {};
  const onlineCount = (snap.agents || []).filter(a => a.online).length;
  const lines = [
    `🎰 ${onlineCount} agent${onlineCount === 1 ? '' : 's'} online`,
    '🔔 Click a slot machine to give it a pull',
    '🪙 The house always wins — but the agents work for free',
  ];
  if (lastJackpotAgent && (Date.now() - lastJackpotAt) < 10 * 60 * 1000) {
    lines.splice(1, 0, `💰 Last jackpot: ${lastJackpotAgent}`);
  }
  // A lucky symbol picked from the SLOP_SYMBOLS cycle — rotates
  // every minute so the marquee feels alive on a quiet dashboard.
  const lucky = SLOP_SYMBOLS[Math.floor(Date.now() / 60000) % SLOP_SYMBOLS.length];
  lines.push(`${lucky} Today's lucky symbol: ${lucky}`);
  return lines;
}

function tickerString() {
  return tickerLines().join('   ✦   ');
}

// updateMarqueeText writes the current ticker string into the marquee
// node IFF it changed. The diff is the cheap-but-effective guard
// against snapshot ticks causing pointless mid-scroll jumps when the
// computed string is byte-identical to what's already showing.
function updateMarqueeText(text) {
  const next = tickerString();
  if (text.textContent !== next) text.textContent = next;
}

export function bindSlopMarquee() {
  const text = document.getElementById('slop-marquee-text');
  const track = document.getElementById('slop-marquee-track');
  if (!text || !track) return;
  // Initial paint — gated on lastSnapshot. With no snapshot yet,
  // tickerString() would bake in "🎰 0 agents online" and that bogus
  // count would be visible for the brief window until the first poll
  // returns. The HTML placeholder ("🎰 The slop machine") is more
  // honest in that window, so we leave it alone and rely on the
  // snapshot event below to swap in live data once it arrives. On a
  // mid-session slop toggle lastSnapshot is already populated, so
  // the initial paint runs and the marquee shows real numbers
  // immediately.
  if (lastSnapshot) updateMarqueeText(text);
  // Primary refresh trigger: every successful /api/snapshot. The
  // listener stays bound for the page lifetime; the diff inside
  // updateMarqueeText suppresses redundant writes so steady-state
  // snapshots cause no jump.
  document.addEventListener('tclaude:snapshot', () => {
    if (!isSlopActive()) return;
    updateMarqueeText(text);
  });
  // Backstop: the lucky-symbol minute rolls over independently of
  // snapshot ticks, so refresh at scroll-loop boundaries too. Cheap
  // and visually quiet (a boundary refresh never causes a jump).
  track.addEventListener('animationiteration', () => {
    if (!isSlopActive()) return;
    updateMarqueeText(text);
  });
}
