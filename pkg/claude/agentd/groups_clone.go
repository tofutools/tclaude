package agentd

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// groupCloneAfterProofForTest is a deterministic seam for liveness-transition
// flow tests. Production leaves it nil.
var groupCloneAfterProofForTest func()

// handleGroupClone clones an entire group: snapshots source members +
// owners, creates a new group, clones each member into the new group
// using the same spawn machinery as `agent clone`, then optionally
// copies owners (same conv-id, no clone) onto the new group.
//
// Permission slug: groups.clone (default human-only). Same dispatcher
// as the other groups.* verbs.
//
// Failure handling: best-effort per member. If the new group is created
// successfully but a member-clone fails partway, the new group is left
// in place with the members that succeeded so far + their per-member
// status surfaced in the response. The human can retry the failed ones
// with `agent clone --target <new-group>`. We deliberately do NOT
// auto-rollback — partial success is recoverable, full rollback would
// destroy the work that did land.
func handleGroupClone(w http.ResponseWriter, r *http.Request, src *db.AgentGroup) {
	caller, ok := requirePermission(w, r, PermGroupsClone)
	if !ok {
		return
	}
	if !requireGroupActive(w, src) {
		return
	}
	var body struct {
		NewName    string `json:"new_name,omitempty"`
		NoCopyConv bool   `json:"no_copy_conv,omitempty"`
		// NoCloneMembers skips the per-member clone loop entirely: the new
		// group is created with every source setting but no member agents.
		// The "clone the group config, not the workers" mode. Default false
		// = clone members (the original behaviour).
		NoCloneMembers bool `json:"no_clone_members,omitempty"`
		// CopyOwners is pointer-valued for compatibility: omitted preserves
		// the historical API/CLI default of copying owner rows, while the
		// dashboard can explicitly opt out when it clones settings only.
		CopyOwners      *bool  `json:"copy_owners,omitempty"`
		WriteProofToken string `json:"write_proof_token,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
	}
	newName := body.NewName
	if newName == "" {
		newName = nextGroupCloneName(src.Name)
	} else {
		if err := validateGroupName(newName); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
	}
	if existing, _ := db.GetAgentGroupByName(newName); existing != nil {
		writeError(w, http.StatusConflict, "exists",
			"a group named \""+newName+"\" already exists")
		return
	}
	copyOwners := true
	if body.CopyOwners != nil {
		copyOwners = *body.CopyOwners
	}

	// Snapshot source state. Read-only — partial reads here just mean
	// the response under-reports, never writes the wrong thing.
	srcMembers, err := db.ListAgentGroupMembers(src.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"snapshot members: "+err.Error())
		return
	}
	srcOwners, err := db.ListAgentGroupOwners(src.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"snapshot owners: "+err.Error())
		return
	}
	type memberGrant struct {
		commonDir string
		writeDirs []string
		cwd       string
		// skipOffline binds the proof-time liveness snapshot. A member that
		// comes online later in the same request was not part of the proved
		// launch set and must not be cloned without a fresh challenge.
		skipOffline bool
	}
	memberGrants := map[string]memberGrant{}
	var proofDirs []string
	proofToken := ""
	if !body.NoCloneMembers && !isHumanCloneCaller(caller) {
		var rawDirs []string
		for _, member := range srcMembers {
			sess := pickAliveSession(member.ConvID)
			if sess == nil {
				memberGrants[member.ConvID] = memberGrant{skipOffline: true}
				continue
			}
			harnessName := harnessForConv(member.ConvID).Name
			sandboxMode := sandboxForHarness(harnessName)
			commonDir, err := spawnGitCommonDir(harnessName, sandboxMode, sess.Cwd)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			}
			grant := memberGrant{commonDir: commonDir, cwd: sess.Cwd}
			if spawnUsesPinnedGitCommonDir(harnessName, sandboxMode) {
				home, err := os.UserHomeDir()
				if err != nil {
					writeError(w, http.StatusInternalServerError, "io", err.Error())
					return
				}
				grant.writeDirs = harness.GitWorktreeWriteDirs(sess.Cwd, commonDir, home)
			}
			memberGrants[member.ConvID] = grant
			rawDirs = appendUniqueDirs(rawDirs, sess.Cwd)
			rawDirs = appendUniqueDirs(rawDirs, grant.writeDirs...)
		}
		if len(rawDirs) > 0 {
			resolved, ok := requireDirWriteProof(w, caller, body.WriteProofToken, rawDirs)
			if !ok {
				return
			}
			if resolved != nil {
				proofToken = strings.TrimSpace(body.WriteProofToken)
				for convID, grant := range memberGrants {
					if v := resolved[grant.cwd]; v != "" {
						grant.cwd = v
					}
					for i, dir := range grant.writeDirs {
						if v := resolved[dir]; v != "" {
							grant.writeDirs[i] = v
						}
					}
					memberGrants[convID] = grant
				}
				for _, dir := range rawDirs {
					proofDirs = appendUniqueDirs(proofDirs, resolved[dir])
				}
			}
		}
	}
	if proofToken != "" {
		defer cleanupDirWriteProofMarkers(proofToken, proofDirs)
	}
	if groupCloneAfterProofForTest != nil {
		groupCloneAfterProofForTest()
	}

	// Clone EVERY configurable setting onto the new group — default cwd,
	// startup context, default profile, max-members cap and the notify
	// switch — not just the description. A clone is meant to come up
	// configured identically to its source.
	//
	// normalizeGroupDescr again on the cloned-over descr: the source
	// value was stored before this fold existed, so re-applying it
	// keeps the one-line header invariant on the clone too.
	srcSettings := *src
	srcSettings.Descr = normalizeGroupDescr(src.Descr)
	newGroupID, err := db.CreateAgentGroupFrom(newName, srcSettings)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"create new group: "+err.Error())
		return
	}

	granter := "system:groups.clone"
	if caller != "" {
		granter = "system:groups.clone:by=" + auditedCaller(caller, PermGroupsClone)
	}

	// Per-member clone results. Each row carries the source conv-id
	// (so the human can correlate) plus the new conv-id (when it
	// succeeded) or an error message (when it didn't).
	type memberResult struct {
		SrcConv string `json:"src_conv"`
		NewConv string `json:"new_conv,omitempty"`
		Title   string `json:"title,omitempty"`
		Label   string `json:"label,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]memberResult, 0, len(srcMembers))

	// "Without agents" mode skips the per-member clone loop — the new
	// group keeps the source settings but gets no member agents. results
	// stays an empty slice, surfacing as 0 members in the response.
	membersToClone := srcMembers
	if body.NoCloneMembers {
		membersToClone = nil
	}
	for _, m := range membersToClone {
		grant, proofStateBound := memberGrants[m.ConvID]
		if proofStateBound && grant.skipOffline {
			results = append(results, memberResult{
				SrcConv: m.ConvID,
				Error:   "skipped: source had no live tmux session when launch authority was proved",
			})
			continue
		}
		// Source member must be alive — cloneSpawnOnce reads the cwd
		// from a live tmux session. Surface a clear "skipped — offline"
		// status instead of failing the whole group clone.
		oldSess := pickAliveSession(m.ConvID)
		if oldSess == nil {
			results = append(results, memberResult{
				SrcConv: m.ConvID,
				Error:   "skipped: source has no live tmux session (cwd unknown)",
			})
			continue
		}
		// Each clone runs the model + effort its own source is live on
		// (inheritedLaunchFlags; "" falls back to claude's default).
		effort, model := inheritedLaunchFlags(oldSess.ID)
		srcHarness := harnessForConv(m.ConvID).Name
		cloneSandbox := sandboxForHarness(srcHarness)
		codexGitCommonDir, gerr := spawnGitCommonDir(srcHarness, cloneSandbox, oldSess.Cwd)
		if gerr != nil {
			results = append(results, memberResult{
				SrcConv: m.ConvID,
				Error:   "spawn: " + gerr.Error(),
			})
			continue
		}
		if grant.commonDir != "" || grant.writeDirs != nil {
			codexGitCommonDir = grant.commonDir
		}
		cloneCwd := oldSess.Cwd
		if grant.cwd != "" {
			cloneCwd = grant.cwd
		}
		newConv, _, label, warn, spawnErr := cloneSpawnOnce(m.ConvID, cloneCwd, body.NoCopyConv, effort, model, proofToken, proofToken != "", proofDirs, codexGitCommonDir, grant.writeDirs)
		if spawnErr != nil {
			results = append(results, memberResult{
				SrcConv: m.ConvID,
				Error:   "spawn: " + spawnErr.Msg,
			})
			continue
		}
		if warn != "" {
			slog.Warn("groups clone: spawn-warn", "src", m.ConvID, "new_conv", newConv, "warning", warn)
		}
		// Add ONLY to the new group — the source group is left
		// untouched. Per-conv permissions are copied so the clone
		// inherits its source's slugs (mirrors runCloneOrchestration).
		//
		// The clone's title is derived from the source member's title
		// as `<base>-c-<N>`, then injected via /rename below — exactly
		// the per-conv `agent clone` naming scheme. Membership rows
		// carry no name of their own.
		newTitle := uniqueCloneTitle(agent.FreshTitle(m.ConvID))
		newMember := &db.AgentGroupMember{
			GroupID: newGroupID,
			ConvID:  newConv,
			Role:    m.Role,
			Descr:   m.Descr,
		}
		if err := db.AddAgentGroupMember(newMember); err != nil {
			slog.Warn("groups clone: add new member failed",
				"group", newName, "conv", newConv, "error", err)
			results = append(results, memberResult{
				SrcConv: m.ConvID,
				NewConv: newConv,
				Title:   newTitle,
				Label:   label,
				Error:   "add to new group: " + err.Error(),
			})
			continue
		}
		// Rename the clone to its computed title, materialising the
		// .jsonl — same post-init the per-conv clone path runs.
		srcConv := m.ConvID
		goBackground(func() { runClonePostInit(newConv, newTitle, srcConv, caller) })
		// Per-conv perms — grant AND deny overrides, best-effort, mirror
		// runCloneOrchestration.
		if perms, err := db.ListAgentPermissionOverridesForConv(m.ConvID); err == nil {
			for slug, effect := range perms {
				if err := db.SetAgentPermissionOverride(newConv, slug, effect, granter); err != nil {
					slog.Warn("groups clone: copy perm failed",
						"group", newName, "conv", newConv, "slug", slug, "effect", effect, "error", err)
				}
			}
		}
		results = append(results, memberResult{
			SrcConv: m.ConvID,
			NewConv: newConv,
			Title:   newTitle,
			Label:   label,
		})
	}

	// Owners stay as the same conv-id — they're separate from members.
	// Same conv-id, same granted_by audit semantics as a manual grant.
	ownersCopied := 0
	if copyOwners {
		for _, o := range srcOwners {
			if err := db.AddAgentGroupOwner(newGroupID, o.ConvID, granter); err != nil {
				slog.Warn("groups clone: add owner failed",
					"group", newName, "owner", o.ConvID, "error", err)
				continue
			}
			ownersCopied++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"group":         newName,
		"src_group":     src.Name,
		"members":       results,
		"owners_copied": ownersCopied,
	})
}

// nextGroupCloneName picks the smallest free `<base>-c-<N>` slot
// in the agent_groups namespace. base is srcName with any trailing
// `-c-<digits>` stripped, so a clone-of-a-clone bumps N rather than
// nesting (`team-c-2` clones to `team-c-3`, not `team-c-2-c-1`).
// Reuses the same regex as the per-conv clone-title logic for symmetry.
//
// Lookup error → fall back to `<base>-c-1`. The caller will hit the
// 409 collision check next, so a corrupt DB still fails loudly
// rather than silently picking a colliding name.
func nextGroupCloneName(srcName string) string {
	base := srcName
	if m := cloneSuffixRegex.FindStringSubmatch(base); m != nil {
		base = m[1]
	}
	prefix := base + "-c-"
	used := scanGroupCloneSuffixes(prefix)
	for n := 1; ; n++ {
		if !used[n] {
			return prefix + strconv.Itoa(n)
		}
	}
}

// scanGroupCloneSuffixes walks every group and returns the set of
// integers N where some name equals `<prefix><N>`. Used by
// nextGroupCloneName to pick the smallest free N. Sibling of
// scanCloneSuffixes but scoped to group names rather than conv
// titles.
func scanGroupCloneSuffixes(prefix string) map[int]bool {
	used := map[int]bool{}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return used
	}
	for _, g := range groups {
		if !strings.HasPrefix(g.Name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(g.Name, prefix)
		n, err := strconv.Atoi(suffix)
		if err != nil {
			continue
		}
		used[n] = true
	}
	return used
}
