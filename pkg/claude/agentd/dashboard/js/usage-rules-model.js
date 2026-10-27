// Pure helpers behind the "Usage limits & budgets" dialog (usage-rules.js).
// The daemon owns validation (usagerules.Rule.Validate); these only shape
// drafts, summarize rules and catch what a form can see before a round trip.

export const METRICS = Object.freeze([
  { id: 'total_usd', label: 'Cost (API or what-if)', hint: 'The Costs view figure: recorded API spend, else the subscription-equivalent estimate' },
  { id: 'real_usd', label: 'API spend only', hint: 'Recorded pay-as-you-go spend; subscription usage counts nothing' },
  { id: 'whatif_usd', label: 'What-if only', hint: 'The subscription-equivalent estimate only' },
  { id: 'window_pct', label: 'Quota window %', hint: 'A provider account quota window; global limits only' },
]);

export const PERIODS = Object.freeze([
  { id: 'day', label: 'per day' },
  { id: 'week', label: 'per week' },
  { id: 'month', label: 'per month' },
  { id: 'lifetime', label: 'agent lifetime' },
]);

export const ACTIONS = Object.freeze([
  { id: 'notify', label: 'Notify', verb: 'notify' },
  { id: 'wrap_up', label: 'Wrap up', verb: 'ask to wrap up and go idle' },
  { id: 'hard_stop', label: 'Hard stop', verb: 'stop the process' },
  { id: 'block_spawn', label: 'Block spawns', verb: 'block new spawns' },
]);

const label = (list, id) => list.find((item) => item.id === id)?.label || id;
export const metricLabel = (id) => label(METRICS, id);
export const periodLabel = (id) => (id === 'window' ? 'per window' : label(PERIODS, id));
export const actionLabel = (id) => label(ACTIONS, id);

// metricsFor / periodsFor / actionsFor narrow the choices to what a scope
// accepts: limits measure an aggregate, profile rules each agent.
export function metricsFor(scope) {
  return scope === 'global' ? METRICS : METRICS.filter((m) => m.id !== 'window_pct');
}
export function periodsFor(scope) {
  return scope === 'agent' ? PERIODS : PERIODS.filter((p) => p.id !== 'lifetime');
}
export function actionsFor(scope) {
  return scope === 'agent' ? ACTIONS.filter((a) => a.id !== 'block_spawn') : ACTIONS;
}

export function blankStep(at = 100) {
  return { at, action: 'notify', notify: ['operator'], repeat: '' };
}

export function blankRule(scope = 'agent') {
  return {
    name: '', metric: 'total_usd', period: scope === 'agent' ? 'lifetime' : 'month', window: '',
    budget: scope === 'agent' ? 20 : 500, dry_run: false,
    steps: [{ at: 80, action: 'notify', notify: scope === 'agent' ? ['agent', 'operator'] : ['operator'], repeat: '' },
      { at: 100, action: 'wrap_up', notify: [], repeat: '' }],
  };
}

// Presets: ready-made ladders for the common asks, editable before saving.
export const LIMIT_PRESETS = Object.freeze([
  {
    id: 'monthly', label: 'Monthly spend cap',
    rule: { name: 'Monthly spend', metric: 'total_usd', period: 'month', budget: 500, steps: [
      { at: 50, action: 'notify', notify: ['operator'] },
      { at: 80, action: 'notify', notify: ['operator', 'agent'] },
      { at: 100, action: 'wrap_up' },
      { at: 100, action: 'block_spawn' },
      { at: 120, action: 'hard_stop' },
    ] },
  },
  {
    id: 'daily', label: 'Daily spend cap',
    rule: { name: 'Daily spend', metric: 'total_usd', period: 'day', budget: 50, steps: [
      { at: 80, action: 'notify', notify: ['operator'] },
      { at: 100, action: 'notify', notify: ['operator', 'agent'] },
      { at: 100, action: 'block_spawn' },
    ] },
  },
  {
    id: 'window', label: 'Quota window guard',
    rule: { name: '5h quota', metric: 'window_pct', period: 'window', window: 'anthropic:five_hour', budget: 100, steps: [
      { at: 80, action: 'notify', notify: ['operator'] },
      { at: 95, action: 'block_spawn' },
    ] },
  },
]);

export const PROFILE_PRESETS = Object.freeze([
  {
    id: 'standard', label: 'Standard agent budget',
    rule: { name: 'Lifetime budget', metric: 'total_usd', period: 'lifetime', budget: 20, steps: [
      { at: 50, action: 'notify', notify: ['agent'] },
      { at: 80, action: 'notify', notify: ['agent', 'operator'], repeat_every_pct: 10 },
      { at: 100, action: 'wrap_up' },
      { at: 150, action: 'hard_stop' },
    ] },
  },
  {
    id: 'daily', label: 'Daily agent budget',
    rule: { name: 'Daily budget', metric: 'total_usd', period: 'day', budget: 10, steps: [
      { at: 80, action: 'notify', notify: ['agent', 'operator'] },
      { at: 100, action: 'wrap_up' },
    ] },
  },
  {
    id: 'watch', label: 'Watch only (dry run)',
    rule: { name: 'Watch', metric: 'total_usd', period: 'lifetime', budget: 20, dry_run: true, steps: [
      { at: 100, action: 'notify', notify: ['operator'] },
    ] },
  },
]);

// ruleToDraft turns a stored rule into the editor's form state; repeat is
// one text field ("10%" or "30m") so the two repeat kinds stay exclusive.
export function ruleToDraft(rule) {
  return {
    name: rule.name || '', metric: rule.metric || 'total_usd', period: rule.period || 'month',
    window: rule.window || '', budget: rule.budget ?? '', dry_run: !!rule.dry_run,
    steps: (rule.steps || []).map((step) => ({
      at: step.at, action: step.action || 'notify', notify: [...(step.notify || [])],
      repeat: step.repeat_every_pct ? `${step.repeat_every_pct}%` : step.repeat_every_minutes ? `${step.repeat_every_minutes}m` : '',
    })),
  };
}

// parseRepeat reads "10%" / "10" (percentage points) or "30m" (minutes).
export function parseRepeat(text) {
  const raw = String(text ?? '').trim().toLowerCase();
  if (!raw) return {};
  const minutes = raw.match(/^(\d+)\s*m(in)?$/);
  if (minutes) return { repeat_every_minutes: Number(minutes[1]) };
  const pct = raw.match(/^(\d+(?:\.\d+)?)\s*%?$/);
  if (pct) return { repeat_every_pct: Number(pct[1]) };
  throw new Error(`repeat "${text}" must look like 10% or 30m`);
}

// draftToRule builds the API rule from a draft, throwing on what the form
// can tell is wrong. scope narrows metric/period/action the same way the
// daemon will.
export function draftToRule(draft, scope) {
  const name = draft.name.trim();
  if (!name) throw new Error('give the rule a name');
  const windowPct = draft.metric === 'window_pct';
  if (windowPct && scope !== 'global') throw new Error('quota windows are account-wide; use them in a global limit');
  const budget = windowPct ? 100 : Number(draft.budget);
  if (!windowPct && !(Number.isFinite(budget) && budget > 0)) throw new Error(`${name}: budget must be a positive amount`);
  if (!draft.steps.length) throw new Error(`${name}: add at least one step`);
  const steps = draft.steps.map((step) => {
    const at = Number(step.at);
    if (!(Number.isFinite(at) && at > 0 && at <= 1000)) throw new Error(`${name}: step thresholds must be between 0 and 1000%`);
    if (step.action === 'block_spawn' && scope === 'agent') throw new Error(`${name}: block spawns applies to limits only`);
    if (step.action !== 'notify') return { at, action: step.action };
    if (!step.notify.length) throw new Error(`${name}: the notify step at ${at}% needs a recipient`);
    return { at, action: 'notify', notify: [...step.notify], ...parseRepeat(step.repeat) };
  });
  const rule = { name, metric: draft.metric, period: windowPct ? 'window' : draft.period, budget, steps };
  if (windowPct) rule.window = draft.window.trim();
  if (draft.dry_run) rule.dry_run = true;
  return rule;
}

export function fmtAmount(metric, value) {
  if (metric === 'window_pct') return `${Math.round(value ?? 0)}%`;
  const n = Number(value ?? 0);
  return `$${n >= 100 ? n.toFixed(0) : n.toFixed(2)}`;
}

export function stepSummary(step) {
  const who = step.action === 'notify' && step.notify?.length ? ` ${step.notify.join('+')}` : '';
  const repeat = step.repeat_every_pct ? ` every +${step.repeat_every_pct}%` : step.repeat_every_minutes ? ` every ${step.repeat_every_minutes}m` : '';
  return `${step.at}% ${actionLabel(step.action).toLowerCase()}${who}${repeat}`;
}

export function ruleSummary(rule) {
  const measured = rule.metric === 'window_pct'
    ? `${rule.window} quota`
    : `${fmtAmount(rule.metric, rule.budget)} ${periodLabel(rule.period)} · ${metricLabel(rule.metric)}`;
  return `${measured} — ${(rule.steps || []).map(stepSummary).join(', ')}${rule.dry_run ? ' (dry run)' : ''}`;
}

// LINKED_SOURCE marks an agent assignment copied from a spawn profile's
// link: it adds to the agent's group or global profiles, where the agent's
// own assignments replace them (db.UsageLinkedSourcePrefix).
export const LINKED_SOURCE = 'spawn profile ';
const linked = (a) => String(a.source || '').startsWith(LINKED_SOURCE);

// assignedNames lists the profile names assigned at one layer; for an agent,
// its own assignments (linkedNames lists the linked ones).
export function assignedNames(assignments, scope, scopeName = '') {
  return (assignments || [])
    .filter((a) => a.scope === scope && (scope === 'global' || a.scope_name === scopeName || a.scope_id === scopeName))
    .filter((a) => scope !== 'agent' || !linked(a))
    .map((a) => a.profile_name);
}

// linkedNames lists an agent's spawn-profile-linked profiles and the spawn
// profile they came from.
export function linkedNames(assignments, agentID) {
  const rows = (assignments || []).filter((a) => a.scope === 'agent' && a.scope_id === agentID && linked(a));
  return { names: rows.map((a) => a.profile_name), from: rows[0] ? rows[0].source.slice(LINKED_SOURCE.length) : '' };
}

// profileUses names where a profile is assigned, for its card.
export function profileUses(assignments, profileID) {
  return (assignments || []).filter((a) => a.profile_id === profileID).map((a) => {
    if (a.scope === 'global') return 'global default';
    const kind = { group: 'group', spawn_profile: 'spawn profile', template: 'template', agent: 'agent' }[a.scope] || a.scope;
    return `${kind} ${a.scope_name || a.scope_id}`;
  });
}

// agentReadings groups per-agent rule statuses under their agent, worst
// first, joined with any override (exempt / snooze / extensions).
export function agentReadings(overview) {
  const byAgent = new Map();
  for (const status of overview?.statuses || []) {
    if (!status.agent_id) continue;
    const entry = byAgent.get(status.agent_id) || { agentID: status.agent_id, title: status.agent_title || '', statuses: [], override: null };
    entry.statuses.push(status);
    byAgent.set(status.agent_id, entry);
  }
  for (const override of overview?.overrides || []) {
    const entry = byAgent.get(override.agent_id) || { agentID: override.agent_id, title: agentTitle(overview, override.agent_id), statuses: [], override: null };
    entry.override = override;
    byAgent.set(override.agent_id, entry);
  }
  const worst = (entry) => Math.max(0, ...entry.statuses.map((s) => s.pct || 0));
  return [...byAgent.values()].sort((a, b) => worst(b) - worst(a) || a.agentID.localeCompare(b.agentID));
}

export function agentTitle(overview, agentID) {
  return (overview?.agents || []).find((a) => a.id === agentID)?.title || '';
}

// barTone colors a reading: red once a stopping step fired, amber past a
// notify step or 80%, green otherwise.
export function barTone(status) {
  if (status.reached && status.reached !== 'notify') return 'over';
  if (status.reached || (status.pct || 0) >= 80) return 'warn';
  return 'ok';
}

// snoozeActive reports whether an override's snooze is still running.
export function snoozeActive(override, now = Date.now()) {
  const until = override?.snooze_until ? Date.parse(override.snooze_until) : NaN;
  return Number.isFinite(until) && until > now;
}

// --- fleet limits (usage-fleet.js) ---

// FLEET_PRESETS are the limit presets a fleet limit can take: spend caps
// (quota windows are one account's).
export const FLEET_PRESETS = Object.freeze(LIMIT_PRESETS.filter((p) => p.rule.metric !== 'window_pct'));

// fleetTargetOptions lists what a fleet limit can cover: every trusted node,
// a node pool (a sub-fleet) or one node.
export function fleetTargetOptions(fleet) {
  return [
    { value: 'all', label: 'All trusted nodes' },
    ...(fleet?.pools || []).map((p) => ({ value: `pool:${p.id}`, label: `Node pool ${p.name} (${p.members.length} node${p.members.length === 1 ? '' : 's'})` })),
    ...(fleet?.nodes || []).map((n) => ({ value: `peer:${n.node}`, label: `Node ${n.label}` })),
  ];
}

export const MEMBER_STATES = Object.freeze({
  self: 'this node', ok: 'counted', stale: 'stale', not_shared: 'shares no usage', offline: 'offline', error: 'error',
});

export const ENFORCE_STATES = Object.freeze({
  granted: 'acts on its agents', not_granted: 'alerts only', unknown: '', self: 'acts on its agents',
});

// nodeGrants reads which usage grants a peer holds for this node from its summary.
export function nodeGrants(node) {
  const grants = node?.summary?.grants || [];
  return { read: grants.includes('usage.read'), manage: grants.includes('usage-rules.manage'), enforce: grants.includes('usage-rules.enforce') };
}

// worstLimit is a summary's limit reading closest to (or past) its budget.
export function worstLimit(summary) {
  return (summary?.limits || []).reduce((worst, l) => (!worst || (l.pct || 0) > (worst.pct || 0) ? l : worst), null);
}
