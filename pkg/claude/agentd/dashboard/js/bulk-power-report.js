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
export async function reportBulkPowerFailures(verb, out) {
  const failures = (out.agents || []).filter(a => String(a.outcome || '') === 'failed');
  if (!failures.length) return false;
  const lines = failures.map((a) => {
    const name = a.title || a.agent_id || a.conv_id || '(unnamed)';
    const detail = a.detail ? `\n    ${a.detail}` : '';
    return `${name} — ${a.outcome}${detail}`;
  });
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
