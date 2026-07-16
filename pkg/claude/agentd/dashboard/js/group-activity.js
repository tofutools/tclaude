// group-activity.js — the deduped "activity bot" indicator.
//
// A group's <summary> header (groups-list.js) and the top bar (the
// #global-activity slot) both want a glanceable answer to "is anything
// happening in here?" — especially when a group is FOLDED, where the
// per-member rows (and their state pills) are hidden behind the
// disclosure triangle. This module turns a member list into a small row
// of animated robot icons: one bot per DISTINCT status present, deduped,
// each carrying a count when more than one member shares that status.
//
// The mapping (operator's framing — "one dancing bot of different style
// per status"):
//   working / main_agent_idle      → a DANCING bot   (green, lively)
//   idle                           → a STILL bot      (amber, calm)
//   awaiting_permission / _input   → an ASKING bot    (❓, blue — needs you)
//   error                          → an ALARMED bot   (💥, red)
//   offline + exit_reason unexpected → a CRASHED bot  (💀, gold)
//   offline (clean)                → a SLEEPING bot    (💤, dim)
//
// Pure + DOM-free on purpose: the same module the browser imports is unit
// tested under Node (jstest/group-activity.test.mjs). It returns structured
// view models; the bounded Preact owners render those models without an
// HTML-string compatibility seam.

// VARIANT_ORDER is both the dedup key set AND the left-to-right / loudest
// -first priority. The first present variant becomes the container's
// `level-*` class (its overall "mood"), and the bots render in this
// order so the most urgent one leads. A lingering CRASHED corpse is
// deliberately ranked BELOW working/idle: a crash is notable (its bot
// still shows) but it should not permanently paint an otherwise-busy
// group with an alarm colour.
export const VARIANT_ORDER = ['error', 'asking', 'working', 'idle', 'crashed', 'offline'];

// Friendly, count-prefixable nouns for the tooltips ("2 working", "1
// awaiting"). 'asking' collapses awaiting_permission + awaiting_input —
// from a glance they mean the same thing ("go look"); the precise split
// still lives in the per-member row pills.
const VARIANT_LABEL = {
  error: 'error',
  asking: 'awaiting',
  working: 'working',
  idle: 'idle',
  crashed: 'crashed',
  offline: 'offline',
};

// The 🧙 wizard theme re-flavours the same tooltips with arcane verbs (the
// operator's ask): hovering a group's bots in wizard mode reads "2 familiars
// channeling · 1 familiar meditating" instead of "2 working · 1 idle". The
// verbs echo the wizard state pill's vocabulary (helpers.js WIZARD_STATE) so
// the two indicators speak the same language; merged 'asking' (permission +
// input) collapses to one generic decree line. Purely cosmetic flair — the
// honest status still lives in every per-row pill's own tooltip, and the
// regular / slop themes keep the plain nouns above.
const WIZARD_VERB = {
  error:   'backfired',
  asking:  'awaiting a decree',
  working: 'channeling',
  idle:    'meditating',
  crashed: 'slain by a grue',
  offline: 'departed',
};

// variantLabel builds the count-prefixed tooltip fragment for one variant in
// the given theme. Regular (and any unknown/blank theme) → the honest noun,
// e.g. "2 working"; 'wizard' → arcane flavour with a pluralised familiar,
// e.g. "2 familiars channeling" / "1 familiar meditating". Pure string work
// over fixed vocab + an integer — no caller input, so it stays injection-safe.
export function variantLabel(variant, n, theme) {
  if (theme === 'wizard') {
    const noun = n === 1 ? 'familiar' : 'familiars';
    return `${n} ${noun} ${WIZARD_VERB[variant] || VARIANT_LABEL[variant]}`;
  }
  return `${n} ${VARIANT_LABEL[variant]}`;
}

// themedSummaryText renders a summary's per-variant breakdown as one tooltip
// line ("2 working · 1 idle", or the wizard flavour). This is the production
// render path for both tooltip sites (groups-list.js calls it for every theme,
// blank included), so the line can re-flavour when the theme flips;
// activitySummary's cached `.summaryText` is the regular-only convenience
// twin. Empty when nothing is present.
export function themedSummaryText(summary, theme) {
  if (!summary || !summary.present || !summary.present.length) return '';
  return summary.present.map(v => variantLabel(v, summary.counts[v], theme)).join(' · ');
}

// Corner glyph layered over the 🤖 face for the variants whose animation
// alone wouldn't read at a glance. working/idle differ by motion (dance
// vs still) and carry no tag, keeping the common cases clean.
const VARIANT_TAG = {
  asking: '❓',
  error: '💥',
  crashed: '💀',
  offline: '💤',
};

// memberVariant classifies a single snapshot member into one VARIANT_ORDER
// key. Offline-first: a dead process's frozen state.status would mislabel
// it (mirrors statePill's reasoning in helpers.js), so we read online +
// exit_reason for the offline cases and only trust state.status while the
// agent is actually online.
export function memberVariant(m) {
  if (!m || !m.online) {
    const reason = (m && m.state && m.state.exit_reason) || '';
    return reason === 'unexpected' ? 'crashed' : 'offline';
  }
  const s = (m.state && m.state.status) || '';
  if (s === 'error') return 'error';
  if (s === 'awaiting_permission' || s === 'awaiting_input') return 'asking';
  if (s === 'working' || s === 'main_agent_idle') return 'working';
  // idle, exited-while-online, or a blank online status all fold into the
  // calm "idle" bot — online but not actively doing anything notable.
  return 'idle';
}

// activitySummary reduces a member list to counts + the ordered set of
// variants worth showing as bots. Clean-offline is suppressed UNLESS the
// whole group is cleanly offline (so a folded, all-asleep group still
// shows one dim 💤 bot instead of an empty chip); crashed always shows
// (it's notable). Returns { total, online, counts, present, level,
// summaryText } — `present` already filtered + ordered, `level` the
// loudest present variant (or 'empty'), `summaryText` the tooltip line.
export function activitySummary(members) {
  const counts = { error: 0, asking: 0, working: 0, idle: 0, crashed: 0, offline: 0 };
  let total = 0;
  let online = 0;
  for (const m of (members || [])) {
    total++;
    if (m && m.online) online++;
    counts[memberVariant(m)]++;
  }
  const liveCount = total - counts.offline; // everything except clean-offline
  const present = VARIANT_ORDER.filter(v => {
    if (counts[v] <= 0) return false;
    if (v === 'offline') return liveCount === 0; // only when the whole group is asleep
    return true;
  });
  const level = present[0] || 'empty';
  // `.summaryText` is the regular-theme breakdown, kept as a convenience field
  // (and asserted by the unit tests). The production render path no longer
  // reads it — both tooltip sites go through themedSummaryText so they can
  // re-flavour per theme — but it stays for tests / any external reader of the
  // summary shape, and reads identically to themedSummaryText(s) for the
  // blank theme.
  const summaryText = present.map(v => variantLabel(v, counts[v])).join(' · ');
  return { total, online, counts, present, level, summaryText };
}

// === Sprite (pixel-art) bot row — the slop-mode default ==================
//
// The same deduped row, but each bot is a pixellab robot sprite animated
// via pure-CSS discrete background-image keyframes (dashboard.css → the
// spr-* classes + /static/sprites/*.png). The POSE carries the status, so
// — unlike the emoji bots — no corner tag glyph is layered on; crashed and
// offline share a single static frame (greyed / toppled by CSS).
const SPRITE_ANIM = {
  working: 'dance',
  asking: 'asking',
  error: 'error',
  idle: 'idle',
  crashed: 'static',
  offline: 'static',
};

// === Wizard-theme bot row — the 🧙 re-skin (body.wizard) =================
//
// The same deduped row, re-skinned for the wizard theme: each bot is a
// fantasy glyph that CARRIES its status (a dancing 🧙 for working, a still
// 🕯️ for idle, a shaking 💥 for a backfired spell, …). It reuses the exact
// .actbot / .actbot-face / .actbot-<variant> structure of the emoji row, so
// the per-variant motion (dance/breathe/tilt/shake) and syncBotAnimations
// apply UNCHANGED — only the glyph differs. Like the sprite row (and unlike
// the emoji row), the glyph itself distinguishes the states, so no corner
// tag is layered on. Emitted alongside the regular + slop wrappers and
// CSS-swapped by body.wizard — the same "always emit, theme picks" trick as
// wizardPill in helpers.js. Glyphs mirror wizardPill's DnD flavour for
// consistency, but working leads with the theme's mascot 🧙 (it's the icon
// the operator asked to see dance).
const WIZARD_FACE = {
  working: '🧙',
  idle:    '🕯️',
  asking:  '📜',
  error:   '💥',
  crashed: '💀',
  offline: '🪦',
};

// === Wizard-theme SPRITE row — the pixel-art opt-in (body.wizard) =========
//
// The wizard theme's bots default to the glyph row above, but you can opt into
// pixellab WIZARD sprites (a grumpy spellcaster) via config
// dashboard.activity_bots.wizard = "sprites" — mirroring the regular/slop
// emoji-vs-sprites choice. Same status→pose mapping as the slop robots, driven
// by the same discrete background-image keyframes (dashboard.css → spr-wiz-* +
// /static/sprites/wiz_*.png). The wizard sheets have their OWN per-pose
// dimensions, so the bots carry an .actbot-wiz marker that swaps in wizard
// aspect-ratios; crashed + offline share the static standing frame (greyed /
// toppled by CSS). Tooltips speak the same arcane vocabulary as the glyph row.
const WIZ_SPRITE_ANIM = {
  working: 'wiz-cast',
  asking: 'wiz-ask',
  error: 'wiz-error',
  idle: 'wiz-idle',
  crashed: 'wiz-static',
  offline: 'wiz-static',
};

// activityBotView is the structured replacement for the retired HTML bot
// builders. Fixed vocabulary becomes plain properties; caller-controlled
// strings never enter the model.
export function activityBotView(variant, count, style, wizard = false) {
  const sprite = style === 'sprites';
  const theme = wizard ? 'wizard' : undefined;
  const animation = sprite
    ? (wizard ? WIZ_SPRITE_ANIM : SPRITE_ANIM)[variant] || (wizard ? 'wiz-static' : 'static')
    : '';
  return {
    key: variant,
    variant,
    count,
    title: variantLabel(variant, count, theme),
    className: `actbot${sprite ? ' actbot-sprite' : ''}${sprite && wizard ? ' actbot-wiz' : ''} actbot-${variant}`,
    faceClassName: sprite ? `actbot-spr spr-${animation}` : 'actbot-face',
    face: sprite ? '' : wizard ? (WIZARD_FACE[variant] || '🧙') : '🤖',
    tag: !sprite && !wizard ? (VARIANT_TAG[variant] || '') : '',
  };
}

// activityModeViews returns the three theme rows with stable mode/bot keys.
// All enabled modes are emitted together and CSS selects the active one, so a
// wizard toggle never remounts an animation node.
export function activityModeViews(summary, configured = {}) {
  if (!summary?.present?.length) return [];
  const specs = [
    { key: 'regular', className: 'ga-regular', style: configured.regular || 'emoji', wizard: false },
    { key: 'slop', className: 'ga-slop', style: configured.slop || 'sprites', wizard: false },
    { key: 'wizard', className: 'ga-wizard', style: configured.wizard || 'emoji', wizard: true },
  ];
  return specs.filter((mode) => mode.style !== 'off').map((mode) => ({
    ...mode,
    level: summary.level,
    title: themedSummaryText(summary, mode.wizard ? 'wizard' : undefined),
    bots: summary.present.map((variant) => activityBotView(
      variant, summary.counts[variant], mode.style, mode.wizard,
    )),
  }));
}

// aggregateActivity flattens several member lists (every group + the
// ungrouped bucket) into one summary — the backing for the global top-bar
// indicator. Dedups by conv_id: an agent that belongs to several groups
// appears in EACH group's member list, so without this the global counts
// would multiply that agent by its group count. Members with no conv_id
// (e.g. test fixtures) are never deduped — they all pass through.
export function aggregateActivity(memberLists) {
  const all = [];
  const seen = new Set();
  for (const list of (memberLists || [])) {
    for (const m of (list || [])) {
      const id = m && m.conv_id;
      if (id && seen.has(id)) continue;
      if (id) seen.add(id);
      all.push(m);
    }
  }
  return activitySummary(all);
}
