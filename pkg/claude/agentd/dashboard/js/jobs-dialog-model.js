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
    enabled: !!job.enabled,
    runImmediately: !!job.run_immediately,
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
    enabled: prefill.enabled === undefined ? true : !!prefill.enabled,
    runImmediately: prefill.runImmediately === undefined ? false : !!prefill.runImmediately,
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
  const target = cronTargetValue(draft.target);
  if (!target) {
    if (draft.target.mode === 'group') return { code: 'group-target', message: 'Pick a group from the dropdown (or create one first via the Groups tab).' };
    if (draft.target.scopeGroup) return { code: 'scoped-target', message: 'This group has no members to nudge — switch to Group (multicast), or add a member to the group first.' };
    return { code: 'solo-target', message: 'Target is required — type an agt_ id / title / conv-id or use 🔍 to pick.' };
  }
  if (!draft.body) return { code: 'body', message: 'Body is required (the message text the cron job sends).' };
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

  if (dialog.kind === 'edit') {
    const payload = {
      name, body: draft.body, subject, enabled: draft.enabled,
      run_immediately: draft.runImmediately,
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
  };
  if (draft.scheduleMode === 'cron') payload.cron_expr = cronExpr;
  else payload.interval = interval;
  if (owner) payload.owner = owner;
  if (draft.target.mode === 'group' && role) payload.role = role;
  return { path: '/api/cron', method: 'POST', payload };
}

export function resetCronDraftForAnother(draft) {
  return { ...draft, name: '', subject: '', body: '' };
}
