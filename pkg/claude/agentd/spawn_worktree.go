package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// spawn_worktree.go creates the worktree behind `tclaude agent spawn
// --worktree <branch>` (and the task-force deploy that shares the flag).
//
// Why the daemon and not the caller: the checkout is privileged
// host-level setup performed on the NEW agent's behalf, exactly like the
// session launch that follows it. Run as a subprocess of a sandboxed
// caller instead, it is subject to that caller's filesystem
// restrictions, so a repo tracking even one file under a write-denied
// path (a .claude/commands/ entry, say) fails the checkout half-way and
// takes the whole spawn down with it — for a directory the caller never
// reads or writes. Here the caller's own restrictions are simply not
// part of the operation.
//
// What replaces them is the dir write-proof (spawn_dir_proof.go): an
// AGENT caller must prove it can itself write in the repo and in the
// directory the worktree will be created in before the daemon creates
// anything there. That keeps the guarantee the sandbox was providing —
// no agent gets a checkout, or a child agent rooted in one, somewhere it
// could not have put one itself — while dropping the incidental
// dependency on which nested paths its sandbox happens to deny. The
// human operator is the trust root here as everywhere else and passes
// straight through.
//
// This is the agent-reachable sibling of two existing surfaces that
// already resolve worktrees daemon-side: the dashboard's /api/worktrees
// picker and the terminal console's human-only /v1/worktrees
// (tui_worktree.go). The git operations themselves live in the worktree
// package, the single source of truth shared with `tclaude worktree`.

const (
	// worktreePreparePath resolves "the worktree on this branch" into a
	// directory, creating it when it does not exist yet.
	worktreePreparePath = "/v1/worktrees/prepare"

	// worktreeDiscardPath takes back a worktree prepare created, for a
	// caller whose spawn then failed.
	worktreeDiscardPath = "/v1/worktrees/discard"

	// preparedWorktreeTTL bounds how long a discard token stays usable.
	// Long enough to outlive the spawn it was prepared for (including a
	// conv-id poll that runs to its timeout), short enough that a caller
	// that never comes back is forgotten.
	preparedWorktreeTTL = 30 * time.Minute

	// maxPreparedWorktreesPerCaller caps the registry PER CALLER, so a
	// caller looping on prepare cannot grow it without bound and — the
	// reason it is per caller rather than global — cannot evict another
	// agent's discard token and strand that agent's cleanup. Minting past
	// the cap evicts the caller's own oldest entry. An evicted worktree is
	// not removed; it is only no longer addressable by token, exactly like
	// an expired one.
	maxPreparedWorktreesPerCaller = 16
)

// preparedWorktree records a worktree this daemon created for a spawn,
// so the caller can hand it back if the spawn fails. branchCreated
// distinguishes a branch we cut for it — which the discard deletes, so a
// retry cuts a fresh one from the requested base — from one that already
// existed and must outlive the teardown.
//
// The registry is in memory: a daemon restart between prepare and a
// failed spawn forgets the token, and the worktree survives as an
// orphan the operator prunes like any other. Persisting it would trade
// that rare case for a durable record of paths to delete, which is the
// worse thing to get wrong.
type preparedWorktree struct {
	caller        string // conv-id, or "" for the human
	path          string
	branch        string
	branchCreated bool
	expires       time.Time
}

var (
	preparedWorktreesMu sync.Mutex
	preparedWorktrees   = map[string]preparedWorktree{}

	// prepareWorktreeMu serializes the create half of prepare — see the
	// call site for why the branch snapshot and the checkout must not
	// interleave with another prepare's.
	prepareWorktreeMu sync.Mutex
)

// registerPreparedWorktree stores wt under a fresh single-use token and
// returns it. "" on the crypto/rand failure path — the worktree is still
// created and returned, it just cannot be discarded by token.
func registerPreparedWorktree(wt preparedWorktree) string {
	token := newDirWriteProofToken()
	if token == "" {
		return ""
	}
	wt.expires = time.Now().Add(preparedWorktreeTTL)

	preparedWorktreesMu.Lock()
	defer preparedWorktreesMu.Unlock()
	purgeExpiredPreparedWorktreesLocked()
	oldest, mine := "", 0
	for tok, entry := range preparedWorktrees {
		if entry.caller != wt.caller {
			continue
		}
		mine++
		if oldest == "" || entry.expires.Before(preparedWorktrees[oldest].expires) {
			oldest = tok
		}
	}
	if mine >= maxPreparedWorktreesPerCaller {
		delete(preparedWorktrees, oldest)
	}
	preparedWorktrees[token] = wt
	return token
}

// restorePreparedWorktree puts a consumed entry back under its own
// token. Used when the teardown it was consumed for failed: the
// worktree is still there, so the caller (or the operator, retrying)
// must still be able to address it.
func restorePreparedWorktree(token string, wt preparedWorktree) {
	preparedWorktreesMu.Lock()
	defer preparedWorktreesMu.Unlock()
	preparedWorktrees[token] = wt
}

// takePreparedWorktree consumes the entry token addresses, if it exists,
// has not expired, and belongs to caller. Single-use: a token is spent
// whether or not the teardown that follows succeeds, so a retry loop
// cannot re-enter the removal path with the same token.
func takePreparedWorktree(token, caller string) (preparedWorktree, bool) {
	preparedWorktreesMu.Lock()
	defer preparedWorktreesMu.Unlock()
	purgeExpiredPreparedWorktreesLocked()
	entry, ok := preparedWorktrees[token]
	if !ok || entry.caller != caller {
		return preparedWorktree{}, false
	}
	delete(preparedWorktrees, token)
	return entry, true
}

func purgeExpiredPreparedWorktreesLocked() {
	now := time.Now()
	for tok, entry := range preparedWorktrees {
		if now.After(entry.expires) {
			delete(preparedWorktrees, tok)
		}
	}
}

// handleWorktreePrepare answers POST /v1/worktrees/prepare.
//
// Gated on groups.spawn — the worktree is the launch directory half of a
// spawn the caller is about to make — with the owner bypass widened to
// "owns any group": a lead that spawns its own team without an explicit
// grant must be able to prepare the directory it spawns into. The
// group-scoped spawn gate still binds the spawn itself; this endpoint
// creates a directory, not an agent.
func handleWorktreePrepare(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermissionEx(w, r, PermGroupsSpawn, ownsAnyGroupPermitting)
	if !ok {
		return
	}
	var body agent.WorktreePrepareRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	branch := strings.TrimSpace(body.Branch)
	if err := validateTUIWorktreeBranch(branch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	// The base reaches `git worktree add … <base>` positionally too, so
	// it passes the same refname gate as the branch itself.
	base := strings.TrimSpace(body.Base)
	if base != "" {
		if err := validateTUIWorktreeBranch(base); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "base branch: "+err.Error())
			return
		}
	}
	root, status, err := spawnWorktreeRepoRoot(body.Repo)
	if err != nil {
		writeError(w, status, "invalid_worktree", err.Error())
		return
	}

	// Reuse an existing worktree already checked out on this branch —
	// the equivalent of picking it from the dashboard's list. No proof
	// is needed to be told a path that already exists; the spawn that
	// follows proves the launch dir itself.
	//
	// Note what the lookup above and this scan do concede: they answer
	// "does this path exist, is it inside a repo, what is checked out
	// there" for a path the caller named, and they answer it as the
	// daemon rather than inside the caller's sandbox. That is a read
	// capability an agent did not have while the resolution ran
	// in-process. It is accepted deliberately — the answer is a path the
	// caller must still pass the spawn's own write-proof to use — but it
	// is a capability, not nothing, so keep the failure messages to what
	// a caller needs to fix its own request.
	wts, err := worktree.ListWorktreesIn(root)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "worktree",
			fmt.Sprintf("list worktrees in %s: %v", root, err))
		return
	}
	for _, wt := range wts {
		if wt.Branch == branch {
			writeJSON(w, http.StatusOK, agent.WorktreePrepareResponse{Path: wt.Path, Branch: branch})
			return
		}
	}

	// None yet, so this request creates one. An agent caller proves it
	// could have created it itself: the repo (which `git worktree add`
	// writes registration data into) and the parent directory the
	// default `../<repo>-<branch>` path lands in.
	var proofToken string
	var proofDirs []string
	if caller != "" {
		dirs := appendUniqueDirs([]string{root}, filepath.Dir(root))
		resolved, proofOK := requireDirWriteProof(w, r, caller, body.WriteProofToken, dirs)
		if !proofOK {
			return
		}
		if resolved != nil {
			proofToken = strings.TrimSpace(body.WriteProofToken)
			if v := resolved[root]; v != "" {
				root = v
			}
			for _, raw := range dirs {
				proofDirs = appendUniqueDirs(proofDirs, resolved[raw])
			}
			// The proof was taken over the parent of the RAW root; the
			// checkout lands in the parent of the RESOLVED one, and the two
			// differ if the root's own last component is a symlink. Assert
			// they are the same directory rather than assuming it — the same
			// assertion the per-agent-worktree path makes before it writes
			// into a directory it just created.
			if parent := filepath.Dir(root); !dirListContains(proofDirs, parent) {
				writeError(w, http.StatusForbidden, "write_proof_failed", fmt.Sprintf(
					"the worktree would be created in %s, which is not the directory the "+
						"write-proof was verified in; retry to be challenged for it", parent))
				return
			}
		}
	}
	defer cleanupDirWriteProofMarkers(proofToken, proofDirs)

	// Serialize creation so two concurrent prepares for the same new
	// branch cannot interleave the branch-existed snapshot below with
	// each other's `git worktree add`: the loser would otherwise see a
	// branch that appeared since it looked and roll back the WINNER's
	// branch. Worktree creation is a rare, slow, human-scale operation,
	// so one lock for all repos is cheap enough to be worth its
	// simplicity.
	prepareWorktreeMu.Lock()
	defer prepareWorktreeMu.Unlock()

	branchExisted := worktree.BranchExistsIn(root, branch)
	path, addErr := worktree.AddWorktreeIn(root, branch, base, "")
	if addErr != nil {
		// `git worktree add -b` creates the branch ref before it
		// populates the checkout, so a checkout that fails half-way
		// leaves the branch behind while the directory rolls back. Left
		// there, the next attempt takes the reuse-an-existing-branch
		// path and silently ignores the base it was asked to cut from —
		// handing the agent a tree without the code its brief assumes.
		// Roll the branch back with the checkout.
		if !branchExisted && worktree.BranchExistsIn(root, branch) {
			if _, delErr := worktree.DeleteBranchIn(root, branch); delErr != nil {
				slog.Warn("worktree prepare: could not roll back the branch a failed checkout left behind",
					"repo", root, "branch", branch, "error", delErr)
			}
		}
		writeError(w, http.StatusBadRequest, "worktree", addErr.Error())
		return
	}
	token := registerPreparedWorktree(preparedWorktree{
		caller: caller, path: path, branch: branch, branchCreated: !branchExisted,
	})
	writeJSON(w, http.StatusOK, agent.WorktreePrepareResponse{
		Path: path, Branch: branch, Created: true, DiscardToken: token,
	})
}

// handleWorktreeDiscard answers POST /v1/worktrees/discard: the spawn a
// worktree was prepared for failed, so take the worktree back down.
//
// Only a worktree THIS daemon prepared, addressed by the token it was
// prepared under and by the caller that prepared it, can be discarded —
// the endpoint removes a directory, so it deliberately cannot be pointed
// at an arbitrary path.
func handleWorktreeDiscard(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermissionEx(w, r, PermGroupsSpawn, ownsAnyGroupPermitting)
	if !ok {
		return
	}
	var body agent.WorktreeDiscardRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	token := strings.TrimSpace(body.Token)
	if token == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a discard token is required")
		return
	}
	entry, found := takePreparedWorktree(token, caller)
	if !found {
		writeError(w, http.StatusNotFound, "not_found",
			"no worktree is registered under that token — it may have expired, already been "+
				"discarded, or have been prepared by someone else")
		return
	}
	var removed, branchDeleted bool
	var err error
	if entry.branchCreated {
		removed, branchDeleted, err = worktree.RemoveLinkedWorktreeAndBranch(entry.path, entry.branch, true)
	} else {
		removed, err = worktree.RemoveLinkedWorktree(entry.path, true)
	}
	if err != nil {
		// The teardown failed, so the worktree is still there: put the
		// token back rather than spending it on a removal that did not
		// happen, and leave the caller (or the operator) able to retry.
		restorePreparedWorktree(token, entry)
		writeError(w, http.StatusInternalServerError, "worktree", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, agent.WorktreeDiscardResponse{Removed: removed, BranchDeleted: branchDeleted})
}

// spawnWorktreeRepoRoot resolves any path inside the target repo up to
// its root. A blank repo falls back to the daemon's own working
// directory — where a spawn with no directory of its own would land.
// status is the HTTP code the failure deserves: anything the caller can
// fix by editing a field is a 400, a daemon-side read failure is a 500.
func spawnWorktreeRepoRoot(repo string) (root string, status int, err error) {
	dir, err := resolveSpawnCwd(repo)
	if err != nil {
		return "", http.StatusBadRequest, err
	}
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return "", http.StatusInternalServerError,
				fmt.Errorf("read the daemon's working directory: %w", err)
		}
	}
	root, err = worktree.RepoRootForPath(dir)
	if err != nil {
		return "", http.StatusBadRequest, fmt.Errorf("--worktree needs a git repo: %w", err)
	}
	return root, http.StatusOK, nil
}
