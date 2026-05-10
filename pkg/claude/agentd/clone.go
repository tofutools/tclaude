package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// `tclaude agent clone` — fork the calling agent into a sibling that
// inherits its identity (groups, permissions, ownerships) but
// continues running independently. Unlike reincarnate, the original
// is NOT shut down and its identity rows are NOT removed.
//
// Two modes:
//
//   - default: copy the original's conv jsonl onto a fresh conv-id,
//     then spawn a new tclaude session with `-r <new-conv>`. The
//     clone starts with the SAME context as the original — useful for
//     "fork a worker to try a parallel approach."
//   - --no-copy-conv: skip the jsonl copy, spawn fresh CC. The clone
//     inherits identity only — useful for "stand up a peer in the
//     same role without dragging the conversation history along."
//
// Identity in shared groups: the clone joins each of the original's
// groups with alias `<original-alias>-clone` (or `-clone` if the
// original had no alias). v1 doesn't auto-increment the suffix on
// collision; subsequent clones of the same original would clobber
// each other's clone-row alias if attempted in succession (the
// underlying INSERT OR REPLACE is keyed on (group_id, conv_id), and
// each clone has its own conv_id, so the clobber is alias-scoped
// only — the rows themselves are distinct).

// cloneSuffixRegex matches a trailing clone suffix in either the
// current short form `-c-<digits>` or the legacy long form
// `-clone-<digits>`. Recognising both lets a legacy
// `worker-clone-3` cleanly transition to `worker-c-1` (rather than
// nesting as `worker-clone-3-c-1`) the next time it's cloned. Same
// idea for reincarnateSuffixRegex.
var cloneSuffixRegex = regexp.MustCompile(`^(.*?)-(?:c|clone)-\d+$`)

// uniqueCloneAlias computes the clone's per-group alias. The format
// is ALWAYS `<base>-c-<N>` (or `c-<N>` when the original had no
// alias in this group). base is origAlias with any existing
// `-c-<digits>` / `-clone-<digits>` stripped, so a clone-of-a-clone
// bumps N rather than nesting suffixes (`worker-c-3` clones to
// `worker-c-4`, not `worker-c-3-c-1`). The short `-c-` is paired
// with `-r-` for reincarnations — distinct enough at a glance,
// short enough to tile in dashboard rows.
//
// N is chosen globally — the smallest integer such that
// `<base>-c-<N>` doesn't appear as the alias of any
// agent_group_members row anywhere. This intentionally does NOT
// scope by group: the same clone uses the same alias across every
// group it inherits, and parallel clones of the same original each
// pick distinct N's regardless of which groups they're added to.
// The "used" set scans only the new short prefix; legacy
// `-clone-N` aliases don't reserve a number in the new namespace
// (avoids surprising holes after a changeover).
//
// Lookup error → fall back to N=1 (best-effort).
func uniqueCloneAlias(origAlias string) string {
	base := origAlias
	if m := cloneSuffixRegex.FindStringSubmatch(base); m != nil {
		base = m[1]
	}
	prefix := "c-"
	if base != "" {
		prefix = base + "-c-"
	}
	used := scanCloneSuffixesGlobal(prefix)
	for n := 1; ; n++ {
		if !used[n] {
			return prefix + strconv.Itoa(n)
		}
	}
}

// scanCloneSuffixesGlobal walks every group's members and returns the
// set of integers N where some alias equals `<prefix><N>`. Used by
// uniqueCloneAlias to pick the smallest free N.
func scanCloneSuffixesGlobal(prefix string) map[int]bool {
	used := map[int]bool{}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return used
	}
	for _, g := range groups {
		members, err := db.ListAgentGroupMembers(g.ID)
		if err != nil {
			continue
		}
		for _, m := range members {
			if !strings.HasPrefix(m.Alias, prefix) {
				continue
			}
			suffix := strings.TrimPrefix(m.Alias, prefix)
			n, err := strconv.Atoi(suffix)
			if err != nil {
				continue
			}
			used[n] = true
		}
	}
	return used
}

// handleWhoamiClone handles POST /v1/whoami/clone (self path).
// Gated on self.clone (default-granted alongside self.compact /
// self.reincarnate). Delegates to runCloneOrchestration with
// target == caller.
func handleWhoamiClone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requirePermission(w, r, PermSelfClone)
	if !ok {
		return
	}
	if caller == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint clones the calling agent's own conversation; humans should use `tclaude conv copy` directly, or use POST /v1/agent/{conv}/clone to clone another agent")
		return
	}
	followUp, noCopyConv, ok := decodeCloneBody(w, r)
	if !ok {
		return
	}
	runCloneOrchestration(w, caller, caller, followUp, noCopyConv)
}

// handleAgentClone handles POST /v1/agent/{conv}/clone (cross-agent).
// Gated on agent.clone OR group-owner-of-target.
func handleAgentClone(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentClone, targetConv)
	if !ok {
		return
	}
	followUp, noCopyConv, ok := decodeCloneBody(w, r)
	if !ok {
		return
	}
	runCloneOrchestration(w, targetConv, caller, followUp, noCopyConv)
}

// decodeCloneBody parses + validates the optional follow_up and
// no_copy_conv body fields.
func decodeCloneBody(w http.ResponseWriter, r *http.Request) (followUp string, noCopyConv bool, ok bool) {
	var body struct {
		FollowUp   string `json:"follow_up"`
		NoCopyConv bool   `json:"no_copy_conv"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return "", false, false
		}
	}
	body.FollowUp = strings.TrimSpace(body.FollowUp)
	if body.FollowUp != "" && !isValidFollowUp(body.FollowUp) {
		writeError(w, http.StatusBadRequest, "invalid_follow_up",
			"REJECTED. Follow-up must be 1-4096 printable characters; tabs, newlines, "+
				"and other control characters are not allowed (each newline would be "+
				"treated as a submit by tmux send-keys, splitting the prompt).")
		return "", false, false
	}
	return body.FollowUp, body.NoCopyConv, true
}

// runCloneOrchestration is the target-agnostic body shared by self
// and cross-agent clone endpoints.
//
//   - target is the conv being cloned (its identity gets copied to the
//     new conv-id).
//   - caller is the conv that triggered the clone; recorded in the
//     audit trail (`system:clone:by=<caller>` for cross calls) and
//     used as the FromConv on the optional handoff message.
func runCloneOrchestration(w http.ResponseWriter, target, caller, followUp string, noCopyConv bool) {
	// 1. Snapshot target state. Same shape as reincarnate's snapshot
	// pass.
	oldSess := pickAliveSession(target)
	if oldSess == nil {
		writeError(w, http.StatusServiceUnavailable, "no_tmux",
			"target conv "+short8(target)+" has no live tmux session; can't clone without a cwd to spawn the sibling into")
		return
	}
	cwd := oldSess.Cwd

	oldGroups, err := db.ListGroupsForConv(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"snapshot groups: "+err.Error())
		return
	}
	oldMembers := make([]*db.AgentGroupMember, 0, len(oldGroups))
	for _, g := range oldGroups {
		m, err := db.FindMemberInGroup(g.ID, target)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io",
				"snapshot membership: "+err.Error())
			return
		}
		if m != nil {
			oldMembers = append(oldMembers, m)
		}
	}

	oldPerms, err := db.ListAgentPermissionsForConv(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"snapshot perms: "+err.Error())
		return
	}

	oldOwnedIDs, err := db.ListGroupsOwnedBy(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"snapshot ownerships: "+err.Error())
		return
	}

	// 2. Mint the clone's conv-id (and optionally its jsonl). Two
	// branches:
	//   - copy: use convops to fork the existing jsonl onto a fresh
	//     conv-id; then spawn `tclaude session new -r <new-conv>` so
	//     CC loads the cloned conversation.
	//   - no-copy: spawn `tclaude session new --label <label>` and
	//     poll for whatever conv-id CC mints, same as reincarnate.
	var newConv, newTmux, label string
	if noCopyConv {
		label = generateSpawnLabel()
		if err := spawnDetachedTclaudeNew(label, cwd); err != nil {
			writeError(w, http.StatusInternalServerError, "spawn",
				"failed to launch tclaude session new: "+err.Error())
			return
		}
		// Same poll loop as reincarnate.
		deadline := time.Now().Add(reincarnateSpawnTimeout)
		for time.Now().Before(deadline) {
			s, err := db.LoadSession(label)
			if err == nil && s != nil {
				newTmux = s.TmuxSession
				if s.ConvID != "" {
					newConv = s.ConvID
					break
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		if newConv == "" {
			writeError(w, http.StatusGatewayTimeout, "timeout",
				"spawned session "+label+" but conv-id never materialised within "+
					reincarnateSpawnTimeout.String()+
					" — the session may still come up; check `tclaude session attach "+label+"`")
			return
		}
	} else {
		// Copy the jsonl first; this gives us the new conv-id
		// up-front, which we then resume into.
		copyResult, err := convops.CopyConversationToPath(target, cwd, true /* global */)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "copy",
				"failed to copy conversation jsonl: "+err.Error())
			return
		}
		newConv = copyResult.NewConvID
		if err := spawnDetachedTclaudeResume(newConv, cwd); err != nil {
			writeError(w, http.StatusInternalServerError, "spawn",
				"failed to launch tclaude session new -r: "+err.Error())
			return
		}
		// Wait for the session row to materialise so we can return
		// the tmux session name in the response.
		deadline := time.Now().Add(reincarnateSpawnTimeout)
		for time.Now().Before(deadline) {
			if s, err := db.FindSessionByConvID(newConv); err == nil && s != nil && s.TmuxSession != "" {
				newTmux = s.TmuxSession
				if s.ID != "" {
					label = s.ID
				}
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		// Don't 504 if we don't get the tmux name — the conv-id is
		// already known and the spawn was successful. Empty newTmux
		// in the response just means the human will need to look up
		// the label themselves.
	}

	// 3. Copy identity to the new conv. Crucially, this is ADD-only —
	// the original keeps every membership / permission / ownership it
	// had. Best-effort per row; partial failure is recoverable via
	// the CLI.
	granter := "system:clone"
	if caller != target {
		granter = "system:clone:by=" + caller
	}
	copied := []string{}
	for _, m := range oldMembers {
		alias := uniqueCloneAlias(m.Alias)
		newMember := &db.AgentGroupMember{
			GroupID: m.GroupID,
			ConvID:  newConv,
			Alias:   alias,
			Role:    m.Role,
			Descr:   m.Descr,
		}
		if err := db.AddAgentGroupMember(newMember); err != nil {
			slog.Warn("clone: add new member failed", "group", m.GroupID, "error", err)
			continue
		}
		copied = append(copied, fmt.Sprintf("group:%d", m.GroupID))
	}

	for _, slug := range oldPerms {
		if err := db.GrantAgentPermission(newConv, slug, granter); err != nil {
			slog.Warn("clone: grant new perm failed", "slug", slug, "error", err)
			continue
		}
		copied = append(copied, "perm:"+slug)
	}

	for _, gID := range oldOwnedIDs {
		if err := db.AddAgentGroupOwner(gID, newConv, granter); err != nil {
			slog.Warn("clone: add new owner failed", "group", gID, "error", err)
			continue
		}
		copied = append(copied, fmt.Sprintf("owner:%d", gID))
	}

	// 4. Optional follow-up. Same shape as reincarnate: enqueue an
	// agent_messages row when there's at least one shared group,
	// otherwise direct send-keys into the new pane. FromConv is the
	// caller (original for self-clone, manager for cross-clone), so
	// the new clone sees who asked it to pick up work.
	var msgID int64
	if followUp != "" {
		if len(oldMembers) > 0 {
			id, err := db.InsertAgentMessage(&db.AgentMessage{
				GroupID:  oldMembers[0].GroupID,
				FromConv: caller,
				ToConv:   newConv,
				Subject:  "clone handoff",
				Body:     followUp,
			})
			if err != nil {
				slog.Warn("clone: insert handoff message failed", "error", err)
			} else {
				msgID = id
				go deliverHandoffViaFlush(newConv)
			}
		} else {
			go injectFollowUpDirect(newConv, followUp)
		}
	}

	// NB: no /exit on the original — that's the whole difference vs
	// reincarnate.

	resp := map[string]any{
		"old_conv":     target,
		"new_conv":     newConv,
		"label":        label,
		"tmux_session": newTmux,
		"copied":       copied,
		"copy_conv":    !noCopyConv,
	}
	if caller != target {
		resp["caller_conv"] = caller
	}
	if newTmux != "" && label != "" {
		resp["attach_cmd"] = "tclaude session attach " + label
	} else {
		resp["attach_cmd"] = "tclaude session resume " + newConv
	}
	if followUp != "" {
		resp["follow_up"] = followUp
		if msgID > 0 {
			resp["message_id"] = msgID
			resp["note"] = fmt.Sprintf("clone %s spawned alongside original %s; follow-up queued as message #%d",
				short8(newConv), short8(target), msgID)
		} else {
			resp["note"] = fmt.Sprintf("clone %s spawned alongside original %s; follow-up will be injected into the new pane once it's ready",
				short8(newConv), short8(target))
		}
	} else {
		resp["note"] = fmt.Sprintf("clone %s spawned alongside original %s; both are now running",
			short8(newConv), short8(target))
	}
	writeJSON(w, http.StatusOK, resp)
}
