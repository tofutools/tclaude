export const TRI_OPTIONS = [
  ['', "Default (leave dialog's own)"], ['1', 'on'], ['0', 'off'],
];

export function triValue(value) {
  return value == null ? '' : value ? '1' : '0';
}

export function readTri(value) {
  return value === '' ? null : value === '1';
}

export function harnessByName(catalog, name) {
  return (catalog || []).find((entry) => entry.name === name) || null;
}

export function defaultHarness(catalog, requested = '') {
  if (requested && harnessByName(catalog, requested)) return requested;
  if (harnessByName(catalog, 'claude')) return 'claude';
  return catalog?.[0]?.name || requested || 'claude';
}

export function harnessDefaults(harness) {
  return {
    sandbox: harness?.default_sandbox || harness?.sandbox_modes?.[0] || '',
    approval: harness?.default_approval || harness?.approval_modes?.[0] || '',
    ask_user_question_timeout: harness?.default_ask_timeout || harness?.ask_timeout_modes?.[0] || '',
  };
}

export function profileDraft(seed = null, { editExisting = true, local = null } = {}, catalog = []) {
  const harness = defaultHarness(catalog, seed?.harness);
  const h = harnessByName(catalog, harness);
  const defaults = harnessDefaults(h);
  return {
    name: !local && editExisting ? seed?.name || '' : '', aliases_text: (seed?.aliases || []).join(', '), harness,
    disabled: !!seed?.disabled, disabled_reason: seed?.disabled_reason || '',
    model: seed?.model || '', effort: seed?.effort || '', sandbox: seed?.sandbox || defaults.sandbox,
    approval: seed?.approval || defaults.approval, ask_user_question_timeout: seed?.ask_user_question_timeout || defaults.ask_user_question_timeout,
    trust_dir: triValue(seed?.trust_dir), remote_control: triValue(seed?.remote_control),
    agent_name: seed?.agent_name || '', role: seed?.role || '', descr: seed?.descr || '',
    initial_message: seed?.initial_message || '', sync_worktree: triValue(seed?.sync_worktree),
    auto_focus: triValue(seed?.auto_focus), include_group_default_context: triValue(seed?.include_group_default_context),
    is_owner: triValue(seed?.is_owner), permission_overrides: { ...(seed?.permission_overrides || {}) },
  };
}

export function profilePayload(draft, original = null, catalog = [], { local = false } = {}) {
  const h = harnessByName(catalog, draft.harness);
  const body = {
    name: draft.name.trim(), harness: draft.harness, model: draft.model.trim(), effort: draft.effort,
    agent_name: draft.agent_name.trim(), role: draft.role.trim(), descr: draft.descr.trim(),
    initial_message: draft.initial_message, disabled: !!draft.disabled,
  };
  if (draft.disabled_reason.trim()) body.disabled_reason = draft.disabled_reason.trim();
  const aliases = String(draft.aliases_text || '').split(/[\n,]/).map((value) => value.trim()).filter(Boolean);
  if (aliases.length) body.aliases = [...new Set(aliases)];
  if (h?.can_sandbox && draft.sandbox) body.sandbox = draft.sandbox;
  const surfacesApproval = !!(h?.can_approval && h.approval_modes?.length);
  if (surfacesApproval && draft.approval) body.approval = draft.approval;
  if (h?.can_ask_timeout && h.ask_timeout_modes?.length && draft.ask_user_question_timeout) body.ask_user_question_timeout = draft.ask_user_question_timeout;
  const trust = draft.harness === 'codex' ? readTri(draft.trust_dir) : null;
  if (trust != null) body.trust_dir = trust;
  const remote = (!h || h.can_remote_control) ? readTri(draft.remote_control) : null;
  if (remote != null) body.remote_control = remote;
  for (const [key, value] of [['sync_worktree', draft.sync_worktree], ['auto_focus', draft.auto_focus], ['include_group_default_context', draft.include_group_default_context], ['is_owner', draft.is_owner]]) {
    const parsed = readTri(value); if (parsed != null) body[key] = parsed;
  }
  if (Object.keys(draft.permission_overrides).length) body.permission_overrides = { ...draft.permission_overrides };
  const norm = (name) => name || 'claude';
  if (original && norm(original.harness) === norm(draft.harness)) {
    if (!surfacesApproval && original.approval) body.approval = original.approval;
    if (original.auto_review != null) body.auto_review = original.auto_review;
  }
  if (local) {
    for (const key of ['name', 'aliases', 'disabled', 'disabled_reason', 'agent_name', 'role', 'descr', 'initial_message', 'sync_worktree', 'auto_focus', 'include_group_default_context']) delete body[key];
  }
  return body;
}

export function roleDraft(seed = null, catalog = []) {
  const harness = defaultHarness(catalog, seed?.harness);
  const h = harnessByName(catalog, harness);
  const defaults = harnessDefaults(h);
  return {
    name: seed?.name || '', descr: seed?.descr || '', brief: seed?.brief || '',
    harness, model: seed?.model || '', effort: seed?.effort || '',
    sandbox: seed?.sandbox || defaults.sandbox, approval: seed?.approval || defaults.approval, spawn_profile: seed?.spawn_profile || '',
    permissions: [...(seed?.permissions || [])],
  };
}

export function rolePayload(draft, catalog = []) {
  const h = harnessByName(catalog, draft.harness);
  const body = {
    name: draft.name.trim(), descr: draft.descr.trim(), brief: draft.brief, harness: draft.harness,
    model: draft.model.trim(), effort: draft.effort, spawn_profile: draft.spawn_profile.trim(),
    permissions: [...draft.permissions],
  };
  if (h?.can_sandbox && draft.sandbox) body.sandbox = draft.sandbox;
  if (h?.can_approval && h.approval_modes?.length && draft.approval) body.approval = draft.approval;
  return body;
}

export function dirtyDraft(draft, baseline) {
  return JSON.stringify(draft) !== JSON.stringify(baseline);
}
