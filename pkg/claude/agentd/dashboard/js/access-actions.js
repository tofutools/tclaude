export function createAccessActions({ requestMutation, confirm, notify, openGrant, state } = {}) {
  for (const [name, dependency] of Object.entries({ requestMutation, confirm, notify, openGrant })) {
    if (typeof dependency !== 'function') throw new TypeError(`access actions require ${name}`);
  }
  if (!state?.beginMutation) throw new TypeError('access actions require state');

  async function revoke(grant) {
    const key = `revoke:${grant.id}`;
    if (state.mutation.value.busy.has(key)) return false;
    const accepted = await confirm({
      title: 'Revoke sudo grant?',
      body: 'The agent loses access to this slug immediately. They can request again if needed.',
      meta: `#${grant.id} · ${grant.slug || ''}${grant.conv_title || grant.conv_id ? ' · ' + (grant.conv_title || grant.conv_id) : ''}`,
      okLabel: 'Revoke',
    });
    if (!accepted || !state.beginMutation(key)) return false;
    try {
      await requestMutation('/api/sudo/' + encodeURIComponent(grant.id), { method: 'DELETE' });
      state.endMutation(key);
      notify(`sudo revoked: ${grant.slug}`);
      return true;
    } catch (error) {
      state.failMutation(key, error);
      notify(`Revoke failed: ${error?.body || error?.message || error}`, true);
      return false;
    }
  }

  return Object.freeze({ revoke, openGrant });
}
