package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// tui_worktree.go backs the terminal console's spawn-form worktree picker.
//
// The console has no picker of its own to browse a repo with, so it asks for
// one branch by name and the daemon resolves it the way the CLI's
// `--worktree <branch>` does: reuse the worktree already checked out on that
// branch, otherwise create one. The git operations themselves live in the
// worktree package, the single source of truth shared with `tclaude worktree`,
// the dashboard's /api/worktrees picker and the CLI.
//
// Why a route rather than a direct call: the standalone console
// (`tclaude agent tui-dashboard`) is on another machine, so the worktree has
// to be made on the daemon's host either way. Keeping it an HTTP shape lets
// both consoles run the identical code path, the way every other thing the
// console does already goes through the daemon's API.
//
// The route is mounted on the console surfaces only — buildTUIHTTPHandler for
// the standalone client and buildTUIConsoleMux for the in-process one — and
// never on buildMux, the Unix-socket mux agents reach. Creating directories
// outside any sandbox is a human move: agents get worktrees through the
// `tclaude worktree` CLI, exactly as they do for the dashboard's own
// /api/worktrees endpoint.

// tuiWorktreePath is the console-only worktree route, shared by the model, the
// in-process mux and the standalone client's mux so the three cannot drift.
const tuiWorktreePath = "/v1/worktrees"

// tuiWorktreeRequest asks for the worktree on Branch in the repo containing
// Repo. A blank Repo means the daemon's own working directory — the same
// fallback a spawn with a blank cwd lands in.
type tuiWorktreeRequest struct {
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch"`
}

// tuiWorktreeResponse is the resolved worktree. Created distinguishes a fresh
// worktree from one that already existed on that branch, which is what lets
// the console say which of the two it just did — and, when the spawn behind it
// fails, name a directory that was not there before.
type tuiWorktreeResponse struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Created bool   `json:"created"`
}

// buildTUIConsoleMux is the in-process console's handler: the daemon's own /v1
// mux plus the console-only routes above it. ServeMux prefers the more
// specific pattern, so the worktree route wins over the "/" catch-all that
// carries everything else into buildMux.
//
// The route carries its own copy of the middleware buildMux applies inside
// itself, since the catch-all is what would otherwise have supplied it. That
// keeps this console's worktree calls logged, audited and idempotent exactly
// as the standalone console's are (buildTUIHTTPHandler wraps the same chain).
func buildTUIConsoleMux() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST "+tuiWorktreePath,
		idempotencyRequests(logRequest(auditRequests(http.HandlerFunc(handleTUIWorktree)))))
	mux.Handle("/", buildMux())
	return mux
}

// handleTUIWorktree answers POST /v1/worktrees for a terminal console.
//
// Human-only, like the dashboard's picker: the resolution runs as the daemon
// process, outside any agent sandbox, and it creates a directory of the
// caller's choosing. An in-process console started from inside a harness pane
// classifies as that agent (see inProcessTUIAPI), and the agent driving that
// pane must not reach through it.
func handleTUIWorktree(w http.ResponseWriter, r *http.Request) {
	if classify(peerFromContext(r.Context())) != classHuman {
		writeError(w, http.StatusForbidden, "forbidden",
			"creating a worktree is an operator-console operation")
		return
	}
	var body tuiWorktreeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	branch := strings.TrimSpace(body.Branch)
	if err := validateTUIWorktreeBranch(branch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	path, created, status, err := resolveTUIWorktree(body.Repo, branch)
	if err != nil {
		writeError(w, status, "worktree", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tuiWorktreeResponse{Path: path, Branch: branch, Created: created})
}

// resolveTUIWorktree turns "the worktree on this branch" into a directory,
// mirroring the CLI's resolveSpawnWorktree: an existing worktree already
// checked out on the branch is reused, otherwise one is created — a checkout
// of an existing branch, or a new branch cut from the repo's default one.
//
// There is no base-branch choice here, unlike `--worktree-base` and the
// dashboard's picker: the console is the small surface, and cutting from
// somewhere other than the default branch is answered by making the worktree
// first (`tclaude worktree add <branch> --from-branch <base> --detached`) and
// then naming that branch in the form, which reuses it.
//
// repoDir is any path inside the target repo; blank falls back to the daemon's
// own working directory, which is where a spawn with no directory of its own
// would have landed.
//
// status is the HTTP code the failure deserves, so the handler does not have
// to guess: everything the caller can fix by editing a field is a 400, and a
// git command that broke on the daemon's side is a 500 — the same split the
// dashboard's own worktree endpoint makes.
func resolveTUIWorktree(repoDir, branch string) (path string, created bool, status int, err error) {
	dir, err := resolveSpawnCwd(repoDir)
	if err != nil {
		return "", false, http.StatusBadRequest, err
	}
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return "", false, http.StatusInternalServerError,
				fmt.Errorf("read the daemon's working directory: %w", err)
		}
	}
	root, err := worktree.RepoRootForPath(dir)
	if err != nil {
		return "", false, http.StatusBadRequest, fmt.Errorf("a new worktree needs a git repo: %w", err)
	}
	wts, err := worktree.ListWorktreesIn(root)
	if err != nil {
		return "", false, http.StatusInternalServerError, fmt.Errorf("list worktrees in %s: %w", root, err)
	}
	for _, wt := range wts {
		if wt.Branch == branch {
			return wt.Path, false, http.StatusOK, nil
		}
	}
	made, err := worktree.AddWorktreeIn(root, branch, "", "")
	if err != nil {
		return "", false, http.StatusBadRequest, fmt.Errorf("create worktree: %w", err)
	}
	return made, true, http.StatusOK, nil
}

// tuiMaxWorktreeBranchLen bounds a typed branch name. Long enough for the
// `team/TCL-123-some-description` shapes people actually use, short enough
// that the derived `../<repo>-<branch>` directory stays inside the filesystem's
// own name limits.
const tuiMaxWorktreeBranchLen = 100

// validateTUIWorktreeBranch gates the one console field that becomes git
// argv. The branch reaches `git worktree add -b <branch> …` positionally, so a
// leading "-" would be read as a flag; the rest of the rules are git's own
// refname shape, checked here so a typo comes back as a form error instead of
// a git failure the operator has to decode.
//
// The charset is deliberately a superset of the spawn-name charset
// (isValidSpawnName), because the branch defaults to the agent's name: every
// name that can be synced across is a branch name that passes here.
func validateTUIWorktreeBranch(branch string) error {
	if branch == "" {
		return fmt.Errorf("branch name is required")
	}
	if len(branch) > tuiMaxWorktreeBranchLen {
		return fmt.Errorf("branch name is too long (max %d characters)", tuiMaxWorktreeBranchLen)
	}
	for _, r := range branch {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '/':
		default:
			return fmt.Errorf("branch names may use only letters, digits, '-', '_', '.' and '/'")
		}
	}
	switch {
	case strings.HasPrefix(branch, "-"), strings.HasPrefix(branch, "/"),
		strings.HasPrefix(branch, "."):
		return fmt.Errorf("branch names may not start with '-', '/' or '.'")
	case strings.HasSuffix(branch, "/"), strings.HasSuffix(branch, "."):
		return fmt.Errorf("branch names may not end with '/' or '.'")
	case strings.Contains(branch, ".."), strings.Contains(branch, "//"):
		return fmt.Errorf("branch names may not contain '..' or '//'")
	}
	return nil
}
