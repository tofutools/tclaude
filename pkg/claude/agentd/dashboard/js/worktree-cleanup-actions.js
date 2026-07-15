import { worktreeCleanupSummary } from './worktree-cleanup-model.js';

async function responsePayload(response) {
  const text = await response.text();
  if (!text) return {};
  try { return JSON.parse(text); } catch (_) { return { error: text }; }
}

function responseError(response, payload, fallback) {
  const message = payload?.error || payload?.message || fallback || `HTTP ${response.status}`;
  const error = new Error(String(message));
  error.status = response.status;
  error.payload = payload;
  return error;
}

export function createWorktreeCleanupActions({
  fetchImpl = fetch,
  refresh = async () => {},
  notify = () => {},
} = {}) {
  return Object.freeze({
    async scan(group = '') {
      const normalizedGroup = String(group || '');
      const url = normalizedGroup
        ? `/api/groups/${encodeURIComponent(normalizedGroup)}/worktrees`
        : '/api/worktrees/cleanup';
      const response = await fetchImpl(url, { credentials: 'same-origin' });
      const payload = await responsePayload(response);
      if (!response.ok) throw responseError(response, payload, 'scan failed');
      return Object.freeze({
        repoRoots: Object.freeze([...(payload.repo_roots || [])].map(String)),
        worktrees: Object.freeze([...(payload.worktrees || [])]),
      });
    },

    async cleanup(request) {
      const response = await fetchImpl('/api/worktrees/cleanup', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          paths: request.paths,
          delete_branches: request.deleteBranches === true,
        }),
      });
      const payload = await responsePayload(response);
      if (!response.ok) throw responseError(response, payload, 'cleanup failed');
      const result = Object.freeze({
        ...payload,
        outcomes: Object.freeze([...(payload.outcomes || [])].map((outcome) => Object.freeze({
          path: String(outcome?.path || ''),
          branch: String(outcome?.branch || ''),
          result: String(outcome?.result || ''),
          detail: String(outcome?.detail || ''),
        }))),
      });
      try { notify(worktreeCleanupSummary(result), Number(result.failed || 0) > 0); } catch (_) { /* advisory */ }
      // The destructive response is authoritative. Never keep its result UI
      // or close paths behind an ordinary snapshot refresh that may be slow or
      // unavailable; reconcile the background dashboard independently.
      void Promise.resolve().then(refresh).catch(() => {});
      return result;
    },
  });
}
