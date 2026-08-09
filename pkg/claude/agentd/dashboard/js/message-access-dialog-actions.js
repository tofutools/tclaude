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
  async function sendOperatorMessage(draft) {
    let attachmentToken = '';
    if (draft.files.length) {
      const form = new FormData();
      draft.files.forEach((file, index) => {
        form.append('file', file, file.name || `pasted-image-${index + 1}.png`);
      });
      const uploaded = await requestJSON(fetchImpl, '/api/spawn-attachments', {
        method: 'POST', body: form,
      });
      attachmentToken = uploaded.token || '';
    }
    return requestJSON(fetchImpl, '/api/operator-message', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(draft.allLive ? {
        to: '', subject: draft.subject, body: draft.body, all_live: true,
      } : {
        to: draft.to, subject: draft.subject, body: draft.body,
        attachment_token: attachmentToken,
      }),
    }).then((response) => {
      if (!draft.allLive) return response;
      const recipients = response.recipients || [];
      const queued = recipients.filter((item) => item.queued).length;
      const rejected = recipients.length - queued;
      notify(recipients.length
        ? `announcement saved for ${queued} live agent${queued === 1 ? '' : 's'}${rejected ? `; ${rejected} not queued` : ''}`
        : 'no live agents — nothing announced');
      return response;
    });
  }

  async function sendMessage(payload) {
    const response = await requestJSON(fetchImpl, '/api/message', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    if (payload.to.startsWith('group:') || Array.isArray(response.recipients)) {
      const recipients = response.recipients || [];
      const queued = recipients.filter((item) => item.queued).length;
      const rejected = recipients.length - queued;
      const overloaded = recipients.filter((item) => item.queue_full).length;
      const failed = rejected - overloaded;
      const rejectionParts = [];
      if (overloaded) rejectionParts.push(`${overloaded} not queued (target backlog full)`);
      if (failed) rejectionParts.push(`${failed} not queued (delivery error)`);
      notify(recipients.length
        ? `message saved for ${queued} recipient${queued === 1 ? '' : 's'}${rejected ? `; ${rejectionParts.join('; ')}` : ''}`
        : `no recipients reached in ${response.via_group || payload.to} — nothing sent`);
    } else {
      const ahead = (response.pending || 0) - 1;
      notify(ahead > 0
        ? `message saved in recipient inbox (${ahead} earlier pending)`
        : 'message saved in recipient inbox');
    }
    return response;
  }

  async function replyHuman({ id, body, label }) {
    const response = await requestJSON(fetchImpl, '/api/human-messages/reply', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id, body }),
    });
    notify(response.queued
      ? `reply queued for ${label}`
      : `reply sent to ${label}`);
    // The mutation is already accepted. Let the component close immediately;
    // a slow or stalled snapshot refresh must not leave this non-idempotent
    // reply surface busy and invite a duplicate retry.
    void refresh();
    return response;
  }

  async function grantSudo({ agentID, slugs, duration, reason }) {
    const response = await requestJSON(fetchImpl, '/api/sudo', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ agent_id: agentID, slugs, duration, reason }),
    });
    const ok = (response.grants || []).filter((grant) => grant.id > 0).length;
    const failed = (response.grants || []).length - ok;
    notify(`Granted ${ok} slug${ok === 1 ? '' : 's'} to ${(response.agent_id || agentID).slice(0, 12)}` +
      (failed > 0 ? ` (${failed} failed)` : ''));
    // Match the legacy close-before-refresh behavior. Grant completion is
    // independent from the next dashboard snapshot arriving.
    void refresh();
    return response;
  }

  // Per-slug grant scopes use the same union wire shape in every mode. The
  // fourth argument is group-only owner-bypass narrowing, a distinct concept
  // from the scopes attached to the group's explicit grants.
  async function savePermissions(descriptor, selection, scopes = {}, ownerScopes = null) {
    if (descriptor.mode === 'agent') {
      const response = await requestJSON(fetchImpl, '/api/permissions', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ conv: descriptor.conv, overrides: selection, scopes }),
      });
      const changed = response.changed || 0;
      notify(`Permissions saved — ${changed} change${changed === 1 ? '' : 's'}`);
      await refresh();
      return response;
    }
    if (descriptor.mode === 'group') {
      const permissions = Object.keys(selection).filter((slug) => selection[slug] === 'grant')
        .map((slug) => Object.hasOwn(scopes, slug) ? { slug, scope: scopes[slug] } : slug);
      // owner_scopes rides the same PATCH as the grants: both are permission
      // administration on the group and the endpoint gates them on the same
      // grant+revoke pair, so splitting them into two requests would only
      // create a window where one landed and the other did not. The caller
      // passes null when the box was not edited, and an omitted field means
      // "unchanged" — so a save that only flipped a grant can never clear a
      // narrowing. Note `{}` IS a meaningful value here (clear it), which is
      // why the test is against null rather than truthiness.
      const body = { permissions };
      if (ownerScopes !== null && ownerScopes !== undefined) body.owner_scopes = ownerScopes;
      const response = await requestJSON(fetchImpl, `/api/groups/${encodeURIComponent(descriptor.group)}`, {
        method: 'PATCH', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
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
      if (effect === 'grant' || effect === 'deny') {
        kept[slug] = effect === 'grant' && Object.keys(scopes[slug] || {}).length
          ? { effect, scope: scopes[slug] } : effect;
      }
    }
    await descriptor.onSave?.(kept);
    return { overrides: kept };
  }

  return Object.freeze({ sendMessage, sendOperatorMessage, replyHuman, grantSudo, savePermissions });
}
