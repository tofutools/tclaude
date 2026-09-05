const CONCURRENCY_KEY = 'tclaude.git.concurrency';

export function gitConcurrency(value) {
  const number = Number(value);
  return Number.isInteger(number) && number >= 1 && number <= 100 ? number : 4;
}

export function readGitConcurrency() {
  try { return gitConcurrency(localStorage.getItem(CONCURRENCY_KEY)); }
  catch { return 4; }
}

export function rememberGitConcurrency(value) {
  try { localStorage.setItem(CONCURRENCY_KEY, String(gitConcurrency(value))); }
  catch { /* Browser storage can be unavailable; the current choice still works. */ }
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
      let next = 0;
      // A bounded worker pool avoids overwhelming Git credential helpers and
      // delivers progress as each checkout finishes, including partial errors.
      await Promise.all(Array.from({ length: Math.min(gitConcurrency(concurrency), requests.length) }, async () => {
        while (next < requests.length) {
          const request = requests[next++];
          onResult({ path: request.path, status: 'running', detail: '' });
          let result;
          try {
            result = await json('/api/git-repositories', {
              method: 'POST', headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify(request),
            });
          } catch (error) {
            // Do not automatically retry a mutation after an uncertain response.
            result = { path: request.path, status: 'failed', detail: `${error.message}. Check the checkout before retrying.` };
          }
          onResult(result);
        }
      }));
    },
  };
}
