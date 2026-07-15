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

export function freezeWorktreeCleanupRequest(candidates, deleteBranches) {
  return Object.freeze({
    paths: Object.freeze(selectedWorktrees(candidates).map((candidate) => candidate.path)),
    deleteBranches: deleteBranches === true,
  });
}

export function worktreeCleanupSummary(response) {
  const removed = Number(response?.removed || 0);
  const branches = Number(response?.branches || 0);
  const skipped = Number(response?.skipped || 0);
  const failed = Number(response?.failed || 0);
  let summary = `removed ${removed} worktree${removed === 1 ? '' : 's'}`;
  if (branches) summary += ` (+${branches} branch${branches === 1 ? '' : 'es'})`;
  if (skipped) summary += `, ${skipped} skipped`;
  if (failed) summary += `, ${failed} failed`;
  return summary;
}
