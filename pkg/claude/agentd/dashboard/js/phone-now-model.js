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
// says how long it would last, then its permissions ("Plan for 30 min:
// agent.spawn"), so a cut line still shows the time. Agent text, rendered as
// text.
export function approvalContext(request) {
  const summary = approvalSummary(request);
  if (summary.kind === 'plan') {
    const plan = planDetails(request);
    return `Plan${plan.ok ? ` for ${plan.duration}` : ''}: ${summary.slugs.join(', ') || 'temporary permissions'}`;
  }
  const what = summary.slugs[0] || 'Permission';
  const why = firstLine(request?.body) || (request?.path ? `${request.method ? `${request.method} ` : ''}${request.path}` : '');
  return why ? `${what}: ${why}` : what;
}

// planDetails reads a sudo plan as { ok, slugs, duration, expiresAt, reason }:
// the permissions it asks for, how long they would last ("30 min",
// "1 h 30 min"), and the agent's reason. The slugs are the request's perm,
// which agentd checked; the rest is read from its body, agentd's "Sudo
// request:" preview (buildSudoApprovalPreview in sudo.go): slugs, duration,
// expires_at, then reason, always last. The reason is the agent's own text and
// may run over lines that look like fields, so the body is read strictly in
// that order, and everything after "reason: " is the reason: nothing in it can
// stand in for the duration. ok is false when the body is not in that shape;
// the card then shows only the perm.
export function planDetails(request) {
  const out = { ok: false, slugs: approvalSummary(request).slugs, duration: '', expiresAt: '', reason: '' };
  const lines = String(request?.body || '').split('\n');
  if (lines[0] !== 'Sudo request:' || lines[1] !== '  slugs:') return out;
  let i = 2;
  while (i < lines.length && lines[i].startsWith('    - ')) i++;
  const duration = (lines[i] || '').match(/^ {2}duration: (\S+)$/);
  const expires = (lines[i + 1] || '').match(/^ {2}expires_at: (\S+)$/);
  if (!duration || !expires) return out;
  const rest = lines.slice(i + 2).join('\n');
  if (rest && !rest.startsWith('  reason: ')) return out;
  return { ...out, ok: true, duration: goDuration(duration[1]), expiresAt: expires[1], reason: rest.slice('  reason: '.length).trim() };
}

// approvalTiming is an access request's two clocks at now (ms): left, the
// seconds until it is declined unanswered; waitLeft, the seconds until the
// agent stops waiting for an answer (null when the agent named no limit);
// and extendable, whether "+5 min" can still buy time. agentd never lets the
// deadline pass the agent's wait_ceiling, so once the deadline sits on it an
// extension changes nothing and the button goes.
export function approvalTiming(request, now) {
  const deadline = Date.parse(request?.deadline || '');
  const ceiling = Date.parse(request?.wait_ceiling || '');
  const left = Number.isFinite(deadline) ? Math.max(0, Math.round((deadline - now) / 1000)) : null;
  if (!Number.isFinite(ceiling)) return { left, waitLeft: null, extendable: true, ceilingSecs: null };
  const waitLeft = Math.max(0, Math.round((ceiling - now) / 1000));
  // A second of slack: both ends are stamped by agentd to the second.
  const extendable = !Number.isFinite(deadline) || ceiling - deadline > 1000;
  const created = Date.parse(request?.created_at || '');
  const ceilingSecs = Number.isFinite(created) ? Math.round((ceiling - created) / 1000) : null;
  return { left, waitLeft, extendable, ceilingSecs };
}

// ago is how long ago a moment was, short: "now", "20s ago", "3 min ago",
// "2 h ago". Empty for a moment it cannot read.
export function ago(at, now) {
  const then = Date.parse(at || '');
  if (!Number.isFinite(then)) return '';
  const secs = Math.max(0, Math.round((now - then) / 1000));
  if (secs < 5) return 'now';
  if (secs < 60) return `${secs}s ago`;
  if (secs < 3600) return `${Math.floor(secs / 60)} min ago`;
  return `${Math.floor(secs / 3600)} h ago`;
}

// waitWords is how long an agent asked to wait, in words ("2 min", "1 h
// 30 min", "45 s").
export function waitWords(secs) {
  if (!(secs > 0)) return '';
  const h = Math.floor(secs / 3600);
  const m = Math.floor((secs % 3600) / 60);
  const s = secs % 60;
  const parts = [];
  if (h) parts.push(`${h} h`);
  if (m) parts.push(`${m} min`);
  if (s && !h) parts.push(`${s} s`);
  return parts.join(' ');
}

// approvalAsk is what an access request asks for, as plain data for its card
// and its reader: { plan, slugs, meta, why, title }. meta says how much
// ("for 15 min of sudo", "once": it follows "asks"), why is the agent's reason or, for one command,
// what the command is, and title names it in words ("asks for 3
// permissions"). Agent text, rendered as text.
export function approvalAsk(request) {
  const summary = approvalSummary(request);
  const plan = summary.kind === 'plan';
  const details = plan ? planDetails(request) : null;
  const slugs = summary.slugs;
  const meta = plan ? (details.ok ? `for ${details.duration} of sudo` : 'for sudo') : 'once';
  const why = plan
    ? (details.ok ? details.reason : '')
    : (firstLine(request?.body) || '');
  const title = slugs.length === 1 ? `asks for ${slugs[0]}` : `asks for ${slugs.length || 'some'} permissions`;
  return { plan, slugs, meta, why, title, duration: details?.ok ? details.duration : '' };
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
