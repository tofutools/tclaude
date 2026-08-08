import { shellConfirm as confirmModal } from './shell-state.js';

// reportBulkPowerFailures shows the daemon's PER-AGENT reason for every agent a
// bulk shutdown / power-on could not handle, and reports whether it showed
// anything.
//
// The summary toast counts outcomes ("1 failed") and nothing else, while the
// response has carried `outcome` + `detail` per agent all along — the daemon
// knew the agent's resume died with "duplicate session: 019fde64" and the
// dashboard dropped it on the floor. That is what made a real, repeating
// failure read as "it failed with no error"; the single-agent wake path has
// always surfaced the same fields. A modal rather than a toast because a
// failure list is something to read, and possibly to copy into a bug report,
// not something to catch before it fades.
// POWER_SUCCESS_OUTCOMES are the outcomes that are NOT a failure, for both
// endpoints. The filter is expressed as "not a success" rather than
// "== 'failed'" so it agrees with the daemon's own bucketing by construction:
// the counters behind the summary toast fold anything unrecognized into
// Failed (`default: resp.Failed++`). Matching on the failure name instead
// would let a future or renamed outcome make the toast say "1 failed" while
// this modal listed nothing — straight back to "it failed with no error".
const POWER_SUCCESS_OUTCOMES = new Set([
  'exited_gracefully', 'force_killed', 'already_offline', // shutdown
  'resumed', 'already_online',                            // power-on
]);

// MAX_LISTED_FAILURES bounds the list. .modal has no height cap and
// .modal-overlay centres without scrolling, so an unbounded list pushes its own
// Close button past the viewport — the failing mode is that the thing this
// modal exists to show becomes unreachable. The body scrolls (see
// .confirm-body-preformatted) AND the list is capped, because a 40-agent dump
// is not readable even when it fits.
const MAX_LISTED_FAILURES = 15;

export async function reportBulkPowerFailures(verb, out) {
  const failures = (out.agents || [])
    .filter(a => !POWER_SUCCESS_OUTCOMES.has(String(a.outcome || '')));
  if (!failures.length) return false;
  const lines = failures.slice(0, MAX_LISTED_FAILURES).map((a) => {
    const name = a.title || a.agent_id || a.conv_id || '(unnamed)';
    const detail = a.detail ? `\n    ${a.detail}` : '';
    return `${name} — ${a.outcome}${detail}`;
  });
  if (failures.length > lines.length) {
    lines.push(`… and ${failures.length - lines.length} more (see the daemon log)`);
  }
  await confirmModal({
    title: `${verb}: ${failures.length === 1 ? '1 agent' : failures.length + ' agents'} could not be handled`,
    body: lines.join('\n'),
    meta: `${out.targeted} targeted`,
    okLabel: 'Close',
    informational: true,
    preformatted: true,
  });
  return true;
}
