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
// lists its permissions and how long they would last ("Plan: agent.spawn · for
// 30 min"). Agent text, rendered as text.
export function approvalContext(request) {
  const summary = approvalSummary(request);
  if (summary.kind === 'plan') {
    const plan = planDetails(request);
    const slugs = plan.slugs.length ? plan.slugs : summary.slugs;
    return `Plan: ${slugs.join(', ') || 'temporary permissions'}${plan.duration ? ` · for ${plan.duration}` : ''}`;
  }
  const what = summary.slugs[0] || 'Permission';
  const why = firstLine(request?.body) || (request?.path ? `${request.method ? `${request.method} ` : ''}${request.path}` : '');
  return why ? `${what}: ${why}` : what;
}

// planDetails reads a sudo plan's request body (agentd's "Sudo request:"
// preview: slugs, duration, expires_at, reason) as { slugs, duration, reason,
// expiresAt }: the permissions the plan asks for, how long they would last
// ("30 min", "1 h 30 min"), and the agent's reason. Empty fields when the body
// is not in that shape; the card then falls back to the request's perm.
export function planDetails(request) {
  const out = { slugs: [], duration: '', reason: '', expiresAt: '' };
  const lines = String(request?.body || '').split('\n');
  if (!/^Sudo request:\s*$/.test(lines[0] || '')) return out;
  let inSlugs = false;
  for (const raw of lines.slice(1)) {
    const item = raw.match(/^\s{4}- (.+)$/);
    if (inSlugs && item) { out.slugs.push(item[1].trim()); continue; }
    inSlugs = false;
    const field = raw.match(/^\s{2}(\w+):\s?(.*)$/);
    if (!field) continue;
    const [, key, value] = field;
    if (key === 'slugs') inSlugs = true;
    else if (key === 'duration') out.duration = goDuration(value.trim());
    else if (key === 'expires_at') out.expiresAt = value.trim();
    else if (key === 'reason') out.reason = value.trim();
  }
  return out;
}

// goDuration turns a Go duration ("30m0s", "1h30m0s", "45s") into words
// ("30 min", "1 h 30 min", "45 s"); anything else comes back as it is.
export function goDuration(text) {
  const match = String(text || '').match(/^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$/);
  if (!match || !text) return String(text || '');
  const [, hours, minutes, seconds] = match.map((part, i) => (i ? Number(part || 0) : part));
  const parts = [];
  if (hours) parts.push(`${hours} h`);
  if (minutes) parts.push(`${minutes} min`);
  if (seconds && !hours) parts.push(`${seconds} s`);
  return parts.join(' ') || '0 s';
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
