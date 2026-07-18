export const USAGE_HISTORY_SPANS = [
  { hours: 24, label: '24h' },
  { hours: 168, label: '7d' },
  { hours: 720, label: '30d' },
  { hours: 2160, label: '90d' },
];

export const USAGE_LOOKAHEAD_SPANS = [
  { hours: 5, label: '5h' },
  { hours: 24, label: '24h' },
  { hours: 168, label: '7d' },
  { hours: 720, label: '30d' },
];

export function usageProviderLabel(provider) {
  if (provider === 'anthropic') return 'Claude';
  if (provider === 'openai') return 'Codex';
  return provider || 'Provider';
}

export function usageWindowLabel(name, durationSeconds = 0) {
  if (name === 'five_hour') return '5 hour';
  if (name === 'seven_day') return '7 day';
  if (name === 'seven_day_sonnet') return '7 day Sonnet';
  if (durationSeconds > 0) {
    const hours = durationSeconds / 3600;
    return hours >= 24 && hours % 24 === 0 ? `${hours / 24} day` : `${hours} hour`;
  }
  return String(name || 'limit').replaceAll('_', ' ');
}

export function formatUsageTime(value, now = Date.now()) {
  const at = new Date(value).getTime();
  if (!Number.isFinite(at)) return 'unknown';
  const delta = at - now;
  const abs = Math.abs(delta);
  const mins = Math.max(1, Math.round(abs / 60000));
  const amount = mins < 60 ? `${mins}m` : mins < 2880 ? `${Math.round(mins / 60)}h` : `${Math.round(mins / 1440)}d`;
  return delta >= 0 ? `in ${amount}` : `${amount} ago`;
}

export function formatUsageDuration(milliseconds) {
  if (!Number.isFinite(milliseconds)) return 'unknown';
  let mins = Math.max(0, Math.round(Math.abs(milliseconds) / 60000));
  if (mins === 0) return '<1m';
  const days = Math.floor(mins / 1440);
  mins -= days * 1440;
  const hours = Math.floor(mins / 60);
  mins -= hours * 60;
  return [days && `${days}d`, hours && `${hours}h`, mins && `${mins}m`].filter(Boolean).join(' ');
}

export function formatUsageResetCountdown(value, now = Date.now()) {
  const at = new Date(value).getTime();
  if (!Number.isFinite(at)) return 'reset unknown';
  const delta = at - now;
  if (Math.abs(delta) <= 60_000) return 'resets now';
  return delta > 0
    ? `resets in ${formatUsageDuration(delta)}`
    : `reset ${formatUsageDuration(delta)} ago`;
}

export function usageForecastView(forecast, now = Date.now(), latestAt = '') {
  if (!forecast) return { tone: 'muted', headline: 'Prediction unavailable', lines: [] };
  const rate = forecast.rate_pct_per_hour
    ? `Average usage rate: ${forecast.rate_pct_per_hour.toFixed(1)} percentage points/hour`
    : '';
  const hitAt = new Date(forecast.hits_limit_at).getTime();
  const resetAt = new Date(forecast.reset_at).getTime();
  const beforeReset = Number.isFinite(hitAt) && Number.isFinite(resetAt) && resetAt > hitAt
    ? formatUsageDuration(resetAt - hitAt)
    : '';
  if (forecast.status === 'stale') {
    const resetPassed = forecast.reset_at && new Date(forecast.reset_at).getTime() <= now;
    return {
      tone: 'muted', headline: 'Prediction paused',
      lines: [resetPassed ? `Reported reset ${formatUsageTime(forecast.reset_at, now)}` : `Last sample ${formatUsageTime(latestAt, now)}`],
    };
  }
  if (forecast.status === 'limit') return { tone: 'danger', headline: 'Prediction: limit reached', lines: [] };
  if (forecast.status === 'before_reset') return {
    tone: 'danger',
    headline: `Prediction: limit hit ${formatUsageTime(forecast.hits_limit_at, now)}${beforeReset ? ` (${beforeReset} before reset)` : ''}`,
    lines: [beforeReset && `Predicted time without quota access: ${beforeReset}`, rate].filter(Boolean),
  };
  if (forecast.status === 'after_reset') return {
    tone: 'good', headline: `Prediction: reset ${formatUsageTime(forecast.reset_at, now)} (before limit)`,
    lines: [`At this rate, limit would be hit ${formatUsageTime(forecast.hits_limit_at, now)}`, rate].filter(Boolean),
  };
  if (forecast.status === 'projected') return {
    tone: 'warn', headline: `Prediction: limit hit ${formatUsageTime(forecast.hits_limit_at, now)}`,
    lines: ['Reset time unavailable', rate].filter(Boolean),
  };
  if (forecast.status === 'flat') return { tone: 'good', headline: 'Prediction: no limit crossing', lines: ['Usage is flat'] };
  return {
    tone: 'muted', headline: 'Prediction warming up',
    lines: [`${forecast.sample_count || 0} post-reset sample${forecast.sample_count === 1 ? '' : 's'} · needs 3 over 30m`],
  };
}

export function usageSeriesSort(a, b) {
  const provider = usageProviderLabel(a.provider).localeCompare(usageProviderLabel(b.provider));
  if (provider) return provider;
  return (a.duration_seconds || 0) - (b.duration_seconds || 0) || a.window_name.localeCompare(b.window_name);
}
