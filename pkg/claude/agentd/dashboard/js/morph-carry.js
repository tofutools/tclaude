// morph-carry.js — the Morph dialog's "History carry-over" section: how much
// of the history a cross-harness morph carries, what gives way first, and how
// a large cut is handled. Every number shown comes from the daemon's dry run
// (the same plan the morph applies); nothing here estimates on its own.
import { h } from 'preact';
import htm from 'htm';
import { Button, Input, Select } from './ui-components.js';
import { Tabs } from './ui-tabs.js';

const html = htm.bind(h);

// CATEGORIES are the kinds of carried content the transcript layer can
// shorten on their own. Reasoning and harness context are never carried.
export const CATEGORIES = [['chat', 'Chat text'], ['tool_calls', 'Tool calls'], ['tool_output', 'Tool output']];

export const CUT_FIRST = [['oldest', 'Oldest'], ['tool_output', 'Tool output'], ['tool_calls', 'Tool calls'], ['chat', 'Chat']];

export const METHODS = [
  ['', 'Auto: the source writes a summary when ≥25% is cut'],
  ['summary', 'Always ask the source for a summary first'],
  ['truncate', 'Truncate only: no summary, full-transcript pointer'],
  ['context_file', 'Pointer to the saved history (context file)'],
  ['handoff', 'Handoff only: the agent writes a handoff, no history'],
];

const BUDGETS = [['auto', 'Auto'], ['all', 'All'], ['tokens', 'Tokens']];

export function emptyCarry() {
  return { window: '', budget: 'auto', budgetTokens: '', cutFirst: 'oldest', method: '', keep: {} };
}

// parseTokens reads "600k", "1m", "0.5M" or "300000" as a token count; ''
// is null and anything else NaN.
export function parseTokens(raw) {
  const s = String(raw ?? '').trim().toLowerCase().replace(/[_\s]/g, '');
  if (!s) return null;
  const m = /^(\d+(?:\.\d+)?)([km]?)$/.exec(s);
  if (!m) return NaN;
  if (!m[2]) return m[1].includes('.') ? NaN : Number(m[1]);
  // "4.1m" is 4099999.999… in floating point, so round the scaled value.
  return Math.round(Number(m[1]) * (m[2] === 'k' ? 1e3 : 1e6));
}

// carryInvalid names the first field the daemon would refuse, or ''.
export function carryInvalid(c) {
  const w = parseTokens(c.window);
  if (Number.isNaN(w) || (w !== null && (w < 1024 || w > 10_000_000))) return 'Target window: a token count such as 600000 or 600k';
  if (c.budget === 'tokens') {
    const b = parseTokens(c.budgetTokens);
    if (b === null || Number.isNaN(b) || b < 10_000 || b > 10_000_000) return 'Carry budget: a token count from 10k to 10m';
  }
  return '';
}

// carryBody is the request fields for a carry-over choice; {} is automatic.
export function carryBody(c) {
  const body = {};
  if (!c || carryInvalid(c)) return body;
  const w = parseTokens(c.window);
  if (w) body.target_window_tokens = w;
  if (c.budget === 'all') body.history_budget = 'all';
  if (c.budget === 'tokens') body.history_budget = String(parseTokens(c.budgetTokens));
  if (c.cutFirst && c.cutFirst !== 'oldest') body.history_cut_first = c.cutFirst;
  if (c.method) body.history = c.method;
  const keep = Object.fromEntries(Object.entries(c.keep || {}).filter(([, v]) => Number.isInteger(v)));
  if (Object.keys(keep).length) body.history_keep = keep;
  return body;
}

export function fmtTokens(n) {
  if (!(n > 0)) return '0';
  if (n >= 1e6 && n % 1e5 === 0) return `${n / 1e6}m`;
  if (n >= 1000) return `${Math.round(n / 1000)}k`;
  return String(n);
}

const labelOf = (cat) => CATEGORIES.find(([k]) => k === cat)?.[1] || cat;

// carryWarnings are the dry run's warnings: they inform, never block.
export function carryWarnings(hist) {
  if (!hist) return [];
  const out = [];
  const w = hist.context_window_tokens;
  if (hist.over_window) out.push(`The carried ~${fmtTokens(hist.carried_estimated_tokens)} is more than the ${fmtTokens(w)} window: the agent may fail or compact on its first turn.`);
  else if (hist.over_headroom) out.push(`The carried ~${fmtTokens(hist.carried_estimated_tokens)} is ${Math.round(100 * hist.carried_estimated_tokens / w)}% of the ${fmtTokens(w)} window, above the usual 60% headroom: the agent may compact soon.`);
  return out;
}

// SHOWN_LOSSES have rows of their own below the History row.
const SHOWN_LOSSES = new Set(['truncated_for_budget', 'reasoning_dropped', 'tool_records_stubbed']);

// carryRows are the "What carries over" rows for a translated history.
export function carryRows(hist) {
  if (!hist) return [];
  let text = `translated ${hist.source} → ${hist.target}: ${hist.carried_entries ?? '?'} of ${hist.source_entries ?? '?'} entries carried`;
  if (hist.carried_estimated_tokens > 0) text += `; ~${fmtTokens(hist.carried_estimated_tokens)} of ~${fmtTokens(hist.source_estimated_tokens)} tokens`;
  if (hist.context_window_tokens > 0) {
    text += hist.window_stated
      ? `; window ${fmtTokens(hist.context_window_tokens)} (stated${hist.detected_window_tokens > 0 ? `; detected ${fmtTokens(hist.detected_window_tokens)}` : ''})`
      : `; window ${fmtTokens(hist.context_window_tokens)} (${hist.context_window_source})`;
    text += hist.carry_all ? ', no budget cut' : `, budget ${fmtTokens(hist.budget_tokens)}${hist.budget_source === 'stated' ? ' (stated)' : ''}`;
  }
  const lost = (hist.items || []).filter((i) => !SHOWN_LOSSES.has(i.kind)).map((i) => `${i.kind} ×${i.count}`).join(', ');
  if (lost) text += ` (${lost})`;
  const rows = [['≈', 'History', text]];
  for (const u of hist.categories || []) {
    if (!(u.source_tokens > 0)) continue;
    const share = u.keep_percent != null ? `newest ${u.keep_percent}% whole${hist.cut_first === u.category ? ', cut first' : ''}; ` : '';
    rows.push(u.shortened > 0
      ? ['≈', labelOf(u.category), `${share}${u.shortened} older shortened to one-line stubs (~${fmtTokens(u.source_tokens)} → ~${fmtTokens(u.carried_tokens)})`]
      : ['✓', labelOf(u.category), `${share}kept whole (~${fmtTokens(u.carried_tokens)})`]);
  }
  const omitted = (hist.items || []).find((i) => i.kind === 'truncated_for_budget');
  if (omitted) rows.push(['≈', 'Oldest turns', `${omitted.count} entries omitted to fit; the full transcript keeps them`]);
  if ((hist.items || []).some((i) => i.kind === 'reasoning_dropped')) rows.push(['✗', 'Reasoning', 'not carried (provider-bound)']);
  return rows;
}

// Estimate is a stacked bar of what each category carries, against the
// window (and the budget, when there is one).
function Estimate({ hist }) {
  const cats = (hist.categories || []).filter((u) => u.carried_tokens > 0);
  const carried = hist.carried_estimated_tokens || 0;
  const window = hist.context_window_tokens || 0;
  const scale = Math.max(window, carried, 1);
  const pct = (n) => `${Math.min(100, (100 * n) / scale).toFixed(2)}%`;
  const budget = !hist.carry_all && hist.budget_tokens > 0 && hist.budget_tokens < scale;
  return html`<div class="lt-carry-est" data-lt-carry-estimate="">
    <div class="lt-carry-est-line"><b>Estimate</b> (dry run): ~${fmtTokens(carried)} of ~${fmtTokens(hist.source_estimated_tokens)} carried · ${hist.carry_all ? 'everything' : `budget ${fmtTokens(hist.budget_tokens)}`}${window > 0 ? ` · window ${fmtTokens(window)}` : ' · window unknown'}</div>
    <div class="lt-carry-bar" role="img" aria-label=${`~${fmtTokens(carried)} tokens carried of a ${fmtTokens(window)} window`}>
      ${cats.map((u) => html`<span key=${u.category} class=${`lt-cat-${u.category}`} style=${`width:${pct(u.carried_tokens)}`}></span>`)}
      ${budget && html`<i class="lt-carry-budget" style=${`left:${pct(hist.budget_tokens)}`} title="budget"></i>`}
      ${carried > window && html`<i class="lt-carry-window" style=${`left:${pct(window)}`} title="window"></i>`}
    </div>
    <div class="lt-carry-legend">${cats.map((u) => html`<span key=${u.category}><i class=${`lt-cat-${u.category}`}></i>${labelOf(u.category)} ${fmtTokens(u.carried_tokens)}</span>`)}</div>
  </div>`;
}

// CarryOver is the section's controls. carry is the choice, hist the latest
// dry run's history report.
export function CarryOver({ carry, onChange, hist, disabled, advanced, onAdvanced }) {
  const set = (patch) => onChange({ ...carry, ...patch });
  const setKeep = (cat, v) => {
    const keep = { ...carry.keep };
    if (v === null) delete keep[cat]; else keep[cat] = v;
    set({ keep });
  };
  const usage = Object.fromEntries((hist?.categories || []).map((u) => [u.category, u]));
  const detected = hist ? (hist.window_stated ? hist.detected_window_tokens : hist.context_window_tokens) : 0;
  const detectedSource = hist ? (hist.window_stated ? hist.detected_window_source : hist.context_window_source) : '';
  const invalid = carryInvalid(carry);
  const overridden = Object.keys(carryBody(carry)).length > 0;
  return html`<details class="lt-carry-over" open data-lt-carry-over="">
    <summary>History carry-over <span class=${`ui-tag lt-carry-tag${overridden ? ' ui-tag-warn' : ''}`}>${overridden ? 'overridden' : hist ? `auto: ${hist.kept_percent ?? 100}% kept` : 'auto'}</span></summary>
    <label class="lt-row"><span class="lt-k">Target window</span>
      <${Input} size="sm" id="lt-carry-window" class="lt-carry-num" value=${carry.window} disabled=${disabled}
        placeholder=${detected ? fmtTokens(detected) : 'tokens'} aria-label="Target context window in tokens"
        onInput=${(e) => set({ window: e.currentTarget.value })} />
      <span class="lt-carry-note">${carry.window ? html`stated by you · detected ${fmtTokens(detected)} <${Button} size="sm" variant="ghost" onClick=${() => set({ window: '' })}>reset</${Button}>` : detectedSource ? `detected · ${detectedSource}` : ''}</span></label>
    <div class="lt-row"><span class="lt-k">Carry budget</span>
      <${Tabs} variant="seg" kind="radio" label="Carry budget" value=${carry.budget} onSelect=${(k) => !disabled && set({ budget: k })}
        items=${BUDGETS.map(([key, label]) => ({ key, label, disabled }))} />
      ${carry.budget === 'tokens' && html`<${Input} size="sm" id="lt-carry-budget" class="lt-carry-num" value=${carry.budgetTokens} disabled=${disabled}
        placeholder="300k" aria-label="Carry budget in tokens" onInput=${(e) => set({ budgetTokens: e.currentTarget.value })} />`}
      <span class="lt-carry-note">${carry.budget === 'auto' ? '60% of the window' : carry.budget === 'all' ? 'no cut' : ''}</span></div>
    <div class="lt-row"><span class="lt-k">Cut first from</span>
      <${Tabs} variant="seg" kind="radio" label="Cut first from" value=${carry.cutFirst} onSelect=${(k) => !disabled && set({ cutFirst: k })}
        items=${CUT_FIRST.map(([key, label]) => ({ key, label, disabled }))} /></div>
    <label class="lt-row"><span class="lt-k">When much is cut</span>
      <${Select} size="sm" id="lt-carry-method" value=${carry.method} disabled=${disabled} onChange=${(e) => set({ method: e.currentTarget.value })}>
        ${METHODS.map(([v, label]) => html`<option key=${v} value=${v}>${label}</option>`)}
      </${Select}></label>
    <details class="lt-carry-adv" open=${advanced} onToggle=${(e) => onAdvanced?.(e.currentTarget.open)}>
      <summary>Advanced: keep per category (newest first; the older rest becomes stubs that point at the full transcript)</summary>
      ${CATEGORIES.map(([cat, label]) => {
        const v = carry.keep?.[cat];
        const u = usage[cat];
        const shown = Number.isInteger(v) ? v : (u?.keep_percent ?? 100);
        return html`<div key=${cat} class="lt-cat-row">
          <span>${label}</span>
          <input type="range" class="lt-carry-range" min="0" max="100" step="5" value=${shown} disabled=${disabled}
            aria-label=${`${label}: share kept whole`} data-lt-keep=${cat} onInput=${(e) => setKeep(cat, Number(e.currentTarget.value))} />
          <span class="lt-cat-v">${Number.isInteger(v) ? `${v}%` : 'auto'}</span>
          <span class="lt-cat-t">${u ? `${fmtTokens(u.source_tokens)} → ${fmtTokens(u.carried_tokens)}${u.shortened ? ` · ${u.shortened} stubbed` : ''}` : ''}
            ${Number.isInteger(v) && html` <${Button} size="sm" variant="ghost" onClick=${() => setKeep(cat, null)}>auto</${Button}>`}</span>
        </div>`;
      })}
      <div class="lt-cat-row lt-cat-fixed"><span>Reasoning</span><span>never carried: provider-bound</span></div>
      <div class="lt-cat-row lt-cat-fixed"><span>Harness context</span><span>never carried: the new harness injects its own</span></div>
    </details>
    ${invalid && html`<div class="fa-danger" role="alert">${invalid}</div>`}
    ${hist && html`<${Estimate} hist=${hist} />`}
    ${carryWarnings(hist).map((w) => html`<div key=${w} class="lt-carry-warn" role="status">⚠ ${w} Not blocked.</div>`)}
  </details>`;
}
