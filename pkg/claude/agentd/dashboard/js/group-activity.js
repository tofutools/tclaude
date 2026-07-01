// group-activity.js — the deduped "activity bot" indicator.
//
// A group's <summary> header (render.js) and the top bar (the
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
// tested under Node (jstest/group-activity.test.mjs). The HTML builders
// below emit ONLY fixed class names, emoji and integer counts — never any
// caller-supplied string — so the output needs no escaping and stays
// injection-safe by construction. Group names (used in the global
// tooltip) are joined in render.js and assigned via the .title DOM
// property, which never parses HTML.

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
// line ("2 working · 1 idle", or the wizard flavour). activitySummary caches
// the regular breakdown in `.summaryText`; callers reach for this when the
// active theme may re-flavour it (render.js, per theme). Empty when nothing
// is present.
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
  // `.summaryText` caches the regular-theme breakdown (the common case + what
  // the unit tests assert); themedSummaryText re-derives it for the wizard
  // flavour when a caller needs it.
  const summaryText = present.map(v => variantLabel(v, counts[v])).join(' · ');
  return { total, online, counts, present, level, summaryText };
}

// botHTML renders one bot for a single variant. `n` (a number) feeds the
// count badge (shown only when >1) and the per-bot tooltip; `theme` flavours
// that tooltip (wizard vs plain). No string interpolation of caller input —
// safe to drop into innerHTML.
function botHTML(variant, n, theme) {
  const tagGlyph = VARIANT_TAG[variant];
  const tag = tagGlyph ? `<span class="actbot-tag">${tagGlyph}</span>` : '';
  const count = n > 1 ? `<span class="actbot-count">${n}</span>` : '';
  const tip = variantLabel(variant, n, theme);
  return `<span class="actbot actbot-${variant}" title="${tip}" aria-label="${tip}">`
    + `<span class="actbot-face">🤖</span>${tag}${count}</span>`;
}

// activityBotsHTML emits the inner bot row for a summary (no wrapper).
// Returns '' when there's nothing to show.
export function activityBotsHTML(summary, theme) {
  if (!summary || !summary.present.length) return '';
  return summary.present.map(v => botHTML(v, summary.counts[v], theme)).join('');
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

function spriteBotHTML(variant, n, theme) {
  const anim = SPRITE_ANIM[variant] || 'static';
  const count = n > 1 ? `<span class="actbot-count">${n}</span>` : '';
  const tip = variantLabel(variant, n, theme);
  return `<span class="actbot actbot-sprite actbot-${variant}" title="${tip}" aria-label="${tip}">`
    + `<span class="actbot-spr spr-${anim}"></span>${count}</span>`;
}

// spriteBotsHTML emits the inner sprite row for a summary (no wrapper).
export function spriteBotsHTML(summary, theme) {
  if (!summary || !summary.present.length) return '';
  return summary.present.map(v => spriteBotHTML(v, summary.counts[v], theme)).join('');
}

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

function wizardBotHTML(variant, n) {
  const glyph = WIZARD_FACE[variant] || '🧙';
  const count = n > 1 ? `<span class="actbot-count">${n}</span>` : '';
  // The wizard row is inherently the 🧙 theme, so its tooltips always speak
  // the arcane vocabulary ("2 familiars channeling"), reusing PR #678's
  // variantLabel — no theme arg to thread, it's fixed for this wrapper.
  const tip = variantLabel(variant, n, 'wizard');
  return `<span class="actbot actbot-${variant}" title="${tip}" aria-label="${tip}">`
    + `<span class="actbot-face">${glyph}</span>${count}</span>`;
}

// wizardBotsHTML emits the inner wizard-glyph row for a summary (no wrapper).
export function wizardBotsHTML(summary) {
  if (!summary || !summary.present.length) return '';
  return summary.present.map(v => wizardBotHTML(v, summary.counts[v])).join('');
}

// styledBotsHTML renders the inner bot row for a summary in one of the
// three styles. 'off' (or an empty summary) → ''. The single switchboard
// both render call sites go through, so emoji/sprites stay interchangeable.
// `theme` flavours the per-bot tooltips (wizard vs plain).
export function styledBotsHTML(summary, style, theme) {
  if (!summary || style === 'off' || !summary.present.length) return '';
  return style === 'sprites' ? spriteBotsHTML(summary, theme) : activityBotsHTML(summary, theme);
}

// groupActivityHTML is the one-shot helper render.js drops into a group
// <summary>. It emits a regular-mode wrapper (.ga-regular), a slop-mode
// wrapper (.ga-slop) AND a wizard-mode wrapper (.ga-wizard), each rendered
// in its configured style — CSS shows exactly one per active theme
// (body.slop / body.wizard), so toggling a theme swaps the visual with NO
// re-render (the same trick the slot-machine / wizard state pill uses).
//
// The theme↔wrapper mapping is now 1:1 (regular→.ga-regular, slop→.ga-slop,
// wizard→.ga-wizard), so each wrapper carries a FIXED-flavour tooltip: the
// plain nouns for regular/slop, and the arcane "N familiars channeling"
// (themedSummaryText, from PR #678) for the wizard row. No live-theme arg is
// needed here — the visible wrapper is always correctly flavoured, even the
// instant a theme flips, since its title is baked at render time and CSS
// just reveals it. wizardStyle is on/off (the wizard row has a single visual
// — its glyphs); an absent/'off' value drops the wizard wrapper (so the
// pre-4th-arg callers keep the old two-wrapper output). Returns '' when
// EVERY mode resolves to nothing (off / empty group).
export function groupActivityHTML(members, regularStyle, slopStyle, wizardStyle) {
  const s = activitySummary(members);
  if (!s.present.length) return '';
  const wrap = (cls, inner, tip) =>
    inner
      ? `<span class="${cls} level-${s.level}" title="${tip}">${inner}</span>`
      : '';
  const reg = wrap('ga-regular', styledBotsHTML(s, regularStyle), s.summaryText);
  const slop = wrap('ga-slop', styledBotsHTML(s, slopStyle), s.summaryText);
  const wiz = wrap('ga-wizard',
    (wizardStyle && wizardStyle !== 'off') ? wizardBotsHTML(s) : '',
    themedSummaryText(s, 'wizard'));
  if (!reg && !slop && !wiz) return '';
  return `<span class="group-activity">${reg}${slop}${wiz}</span>`;
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
