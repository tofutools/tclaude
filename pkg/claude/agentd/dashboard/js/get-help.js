// get-help.js -- "Get help" (tcl-z32qq1): the one way a page brings an agent in
// to help with what it shows. Either a new helper (a scoped scribe, POST
// /api/scribe) or an agent already running, picked by the operator; either way
// the agent opens in a regular overlay chat window (the companion's
// openInCompanion), with nothing page-specific in it. Without the companion it
// opens as a terminal tab instead.
//
// The page says what the helper is for: its scribe name, the permission slugs
// it needs, an optional scope for exact-scope reuse, the brief, and the grant
// confirmation the operator sees first. Nothing is granted or started without
// that confirmation.

import { companionAvailable, openInCompanion } from './companion-facade.js';
import { shellConfirm, shellToast } from './shell-state.js';

const json = { 'Content-Type': 'application/json' };

async function post(fetchImpl, path, body) {
  const response = await fetchImpl(path, { method: 'POST', credentials: 'same-origin', headers: json, body: JSON.stringify(body) });
  const text = await response.text().catch(() => '');
  let result = {};
  try { result = text ? JSON.parse(text) : {}; } catch { result = { message: text.trim() }; }
  if (!response.ok) throw new Error(result.message || result.error || `${response.status} ${response.statusText}`);
  return result;
}

// openHelperWindow shows an agent in its regular overlay chat window, or, where
// the page has no companion, as a terminal tab. starting says it was just
// started, so the window waits for it rather than refusing an agent the
// snapshot does not list yet. It answers where it opened: 'chat' or 'terminal'.
export async function openHelperWindow(agentId, label = '', { starting = false } = {}) {
  if (!agentId) return '';
  if (companionAvailable() && openInCompanion(agentId, { starting })) return 'chat';
  const { openWebWindowPane } = await import('./terminals-tab.js');
  await openWebWindowPane(agentId, label || agentId);
  return 'terminal';
}

// summonHelper starts a new helper, or reuses the live one scoped exactly the
// same, after the operator confirms the grant, and opens it. It answers
// { agentId, name, reused, opened }, or null when the operator declined.
// confirmation is the shell confirm's { title, body, meta, okLabel }; with
// none, a generic one names the slugs. Only a caller that already asked the
// operator itself says confirmed, so a missing field never skips the prompt.
export async function summonHelper({
  name, slugs, scope = null, exclusive = false, brief, taskRef = null, confirmation = null, confirmed = false,
  confirm = shellConfirm, fetchImpl = globalThis.fetch,
}) {
  if (!confirmed && !(await confirm(confirmation || helperConfirmation(name, slugs)))) return null;
  const result = await post(fetchImpl, '/api/scribe', {
    name, slugs, brief, exclusive,
    ...(scope ? { scope } : {}),
    ...(taskRef?.url ? { task_ref_url: taskRef.url, task_ref_label: taskRef.label || '' } : {}),
    // The chat window is its surface: no terminal opens for it on its own.
    background: true,
  });
  const agentId = result.agent_id || result.conv_id || '';
  if (!agentId) throw new Error('the summon returned no agent');
  const label = result.name || name;
  const opened = await openHelperWindow(agentId, label, { starting: !result.reused });
  return { agentId, convId: result.conv_id || '', name: label, reused: !!result.reused, opened };
}

// liveHelpers lists the agents a page may ask for help: running, local, and not
// retired, by title.
export function liveHelpers(snapshotValue) {
  const agents = Array.isArray(snapshotValue?.agents) ? snapshotValue.agents : [];
  return agents
    .filter((agent) => agent?.online && (agent.agent_id || agent.conv_id) && !agent.retired)
    .map((agent) => ({
      id: agent.agent_id || agent.conv_id,
      convId: agent.conv_id || '',
      label: (agent.title || '').trim() || agent.agent_id || agent.conv_id,
      groups: Array.isArray(agent.groups) ? agent.groups : [],
      effective: Array.isArray(agent.effective) ? agent.effective : [],
    }))
    .sort((a, b) => a.label.localeCompare(b.label));
}

// missingSlugs is what an agent lacks of slugs: only those are granted, so
// asking an agent never rewrites a grant it already holds, scoped or not.
export function missingSlugs(helper, slugs) {
  const held = new Set(helper?.effective || []);
  return (slugs || []).filter((slug) => !held.has(slug));
}

// helperConfirmation is the confirm a new helper gets when its page names none.
export function helperConfirmation(name, slugs = []) {
  return {
    title: 'Start a helper?',
    body: `This starts a new ${name} agent granted ${slugs.join(', ') || 'no extra permissions'}, and opens its chat window.`,
    meta: slugs.join(', '),
    okLabel: 'Grant & start',
  };
}

// grantConfirmation is the confirm a page that names none gets: what is granted,
// to whom, and that it lasts.
export function grantConfirmation(helper, missing) {
  return {
    title: 'Grant permissions?',
    body: `${helper.label} needs ${missing.join(', ')} to help here. Granting them is permanent until you revoke them in Access.`,
    meta: missing.join(', '),
    okLabel: 'Grant & ask',
  };
}

// askExistingAgent briefs a running agent and opens its chat window. When it
// lacks some of slugs, the operator confirms the grant first, as for a new
// helper, and only the missing slugs are granted. confirmation(missing) builds
// the confirm's { title, body, meta, okLabel }. It answers { agentId, name,
// granted, opened }, or null when the operator declined.
export async function askExistingAgent({
  helper, slugs = [], subject = 'Help wanted', brief, confirmation = null,
  confirm = shellConfirm, fetchImpl = globalThis.fetch,
}) {
  if (!helper?.id) return null;
  const missing = missingSlugs(helper, slugs);
  if (missing.length) {
    const ask = (typeof confirmation === 'function' ? confirmation(missing) : confirmation) || grantConfirmation(helper, missing);
    if (!(await confirm(ask))) return null;
    await post(fetchImpl, '/api/permissions', {
      conv: helper.id,
      overrides: Object.fromEntries(missing.map((slug) => [slug, 'grant'])),
    });
  }
  try {
    await post(fetchImpl, '/api/operator-message', { to: helper.convId || helper.id, subject, body: brief });
  } catch (error) {
    // The grant stands: say so, rather than leave it unmentioned.
    if (missing.length) throw new Error(`${error?.message || error} (${missing.join(', ')} ${missing.length === 1 ? 'was' : 'were'} granted to ${helper.label} anyway; revoke in Access if unwanted)`);
    throw error;
  }
  const opened = await openHelperWindow(helper.id, helper.label);
  return { agentId: helper.id, name: helper.label, granted: missing, opened };
}

// sendToHelper sends a running helper a message, as the operator, and shows its
// chat window: the way a page's context actions reach the agent helping it.
export async function sendToHelper(helper, { subject = 'Help wanted', body }, { fetchImpl = globalThis.fetch, notify = shellToast } = {}) {
  if (!helper?.id) return false;
  try {
    await post(fetchImpl, '/api/operator-message', { to: helper.convId || helper.id, subject, body });
  } catch (error) {
    notify(`Could not reach ${helper.label || helper.id}: ${error?.message || error}`, true);
    return false;
  }
  await openHelperWindow(helper.id, helper.label);
  return true;
}
