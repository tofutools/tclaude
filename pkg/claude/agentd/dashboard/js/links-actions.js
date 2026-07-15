async function responseError(response) {
  return (await response.text()) || `HTTP ${response.status}`;
}

async function request(fetchImpl, url, options = {}) {
  const response = await fetchImpl(url, { credentials: 'same-origin', ...options });
  if (!response.ok) throw new Error(await responseError(response));
  try { return await response.json(); } catch (_) { return {}; }
}

// Link API mutations stay plain and injectable. Components own drafts,
// validation, busy/error presentation and focus; these actions own only the
// daemon request plus the successful close/feedback/refresh boundary.
export function createLinksActions({
  state,
  fetchImpl = globalThis.fetch,
  refresh = async () => {},
  confirm = async () => true,
  notify = () => {},
  words = (plain) => plain,
}) {
  if (!state) throw new TypeError('links actions require state');
  return Object.freeze({
    openManager: state.openManager,
    closeManager: state.closeManager,
    openCreate: state.openCreate,
    openEdit: state.openEdit,
    closeEditor: state.closeEditor,

    async createLink({ from, to, mode, bidir }) {
      await request(fetchImpl, `/api/groups/${encodeURIComponent(from)}/links`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ to, mode, bidir: !!bidir }),
      });
      state.closeEditor();
      notify(`linked: ${from} → ${to}${bidir ? ' (+reverse)' : ''}`);
      await refresh();
    },

    async updateLink({ id, from, to, mode }) {
      const scope = from || to;
      await request(fetchImpl, `/api/groups/${encodeURIComponent(scope)}/links/${encodeURIComponent(id)}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ mode }),
      });
      state.closeEditor();
      notify(`link #${id} mode → ${mode}`);
      await refresh();
    },

    async deleteLink({ id, from = '', to = '', scope = '' }) {
      const accepted = await confirm({
        title: words('Remove this link?', 'Sever this arcane channel?'),
        body: words(
          'Members of FROM lose the ability to message members of TO via this edge. Other groups / links are unaffected. This can\'t be undone — recreate to restore.',
          'Familiars of FROM lose the ability to whisper to familiars of TO via this channel. Other parties / channels are unaffected. This can\'t be undone — weave it anew to restore.',
        ),
        meta: `#${id} · ${from} → ${to}`,
        okLabel: words('Remove link', 'Sever channel'),
      });
      if (!accepted) return false;
      try {
        const group = scope || from || to;
        await request(fetchImpl, `/api/groups/${encodeURIComponent(group)}/links/${encodeURIComponent(id)}`, {
          method: 'DELETE',
        });
        notify(words(`link removed: #${id}`, `channel severed: #${id}`));
        await refresh();
        return true;
      } catch (error) {
        notify(`${words('Remove link', 'Sever channel')} failed: ${error?.message || String(error)}`, true);
        return false;
      }
    },
  });
}
