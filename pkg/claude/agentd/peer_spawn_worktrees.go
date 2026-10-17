package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
// spawn into a worktree (and choose the directory) in some group it can see.
// The routes name a repository, not a group: like the directory grant, the
// worktree grant reaches any repository on this machine, and listing one
// shows its worktrees, branches and nested repositories. A peer never picks
// where a new worktree goes: this node's default sibling location applies.
func servePeerSpawnWorktree(next http.HandlerFunc) func(http.ResponseWriter, *http.Request, *peerView, peerViewRule) {
	return func(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
		if !peerSpawnWorktreeAllowed(v.peer.InstanceID) {
			writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires+" and "+PermGroupsMembersSpawnCwd)
			return
		}
		if r.Method == http.MethodPost && !peerWorktreeBodyHasNoPath(w, r) {
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
		if allowed := fedSpawnOverridesAllowed(peer, g.ID); allowed[fedOverrideWorktree] && allowed[fedOverrideCwd] {
			return true
		}
	}
	return false
}

// peerWorktreeBodyHasNoPath refuses a worktree creation that names its own
// destination, and restores the body for the route.
func peerWorktreeBodyHasNoPath(w http.ResponseWriter, r *http.Request) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	var body struct {
		Path string `json:"path"`
	}
	if err != nil || json.Unmarshal(raw, &body) != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid worktree request")
		return false
	}
	if strings.TrimSpace(body.Path) != "" {
		writeError(w, http.StatusForbidden, "permission", "a remote spawn cannot choose where its worktree is created")
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	return true
}
