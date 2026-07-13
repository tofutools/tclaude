const CLONE_TIMEOUT_MS = 35_000;

async function responseError(response) {
  return (await response.text()) || `HTTP ${response.status}`;
}

async function requestJSON(fetchImpl, url, options = {}) {
  const response = await fetchImpl(url, { credentials: 'same-origin', ...options });
  if (!response.ok) throw new Error(await responseError(response));
  try { return await response.json(); } catch (_) { return {}; }
}

export function normaliseFollowUp(value) {
  return String(value || '').replace(/[\r\n\t]+/g, ' ').replace(/\s+/g, ' ').trim();
}

export function descendantsOf(name, groups) {
  const children = new Map();
  for (const group of groups || []) {
    if (!group.parent) continue;
    const rows = children.get(group.parent) || [];
    rows.push(group.name);
    children.set(group.parent, rows);
  }
  const found = new Set();
  const pending = [name];
  while (pending.length) {
    const current = pending.pop();
    if (found.has(current)) continue;
    found.add(current);
    pending.push(...(children.get(current) || []));
  }
  return found;
}

export function createActionDialogActions({
  state,
  fetchImpl = fetch,
  refresh,
  notify,
  getSnapshot = () => null,
}) {
  return Object.freeze({
    openClone: state.openClone,
    openReincarnate: state.openReincarnate,
    openNest({ group }) {
      if (!group) { notify('no group', true); return; }
      state.openNest({ group });
    },
    close: state.close,
    nestModel(group) {
      const groups = getSnapshot()?.groups || [];
      const current = groups.find((item) => item.name === group);
      const blocked = descendantsOf(group, groups);
      return {
        currentParent: current?.parent || '',
        candidates: groups.map((item) => item.name)
          .filter((name) => !blocked.has(name))
          .sort((a, b) => a.localeCompare(b)),
      };
    },
    async loadWorktrees(repo, { signal } = {}) {
      if (!repo?.trim()) return { is_repo: false, empty: true };
      return requestJSON(fetchImpl, `/api/worktrees?repo=${encodeURIComponent(repo.trim())}`, { signal });
    },
    async createWorktree({ repo, branch, fromBranch }) {
      return requestJSON(fetchImpl, '/api/worktrees', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ repo, branch, from_branch: fromBranch || '' }),
      });
    },
    async cloneAgent({ conv, label, followUp, copyConversation, cwd }) {
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), CLONE_TIMEOUT_MS);
      try {
        const payload = await requestJSON(fetchImpl, `/api/agents/${encodeURIComponent(conv)}/clone`, {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            follow_up: normaliseFollowUp(followUp),
            no_copy_conv: !copyConversation,
            cwd: cwd || '',
          }),
          signal: controller.signal,
        });
        state.close();
        const target = payload.new_conv ? ` → ${String(payload.new_conv).slice(0, 8)}` : '';
        notify(payload.warning
          ? `cloned ${label}${target} (warning: ${payload.warning})`
          : `cloned ${label}${target}`, !!payload.warning);
        await refresh();
        return payload;
      } catch (error) {
        if (error?.name === 'AbortError') {
          throw new Error(`clone timed out after ${CLONE_TIMEOUT_MS / 1000}s — the new agent may still come online; check ~/.tclaude/output.log and refresh in a moment.`);
        }
        throw error;
      } finally { clearTimeout(timer); }
    },
    async reincarnateAgent({ conv, label, mode, focusHint, followUp }) {
      const body = mode === 'force'
        ? { mode: 'force', follow_up: normaliseFollowUp(followUp) }
        : { mode: 'self', ...(focusHint.trim() ? { focus_hint: focusHint.trim() } : {}) };
      if (mode === 'force' && !body.follow_up) throw new Error('follow-up is required for force reincarnate');
      const payload = await requestJSON(fetchImpl, `/api/agents/${encodeURIComponent(conv)}/reincarnate`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
      });
      state.close();
      if (mode === 'force') {
        const suffix = payload.new_title ? ` → ${payload.new_title}`
          : payload.new_conv ? ` → ${String(payload.new_conv).slice(0, 8)}` : '';
        notify(`reincarnated ${label}${suffix}`);
      } else notify(`asked ${label} to reincarnate itself`);
      await refresh();
      return payload;
    },
    async nestGroup({ group, parent }) {
      await requestJSON(fetchImpl, `/api/groups/${encodeURIComponent(group)}/parent`, {
        method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ parent }),
      });
      state.close();
      notify(parent ? `${group}: nested under ${parent}` : `${group}: moved to top level`);
      await refresh({ force: true });
    },
  });
}
