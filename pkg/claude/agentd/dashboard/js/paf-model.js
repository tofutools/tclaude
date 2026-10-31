// PAF in the dashboard (tcl-jkmitn): pure shaping for the export, redact and
// import dialogs, kept free of the DOM so it is unit-testable.

// queryString builds a query from an object, skipping empty values and
// repeating array values (member_harness=a=x&member_harness=b=y).
export function queryString(params) {
  const q = new URLSearchParams();
  for (const [key, value] of Object.entries(params || {})) {
    if (Array.isArray(value)) value.filter(Boolean).forEach((v) => q.append(key, v));
    else if (value === true) q.set(key, 'true');
    else if (value !== undefined && value !== null && value !== false && value !== '') q.set(key, String(value));
  }
  const s = q.toString();
  return s ? `?${s}` : '';
}

// exportParams is the agent export query for the dialog's choices. Without
// history the conversation stays home; native and neutral pick its copies.
export function exportParams({ agent, history = true, native = true, neutral = true, allowFlagged = false }) {
  return {
    agent, format: 'paf', history,
    no_native: history && !native, no_context: history && !neutral,
    allow_flagged: allowFlagged,
  };
}

// landingParams turns the group preview's choices into the
// parseGroupLandingOptions query: an all-members harness/model plus
// per-member overrides keyed by the member's title (or conv id).
export function landingParams({ harness = '', model = '', members = {}, confirmHistoryLoss = false } = {}) {
  const memberHarness = [];
  const memberModel = [];
  for (const [member, choice] of Object.entries(members)) {
    if (choice?.harness) memberHarness.push(`${member}=${choice.harness}`);
    if (choice?.model) memberModel.push(`${member}=${choice.model}`);
  }
  return { harness, model, member_harness: memberHarness, member_model: memberModel, confirm_history_loss: confirmHistoryLoss ? 'true' : '' };
}

const RUNG = {
  native: { cls: 'native', text: 'native', title: 'The history installs as it is: nothing is lost.' },
  translated: { cls: 'translated', text: 'translated', title: 'The conversation is rewritten for the landing harness and model, within its context budget.' },
  none: { cls: 'none', text: 'config only', title: 'The conversation cannot land here; the agent imports as configuration only. tclaude paf render still reads the whole conversation.' },
};

export function rungBadge(rung) {
  return RUNG[rung] || { cls: 'none', text: rung || 'unknown', title: '' };
}

// lossSummary is a one-line account of a translation's losses: counts and
// kinds only, never content.
export function lossSummary(losses) {
  if (!losses) return '';
  const parts = [];
  if (losses.source_entries) parts.push(`carries ${losses.carried_entries} / ${losses.source_entries} entries`);
  if (losses.carried_estimated_tokens) parts.push(`~${Math.round(losses.carried_estimated_tokens / 1000)}k tokens`);
  if (losses.budget_tokens) parts.push(`budget ${Math.round(losses.budget_tokens / 1000)}k`);
  const items = (losses.items || []).filter((item) => item.count > 0)
    .map((item) => `${item.count} ${String(item.kind).replace(/_/g, ' ')}${item.detail ? ` (${item.detail})` : ''}`);
  if (items.length) parts.push(`losses: ${items.join(', ')}`);
  return parts.join(' · ');
}

export function findingsText(findings) {
  return (findings || []).map((f) => `${f.kind}${f.count > 1 ? ` ×${f.count}` : ''}${f.locations?.length ? ` (${f.locations.slice(0, 3).join(', ')}${f.locations.length > 3 ? ', …' : ''})` : ''}`);
}

export function redactionText(report) {
  return (report?.redactions || []).map((r) => `${String(r.kind).replace(/_/g, ' ')}${r.count ? `: ${r.count}` : ''}${r.detail ? ` (${r.detail})` : ''}`);
}

// provenanceRows lists where an agent PAF came from, from its definition.
export function provenanceRows(agent) {
  const rows = [];
  const origin = agent?.origin || {};
  if (origin.instance) rows.push(['From node', origin.instance]);
  if (origin.trigger) rows.push(['Trigger', origin.trigger]);
  if (agent?.harness || origin.model) rows.push(['Original', [agent?.harness, origin.model].filter(Boolean).join(' · ')]);
  if (agent?.paths?.cwd) rows.push(['Recorded dir', agent.paths.cwd]);
  return rows;
}

// spawnableHarnesses lists the harnesses an import can land on.
export function spawnableHarnesses(snapshot) {
  return (snapshot?.harnesses || []).filter((h) => h?.name && h.name !== 'shell');
}

export function modelsFor(snapshot, harness) {
  return spawnableHarnesses(snapshot).find((h) => h.name === harness)?.models || [];
}

// downloadBlob saves a fetched file through a transient anchor; the object
// URL is revoked right after the click.
// dashboard-imperative-boundary: browser-io
export function downloadBlob(blob, filename) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

// safeFilename keeps an agent title usable as a file name.
export function safeFilename(name, ext) {
  const base = String(name || 'agent').replace(/[^A-Za-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '') || 'agent';
  return `${base}${ext}`;
}
