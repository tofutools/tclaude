async function responseError(response) {
  return (await response.text()) || `HTTP ${response.status}`;
}

async function request(fetchImpl, url, options = {}) {
  const response = await fetchImpl(url, { credentials: 'same-origin', ...options });
  if (!response.ok) throw new Error(await responseError(response));
  try { return await response.json(); } catch (_) { return {}; }
}

// crossNodeLinkNotice says what a cross-node create actually did: an
// auto-approved edge is only connecting until the peer acknowledges it, and
// anything else waits on the peer operator's decision.
export function crossNodeLinkNotice(from, peer, edges) {
  const target = `${peer.label}:${peer.groupName}`;
  const out = (edges || []).find((edge) => edge.direction === 'out') || {};
  const reverse = (edges || []).some((edge) => edge.direction === 'in');
  const tail = reverse ? ' (reverse asked separately)' : '';
  if (out.state === 'pending') {
    return `link request sent to ${peer.label}: ${from} → ${target} · pending${out.deadline ? ` until ${new Date(out.deadline).toLocaleString()}` : ''}${tail}`;
  }
  if (out.state === 'active') return `linked: ${from} → ${target}${tail}`;
  return `linking ${from} → ${target}: connecting, waiting for ${peer.label} to acknowledge${tail}`;
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

    // loadLinkTargets lists the peers and peer-shared groups a cross-node
    // link may target. null means this daemon has no cross-node links (an
    // older daemon answers 404), so the dialog stays local-only.
    async loadLinkTargets() {
      const response = await fetchImpl('/api/federation/link-targets', { credentials: 'same-origin' });
      if (response.status === 404) return null;
      if (!response.ok) throw new Error(await responseError(response));
      return response.json();
    },

    async createLink({ from, to, mode, bidir, peer }) {
      const body = peer
        ? { to_peer: peer.id, to_group_id: peer.groupID, mode, bidir: !!bidir }
        : { to, mode, bidir: !!bidir };
      const out = await request(fetchImpl, `/api/groups/${encodeURIComponent(from)}/links`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      state.closeEditor();
      if (peer) notify(crossNodeLinkNotice(from, peer, out?.edges));
      else notify(`linked: ${from} → ${to}${bidir ? ' (+reverse)' : ''}`);
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

    async deleteLink({ id, from = '', to = '', scope = '', peerLabel = '', pending = false }) {
      const accepted = await confirm(peerLabel ? {
        title: pending ? 'Cancel this link request?' : 'Remove this cross-node link?',
        body: pending
          ? `The request to ${peerLabel} is withdrawn. Nothing was linked yet.`
          : `This node stops delivering over this edge immediately, including messages already queued. ${peerLabel} is told when reachable. Other links are unaffected.`,
        meta: `#${id} · ${from} → ${to}`,
        okLabel: pending ? 'Cancel request' : 'Remove link',
      } : {
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
