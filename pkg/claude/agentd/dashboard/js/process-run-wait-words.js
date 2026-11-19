// process-run-wait-words.js -- a waiting wait step in words, for the run
// views (process-run-waits.js), the run graphs and the phone's run cards: how
// long it has left, or which check it is on and when the next one is. Pure:
// jstest/process-run-waits.test.mjs exercises it directly.

// waitingCommands are a run view's waits: timers, and checks that are out.
export function waitingCommands(view) {
  return (view?.commands || []).filter((command) => command.wait && (command.kind === 'wait' || command.wait.checking));
}

// leftWords says a time left: "2d 3h", "2h 13m", "4m 05s", "12s".
export function leftWords(ms) {
  if (!Number.isFinite(ms) || ms <= 0) return '0s';
  const s = Math.ceil(ms / 1000);
  const d = Math.floor(s / 86400);
  const hours = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (d) return `${d}d ${hours}h`;
  if (hours) return `${hours}h ${m}m`;
  if (m) return `${m}m ${String(sec).padStart(2, '0')}s`;
  return `${sec}s`;
}

// atWords says when, short: "14:00" today, else "Thu 14:00", or a date
// further off.
export function atWords(at, now = new Date()) {
  const when = new Date(at);
  if (Number.isNaN(when.getTime())) return '';
  const time = when.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  if (when.toDateString() === now.toDateString()) return time;
  if (Math.abs(when - now) < 6 * 86400000) return `${when.toLocaleDateString([], { weekday: 'short' })} ${time}`;
  return `${when.toLocaleDateString([], { month: 'short', day: 'numeric' })} ${time}`;
}

// lastWords says a check's last answer in words.
function lastWords(last) {
  if (!last) return '';
  if (last === 'yes') return 'yes';
  if (last === 'no') return 'not yet';
  if (last === 'skipped') return 'skipped';
  return last.replace(/^failed: /, 'failed: ');
}

// checkCount is "check 3 of 20" (the one coming next), or "3 checks made".
function checkCount(wait, { next = true } = {}) {
  const n = (wait.check?.checks || 0) + (next ? 1 : 0);
  if (wait.maxChecks) return `check ${Math.min(n, wait.maxChecks)} of ${wait.maxChecks}`;
  return next ? `check ${n}` : `${wait.check?.checks || 0} checks made`;
}

// waitLine is a waiting step's line at now, in words.
export function waitLine(command, now = Date.now()) {
  const wait = command?.wait || {};
  if (wait.failure) return { text: `can't work out its time: ${wait.failure}`, failing: true };
  if (wait.check) {
    const parts = [];
    if (wait.checking) parts.push(`checking now (${checkCount(wait)})`);
    else if (!wait.dueAt) parts.push(`${checkCount(wait)} starting`);
    else {
      const left = Date.parse(wait.dueAt) - now;
      parts.push(left > 0 ? `${checkCount(wait)} in ${leftWords(left)}` : `${checkCount(wait)} now`);
    }
    const last = lastWords(wait.check.last);
    if (last) parts.push(`last: ${last}`);
    if (wait.check.failed > 0) parts.push(`${wait.check.failed} failed in a row${wait.maxFailedChecks ? ` (fails at ${wait.maxFailedChecks})` : ''}`);
    if (wait.givesUpAt) parts.push(`${wait.timesOut ? 'times out' : 'gives up'} ${atWords(wait.givesUpAt, new Date(now))}`);
    return { text: parts.join(' · '), check: true };
  }
  if (!wait.dueAt) return { text: 'starting' };
  const left = Date.parse(wait.dueAt) - now;
  if (left <= 0) return { text: 'finishing…' };
  return { text: `${leftWords(left)} left · until ${atWords(wait.dueAt, new Date(now))}` };
}

// waitShort is a waiting step's few words for a run card: "2h 13m left",
// "check 3/20, next in 4m", "checking (3/20)".
export function waitShort(command, now = Date.now()) {
  const wait = command?.wait || {};
  if (wait.failure) return 'wait failing';
  const of = (n) => (wait.maxChecks ? `${n}/${wait.maxChecks}` : `${n}`);
  if (wait.check) {
    const n = (wait.check.checks || 0) + 1;
    if (wait.checking || !wait.dueAt) return `checking (${of(n)})`;
    const left = Date.parse(wait.dueAt) - now;
    return left > 0 ? `check ${of(n)}, next in ${leftWords(left)}` : `checking (${of(n)})`;
  }
  if (!wait.dueAt) return 'waiting';
  const left = Date.parse(wait.dueAt) - now;
  return left > 0 ? `${leftWords(left)} left` : 'finishing…';
}
