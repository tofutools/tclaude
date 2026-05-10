package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// cloneSpawnError carries enough context to surface either an HTTP
// error (when called from the single-clone handler) or accumulate
// into a per-member result (when called from groups-clone). The two
// callers differ in how they report failure but agree on which
// statuses + codes apply.
type cloneSpawnError struct {
	Status int
	Code   string
	Msg    string
}

func (e *cloneSpawnError) Error() string { return e.Msg }
func (e *cloneSpawnError) write(w http.ResponseWriter) {
	writeError(w, e.Status, e.Code, e.Msg)
}

// cloneSpawnOnce mints a clone's conv-id (and optionally its jsonl).
// Two branches:
//   - copy: use convops to fork the existing jsonl onto a fresh
//     conv-id; spawn `tclaude session new -r <new-conv>` so CC loads
//     the cloned conversation.
//   - no-copy: spawn `tclaude session new --label <label>` and poll
//     for whatever conv-id CC mints, same as reincarnate.
//
// Returns (newConv, newTmux, label, nil) on success. label may be
// empty in the copy path when the session row's id field hasn't
// materialised within the deadline; that's not an error since the
// conv-id is already known.
//
// Extracted from runCloneOrchestration so groups-clone can reuse the
// same race handling without duplicating it. Behaviour-preserving;
// see commit history for the original inline version.
func cloneSpawnOnce(sourceConv, cwd string, noCopyConv bool) (newConv, newTmux, label string, spawnErr *cloneSpawnError) {
	if noCopyConv {
		label = generateSpawnLabel()
		if err := SpawnDetachedTclaudeNew(label, cwd); err != nil {
			return "", "", "", &cloneSpawnError{
				Status: http.StatusInternalServerError, Code: "spawn",
				Msg: "failed to launch tclaude session new: " + err.Error(),
			}
		}
		deadline := time.Now().Add(reincarnateSpawnTimeout)
		for time.Now().Before(deadline) {
			s, err := db.LoadSession(label)
			if err == nil && s != nil {
				newTmux = s.TmuxSession
				if s.ConvID != "" {
					return s.ConvID, newTmux, label, nil
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		return "", newTmux, label, &cloneSpawnError{
			Status: http.StatusGatewayTimeout, Code: "timeout",
			Msg: "spawned session " + label + " but conv-id never materialised within " +
				reincarnateSpawnTimeout.String() +
				" — the session may still come up; check `tclaude session attach " + label + "`",
		}
	}
	// Copy path: fork the jsonl first, then resume into it.
	copyResult, err := convops.CopyConversationToPath(sourceConv, cwd, true /* global */)
	if err != nil {
		return "", "", "", &cloneSpawnError{
			Status: http.StatusInternalServerError, Code: "copy",
			Msg: "failed to copy conversation jsonl: " + err.Error(),
		}
	}
	newConv = copyResult.NewConvID
	if err := SpawnDetachedTclaudeResume(newConv, cwd); err != nil {
		return "", "", "", &cloneSpawnError{
			Status: http.StatusInternalServerError, Code: "spawn",
			Msg: "failed to launch tclaude session new -r: " + err.Error(),
		}
	}
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
	// Don't fail if the tmux name doesn't surface — the conv-id is
	// already known and the spawn was successful. Empty newTmux just
	// means the human needs to look up the label themselves.
	return newConv, newTmux, label, nil
}

// CloneCooldown is the minimum time between two clones of the same
// source conv. The clone handler does an atomic INSERT-WHERE-NOT-
// EXISTS against agent_clone_history to enforce this — see
// db.ClaimCloneSlot. Default 1 minute; flow tests shrink it via
// t.Cleanup-restored assignment to drive the locked/unlocked branches
// without sleeping.
//
// Per source conv, not per caller: the runaway scenario the TODO
// flagged is "the same conv being cloned in a tight loop", regardless
// of which agent triggered it. A manager that wants to fan out clones
// of *different* sources hits the limit only if it tries to fan out
// the *same* source twice within cooldown.
var CloneCooldown = time.Minute

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
// N is monotonically larger than the previous clone's N: we start
// the search at `prevN + 1`, then advance to the smallest free slot
// from that floor. Without the floor, a previously-used N whose
// agent_group_members row has since disappeared (member removed,
// group deleted, etc.) gets recycled — chronologically confusing
// when the new clone descends from a higher-numbered ancestor. The
// "used" set still scans every group globally so parallel clones
// don't collide; legacy `-clone-N` aliases don't reserve a number
// in the new namespace.
//
// Lookup error → fall back to `prevN + 1` (or 1 when prevN is 0).
func uniqueCloneAlias(origAlias string) string {
	base := origAlias
	prevN := 0
	if m := cloneSuffixRegex.FindStringSubmatch(base); m != nil {
		base = m[1]
		// Re-extract N from the final dash-separated token; the regex
		// only captures the base. Same shape as the reincarnate
		// counterpart for symmetry.
		if i := strings.LastIndex(origAlias, "-"); i >= 0 {
			if n, err := strconv.Atoi(origAlias[i+1:]); err == nil {
				prevN = n
			}
		}
	}
	prefix := "c-"
	if base != "" {
		prefix = base + "-c-"
	}
	used := scanCloneSuffixesGlobal(prefix)
	start := prevN + 1
	if start < 1 {
		start = 1
	}
	for n := start; ; n++ {
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

	// Rate limit: refuse a second clone of the same source within
	// CloneCooldown. Clone is the only default-granted, agent-reachable
	// fork-doubling verb (self.clone is granted by default; reincarnate
	// is 1-in-1-out, spawn is human-only) — without this gate, an agent
	// stuck in a tight loop could fork itself unboundedly. Atomic at
	// the DB layer so two concurrent claim attempts can't both pass.
	if err := db.ClaimCloneSlot(target, CloneCooldown, time.Now().UTC()); err != nil {
		if errors.Is(err, db.ErrCloneRateLimited) {
			writeError(w, http.StatusTooManyRequests, "rate_limited",
				"clone of "+short8(target)+" too recent; cooldown is "+CloneCooldown.String()+
					" between consecutive clones of the same source conv")
			return
		}
		writeError(w, http.StatusInternalServerError, "io",
			"clone rate-limit check: "+err.Error())
		return
	}

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

	// 2. Mint the clone's conv-id (and optionally its jsonl). The
	// branching logic + race-handling lives in cloneSpawnOnce so the
	// groups-clone orchestration can reuse the same code path without
	// duplicating it.
	newConv, newTmux, label, spawnErr := cloneSpawnOnce(target, cwd, noCopyConv)
	if spawnErr != nil {
		spawnErr.write(w)
		return
	}

	// 3. Copy identity to the new conv. Crucially, this is ADD-only —
	// the original keeps every membership / permission / ownership it
	// had. Best-effort per row; partial failure is recoverable via
	// the CLI.
	granter := "system:clone"
	if caller != target {
		granter = "system:clone:by=" + caller
	}
	// Resolve the original's display title once so per-group alias
	// derivations can fall back to it when the original member row
	// has no alias set. Without this fallback, a clone of a conv
	// that's been added to a group without an alias gets generic
	// `c-1` instead of the parent's name. Best-effort — empty string
	// just means uniqueCloneAlias falls back to its existing
	// "no-base" behaviour (`c-N`).
	originalTitle := ""
	if row := agent.FreshConvRowResolved(target); row != nil {
		originalTitle = agent.DisplayTitle(row)
	}
	copied := []string{}
	for _, m := range oldMembers {
		base := m.Alias
		if base == "" {
			base = originalTitle
		}
		alias := uniqueCloneAlias(base)
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

	// 4. Pick the rename target. Use the first computed clone alias —
	// for the common single-group case this is the only one anyway,
	// and for multi-group clones tmux/dashboard only displays one
	// title at a time. Skipped when no membership rows landed (which
	// is rare and points at a deeper failure that already logged
	// above). Without this, a freshly-spawned clone has no startup
	// write and ends up as an orphan if no one ever messages it —
	// same trap that bit `tclaude agent spawn` before bc7ec81.
	cloneAlias := ""
	for _, c := range copied {
		if !strings.HasPrefix(c, "group:") {
			continue
		}
		var gid int64
		_, _ = fmt.Sscanf(c, "group:%d", &gid)
		if gid == 0 {
			continue
		}
		if m, err := db.FindMemberInGroup(gid, newConv); err == nil && m != nil && m.Alias != "" {
			cloneAlias = m.Alias
			break
		}
	}
	go runClonePostInit(newConv, cloneAlias, target, caller)

	// 5. Optional follow-up. Same shape as reincarnate: enqueue an
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

// runClonePostInit fires asynchronously after a successful clone. It
// waits for the new pane to come online and injects /rename to the
// computed clone alias, materialising the .jsonl with a meaningful
// title. Same purpose as runSpawnPostInit, just for the clone path —
// the original used to silently leave clones unrenamed (so they
// showed up as "(unknown)" with whatever conv-id-derived label tmux
// picked) and unrecoverable when never used.
//
// Skips when alias is empty or fails the rename charset gate.
// Failures log; never bubble — the clone already succeeded as far as
// the caller is concerned.
func runClonePostInit(newConv, alias, target, caller string) {
	if !waitForConvAlive(newConv) {
		slog.Warn("clone: new conv never came online; rename abandoned", "conv", newConv)
		return
	}
	if alias == "" || !isValidRenameTitle(alias) {
		if alias != "" {
			slog.Warn("clone: alias not a valid rename title; skipping /rename",
				"conv", newConv, "alias", alias)
		}
		return
	}
	// Note: no welcome message here. Reincarnate's flow already injects
	// a handoff via the agent_messages flush path when followUp is set,
	// and same for clone — the orchestration above wrote a clone-handoff
	// row that the flush path will deliver. Spawn doesn't go through
	// that path so it gets a synthetic welcome from runSpawnPostInit;
	// clone doesn't need one. The /rename alone is enough to materialise
	// the .jsonl.
	if !injectSlashCommand(newConv, "/rename "+alias, "") {
		slog.Warn("clone: /rename injection failed", "conv", newConv, "alias", alias)
	}
}
