// Sandbox-profile loading and scribe integration. The management DOM is owned
// by management-island.js; the spawn policy preview is plain model/actions code.
import { toast } from './refresh.js';
import { openTermModal } from './terminals-tab.js';
import { createSandboxDraftQueue } from './sandbox-draft-queue.js';
import { managementController } from './management-controller.js';

const API = '/api/sandbox-profiles';
const SANDBOX_SCRIBE_NAME = 'sandbox-scribe';
const SANDBOX_SCRIBE_SLUGS = ['sandbox-profiles.draft'];

async function api(path, options = {}) {
  const response = await fetch(path, { credentials: 'same-origin', ...options });
  if (!response.ok) {
    const raw = await response.text();
    try { const body = JSON.parse(raw); throw new Error(body.message || body.error || raw || `HTTP ${response.status}`); }
    catch (error) { if (error instanceof SyntaxError) throw new Error(raw || `HTTP ${response.status}`); throw error; }
  }
  if (response.status === 204) return null;
  return response.json().catch(() => ({}));
}

async function loadSandboxProfiles() {
  const list = await api(API);
  return Array.isArray(list) ? list : [];
}

function sandboxScribeToken() {
  if (globalThis.crypto?.randomUUID) return globalThis.crypto.randomUUID().replaceAll('-', '');
  return `${Date.now().toString(36)}${Math.random().toString(36).slice(2)}${Math.random().toString(36).slice(2)}`;
}

function sandboxScribeBrief(token, targetName, seed) {
  return [
    'You are a sandbox-profile scribe. Talk with the human to interactively design one filesystem/environment/network sandbox profile, including optional per-agent generated directories.',
    'Critical safety boundary: create a structured DRAFT only. Never create, edit, delete, assign, or apply a sandbox profile; never launch or relaunch an agent; never request sandbox-profiles.manage. Your purpose-specific permission is sandbox-profiles.draft.',
    'Environment values are ordinary non-secret configuration. Filesystem entries are absolute-path rules with access "read", "write", or "deny". network_access is "internet", "none", or omitted to inherit. Explicit network policies currently require the Codex managed sandbox and control external IP connectivity while preserving agentd; offline is supported on macOS and rejected on Linux/WSL until Codex can preserve the agentd Unix socket there. Managed Codex profiles block the host tmux server independently. agent_directories is an array of environment-variable names backed by isolated writable directories created at spawn. includes is an ordered array of other profiles composed first. read_baseline is omitted (harness default, broad reads) or "minimal" (strict opt-in read scope; strictest-wins when composed). read_baseline_exclusions is an optional array of stable semantic IDs selected from the server catalog; never invent IDs or persist resolved machine paths. Exclusions union across includes/scopes and home.directory covers every narrower Home leaf. break_glass_filesystem lists exact-path {path, access:"read"|"write"} rules for normally protected tclaude/harness state — an exceptional, dangerous debugging mechanism, never a recommended posture: only include it when the human explicitly asks for it, never suggest it yourself, and remind the human that saving or assigning it requires an explicit break-glass risk acknowledgement. The daemon remains authoritative for validation.',
    targetName ? `This is a proposed replacement for the existing profile named "${targetName}".` : 'This is a proposed new sandbox profile.',
    `Starting draft:\n${JSON.stringify(seed, null, 2)}`,
    'Discuss the desired paths, access levels, network posture, environment names/values, included profiles, agent-owned directory variables, and profile name. Wait until the human agrees that the proposal is ready.',
    `Then write the complete profile JSON to a file and run exactly this draft handoff:\n\`tclaude agent sandbox-profiles draft --token ${token} --file <path>\``,
    'That command validates and returns the draft to the dashboard; it does NOT save anything.',
  ].join('\n\n');
}

function openSandboxProfileEditor(seed = null, options = {}) { return managementController().openSandboxProfileEditor(seed, options); }
function openSandboxProfilesManageModal() { return managementController().openSandboxProfilesManageModal(); }

const sandboxDraftQueue = createSandboxDraftQueue({
  canDeliver: () => !document.querySelector('#sandbox-profile-editor-modal'),
  deliver: ({ draft, targetName, onCreate }) => {
    openSandboxProfileEditor(draft.profile, { targetName, onCreate, notice: 'Agent draft loaded. Review every field, then explicitly save.' });
    toast('sandbox scribe draft ready — review and explicitly save');
  },
});

async function pollSandboxScribeDraft(token, targetName, onCreate) {
  const deadline = Date.now() + 30 * 60 * 1000;
  while (Date.now() < deadline) {
    try {
      const response = await fetch(`/api/sandbox-profile-drafts/${encodeURIComponent(token)}`, { credentials: 'same-origin' });
      if (response.ok) { const draft = await response.json(); const opened = sandboxDraftQueue.enqueue({ draft, targetName, onCreate }); if (!opened) toast(`sandbox scribe draft ready — queued for review (${sandboxDraftQueue.pendingCount()} waiting)`); return; }
      if (response.status !== 404) throw new Error((await response.text()) || `HTTP ${response.status}`);
    } catch (error) { toast(`sandbox draft handoff failed: ${error.message || String(error)}`, true); return; }
    await new Promise((resolve) => setTimeout(resolve, 1500));
  }
}

async function summonSandboxScribe(seed, targetName = '', onCreate = null) {
  const token = sandboxScribeToken();
  try {
    const response = await fetch('/api/scribe', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: SANDBOX_SCRIBE_NAME, slugs: SANDBOX_SCRIBE_SLUGS, brief: sandboxScribeBrief(token, targetName, seed) }) });
    if (!response.ok) throw new Error((await response.text()) || `HTTP ${response.status}`);
    const result = await response.json().catch(() => ({})); const name = result.name || SANDBOX_SCRIBE_NAME;
    if (result.focus_mode === 'browser' && result.focus_ws) openTermModal({ wsPath: result.focus_ws, label: name, hideConv: result.conv_id || null });
    toast(`summoned ${name}${result.focus_mode === 'browser' ? ' — opened in-browser terminal' : ' — opening its terminal'}`); void pollSandboxScribeDraft(token, targetName, onCreate);
  } catch (error) { openSandboxProfileEditor(seed, { targetName, onCreate, notice: `Could not summon sandbox scribe: ${error.message || String(error)}` }); toast(error.message || String(error), true); }
}

function bindSandboxProfilesUI() {
  document.querySelector('#sandbox-profiles-manage-open')?.addEventListener('click', openSandboxProfilesManageModal);
}

export { bindSandboxProfilesUI, loadSandboxProfiles, openSandboxProfilesManageModal, openSandboxProfileEditor, summonSandboxScribe };
