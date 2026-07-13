function clone(value) {
  return JSON.parse(JSON.stringify(value));
}

export function blankTemplateAgent() {
  return {
    name: '',
    role: '',
    descr: '',
    initial_message: '',
    is_owner: false,
    permissions: [],
    role_ref: '',
    spawn_profile: '',
    harness: '',
    model: '',
    effort: '',
    sandbox: '',
    approval: '',
    profile_inline: null,
    wave: 0,
  };
}

export function templateDraft(template = null) {
  return {
    name: template?.name || '',
    descr: template?.descr || '',
    default_context: template?.default_context || '',
    per_agent_worktrees: !!template?.per_agent_worktrees,
    agents: template
      ? (template.agents || []).map((agent) => ({
          ...blankTemplateAgent(),
          ...clone(agent),
          permissions: [...(agent.permissions || [])],
          profile_inline: agent.profile_inline
            ? clone(agent.profile_inline)
            : null,
          wave: agent.wave || 0,
        }))
      : [blankTemplateAgent()],
    work_pattern: (template?.work_pattern || []).map((step) => ({
      send_to: step.send_to || 'all',
      value: step.value || '',
    })),
    process: (template?.process || []).map((phase) => ({
      name: phase.name || '',
      roles: [...(phase.roles || [])],
      roles_text: (phase.roles || []).join(', '),
      criteria: phase.criteria || '',
    })),
    rhythms: (template?.rhythms || []).map((rhythm) => ({
      name: rhythm.name || '',
      target_role: rhythm.target_role || '',
      interval: rhythm.interval || '',
      cron_expr: rhythm.cron_expr || '',
      subject: rhythm.subject || '',
      body: rhythm.body || '',
    })),
    wave_max_wait: template?.wave_max_wait || 0,
  };
}

export function templatePayload(draft) {
  return {
    name: draft.name.trim(),
    descr: draft.descr.trim(),
    default_context: draft.default_context,
    per_agent_worktrees: !!draft.per_agent_worktrees,
    agents: draft.agents.map((agent) => ({
      ...agent,
      name: agent.name.trim(),
      role: agent.role.trim(),
      descr: agent.descr.trim(),
      role_ref: agent.role_ref.trim(),
      spawn_profile: agent.spawn_profile.trim(),
      wave: Number.parseInt(agent.wave, 10) || 0,
      permissions: [...(agent.permissions || [])],
      profile_inline: agent.profile_inline || null,
    })),
    work_pattern: draft.work_pattern.map((step) => ({
      send_to: step.send_to || 'all',
      value: step.value,
    })),
    process: draft.process.map((phase) => ({
      name: phase.name.trim(),
      roles: String(
        phase.roles_text ??
          (Array.isArray(phase.roles)
            ? phase.roles.join(', ')
            : phase.roles || ''),
      )
        .split(',')
        .map((role) => role.trim())
        .filter(Boolean),
      criteria: phase.criteria,
    })),
    rhythms: draft.rhythms.map((rhythm) => ({
      name: rhythm.name.trim(),
      target_role: rhythm.target_role.trim(),
      interval: rhythm.interval.trim(),
      cron_expr: rhythm.cron_expr.trim(),
      subject: rhythm.subject.trim(),
      body: rhythm.body,
    })),
    wave_max_wait: Number.parseInt(draft.wave_max_wait, 10) || 0,
  };
}

export function filterTemplates(templates, query) {
  const normalized = query.trim().toLowerCase();
  if (!normalized) return templates;
  return templates.filter(
    (template) =>
      [template.name, template.descr].some((value) =>
        String(value || '')
          .toLowerCase()
          .includes(normalized),
      ) ||
      (template.agents || []).some((agent) =>
        [agent.name, agent.role].some((value) =>
          String(value || '')
            .toLowerCase()
            .includes(normalized),
        ),
      ),
  );
}

export function templateWaveCount(template) {
  return new Set((template.agents || []).map((agent) => agent.wave || 0)).size;
}

export function agentHasLegacyLaunch(agent) {
  return (
    [
      agent.harness,
      agent.model,
      agent.effort,
      agent.sandbox,
      agent.approval,
    ].some(Boolean) || (agent.permissions || []).length > 0
  );
}

export function agentInheritsDeployDefault(agent) {
  const hasInline = [
    agent.harness,
    agent.model,
    agent.effort,
    agent.sandbox,
    agent.approval,
  ].some(Boolean);
  return !agent.role_ref && !hasInline && !agent.profile_inline;
}

export function effectiveTemplateOwner(agent, profiles) {
  let owner = false;
  const referenced =
    agent.spawn_profile &&
    profiles.find((profile) => profile.name === agent.spawn_profile);
  if (referenced && typeof referenced.is_owner === 'boolean')
    owner = referenced.is_owner;
  if (
    agent.profile_inline &&
    typeof agent.profile_inline.is_owner === 'boolean'
  )
    owner = agent.profile_inline.is_owner;
  return owner || !!agent.is_owner;
}

export function moveItem(items, index, direction) {
  const next = [...items];
  const target = index + direction;
  if (target < 0 || target >= next.length) return next;
  [next[index], next[target]] = [next[target], next[index]];
  return next;
}
