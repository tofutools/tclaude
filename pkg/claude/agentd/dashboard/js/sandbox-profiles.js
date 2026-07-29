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
    'Profile shape: filesystem entries are absolute-path rules with access "read", "write", or "deny". environment values are ordinary non-secret configuration — never credentials. agent_directories is an array of environment-variable names backed by isolated writable directories created at spawn. includes is an ordered array of other profiles composed first. New network drafts use baseline "deny", "allow", or "inherit". Only "deny" may carry packs (stable built-in pack IDs) and manual network.allow rows. Each manual row sets exactly one of host, domain (+ optional include_subdomains), cidr, or loopback, plus optional integer ports. Never copy a built-in pack\'s destinations into network.allow when its pack ID expresses the intent. unix_sockets remains an independent object whose mode is "open", "closed", "list", or omitted/unset; its allow rows set exactly one absolute path or a bounded path_glob, and ** is forbidden. The agentd socket floor is always reachable and is not an editable row. network_access and network.mode are legacy compatibility input only; author new drafts with network.baseline/packs/allow and unix_sockets. Access-list composition is intersection, so incompatible tiers may intentionally produce an empty allow set; surface that warning but do not treat it as invalid. The daemon remains authoritative for validation, pack expansion, and launch capability outcomes.',
    'Strictness has no separate mechanism: express it as ordinary rows — a broad "deny" (commonly the home directory) plus narrower "read"/"write" rows that reopen exactly what the agent needs. tclaude implicitly reopens only the workspace/worktree, the verified Git admin paths, the profile\'s own write grants, declared agent_directories, the agentd socket, and (on Codex, when its probe requires it) the Codex executable. Everything else you must enumerate: the harness state directory, the language runtime, and the toolchain.',
    'A read/write row beneath a deny row ("reopen-under-deny") is capability-gated at launch: it requires Claude Code sandbox mode "on", or Codex on Linux with the managed tclaude-agent profile and a verified split-policy probe. Claude "inherit"/"off", Codex on macOS, legacy Landlock, and raw Codex --sandbox modes are refused rather than silently running a strict-looking profile with a broad baseline. Note a bare home deny with no authored reopens is still gated this way, because tclaude adds the implicit reopens above.',
    'Pitfalls to raise with the human before proposing a home-wide deny. Rows must be DIRECTORIES: home-level dotfiles (~/.gitconfig, ~/.npmrc, shell rc files) cannot be reopened individually and stay denied — losing ~/.gitconfig costs Git its identity and credential helper. A row may not cover a directory containing a protected root, so ~/.claude cannot be reopened wholesale (reopen specific children like ~/.claude/plugins); ~/.codex is not protected and must be reopened for managed Codex agents. Reopening toolchain CACHES is not enough to build — the toolchain BINARIES (often under a version-manager root such as ~/.local/share/mise/installs) must be reopened too, or they become "command not found" despite being on $PATH. tclaude\'s own binary directory is NOT implicitly reopened; if it lives under the deny, the agent reaches the agentd socket but cannot run tclaude agent. On Linux, writes to denied paths have been observed to fail SILENTLY with exit 0 rather than erroring, so advise validating the profile on a throwaway agent before attaching real work.',
    'Protected tclaude/harness state (~/.tclaude/data, ~/.claude/sessions) is unreachable: no rule of any kind may name a path at or beneath it, and the daemon refuses a profile that tries. There is no exception mechanism — if the human asks for one, tell them the only way to work without that wall is to launch with the sandbox disabled.',
    targetName ? `This is a proposed replacement for the existing profile named "${targetName}".` : 'This is a proposed new sandbox profile.',
    `Starting draft:\n${JSON.stringify(seed, null, 2)}`,
    'Discuss the desired paths, access levels, network destinations/ports, Unix-socket posture/paths, environment names/values, included profiles, agent-owned directory variables, and profile name. Explain that target capability differs by implementation/harness/platform and the dashboard will run the authoritative prediction. Wait until the human agrees that the proposal is ready.',
    `Then write the complete profile JSON to a file and run exactly this draft handoff:\n\`tclaude agent sandbox-profiles draft --token ${token} --file <path>\``,
    'That command validates and returns the draft to the dashboard; it does NOT save anything.',
  ].join('\n\n');
}

function openSandboxProfileEditor(seed = null, options = {}) { return managementController().openSandboxProfileEditor(seed, options); }
function openSandboxProfilesManageModal() { return managementController().openSandboxProfilesManageModal(); }

const sandboxDraftQueue = createSandboxDraftQueue({
  canDeliver: () => !document.querySelector('#sandbox-profile-editor-modal'),
  deliver: ({ draft, targetName, onCreate, editorOptions }) => {
    openSandboxProfileEditor(draft.profile, { ...editorOptions, targetName, onCreate, notice: 'Agent draft loaded. Review every field, then explicitly save.' });
    toast('sandbox scribe draft ready — review and explicitly save');
  },
});

async function pollSandboxScribeDraft(token, targetName, onCreate, editorOptions) {
  const deadline = Date.now() + 30 * 60 * 1000;
  while (Date.now() < deadline) {
    try {
      const response = await fetch(`/api/sandbox-profile-drafts/${encodeURIComponent(token)}`, { credentials: 'same-origin' });
      if (response.ok) { const draft = await response.json(); const opened = sandboxDraftQueue.enqueue({ draft, targetName, onCreate, editorOptions }); if (!opened) toast(`sandbox scribe draft ready — queued for review (${sandboxDraftQueue.pendingCount()} waiting)`); return; }
      if (response.status !== 404) throw new Error((await response.text()) || `HTTP ${response.status}`);
    } catch (error) { toast(`sandbox draft handoff failed: ${error.message || String(error)}`, true); return; }
    await new Promise((resolve) => setTimeout(resolve, 1500));
  }
}

async function summonSandboxScribe(seed, targetName = '', onCreate = null, editorOptions = {}) {
  const token = sandboxScribeToken();
  try {
    const response = await fetch('/api/scribe', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: SANDBOX_SCRIBE_NAME, slugs: SANDBOX_SCRIBE_SLUGS, brief: sandboxScribeBrief(token, targetName, seed) }) });
    if (!response.ok) throw new Error((await response.text()) || `HTTP ${response.status}`);
    const result = await response.json().catch(() => ({})); const name = result.name || SANDBOX_SCRIBE_NAME;
    if (result.focus_mode === 'browser' && result.focus_ws) openTermModal({ wsPath: result.focus_ws, label: name, hideConv: result.conv_id || null });
    toast(`summoned ${name}${result.focus_mode === 'browser' ? ' — opened in-browser terminal' : ' — opening its terminal'}`); void pollSandboxScribeDraft(token, targetName, onCreate, editorOptions);
  } catch (error) { openSandboxProfileEditor(seed, { ...editorOptions, targetName, onCreate, notice: `Could not summon sandbox scribe: ${error.message || String(error)}` }); toast(error.message || String(error), true); }
}

function bindSandboxProfilesUI() {
  document.querySelector('#sandbox-profiles-manage-open')?.addEventListener('click', openSandboxProfilesManageModal);
}

export { bindSandboxProfilesUI, loadSandboxProfiles, openSandboxProfilesManageModal, openSandboxProfileEditor, summonSandboxScribe };
