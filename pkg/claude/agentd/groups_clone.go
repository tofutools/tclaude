package agentd

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// handleGroupClone clones an entire group: snapshots source members +
// owners, creates a new group, clones each member into the new group
// using the same spawn machinery as `agent clone`, then copies owners
// (same conv-id, no clone) onto the new group.
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

	// normalizeGroupDescr again on the cloned-over descr: the source
	// value was stored before this fold existed, so re-applying it
	// keeps the one-line header invariant on the clone too.
	newGroupID, err := db.CreateAgentGroup(newName, normalizeGroupDescr(src.Descr))
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

	for _, m := range srcMembers {
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
		newConv, _, label, warn, spawnErr := cloneSpawnOnce(m.ConvID, oldSess.Cwd, body.NoCopyConv)
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
	for _, o := range srcOwners {
		if err := db.AddAgentGroupOwner(newGroupID, o.ConvID, granter); err != nil {
			slog.Warn("groups clone: add owner failed",
				"group", newName, "owner", o.ConvID, "error", err)
			continue
		}
		ownersCopied++
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
