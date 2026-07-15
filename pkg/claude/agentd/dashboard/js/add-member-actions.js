async function responseError(response, fallback) {
  return (await response.text()) || fallback || `HTTP ${response.status}`;
}

export async function loadAddMemberPromotionPool({ fetchImpl = fetch } = {}) {
  const response = await fetchImpl('/api/conversations', {
    credentials: 'same-origin',
  });
  if (!response.ok) {
    throw new Error(`load conversations failed: ${await responseError(response)}`);
  }
  const result = await response.json();
  return Array.isArray(result.rows) ? result.rows : [];
}

export async function addExistingMemberRequest({
  group,
  candidate,
  fetchImpl = fetch,
}) {
  const selector = candidate?.agent_id || candidate?.conv_id;
  if (!group || !selector) throw new Error('add member requires a group and candidate');
  const response = await fetchImpl(`/api/groups/${encodeURIComponent(group)}/members`, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ conv: selector }),
  });
  if (!response.ok) {
    throw new Error(`add failed: ${await responseError(response)}`);
  }
  return response.json().catch(() => ({}));
}
