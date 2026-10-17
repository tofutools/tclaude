package agentd

import (
	"context"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A remote spawn into a git worktree (groups.members.spawn.worktree): the
// peer's dialog lists a repository's worktrees and branches, and creates the
// worktree before it sends the spawn request, through this node's own
// worktree routes -- as this node's dialog does. Only these methods open on
// the peer path; worktree cleanup and every other method stay local-only.
var peerSpawnWorktreeRoutes = map[string]http.HandlerFunc{
	"GET /api/worktrees":          handleDashboardWorktreesAPI,
	"POST /api/worktrees":         handleDashboardWorktreesAPI,
	"GET /api/worktrees/progress": dashboardWorktreeProgressAPI,
}

func addPeerSpawnWorktreeRules(rules map[string]peerViewRule) {
	for pattern, next := range peerSpawnWorktreeRoutes {
		rule := peerViewRule{feature: "spawn.worktree", requires: PermGroupsMembersSpawnWorktree}
		serve := servePeerSpawnWorktree(next)
		if strings.HasPrefix(pattern, "GET ") {
			rule.serve = serve
		} else {
			rule.write = serve
		}
		rules[pattern] = rule
	}
}

// servePeerSpawnWorktree runs this node's worktree route for a peer that may
// spawn into a worktree in some group it can see. The routes name a
// repository, not a group: like the directory grant, the worktree grant
// reaches any repository on this machine.
func servePeerSpawnWorktree(next http.HandlerFunc) func(http.ResponseWriter, *http.Request, *peerView, peerViewRule) {
	return func(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
		if !peerSpawnWorktreeAllowed(v.peer.InstanceID) {
			writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
			return
		}
		next(w, r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true)))
	}
}

// peerSpawnWorktreeAllowed reports whether peer may launch a remote spawn in
// a worktree in any group it can see.
func peerSpawnWorktreeAllowed(peer string) bool {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g == nil || g.IsArchived() || !fedPeerGroupVisible(peer, g.ID) {
			continue
		}
		if fedSpawnOverridesAllowed(peer, g.ID)[fedOverrideWorktree] {
			return true
		}
	}
	return false
}
