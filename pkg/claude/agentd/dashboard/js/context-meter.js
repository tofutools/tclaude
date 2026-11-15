// context-meter.js -- an agent's context window as the desktop board shows it
// beside the state pill: five segments lit bottom-up in green, yellow and red
// bands, its exact figure in the title, a wizard-mode mana twin, and dimmed
// while the harness has not reported usage. The phone's agent rows and cards
// wear the same meter; contextFigure is the exact figure in words.

import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

export function fmtTokens(value) {
  const n = Number(value) || 0;
  if (n >= 1000000) return `${(n / 1000000).toFixed(n % 1000000 === 0 ? 0 : 1)}M`;
  if (n >= 1000) return `${Math.round(n / 1000)}k`;
  return String(n);
}

export function ContextMeter({ state }) {
  const pct = Math.max(0, Math.min(100, Number(state?.context_pct || 0)));
  const configuredMax = Number(state?.context_window_max || 0);
  const known = pct > 0 || configuredMax > 0 || Number(state?.context_window_size || 0) > 0;
  const filled = pct > 0 ? Math.min(5, Math.max(1, Math.round(pct / 20))) : 0;
  const total = Number(state?.tokens_input || 0) + Number(state?.tokens_output || 0);
  // The window here is already the effective one and context_pct is measured
  // against it, so the regular usage figures are sufficient on their own.
  const win = configuredMax > 0 ? configuredMax : Number(state?.context_window_size || 0);
  // 'reported' is a limit the local harness disclosed; 'catalog' came from
  // Copilot's authenticated remote model catalog; 'assumed' is tclaude's
  // static per-model fallback.
  const source = state?.context_window_source === 'configured' ? ' (configured cap)'
    : state?.context_window_source === 'reported' ? ' (reported cap)'
      : state?.context_window_source === 'catalog' ? ' (catalog cap)'
        : state?.context_window_source === 'assumed' ? ' (assumed cap)' : '';
  const regularTitle = !known ? 'context window: usage not reported yet'
    : win > 0 && total > 0 ? `context: ${fmtTokens(total)} / ${fmtTokens(win)} tokens${source} — ${Math.round(pct)}%`
      : `context: ${Math.round(pct)}% full`;
  const wizardTitle = !known ? '🔮 Mana reserves: not yet divined'
    : win > 0 && total > 0 ? `🔮 Mana: ${fmtTokens(total)} / ${fmtTokens(win)} channeled${source} — ${Math.round(pct)}%`
      : `🔮 Mana: ${Math.round(pct)}% channeled`;
  const segments = [...Array(5)].map((_, i) => {
    const band = i >= 4 ? 'red' : i >= 2 ? 'yellow' : 'green';
    return html`<span key=${i} class=${`ctx-seg${i < filled ? ` lit-${band}` : ''}`}></span>`;
  });
  const unknown = known ? '' : ' ctx-unknown';
  // An image to a screen reader, named by its title: its segments say nothing.
  // The theme hides one twin (display: none), so only the other is read.
  return html`<span class=${`ctx-meter ctx-regular${unknown}`} role="img" aria-label=${regularTitle} title=${regularTitle}>${segments}</span><span class=${`ctx-meter ctx-mana${unknown}`} role="img" aria-label=${wizardTitle} title=${wizardTitle}>${segments}</span>`;
}

// contextFigure is an agent's context in words, "ctx 91% · 182k / 200k", or
// "ctx 91%" when the token counts are not reported; '' when nothing is.
export function contextFigure(state) {
  const raw = state?.context_pct;
  const pct = Number(raw);
  if (raw === null || raw === undefined || !Number.isFinite(pct)) return '';
  const total = Number(state?.tokens_input || 0) + Number(state?.tokens_output || 0);
  const max = Number(state?.context_window_max || 0);
  const win = max > 0 ? max : Number(state?.context_window_size || 0);
  const figure = `ctx ${Math.round(pct)}%`;
  return win > 0 && total > 0 ? `${figure} · ${fmtTokens(total)} / ${fmtTokens(win)}` : figure;
}
