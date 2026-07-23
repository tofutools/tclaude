/* The unsandboxed-autonomy probe (TCL-586), shared by every dialog that lets an
   operator pick a sandbox + approval posture — the spawn dialog and the
   profile/role editors.

   The browser cannot decide this itself: Claude's usual sandbox mode is
   `inherit`, whose meaning lives in settings.json files on the daemon's host.
   The daemon applies the same harness defaults a launch will and reads those
   files, so a select left on its blank default is answered for the posture it
   resolves to, and an explicit `off` is reported as unconfined regardless of
   which machine later runs the profile.

   dir scopes the settings search. The spawn dialog passes the launch CWD so
   project-level settings count; the profile editors pass "" because a profile
   is portable — only the machine-global tiers (managed policy + the user's
   ~/.claude/settings.json) are knowable at edit time, which is exactly what the
   daemon reads when dir is empty.

   A failed probe resolves to no warnings rather than rejecting: this is an
   advisory line beside the controls, and turning a transient fetch error into a
   blocked or alarming dialog would be worse than saying nothing. The Go side is
   the authority that still runs the check at spawn time, and its warning rides
   the spawn response either way. */
export async function fetchUnsandboxedAutonomy(fetchImpl, { harness = '', sandbox = '', approval = '', dir = '' } = {}) {
  const query = new URLSearchParams({ harness, sandbox, approval, dir });
  try {
    const response = await fetchImpl(`/api/spawn/effective-sandbox?${query}`, { credentials: 'same-origin' });
    if (!response.ok) return emptyAutonomy();
    const payload = await response.json().catch(() => ({}));
    return {
      warnings: Array.isArray(payload.warnings) ? payload.warnings : [],
      sandboxState: String(payload.sandbox_state || ''),
      sandboxSource: String(payload.sandbox_source || ''),
    };
  } catch (_) {
    return emptyAutonomy();
  }
}

function emptyAutonomy() {
  return { warnings: [], sandboxState: '', sandboxSource: '' };
}
