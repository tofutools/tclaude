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
  const fs = profile.filesystem || []; const env = profile.environment || []; const inc = profile.includes || []; const own = profile.agent_directories || [];
  const parts = [['read', 'read'], ['write', 'write'], ['deny', 'deny']].flatMap(([access, label]) => { const count = fs.filter((entry) => entry.access === access).length; return count ? [`${count} ${label}`] : []; });
  if (inc.length) parts.push(`${inc.length} include${inc.length === 1 ? '' : 's'}`);
  if (env.length) parts.push(`${env.length} env key${env.length === 1 ? '' : 's'}`);
  if (own.length) parts.push(`${own.length} agent dir${own.length === 1 ? '' : 's'}`);
  const axes = sandboxAccessAxes(profile);
  if (axes.network.mode) parts.push(`network ${axes.network.mode}${axes.network.mode === 'list' ? ` (${axes.network.allow.length})` : ''}`);
  if (axes.unix_sockets.mode) parts.push(`sockets ${axes.unix_sockets.mode}${axes.unix_sockets.mode === 'list' ? ` (${axes.unix_sockets.allow.length})` : ''}`);
  return parts.join(' · ') || 'no sandbox rules';
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

export function sandboxProfileForWire(draft) {
  const value = structuredClone(draft || {});
  if (!Object.hasOwn(value, 'network') && !Object.hasOwn(value, 'unix_sockets')) return value;
  value.network ||= { mode: '', allow: [] };
  value.unix_sockets ||= { mode: '', allow: [] };
  value.network.allow = (value.network.allow || []).map((entry) => ({
    ...(entry.host ? { host: entry.host } : {}),
    ...(entry.domain ? { domain: entry.domain, include_subdomains: !!entry.include_subdomains } : {}),
    ...(entry.cidr ? { cidr: entry.cidr } : {}),
    ...(entry.loopback ? { loopback: true } : {}),
    ...(portsForWire(entry.ports).length ? { ports: portsForWire(entry.ports) } : {}),
  }));
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
  const axes = sandboxAccessAxes(draft);
  if (!['', 'open', 'closed', 'list'].includes(axes.network.mode)) errors.push('Network mode is invalid.');
  if (!['', 'open', 'closed', 'list'].includes(axes.unix_sockets.mode)) errors.push('Unix-socket mode is invalid.');
  if (axes.network.mode !== 'list' && axes.network.allow.length) errors.push('Network entries require Access list mode.');
  if (axes.unix_sockets.mode !== 'list' && axes.unix_sockets.allow.length) errors.push('Unix-socket entries require Access list mode.');
  axes.network.allow.forEach((entry, index) => {
    const selectors = ['host', 'domain', 'cidr'].filter((key) => entry[key]).length + (entry.loopback ? 1 : 0);
    if (selectors !== 1) errors.push(`Network row ${index + 1} must set exactly one selector.`);
    if (entry.host && dnsError(entry.host)) errors.push(`Network row ${index + 1} host ${dnsError(entry.host)}.`);
    if (entry.domain && dnsError(entry.domain)) errors.push(`Network row ${index + 1} domain ${dnsError(entry.domain)}.`);
    if (entry.cidr && !/^([0-9a-f:.]+)\/\d{1,3}$/i.test(entry.cidr)) errors.push(`Network row ${index + 1} CIDR is invalid.`);
    const rawPorts = Array.isArray(entry.ports) ? entry.ports : String(entry.ports || '').split(',').filter((part) => part.trim());
    if (rawPorts.some((port) => !/^\d+$/.test(String(port).trim()) || Number(port) < 1 || Number(port) > 65535)) {
      errors.push(`Network row ${index + 1} ports must be comma-separated values from 1 to 65535.`);
    }
  });
  axes.unix_sockets.allow.forEach((entry, index) => {
    const value = entry.path_glob || entry.path || '';
    if (!value.startsWith('/')) errors.push(`Unix-socket row ${index + 1} must be an absolute path.`);
    if (value.includes('**')) errors.push(`Unix-socket row ${index + 1} must not contain **.`);
    if (entry.path_glob && !entry.path_glob.includes('*')) errors.push(`Unix-socket row ${index + 1} glob must contain *.`);
  });
  return errors;
}

export function sandboxPredictionWarnings(prediction) {
  const capability = [];
  for (const target of prediction?.targets || []) {
    for (const axis of ['filesystem', 'environment', 'agent_directories', 'network', 'unix_sockets']) {
      const verdict = target.axes?.[axis];
      if (verdict && verdict.outcome !== 'enforced') capability.push(verdict.detail);
    }
  }
  const composition = (prediction?.contexts || []).flatMap((context) => context.notices || []);
  return { capability: [...new Set(capability)], composition };
}

function networkRuleLabel(entry = {}) {
  let selector = '';
  if (entry.host) selector = `host ${entry.host}`;
  if (entry.domain) selector = `domain ${entry.domain}${entry.include_subdomains ? ' and subdomains' : ''}`;
  if (entry.cidr) selector = `network ${entry.cidr}`;
  if (entry.loopback) selector = 'local machine';
  const ports = Array.isArray(entry.ports) ? entry.ports : [];
  return `Allow network: ${selector || 'configured destination'}${ports.length ? ` · port${ports.length === 1 ? '' : 's'} ${ports.join(', ')}` : ''}`;
}

function effectiveRuleRows(context = {}) {
  const rows = [];
  for (const entry of context.filesystem || []) {
    const prefix = entry.access === 'write' ? 'Read/write'
      : entry.access === 'read' ? 'Read-only' : 'Block';
    rows.push({ axis: 'filesystem', label: `${prefix}: ${entry.path}` });
  }
  for (const name of context.environment || []) {
    rows.push({ axis: 'environment', label: `Set environment: ${name}` });
  }
  for (const name of context.agent_directories || []) {
    rows.push({ axis: 'agent_directories', label: `Private read/write directory: $${name}` });
  }

  const axes = sandboxAccessAxes(context);
  if (axes.network.mode === 'open') {
    rows.push({ axis: 'network', label: 'Allow outbound network' });
  } else if (axes.network.mode === 'closed') {
    rows.push({ axis: 'network', label: 'Block outbound network' });
  } else if (axes.network.mode === 'list') {
    if (axes.network.allow.length) {
      rows.push(...axes.network.allow.map((entry) => ({ axis: 'network', label: networkRuleLabel(entry) })));
      rows.push({ axis: 'network', label: 'Block all other network destinations' });
    } else {
      rows.push({ axis: 'network', label: 'Block outbound network (allow list is empty)' });
    }
  }

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
  return rows;
}

function bucketKey(outcome) {
  if (outcome === 'enforced') return 'applied';
  if (outcome === 'enforced_partial') return 'partial';
  return 'notApplied';
}

// Turns the evaluator's axis-oriented response into the operator's read model:
// concrete effective rules grouped only by whether this target applies them.
// `axes` should be the selected assignment's context_axes entry when present;
// callers fall back to the target-wide worst-case axes for older daemons.
export function sandboxRuleBuckets(axes = {}, context = {}) {
  const buckets = {
    applied: { key: 'applied', label: 'Rules fully applied', rules: [], reasons: [] },
    partial: { key: 'partial', label: 'Rules partially applied', rules: [], reasons: [] },
    notApplied: { key: 'not-applied', label: 'Rules not applied', rules: [], reasons: [] },
  };
  const seenReasons = new Set();
  let launchRefused = false;
  for (const rule of effectiveRuleRows(context)) {
    const verdict = rule.axis === 'control_socket'
      ? { outcome: 'enforced', detail: '' }
      : axes?.[rule.axis] || { outcome: 'not_enforced', detail: 'No enforcement verdict was returned.' };
    const bucket = buckets[bucketKey(verdict.outcome)];
    bucket.rules.push(rule.label);
    if (verdict.outcome === 'refused') launchRefused = true;
    if (bucket !== buckets.applied && verdict.detail) {
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
  return { ...buckets, launchRefused };
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
  return `${harness} on ${platform} · ${implementation}`;
}
