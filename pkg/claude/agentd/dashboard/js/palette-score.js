// palette-score.js — the command palette's pure ranking logic, split out
// from palette.js so it can be unit-tested in isolation: it touches no
// DOM and imports nothing, so a node:test suite imports it directly while
// the DOM-bound rest of palette.js stays out of the test runner.
//
// The ranking has subtle rules — a prefix beats a mid-word hit, a curated
// keyword phrase beats scattered tokens, and synonyms map "hide"→"unfocus"
// / "show"→"focus" — which is exactly the kind of logic worth pinning with
// tests (see palette-score.test.mjs).

// SYNONYMS maps a word the human might TYPE to the word(s) the commands
// actually use, so search reads intent rather than literal strings: the
// bulk window ops are labelled "Unfocus all" / "Focus all", but a human
// thinks "hide" / "show". Bidirectional within each pair so either
// spelling finds the other. Extend by adding pairs here.
export const SYNONYMS = {
  hide: ['unfocus'],
  unfocus: ['hide'],
  show: ['focus'],
  focus: ['show'],
};

// scoreMatch ranks how directly a (possibly synonym-substituted) query
// answers one command's text. Ladder, high→low:
//
//   100 exact label
//    90 label starts with the query
//    80 query begins at a word boundary in the label
//    60 query anywhere in the label
//    50 query as a contiguous phrase in the hint / keywords
//    40 all tokens present (scattered) in the label
//    20 all tokens present (scattered) anywhere
//     0 no match (filtered out)
//
// The prefix / word-start tiers are what put "Focus all windows" (a
// prefix hit) above "Unfocus all windows" (query only mid-word,
// "un|focus all") for the query "focus all". hay includes the label, but
// the label tiers return first, so the phrase / token-in-hay tiers only
// fire on a hint/keyword-only hit.
export function scoreMatch(q, tokens, label, hay) {
  if (label === q) return 100;
  if (label.startsWith(q)) return 90;
  if (label.includes(' ' + q)) return 80;          // query at a non-first word start
  if (label.includes(q)) return 60;                // query somewhere in the label
  if (hay.includes(q)) return 50;                  // query as a phrase in hint/keywords
  if (tokens.every(t => label.includes(t))) return 40;
  if (tokens.every(t => hay.includes(t))) return 20;
  return 0;
}

// expandQuery returns the query plus every synonym-substituted variant —
// the cross-product of each token with its synonyms (the token itself
// first). "hide all" → ["hide all", "unfocus all"]; "show all" →
// ["show all", "focus all"]. Capped so a pathological query can't blow up
// the cross-product.
export function expandQuery(q) {
  const tokens = q.split(/\s+/);
  let variants = [[]];
  for (const t of tokens) {
    const opts = [t, ...(SYNONYMS[t] || [])];
    const next = [];
    for (const v of variants) {
      for (const o of opts) next.push(v.concat(o));
    }
    variants = next;
    if (variants.length > 16) { variants = variants.slice(0, 16); break; }
  }
  const seen = new Set();
  const out = [];
  for (const v of variants) {
    const s = v.join(' ');
    if (!seen.has(s)) { seen.add(s); out.push(s); }
  }
  return out;
}

// scoreCommand is the best score across the query's synonym variants, so
// "hide all" scores "Unfocus all windows" as if the human had typed
// "unfocus all" (its 90 prefix hit) instead of the weaker literal match.
export function scoreCommand(q, label, hay) {
  let best = 0;
  for (const variant of expandQuery(q)) {
    best = Math.max(best, scoreMatch(variant, variant.split(/\s+/), label, hay));
    if (best === 100) break;
  }
  return best;
}

// rankCommands filters + orders the command list for a query (empty → the
// whole list in build order). Each command exposes { label, hint?,
// keywords? }. Ties keep build order via a stable sort (modern engines).
export function rankCommands(commands, query) {
  const q = (query || '').trim().toLowerCase();
  if (!q) return commands.slice();
  const scored = [];
  for (const cmd of commands) {
    const label = cmd.label.toLowerCase();
    const hay = (cmd.label + ' ' + (cmd.hint || '') + ' ' + (cmd.keywords || '')).toLowerCase();
    const score = scoreCommand(q, label, hay);
    if (score > 0) scored.push({ cmd, score });
  }
  scored.sort((a, b) => b.score - a.score);
  return scored.map(s => s.cmd);
}
