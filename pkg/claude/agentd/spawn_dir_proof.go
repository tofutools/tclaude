package agentd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Spawn-dir write-proof — the launch-DIRECTORY half of the spawn sandbox
// guard. spawn_sandbox_guard.go stops an agent from minting a child with a
// looser sandbox MODE than its own; this file stops the remaining escape
// through the mode's anchor point: sandboxes grant write access rooted at
// the launch cwd, so an agent that picks the child's launch directory picks
// where that write access lands. Without a check, a parent whose own sandbox
// cannot touch /some/dir could spawn a child INTO /some/dir and use the
// child as its writable proxy there.
//
// The daemon cannot see the parent's sandbox policy (it is harness-specific,
// lives in settings files, and may not even exist for a raw caller), so
// instead the parent proves the capability directly: agentd hands out a
// single-use random token, the parent creates an empty file named after the
// token in every directory in question — a write its own sandbox must allow
// — and retries; agentd verifies the files exist, deletes them, and lets the
// spawn proceed. `tclaude agent spawn` runs inside the calling agent's
// sandbox, so it answers the challenge transparently; an agent whose sandbox
// forbids the write gets a clear refusal instead of an escape.
//
// Humans are exempt (they are the trust root everywhere else in agentd), as
// are parents whose recorded launch sandbox is fully open (Claude `off`,
// Codex `danger-full-access` — they can already write anywhere), and spawns
// whose CHILD gets no write access at its cwd (Codex `read-only`).

const (
	// dirWriteProofCode is the error code of the 403 challenge response.
	// The CLI (pkg/claude/agent) and the flow-test harness key on it.
	dirWriteProofCode = "write_proof_required"

	// dirWriteProofFilePrefix + token is the proof file's name. Hidden
	// (dot-prefixed) because it is transient plumbing: agentd deletes it as
	// part of verification, and the CLI best-effort removes it on failure.
	dirWriteProofFilePrefix = clcommon.SpawnDirWriteProofPrefix

	// dirWriteProofMaxPerConv caps outstanding challenges per caller so a
	// looping agent cannot grow the challenge table unboundedly; minting
	// past the cap evicts the caller's oldest outstanding challenge.
	dirWriteProofMaxPerConv = 8
)

// dirWriteProofTTL is how long a minted challenge token stays valid. A var
// (not a const) so unit tests can exercise expiry without sleeping.
var dirWriteProofTTL = 2 * time.Minute

type dirWriteChallenge struct {
	convID  string
	dirs    []string // symlink-resolved, deduped, sorted
	expires time.Time
}

var (
	dirWriteChallengeMu sync.Mutex
	dirWriteChallenges  = map[string]dirWriteChallenge{}
)

// mintDirWriteChallenge registers a fresh single-use challenge for convID
// over the (already resolved) dirs and returns its token. "" on the
// crypto/rand failure path — callers turn that into a 500.
func mintDirWriteChallenge(convID string, dirs []string) string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	token := hex.EncodeToString(buf)

	dirWriteChallengeMu.Lock()
	defer dirWriteChallengeMu.Unlock()
	now := time.Now()
	callerTokens := make([]string, 0, dirWriteProofMaxPerConv)
	for tok, ch := range dirWriteChallenges {
		if now.After(ch.expires) {
			delete(dirWriteChallenges, tok)
			continue
		}
		if ch.convID == convID {
			callerTokens = append(callerTokens, tok)
		}
	}
	if len(callerTokens) >= dirWriteProofMaxPerConv {
		oldest := callerTokens[0]
		for _, tok := range callerTokens[1:] {
			if dirWriteChallenges[tok].expires.Before(dirWriteChallenges[oldest].expires) {
				oldest = tok
			}
		}
		delete(dirWriteChallenges, oldest)
	}
	dirWriteChallenges[token] = dirWriteChallenge{
		convID:  convID,
		dirs:    dirs,
		expires: now.Add(dirWriteProofTTL),
	}
	return token
}

// takeDirWriteChallenge consumes a token: every lookup removes it, so a
// token is good for exactly one verification attempt, successful or not.
func takeDirWriteChallenge(token string) (dirWriteChallenge, bool) {
	dirWriteChallengeMu.Lock()
	defer dirWriteChallengeMu.Unlock()
	ch, ok := dirWriteChallenges[token]
	if ok {
		delete(dirWriteChallenges, token)
	}
	return ch, ok
}

// resolveDirWriteProofDirs canonicalises the launch dirs a request names:
// blank (= inherit the daemon's own cwd) becomes the daemon's cwd, symlinks
// are resolved so the dir the proof is verified in is the dir the child
// actually launches in (a symlink swapped between challenge and spawn must
// not retarget the grant), then deduped and sorted. Returns the canonical
// set plus a raw→resolved mapping the caller uses to pin the spawn onto the
// verified paths.
func resolveDirWriteProofDirs(rawDirs []string) ([]string, map[string]string, error) {
	mapping := make(map[string]string, len(rawDirs))
	var resolved []string
	for _, raw := range rawDirs {
		dir := raw
		if strings.TrimSpace(dir) == "" {
			wd, err := os.Getwd()
			if err != nil {
				return nil, nil, fmt.Errorf("resolve daemon working directory: %v", err)
			}
			dir = wd
		}
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve launch directory %s: %v", dir, err)
		}
		mapping[raw] = real
		if !slices.Contains(resolved, real) {
			resolved = append(resolved, real)
		}
	}
	slices.Sort(resolved)
	return resolved, mapping, nil
}

// dirWriteProofCallerExempt reports whether callerConvID is exempt from the
// write-proof: humans (empty / the dashboard sentinel) always, and agents
// whose own recorded launch sandbox is fully open — they can already write
// everywhere the child could, so there is nothing to prove.
func dirWriteProofCallerExempt(callerConvID string) (bool, error) {
	if callerConvID == "" || callerConvID == dashboardGranter {
		return true, nil
	}
	parent, err := spawnLineageParentSandbox(callerConvID)
	if err != nil {
		return false, err
	}
	if parent.Harness == harness.DefaultName && parent.Mode == harness.ClaudeSandboxOff {
		return true, nil
	}
	if parent.Harness == harness.CodexName && parent.Mode == harness.SandboxDangerFull {
		return true, nil
	}
	return false, nil
}

// childSandboxGrantsDirWrite reports whether a child launched with the given
// harness/sandbox gets write access rooted at its launch directory — the
// grant the write-proof protects. Only Codex read-only confers none; every
// other mode either writes its cwd subtree (Codex workspace-write / managed
// profile, Claude on/inherit) or is fully open (gated by the lineage guard
// to fully-open parents, which are proof-exempt anyway).
func childSandboxGrantsDirWrite(harnessName, mode string) bool {
	return harnessOrDefault(harnessName) != harness.CodexName ||
		strings.TrimSpace(mode) != harness.SandboxReadOnly
}

// requireDirWriteProof gates a dir-granting request behind the write-proof.
//
// Returns (mapping, true) when the request may proceed: mapping is
// raw-dir → symlink-resolved dir for a verified proof (the caller MUST
// substitute these so the spawn is pinned to the verified paths), or nil for
// an exempt caller (no substitution, no behaviour change).
//
// Returns (nil, false) after writing the HTTP response itself: a 403
// challenge (code "write_proof_required", with token / filename / dirs for
// the client to act on), a 403 refusal when the proof file is missing, or a
// 400/500 on resolution errors.
func requireDirWriteProof(w http.ResponseWriter, callerConvID, token string, rawDirs []string) (map[string]string, bool) {
	exempt, err := dirWriteProofCallerExempt(callerConvID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "dir write-proof: "+err.Error())
		return nil, false
	}
	if exempt {
		return nil, true
	}
	resolved, mapping, err := resolveDirWriteProofDirs(rawDirs)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
		return nil, false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		writeDirWriteProofChallenge(w, callerConvID, resolved)
		return nil, false
	}
	ch, ok := takeDirWriteChallenge(token)
	if !ok || ch.convID != callerConvID || time.Now().After(ch.expires) ||
		!slices.Equal(ch.dirs, resolved) {
		// Unknown, expired, foreign, or dir-set-mismatched token — issue a
		// fresh challenge rather than leaking WHY the old one was refused.
		writeDirWriteProofChallenge(w, callerConvID, resolved)
		return nil, false
	}

	filename := dirWriteProofFilePrefix + token
	var missing []string
	for _, dir := range ch.dirs {
		path := filepath.Join(dir, filename)
		fi, statErr := os.Lstat(path)
		if statErr != nil || !fi.Mode().IsRegular() {
			missing = append(missing, dir)
			continue
		}
	}
	if len(missing) > 0 {
		slog.Warn("spawn dir write-proof refused: proof file missing",
			"caller", short8(callerConvID), "dirs", strings.Join(missing, ","))
		writeError(w, http.StatusForbidden, "write_proof_failed", fmt.Sprintf(
			"write-permission proof file %s not found in: %s. Agent %s must itself be able "+
				"to create files in every directory the new agent would get write access to; "+
				"its sandbox evidently does not allow writing there, so it may not launch an "+
				"agent there either. Pick a directory you can write to, or have a human do the spawn.",
			filename, strings.Join(missing, ", "), short8(callerConvID)))
		return nil, false
	}
	slog.Info("spawn dir write-proof verified",
		"caller", short8(callerConvID), "dirs", strings.Join(ch.dirs, ","))
	return mapping, true
}

// reassertDirWriteProof re-verifies that each proof-verified dir is still the
// exact canonical path it was verified as: it must resolve to itself under
// EvalSymlinks (no symlink swapped into any component) and still be a
// directory. Called immediately before the spawn fork to close the window
// between HTTP-boundary verification and the child inheriting the cwd — a
// path swap performed after verification is refused here rather than launched
// into. Empty input (an exempt / unverified spawn) is a no-op.
func reassertDirWriteProof(dirs []string) *spawnFailure {
	for _, dir := range dirs {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return &spawnFailure{http.StatusForbidden, "write_proof_failed", fmt.Sprintf(
				"launch directory %s changed after its write-proof was verified: %v", dir, err)}
		}
		if real != dir {
			return &spawnFailure{http.StatusForbidden, "write_proof_failed", fmt.Sprintf(
				"launch directory %s was replaced by a symlink to %s after its write-proof "+
					"was verified; refusing to launch there", dir, real)}
		}
		fi, err := os.Lstat(dir)
		if err != nil || !fi.IsDir() {
			return &spawnFailure{http.StatusForbidden, "write_proof_failed", fmt.Sprintf(
				"launch directory %s is no longer a directory after its write-proof was verified", dir)}
		}
	}
	return nil
}

func cleanupDirWriteProofMarkers(token string, dirs []string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	filename := dirWriteProofFilePrefix + token
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		_ = os.Remove(filepath.Join(dir, filename))
	}
}

// requireTemplateDirWriteProof gates the template spawn surfaces (instantiate
// / deploy / reinforce) behind the dir write-proof for an AGENT caller. The
// whole cast shares one launch cwd, so proving it once covers every child; a
// shared worktree path and the per-agent-worktree repo (the write anchor the
// daemon cuts each child's worktree under) are proven too when present. On a
// verified proof it returns the symlink-resolved cwd / worktree the caller
// must pin the cast to. Humans and casts with no dir authority pass through
// unchanged (nil resolution). ok=false means it already wrote the HTTP
// response (a challenge, a refusal, or an error).
//
// Unlike handleGroupSpawn this does not skip read-only-child harnesses: a
// template roster mixes harnesses, and proving the shared launch dirs once is
// simpler and strictly safe. A caller that can write the dirs (the common
// "deploy into my project" case) clears it transparently via the CLI.
func requireTemplateDirWriteProof(w http.ResponseWriter, caller, token, cwd, worktreePath, repo string) (resolvedCwd, resolvedWorktree, resolvedRepo string, proofDirs []string, ok bool) {
	if caller == "" {
		return cwd, worktreePath, repo, nil, true
	}
	// Order fixed so the raw→resolved mapping keys are unambiguous.
	dirs := []string{cwd}
	if worktreePath != "" {
		dirs = append(dirs, worktreePath)
	}
	if repo != "" && repo != cwd && repo != worktreePath {
		dirs = append(dirs, repo)
	}
	resolved, proofOK := requireDirWriteProof(w, caller, token, dirs)
	if !proofOK {
		return "", "", "", nil, false
	}
	if resolved == nil { // exempt caller (fully-open sandbox)
		return cwd, worktreePath, repo, nil, true
	}
	resolvedCwd = cwd
	if v := resolved[cwd]; v != "" {
		resolvedCwd = v
	}
	resolvedWorktree = worktreePath
	if worktreePath != "" {
		if v := resolved[worktreePath]; v != "" {
			resolvedWorktree = v
		}
	}
	resolvedRepo = repo
	if repo != "" {
		if v := resolved[repo]; v != "" {
			resolvedRepo = v
		}
	}
	proofDirs = []string{resolvedCwd}
	if resolvedWorktree != "" && resolvedWorktree != resolvedCwd {
		proofDirs = append(proofDirs, resolvedWorktree)
	}
	if resolvedRepo != "" && resolvedRepo != resolvedCwd && resolvedRepo != resolvedWorktree {
		proofDirs = append(proofDirs, resolvedRepo)
	}
	return resolvedCwd, resolvedWorktree, resolvedRepo, proofDirs, true
}

// writeDirWriteProofChallenge mints a challenge and writes the 403 response
// that carries it. The body is self-describing on purpose: a raw caller (an
// LLM agent driving the HTTP API without the CLI) can read the instructions
// and answer the challenge without any other documentation.
func writeDirWriteProofChallenge(w http.ResponseWriter, convID string, dirs []string) {
	token := mintDirWriteChallenge(convID, dirs)
	if token == "" {
		writeError(w, http.StatusInternalServerError, "io", "dir write-proof: mint challenge token")
		return
	}
	filename := dirWriteProofFilePrefix + token
	writeJSON(w, http.StatusForbidden, map[string]any{
		"code": dirWriteProofCode,
		"error": fmt.Sprintf(
			"write-permission proof required: this request launches an agent with write access "+
				"under [%s], and the caller has not proven it can write there itself. Create an "+
				"empty file named %q in each of those directories (you must create it yourself, "+
				"from inside your own sandbox), then retry the same request with "+
				"write_proof_token=%q. The token is single-use and expires in %s.",
			strings.Join(dirs, ", "), filename, token, dirWriteProofTTL),
		"write_proof": map[string]any{
			"token":    token,
			"filename": filename,
			"dirs":     dirs,
		},
	})
}
