const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

export function usageAxisStart(requestedStart, firstObserved, now) {
  const requested = requestedStart === null || requestedStart === undefined ? NaN : Number(requestedStart);
  const observed = Number(firstObserved);
  const current = Number(now);
  const fallback = Number.isFinite(observed) ? observed : current - MINUTE;
  const candidate = Number.isFinite(requested) ? requested : fallback;
  return Math.min(candidate, current - MINUTE);
}

export function usageAxisTicks(start, end, count = 5) {
  const first = Number(start);
  const last = Number(end);
  const total = Math.max(2, Math.floor(Number(count) || 0));
  if (!Number.isFinite(first) || !Number.isFinite(last) || last <= first) return [];
  return Array.from({ length: total }, (_, index) => {
    const ratio = index / (total - 1);
    return { time: first + (last - first) * ratio, ratio };
  });
}

export function usageForecastPoint(startTime, startPct, ratePctPerHour, endTime, ratio) {
  const first = Number(startTime);
  const last = Number(endTime);
  const pct = Number(startPct);
  const rate = Number(ratePctPerHour);
  if (![first, last, pct, rate].every(Number.isFinite) || last <= first) return null;
  const position = Math.max(0, Math.min(1, Number(ratio) || 0));
  const time = first + (last - first) * position;
  return {
    time,
    pct: Math.min(100, pct + rate * Math.max(0, time - first) / HOUR),
    ratio: position,
  };
}

export function formatUsageAxisTick(time, start, end, locale) {
  const date = new Date(time);
  if (!Number.isFinite(date.getTime())) return '';
  const span = Math.max(0, Number(end) - Number(start));
  const day = date.toLocaleDateString(locale, { day: 'numeric', month: 'short' });
  if (span <= 2 * DAY) {
    const clock = date.toLocaleTimeString(locale, { hour: '2-digit', minute: '2-digit' });
    return `${day} ${clock}`;
  }
  if (span <= 366 * DAY) return day;
  return date.toLocaleDateString(locale, { month: 'short', year: 'numeric' });
}
