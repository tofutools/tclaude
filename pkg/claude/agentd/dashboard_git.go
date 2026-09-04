package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
		err = fmt.Errorf("Git output exceeded 4 MiB")
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
	return filepath.EvalSymlinks(root)
}

// Only group home directories are anchors: never silently include agents'
// working directories or all linked worktrees of a discovered repository.
func dashboardGitHomes(ctx context.Context, group, selectedPath string) ([]dashboardGitRepo, []dashboardGitIssue, error) {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, nil, err
	}
	repos := []dashboardGitRepo{}
	issues := []dashboardGitIssue{}
	seen := map[string]int{}
	found := group == ""
	for _, g := range groups {
		if group != "" && g.Name != group {
			continue
		}
		found = true
		if g.DefaultCwd == "" {
			issues = append(issues, dashboardGitIssue{g.Name, "No group home directory configured"})
			continue
		}
		// A selected root must contain its configured home directory. Avoid
		// spawning Git in every other repository for each item in a batch.
		if selectedPath != "" {
			home, e := filepath.EvalSymlinks(g.DefaultCwd)
			if e != nil {
				continue
			}
			rel, e := filepath.Rel(selectedPath, home)
			if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
		}
		root, err := dashboardGitRoot(ctx, g.DefaultCwd)
		if err != nil {
			issues = append(issues, dashboardGitIssue{g.Name, "Home directory is unavailable or is not a Git checkout"})
			continue
		}
		if i, ok := seen[root]; ok {
			repos[i].Groups = append(repos[i].Groups, g.Name)
		} else {
			seen[root] = len(repos)
			repos = append(repos, dashboardGitRepo{Path: root, Name: filepath.Base(root), Groups: []string{g.Name}})
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("group %q no longer exists", group)
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

func inspectDashboardGit(ctx context.Context, repo dashboardGitRepo) dashboardGitRepo {
	repo.Branch, _ = dashboardGit(ctx, repo.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
	status, err := dashboardGit(ctx, repo.Path, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		repo.Error = err.Error()
		return repo
	}
	repo.Dirty = status != ""
	repo.Remote, err = dashboardGitRemote(ctx, repo.Path, repo.Branch)
	if err == nil {
		repo.DefaultBranch, err = dashboardGitDefault(ctx, repo.Path, repo.Remote)
	}
	if err != nil {
		repo.Error = err.Error()
	}
	return repo
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
		target = repo.DefaultBranch
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
	if target != repo.Branch && strings.Contains(worktrees, "\nbranch refs/heads/"+target+"\n") {
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
	}
	repos, issues, err := dashboardGitHomes(ctx, group, request.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodGet {
		// Remote HEAD lookups must not serialize a hundred-repository preview.
		var wg sync.WaitGroup
		slots := make(chan struct{}, 8)
		for i := range repos {
			wg.Add(1)
			go func() {
				defer wg.Done()
				slots <- struct{}{}
				defer func() { <-slots }()
				child, stop := context.WithTimeout(ctx, 15*time.Second)
				defer stop()
				repos[i] = inspectDashboardGit(child, repos[i])
			}()
		}
		wg.Wait()
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
		http.Error(w, "repository is no longer a home directory in this scope; rescan", http.StatusConflict)
		return
	}
	result := runDashboardGit(ctx, request)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
