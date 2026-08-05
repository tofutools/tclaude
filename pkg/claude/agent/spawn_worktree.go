package agent

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// spawn_worktree.go is the CLI half of `tclaude agent spawn --worktree`
// (and `tclaude agent task-force deploy --worktree`): it asks agentd to
// resolve the branch into a worktree directory and, when a spawn that
// got a fresh worktree then fails, to take that worktree back down.
//
// The git commands run in the DAEMON, not here. A sandboxed agent's own
// filesystem restrictions have nothing to do with a checkout it neither
// reads nor writes, and a repo tracking a single file under a
// write-denied path (say .claude/commands/) would otherwise fail the
// checkout mid-way and abort the spawn. Creating the worktree is
// privileged host-level setup done on the new agent's behalf, exactly
// like the session launch that follows it, so it belongs on the same
// side of the socket. The daemon still refuses to create one where the
// caller could not have created it itself: an agent caller must answer
// the dir write-proof challenge for the repo and the directory the
// worktree lands in (see pkg/claude/agentd/spawn_worktree.go).
//
// The dashboard reaches the same resolution through its worktree picker
// before POSTing a spawn; the CLI has no picker, so it calls the
// endpoint directly and then sends the identical cwd / worktree_path /
// worktree_branch wire shape.

// WorktreePrepareRequest asks agentd for the worktree on Branch in the
// repo containing Repo, creating it when no worktree is checked out on
// that branch yet. Base names the branch a NEW branch is cut from
// (blank = the repo's default branch); it is ignored when the branch
// already exists. Repo may be any path inside the target repo; blank
// falls back to the daemon's own working directory.
//
// WriteProofToken carries the answer to a dir write-proof challenge —
// filled in transparently by DaemonRequestWithWriteProof, never by hand.
type WorktreePrepareRequest struct {
	Repo            string `json:"repo,omitempty"`
	Branch          string `json:"branch"`
	Base            string `json:"base,omitempty"`
	WriteProofToken string `json:"write_proof_token,omitempty"`
}

// WorktreePrepareResponse is the resolved worktree. Created distinguishes
// a freshly cut worktree from one that already existed on the branch, and
// DiscardToken (set only for a fresh one) names it for a later discard
// call, so a caller whose spawn then fails can undo exactly what this
// request did — and nothing else.
type WorktreePrepareResponse struct {
	Path         string `json:"path"`
	Branch       string `json:"branch"`
	Created      bool   `json:"created"`
	DiscardToken string `json:"discard_token,omitempty"`
}

// WorktreeDiscardRequest takes back a worktree prepared earlier in this
// daemon's lifetime, addressed by the token that created it.
type WorktreeDiscardRequest struct {
	Token string `json:"token"`
}

// WorktreeDiscardResponse reports what the teardown actually removed.
// BranchDeleted is true only when the daemon cut the branch itself for
// this worktree — a branch that already existed outlives the discard.
type WorktreeDiscardResponse struct {
	Removed       bool `json:"removed"`
	BranchDeleted bool `json:"branch_deleted"`
}

// worktreeDaemonTimeout bounds the prepare call. Cutting a worktree in a
// large repo is a full checkout, which can run well past the default
// request budget — the same reason the task-force deploy call takes a
// minutes-scale timeout.
const worktreeDaemonTimeout = 5 * time.Minute

// spawnWorktree is a resolved worktree as the spawn paths use it.
// DiscardToken is non-empty only when the daemon created the worktree
// for this request, which is exactly when a failed spawn should hand it
// back.
type spawnWorktree struct {
	Path         string
	Created      bool
	DiscardToken string
}

// resolveSpawnWorktree turns a `--worktree <branch>` request into a
// concrete worktree directory by asking agentd to reuse the worktree
// already checked out on that branch, or cut a new one (a new branch
// from base, or a checkout of an existing branch).
//
// repoDir is any path inside the target git repo; the daemon resolves it
// up to the repo root. ask carries the caller's --ask-human budget, so a
// spawn that leans on a human popup for its authority gets the same
// treatment on the worktree half rather than failing before the popup.
func resolveSpawnWorktree(repoDir, branch, base string, ask time.Duration) (spawnWorktree, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return spawnWorktree{}, fmt.Errorf("worktree branch name is required")
	}
	mkBody := func(writeProofToken string) any {
		return WorktreePrepareRequest{
			Repo:            strings.TrimSpace(repoDir),
			Branch:          branch,
			Base:            strings.TrimSpace(base),
			WriteProofToken: writeProofToken,
		}
	}
	var resp WorktreePrepareResponse
	// DaemonRequestWithWriteProof answers the daemon's dir write-proof
	// challenge from inside this process — i.e. inside the calling
	// agent's own sandbox, which is the capability being proven. The
	// proof files land in the repo root and the directory the worktree
	// will be created in, both of which the caller must be able to write
	// for the daemon to create a worktree there on its behalf.
	if err := DaemonRequestWithWriteProof(http.MethodPost, "/v1/worktrees/prepare", mkBody, &resp,
		DaemonOpts{Timeout: worktreeDaemonTimeout, AskHuman: ask}); err != nil {
		return spawnWorktree{}, err
	}
	if strings.TrimSpace(resp.Path) == "" {
		return spawnWorktree{}, fmt.Errorf("daemon returned no worktree path for branch %s", branch)
	}
	return spawnWorktree{Path: resp.Path, Created: resp.Created, DiscardToken: resp.DiscardToken}, nil
}

// discardSpawnWorktree tears down a worktree resolveSpawnWorktree just
// created, used when the spawn it was created for then fails. The daemon
// removes the working directory and — only when it cut the branch for
// this worktree — the branch too, so a retry starts from the requested
// base instead of silently reusing a branch left over from a failed
// attempt.
func discardSpawnWorktree(token string, ask time.Duration) (WorktreeDiscardResponse, error) {
	var resp WorktreeDiscardResponse
	token = strings.TrimSpace(token)
	if token == "" {
		return resp, fmt.Errorf("no discard token for this worktree")
	}
	err := DaemonRequest(http.MethodPost, "/v1/worktrees/discard", WorktreeDiscardRequest{Token: token}, &resp,
		DaemonOpts{Timeout: worktreeDaemonTimeout, AskHuman: ask})
	return resp, err
}

// undoSpawnWorktree hands a freshly created worktree back to the daemon
// after the spawn or deploy it was made for failed, and reports on
// stderr what actually happened — including whether the branch went with
// it, which decides what a retry will cut from. what names the failed
// operation ("spawn" / "deploy") for the message.
func undoSpawnWorktree(stderr io.Writer, path, token, what string, ask time.Duration) {
	resp, err := discardSpawnWorktree(token, ask)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "Note: could not remove the worktree created for this %s (%s): %v\n",
			what, path, err)
	case resp.BranchDeleted:
		fmt.Fprintf(stderr, "Note: removed the worktree and branch created for this %s (%s)\n",
			what, path)
	default:
		fmt.Fprintf(stderr, "Note: removed the worktree created for this %s (%s)\n", what, path)
	}
}
