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

// usageSeriesKeyOf identifies a series (provider × quota window) for the
// per-series span preferences and the request's `spans` overrides. Provider
// and window names are server-defined slugs without ':' or ','.
export function usageSeriesKeyOf(series) {
  return `${series.provider}:${series.window_name}`;
}

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

// usageScopeLabel names the series (provider × quota window) for the chart's
// tooltips and aria-labels. Shared because both used to build it inline: two
// copies of the same sentence, differing only in separator, both applying the
// same window/cycle swap — editing one and not the other would desync a
// graph's spoken name from its visible tooltip.
export function usageWindowScopeLabel(series, wizard = false) {
  return `${usageWindowLabel(series.window_name, series.duration_seconds)} ${wizard ? 'cycle' : 'window'}`;
}

export function usageScopeLabel(series, wizard = false, separator = ' ') {
  return `${usageProviderLabel(series.provider)}${separator}${usageWindowScopeLabel(series, wizard)}`;
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

// The wizard voice for this tab. The dashboard already calls a context window
// an agent's "mana reserve" (the .ctx-mana crystal gauge and its 🔮 tooltip in
// groups-member-table.js), and the nav tab is already "📈 Reserves" — so a
// provider quota is mana too, spending it is *channeling*, a quota reset is a
// *replenishment*, and a forecast is a *prophecy*. Nothing here invents a new
// metaphor; it finishes one the rest of the dashboard already speaks.
//
// Unlike most wizard copy this is NOT the two-spans/CSS-reveal `Words` trick:
// most of these strings end up inside SVG <text> tooltips, where a <span> twin
// is invalid markup. The island subscribes to the tclaude:wizard edge and
// repaints instead (see useWizardTheme in usage-history-island.js), which keeps
// the instant-swap property the `Words` pattern exists to provide, so plain
// strings are safe for the whole tab and one mechanism covers it.
export function formatUsageResetCountdown(value, now = Date.now(), wizard = false) {
  const at = new Date(value).getTime();
  if (!Number.isFinite(at)) return wizard ? 'replenishment unknown' : 'reset unknown';
  const delta = at - now;
  if (Math.abs(delta) <= 60_000) return wizard ? 'replenishing now' : 'resets now';
  if (wizard) {
    return delta > 0
      ? `replenishes in ${formatUsageDuration(delta)}`
      : `replenished ${formatUsageDuration(delta)} ago`;
  }
  return delta > 0
    ? `resets in ${formatUsageDuration(delta)}`
    : `reset ${formatUsageDuration(delta)} ago`;
}

export function usageForecastView(forecast, now = Date.now(), latestAt = '', wizard = false) {
  const w = (plain, wizardly) => (wizard ? wizardly : plain);
  if (!forecast) return { tone: 'muted', headline: w('Prediction unavailable', 'No prophecy to be had'), lines: [] };
  const rate = forecast.rate_pct_per_hour
    ? w(`Average usage rate: ${forecast.rate_pct_per_hour.toFixed(1)} percentage points/hour`,
      `Channeling rate: ${forecast.rate_pct_per_hour.toFixed(1)} percentage points of mana per hour`)
    : '';
  const hitAt = new Date(forecast.hits_limit_at).getTime();
  const resetAt = new Date(forecast.reset_at).getTime();
  const beforeReset = Number.isFinite(hitAt) && Number.isFinite(resetAt) && resetAt > hitAt
    ? formatUsageDuration(resetAt - hitAt)
    : '';
  if (forecast.status === 'stale') {
    const resetPassed = forecast.reset_at && new Date(forecast.reset_at).getTime() <= now;
    return {
      tone: 'muted', headline: w('Prediction paused', 'The vision has clouded over'),
      lines: [resetPassed
        ? w(`Reported reset ${formatUsageTime(forecast.reset_at, now)}`, `Replenishment reported ${formatUsageTime(forecast.reset_at, now)}`)
        : w(`Last sample ${formatUsageTime(latestAt, now)}`, `Last reading ${formatUsageTime(latestAt, now)}`)],
    };
  }
  if (forecast.status === 'limit') return { tone: 'danger', headline: w('Prediction: limit reached', 'The reserves are spent'), lines: [] };
  if (forecast.status === 'before_reset') return {
    tone: 'danger',
    headline: w(
      `Prediction: limit hit ${formatUsageTime(forecast.hits_limit_at, now)}${beforeReset ? ` (${beforeReset} before reset)` : ''}`,
      `Prophecy: reserves run dry ${formatUsageTime(forecast.hits_limit_at, now)}${beforeReset ? ` (${beforeReset} before replenishment)` : ''}`,
    ),
    lines: [beforeReset && w(`Predicted time without quota access: ${beforeReset}`, `Foretold time without mana: ${beforeReset}`), rate].filter(Boolean),
  };
  if (forecast.status === 'after_reset') return {
    tone: 'good',
    headline: w(`Prediction: reset ${formatUsageTime(forecast.reset_at, now)} (before limit)`,
      `Prophecy: replenishment ${formatUsageTime(forecast.reset_at, now)} (the reserves hold)`),
    lines: [w(`At this rate, limit would be hit ${formatUsageTime(forecast.hits_limit_at, now)}`,
      `At this rate the reserves would run dry ${formatUsageTime(forecast.hits_limit_at, now)}`), rate].filter(Boolean),
  };
  if (forecast.status === 'projected') return {
    tone: 'warn',
    headline: w(`Prediction: limit hit ${formatUsageTime(forecast.hits_limit_at, now)}`,
      `Prophecy: reserves run dry ${formatUsageTime(forecast.hits_limit_at, now)}`),
    lines: [w('Reset time unavailable', 'The hour of replenishment is unknown'), rate].filter(Boolean),
  };
  if (forecast.status === 'flat') return {
    tone: 'good', headline: w('Prediction: no limit crossing', 'Prophecy: the reserves hold'),
    lines: [w('Usage is flat', 'No mana is being channeled')],
  };
  return {
    tone: 'muted', headline: w('Prediction warming up', 'The vision is still forming'),
    lines: [w(
      `${forecast.sample_count || 0} post-reset sample${forecast.sample_count === 1 ? '' : 's'} · needs 3 over 30m`,
      `${forecast.sample_count || 0} reading${forecast.sample_count === 1 ? '' : 's'} since replenishment · needs 3 over 30m`,
    )],
  };
}

export function usageSeriesSort(a, b) {
  const provider = usageProviderLabel(a.provider).localeCompare(usageProviderLabel(b.provider));
  if (provider) return provider;
  return (a.duration_seconds || 0) - (b.duration_seconds || 0) || a.window_name.localeCompare(b.window_name);
}
