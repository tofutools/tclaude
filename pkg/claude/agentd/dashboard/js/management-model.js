import { readReviewer, reviewerValue } from './approval-controls.js';

export const TRI_OPTIONS = [
  ['', "Default (leave dialog's own)"], ['1', 'on'], ['0', 'off'],
];

// Auto memory's unset case is NOT "leave the dialog's own default" — tclaude
// resolves an unset profile to OFF and injects CLAUDE_CODE_DISABLE_AUTO_MEMORY,
// because agents sharing a repo cross-pollute one project memory store. Label
// it so the operator can see what "Default" actually does here.
export const AUTO_MEMORY_TRI_OPTIONS = [
  ['', 'Default (off — recommended)'], ['1', 'on'], ['0', 'off'],
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
    tools: harness?.default_tools || harness?.tools_modes?.[0] || '',
    approval_reviewer: '',
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
    approval: seed?.approval || defaults.approval, tools: seed?.tools || defaults.tools,
    ask_user_question_timeout: seed?.ask_user_question_timeout || defaults.ask_user_question_timeout,
    auto_compact_window: seed?.auto_compact_window || '',
    // "" = unset, so the profile stays silent and lower spawn tiers still speak.
    sandbox_implementation: seed?.sandbox_implementation || '',
    // Set only by a harness switch that discarded a selection. Editor-local —
    // profilePayload never sends it.
    sandbox_implementation_cleared: null,
    approval_reviewer: reviewerValue(seed?.auto_review),
    trust_dir: triValue(seed?.trust_dir), remote_control: triValue(seed?.remote_control),
    auto_memory: triValue(seed?.auto_memory),
    ssh_workaround: seed?.ssh_workaround !== false,
    agent_name: seed?.agent_name || '', role: seed?.role || '', descr: seed?.descr || '',
    initial_message: seed?.initial_message || '', sync_worktree: triValue(seed?.sync_worktree),
    auto_focus: triValue(seed?.auto_focus), include_group_default_context: triValue(seed?.include_group_default_context),
    is_owner: triValue(seed?.is_owner), permission_overrides: { ...(seed?.permission_overrides || {}) },
    context_features: { ...(seed?.context_features || {}) },
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
  const surfacesTools = !!(h?.can_tools && h.tools_modes?.length);
  if (surfacesTools && draft.tools) body.tools = draft.tools;
  const reviewer = h?.can_auto_review ? readReviewer(draft.approval_reviewer) : null;
  if (reviewer != null) body.auto_review = reviewer;
  if (h?.can_ask_timeout && h.ask_timeout_modes?.length && draft.ask_user_question_timeout) body.ask_user_question_timeout = draft.ask_user_question_timeout;
  // Blank means "no window pinned", which is the column default — so an empty
  // field simply omits the key rather than sending "".
  if ((!h || h.can_auto_compact_window) && String(draft.auto_compact_window || '').trim()) {
    body.auto_compact_window = String(draft.auto_compact_window).trim();
  }
  // Blank omits the key: an untouched row must leave the profile silent rather
  // than pinning harness-builtin over whatever a lower spawn tier would supply.
  // Gated on the harness capability so a value cannot outlive a harness switch
  // that made it invalid.
  if ((!h || h.can_tclaude_layer) && String(draft.sandbox_implementation || '').trim()) {
    body.sandbox_implementation = String(draft.sandbox_implementation).trim();
  }
  const trust = (!h || h.can_dir_trust) ? readTri(draft.trust_dir) : null;
  if (trust != null) body.trust_dir = trust;
  const remote = (!h || h.can_remote_control) ? readTri(draft.remote_control) : null;
  if (remote != null) body.remote_control = remote;
  const autoMemory = (!h || h.can_auto_memory) ? readTri(draft.auto_memory) : null;
  if (autoMemory != null) body.auto_memory = autoMemory;
  if (h?.can_ssh_workaround) {
    body.ssh_workaround = draft.sandbox === 'tclaude-agent' && !!draft.ssh_workaround;
  }
  for (const [key, value] of [['sync_worktree', draft.sync_worktree], ['auto_focus', draft.auto_focus], ['include_group_default_context', draft.include_group_default_context], ['is_owner', draft.is_owner]]) {
    const parsed = readTri(value); if (parsed != null) body[key] = parsed;
  }
  if (Object.keys(draft.permission_overrides).length) body.permission_overrides = { ...draft.permission_overrides };
  // Only send trims the harness can actually deliver: a Codex profile carrying
  // Claude-Code trims would be a 400, and the editor hides the control there.
  if ((!h || h.can_context_features) && Object.keys(draft.context_features || {}).length) {
    body.context_features = { ...draft.context_features };
  }
  const norm = (name) => name || 'claude';
  if (original && norm(original.harness) === norm(draft.harness)) {
    if (!surfacesApproval && original.approval) body.approval = original.approval;
    if (!surfacesTools && original.tools) body.tools = original.tools;
    if (!h?.can_auto_review && original.auto_review != null) body.auto_review = original.auto_review;
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
    sandbox: seed?.sandbox || defaults.sandbox, approval: seed?.approval || defaults.approval,
    tools: seed?.tools || defaults.tools, spawn_profile: seed?.spawn_profile || '',
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
  if (h?.can_tools && h.tools_modes?.length && draft.tools) body.tools = draft.tools;
  return body;
}

export function dirtyDraft(draft, baseline) {
  return JSON.stringify(draft) !== JSON.stringify(baseline);
}
