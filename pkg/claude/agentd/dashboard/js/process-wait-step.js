// process-wait-step.js -- the editors' pure layer for a wait step
// (model.WaitConfig): for a time (duration and/or until) or until a
// criterion (wait.check), checked again and again. No DOM and no fetch: the
// full editor's inspector (process-node-inspector.js) and the inline mini
// editor (groups-mini-editor.js) both edit a node draft through these, and
// jstest/process-wait-step.test.mjs exercises this file directly.

// Units mirror model.waitDurationUnits (process/model/wait.go).
const SECOND = 1000;
const UNITS = {
  ns: 1e-6, us: 1e-3, 'µs': 1e-3, ms: 1,
  s: SECOND, sec: SECOND, secs: SECOND, second: SECOND, seconds: SECOND,
  m: 60 * SECOND, min: 60 * SECOND, mins: 60 * SECOND, minute: 60 * SECOND, minutes: 60 * SECOND,
  h: 3600 * SECOND, hr: 3600 * SECOND, hrs: 3600 * SECOND, hour: 3600 * SECOND, hours: 3600 * SECOND,
  d: 86400 * SECOND, day: 86400 * SECOND, days: 86400 * SECOND,
  w: 604800 * SECOND, wk: 604800 * SECOND, week: 604800 * SECOND, weeks: 604800 * SECOND,
};
const MAX_WAIT_MS = 10 * 365 * 86400 * SECOND;
const PART = /^(\d+(?:\.\d+)?)\s*([a-zµ]+)/;

// The outcomes a check wait that times out routes by (model.WaitOutcome*).
export const WAIT_MET = 'met';
export const WAIT_TIMED_OUT = 'timedOut';
export const DEFAULT_MAX_FAILED_CHECKS = 3;
export const DEFAULT_BACKOFF_FACTOR = 2;

// The ways a check answers: a condition the engine reads, a program's exit
// code, or an agent's yes/no.
export const CHECK_SOURCES = Object.freeze([
  { key: 'condition', label: 'a condition holds', hint: 'A rule over params, outputs and step results, e.g. outputs.state == "ready"' },
  { key: 'program', label: 'a command says yes', hint: 'Exit 0 is yes, 1 is not yet; anything else (or the timeout) is a failed check' },
  { key: 'agent', label: 'an agent says yes', hint: 'The agent answers yes or no' },
]);

// parseWaitDuration mirrors model.ParseWaitDuration: milliseconds, or an
// error message the editor shows beside the field.
export function parseWaitDuration(value) {
  const original = String(value ?? '');
  let rest = original.trim().toLowerCase();
  if (!rest) return { error: 'a duration is required' };
  let total = 0;
  while (rest) {
    const match = PART.exec(rest);
    if (!match) return { error: `not a duration such as 30m, 2h, 3d or "3 days"` };
    const unit = UNITS[match[2]];
    if (unit === undefined) return { error: `unknown unit "${match[2]}" (use s, m, h, d, w, or minutes, hours, days, weeks)` };
    total += Number(match[1]) * unit;
    rest = rest.slice(match[0].length).trim();
    if (rest.startsWith(',')) rest = rest.slice(1).trim();
    if (rest.startsWith('and ')) rest = rest.slice(4).trim();
  }
  if (total <= 0) return { error: 'a duration must be positive' };
  if (total > MAX_WAIT_MS) return { error: 'longer than a wait can be' };
  return { ms: total };
}

// formatWaitDuration says a duration in words: "3 days", "1 hour 30 minutes".
export function formatWaitDuration(ms) {
  if (!Number.isFinite(ms) || ms <= 0) return '';
  const parts = [];
  let rest = Math.round(ms / SECOND);
  for (const [unit, size] of [['week', 604800], ['day', 86400], ['hour', 3600], ['minute', 60], ['second', 1]]) {
    const n = Math.floor(rest / size);
    if (n > 0) parts.push(`${n} ${unit}${n === 1 ? '' : 's'}`);
    rest -= n * size;
    if (parts.length === 2) break;
  }
  return parts.join(' ') || 'under a second';
}

// durationFeedback is what the editor says beside a duration field: "= 3
// days", the error, or nothing for a blank field or one with a {{ ref }}.
export function durationFeedback(value) {
  const text = String(value ?? '').trim();
  if (!text || text.includes('{{')) return { text: '' };
  const parsed = parseWaitDuration(text);
  return parsed.error ? { text: parsed.error, error: true } : { text: `= ${formatWaitDuration(parsed.ms)}` };
}

// waitMode is how a wait step waits: 'time' or 'check'.
export function waitMode(node) {
  return node?.wait?.check ? 'check' : 'time';
}

// checkSource is which of CHECK_SOURCES a check wait uses.
export function checkSource(check) {
  if (!check?.performer) return 'condition';
  return check.performer.kind === 'agent' ? 'agent' : 'program';
}

// seedCheck is a new check's starting point: every 5 minutes, at most 12
// times (an hour), failing the step if it never holds.
function seedCheck(source) {
  const check = { every: '5m', maxChecks: 12 };
  return applySource(check, source);
}

function applySource(check, source) {
  const next = { ...check };
  delete next.when;
  delete next.performer;
  if (source === 'program') {
    next.performer = { kind: 'program', profile: '', run: '' };
  } else if (source === 'agent') {
    next.performer = { kind: 'agent', prompt: '', anyone: true };
    // A timeout bounds a program's run; an agent check has none.
    delete next.timeout;
  } else {
    next.when = '';
    delete next.timeout;
  }
  return next;
}

// setWaitMode switches a wait step between waiting for a time and for a
// criterion. Each mode's fields go; the other's start from a seed.
export function setWaitMode(node, mode, source = 'condition') {
  if (mode !== 'time' && mode !== 'check') throw new Error(`unknown wait mode ${mode}`);
  const wait = { ...(node.wait || {}) };
  if (mode === 'check') {
    delete wait.duration;
    delete wait.until;
    delete wait.signal;
    if (!wait.check) wait.check = seedCheck(source);
  } else {
    delete wait.check;
  }
  if (Object.keys(wait).length) node.wait = wait;
  else delete node.wait;
}

// setCheckSource changes what answers a check, keeping its schedule and
// limits.
export function setCheckSource(node, source) {
  if (!CHECK_SOURCES.some((item) => item.key === source)) throw new Error(`unknown check source ${source}`);
  const check = node.wait?.check;
  if (!check) throw new Error('not a wait for a criterion');
  node.wait = { ...node.wait, check: applySource(check, source) };
}

const CHECK_TEXT = ['when', 'every', 'maxEvery', 'timeout', 'within'];
const CHECK_INTS = ['maxChecks', 'maxFailedChecks'];

// setCheckField edits one field of a wait's check. Blank clears it; counts
// and the factor must be numbers. Returns an error message for a bad value
// and leaves the node alone.
export function setCheckField(node, key, raw) {
  const check = { ...(node.wait?.check || {}) };
  if (!node.wait?.check) throw new Error('not a wait for a criterion');
  const text = String(raw ?? '').trim();
  if (CHECK_TEXT.includes(key)) {
    if (text || key === 'when') check[key] = text;
    else delete check[key];
  } else if (CHECK_INTS.includes(key)) {
    if (!text) delete check[key];
    else {
      const n = Number(text);
      if (!Number.isSafeInteger(n) || n < 1) return `${key === 'maxChecks' ? 'checks' : 'failed checks'} must be a whole number of at least 1`;
      check[key] = n;
    }
  } else if (key === 'factor') {
    if (!text) delete check.factor;
    else {
      const n = Number(text);
      if (!Number.isFinite(n) || n <= 1) return 'the factor must be a number above 1';
      check.factor = n;
    }
  } else if (key === 'backoff') {
    if (text === 'exponential') check.backoff = 'exponential';
    else {
      // Fixed is the default, and its knobs go with it.
      delete check.backoff;
      delete check.factor;
      delete check.maxEvery;
    }
  } else if (key === 'onLimit') {
    if (text === WAIT_TIMED_OUT) check.onLimit = WAIT_TIMED_OUT;
    else delete check.onLimit;
  } else {
    throw new Error(`unknown check field ${key}`);
  }
  node.wait = { ...node.wait, check };
  return '';
}

// setCheckPerformerField edits the program or agent that answers a check.
export function setCheckPerformerField(node, key, raw) {
  const performer = node.wait?.check?.performer;
  if (!performer) throw new Error('this check has no performer');
  if (!['profile', 'run', 'prompt', 'role'].includes(key)) throw new Error(`unknown check performer field ${key}`);
  const next = { ...performer };
  const text = String(raw ?? '').trim();
  if (key === 'role') {
    // An agent check is done by a role, or by anyone in the group.
    if (text) { next.role = text; delete next.anyone; } else { delete next.role; next.anyone = true; }
  } else {
    next[key] = text;
  }
  node.wait = { ...node.wait, check: { ...node.wait.check, performer: next } };
}

// waitCheckTimesOut reports whether a check wait has a timedOut route.
export function waitCheckTimesOut(node) {
  return node?.wait?.check?.onLimit === WAIT_TIMED_OUT;
}

// waitRouteHint says what the canvas still needs for a check wait's routes,
// given its outgoing edge outcomes; '' when they are right.
export function waitRouteHint(node, outcomes) {
  if (!waitCheckTimesOut(node)) return '';
  const have = new Set(outcomes);
  const missing = [WAIT_MET, WAIT_TIMED_OUT].filter((outcome) => !have.has(outcome));
  if (!missing.length) return '';
  return `Draw ${missing.length === 2 ? 'two connectors' : 'a connector'} labelled ${missing.join(' and ')}: where to go when it holds, and when it gives up.`;
}

// waitOutcomeFor names a new connector from a wait step: met, then
// timedOut, for a check wait that times out; '' to leave it to the caller.
export function waitOutcomeFor(node, taken) {
  if (!waitCheckTimesOut(node)) return '';
  const used = new Set(taken);
  return [WAIT_MET, WAIT_TIMED_OUT].find((outcome) => !used.has(outcome)) || '';
}

// describeWait says a wait step in a line: "Wait 3 days", "Every 5 minutes
// until outputs.state == "ready", at most 12 checks".
export function describeWait(node) {
  const wait = node?.wait || {};
  if (!wait.check) {
    const parts = [];
    if (wait.duration) {
      const parsed = parseWaitDuration(wait.duration);
      parts.push(`Wait ${parsed.error ? wait.duration : formatWaitDuration(parsed.ms)}`);
    }
    if (wait.until) parts.push(`${parts.length ? 'or until' : 'Until'} ${wait.until}`);
    return parts.join(' ') || 'Wait';
  }
  return describeCheck(wait.check);
}

function words(value) {
  const parsed = parseWaitDuration(value);
  return parsed.error ? String(value) : formatWaitDuration(parsed.ms);
}

// describeCheck says a check's schedule and limits in one line.
export function describeCheck(check) {
  const source = checkSource(check);
  const what = source === 'condition' ? (check.when ? `until ${check.when}` : 'until a condition holds')
    : source === 'program' ? (check.performer.run ? `until ${check.performer.run} says yes` : 'until a command says yes')
      : 'until an agent says yes';
  let schedule = check.every ? `every ${words(check.every)}` : 'every …';
  if (check.backoff === 'exponential') {
    schedule += `, backing off ×${check.factor || DEFAULT_BACKOFF_FACTOR}${check.maxEvery ? ` up to ${words(check.maxEvery)}` : ''}`;
  }
  const limits = [];
  if (check.maxChecks) limits.push(`${check.maxChecks} check${check.maxChecks === 1 ? '' : 's'}`);
  if (check.within) limits.push(words(check.within));
  const limit = limits.length ? `gives up after ${limits.join(' or ')}` : 'no limit yet';
  const then = check.onLimit === WAIT_TIMED_OUT ? 'then takes timedOut' : 'then fails';
  return `Check ${schedule} ${what}; ${limit}, ${then}`;
}

// checkMoreSet reports whether any of a check's "more" knobs is set, so the
// editor opens that section on a wait that uses one.
export function checkMoreSet(check) {
  return !!(check && (check.backoff || check.factor || check.maxEvery || check.timeout || check.maxFailedChecks));
}

// waitSub is a wait's sub line on a drawn graph: "3 days", "until launch_at",
// "checks every 5 minutes".
export function waitSub(node) {
  const wait = node?.wait || {};
  if (wait.check) return wait.check.every ? `checks every ${words(wait.check.every)}` : 'checks a condition';
  if (wait.duration) return words(wait.duration);
  if (wait.until) return `until ${wait.until}`;
  return '';
}
