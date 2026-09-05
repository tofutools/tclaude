import { readJSONLines } from './stream-json.js';
import { dashPrefs } from './prefs.js';

const CONCURRENCY_KEY = 'tclaude.dash.git.concurrency';

export function gitConcurrency(value) {
  const number = Number(value);
  return Number.isInteger(number) && number >= 1 && number <= 100 ? number : 4;
}

export function readGitConcurrency() {
  return gitConcurrency(dashPrefs.getItem(CONCURRENCY_KEY));
}

export function rememberGitConcurrency(value) {
  dashPrefs.setItem(CONCURRENCY_KEY, String(gitConcurrency(value)));
}

export function matchingGitRepos(repos, query) {
  const text = query.trim().toLowerCase();
  return repos.filter((repo) => [repo.name, repo.path, ...(repo.groups || [])]
    .some((value) => String(value).toLowerCase().includes(text)));
}

export function gitRepoRequests(repos, selected, { group, mode, switchDefault, discard }) {
  return repos.filter((repo) => selected.has(repo.path)).map((repo) => Object.freeze({
    group, mode, path: repo.path, switch_default: switchDefault, discard,
  }));
}

export function createGitRepositoriesActions({ fetchImpl = fetch } = {}) {
  async function json(url, options = {}) {
    const response = await fetchImpl(url, { credentials: 'same-origin', ...options });
    if (!response.ok) throw new Error(await response.text() || `HTTP ${response.status}`);
    return response.json();
  }
  return {
    scan: (group, signal) => json(`/api/git-repositories?group=${encodeURIComponent(group)}`, { signal }),
    async run(requests, onResult, concurrency = 4) {
      if (!requests.length) return;
      const pending = new Set(requests.map((request) => request.path));
      try {
        const response = await fetchImpl('/api/git-repositories/batch', {
          credentials: 'same-origin', method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ requests, concurrency: gitConcurrency(concurrency) }),
        });
        if (!response.ok) throw new Error(await response.text() || `HTTP ${response.status}`);
        let complete = false;
        for await (const result of readJSONLines(response)) {
          if (result.status === 'complete') { complete = true; continue; }
          if (complete || !pending.has(result.path) || !['running', 'updated', 'skipped', 'failed'].includes(result.status)) {
            throw new Error('Unexpected batch progress');
          }
          if (result.status !== 'running') pending.delete(result.path);
          onResult(result);
        }
        if (!complete || pending.size) throw new Error('Batch progress stream ended early');
      } catch (error) {
        // Never retry mutations after a lost response; preserve completed rows.
        for (const path of pending) {
          onResult({ path, status: 'failed', detail: `${error.message}. Check the checkout before retrying.` });
        }
        throw error;
      }
    },
  };
}
