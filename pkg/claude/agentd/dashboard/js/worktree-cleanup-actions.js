import { worktreeCleanupSummary } from './worktree-cleanup-model.js';
import { announceNodeChanged, nodeFetch } from './node-api.js';

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
  // node is another node's group: its worktrees are that node's.
  const on = (node) => nodeFetch(node || '', { fetchImpl });
  return Object.freeze({
    async scan(group = '', node = '') {
      const normalizedGroup = String(group || '');
      const url = normalizedGroup
        ? `/api/groups/${encodeURIComponent(normalizedGroup)}/worktrees`
        : '/api/worktrees/cleanup';
      const response = await on(node)(url, { credentials: 'same-origin' });
      const payload = await responsePayload(response);
      if (!response.ok) throw responseError(response, payload, 'scan failed');
      return Object.freeze({
        repoRoots: Object.freeze([...(payload.repo_roots || [])].map(String)),
        worktrees: Object.freeze([...(payload.worktrees || [])]),
        prunableRepos: Object.freeze([...(payload.prunable_repos || [])]),
        pruneScanErrors: Object.freeze([...(payload.prune_errors || [])].map((entry) => Object.freeze({
          repo_root: String(entry?.repo_root || ''),
          detail: String(entry?.detail || ''),
        }))),
      });
    },

    async cleanup(request, node = '') {
      const response = await on(node)('/api/worktrees/cleanup', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          paths: request.paths,
          prune_roots: request.pruneRoots || [],
          delete_branches: request.deleteBranches === true,
          force_branches: request.forceBranches || [],
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
        prune_outcomes: Object.freeze([...(payload.prune_outcomes || [])].map((outcome) => Object.freeze({
          repo_root: String(outcome?.repo_root || ''),
          before: Number(outcome?.before || 0),
          cleared: Number(outcome?.cleared || 0),
          remaining: Number(outcome?.remaining || 0),
          result: String(outcome?.result || ''),
          detail: String(outcome?.detail || ''),
        }))),
      });
      try {
        notify(worktreeCleanupSummary(result),
          Number(result.failed || 0) > 0 || Number(result.prune_failed || 0) > 0);
      } catch (_) { /* advisory */ }
      // The destructive response is authoritative. Never keep its result UI
      // or close paths behind an ordinary snapshot refresh that may be slow or
      // unavailable; reconcile the background dashboard independently.
      if (node) announceNodeChanged(node);
      void Promise.resolve().then(refresh).catch(() => {});
      return result;
    },
  });
}
