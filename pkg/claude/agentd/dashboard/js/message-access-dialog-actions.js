function errorMessage(body, status) {
  if (body && typeof body === 'object') return body.error || body.message || `HTTP ${status}`;
  return String(body || `HTTP ${status}`);
}

async function requestJSON(fetchImpl, url, options) {
  const response = await fetchImpl(url, { credentials: 'same-origin', ...options });
  const text = await response.text();
  let body = null;
  try { body = text ? JSON.parse(text) : {}; } catch (_) { body = text; }
  if (!response.ok) {
    const error = new Error(errorMessage(body, response.status));
    error.status = response.status;
    error.code = body && typeof body === 'object' ? body.code || '' : '';
    error.body = body;
    throw error;
  }
  return body || {};
}

export function createMessageAccessDialogActions({
  fetchImpl = fetch,
  refresh = async () => {},
  notify = () => {},
  words = (plain) => plain,
} = {}) {
  async function sendMessage(payload) {
    const response = await requestJSON(fetchImpl, '/api/message', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    if (payload.to.startsWith('group:')) {
      const recipients = response.recipients || [];
      const queued = recipients.filter((item) => item.queued).length;
      notify(recipients.length
        ? `multicast queued for ${queued} member${queued === 1 ? '' : 's'} of ${response.via_group || payload.to}`
        : `no recipients reached in ${response.via_group || payload.to} — nothing sent`);
    } else {
      const ahead = (response.pending || 0) - 1;
      notify(ahead > 0
        ? `message queued in recipient inbox (${ahead} ahead in delivery queue)`
        : 'message queued in recipient inbox');
    }
    return response;
  }

  async function replyHuman({ id, body, label }) {
    const response = await requestJSON(fetchImpl, '/api/human-messages/reply', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id, body }),
    });
    notify(response.held
      ? `reply queued for ${label} — it’s mid-prompt, will see it when it resumes`
      : `reply sent to ${label}`);
    // The mutation is already accepted. Let the component close immediately;
    // a slow or stalled snapshot refresh must not leave this non-idempotent
    // reply surface busy and invite a duplicate retry.
    void refresh();
    return response;
  }

  async function grantSudo({ conv, slugs, duration, reason }) {
    const response = await requestJSON(fetchImpl, '/api/sudo', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ conv, slugs, duration, reason }),
    });
    const ok = (response.grants || []).filter((grant) => grant.id > 0).length;
    const failed = (response.grants || []).length - ok;
    notify(`Granted ${ok} slug${ok === 1 ? '' : 's'} to ${(response.agent_id || response.conv_id || conv).slice(0, 12)}` +
      (failed > 0 ? ` (${failed} failed)` : ''));
    // Match the legacy close-before-refresh behavior. Grant completion is
    // independent from the next dashboard snapshot arriving.
    void refresh();
    return response;
  }

  async function savePermissions(descriptor, selection) {
    if (descriptor.mode === 'agent') {
      const response = await requestJSON(fetchImpl, '/api/permissions', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ conv: descriptor.conv, overrides: selection }),
      });
      const changed = response.changed || 0;
      notify(`Permissions saved — ${changed} change${changed === 1 ? '' : 's'}`);
      await refresh();
      return response;
    }
    if (descriptor.mode === 'group') {
      const permissions = Object.keys(selection).filter((slug) => selection[slug] === 'grant');
      const response = await requestJSON(fetchImpl, `/api/groups/${encodeURIComponent(descriptor.group)}`, {
        method: 'PATCH', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ permissions }),
      });
      notify(words(
        `${descriptor.group}: ${permissions.length} group permission grant${permissions.length === 1 ? '' : 's'} saved`,
        `${descriptor.group}: ${permissions.length} party boon${permissions.length === 1 ? '' : 's'} bound`,
      ));
      await refresh();
      return response;
    }
    const kept = {};
    for (const [slug, effect] of Object.entries(selection)) {
      if (effect === 'grant' || effect === 'deny') kept[slug] = effect;
    }
    await descriptor.onSave?.(kept);
    return { overrides: kept };
  }

  return Object.freeze({ sendMessage, replyHuman, grantSudo, savePermissions });
}
