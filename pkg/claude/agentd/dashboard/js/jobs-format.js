// Shared pure formatting for the Preact Jobs island and the legacy cron modal.
export function formatJobInterval(seconds) {
  if (!seconds) return '';
  if (seconds < 60) return seconds + 's';
  if (seconds < 3600) return Math.floor(seconds / 60) + 'm';
  if (seconds < 86400) return Math.floor(seconds / 3600) + 'h';
  return Math.floor(seconds / 86400) + 'd';
}
