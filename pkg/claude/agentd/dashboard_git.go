package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type dashboardGitRepo struct {
	Path          string   `json:"path"`
	Name          string   `json:"name"`
	Groups        []string `json:"groups"`
	Branch        string   `json:"branch"`
	DefaultBranch string   `json:"default_branch"`
	Remote        string   `json:"remote"`
	Dirty         bool     `json:"dirty"`
	Error         string   `json:"error,omitempty"`
}

type dashboardGitIssue struct {
	Group  string `json:"group"`
	Detail string `json:"detail"`
}

type dashboardGitRequest struct {
	Group         string `json:"group"`
	Path          string `json:"path"`
	Mode          string `json:"mode"`
	SwitchDefault bool   `json:"switch_default"`
	Discard       bool   `json:"discard"`
}

type dashboardGitResult struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Retain only a bounded diagnostic from potentially noisy Git subprocesses.
// Return the original size so truncation never blocks Git's pipe writer.
type dashboardGitOutput struct {
	data      []byte
	limit     int
	truncated bool
}

func (b *dashboardGitOutput) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.data)+n > b.limit {
		b.truncated = true
	}
	if left := b.limit - len(b.data); left > 0 {
		b.data = append(b.data, p[:min(left, n)]...)
	}
	return n, nil
}

func dashboardGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	cmd.WaitDelay = 2 * time.Second
	stdout := dashboardGitOutput{limit: 4 * 1024 * 1024}
	stderr := dashboardGitOutput{limit: 16 * 1024}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := string(stdout.data)
	if !strings.HasSuffix(out, "\x00") {
		out = strings.TrimSpace(out)
	}
	if stdout.truncated && err == nil {
		err = fmt.Errorf("git output exceeded 4 MiB")
	}
	if err != nil {
		detail := strings.TrimSpace(string(stderr.data))
		if ctx.Err() != nil {
			detail = ctx.Err().Error()
		}
		return out, fmt.Errorf("git %s: %s (%w)", args[0], detail, err)
	}
	return out, nil
}

func dashboardGitRoot(ctx context.Context, dir string) (string, error) {
	root, err := dashboardGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return canonicalDashboardGitPath(root)
}

func canonicalDashboardGitPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

// Request-scoped reads are shared across overlapping homes. sync.Once allows
// unrelated directories to be read concurrently without a global I/O lock.
type dashboardGitDirectory struct {
	once     sync.Once
	path     string
	info     os.FileInfo
	err      error
	git      bool
	readOnce sync.Once
	entries  []os.DirEntry
	readErr  error
}

type dashboardGitScan struct {
	paths       sync.Map
	directories sync.Map
}

func (s *dashboardGitScan) directory(path string) *dashboardGitDirectory {
	value, _ := s.paths.LoadOrStore(path, &dashboardGitDirectory{})
	resolved := value.(*dashboardGitDirectory)
	resolved.once.Do(func() { resolved.path, resolved.err = canonicalDashboardGitPath(path) })
	if resolved.err != nil {
		return resolved
	}
	value, _ = s.directories.LoadOrStore(resolved.path, &dashboardGitDirectory{path: resolved.path})
	dir := value.(*dashboardGitDirectory)
	dir.once.Do(func() {
		dir.info, dir.err = os.Stat(dir.path)
		_, err := os.Stat(filepath.Join(dir.path, ".git"))
		dir.git = err == nil
	})
	return dir
}

// Scan logical directory depths 0, 1 and 2. Directory symlinks participate,
// but a canonical directory is visited only at its shallowest depth, so
// cycles cannot expand the scope. Git administrative directories are excluded.
func (scan *dashboardGitScan) home(ctx context.Context, home, selectedPath string) ([]string, error) {
	type directory struct {
		path  string
		depth int
	}
	queue := []directory{{home, 0}}
	visited := map[string]bool{}
	roots := []string{}
	var scanErr error
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return roots, err
		}
		next := queue[0]
		queue = queue[1:]
		dir := scan.directory(next.path)
		if dir.err != nil {
			scanErr = dir.err
			continue
		}
		path := dir.path
		if !dir.info.IsDir() || visited[path] {
			continue
		}
		visited[path] = true
		// Homes may be inside a checkout; descendants need their own .git.
		candidate := next.depth == 0 || dir.git
		if selectedPath != "" {
			// During per-repository execution, inspect only the selected
			// checkout while retaining the identical directory traversal.
			rel, e := filepath.Rel(selectedPath, path)
			candidate = candidate && (path == selectedPath || (next.depth == 0 && e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
		}
		if candidate {
			roots = append(roots, path)
		}
		if next.depth == 2 {
			continue
		}
		dir.readOnce.Do(func() { dir.entries, dir.readErr = os.ReadDir(path) })
		if dir.readErr != nil {
			scanErr = dir.readErr
			continue
		}
		for _, entry := range dir.entries {
			if entry.Name() == ".git" {
				continue
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				queue = append(queue, directory{filepath.Join(path, entry.Name()), next.depth + 1})
			}
		}
	}
	return roots, scanErr
}

// Group homes anchor the depth-limited scan. Overlapping group directories
// and symlink aliases share a single canonical absolute checkout entry.
func dashboardGitHomes(ctx context.Context, group, selectedPath string) ([]dashboardGitRepo, []dashboardGitIssue, error) {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, nil, err
	}
	candidates := []dashboardGitRepo{}
	issues := []dashboardGitIssue{}
	seenDirs := map[string]int{}
	hasIssue := map[string]bool{}
	scoped := []string{}
	type homeResult struct {
		dirs []string
		err  error
	}
	scans := make([]homeResult, len(groups))
	scan := &dashboardGitScan{}
	var walks sync.WaitGroup
	walkJobs := make(chan int)
	for range min(8, len(groups)) {
		walks.Add(1)
		go func() {
			defer walks.Done()
			for i := range walkJobs {
				scans[i].dirs, scans[i].err = scan.home(ctx, groups[i].DefaultCwd, selectedPath)
			}
		}()
	}
	for i, g := range groups {
		if (group == "" || g.Name == group) && g.DefaultCwd != "" {
			walkJobs <- i
		}
	}
	close(walkJobs)
	walks.Wait()
	for groupIndex, g := range groups {
		if group != "" && g.Name != group {
			continue
		}
		scoped = append(scoped, g.Name)
		if g.DefaultCwd == "" {
			issues = append(issues, dashboardGitIssue{g.Name, "No group home directory configured"})
			hasIssue[g.Name] = true
			continue
		}
		dirs, scanErr := scans[groupIndex].dirs, scans[groupIndex].err
		if scanErr != nil {
			issues = append(issues, dashboardGitIssue{g.Name, "Some directories could not be scanned: " + scanErr.Error()})
			hasIssue[g.Name] = true
		}
		for _, dir := range dirs {
			if i, ok := seenDirs[dir]; ok {
				candidates[i].Groups = append(candidates[i].Groups, g.Name)
			} else {
				seenDirs[dir] = len(candidates)
				candidates = append(candidates, dashboardGitRepo{Path: dir, Groups: []string{g.Name}})
			}
		}
	}
	if group != "" && len(scoped) == 0 {
		return nil, nil, fmt.Errorf("group %q no longer exists", group)
	}
	// Resolve candidate checkouts in parallel even when a single container
	// home contains all the repos. Canonical directory aliases are inspected
	// once; final checkout roots are deduplicated after resolution as well.
	roots := make([]string, len(candidates))
	var wg sync.WaitGroup
	jobs := make(chan int)
	for range min(8, len(candidates)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				roots[i], _ = dashboardGitRoot(ctx, candidates[i].Path)
			}
		}()
	}
	for i := range candidates {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	repos := []dashboardGitRepo{}
	seenRoots := map[string]int{}
	groupHasRepo := map[string]bool{}
	for i, root := range roots {
		if root == "" {
			continue
		}
		index, ok := seenRoots[root]
		if !ok {
			index = len(repos)
			seenRoots[root] = index
			repos = append(repos, dashboardGitRepo{Path: root, Name: filepath.Base(root), Groups: []string{}})
		}
		for _, name := range candidates[i].Groups {
			groupHasRepo[name] = true
			already := false
			for _, existing := range repos[index].Groups {
				if existing == name {
					already = true
					break
				}
			}
			if !already {
				repos[index].Groups = append(repos[index].Groups, name)
			}
		}
	}
	for _, name := range scoped {
		if !groupHasRepo[name] && !hasIssue[name] {
			issues = append(issues, dashboardGitIssue{name, "No Git checkouts found in the home directory or two levels below it"})
		}
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Path < repos[j].Path })
	return repos, issues, nil
}

func dashboardGitRemote(ctx context.Context, path, branch string) (string, error) {
	// Honor the current branch's configured remote, including when the user
	// asks to pull that branch without switching. Otherwise prefer origin.
	if branch != "" {
		remote, _ := dashboardGit(ctx, path, "config", "--get", "branch."+branch+".remote")
		if remote != "" && remote != "." {
			return remote, nil
		}
	}
	remotes, err := dashboardGit(ctx, path, "remote")
	if err != nil {
		return "", err
	}
	names := strings.Fields(remotes)
	for _, name := range names {
		if name == "origin" {
			return name, nil
		}
	}
	if len(names) == 1 {
		return names[0], nil
	}
	return "", fmt.Errorf("no unambiguous remote configured")
}

func dashboardGitDefault(ctx context.Context, path, remote string) (string, error) {
	// A fetch does not refresh refs/remotes/<remote>/HEAD. Read the live
	// default so a server-side main -> trunk change cannot pick an old branch.
	out, err := dashboardGit(ctx, path, "ls-remote", "--symref", "--", remote, "HEAD")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD" && strings.HasPrefix(fields[1], "refs/heads/") {
			return strings.TrimPrefix(fields[1], "refs/heads/"), nil
		}
	}
	return "", fmt.Errorf("remote default branch could not be determined")
}

// Preview inspection is strictly local. Cached remote HEAD is only a hint;
// execution resolves the live default before switching branches.
func inspectDashboardGit(ctx context.Context, repo dashboardGitRepo) dashboardGitRepo {
	status, err := dashboardGit(ctx, repo.Path, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=normal")
	if err != nil {
		repo.Error = err.Error()
		return repo
	}
	for _, record := range strings.Split(status, "\x00") {
		if strings.HasPrefix(record, "# branch.head ") {
			repo.Branch = strings.TrimPrefix(record, "# branch.head ")
			if repo.Branch == "(detached)" {
				repo.Branch = ""
			}
		} else if record != "" && !strings.HasPrefix(record, "# ") {
			repo.Dirty = true
			break
		}
	}
	repo.Remote, err = dashboardGitRemote(ctx, repo.Path, repo.Branch)
	if err != nil {
		repo.Error = err.Error()
		return repo
	}
	head, _ := dashboardGit(ctx, repo.Path, "symbolic-ref", "--quiet", "refs/remotes/"+repo.Remote+"/HEAD")
	repo.DefaultBranch = strings.TrimPrefix(head, "refs/remotes/"+repo.Remote+"/")
	return repo
}

// Preview only the locally known default branch checkout. If no default hint
// exists, main/master are recognized without contacting the remote.
func dashboardGitPreviewCheckout(ctx context.Context, path string) bool {
	branch, err := dashboardGit(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return false
	}
	remote, err := dashboardGitRemote(ctx, path, branch)
	if err == nil {
		head, _ := dashboardGit(ctx, path, "symbolic-ref", "--quiet", "refs/remotes/"+remote+"/HEAD")
		if head != "" {
			return head == "refs/remotes/"+remote+"/"+branch
		}
	}
	return branch == "main" || branch == "master"
}

var dashboardGitActive = struct {
	sync.Mutex
	paths map[string]bool
}{paths: make(map[string]bool)}

func runDashboardGit(ctx context.Context, request dashboardGitRequest) dashboardGitResult {
	result := dashboardGitResult{Path: request.Path, Status: "skipped"}
	// Serialize linked checkouts too: their refs and config share a Git dir.
	common, err := dashboardGit(ctx, request.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		result.Detail = err.Error()
		return result
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		result.Detail = err.Error()
		return result
	}
	dashboardGitActive.Lock()
	if dashboardGitActive.paths[common] {
		dashboardGitActive.Unlock()
		result.Detail = "Another pull or sync is already running for this repository"
		return result
	}
	dashboardGitActive.paths[common] = true
	dashboardGitActive.Unlock()
	defer func() {
		dashboardGitActive.Lock()
		delete(dashboardGitActive.paths, common)
		dashboardGitActive.Unlock()
	}()
	repo := inspectDashboardGit(ctx, dashboardGitRepo{Path: request.Path})
	if repo.Error != "" {
		result.Detail = repo.Error
		return result
	}
	if repo.Dirty && !request.Discard {
		result.Detail = "Uncommitted changes kept; commit, stash or select discard before pulling"
		return result
	}
	target := repo.Branch
	remoteBranch := target
	if request.SwitchDefault {
		target, err = dashboardGitDefault(ctx, request.Path, repo.Remote)
		if err != nil {
			result.Detail = err.Error()
			return result
		}
		remoteBranch = target
	} else if target != "" {
		merge, _ := dashboardGit(ctx, request.Path, "config", "--get", "branch."+target+".merge")
		if strings.HasPrefix(merge, "refs/heads/") {
			remoteBranch = strings.TrimPrefix(merge, "refs/heads/")
		}
	}
	if target == "" {
		result.Detail = "Detached HEAD; select switch to default branch"
		return result
	}
	remoteRef := "refs/remotes/" + repo.Remote + "/" + remoteBranch
	if _, err = dashboardGit(ctx, request.Path, "check-ref-format", remoteRef); err != nil {
		result.Detail = "Invalid upstream branch"
		return result
	}
	// Refuse a branch already checked out elsewhere before discard can run.
	worktrees, err := dashboardGit(ctx, request.Path, "worktree", "list", "--porcelain")
	if err != nil {
		result.Detail = err.Error()
		return result
	}
	if target != repo.Branch && slices.Contains(strings.Split(worktrees, "\n"), "branch refs/heads/"+target) {
		result.Detail = "Default branch is checked out in another worktree"
		return result
	}
	// An unresolved merge/rebase must never be implicitly aborted by discard.
	for _, marker := range []string{"MERGE_HEAD", "rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD", "REVERT_HEAD", "sequencer"} {
		path, e := dashboardGit(ctx, request.Path, "rev-parse", "--git-path", marker)
		if e != nil {
			result.Detail = e.Error()
			return result
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(request.Path, path)
		}
		if _, e = os.Stat(path); e == nil {
			result.Detail = "Finish or abort the in-progress Git operation first"
			return result
		}
	}
	args := []string{"fetch", "--no-recurse-submodules"}
	if request.Mode == "sync" {
		args = append(args, "--prune")
	}
	args = append(args, "--", repo.Remote)
	if _, err = dashboardGit(ctx, request.Path, args...); err != nil {
		result.Status = "failed"
		result.Detail = err.Error()
		return result
	}
	if _, err = dashboardGit(ctx, request.Path, "rev-parse", "--verify", remoteRef+"^{commit}"); err != nil {
		result.Detail = "Upstream branch is unavailable after fetch"
		return result
	}
	localRef := "refs/heads/" + target
	_, localErr := dashboardGit(ctx, request.Path, "rev-parse", "--verify", localRef+"^{commit}")
	if localErr == nil {
		_, ahead := dashboardGit(ctx, request.Path, "merge-base", "--is-ancestor", remoteRef, localRef)
		_, behind := dashboardGit(ctx, request.Path, "merge-base", "--is-ancestor", localRef, remoteRef)
		if ahead != nil && behind != nil {
			result.Detail = "Branches diverged; fast-forward pull is not possible"
			return result
		}
	}
	changed := false
	run := func(args ...string) bool {
		if _, err := dashboardGit(ctx, request.Path, args...); err != nil {
			result.Status = "failed"
			result.Detail = err.Error()
			if changed {
				result.Detail = "Earlier steps completed; inspect the checkout. " + result.Detail
			}
			return false
		}
		changed = true
		return true
	}
	if request.Discard {
		// reset --hard can delete an ignored directory obstructing a tracked
		// file. Refuse such collisions before any requested discard, just as
		// switch/merge refuse ignored-file collisions below.
		if err := dashboardGitCheckDiscard(ctx, request.Path); err != nil {
			result.Detail = err.Error()
			return result
		}
		if !run("reset", "--hard", "HEAD") || !run("clean", "-fd") {
			return result
		}
	}
	if target != repo.Branch {
		if localErr == nil {
			if !run("switch", "--no-overwrite-ignore", "--no-guess", "--", target) {
				return result
			}
		} else {
			if !run("switch", "--no-overwrite-ignore", "--create", target, "--track", remoteRef) {
				return result
			}
		}
	}
	if !run("merge", "--no-overwrite-ignore", "--ff-only", "--no-edit", "--", remoteRef) {
		return result
	}
	// Keep the local preview hint aligned with the default verified for this update.
	if request.SwitchDefault && !run("symbolic-ref", "refs/remotes/"+repo.Remote+"/HEAD", remoteRef) {
		return result
	}
	result.Status = "updated"
	result.Detail = "Up to date on " + target
	return result
}

// Check both directions: an ignored directory may contain a tracked path,
// or may itself live underneath a path HEAD expects to be a regular file.
func dashboardGitCheckDiscard(ctx context.Context, path string) error {
	ignored, err := dashboardGit(ctx, path, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	if ignored == "" {
		return nil
	}
	tracked, err := dashboardGit(ctx, path, "ls-tree", "-r", "--name-only", "-z", "HEAD")
	if err != nil {
		return err
	}
	ignoredPaths := map[string]bool{}
	ignoredParents := map[string]string{}
	for _, local := range strings.Split(ignored, "\x00") {
		local = strings.TrimSuffix(local, "/")
		if local == "" {
			continue
		}
		ignoredPaths[local] = true
		for parent := local; parent != "."; parent = filepath.Dir(parent) {
			ignoredParents[parent] = local
		}
	}
	for _, file := range strings.Split(tracked, "\x00") {
		if file == "" {
			continue
		}
		if local, ok := ignoredParents[file]; ok {
			return fmt.Errorf("ignored path %q obstructs a tracked file; move it before discarding", local)
		}
		for parent := file; parent != "."; parent = filepath.Dir(parent) {
			if ignoredPaths[parent] {
				return fmt.Errorf("ignored path %q obstructs a tracked file; move it before discarding", parent)
			}
		}
	}
	return nil
}

func handleDashboardGit(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	group := r.URL.Query().Get("group")
	var request dashboardGitRequest
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if request.Mode != "pull" && request.Mode != "sync" {
			http.Error(w, "mode must be pull or sync", http.StatusBadRequest)
			return
		}
		group = request.Group
		if request.Path == "" {
			http.Error(w, "repository path required", http.StatusBadRequest)
			return
		}
		canonical, err := canonicalDashboardGitPath(request.Path)
		if err != nil {
			http.Error(w, "repository is unavailable; rescan", http.StatusConflict)
			return
		}
		request.Path = canonical
	}
	started := time.Now()
	repos, issues, err := dashboardGitHomes(ctx, group, request.Path)
	discovered := time.Now()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodGet {
		// Local inspection is bounded independently for each checkout.
		var wg sync.WaitGroup
		slots := make(chan struct{}, 8)
		for i := range repos {
			wg.Add(1)
			go func() {
				defer wg.Done()
				slots <- struct{}{}
				defer func() { <-slots }()
				child, stop := context.WithTimeout(r.Context(), 15*time.Second)
				defer stop()
				if dashboardGitPreviewCheckout(child, repos[i].Path) {
					repos[i] = inspectDashboardGit(child, repos[i])
				} else {
					repos[i].Path = ""
				}
			}()
		}
		wg.Wait()
		repos = slices.DeleteFunc(repos, func(repo dashboardGitRepo) bool { return repo.Path == "" })
		w.Header().Set("Server-Timing", fmt.Sprintf("discovery;dur=%.1f, local_git;dur=%.1f, total;dur=%.1f", float64(discovered.Sub(started).Microseconds())/1000, float64(time.Since(discovered).Microseconds())/1000, float64(time.Since(started).Microseconds())/1000))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"repos": repos, "issues": issues})
		return
	}
	// Re-resolve the scope at execution, refusing arbitrary paths or a home
	// directory which was removed/retargeted after the preview was opened.
	allowed := false
	for _, repo := range repos {
		if repo.Path == request.Path {
			allowed = true
			break
		}
	}
	if !allowed {
		http.Error(w, "repository is no longer within this group directory scan; rescan", http.StatusConflict)
		return
	}
	result := runDashboardGit(ctx, request)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
