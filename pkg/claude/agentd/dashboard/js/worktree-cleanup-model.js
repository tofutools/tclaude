export const WORKTREE_CATEGORIES = Object.freeze([
  Object.freeze({ key: 'orphan', label: 'orphans', wizardLabel: 'orphans' }),
  Object.freeze({ key: 'retired', label: 'retired', wizardLabel: 'banished' }),
  Object.freeze({ key: 'agent', label: 'agent-bound', wizardLabel: 'familiar-bound' }),
  Object.freeze({ key: 'live', label: 'live', wizardLabel: 'channeling' }),
]);

function normalizeAgent(agent) {
  return Object.freeze({
    agent_id: String(agent?.agent_id || ''),
    conv_id: String(agent?.conv_id || ''),
    title: String(agent?.title || ''),
    online: agent?.online === true,
    retired: agent?.retired === true,
  });
}

export function normalizeWorktreeCandidate(worktree) {
  const isMain = worktree?.is_main === true;
  return Object.freeze({
    path: String(worktree?.path || ''),
    name: String(worktree?.name || ''),
    branch: String(worktree?.branch || ''),
    repo_root: String(worktree?.repo_root || ''),
    category: String(worktree?.category || ''),
    is_main: isMain,
    checked: !isMain && worktree?.checked === true,
    dirty: worktree?.dirty === true,
    reason: String(worktree?.reason || ''),
    agents: Object.freeze((worktree?.agents || []).map(normalizeAgent)),
  });
}

export function normalizeWorktreeCandidates(worktrees) {
  const seen = new Set();
  const result = [];
  for (const source of worktrees || []) {
    const candidate = normalizeWorktreeCandidate(source);
    if (!candidate.path || seen.has(candidate.path)) continue;
    seen.add(candidate.path);
    result.push(candidate);
  }
  return Object.freeze(result);
}

export function normalizePrunableRepo(source) {
  const reasons = Object.freeze((source?.reasons || []).map((entry) => Object.freeze({
    reason: String(entry?.reason || ''),
    count: Math.max(0, Number(entry?.count || 0)),
  })).filter((entry) => entry.reason && entry.count > 0));
  const count = Math.max(0, Number(source?.count || 0));
  return Object.freeze({
    repo_root: String(source?.repo_root || ''),
    count,
    reasons,
    checked: count > 0 && source?.checked === true,
  });
}

export function normalizePrunableRepos(repos) {
  const seen = new Set();
  const result = [];
  for (const source of repos || []) {
    const repo = normalizePrunableRepo(source);
    if (!repo.repo_root || repo.count === 0 || seen.has(repo.repo_root)) continue;
    seen.add(repo.repo_root);
    result.push(repo);
  }
  return Object.freeze(result);
}

export function reconcilePrunableRepos(repos, touchedChoices = new Map()) {
  const normalized = normalizePrunableRepos(repos);
  const presentRoots = new Set(normalized.map((repo) => repo.repo_root));
  for (const root of touchedChoices.keys()) {
    if (!presentRoots.has(root)) touchedChoices.delete(root);
  }
  return Object.freeze(normalized.map((repo) => touchedChoices.has(repo.repo_root)
    ? Object.freeze({ ...repo, checked: touchedChoices.get(repo.repo_root) === true })
    : repo));
}

export function prunableRepoMatches(repo, query) {
  const needle = String(query || '').trim().toLowerCase();
  if (!needle) return true;
  const reasons = (repo.reasons || []).map((entry) => entry.reason).join(' ');
  return `${repo.repo_root} ${reasons}`.toLowerCase().includes(needle);
}

export function selectedPrunableRepos(repos) {
  return (repos || []).filter((repo) => repo.checked);
}

export function prunableRecordCount(repos) {
  return (repos || []).reduce((total, repo) => total + Number(repo.count || 0), 0);
}

// Reconcile only choices that the human explicitly touched, keyed by the
// server's exact worktree path. A successful snapshot also proves which paths
// are absent, so forget choices for paths it no longer contains. If a new
// worktree later reuses that path, it must take the latest server default.
// Main worktrees always win the safety gate and stay off.
export function reconcileWorktreeCandidates(worktrees, touchedChoices = new Map()) {
  const candidates = normalizeWorktreeCandidates(worktrees);
  const presentPaths = new Set(candidates.map((candidate) => candidate.path));
  for (const path of touchedChoices.keys()) {
    if (!presentPaths.has(path)) touchedChoices.delete(path);
  }
  return Object.freeze(candidates.map((candidate) => {
    if (candidate.is_main || !touchedChoices.has(candidate.path)) return candidate;
    return Object.freeze({ ...candidate, checked: touchedChoices.get(candidate.path) === true });
  }));
}

export function worktreeMatches(candidate, query) {
  const needle = String(query || '').trim().toLowerCase();
  if (!needle) return true;
  const agents = (candidate.agents || []).map((agent) => `${agent.title} ${agent.conv_id}`).join(' ');
  return `${candidate.path} ${candidate.branch} ${agents}`.toLowerCase().includes(needle);
}

export function removableWorktrees(candidates) {
  return (candidates || []).filter((candidate) => !candidate.is_main);
}

export function selectedWorktrees(candidates) {
  return removableWorktrees(candidates).filter((candidate) => candidate.checked);
}

export function categoryWorktrees(candidates, category) {
  return removableWorktrees(candidates).filter((candidate) => candidate.category === category);
}

export function dirtyWorktrees(candidates) {
  return removableWorktrees(candidates).filter((candidate) => candidate.dirty);
}

export function visibleWorktrees(candidates, query) {
  const removable = removableWorktrees(candidates).filter((candidate) => worktreeMatches(candidate, query));
  const mains = (candidates || []).filter(
    (candidate) => candidate.is_main && worktreeMatches(candidate, query),
  );
  return [...removable, ...mains];
}

export function freezeWorktreeCleanupRequest(candidates, prunableRepos, deleteBranches) {
  return Object.freeze({
    paths: Object.freeze(selectedWorktrees(candidates).map((candidate) => candidate.path)),
    pruneRoots: Object.freeze(selectedPrunableRepos(prunableRepos).map((repo) => repo.repo_root)),
    deleteBranches: deleteBranches === true,
  });
}

export function worktreeCleanupSummary(response) {
  const removed = Number(response?.removed || 0);
  const branches = Number(response?.branches || 0);
  const skipped = Number(response?.skipped || 0);
  const failed = Number(response?.failed || 0);
  const pruned = Number(response?.pruned || 0);
  const pruneRemaining = Number(response?.prune_remaining || 0);
  const pruneFailed = Number(response?.prune_failed || 0);
  const pruneSkipped = Number(response?.prune_skipped || 0);
  let summary = `removed ${removed} worktree${removed === 1 ? '' : 's'}`;
  if (branches) summary += ` (+${branches} branch${branches === 1 ? '' : 'es'})`;
  if (skipped) summary += `, ${skipped} skipped`;
  if (failed) summary += `, ${failed} failed`;
  if (pruned || pruneRemaining || pruneFailed || pruneSkipped) {
    summary += `; pruned ${pruned} stale Git record${pruned === 1 ? '' : 's'}`;
    if (pruneRemaining) summary += `, ${pruneRemaining} ${pruneRemaining === 1 ? 'remains' : 'remain'}`;
    if (pruneFailed) summary += ` (${pruneFailed} repo${pruneFailed === 1 ? '' : 's'} failed/partial)`;
    if (pruneSkipped) summary += `, ${pruneSkipped} repo${pruneSkipped === 1 ? '' : 's'} skipped`;
  }
  return summary;
}
