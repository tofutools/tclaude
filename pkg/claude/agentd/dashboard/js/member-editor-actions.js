import { announceNodeChanged, nodeFetch } from './node-api.js';

async function responseError(response, fallback) {
  return (await response.text()) || fallback || `HTTP ${response.status}`;
}

export async function saveMemberEditorRequests({
  descriptor, changes, fetchImpl: pageFetch = fetch, notify = () => {}, refresh = () => {},
}) {
  // descriptor.node names another node's agent (the fused Groups view).
  const fetchImpl = nodeFetch(descriptor.node || '', { fetchImpl: pageFetch });
  const succeeded = [];
  const errors = [];
  const attempt = async (key, request, successMessage) => {
    try {
      const response = await request();
      if (!response.ok) throw new Error(await responseError(response));
      succeeded.push(key);
      notify(successMessage);
    } catch (error) {
      errors.push({ key, message: (error && error.message) || String(error) });
    }
  };
  if (changes.rename) {
    await attempt('rename', () => fetchImpl(
      `/api/agents/${encodeURIComponent(descriptor.agent)}/rename`, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(changes.rename),
      },
    ), changes.rename.auto
      ? `auto-rename nudge sent: ${descriptor.label}`
      : `renaming ${descriptor.label} → ${changes.rename.title}`);
  }
  if ('role' in changes || 'descr' in changes) {
    const body = {};
    if ('role' in changes) body.role = changes.role;
    if ('descr' in changes) body.descr = changes.descr;
    await attempt('membership', () => fetchImpl(
      `/api/groups/${encodeURIComponent(descriptor.group)}/members/${encodeURIComponent(descriptor.agent)}`, {
        method: 'PATCH', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      },
    ), `updated ${descriptor.label}`);
  }
  if ('tags' in changes) {
    await attempt('tags', () => fetchImpl(
      `/api/agents/${encodeURIComponent(descriptor.agent)}/tags`, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ tags: changes.tags }),
      },
    ), `tags updated: ${descriptor.label}`);
  }
  if ('owner' in changes) {
    const url = changes.owner
      ? `/api/groups/${encodeURIComponent(descriptor.group)}/owners`
      : `/api/groups/${encodeURIComponent(descriptor.group)}/owners/${encodeURIComponent(descriptor.agent)}`;
    await attempt('owner', () => fetchImpl(url, changes.owner ? {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ conv: descriptor.agent }),
    } : {
      method: 'DELETE', credentials: 'same-origin',
    }), changes.owner
      ? `${descriptor.label} is now an owner of ${descriptor.group}`
      : `${descriptor.label} is no longer an owner of ${descriptor.group}`);
  }
  if (succeeded.length) {
    announceNodeChanged(descriptor.node);
    void refresh();
  }
  return { succeeded, errors };
}
