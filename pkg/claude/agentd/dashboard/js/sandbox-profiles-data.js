import { CODEX_BUILTIN_FILTERED_NETWORK_SHORT } from './sandbox-network-disclosure.js';

const API = '/api/sandbox-profiles';

async function request(path, options = {}) {
  const response = await fetch(path, { credentials: 'same-origin', ...options });
  if (!response.ok) {
    // Failures carry the daemon's structured {"error", "code"} body; the
    // status and typed code stay on the thrown Error so callers can key
    // recovery (e.g. break_glass_removed) off them instead of
    // pattern-matching message text.
    const raw = await response.text();
    let body = null;
    try { body = JSON.parse(raw); } catch (_) { body = null; }
    const error = new Error(body?.message || body?.error || raw || `HTTP ${response.status}`);
    error.status = response.status;
    if (body?.code) error.code = body.code;
    throw error;
  }
  if (response.status === 204) return null;
  return response.json().catch(() => ({}));
}

export async function loadSandboxProfiles() { const value = await request(API); return Array.isArray(value) ? value : []; }
// The filesystem-editor feed: audited deny presets the editor may insert as
// ordinary rows, plus provenance-rich harness-global rows it renders read-only.
// The endpoint keeps its historical read-exclusions path.
export function loadSandboxCommonRules() { return request('/api/sandbox-profile-read-exclusions'); }
export function predictSandboxProfile(draft, targets = [], context = {}) {
  return request('/api/sandbox-profile-enforcement', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ draft: sandboxProfileForWire(draft), targets, context }),
  });
}
export async function previewSandboxProfile(name, body) {
  const target = name ? `${API}/${encodeURIComponent(name)}` : API;
  return request(`${target}?dry_run=1`, { method: name ? 'PATCH' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
}
export async function saveSandboxProfile(name, body, revision = '') {
  const target = name ? `${API}/${encodeURIComponent(name)}?revision=${encodeURIComponent(revision)}` : API;
  return request(target, { method: name ? 'PATCH' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
}
export function deleteSandboxProfile(name) { return request(`${API}/${encodeURIComponent(name)}`, { method: 'DELETE' }); }
export async function exportSandboxProfiles(names) { const query = new URLSearchParams(); names.forEach((name) => query.append('name', name)); return request(`${API}/export?${query}`); }
export function inspectSandboxImport(envelope) { return request(`${API}/import/inspect`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(envelope) }); }
export function importSandboxProfiles(envelope, onConflict) { return request(`${API}/import`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ...envelope, on_conflict: onConflict, apply_assignments: false }) }); }
export function inspectSandboxDirectories(body) { return request('/api/sandbox-profile-directories/inspect', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }); }
export function createSandboxDirectories(body) { return request('/api/sandbox-profile-directories/create', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }); }

export function sandboxProfileSummary(profile) {
  const fs = profile.filesystem || []; const env = profile.environment || []; const inc = profile.includes || []; const own = profile.agent_directories || []; const pre = profile.pre_launch || [];
  const parts = [['read', 'read'], ['write', 'write'], ['deny', 'deny']].flatMap(([access, label]) => { const count = fs.filter((entry) => entry.access === access).length; return count ? [`${count} ${label}`] : []; });
  if (inc.length) parts.push(`${inc.length} include${inc.length === 1 ? '' : 's'}`);
  if (env.length) parts.push(`${env.length} env key${env.length === 1 ? '' : 's'}`);
  if (own.length) parts.push(`${own.length} agent dir${own.length === 1 ? '' : 's'}`);
  if (pre.length) parts.push(`${pre.length} pre-launch script${pre.length === 1 ? '' : 's'}`);
  const limits = profile.resource_limits || {};
  if (limits.memory) parts.push(`memory ${limits.memory}`);
  if (limits.cpu != null) parts.push(`CPU ${limits.cpu}`);
  if (profile.darwin_allow_mach_register) parts.push('Mach registration');
  const authoredNetwork = sandboxNetworkAuthoring(profile);
  if (authoredNetwork.baseline === 'allow') parts.push('network allow all');
  if (authoredNetwork.baseline === 'deny') {
    const unlocks = authoredNetwork.packs.length + authoredNetwork.allow.length;
    parts.push(`network deny all${unlocks ? ` (${unlocks} unlock${unlocks === 1 ? '' : 's'})` : ''}`);
  }
  const networkDenies = authoredNetwork.deny_packs.length + authoredNetwork.deny.length;
  if (networkDenies) parts.push(`${networkDenies} network deny${networkDenies === 1 ? '' : 's'}`);
  if (authoredNetwork.engine === 'packet') parts.push('packet filter');
  if (authoredNetwork.engine === 'proxy') parts.push('proxy filter');
  if (authoredNetwork.namespace === 'private') parts.push('private routed network');
  if (authoredNetwork.namespace === 'host') parts.push('shared host network');
  const axes = sandboxAccessAxes(profile);
  if (axes.unix_sockets.mode) parts.push(`sockets ${axes.unix_sockets.mode}${axes.unix_sockets.mode === 'list' ? ` (${axes.unix_sockets.allow.length})` : ''}`);
  return parts.join(' · ') || 'no sandbox rules';
}

export function sandboxResourceLimitsForWire(resourceLimits = {}) {
  const memory = String(resourceLimits.memory ?? '').trim();
  const cpuText = String(resourceLimits.cpu ?? '').trim();
  return {
    ...(memory ? { memory } : {}),
    ...(cpuText ? { cpu: Number(cpuText) } : {}),
  };
}

export function sandboxResourceLimitErrors(resourceLimits = {}) {
  const errors = [];
  const memory = String(resourceLimits.memory ?? '').trim();
  if (memory && !/^(?:\d+(?:\.\d+)?|\.\d+)(?:[KMGT](?:I(?:B)?|B)?|B)$/i.test(memory)) {
    errors.push('Memory limit must be a positive quantity with a B, K/KB/KiB, M/MB/MiB, G/GB/GiB, or T/TB/TiB unit.');
  }
  if (memory && /^(?:0+(?:\.0*)?|\.0+)[A-Za-z]+$/i.test(memory)) {
    errors.push('Memory limit must be greater than zero.');
  }
  const cpu = String(resourceLimits.cpu ?? '').trim();
  if (cpu && (!/^(?:\d+(?:\.\d+)?|\.\d+)$/.test(cpu) || !Number.isFinite(Number(cpu)) || Number(cpu) < 0.01)) {
    errors.push('CPU limit must be at least 0.01 finite cores, such as 0.5 or 2.');
  }
  return errors;
}

// The editor always authors the compositional baseline shape. Legacy mode-based
// payloads remain loadable and become manual rows; no pack reference is inferred
// from an exact materialized entry set.
export function sandboxNetworkAuthoring(profile = {}) {
  const network = profile.network ? structuredClone(profile.network) : null;
  if (network?.baseline) {
    network.packs ||= [];
    network.deny_packs ||= [];
    network.allow ||= [];
    network.deny ||= [];
    return network;
  }
  const legacy = profile.network_access || '';
  const mode = network?.mode || (legacy === 'internet' ? 'open' : legacy === 'none' ? 'closed' : '');
  return {
    baseline: mode === 'open' ? 'allow' : mode === 'closed' || mode === 'list' ? 'deny' : 'inherit',
    packs: [],
    deny_packs: [],
    allow: network?.allow || [],
    deny: [],
    // Engine is orthogonal to the legacy mode this branch reconstructs, so a
    // legacy payload that already names one keeps it.
    ...(network?.engine ? { engine: network.engine } : {}),
    ...(network?.namespace ? { namespace: network.namespace } : {}),
  };
}

export function sandboxAccessAxes(profile = {}) {
  const legacy = profile.network_access || '';
  const network = profile.network
    ? structuredClone(profile.network)
    : { mode: legacy === 'internet' ? 'open' : legacy === 'none' ? 'closed' : '', allow: [] };
  const unixSockets = profile.unix_sockets
    ? structuredClone(profile.unix_sockets)
    : { mode: legacy === 'none' ? 'closed' : '', allow: [] };
  network.allow ||= [];
  unixSockets.allow ||= [];
  return { network, unix_sockets: unixSockets };
}

function portsForWire(value) {
  if (Array.isArray(value)) return value.map(Number);
  const text = String(value || '').trim();
  return text ? text.split(',').map((part) => Number(part.trim())) : [];
}

export function sandboxNetworkEntryKey(entry = {}) {
  const ports = [...new Set(portsForWire(entry.ports))].sort((a, b) => a - b);
  return JSON.stringify({
    ...(entry.host ? { host: entry.host } : {}),
    ...(entry.domain ? {
      domain: entry.domain,
      ...(entry.include_subdomains ? { include_subdomains: true } : {}),
    } : {}),
    ...(entry.cidr ? { cidr: entry.cidr } : {}),
    ...(entry.loopback ? { loopback: true } : {}),
    ...(ports.length ? { ports } : {}),
  });
}

export function sandboxNetworkModeEntryKey(mode, entry = {}) {
  return `${mode}:${sandboxNetworkEntryKey(entry)}`;
}

function networkEntriesForWire(entries = []) {
  return entries.map((entry) => ({
    ...(entry.host ? { host: entry.host } : {}),
    ...(entry.domain ? { domain: entry.domain, include_subdomains: !!entry.include_subdomains } : {}),
    ...(entry.cidr ? { cidr: entry.cidr } : {}),
    ...(entry.loopback ? { loopback: true } : {}),
    ...(portsForWire(entry.ports).length ? { ports: portsForWire(entry.ports) } : {}),
  }));
}

export function sandboxProfileForWire(draft) {
  const value = structuredClone(draft || {});
  if (!Object.hasOwn(value, 'network') && !Object.hasOwn(value, 'unix_sockets')) return value;
  value.network ||= { mode: '', allow: [] };
  value.unix_sockets ||= { mode: '', allow: [] };
  if (value.network.baseline) {
    value.network = {
      baseline: value.network.baseline,
      ...(value.network.packs?.length ? { packs: [...value.network.packs].sort() } : {}),
      ...(value.network.deny_packs?.length ? { deny_packs: [...value.network.deny_packs].sort() } : {}),
      ...((value.network.allow || []).length ? { allow: value.network.allow } : {}),
      ...((value.network.deny || []).length ? { deny: value.network.deny } : {}),
      ...(value.network.engine ? { engine: value.network.engine } : {}),
      ...(value.network.namespace ? { namespace: value.network.namespace } : {}),
    };
  }
  const networkAllow = networkEntriesForWire(value.network.allow);
  if (networkAllow.length) value.network.allow = networkAllow;
  else delete value.network.allow;
  const networkDeny = networkEntriesForWire(value.network.deny);
  if (networkDeny.length) value.network.deny = networkDeny;
  else delete value.network.deny;
  value.unix_sockets.allow = (value.unix_sockets.allow || []).map((entry) =>
    entry.path_glob ? { path_glob: entry.path_glob } : { path: entry.path || '' });
  value.network_access = '';
  return value;
}

function dnsError(value) {
  if (!value || value !== value.trim() || value.length > 253 || /[/:*]/.test(value)
      || value.startsWith('.') || value.endsWith('.') || !/^[\x00-\x7f]+$/.test(value)) {
    return 'must be an ASCII host/domain without scheme, path, port, or wildcard';
  }
  if (!value.split('.').every((label) =>
    /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/.test(label))) {
    return 'must use DNS letters, digits, and interior hyphens';
  }
  return '';
}

export function sandboxAccessDraftErrors(draft) {
  const errors = [];
  const authoredNetwork = sandboxNetworkAuthoring(draft);
  const axes = sandboxAccessAxes(draft);
  if (!['inherit', 'allow', 'deny'].includes(authoredNetwork.baseline)) errors.push('Network baseline is invalid.');
  if (!['', 'packet', 'proxy'].includes(authoredNetwork.engine || '')) errors.push('Network filtering engine is invalid.');
  if (!['', 'host', 'private'].includes(authoredNetwork.namespace || '')) errors.push('Network namespace is invalid.');
  if (authoredNetwork.baseline === 'inherit' &&
      (authoredNetwork.packs.length || authoredNetwork.deny_packs.length ||
       authoredNetwork.allow.length || authoredNetwork.deny.length)) {
    errors.push('Network packs and entries require Deny all or Allow all baseline.');
  }
  const allowPacks = new Set(authoredNetwork.packs);
  const packOverlap = authoredNetwork.deny_packs.find((id) => allowPacks.has(id));
  if (packOverlap) {
    errors.push(`Network pack ${packOverlap} must use exactly one Allow or Deny mode.`);
  }
  if (!['', 'open', 'closed', 'list'].includes(axes.unix_sockets.mode)) errors.push('Unix-socket mode is invalid.');
  if (axes.unix_sockets.mode !== 'list' && axes.unix_sockets.allow.length) errors.push('Unix-socket entries require Access list mode.');
  const validateNetworkEntries = (entries, mode) => entries.forEach((entry, index) => {
    const selectors = ['host', 'domain', 'cidr'].filter((key) => entry[key]).length + (entry.loopback ? 1 : 0);
    if (selectors !== 1) errors.push(`Network ${mode} row ${index + 1} must set exactly one selector.`);
    if (entry.host && dnsError(entry.host)) errors.push(`Network ${mode} row ${index + 1} host ${dnsError(entry.host)}.`);
    if (entry.domain && dnsError(entry.domain)) errors.push(`Network ${mode} row ${index + 1} domain ${dnsError(entry.domain)}.`);
    if (entry.cidr && !/^([0-9a-f:.]+)\/\d{1,3}$/i.test(entry.cidr)) errors.push(`Network ${mode} row ${index + 1} CIDR is invalid.`);
    const rawPorts = Array.isArray(entry.ports) ? entry.ports : String(entry.ports || '').split(',').filter((part) => part.trim());
    if (rawPorts.some((port) => !/^\d+$/.test(String(port).trim()) || Number(port) < 1 || Number(port) > 65535)) {
      errors.push(`Network ${mode} row ${index + 1} ports must be comma-separated values from 1 to 65535.`);
    }
  });
  validateNetworkEntries(authoredNetwork.allow, 'allow');
  validateNetworkEntries(authoredNetwork.deny, 'deny');
  axes.unix_sockets.allow.forEach((entry, index) => {
    const value = entry.path_glob || entry.path || '';
    if (!value.startsWith('/')) errors.push(`Unix-socket row ${index + 1} must be an absolute path.`);
    if (value.includes('**')) errors.push(`Unix-socket row ${index + 1} must not contain **.`);
    if (entry.path_glob && !entry.path_glob.includes('*')) errors.push(`Unix-socket row ${index + 1} glob must contain *.`);
  });
  return errors;
}

/* A per-target refusal (TCL-885). The daemon returns it when this target's
   harness cannot enforce this policy at all — decided BEFORE any individual rule
   was judged, so the target carries no axes. It is deliberately its own field
   rather than an axis outcome: `sandboxRuleBuckets` already substitutes
   {outcome:'not_enforced'} for an axis an OLD daemon omitted, and a refusal
   folded into that path would be indistinguishable from that fallback — a
   refusal silently reading as "not enforced" is the exact failure this ticket
   removes. Every consumer must call this BEFORE reading axes.

   `contextIndex` selects the effective-assignment context the editor is showing;
   a refusal formed only in that context wins over the target-wide one. */
export function sandboxTargetRefusal(target = {}, contextIndex = null) {
  const scoped = Number.isInteger(contextIndex)
    ? target.context_refusals?.[contextIndex] : null;
  return scoped || target.refusal || null;
}

/* Contexts OTHER than the one on screen that this target refuses. The overall
   safety check covers every assignment, so a refusal in an unselected context
   must still be visible — the same contract sandboxOtherAssignmentWarnings
   carries for axis verdicts. */
export function sandboxOtherContextRefusals(target = {}, contextIndex = null) {
  const listed = (target.context_refusals || []).flatMap((refusal, index) =>
    refusal && index !== contextIndex ? [{ index, refusal }] : []);
  // Assignments past the display cap have no index of their own, and they
  // contribute nothing to the aggregate either (it summarizes surviving contexts
  // only) — so without these the editor would claim every assignment was checked
  // while a refusal among the omitted ones went unmentioned.
  // TCL-913: an omitted assignment has no index to look its name up by, so the
  // daemon sends the assignment's own context beside the refusal and it is
  // passed through here. The KEY IS DROPPED, never defaulted: a daemon that
  // predates the field and a daemon that sent an empty identity must not look
  // the same to the caller, or the compat branch below cannot be decided — and
  // an absent identity is exactly what makes the renderer fall back to today's
  // unnamed wording rather than to a blank.
  return [
    ...listed,
    // filter(Boolean) keeps the tolerance the previous non-destructuring form
    // had for free: destructuring a null entry THROWS, and this list feeds
    // sandboxPolicyNeedsAttention, which previously only read .length and so
    // survived one. The daemon drops nil refusals, so this is unreachable from
    // a current one; the point is that a future daemon sending one degrades to
    // a missing row rather than to a dead preview panel.
    ...(target.omitted_refusals || []).filter(Boolean).map(({ context, ...refusal }) => ({
      index: null,
      refusal,
      ...(context ? { context } : {}),
    })),
  ];
}

export function sandboxPredictionWarnings(prediction) {
  const capability = [];
  for (const target of prediction?.targets || []) {
    // A refused target has no axes to iterate. Reading them anyway would report
    // a fully-enforced profile, which is the most dangerous possible summary of
    // a target that cannot run the policy at all.
    const refusals = [
      target.refusal,
      ...(target.context_refusals || []),
      ...(target.omitted_refusals || []),
    ].filter(Boolean);
    // Prefixed, because these land in the same flat warning line as
    // "partially enforced" details. "This launch will be REFUSED" and "network
    // is partly enforced" are not the same news, and an unlabelled join makes
    // them read alike.
    capability.push(...refusals.map((refusal) => `Launch refused: ${refusal.message}`));
    if (target.refusal) continue;
    for (const axis of ['filesystem', 'environment', 'agent_directories', 'network', 'unix_sockets']) {
      const verdict = target.axes?.[axis];
      if (verdict && verdict.outcome !== 'enforced') capability.push(verdict.detail);
    }
  }
  const composition = (prediction?.contexts || []).flatMap((context) => context.notices || []);
  /* The Set is deliberate and deliberately DISAGREES with the profile editor.
     N contexts refusing for one reason render as N rows there — each naming its
     group, cap-omitted ones included, since TCL-913 carries the assignment's
     identity beside the refusal — because "which and how many assignments are
     affected" is scope
     the operator acts on. This is the spawn dialog's one-line summary, which has no
     room to carry that scope, so identical messages collapse. Do not "fix" one
     surface to match the other without deciding which question is being asked:
     the divergence is the answer to two different ones. */
  return { capability: [...new Set(capability)], composition };
}

function networkRuleLabel(entry = {}, mode = 'allow') {
  let selector = '';
  if (entry.host) selector = `host ${entry.host}`;
  if (entry.domain) selector = `domain ${entry.domain}${entry.include_subdomains ? ' and subdomains' : ''}`;
  // "network CIDR …" rather than "network network …": the selector name the
  // editor's own dropdown uses, so the row does not repeat the axis word.
  if (entry.cidr) selector = `CIDR ${entry.cidr}`;
  if (entry.loopback) selector = 'local machine';
  const ports = Array.isArray(entry.ports) ? entry.ports : [];
  const verb = mode === 'deny' ? 'Deny' : 'Allow';
  return `${verb} network: ${selector || 'configured destination'}${ports.length ? ` · port${ports.length === 1 ? '' : 's'} ${ports.join(', ')}` : ''}`;
}

/* `constructedRoot` says this target builds its own filesystem root instead of
   inheriting the host's. That is not a verdict on any rule the operator wrote —
   every one of them is still enforced — but it changes what EXISTS around them,
   so it is stated as its own rule row rather than left to warning prose. It
   mirrors the closing rows an allow list already gets ("Block all other network
   destinations", "Block all other Unix sockets"): the implicit half of a
   restricting posture, made visible next to the explicit half. */
function effectiveRuleRows(context = {}, constructedRoot = false) {
  const rows = [];
  for (const entry of context.filesystem || []) {
    const prefix = entry.access === 'write' ? 'Read/write'
      : entry.access === 'read' ? 'Read-only' : 'Block';
    // A remapped rule names the path the agent will actually see FIRST, with
    // the host directory the authority came from after it (TCL-866). This
    // preview is the canonical always-visible disclosure of the mapping — the
    // editor row's own control is a popover — so the rule line has to carry it
    // rather than reading like an ordinary same-path grant.
    const mountPath = (entry.mount_path || '').trim();
    const target = mountPath && mountPath !== entry.path
      ? `${mountPath} ← ${entry.path}` : entry.path;
    rows.push({
      axis: 'filesystem',
      label: `${prefix}: ${target}`,
      hasMountPath: target !== entry.path,
    });
  }
  for (const name of context.environment || []) {
    rows.push({ axis: 'environment', label: `Set environment: ${name}` });
  }
  for (const name of context.agent_directories || []) {
    rows.push({ axis: 'agent_directories', label: `Private read/write directory: $${name}` });
  }
  // Blocks are the one axis that is arbitrary shell rather than a rule, so the
  // preview names them and says what they promise to define, without pretending
  // a script can be summarised the way a grant can.
  for (const block of context.pre_launch || []) {
    if (!block || !block.name) continue;
    const exports = (block.exports || []).join(', ');
    rows.push({
      axis: 'pre_launch',
      label: exports
        ? `Pre-launch script: ${block.name} → ${exports}`
        : `Pre-launch script: ${block.name}`,
    });
  }

  const axes = sandboxAccessAxes(context);
  if (axes.network.mode === 'open') {
    rows.push({ axis: 'network', label: 'Allow outbound network' });
  } else if (axes.network.mode === 'closed') {
    rows.push({ axis: 'network', label: 'Block outbound network' });
  } else if (axes.network.mode === 'list') {
    if (axes.network.allow.length) {
      rows.push(...axes.network.allow.map((entry) => ({
        axis: 'network',
        label: networkRuleLabel(entry),
        networkKey: sandboxNetworkModeEntryKey('allow', entry),
      })));
      rows.push({ axis: 'network', label: 'Block all other network destinations' });
    } else {
      rows.push({ axis: 'network', label: 'Block outbound network (allow list is empty)' });
    }
  }
  rows.push(...(axes.network.deny || []).map((entry) => ({
    axis: 'network',
    label: networkRuleLabel(entry, 'deny'),
    networkKey: sandboxNetworkModeEntryKey('deny', entry),
  })));

  if (axes.unix_sockets.mode === 'open') {
    rows.push({ axis: 'unix_sockets', label: 'Allow Unix sockets' });
  } else if (axes.unix_sockets.mode === 'closed') {
    rows.push({ axis: 'unix_sockets', label: 'Block Unix sockets' });
  } else if (axes.unix_sockets.mode === 'list') {
    if (axes.unix_sockets.allow.length) {
      rows.push(...axes.unix_sockets.allow.map((entry) => ({
        axis: 'unix_sockets',
        label: `Allow Unix socket: ${entry.path_glob || entry.path || 'configured path'}`,
      })));
      rows.push({ axis: 'unix_sockets', label: 'Block all other Unix sockets' });
    } else {
      rows.push({ axis: 'unix_sockets', label: 'Block Unix sockets (allow list is empty)' });
    }
  }
  if (context.agentd_socket && axes.unix_sockets.mode !== 'open') {
    rows.push({ axis: 'control_socket', label: 'Allow Unix socket: tclaude agent control' });
  }
  if (constructedRoot) {
    rows.push({
      axis: 'filesystem',
      label: 'Block: every other host path (tclaude builds the sandbox root from the directory rules above plus a read-only OS surface)',
    });
  }
  return rows;
}

function bucketKey(outcome) {
  if (outcome === 'enforced') return 'applied';
  if (outcome === 'enforced_partial') return 'partial';
  return 'notApplied';
}

// Turns the evaluator's axis-oriented response into the operator's read model:
// concrete effective rules grouped only by whether this target supports them.
// `axes` should be the selected assignment's context_axes entry when present;
// callers fall back to the target-wide worst-case axes for older daemons.
// `refusal` short-circuits the whole bucketing: see sandboxTargetRefusal. A
// refused target produces none of the three VERDICT buckets rather than empty
// ones, because an empty "Fully supported rules" bucket is itself a verdict, and
// none was reached. The missing-axis fallback below must never be the path a
// refusal takes.
//
// TCL-915 (Option C, the operator's choice). The rules are still LISTED, in a
// fourth `unjudged` bucket that carries no verdict at all. It is deliberately
// not a fourth verdict: the alternative design put the rules in a "Blocked"
// group, which asserts each rule was judged and each was blocked, when
// evaluation never got that far. Listing without judging is the only honest
// shape, so this bucket says so in prose rather than leaving it to a colour.
//
// `bucketKey` cannot return 'unjudged', so the normal path can never route a
// judged rule here — this bucket is reachable only from the refusal branch.
export const SANDBOX_RULES_NOT_EVALUATED_NOTE = 'These are the rules this profile would apply. '
  + 'The target was refused before any of them was judged, so none carries a verdict.';

export function sandboxRuleBuckets(axes = {}, context = {}, networkEntries = [], refusal = null) {
  const buckets = {
    applied: { key: 'applied', label: 'Fully supported rules', rules: [], items: [], reasons: [], hasMountPath: false },
    partial: { key: 'partial', label: 'Partially supported rules', rules: [], items: [], reasons: [], hasMountPath: false },
    notApplied: { key: 'not-applied', label: 'Unsupported rules', rules: [], items: [], reasons: [], hasMountPath: false },
    unjudged: {
      key: 'unjudged', label: 'Rules not evaluated', rules: [], items: [], reasons: [],
      hasMountPath: false, note: SANDBOX_RULES_NOT_EVALUATED_NOTE,
    },
  };
  if (refusal) {
    /* Rows only — no verdict is read, and none is invented. `constructed_root`
       is a PREDICTION result, so a refused target's zero axes leave it false and
       the synthetic "tclaude builds the sandbox root" row is correctly absent:
       it describes what the evaluator worked out, and the evaluator never ran.
       Only the operator's own authored rules are listed. */
    for (const rule of effectiveRuleRows(context, axes?.constructed_root === true)) {
      if (rule.hasMountPath) buckets.unjudged.hasMountPath = true;
      buckets.unjudged.rules.push(rule.label);
      buckets.unjudged.items.push({
        label: rule.label,
        // A distinct outcome, NOT '' and not 'not_enforced'. Both of those are
        // read as verdicts downstream — '' ranks equal to 'enforced', and
        // 'not_enforced' claims the rule was checked and found unsupported.
        outcome: 'not_evaluated',
        detail: '',
      });
    }
    return { ...buckets, launchRefused: true, refusal };
  }
  const networkPredictions = new Map();
  for (const prediction of networkEntries || []) {
    for (const key of prediction.keys || []) networkPredictions.set(key, prediction);
  }
  const seenReasons = new Set();
  let launchRefused = false;
  for (const rule of effectiveRuleRows(context, axes?.constructed_root === true)) {
    const rowPrediction = rule.networkKey
      ? networkPredictions.get(rule.networkKey) : null;
    // control_socket and pre_launch have no daemon verdict, for opposite
    // reasons: the socket floor is always reachable, and a pre-launch block is
    // not an access rule at all — it is shell that always runs, so there is no
    // enforcement axis to predict. Without a synthetic verdict both fall into
    // the not_enforced fallback and the preview tells the operator their
    // working setup script is unsupported and will not be applied, which is
    // worse than not showing it.
    const verdict = rowPrediction || (rule.axis === 'control_socket' || rule.axis === 'pre_launch'
      ? { outcome: 'enforced', detail: '' }
      : axes?.[rule.axis] || { outcome: 'not_enforced', detail: 'No enforcement verdict was returned.' });
    const bucket = buckets[bucketKey(verdict.outcome)];
    // Recorded so a bucket that would otherwise ship collapsed can decide to
    // open: a remapped rule's mapping is only legible here (TCL-866).
    if (rule.hasMountPath) bucket.hasMountPath = true;
    bucket.rules.push(rule.label);
    bucket.items.push({
      label: rule.label,
      outcome: verdict.outcome,
      detail: verdict.detail || '',
    });
    if (verdict.outcome === 'refused') launchRefused = true;
    // Per-rule prediction detail belongs behind that row's keyboard-reachable
    // help affordance. Keep only target/axis-wide reasons visible beneath the
    // bucket, or an 8-row DNS policy repeats the same long caveat eight times.
    if (!rowPrediction && bucket !== buckets.applied && verdict.detail) {
      const identity = `${rule.axis}\0${verdict.outcome}\0${verdict.detail}`;
      if (!seenReasons.has(identity)) {
        seenReasons.add(identity);
        bucket.reasons.push({
          label: verdict.outcome === 'refused' ? 'Launch blocked'
            : verdict.outcome === 'enforced_partial' ? 'Limitation' : 'Unsupported',
          detail: verdict.detail,
        });
      }
    }
  }
  return { ...buckets, launchRefused, refusal: null };
}

const outcomeRank = {
  enforced: 0,
  enforced_partial: 1,
  not_enforced: 2,
  refused: 3,
};

// The selectable assignment uses context_axes, but the aggregate axes still
// guard every assignment, including contexts omitted from the selector.
export function sandboxOtherAssignmentWarnings(overallAxes = {}, selectedAxes = {}) {
  const labels = {
    filesystem: 'Directory rules',
    environment: 'Environment rules',
    agent_directories: 'Private-directory rules',
    pre_launch: 'Pre-launch scripts',
    network: 'Network rules',
    unix_sockets: 'Unix-socket rules',
  };
  return Object.entries(labels).flatMap(([axis, label]) => {
    const overall = overallAxes?.[axis];
    const selected = selectedAxes?.[axis];
    if (!overall || !selected || overall.outcome === 'enforced'
        || (outcomeRank[overall.outcome] ?? 2) <= (outcomeRank[selected.outcome] ?? 2)) {
      return [];
    }
    return [{ axis, label, outcome: overall.outcome, detail: overall.detail }];
  });
}

export function sandboxTargetLabel(value = {}) {
  const target = value.target || value;
  const harness = { claude: 'Claude', codex: 'Codex', opencode: 'OpenCode' }[target.harness] || target.harness || 'Harness';
  const platform = { linux: 'Linux', darwin: 'macOS' }[target.platform] || target.platform || 'current platform';
  const implementation = {
    'harness-builtin': 'built-in sandbox',
    'tclaude-layer': 'tclaude sandbox',
    stacked: 'stacked sandboxes',
  }[target.implementation] || target.implementation || 'default sandbox';
  const networkDisclosure = target.harness === 'codex'
    && target.implementation === 'harness-builtin'
    ? ` · ${CODEX_BUILTIN_FILTERED_NETWORK_SHORT}`
    : '';
  // Naming the implementation says who WOULD own containment, not that it is
  // switched on. Under Claude's `inherit` that second question is still open —
  // settings.json decides — so the label must not answer it.
  const undecided = target.harness === 'claude'
    && target.implementation === 'harness-builtin'
    && target.sandbox === 'inherit'
    ? ' (enabled only if Claude settings enable it)'
    : '';
  return `${harness} on ${platform} · ${implementation}${undecided}${networkDisclosure}`;
}

// The daemon's per-context axis is authoritative: the same authored rule can
// require a constructed root on Linux tclaude-layer and remain on the host root
// everywhere else. Keep the explanation close to the controls that caused the
// change, while leaving the effective-policy preview to describe the complete
// resolved filesystem posture.
export function sandboxConstructedRootWarning(prediction = {}, contextIndex = 0) {
  const targets = (prediction?.targets || []).filter((target) => {
    const axes = target.context_axes?.[contextIndex] || target.axes || {};
    return axes.constructed_root === true;
  });
  if (!targets.length) return null;

  const context = prediction?.contexts?.[contextIndex]?.context || {};
  const authoredNetwork = sandboxNetworkAuthoring(context);
  const axes = sandboxAccessAxes(context);
  const reasons = [];
  if (authoredNetwork.namespace === 'private') reasons.push('the private network namespace');
  if (authoredNetwork.baseline === 'deny' || ['closed', 'list'].includes(axes.network.mode)
      || authoredNetwork.deny_packs.length || (axes.network.deny || []).length) {
    reasons.push('the restricted network rules');
  }
  if (['closed', 'list'].includes(axes.unix_sockets.mode)) {
    reasons.push('the Unix-socket restriction');
  }

  return {
    reasons,
    targets: targets.map(sandboxTargetLabel),
  };
}
