import { formatJobInterval } from './jobs-format.js';

function text(value) { return String(value ?? ''); }

export function cronJobToPrefill(job = {}, { duplicate = false } = {}) {
  const groupTarget = job.target_kind === 'group';
  return {
    name: duplicate && job.name ? `${job.name}-copy` : text(job.name),
    owner: job.owner_agent || job.owner_conv || '',
    targetMode: groupTarget ? 'group' : 'solo',
    target: groupTarget ? '' : (job.target_agent || job.target_conv || ''),
    groupName: groupTarget ? text(job.group_name) : '',
    interval: formatJobInterval(job.interval_seconds) || '',
    cronExpr: text(job.cron_expr),
    subject: text(job.subject),
    body: text(job.body),
    actionKind: job.action_kind === 'spawn' ? 'spawn' : 'message',
    spawnProfile: text(job.spawn_profile),
    spawnRoles: Array.isArray(job.spawn_roles) ? [...job.spawn_roles] : [],
    spawnNameTemplate: text(job.spawn_name_template),
    spawnInstructionTemplate: text(job.spawn_instruction_template),
    spawnConcurrencyPolicy: ['Forbid', 'Replace', 'Allow'].includes(job.spawn_concurrency_policy)
      ? job.spawn_concurrency_policy : 'Forbid',
    spawnMaxLiveWorkers: Number(job.spawn_max_live_workers) || 1,
    spawnWorkerDeadlineSeconds: Number(job.spawn_worker_deadline_seconds) || 0,
    enabled: !!job.enabled,
    runImmediately: !!job.run_immediately,
    queueWhenOffline: !!job.queue_when_offline,
    role: groupTarget ? text(job.target_role) : '',
  };
}

export function createCronDraft(prefill = {}) {
  const mode = prefill.targetMode === 'group' ? 'group' : 'solo';
  const cronExpr = text(prefill.cronExpr);
  return {
    name: text(prefill.name),
    owner: text(prefill.owner),
    target: {
      mode,
      target: mode === 'solo' ? text(prefill.target) : '',
      groupName: mode === 'group' ? text(prefill.groupName) : '',
      scopeGroup: text(prefill.scopeGroup),
    },
    role: text(prefill.role),
    scheduleMode: cronExpr ? 'cron' : 'interval',
    interval: text(prefill.interval),
    cronExpr,
    subject: text(prefill.subject),
    body: text(prefill.body),
    actionKind: prefill.actionKind === 'spawn' ? 'spawn' : 'message',
    spawn: {
      profile: text(prefill.spawnProfile),
      roles: Array.isArray(prefill.spawnRoles) ? [...prefill.spawnRoles] : [],
      nameTemplate: text(prefill.spawnNameTemplate),
      instructionTemplate: text(prefill.spawnInstructionTemplate),
      concurrencyPolicy: ['Forbid', 'Replace', 'Allow'].includes(prefill.spawnConcurrencyPolicy)
        ? prefill.spawnConcurrencyPolicy : 'Forbid',
      maxLiveWorkers: Number(prefill.spawnMaxLiveWorkers) || 1,
      workerDeadlineSeconds: Number(prefill.spawnWorkerDeadlineSeconds) || 0,
    },
    enabled: prefill.enabled === undefined ? true : !!prefill.enabled,
    runImmediately: prefill.runImmediately === undefined ? false : !!prefill.runImmediately,
    queueWhenOffline: prefill.queueWhenOffline === undefined ? false : !!prefill.queueWhenOffline,
  };
}

export function cronDraftDirty(draft, initial) {
  return JSON.stringify(draft) !== JSON.stringify(initial);
}

export function cronTargetValue(target = {}) {
  if (target.mode === 'group') {
    return target.groupName ? `group:${target.groupName}` : '';
  }
  return text(target.target).trim();
}

export function validateCronDraft(dialog, draft) {
  if (draft.actionKind === 'spawn' && draft.target.mode !== 'group') {
    return { code: 'spawn-group', message: 'Spawn jobs require a group target.' };
  }
  const target = cronTargetValue(draft.target);
  if (!target) {
    if (draft.target.mode === 'group') return { code: 'group-target', message: 'Pick a group from the dropdown (or create one first via the Groups tab).' };
    if (draft.target.scopeGroup) return { code: 'scoped-target', message: 'This group has no members to nudge — switch to Group (multicast), or add a member to the group first.' };
    return { code: 'solo-target', message: 'Target is required — type an agt_ id / title / conv-id or use 🔍 to pick.' };
  }
  if (draft.actionKind === 'spawn') {
    if (!draft.spawn.profile.trim()) return { code: 'spawn-profile', message: 'Spawn profile is required.' };
    if (!draft.spawn.instructionTemplate.trim()) return { code: 'spawn-instruction', message: 'Spawn instruction template is required.' };
    if (!['Forbid', 'Replace', 'Allow'].includes(draft.spawn.concurrencyPolicy)) {
      return { code: 'spawn-concurrency', message: 'Concurrency policy must be Forbid, Replace, or Allow.' };
    }
    if (Number(draft.spawn.maxLiveWorkers) <= 0) return { code: 'spawn-max-live', message: 'Max live workers must be positive.' };
    if (Number(draft.spawn.workerDeadlineSeconds) < 0) return { code: 'spawn-deadline', message: 'Worker deadline cannot be negative.' };
  } else if (!draft.body) {
    return { code: 'body', message: 'Body is required (the message text the cron job sends).' };
  }
  if (draft.runImmediately && !draft.enabled) {
    return { code: 'immediate-disabled', message: 'Run immediately requires Enabled, so the requested first run can be delivered.' };
  }
  if (draft.scheduleMode === 'cron' && !draft.cronExpr.trim()) {
    return { code: 'cron-expr', message: 'Cron expression is required — type one (e.g. */5 * * * *) or switch back to Interval.' };
  }
  if (dialog.kind !== 'edit' && draft.scheduleMode === 'interval' && !draft.interval.trim()) {
    return { code: 'interval', message: 'Schedule is required — click a chip or type a custom duration.' };
  }
  if (dialog.kind === 'edit' && draft.scheduleMode === 'interval' && !draft.interval.trim() && dialog.originalExpr) {
    return { code: 'edit-interval', message: 'Type an interval (e.g. 10m) — switching away from the cron expression needs one.' };
  }
  return null;
}

export function buildCronMutation(dialog, draft) {
  const name = draft.name.trim();
  const owner = draft.owner.trim();
  const target = cronTargetValue(draft.target);
  const role = draft.target.mode === 'group' ? draft.role.trim() : '';
  const interval = draft.interval.trim();
  const cronExpr = draft.cronExpr.trim();
  const subject = draft.subject.trim();
  const spawnPayload = {
    action_kind: draft.actionKind,
    spawn_profile: draft.spawn.profile.trim(),
    spawn_roles: [...draft.spawn.roles],
    spawn_name_template: draft.spawn.nameTemplate,
    spawn_instruction_template: draft.spawn.instructionTemplate,
    spawn_concurrency_policy: draft.spawn.concurrencyPolicy,
    spawn_max_live_workers: Number(draft.spawn.maxLiveWorkers),
    spawn_worker_deadline_seconds: Number(draft.spawn.workerDeadlineSeconds),
  };

  if (dialog.kind === 'edit') {
    const payload = {
      name, body: draft.body, subject, enabled: draft.enabled,
      run_immediately: draft.runImmediately,
      queue_when_offline: draft.queueWhenOffline,
      ...spawnPayload,
    };
    if (owner) payload.owner = owner;
    if (draft.scheduleMode === 'cron') payload.cron_expr = cronExpr;
    else if (interval) {
      payload.interval = interval;
      payload.cron_expr = '';
    }
    if (draft.target.mode === 'solo' && target !== dialog.originalTarget) {
      payload.target = target;
      payload.group_id = 0;
    } else if (draft.target.mode === 'group') {
      payload.target = target;
    }
    if (draft.target.mode === 'group') payload.role = role;
    return { path: `/api/cron/${encodeURIComponent(dialog.id)}`, method: 'PATCH', payload };
  }

  const payload = {
    name, target, subject, body: draft.body, enabled: draft.enabled,
    run_immediately: draft.runImmediately,
    queue_when_offline: draft.queueWhenOffline,
    action_kind: draft.actionKind,
  };
  if (draft.actionKind === 'spawn') Object.assign(payload, spawnPayload);
  if (draft.scheduleMode === 'cron') payload.cron_expr = cronExpr;
  else payload.interval = interval;
  if (owner) payload.owner = owner;
  if (draft.target.mode === 'group' && role) payload.role = role;
  return { path: '/api/cron', method: 'POST', payload };
}

export function resetCronDraftForAnother(draft) {
  return {
    ...draft, name: '', subject: '', body: '',
    spawn: { ...draft.spawn, nameTemplate: '', instructionTemplate: '' },
  };
}

export function standingOrderToPrefill(order = {}) {
  const target = order.target || {};
  const groupTarget = target.kind === 'group';
  const globalTarget = target.kind === 'global';
  return {
    name: text(order.name),
    revision: Number(order.revision) || 0,
    rowVersion: Number(order.row_version) || 0,
    targetMode: globalTarget ? 'global' : (groupTarget ? 'group' : 'solo'),
    target: groupTarget || globalTarget ? '' : text(target.agent),
    groupName: groupTarget ? text(target.group_name || target.group_id) : '',
    role: groupTarget ? text(target.role) : '',
    summary: text(order.summary),
    triggerEvent: text(order.trigger?.event),
    hookSelectors: Array.isArray(order.trigger?.selectors)
      ? order.trigger.selectors.map((selector) => ({
        harness: text(selector?.harness), event: text(selector?.event),
      })) : [],
    sources: Array.isArray(order.trigger?.sources) ? [...order.trigger.sources] : [],
    matchField: text(order.trigger?.match_field),
    matchRegex: text(order.trigger?.match_regex),
    timing: text(order.timing),
    cadence: text(order.cadence),
    cooldownSeconds: Number(order.cooldown_seconds) || 0,
    debounceSeconds: Number(order.debounce_seconds) || 0,
    enabled: !!order.enabled,
  };
}

export function createStandingOrderDraft(prefill = {}) {
  const mode = ['global', 'group'].includes(prefill.targetMode)
    ? prefill.targetMode : 'solo';
  const sources = Array.isArray(prefill.sources) ? [...prefill.sources] : [];
  const hookSelectors = Array.isArray(prefill.hookSelectors)
    ? prefill.hookSelectors.map((selector) => ({
      harness: text(selector?.harness), event: text(selector?.event),
    })) : [];
  return {
    name: text(prefill.name),
    revision: Number(prefill.revision) || 0,
    rowVersion: Number(prefill.rowVersion) || 0,
    target: {
      mode,
      target: mode === 'solo' ? text(prefill.target) : '',
      groupName: mode === 'group' ? text(prefill.groupName) : '',
      scopeGroup: text(prefill.scopeGroup),
    },
    role: text(prefill.role),
    summary: text(prefill.summary),
    triggerEvent: ['user.prompt', 'tool.before', 'tool.after'].includes(prefill.triggerEvent)
      ? prefill.triggerEvent : 'session.start',
    triggerMode: hookSelectors.length ? 'native' : 'portable',
    hookSelectors,
    sourceMode: sources.length ? 'selected' : 'any',
    sources,
    matchField: text(prefill.matchField),
    matchRegex: text(prefill.matchRegex),
    timing: prefill.timing === 'next-turn' ? 'next-turn' : 'same-continuation',
    cadence: prefill.cadence === 'once-per-generation' ? 'once-per-generation' : 'always',
    cooldownSeconds: Number(prefill.cooldownSeconds) || 0,
    debounceSeconds: Number(prefill.debounceSeconds) || 0,
    enabled: prefill.enabled === undefined ? true : !!prefill.enabled,
  };
}

export function standingOrderDraftDirty(draft, initial) {
  return JSON.stringify(draft) !== JSON.stringify(initial);
}

export const STANDING_ORDER_MATCH_FIELDS = Object.freeze({
  'session.start': Object.freeze(['', 'cwd']),
  'user.prompt': Object.freeze(['', 'prompt', 'cwd']),
  'tool.before': Object.freeze(['', 'tool_name', 'tool_input', 'cwd']),
  'tool.after': Object.freeze(['', 'tool_name', 'tool_input', 'cwd']),
  'hook.event': Object.freeze(['', 'cwd']),
});

export function standingOrderTargetValue(target = {}) {
  if (target.mode === 'global') return 'global';
  return cronTargetValue(target);
}

export function validateStandingOrderDraft(dialog, draft) {
  const target = standingOrderTargetValue(draft.target);
  if (!target) {
    if (draft.target.mode === 'group') {
      return { code: 'group-target', message: 'Pick a group from the dropdown (or create one first via the Groups tab).' };
    }
    if (draft.target.scopeGroup) {
      return { code: 'scoped-target', message: 'This group has no members — switch to Group (multicast), or add a member first.' };
    }
    return { code: 'solo-target', message: 'Target is required — type a stable agt_ id / title or use 🔍 to pick.' };
  }
  if (!draft.name.trim()) return { code: 'name', message: 'Name is required.' };
  if (!draft.summary.trim()) return { code: 'summary', message: 'Instruction is required.' };
  if (draft.triggerMode === 'native' && draft.hookSelectors.length === 0) {
    return { code: 'hook-selectors', message: 'Select at least one harness hook.' };
  }
  if (draft.triggerMode !== 'native' && draft.triggerEvent === 'session.start' &&
      draft.sourceMode === 'selected' && draft.sources.length === 0) {
    return { code: 'sources', message: 'Select at least one session-boundary source, or choose Any source.' };
  }
  const effectiveTrigger = draft.triggerMode === 'native' ? 'hook.event' : draft.triggerEvent;
  if (!STANDING_ORDER_MATCH_FIELDS[effectiveTrigger]) {
    return { code: 'trigger', message: 'Pick a supported trigger event.' };
  }
  if (!STANDING_ORDER_MATCH_FIELDS[effectiveTrigger].includes(draft.matchField)) {
    return { code: 'match-field', message: 'Pick a match field supported by this trigger.' };
  }
  if (!!draft.matchField !== !!draft.matchRegex.trim()) {
    return { code: 'match-regex', message: 'Choose a field and enter its RE2 match expression together.' };
  }
  if (draft.matchRegex.length > 1024) {
    return { code: 'match-regex-length', message: 'Match expression must be at most 1024 characters.' };
  }
  if (/(\\[1-9]|\\k<|\(\?(?:[=!]|<[=!]))/.test(draft.matchRegex)) {
    return { code: 'match-regex-re2', message: 'RE2 does not support backreferences or look-around assertions.' };
  }
  if (!Number.isInteger(Number(draft.cooldownSeconds)) ||
      Number(draft.cooldownSeconds) < 0 ||
      Number(draft.cooldownSeconds) > 31536000) {
    return { code: 'cooldown', message: 'Minimum interval must be a whole number of seconds from 0 to 31536000.' };
  }
  if (!Number.isInteger(Number(draft.debounceSeconds)) ||
      Number(draft.debounceSeconds) < 0 ||
      Number(draft.debounceSeconds) > 86400) {
    return { code: 'debounce', message: 'Debounce must be a whole number of seconds from 0 to 86400.' };
  }
  if (Number(draft.debounceSeconds) > 0 && draft.timing !== 'next-turn') {
    return { code: 'debounce-timing', message: 'Trailing-edge debounce requires Next turn timing.' };
  }
  if (dialog.kind === 'edit' && !draft.rowVersion) {
    return { code: 'row-version', message: 'This order has no edit token; reload the Automations page and try again.' };
  }
  return null;
}

export function buildStandingOrderMutation(dialog, draft) {
  const payload = {
    name: draft.name.trim(),
    target: standingOrderTargetValue(draft.target),
    role: draft.target.mode === 'group' ? draft.role.trim() : '',
    summary: draft.summary.trim(),
    trigger_event: draft.triggerMode === 'native' ? 'hook.event' : draft.triggerEvent,
    hook_selectors: draft.triggerMode === 'native'
      ? draft.hookSelectors.map((selector) => ({ ...selector })) : [],
    sources: draft.triggerMode !== 'native' &&
      draft.triggerEvent === 'session.start' && draft.sourceMode !== 'any'
      ? [...draft.sources] : [],
    match_field: draft.matchField,
    match_regex: draft.matchRegex,
    timing: draft.timing,
    cadence: draft.cadence,
    cooldown_seconds: Number(draft.cooldownSeconds) || 0,
    debounce_seconds: Number(draft.debounceSeconds) || 0,
    enabled: draft.enabled,
  };
  if (dialog.kind === 'edit') {
    payload.row_version = draft.rowVersion;
    return {
      path: `/api/standing-orders/${encodeURIComponent(dialog.id)}`,
      method: 'PATCH',
      payload,
    };
  }
  return { path: '/api/standing-orders', method: 'POST', payload };
}
