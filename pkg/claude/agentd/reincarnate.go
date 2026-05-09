package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// `tclaude agent reincarnate` — replace the calling agent with a fresh
// CC instance that inherits its identity (groups, per-conv permission
// grants, group ownerships) and, optionally, picks up a follow-up
// prompt as its first turn.
//
// Why not just inject /clear? CC's /clear rotates the conv-id, which
// orphans every row in the agentd DB that's keyed on it: group
// memberships, granted permissions, ownerships. The agent comes back
// stripped of identity. Reincarnate does the orchestration to migrate
// that state onto the new conv-id atomically (best-effort transaction;
// see "what can go wrong" notes inline).
//
// Sequence:
//  1. Snapshot old conv state from SQLite + sessions table.
//  2. Spawn a fresh tclaude session in the same cwd; poll for new
//     conv-id (mirrors handleGroupSpawn).
//  3. Migrate memberships / permissions / ownerships old → new.
//  4. Optionally enqueue follow-up as an agent_messages row addressed
//     to the new conv. Background goroutine waits for the new pane to
//     come online and runs flush() to deliver via the existing nudge
//     pipeline. Solo agents (no group) get a direct send-keys
//     injection of the follow-up text instead.
//  5. Soft-stop the old pane via /exit.
//
// Identity is preserved; task state is *not* migrated — the agent is
// expected to persist work-in-progress to disk before calling, per
// the agent-lifecycle skill. Conversation title (set via /rename
// inside CC) is also not migrated; the new agent can self-rename in
// its follow-up.

// reincarnateSpawnTimeout caps how long we wait for the new tclaude
// session's conv-id to materialise. Mirrors handleGroupSpawn's
// default. If we hit this, the spawned session may still come up —
// the human can attach via the label we return.
const reincarnateSpawnTimeout = 30 * time.Second

// reincarnateAliveTimeout caps how long the post-spawn delivery
// goroutine waits for the new pane to be online before giving up on
// proactive delivery. The follow-up message stays in the inbox
// regardless; this is just about whether the nudge fires
// automatically.
const reincarnateAliveTimeout = 60 * time.Second

// reincarnateReadyDelay is how long we sleep after the new pane is
// "alive" before injecting any keys. CC's TUI takes a moment after
// startup before the input box is ready; without this, follow-up
// keystrokes can land mid-render.
const reincarnateReadyDelay = 1 * time.Second

// handleWhoamiReincarnate is the orchestration. POST-only,
// permission-gated on self.reincarnate (default-granted).
func handleWhoamiReincarnate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	oldConv, ok := requirePermission(w, r, PermSelfReincarnate)
	if !ok {
		return
	}
	if oldConv == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint operates on the calling agent's own conversation; humans should manage CC sessions directly")
		return
	}

	var body struct {
		FollowUp string `json:"follow_up"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
	}
	body.FollowUp = strings.TrimSpace(body.FollowUp)
	if body.FollowUp != "" && !isValidFollowUp(body.FollowUp) {
		writeError(w, http.StatusBadRequest, "invalid_follow_up",
			"REJECTED. Follow-up must be 1-4096 printable characters; tabs, newlines, "+
				"and other control characters are not allowed (each newline would be "+
				"treated as a submit by tmux send-keys, splitting the prompt).")
		return
	}

	// 1. Snapshot old conv state. We require an alive tmux session for
	// the caller — that's the cwd source and the target of the final
	// /exit injection. Should always be the case since the perm check
	// already established the caller is an agent.
	oldSess := pickAliveSession(oldConv)
	if oldSess == nil {
		writeError(w, http.StatusServiceUnavailable, "no_tmux",
			"caller has no live tmux session; can't reincarnate without a cwd to spawn into")
		return
	}
	cwd := oldSess.Cwd

	oldGroups, err := db.ListGroupsForConv(oldConv)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"snapshot groups: "+err.Error())
		return
	}
	oldMembers := make([]*db.AgentGroupMember, 0, len(oldGroups))
	for _, g := range oldGroups {
		m, err := db.FindMemberInGroup(g.ID, oldConv)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io",
				"snapshot membership: "+err.Error())
			return
		}
		if m != nil {
			oldMembers = append(oldMembers, m)
		}
	}

	oldPerms, err := db.ListAgentPermissionsForConv(oldConv)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"snapshot perms: "+err.Error())
		return
	}

	oldOwnedIDs, err := db.ListGroupsOwnedBy(oldConv)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"snapshot ownerships: "+err.Error())
		return
	}

	// 2. Spawn a fresh tclaude session in the same cwd.
	label := generateSpawnLabel()
	if err := spawnDetachedTclaudeNew(label, cwd); err != nil {
		writeError(w, http.StatusInternalServerError, "spawn",
			"failed to launch tclaude session new: "+err.Error())
		return
	}

	// 3. Poll the sessions table for the new conv-id (the hook
	// callback writes it once CC starts inside tmux).
	deadline := time.Now().Add(reincarnateSpawnTimeout)
	var newConv, newTmux string
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

	// 4. Migrate identity. Best-effort: errors on individual rows are
	// logged but don't abort. A partial migration is recoverable
	// (humans can use `tclaude agent permissions` / `groups add` to
	// fix), and full rollback would be more harmful than leaving the
	// new agent partially provisioned.
	migrated := []string{}
	const granter = "system:reincarnate"

	for _, m := range oldMembers {
		newMember := &db.AgentGroupMember{
			GroupID: m.GroupID,
			ConvID:  newConv,
			Alias:   m.Alias,
			Role:    m.Role,
			Descr:   m.Descr,
		}
		if err := db.AddAgentGroupMember(newMember); err != nil {
			slog.Warn("reincarnate: add new member failed", "group", m.GroupID, "error", err)
			continue
		}
		if err := db.RemoveAgentGroupMember(m.GroupID, oldConv); err != nil {
			slog.Warn("reincarnate: remove old member failed", "group", m.GroupID, "error", err)
		}
		migrated = append(migrated, fmt.Sprintf("group:%d", m.GroupID))
	}

	for _, slug := range oldPerms {
		if err := db.GrantAgentPermission(newConv, slug, granter); err != nil {
			slog.Warn("reincarnate: grant new perm failed", "slug", slug, "error", err)
			continue
		}
		if _, err := db.RevokeAgentPermission(oldConv, slug); err != nil {
			slog.Warn("reincarnate: revoke old perm failed", "slug", slug, "error", err)
		}
		migrated = append(migrated, "perm:"+slug)
	}

	for _, gID := range oldOwnedIDs {
		if err := db.AddAgentGroupOwner(gID, newConv, granter); err != nil {
			slog.Warn("reincarnate: add new owner failed", "group", gID, "error", err)
			continue
		}
		if _, err := db.RemoveAgentGroupOwner(gID, oldConv); err != nil {
			slog.Warn("reincarnate: remove old owner failed", "group", gID, "error", err)
		}
		migrated = append(migrated, fmt.Sprintf("owner:%d", gID))
	}

	// 5. Carry any tmux clients attached to the old session over to
	// the new session BEFORE we /exit the old pane. Without this, the
	// human's terminal gets detached when CC dies and they have to
	// manually `tclaude session attach <label>`. Best-effort — if
	// nobody was attached or the switch fails, the attach_cmd in the
	// response is the fallback.
	switchedClients := switchTmuxClients(oldSess.TmuxSession, newTmux)

	// 6. Deliver the follow-up. Two paths:
	//   - new conv has at least one group → enqueue agent_messages,
	//     deliver via the existing flush nudge pipeline once the
	//     pane is alive.
	//   - solo (no group) → direct tmux send-keys into the new pane.
	var msgID int64
	if body.FollowUp != "" {
		if len(oldMembers) > 0 {
			id, err := db.InsertAgentMessage(&db.AgentMessage{
				GroupID:  oldMembers[0].GroupID,
				FromConv: oldConv,
				ToConv:   newConv,
				Subject:  "reincarnation handoff",
				Body:     body.FollowUp,
			})
			if err != nil {
				slog.Warn("reincarnate: insert handoff message failed", "error", err)
			} else {
				msgID = id
				go deliverHandoffViaFlush(newConv)
			}
		} else {
			go injectFollowUpDirect(newConv, body.FollowUp)
		}
	}

	// 7. Soft-stop the old conv. Best-effort — if the old pane is
	// already gone (somehow), we still consider the reincarnation
	// successful.
	_ = injectSlashCommand(oldConv, "/exit", "")

	resp := map[string]any{
		"old_conv":         oldConv,
		"new_conv":         newConv,
		"label":            label,
		"tmux_session":     newTmux,
		"attach_cmd":       "tclaude session attach " + label,
		"migrated":         migrated,
		"switched_clients": switchedClients,
	}
	carry := ""
	switch switchedClients {
	case 0:
		carry = "no tmux client was attached, so the human will need to run attach_cmd manually"
	case 1:
		carry = "human's tmux client carried over to the new session"
	default:
		carry = fmt.Sprintf("%d tmux clients carried over to the new session", switchedClients)
	}
	if body.FollowUp != "" {
		resp["follow_up"] = body.FollowUp
		if msgID > 0 {
			resp["message_id"] = msgID
			resp["note"] = fmt.Sprintf("old %s soft-stopped via /exit; %s; follow-up queued as message #%d for %s",
				short8(oldConv), carry, msgID, short8(newConv))
		} else {
			resp["note"] = fmt.Sprintf("old %s soft-stopped via /exit; %s; follow-up will be injected into %s once its pane is ready",
				short8(oldConv), carry, short8(newConv))
		}
	} else {
		resp["note"] = fmt.Sprintf("old %s soft-stopped via /exit; %s; new %s is up at %s",
			short8(oldConv), carry, short8(newConv), newTmux)
	}
	writeJSON(w, http.StatusOK, resp)
}

// deliverHandoffViaFlush waits for the new pane to come online, then
// runs flush() so any pending agent_messages addressed to it are
// delivered through the normal nudge pipeline. The flush helper
// claims delivery atomically, so if a future request from the new
// conv triggers maybeFlushUndelivered we don't double-deliver.
func deliverHandoffViaFlush(newConv string) {
	deadline := time.Now().Add(reincarnateAliveTimeout)
	for time.Now().Before(deadline) {
		if isConvOnline(newConv) {
			time.Sleep(reincarnateReadyDelay)
			flush(newConv, realFlushSender)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	slog.Warn("reincarnate: new conv never came online; handoff message left in inbox for next agent request",
		"conv", newConv)
}

// injectFollowUpDirect is the no-group fallback. Waits for the new
// pane to be alive, then types the follow-up directly via send-keys
// — splitting text from the submit Enter to defeat CC's TUI paste-
// mode coalescing (where embedded Enters become newlines instead of
// submits).
func injectFollowUpDirect(newConv, followUp string) {
	deadline := time.Now().Add(reincarnateAliveTimeout)
	var sess *db.SessionRow
	for time.Now().Before(deadline) {
		s := pickAliveSession(newConv)
		if s != nil {
			sess = s
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if sess == nil {
		slog.Warn("reincarnate: solo follow-up injection — new conv never came online", "conv", newConv)
		return
	}
	time.Sleep(reincarnateReadyDelay)
	target := sess.TmuxSession + ":0.0"
	if err := clcommon.TmuxCommand("send-keys", "-t", target, followUp).Run(); err != nil {
		slog.Warn("reincarnate: solo follow-up text send failed", "error", err, "tmux", sess.TmuxSession)
		return
	}
	// Small gap so the text and the submit Enter arrive in separate
	// reads on CC's side; otherwise the trailing Enter risks being
	// treated as a paste-newline.
	time.Sleep(200 * time.Millisecond)
	if err := clcommon.TmuxCommand("send-keys", "-t", target, "Enter").Run(); err != nil {
		slog.Warn("reincarnate: solo follow-up submit failed", "error", err, "tmux", sess.TmuxSession)
		return
	}
	time.Sleep(200 * time.Millisecond)
	// Belt-and-suspenders second Enter — same pattern injectSlashCommand
	// uses; harmless if the first one already submitted.
	_ = clcommon.TmuxCommand("send-keys", "-t", target, "Enter").Run()
}

// switchTmuxClients moves tmux clients currently attached to oldTmux
// over to newTmux via `tmux switch-client -c <tty> -t <new>`. Returns
// the number of clients successfully switched. Best-effort: per-client
// failures are logged and skipped, since a stale client is harmless
// and the human can always fall back to the attach_cmd in the response.
//
// Run this BEFORE injecting /exit on the old pane — once /exit kills
// CC, the pane closes and any attached client is detached, defeating
// the carry-over.
func switchTmuxClients(oldTmux, newTmux string) int {
	out, err := clcommon.TmuxCommand("list-clients", "-t", oldTmux, "-F", "#{client_tty}").Output()
	if err != nil {
		slog.Warn("reincarnate: list-clients failed; skipping client switch", "tmux", oldTmux, "error", err)
		return 0
	}
	n := 0
	for _, tty := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if tty == "" {
			continue
		}
		if err := clcommon.TmuxCommand("switch-client", "-c", tty, "-t", newTmux).Run(); err != nil {
			slog.Warn("reincarnate: switch-client failed", "tty", tty, "from", oldTmux, "to", newTmux, "error", err)
			continue
		}
		n++
	}
	return n
}

// short8 formats a conv-id for human output. Same shape as the
// `short` helper on the agent side; duplicated here so the daemon
// doesn't depend on the agent CLI package.
func short8(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}
