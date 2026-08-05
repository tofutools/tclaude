package agentd

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Caller-owned launch dirs — the second exemption that lets an AGENT-initiated
// spawn request trust_dir at all (defaultSiblingWorktreeTrust in
// spawn_dir_proof.go is the first).
//
// The sibling-worktree exemption is defined over a git layout, which cannot
// describe the directory plenty of agents actually start in: a workspace ROOT
// holding N repo clones side by side is not a repo, so it is not a worktree of
// anything and cannot be a sibling of anything. An agent launched there had no
// permitted `--cwd` at all — not even its own start dir, and not even the
// documented "leave --cwd off and inherit mine" — so every child had to be
// spawned by the human, indefinitely.
//
// The predicate here is containment instead of layout: a child may be
// pre-trusted into the CALLING agent's own launch directory, or any
// subdirectory of it. That crosses no privilege boundary. Pre-trusting a dir
// means the child will run the harness config that dir carries without a trust
// dialog; inside the caller's own tree the caller can already write that config
// — the dir write-proof makes it prove exactly that for the child's cwd, and
// this exemption FORCES that proof even for a child whose own sandbox would not
// have required one (see the proof trigger in handleGroupSpawn) — so the trust
// grants the child nothing the caller does not already hold. Containment also
// cannot be used to WIDEN the caller's reach: a subdirectory is strictly
// narrower than its root.
//
// The root set is deliberately confined to state the CALLER DOES NOT AUTHOR.
// The launch dir is recorded by the daemon at spawn time (and pinned physically
// by resume provenance); the caller never writes it. The edit dir tclaude also
// tracks — `agent_workdir`, the "current dir" of `tclaude agent dir` — is
// excluded for exactly that reason: it is recorded from a tool-use payload by
// the PostToolUse hook, including on the FAILURE arm, so an agent could nominate
// any path by attempting an edit its own sandbox denies. That is display state,
// not an authorization root, and promoting it would hand the caller the pen.
//
// Unlike the sibling-worktree exemption this only PERMITS trust_dir; it never
// forces it on. A fresh default sibling worktree must be trusted or the
// detached child hangs on the dialog; a directory the caller already lives in
// is usually trusted already, so the opt-in stays the operator's (or the spawn
// profile's) to make.

// trustDirRestrictedMessage is the refusal both spawn paths return when an
// agent caller asked for trust_dir over a directory neither exemption covers.
//
// It names the permitted shapes rather than the old "tclaude's verified default
// sibling worktrees" phrasing, which read as a filesystem property when the real
// predicate is the DEFAULT PATH tclaude itself would have picked — sending
// operators to inspect an ordinary sibling worktree that was never going to
// qualify.
const trustDirRestrictedMessage = "agent-initiated spawns may pre-trust only the calling agent's own launch " +
	"directory or a subdirectory of it, or a worktree at the default <repo>-<branch> sibling path tclaude " +
	"itself would create; leave trust_dir off or ask the human to spawn this child"

// callerOwnedDirTrust reports whether cwd is the launch directory of the
// spawning agent identified by spawnerConvID, or lies beneath it, and may
// therefore be pre-trusted on that agent's behalf.
//
// A true answer GRANTS, so the comparison is deliberately allow-biased in the
// safe direction: both sides are fully symlink-resolved and compared byte-exact
// (sandboxpolicy.PathContainsOrEqual, not the refuse-biased
// GuardContainsOrEqual). A path that does not resolve — or a case-variant
// spelling on a case-insensitive volume — simply fails to match, which is a
// refusal.
func callerOwnedDirTrust(spawnerConvID, cwd string) bool {
	if strings.TrimSpace(spawnerConvID) == "" {
		return false
	}
	target := resolveTrustRootPath(cwd)
	if target == "" {
		return false
	}
	for _, root := range callerOwnedTrustRoots(spawnerConvID) {
		if sandboxpolicy.PathContainsOrEqual(root, target) {
			return true
		}
	}
	return false
}

// callerOwnedDirTrustProved is callerOwnedDirTrust plus the write-proof pairing
// the exemption's justification rests on, asserted at the gate rather than
// assumed of the caller: the launch dir must be one the caller demonstrably
// could have written itself.
//
// A caller the proof exempts entirely (a human, or a parent whose own sandbox
// is fully open) passes on the same terms it passes everywhere else — it can
// already write anywhere, so there is nothing to demonstrate. Everyone else must
// arrive with a token whose verified dir set covers this exact resolved cwd,
// which handleGroupSpawn's proof block and requireTemplateDirWriteProof both
// produce. A path that entered the proof and a path that entered this check must
// resolve identically for the pairing to hold, so both go through
// resolveTrustRootPath.
func callerOwnedDirTrustProved(spawnerConvID, cwd, proofToken string, proofDirs []string) (bool, error) {
	if !callerOwnedDirTrust(spawnerConvID, cwd) {
		return false, nil
	}
	exempt, err := dirWriteProofCallerExempt(spawnerConvID)
	if err != nil {
		return false, err
	}
	if exempt {
		return true, nil
	}
	if strings.TrimSpace(proofToken) == "" {
		return false, nil
	}
	resolved := resolveTrustRootPath(cwd)
	return resolved != "" && slices.Contains(proofDirs, resolved), nil
}

// callerOwnedTrustRoots returns the symlink-resolved launch directories of the
// caller: the lexically recorded cwd and, when resume provenance pinned one,
// the immutable physical path behind it (which differ when the recorded cwd
// went through a symlink that was later removed or retargeted).
//
// Both come from the daemon's own session record, and nothing else does. The
// tracked edit dir is excluded for the reason given in the file comment; so is
// agent.ResolveLocation's conv_index fallback, which reads the cwd recorded in
// the harness's own transcript — a file that lives inside the agent's project
// tree and is therefore, unlike the session row, something the agent can write.
// A caller with no session row has no known launch dir, and gets no exemption.
func callerOwnedTrustRoots(convID string) []string {
	sess, err := db.FindSessionByConvID(convID)
	if err != nil || sess == nil {
		return nil
	}
	raw := []string{sess.Cwd}
	if physical, perr := recordedStartupDir(sess); perr == nil {
		raw = append(raw, physical)
	}
	var roots []string
	for _, dir := range raw {
		roots = appendUniqueDirs(roots, resolveTrustRootPath(dir))
	}
	return roots
}

// resolveTrustRootPath canonicalizes a directory for the containment test above,
// returning "" for anything it cannot pin to a real absolute path.
func resolveTrustRootPath(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	return filepath.Clean(resolved)
}
