package agentd

import (
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
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
// pre-trusted into a directory the CALLING agent is already working in — its
// recorded startup dir, the dir its edits are landing in, or any subdirectory
// of either. That crosses no privilege boundary. Pre-trusting a dir means the
// child will run the harness config that dir carries without a trust dialog;
// inside the caller's own tree the caller can already write that config (the
// dir write-proof makes it prove exactly that for the child's cwd), so the
// trust grants the child nothing the caller does not already hold. Containment
// also cannot be used to WIDEN the caller's reach: every root comes from
// tclaude's own record of where the caller is, and a subdirectory is strictly
// narrower than its root.
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
const trustDirRestrictedMessage = "agent-initiated spawns may pre-trust only a directory the calling agent " +
	"already works in (its start dir, its current dir, or a subdirectory of either), or a worktree at the " +
	"default <repo>-<branch> sibling path tclaude itself would create; leave trust_dir off or ask the human " +
	"to spawn this child"

// callerOwnedDirTrust reports whether cwd lies within a directory the spawning
// agent identified by spawnerConvID is already working in, and may therefore be
// pre-trusted on that agent's behalf.
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

// callerOwnedTrustRoots returns the symlink-resolved directories the caller is
// known to be working in: its startup dir (both the lexically recorded cwd and,
// when resume provenance pinned one, the immutable physical path) and the
// most-recent dir its edits landed in.
//
// The git working-tree ROOT tclaude also tracks is deliberately NOT a root
// here. It is derived by walking UP from the edit dir, so for an agent editing
// deep inside a repo it names a tree wider than the one the agent is working
// in — the one place this set could grow rather than narrow.
func callerOwnedTrustRoots(convID string) []string {
	loc := agent.ResolveLocation(convID)
	raw := []string{loc.StartupDir, loc.EditDir}
	if sess, err := db.FindSessionByConvID(convID); err == nil && sess != nil {
		raw = append(raw, sess.Cwd)
		if physical, perr := recordedStartupDir(sess); perr == nil {
			raw = append(raw, physical)
		}
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
