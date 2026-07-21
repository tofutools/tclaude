// Shared break-glass vocabulary for sandbox profiles. break_glass_filesystem
// is the only representation allowed to touch protected tclaude/harness state
// (~/.tclaude/data, ~/.claude/sessions, ~/.codex); every surface that can
// attach such a profile to an agent — editor, import, global/group assignment,
// explicit spawn selection — must render the same concrete consequences and
// collect an explicit acknowledgement (wire field: break_glass_acknowledged).
// The daemon independently rejects unacknowledged commits with the typed 422
// code "break_glass_acknowledgement_required"; the UI gate is the first line,
// not the enforcement boundary.
import { composeSandboxProfilePolicy } from './sandbox-profile-preview.js';

export const BREAK_GLASS_WARNING = 'Break-glass rules expose protected tclaude/harness state that ordinary sandbox rules must never touch. An agent with this access can read operator credentials and session state (~/.tclaude/data, ~/.claude/sessions, ~/.codex), corrupt the daemon’s SQLite/config/runtime state, bypass or invalidate agent-authorization assumptions, take control of other sessions through host-control sockets, and break the daemon or harness. Write access is materially more dangerous than read. This is an exceptional, operator-only debugging mechanism — never a routine or recommended posture.';

export function breakGlassRules(profile) {
  return Array.isArray(profile?.break_glass_filesystem) ? profile.break_glass_filesystem : [];
}

export function describeBreakGlassEntries(entries) {
  return entries.map((entry) => `${entry.access} ${entry.path}${entry.origins?.length ? ` (${entry.origins.join(', ')})` : ''}`).join(' · ');
}

// assignedBreakGlass reports every break-glass rule the named profile would
// carry once assigned at the given scope, flattening includes so a rule an
// included profile contributes is attributed to its origin rather than hidden.
export function assignedBreakGlass(name, profiles, scope) {
  const byName = Object.fromEntries((profiles || []).map((profile) => [profile.name, profile]));
  const profile = byName[name];
  if (!profile) return [];
  return composeSandboxProfilePolicy([{ scope, profile }], byName).breakGlass;
}

// breakGlassAssignmentPrompt shapes the shell confirmation for the persistent
// assignment surfaces (global default, group default). The scopeLabel spells
// out the persistence: every future launch under that scope inherits the
// protected access until the assignment is removed.
export function breakGlassAssignmentPrompt({ scopeLabel, name, entries }) {
  return {
    title: '\u{1f6a8} Assign break-glass sandbox profile?',
    body: `${scopeLabel} This profile carries break-glass protected-path access: ${describeBreakGlassEntries(entries)}. ${BREAK_GLASS_WARNING}`,
    meta: name,
    okLabel: 'I understand the risk — assign',
  };
}
