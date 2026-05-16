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

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// reincarnateSuffixRegex matches a trailing reincarnation suffix in
// either the current short form `-r-<digits>` or the legacy long
// form `-reincarnate-<digits>`. Recognising both lets a legacy
// `worker-reincarnate-3` cleanly transition to `worker-r-1` (rather
// than nesting as `worker-reincarnate-3-r-1`) the next time it
// reincarnates. Same idea for cloneSuffixRegex.
var reincarnateSuffixRegex = regexp.MustCompile(`^(.*?)-(?:r|reincarnate)-\d+$`)

// uniqueReincarnateTitle picks the new instance's CC title in the
// pattern `<base>-r-<N>` (or `r-<N>` when the previous instance had
// no title). base is prevTitle with any existing `-r-<digits>` /
// `-reincarnate-<digits>` stripped. The short `-r-` is paired with
// `-c-` for clones — distinct enough at a glance, short enough to
// tile in tmux pane headers.
//
// N is monotonically larger than the previous instance's N: we start
// the search at `prevN + 1`, then advance to the smallest free slot
// from that floor. Without the floor, a previously-used N whose
// conv_index row has since disappeared (pruned, retitled, file
// deleted) gets recycled — so a chain like r-1 → r-2 → r-3 could
// surprise-reset back to r-1 on the next reincarnation. Anchoring on
// prevN keeps the lineage chronologically readable. The "used" set
// only scans the new short prefix; legacy `-reincarnate-N` titles
// don't reserve a number in the new namespace.
//
// Lookup error → fall back to `prevN + 1` (or 1 when prevN is 0).
func uniqueReincarnateTitle(prevTitle string) string {
	base := prevTitle
	prevN := 0
	if m := reincarnateSuffixRegex.FindStringSubmatch(base); m != nil {
		base = m[1]
		// Re-extract N from the original suffix; the capture group only
		// pins the base, so we have to re-parse to recover the digits.
		// Splitting on "-" is safe: the regex anchors `-r-\d+$` (or the
		// legacy `-reincarnate-\d+$`), so the trailing token is always
		// the integer.
		if i := strings.LastIndex(prevTitle, "-"); i >= 0 {
			if n, err := strconv.Atoi(prevTitle[i+1:]); err == nil {
				prevN = n
			}
		}
	}
	prefix := "r-"
	if base != "" {
		prefix = base + "-r-"
	}
	used := scanReincarnateSuffixes(prefix)
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

// scanReincarnateSuffixes walks every conv_index row and returns the
// set of integers N where some custom_title equals `<prefix><N>`.
// Used by uniqueReincarnateTitle to pick the smallest free N.
func scanReincarnateSuffixes(prefix string) map[int]bool {
	used := map[int]bool{}
	rows, err := db.ListAllConvIndex()
	if err != nil {
		return used
	}
	for _, r := range rows {
		if !strings.HasPrefix(r.CustomTitle, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(r.CustomTitle, prefix)
		n, err := strconv.Atoi(suffix)
		if err != nil {
			continue
		}
		used[n] = true
	}
	return used
}

// waitForConvAlive polls for newConv's tmux pane to come online,
// then sleeps reincarnateReadyDelay so CC's TUI is ready to accept
// keystrokes. Returns true if the pane became alive within
// reincarnateAliveTimeout, false otherwise.
func waitForConvAlive(newConv string) bool {
	deadline := time.Now().Add(reincarnateAliveTimeout)
	for time.Now().Before(deadline) {
		if isConvOnline(newConv) {
			time.Sleep(reincarnateReadyDelay)
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// `tclaude agent reincarnate` — replace the calling agent with a fresh
// CC instance that inherits its identity (groups, per-conv permission
// grants, group ownerships) and picks up a follow-up prompt (REQUIRED)
// as its first turn. The follow-up is required because the new pane
// comes up with a clean context window and would otherwise sit idle;
// when the caller has no concrete next directive, the convention is to
// pass a short summary of the previous "life" (what was happening,
// where the relevant files are) so the successor has something to
// start from.
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
// the agent-lifecycle skill. Conversation title is auto-renamed to
// `<prev>-reincarnate-<N>` (smallest free N globally across
// conv_index.custom_title); the rename is injected BEFORE the
// follow-up so the new pane shows the proper title from the start.

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
//
// Declared as `var` (not `const`) so flow tests can shrink it via
// SetWaitTimingsForTest — otherwise the post-init drain in newFlow's
// cleanup can sit on the full 60s when a test scenario never brings
// the conv online.
var reincarnateAliveTimeout = 60 * time.Second

// reincarnateReadyDelay is how long we sleep after the new pane is
// "alive" before injecting any keys. CC's TUI takes a moment after
// startup before the input box is ready; without this, follow-up
// keystrokes can land mid-render.
//
// Same `var` rationale as reincarnateAliveTimeout above.
var reincarnateReadyDelay = 1 * time.Second

// handleWhoamiReincarnate handles POST /v1/whoami/reincarnate (self
// path). Gated on self.reincarnate (default-granted). Delegates to
// runReincarnationOrchestration with target == caller.
func handleWhoamiReincarnate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requirePermission(w, r, PermSelfReincarnate)
	if !ok {
		return
	}
	if caller == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint operates on the calling agent's own conversation; humans should manage CC sessions directly, or use POST /v1/agent/{conv}/reincarnate to reincarnate another agent")
		return
	}
	followUp, ok := decodeReincarnateFollowUp(w, r)
	if !ok {
		return
	}
	runReincarnationOrchestration(w, caller, caller, PermSelfReincarnate, followUp)
}

// handleAgentReincarnate handles POST /v1/agent/{conv}/reincarnate
// (cross-agent path). Gated on agent.reincarnate OR group-owner-of-target.
// Routed via handleAgentByConv, which has already resolved targetConv.
func handleAgentReincarnate(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, targetConv)
	if !ok {
		return
	}
	followUp, ok := decodeReincarnateFollowUp(w, r)
	if !ok {
		return
	}
	runReincarnationOrchestration(w, targetConv, caller, PermAgentReincarnate, followUp)
}

// decodeReincarnateFollowUp parses + validates the REQUIRED follow_up
// body field. Returns (followUp, true) on success; on failure the error
// response is already written and the caller should return. An empty
// or missing follow_up is rejected: the new pane comes up with a clean
// context window and would otherwise sit idle. Callers with no
// concrete next directive should pass a short summary of the previous
// "life" (what was being worked on, where the relevant files are) so
// the successor has something to start from.
func decodeReincarnateFollowUp(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		FollowUp string `json:"follow_up"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return "", false
		}
	}
	body.FollowUp = strings.TrimSpace(body.FollowUp)
	if body.FollowUp == "" {
		writeError(w, http.StatusBadRequest, "missing_follow_up",
			"follow_up is required. The new agent comes up with a clean context "+
				"window and would otherwise sit idle. If you have no concrete next "+
				"directive, summarise your previous 'life' (what you were doing, "+
				"where the relevant files are, what's next) so the successor has "+
				"something to start from.")
		return "", false
	}
	// Charset/length: validate against the LENIENT inbox rule here. A
	// grouped successor receives the handoff as an inbox message, so it
	// tolerates the same ≤16384-byte, newline-friendly charset as a
	// spawn --initial-message. The stricter solo-pane rule is enforced
	// later by soloFollowUpRejection, once the membership snapshot in
	// runReincarnationOrchestration reveals which delivery path applies.
	if !isValidInitialMessage(body.FollowUp) {
		writeError(w, http.StatusBadRequest, "invalid_follow_up",
			fmt.Sprintf("REJECTED. follow_up must be at most %d characters; newlines "+
				"and tabs are allowed (a grouped successor receives the handoff in "+
				"its inbox, like a spawn brief), but NUL / escape / other control "+
				"characters are not.", agent.MaxInitialMessageBytes))
		return "", false
	}
	return body.FollowUp, true
}

// runReincarnationOrchestration is the target-agnostic body shared by
// the self and cross-agent endpoints.
//
//   - target is the conv being reincarnated (its identity migrates onto
//     the new conv-id, its tmux pane is /exit-ed at the end).
//   - caller is the conv that triggered the reincarnation (recorded in
//     the audit trail as `system:reincarnate:by=<caller>` for cross-agent,
//     plain `system:reincarnate` when caller == target). It's also the
//     handoff message's FromConv so the new agent sees who asked it to
//     pick up.
//   - followUp is an optional first-turn prompt; empty means "just
//     reincarnate, no handoff message".
//
//   - perm is the slug requirePermission gated this call on
//     (PermSelfReincarnate / PermAgentReincarnate). Used by
//     auditedCaller to annotate via-sudo grants in the audit trail.
//
// Writes the JSON response (or error) directly to w.
func runReincarnationOrchestration(w http.ResponseWriter, target, caller, perm, followUp string) {
	// 1. Snapshot target conv state. We require an alive tmux session
	// for the target — that's the cwd source and the target of the
	// final /exit injection.
	oldSess := pickAliveSession(target)
	if oldSess == nil {
		writeError(w, http.StatusServiceUnavailable, "no_tmux",
			"target conv "+short8(target)+" has no live tmux session; can't reincarnate without a cwd to spawn into (try `tclaude agent groups resume` first if it's offline)")
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

	// A solo (groupless) successor has no inbox — its handoff is typed
	// into the new pane via send-keys. decodeReincarnateFollowUp only
	// applied the lenient inbox charset; now that the membership
	// snapshot is in, reject a follow-up the strict pane rule can't
	// carry. Done before the spawn so a bad request wastes no session.
	if reason := soloFollowUpRejection(followUp, len(oldMembers) > 0); reason != "" {
		writeError(w, http.StatusBadRequest, "invalid_follow_up", reason)
		return
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

	// 2. Spawn a fresh tclaude session in the same cwd.
	label := generateSpawnLabel()
	if err := SpawnDetachedTclaudeNew(label, cwd); err != nil {
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
	granter := "system:reincarnate"
	if caller != target {
		granter = "system:reincarnate:by=" + auditedCaller(caller, perm)
	} else if grantID, _ := db.LookupActiveSudoGrantID(caller, perm); grantID > 0 {
		granter = fmt.Sprintf("system:reincarnate:via-sudo:grant-id=%d", grantID)
	}

	for _, m := range oldMembers {
		newMember := &db.AgentGroupMember{
			GroupID: m.GroupID,
			ConvID:  newConv,
			Role:    m.Role,
			Descr:   m.Descr,
		}
		if err := db.AddAgentGroupMember(newMember); err != nil {
			slog.Warn("reincarnate: add new member failed", "group", m.GroupID, "error", err)
			continue
		}
		if err := db.RemoveAgentGroupMember(m.GroupID, target); err != nil {
			slog.Warn("reincarnate: remove old member failed", "group", m.GroupID, "error", err)
		}
		migrated = append(migrated, fmt.Sprintf("group:%d", m.GroupID))
	}

	for _, slug := range oldPerms {
		if err := db.GrantAgentPermission(newConv, slug, granter); err != nil {
			slog.Warn("reincarnate: grant new perm failed", "slug", slug, "error", err)
			continue
		}
		if _, err := db.RevokeAgentPermission(target, slug); err != nil {
			slog.Warn("reincarnate: revoke old perm failed", "slug", slug, "error", err)
		}
		migrated = append(migrated, "perm:"+slug)
	}

	for _, gID := range oldOwnedIDs {
		if err := db.AddAgentGroupOwner(gID, newConv, granter); err != nil {
			slog.Warn("reincarnate: add new owner failed", "group", gID, "error", err)
			continue
		}
		if _, err := db.RemoveAgentGroupOwner(gID, target); err != nil {
			slog.Warn("reincarnate: remove old owner failed", "group", gID, "error", err)
		}
		migrated = append(migrated, fmt.Sprintf("owner:%d", gID))
	}

	// Eagerly rewrite cron job refs from old → new. Without this, jobs
	// owned by or targeted at the reincarnated conv keep firing against
	// a dead conv-id (the old pane is /exit'd below). Best-effort: the
	// succession-chain lookup is the safety net for any reference we
	// might miss here.
	if n, err := db.MigrateCronJobConvRef(target, newConv); err != nil {
		slog.Warn("reincarnate: migrate cron job refs failed",
			"old", target, "new", newConv, "error", err)
	} else if n > 0 {
		migrated = append(migrated, fmt.Sprintf("cron:%d", n))
	}

	// Record the succession edge so historical references (CLI
	// selectors, log spelunking, things we forgot to eagerly migrate)
	// can be walked forward via db.ResolveLatestConv.
	if err := db.RecordConvSuccession(target, newConv, "reincarnate"); err != nil {
		slog.Warn("reincarnate: record conv succession failed",
			"old", target, "new", newConv, "error", err)
	}

	// 5. Carry any tmux clients attached to the old session over to
	// the new session BEFORE we /exit the old pane. Without this, the
	// human's terminal gets detached when CC dies and they have to
	// manually `tclaude session attach <label>`. Best-effort — if
	// nobody was attached or the switch fails, the attach_cmd in the
	// response is the fallback.
	switchedClients := switchTmuxClients(oldSess.TmuxSession, newTmux)

	// 6. Compute the new instance's CC title — `<prev>-reincarnate-<N>`,
	// global N across all conv_index rows. Done here (before /exit on
	// the old pane below) so the lookup of prevTitle still resolves
	// cleanly. FreshConvRowAt scans the parent's .jsonl when conv_index
	// has no row for it — required for back-to-back reincarnations
	// where the parent itself was just spawned and never indexed yet
	// (otherwise prevTitle would be "" and we'd produce `reincarnate-1`
	// instead of `<parent>-reincarnate-N`).
	prevTitle := ""
	if row := agent.FreshConvRowAt(target, oldSess.Cwd); row != nil {
		prevTitle = agent.DisplayTitle(row)
	}
	newTitle := uniqueReincarnateTitle(prevTitle)

	// 7. Queue the follow-up message BEFORE the post-spawn goroutine
	// runs — gets the row written so the rename can land first and
	// the message delivery picks it up next. Solo path has no row to
	// queue; the goroutine below sends the text directly after the
	// rename. (decodeReincarnateFollowUp guarantees followUp != "".)
	var msgID int64
	if len(oldMembers) > 0 {
		id, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:  oldMembers[0].GroupID,
			FromConv: caller,
			ToConv:   newConv,
			Subject:  "reincarnation handoff",
			Body:     followUp,
		})
		if err != nil {
			slog.Warn("reincarnate: insert handoff message failed", "error", err)
		} else {
			msgID = id
		}
	}

	// 8. Post-spawn injection: wait for alive → /rename → follow-up.
	// Single goroutine so ordering is deterministic — without this,
	// rename and follow-up race and the user briefly sees the wrong
	// title in the new pane.
	goBackground(func() {
		runReincarnatePostSpawn(newConv, newTitle, followUp, len(oldMembers) > 0)
	})

	// 9. Mark the old conv as archived (soft-deleted), then soft-stop.
	//
	// Two writes happen here, in this order:
	//
	//   a. Stamp `conv_index.archived_at = now` on the old conv
	//      (canonical signal — survives renames, tool-poking, etc.).
	//      Listing surfaces filter on this column primarily.
	//   b. Inject `/rename <prevTitle>-x` into the old pane, writing
	//      a custom-title record to the .jsonl before /exit closes
	//      the pane. Cosmetic UX cue so the dead conv shows up as
	//      `<prev>-x` in tmux pane titles + tools that read .jsonl
	//      directly. The watch model / FreshConvRow refresh picks
	//      it up on mtime.
	//
	// Either write failing is non-fatal — the other still gives
	// listing surfaces a way to detect the archived state. Idempotent:
	// the rename skips when prevTitle is empty or already ends in
	// `-x`; the column stamp is a single UPDATE.
	if err := db.SetConvIndexArchived(target, true); err != nil {
		slog.Warn("reincarnate: stamp archived_at failed",
			"old", target, "error", err)
	}
	// The old conv is superseded: its identity has migrated onto the
	// successor (enrolled by RecordConvSuccession above). Drop its
	// enrollment row so it doesn't linger on the agent roster as an
	// offline ghost — the successor IS this agent now. The succession
	// chain still resolves stale references forward.
	if _, err := db.DeleteEnrollment(target); err != nil {
		slog.Warn("reincarnate: delete old enrollment failed",
			"old", target, "error", err)
	}
	if prevTitle != "" && !strings.HasSuffix(prevTitle, "-x") {
		_ = injectSlashCommand(target, "/rename "+prevTitle+"-x", "")
	}
	_ = injectSlashCommand(target, "/exit", "")

	resp := map[string]any{
		"old_conv":         target,
		"new_conv":         newConv,
		"new_title":        newTitle,
		"label":            label,
		"tmux_session":     newTmux,
		"attach_cmd":       "tclaude session attach " + label,
		"migrated":         migrated,
		"switched_clients": switchedClients,
	}
	if caller != target {
		resp["caller_conv"] = caller
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
	resp["follow_up"] = followUp
	if msgID > 0 {
		resp["message_id"] = msgID
		resp["note"] = fmt.Sprintf("old %s soft-stopped via /exit; %s; new pane will be /renamed to %q then receive message #%d",
			short8(target), carry, newTitle, msgID)
	} else {
		resp["note"] = fmt.Sprintf("old %s soft-stopped via /exit; %s; new pane will be /renamed to %q then receive the follow-up directly",
			short8(target), carry, newTitle)
	}
	writeJSON(w, http.StatusOK, resp)
}

// runReincarnatePostSpawn is the single goroutine that handles
// post-spawn injection in deterministic order: wait-for-alive →
// /rename → follow-up. Renaming first means the new pane's CC
// title shows the proper `<prev>-reincarnate-<N>` immediately,
// before any work output starts streaming.
//
// followUp is guaranteed non-empty (decodeReincarnateFollowUp
// rejects empty bodies). Skips rename when newTitle == "" (defensive
// — should not happen in practice since uniqueReincarnateTitle
// always returns a non-empty string).
func runReincarnatePostSpawn(newConv, newTitle, followUp string, hasGroup bool) {
	if !waitForConvAlive(newConv) {
		slog.Warn("reincarnate: new conv never came online; rename + handoff abandoned", "conv", newConv)
		return
	}
	if newTitle != "" {
		if !injectSlashCommand(newConv, "/rename "+newTitle, "") {
			slog.Warn("reincarnate: rename injection failed", "conv", newConv, "title", newTitle)
		}
		// Gap so CC has time to process the rename slash command
		// before we type the follow-up. Without this the follow-up
		// keystrokes can land in the rename's still-open prompt.
		time.Sleep(reincarnateReadyDelay)
	}
	if hasGroup {
		// agent_messages row was already inserted before this
		// goroutine fired; flush picks it up via the normal nudge
		// pipeline.
		flush(newConv, realFlushSender)
		return
	}
	// Solo (no group) follow-up: direct send-keys, mirroring
	// injectFollowUpDirect's tmux pattern. The pane is already
	// alive (waitForConvAlive returned true) so no second wait.
	sess := pickAliveSession(newConv)
	if sess == nil {
		slog.Warn("reincarnate: solo follow-up — pane went away between rename and send", "conv", newConv)
		return
	}
	target := sess.TmuxSession + ":0.0"
	if err := clcommon.TmuxCommand("send-keys", "-t", target, followUp).Run(); err != nil {
		slog.Warn("reincarnate: solo follow-up text send failed", "error", err, "tmux", sess.TmuxSession)
		return
	}
	// 500ms gap — paste-mode coalescing safety; see injectTextAndSubmit.
	time.Sleep(500 * time.Millisecond)
	if err := clcommon.TmuxCommand("send-keys", "-t", target, "Enter").Run(); err != nil {
		slog.Warn("reincarnate: solo follow-up submit failed", "error", err, "tmux", sess.TmuxSession)
		return
	}
	time.Sleep(500 * time.Millisecond)
	_ = clcommon.TmuxCommand("send-keys", "-t", target, "Enter").Run()
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
	// 500ms gap so the text and the submit Enter arrive in separate
	// reads on CC's side; otherwise the trailing Enter risks being
	// treated as a paste-newline. Same as injectTextAndSubmit.
	time.Sleep(500 * time.Millisecond)
	if err := clcommon.TmuxCommand("send-keys", "-t", target, "Enter").Run(); err != nil {
		slog.Warn("reincarnate: solo follow-up submit failed", "error", err, "tmux", sess.TmuxSession)
		return
	}
	time.Sleep(500 * time.Millisecond)
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
