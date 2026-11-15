// phone-now-model.js -- what the phone's Now view shows, as plain data: the
// one-line usage strip, an access request's one-line context, and which of a
// decision's options fit on its card. No DOM, no rendering: phone-now.js and
// phone-approval.js render these.

import { usageView } from './shell-model.js';
import { approvalSummary } from './companion-model.js';

const SOURCE_NAMES = Object.freeze({ claude: 'Claude', codex: 'Codex', copilot: 'Copilot' });

// usageStrip is Now's usage line: for each subscription (Claude, Codex,
// Copilot) the window closest to its limit, as { key, name, pct, window,
// color }. Costs stay on the Usage page. Empty when no subscription reports
// quota, so Now shows no strip.
export function usageStrip(usage) {
  const view = usageView(usage);
  if (view.na) return [];
  const out = [];
  for (const line of view.lines) {
    const name = SOURCE_NAMES[line.key];
    if (!name) continue;
    const windows = line.tokens.filter((token) => token.kind === 'window' && !token.hidden);
    if (!windows.length) continue;
    const top = windows.reduce((a, b) => (b.pct > a.pct ? b : a));
    out.push({ key: line.key, name, pct: top.pct, window: top.label || 'month', color: top.color });
  }
  return out;
}

// firstLine is text's first non-empty line, trimmed.
function firstLine(text) {
  return String(text || '').split('\n').map((line) => line.trim()).find(Boolean) || '';
}

// approvalContext is an access request in one line: what it grants and, for
// one command, the agent's words about it ("git.push: push PR #4351"). A plan
// lists its permissions. Agent text, rendered as text.
export function approvalContext(request) {
  const summary = approvalSummary(request);
  if (summary.kind === 'plan') return `Plan: ${summary.slugs.join(', ') || 'temporary permissions'}`;
  const what = summary.slugs[0] || 'Permission';
  const why = firstLine(request?.body) || (request?.path ? `${request.method ? `${request.method} ` : ''}${request.path}` : '');
  return why ? `${what}: ${why}` : what;
}

// CARD_OPTIONS is how many of a decision's options its card shows as buttons;
// the rest, the note and Dismiss are behind ⋯.
export const CARD_OPTIONS = 2;

// cardOptions picks the options a decision card shows, in their order, as
// { choice (1-based), label, recommended }: the recommended one always, then
// the first others up to CARD_OPTIONS.
export function cardOptions(decision) {
  const all = (decision?.options || []).map((label, i) => ({ choice: i + 1, label, recommended: i + 1 === decision.recommended }));
  const picked = new Set(all.filter((o) => o.recommended).map((o) => o.choice));
  for (const option of all) {
    if (picked.size >= CARD_OPTIONS) break;
    picked.add(option.choice);
  }
  return all.filter((o) => picked.has(o.choice));
}

// countdown is seconds left as m:ss, or past an hour as 2h 05m.
export function countdown(seconds) {
  if (seconds >= 3600) return `${Math.floor(seconds / 3600)}h ${String(Math.floor((seconds % 3600) / 60)).padStart(2, '0')}m`;
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
}
