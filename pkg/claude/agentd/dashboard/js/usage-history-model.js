export const USAGE_HISTORY_SPANS = [
  { hours: 24, label: '24h' },
  { hours: 168, label: '7d' },
  { hours: 720, label: '30d' },
  { hours: 2160, label: '90d' },
];

export function usageProviderLabel(provider) {
  if (provider === 'anthropic') return 'Claude';
  if (provider === 'openai') return 'Codex';
  return provider || 'Provider';
}

export function usageWindowLabel(name, durationSeconds = 0) {
  if (name === 'five_hour') return '5 hour';
  if (name === 'seven_day') return '7 day';
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

export function usageForecastView(forecast, now = Date.now()) {
  if (!forecast) return { tone: 'muted', headline: 'No forecast', detail: '' };
  const rate = forecast.rate_pct_per_hour ? `${forecast.rate_pct_per_hour.toFixed(1)}%/h` : '';
  if (forecast.status === 'limit') return { tone: 'danger', headline: 'Limit reached', detail: '' };
  if (forecast.status === 'before_reset') return {
    tone: 'danger', headline: `Limit ${formatUsageTime(forecast.hits_limit_at, now)}`,
    detail: `before reset ${formatUsageTime(forecast.reset_at, now)} · ${rate}`,
  };
  if (forecast.status === 'after_reset') return {
    tone: 'good', headline: `Reset first ${formatUsageTime(forecast.reset_at, now)}`,
    detail: `straight-line limit ${formatUsageTime(forecast.hits_limit_at, now)} · ${rate}`,
  };
  if (forecast.status === 'projected') return {
    tone: 'warn', headline: `Limit ${formatUsageTime(forecast.hits_limit_at, now)}`,
    detail: `${rate} · reset time unavailable`,
  };
  if (forecast.status === 'flat') return { tone: 'good', headline: 'Usage is flat', detail: 'No limit crossing projected' };
  return {
    tone: 'muted', headline: 'Forecast warming up',
    detail: `${forecast.sample_count || 0} post-reset sample${forecast.sample_count === 1 ? '' : 's'} · needs 3 over 30m`,
  };
}

export function usageSeriesSort(a, b) {
  const provider = usageProviderLabel(a.provider).localeCompare(usageProviderLabel(b.provider));
  if (provider) return provider;
  return (a.duration_seconds || 0) - (b.duration_seconds || 0) || a.window_name.localeCompare(b.window_name);
}
