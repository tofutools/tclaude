package worktree

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// repo.go holds repo-path-aware twins of the CWD-implicit helpers in
// worktree.go. The CLI variants run `git` in the process's working
// directory, which is fine for an interactive `tclaude worktree …`
// invocation. The agentd daemon, by contrast, needs to inspect and
// mutate worktrees for an *arbitrary* repo (the one a spawn/clone is
// targeting), so every git call here is explicitly anchored with a
// directory rather than relying on os.Getwd().

// gitIn runs a git command anchored at dir. dir may be any path inside
// the repo — git walks up to the repo root itself. On failure the
// returned error carries git's stderr so callers can surface a useful
// message rather than a bare exit code.
func gitIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			msg := strings.TrimSpace(string(ee.Stderr))
			if msg != "" {
				return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
			}
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// asExitError is errors.As specialised to *exec.ExitError, kept local
// so gitIn doesn't need to pull errors into its import set just for
// one type assertion.
func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// RepoRootForPath returns the absolute repo root of the git repository
// containing path. Errors if path doesn't exist or isn't inside a git
// repo — both are conditions the caller wants to report distinctly
// from "no worktrees".
func RepoRootForPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("no path given")
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is not an accessible directory", path)
	}
	root, err := gitIn(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository", path)
	}
	return root, nil
}

// parseWorktreePorcelain turns `git worktree list --porcelain` output
// into WorktreeInfo records. Shared by the CWD-implicit ListWorktrees
// and the repo-anchored ListWorktreesIn.
func parseWorktreePorcelain(output string) []WorktreeInfo {
	var worktrees []WorktreeInfo
	var current WorktreeInfo

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if current.Path != "" {
				worktrees = append(worktrees, current)
				current = WorktreeInfo{}
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			current.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "HEAD "):
			current.Commit = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			current.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "bare":
			current.IsBare = true
		case line == "prunable" || strings.HasPrefix(line, "prunable "):
			current.Prunable = true
			current.PrunableReason = strings.TrimSpace(strings.TrimPrefix(line, "prunable"))
		case line == "locked" || strings.HasPrefix(line, "locked "):
			current.Locked = true
		}
	}
	if current.Path != "" {
		worktrees = append(worktrees, current)
	}
	if len(worktrees) > 0 {
		worktrees[0].IsMain = true
	}
	return worktrees
}

// MainRepoForPath returns the absolute path of the MAIN worktree of the
// repo containing dir — the primary checkout, not the linked worktree
// dir happens to be. Resolved from `git --git-common-dir` (the shared
// .git all worktrees point at): its parent is the main worktree. Used to
// anchor a post-removal `git worktree prune` at a checkout that survives
// the sweep. Returns "" on any git failure (best-effort tidy-up).
func MainRepoForPath(dir string) string {
	out, err := gitIn(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(out)
	if common == "" {
		return ""
	}
	// common is "<main>/.git" for a normal repo; its parent is the main
	// worktree. A bare repo has no working tree — filepath.Dir is still a
	// sane anchor for prune.
	if strings.HasSuffix(common, string(filepath.Separator)+".git") || strings.HasSuffix(common, "/.git") {
		return filepath.Dir(common)
	}
	return filepath.Dir(common)
}

// PrunableWorktree is one stale administrative entry reported by
// `git worktree prune --dry-run --verbose`. AdminDir is Git's name below
// .git/worktrees; Reason explains why Git considers it removable.
type PrunableWorktree struct {
	AdminDir     string
	Reason       string
	WorktreePath string // non-empty when Git can still associate a checkout path
}

// PrunableWorktreesIn reports stale worktree administrative entries that are
// absent from `worktree list`, without changing them. Prune's dry-run is the
// authoritative discovery surface; candidates that porcelain still exposes
// stay in the existing individually selectable worktree flow.
func PrunableWorktreesIn(dir string) ([]PrunableWorktree, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	stdout, stderr, err := runWorktreePrune(dir, true)
	if err != nil {
		return nil, pruneCommandError(true, stderr, err)
	}
	// Git writes verbose dry-run records to stderr on current versions even
	// when the command succeeds. Parse both streams so discovery does not
	// accidentally recreate the exact "invisible stale records" defect.
	entries := parsePrunableWorktrees(stdout + "\n" + stderr)
	if resolveErr := resolvePrunableWorktreePaths(dir, entries); resolveErr != nil {
		return nil, resolveErr
	}

	// A prune candidate with a readable gitdir can also appear in porcelain
	// as an individually selectable/protected worktree row. Keep those out of
	// the repo aggregate: PruneWorktreesIn temporarily locks them as a second
	// line of defence, while the existing checkout flow remains authoritative.
	listed, listErr := ListWorktreesIn(dir)
	if listErr != nil {
		return nil, fmt.Errorf("list worktrees while classifying prune preview: %w", listErr)
	}
	filtered := entries[:0]
	for _, entry := range entries {
		visible := false
		for _, wt := range listed {
			if wt.Prunable && entry.WorktreePath != "" && sameDir(wt.Path, entry.WorktreePath) {
				visible = true
				break
			}
		}
		if visible {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered, nil
}

// PruneWorktreesIn runs `git worktree prune` in the repo containing dir,
// clearing the administrative registrations of worktrees whose working
// directories have been deleted out-of-band (by hand, or by a tool that
// removed the dir without telling git). `git worktree remove` already
// cleans the link for worktrees it removes; this mops up the *dangling*
// links a directory-only delete leaves behind. It only ever touches
// entries whose dir is already missing — never a live worktree — so it is
// safe to run repo-wide after a sweep.
//
// Git may write per-entry failures to stderr and still exit 0. The stderr is
// therefore returned even on success so callers can show it as advisory
// detail; callers must verify the post-state with PrunableWorktreesIn rather
// than trusting either the exit status or an empty error.
func PruneWorktreesIn(dir string) (stderr string, err error) {
	if strings.TrimSpace(dir) == "" {
		return "", nil
	}
	listed, listErr := ListWorktreesIn(dir)
	if listErr != nil {
		return "", fmt.Errorf("protect listed worktrees before prune: %w", listErr)
	}
	locked := make([]string, 0)
	for _, wt := range listed {
		if !wt.Prunable || wt.Locked || wt.IsMain {
			continue
		}
		if _, lockErr := gitIn(dir, "worktree", "lock", "--reason",
			"tclaude cleanup protects individually managed worktree", wt.Path); lockErr != nil {
			unlockWorktreesIn(dir, locked)
			return "", fmt.Errorf("protect listed worktree %s before prune: %w", wt.Path, lockErr)
		}
		locked = append(locked, wt.Path)
	}

	_, stderr, err = runWorktreePrune(dir, false)
	unlockErr := unlockWorktreesIn(dir, locked)
	if err != nil {
		return stderr, pruneCommandError(false, stderr, err)
	}
	if unlockErr != nil {
		return stderr, unlockErr
	}
	return stderr, nil
}

func unlockWorktreesIn(dir string, paths []string) error {
	var first error
	for _, path := range paths {
		if _, err := gitIn(dir, "worktree", "unlock", path); err != nil && first == nil {
			first = fmt.Errorf("restore listed worktree lock %s after prune: %w", path, err)
		}
	}
	return first
}

func runWorktreePrune(dir string, dryRun bool) (stdout, stderr string, err error) {
	args := []string{"worktree", "prune"}
	if dryRun {
		args = append(args, "--dry-run", "--verbose")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return strings.TrimSpace(outBuf.String()), strings.TrimSpace(errBuf.String()), err
}

func pruneCommandError(dryRun bool, stderr string, err error) error {
	args := "worktree prune"
	if dryRun {
		args += " --dry-run --verbose"
	}
	if stderr != "" {
		return fmt.Errorf("git %s: %s", args, stderr)
	}
	return fmt.Errorf("git %s: %w", args, err)
}

func parsePrunableWorktrees(output string) []PrunableWorktree {
	var out []PrunableWorktree
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		const prefix = "Removing worktrees/"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		adminAndReason := strings.TrimPrefix(line, prefix)
		adminDir, reason, ok := strings.Cut(adminAndReason, ": ")
		adminDir, reason = strings.TrimSpace(adminDir), strings.TrimSpace(reason)
		if !ok || adminDir == "" || reason == "" {
			continue
		}
		out = append(out, PrunableWorktree{AdminDir: adminDir, Reason: reason})
	}
	return out
}

func resolvePrunableWorktreePaths(dir string, entries []PrunableWorktree) error {
	common, err := gitIn(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("resolve Git common dir for prune preview: %w", err)
	}
	if strings.TrimSpace(common) == "" {
		return fmt.Errorf("resolve Git common dir for prune preview: empty path")
	}
	for i := range entries {
		if entries[i].AdminDir == "" || entries[i].AdminDir == "." || entries[i].AdminDir == ".." ||
			filepath.Base(entries[i].AdminDir) != entries[i].AdminDir {
			return fmt.Errorf("unsafe worktree administrative name %q", entries[i].AdminDir)
		}
		gitdirPath := filepath.Join(strings.TrimSpace(common), "worktrees", entries[i].AdminDir, "gitdir")
		data, readErr := os.ReadFile(gitdirPath)
		if readErr != nil {
			continue
		}
		gitdir := filepath.Clean(strings.TrimSpace(string(data)))
		if filepath.Base(gitdir) == ".git" {
			entries[i].WorktreePath = filepath.Dir(gitdir)
		}
	}
	return nil
}

// ListWorktreesIn returns all worktrees of the repo containing dir.
func ListWorktreesIn(dir string) ([]WorktreeInfo, error) {
	out, err := gitIn(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees: %w", err)
	}
	return parseWorktreePorcelain(out), nil
}

// IsDirtyIn reports whether the working tree at dir has uncommitted
// changes — modified, staged, OR untracked files (`git status
// --porcelain` lists all three; untracked show as "??"). The worktree
// janitor uses this to badge a worktree whose removal would lose work,
// so it can leave it un-ticked by default.
//
// Best-effort: a git failure (dir gone, not a repo, flaky call) comes
// back false rather than erroring — dirtiness is advisory UI, and a
// discovery scan over many worktrees must not abort on one bad dir.
// The actual removal still uses --force, so a stale "clean" reading
// never blocks an explicit, human-confirmed delete.
func IsDirtyIn(dir string) bool {
	out, err := gitIn(dir, "status", "--porcelain")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// BranchesIn returns the deduplicated short branch names (local +
// remote, origin/ prefix stripped) of the repo containing dir. Used to
// populate the "base branch" picker when creating a worktree.
func BranchesIn(dir string) []string {
	out, err := gitIn(dir, "branch", "-a", "--format=%(refname:short)")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "->") {
			continue
		}
		branch := strings.TrimPrefix(line, "origin/")
		if !seen[branch] {
			seen[branch] = true
			branches = append(branches, branch)
		}
	}
	return branches
}

// DefaultBranchIn returns the repo's default branch (origin/HEAD if
// known, else the first of main/master that exists).
func DefaultBranchIn(dir string) (string, error) {
	if ref, err := gitIn(dir, "symbolic-ref", "refs/remotes/origin/HEAD"); err == nil {
		parts := strings.Split(ref, "/")
		if len(parts) > 0 && parts[len(parts)-1] != "" {
			return parts[len(parts)-1], nil
		}
	}
	for _, branch := range []string{"main", "master"} {
		if branchExistsIn(dir, branch) {
			return branch, nil
		}
	}
	return "", fmt.Errorf("could not determine default branch (tried main, master)")
}

// branchExistsIn reports whether branch resolves in the repo at dir.
func branchExistsIn(dir, branch string) bool {
	_, err := gitIn(dir, "rev-parse", "--verify", "--quiet", branch)
	return err == nil
}

// HasCommitsIn reports whether the repo containing dir has at least one
// commit. A freshly `git init`-ed repo has an unborn HEAD — a current
// branch ref (e.g. main) that points at no commit yet. Such a repo has
// no commit to base a worktree on, so callers branch on this to create
// an orphan-branch worktree instead of `git worktree add … <base>`.
func HasCommitsIn(dir string) bool {
	_, err := gitIn(dir, "rev-parse", "--verify", "--quiet", "HEAD")
	return err == nil
}

// AddWorktreeIn creates a git worktree for branch in the repo
// containing repoPath, and returns the absolute path of the new
// worktree. If branch already exists it is checked out; otherwise a
// new branch is created from fromBranch (defaults to the repo's
// default branch). path, when non-empty, overrides the default
// `../<repo>-<branch>` location.
//
// A repo with no commits yet (a freshly `git init`-ed repo with an
// unborn HEAD) has nothing to base a worktree on, so `git worktree add
// … <base>` fails. There the new worktree is cut as an orphan branch
// instead — fromBranch is irrelevant and ignored — so spawning into a
// brand-new repo still lands the agent in its own worktree. (Needs git
// ≥ 2.42 for `worktree add --orphan`.)
//
// This is the non-printing, repo-anchored core of RunAdd — RunAdd
// stays as the chatty CLI front door; the agentd worktree endpoint
// calls this.
func AddWorktreeIn(repoPath, branch, fromBranch, path string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", fmt.Errorf("branch name is required")
	}
	repoRoot, err := RepoRootForPath(repoPath)
	if err != nil {
		return "", err
	}

	branchExists := branchExistsIn(repoRoot, branch)
	// No commits ⇒ unborn HEAD ⇒ no branch can resolve, so branchExists
	// is necessarily false here; we cut an orphan branch below.
	hasCommits := HasCommitsIn(repoRoot)

	baseBranch := strings.TrimSpace(fromBranch)
	if !branchExists && hasCommits {
		if baseBranch == "" {
			baseBranch, err = DefaultBranchIn(repoRoot)
			if err != nil {
				return "", fmt.Errorf("could not determine base branch: %w (specify one explicitly)", err)
			}
		}
		if !branchExistsIn(repoRoot, baseBranch) {
			return "", fmt.Errorf("base branch %q does not exist", baseBranch)
		}
	}

	worktreePath := strings.TrimSpace(path)
	if worktreePath == "" {
		// Default: sibling of the repo root, ../<repo>-<branch>, with
		// slashes in the branch flattened to "--" so a feature branch
		// doesn't create nested directories.
		safeBranch := strings.ReplaceAll(branch, "/", "--")
		safeBranch = strings.ReplaceAll(safeBranch, "\\", "--")
		worktreePath = filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+"-"+safeBranch)
	}
	if !filepath.IsAbs(worktreePath) {
		worktreePath = filepath.Join(repoRoot, worktreePath)
	}
	if _, err := os.Stat(worktreePath); err == nil {
		return "", fmt.Errorf("worktree path already exists: %s", worktreePath)
	}

	var args []string
	switch {
	case branchExists:
		args = []string{"worktree", "add", worktreePath, branch}
	case !hasCommits:
		// Empty repo: no commit to base on. Cut an orphan branch so the
		// agent still gets its own worktree to bootstrap the repo in.
		args = []string{"worktree", "add", "--orphan", "-b", branch, worktreePath}
	default:
		args = []string{"worktree", "add", "-b", branch, worktreePath, baseBranch}
	}
	if _, err := gitIn(repoRoot, args...); err != nil {
		if !hasCommits {
			return "", fmt.Errorf("failed to create worktree in a repo with no commits "+
				"(needs git ≥ 2.42 for orphan worktrees, or make an initial commit first): %w", err)
		}
		return "", fmt.Errorf("failed to create worktree: %w", err)
	}
	return worktreePath, nil
}

// SubRepo is one nested git repository discovered under a directory
// that is not itself a git repo. The dashboard's spawn modal uses
// these to populate a quick-pick list when the launch directory is a
// "virtual monorepo" — a plain folder holding shared docs alongside
// several independent git repos.
type SubRepo struct {
	Path string `json:"path"` // absolute path to the repo root
	Rel  string `json:"rel"`  // path relative to the scanned directory
}

// FindSubRepos walks dir up to maxDepth directory levels deep and
// returns every nested git repository it finds, sorted by relative
// path. A directory counts as a repo when it contains a ".git" entry
// — a directory for a normal clone, a file for a linked worktree. The
// walk does not descend into a directory once it's identified as a
// repo, so a repo's own nested worktrees and submodules don't
// multiply the result. Hidden directories and a couple of notoriously
// heavy non-source trees are skipped. dir itself is never returned.
//
// This is the discovery half of the spawn modal's "worktree a sub-repo
// of a monorepo launch dir" flow — RepoRootForPath fails on the
// monorepo dir, and this offers the nested repos to pick from instead.
func FindSubRepos(dir string, maxDepth int) []SubRepo {
	if dir == "" || maxDepth < 1 {
		return nil
	}
	var out []SubRepo
	var walk func(cur string, depth int)
	walk = func(cur string, depth int) {
		entries, err := os.ReadDir(cur)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if skipScanDir(name) {
				continue
			}
			child := filepath.Join(cur, name)
			if isGitRepoRoot(child) {
				rel, relErr := filepath.Rel(dir, child)
				if relErr != nil {
					rel = child
				}
				out = append(out, SubRepo{Path: child, Rel: rel})
				continue // a repo is a leaf — don't descend into it
			}
			if depth < maxDepth {
				walk(child, depth+1)
			}
		}
	}
	walk(dir, 1)
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// isGitRepoRoot reports whether path has a ".git" entry directly
// inside it (a directory for a clone, a file for a linked worktree).
func isGitRepoRoot(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// skipScanDir reports whether FindSubRepos should ignore a directory
// by name — hidden dirs (".git" included) and dependency trees that
// are large to walk and never hold a repo worth worktreeing.
func skipScanDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "vendor":
		return true
	default:
		return false
	}
}
