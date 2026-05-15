package agentd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/conv"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// memberOpResult is the per-member outcome of a bulk lifecycle op
// (stop / resume). The CLI prints these as a summary table so the
// human can see which members succeeded, which were no-ops, and
// which failed.
type memberOpResult struct {
	ConvID  string `json:"conv_id"`
	Alias   string `json:"alias,omitempty"`
	Action  string `json:"action"`           // "soft_stopped", "killed", "resumed", "skipped:already_online", "skipped:no_conv_id", "error"
	Detail  string `json:"detail,omitempty"` // human-readable note (e.g. error message)
	TmuxSes string `json:"tmux_session,omitempty"`
}

type groupOpResp struct {
	Group   string           `json:"group"`
	Action  string           `json:"action"`
	Members []memberOpResult `json:"members"`
}

// handleGroupStop ends every member's running tmux session.
//
// Modes:
//   - soft (default): inject `/exit` via tmux send-keys, mirroring the
//     /rename pattern. Lets CC clean up its own state. The actual tmux
//     session usually goes away on CC's next iteration.
//   - force (?force=1): tmux kill-session -t <name>. Last resort —
//     drops any unsubmitted input the agent hadn't sent yet.
//
// Members that aren't currently online are reported as
// `skipped:already_offline` and skipped — stop is idempotent.
func handleGroupStop(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if _, ok := requirePermission(w, r, PermGroupsStop); !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	out := groupOpResp{Group: g.Name, Action: "stop", Members: []memberOpResult{}}
	for _, m := range members {
		res := stopOneConv(m.ConvID, force)
		res.Alias = m.Alias
		out.Members = append(out.Members, res)
	}
	writeJSON(w, http.StatusOK, out)
}

// stopOneConv soft-stops (or force-kills with `force=true`) the live
// tmux session for convID. Returns the per-conv result. Shared between
// the bulk groups.stop loop and the single-conv agent.stop endpoint.
//
// Result shape mirrors the existing memberOpResult so the bulk
// summary table renders the same regardless of how the call was
// initiated. Idempotent: convs already offline come back as
// `skipped:already_offline`.
func stopOneConv(convID string, force bool) memberOpResult {
	res := memberOpResult{ConvID: convID}
	sess := pickAliveSession(convID)
	if sess == nil {
		res.Action = "skipped:already_offline"
		return res
	}
	res.TmuxSes = sess.TmuxSession
	if force {
		if err := clcommon.TmuxCommand("kill-session", "-t", sess.TmuxSession).Run(); err != nil {
			res.Action = "error"
			res.Detail = "kill-session: " + err.Error()
		} else {
			res.Action = "killed"
		}
		return res
	}
	// Soft stop: inject `/exit`. Same two-step send-keys the /rename
	// injector uses (see injectSlashCommand). CC closes the conversation
	// cleanly; tmux session goes away when CC exits.
	if injectSlashCommand(convID, "/exit", "") {
		res.Action = "soft_stopped"
	} else {
		res.Action = "error"
		res.Detail = "send-keys /exit failed"
	}
	return res
}

// handleGroupResume starts a tclaude session for every member that
// has a known conv-id but no live tmux session. Spawns the
// subprocess detached (`tclaude session new -r <conv> -d --global`)
// so each member gets a fresh tmux pane attached to its existing conv.
//
// Members already online are reported as `skipped:already_online`
// — resume is idempotent. The "ensure my team is up" reconciliation
// the TODO design described.
func handleGroupResume(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if _, ok := requirePermission(w, r, PermGroupsResume); !ok {
		return
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	out := groupOpResp{Group: g.Name, Action: "resume", Members: []memberOpResult{}}
	for _, m := range members {
		res := resumeOneConv(m.ConvID)
		res.Alias = m.Alias
		out.Members = append(out.Members, res)
	}
	writeJSON(w, http.StatusOK, out)
}

// resumeOneConv spawns a detached `tclaude session new -r <conv>`
// for convID if it isn't already online. Returns the per-conv
// result. Shared between the bulk groups.resume loop and the
// single-conv agent.resume endpoint.
//
// Idempotent: convs already online come back as
// `skipped:already_online`. Empty conv-ids (placeholder members
// with no conv yet) come back as `skipped:no_conv_id` since we
// have no .jsonl to resume from — those are template-based
// spawns, deferred to a future "groups create --team" pass.
func resumeOneConv(convID string) memberOpResult {
	res := memberOpResult{ConvID: convID}
	if isConvOnline(convID) {
		res.Action = "skipped:already_online"
		return res
	}
	if convID == "" {
		res.Action = "skipped:no_conv_id"
		res.Detail = "placeholder member (no conv yet) — Phase B will support template-based fresh spawn"
		return res
	}
	// Look up the recorded cwd so resume lands the agent in the
	// directory they were last running in. Falls back to "" which
	// makes `tclaude session new` use its own default.
	cwd := ""
	if rows, _ := db.FindSessionsByConvID(convID); len(rows) > 0 {
		cwd = rows[0].Cwd
	}
	if err := SpawnDetachedTclaudeResume(convID, cwd); err != nil {
		res.Action = "error"
		res.Detail = "spawn: " + err.Error()
	} else {
		res.Action = "resumed"
	}
	return res
}

// handleAgentStop stops a single conv's tmux session. Sibling of
// the bulk groups.stop. Auth: agent.stop slug OR caller is owner of
// a group containing target. Routed via /v1/agent/{selector}/stop;
// `?force=1` switches to tmux kill-session.
func handleAgentStop(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentStop, targetConv)
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	res := stopOneConv(targetConv, force)
	resp := map[string]any{
		"conv_id":      res.ConvID,
		"action":       res.Action,
		"tmux_session": res.TmuxSes,
	}
	if res.Detail != "" {
		resp["detail"] = res.Detail
	}
	if caller != "" && caller != targetConv {
		resp["caller_conv"] = caller
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAgentDelete permanently removes an agent: every row in every
// agent / conv / session table that references the conv-id, plus the
// .jsonl file and the ~/.claude/session-env/<conv-id> token. Sibling
// of stop / resume but DESTRUCTIVE — there is no undo. Auth:
// agent.delete slug OR caller is owner of a group containing target.
// Default-grant policy explicitly excludes agent.delete (humans
// only, unless someone explicitly grants).
//
// Refuses when the target's tmux session is alive — the human must
// stop it first via `tclaude agent stop`. `?force=1` kills the tmux
// session inline before deleting (mirrors the stop endpoint's force
// switch). Refusing-by-default avoids racing the live agent's writes
// to its own .jsonl while we're tearing it down.
//
// Returns the per-table deletion counts so the human can see scope.
func handleAgentDelete(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method", "DELETE only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentDelete, targetConv)
	if !ok {
		return
	}
	// Self-delete prevention. An agent shouldn't be able to wipe its
	// own conv mid-turn — the daemon's own request context is keyed
	// off the caller's conv-id, and the cleanup goroutine would race
	// the response write. Humans (caller == "") can always proceed.
	if caller != "" && caller == targetConv {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"cannot delete self via this endpoint; use `tclaude conv rm` from a human shell or have a peer/owner do it")
		return
	}
	force := r.URL.Query().Get("force") == "1"
	stopRes := stopOneConv(targetConv, force)
	if stopRes.Action == "error" {
		writeError(w, http.StatusInternalServerError, "stop", stopRes.Detail)
		return
	}
	// If the conv is alive but force wasn't passed, stopOneConv
	// returned `soft_stopped` (sent /exit) — the tmux pane may still
	// be in the process of dying. Refuse without ?force=1 to avoid
	// racing the live agent's writes during teardown.
	if !force && stopRes.Action == "soft_stopped" {
		writeError(w, http.StatusConflict, "alive",
			"target had a live tmux session; sent /exit. Re-run with ?force=1 to delete now, or wait for the pane to exit and retry.")
		return
	}

	// Comprehensive cleanup: DB purge + filesystem + sync tombstone +
	// session-env. Single source of truth shared with the dashboard
	// `DELETE /api/agents/...` path and `tclaude conv rm`.
	counts, err := conv.DeleteConvByID(targetConv)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"delete failed: "+err.Error())
		return
	}

	resp := map[string]any{
		"conv_id":   targetConv,
		"action":    "deleted",
		"db_counts": counts,
	}
	if caller != "" && caller != targetConv {
		resp["caller_conv"] = caller
	}
	if stopRes.Action != "skipped:already_offline" {
		resp["pre_stop"] = stopRes.Action
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAgentResume resumes a single conv into a fresh detached
// tmux session. Sibling of the bulk groups.resume. Auth:
// agent.resume slug OR caller is owner of a group containing
// target. Routed via /v1/agent/{selector}/resume.
func handleAgentResume(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentResume, targetConv)
	if !ok {
		return
	}
	res := resumeOneConv(targetConv)
	resp := map[string]any{
		"conv_id": res.ConvID,
		"action":  res.Action,
	}
	if res.Detail != "" {
		resp["detail"] = res.Detail
	}
	if caller != "" && caller != targetConv {
		resp["caller_conv"] = caller
	}
	writeJSON(w, http.StatusOK, resp)
}

// pickAliveSession returns the most-recent session row for convID
// whose tmux session is still alive. Same selector as nudgeIfAlive.
func pickAliveSession(convID string) *db.SessionRow {
	candidates, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return nil
	}
	for _, c := range candidates {
		if c.TmuxSession != "" && session.IsTmuxSessionAlive(c.TmuxSession) {
			return c
		}
	}
	return nil
}

// handleGroupSpawn starts a fresh CC session and registers it in
// the group as soon as its conv-id materialises.
//
// Flow:
//  1. Pick a unique label (used as the tclaude session ID + tmux
//     session name).
//  2. Fork-exec `tclaude session new -d --global --label <label>`
//     fully detached. The wrapper exits in milliseconds; the actual
//     CC process is parented to the long-running tmux server, so
//     CC's process-ownership checks see no Claude ancestor in the
//     daemon's chain.
//  3. Poll the sessions table for that label until conv-id appears
//     (CC's first hook callback writes it). 30s default timeout.
//  4. Add the conv to the group with the supplied alias/role/descr.
//
// Permission: groups.spawn (default human-only — this lets an agent
// run arbitrary CC instances on the human's machine, blast radius
// matches `agent.spawn` in the design doc).
func handleGroupSpawn(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if _, ok := requirePermission(w, r, PermGroupsSpawn); !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	var body struct {
		Alias string `json:"alias,omitempty"`
		Role  string `json:"role,omitempty"`
		// Descr is the short, one-line description shown on the dashboard
		// (the group-member "Description" column). Keep it terse — the
		// agent's actual task brief goes in InitialMessage instead.
		Descr string `json:"descr,omitempty"`
		// InitialMessage, when set, is delivered to the new agent as its
		// first real prompt — a separate turn after the welcome. It is
		// deliberately split from Descr so a long task brief doesn't bloat
		// the dashboard's description column.
		InitialMessage string `json:"initial_message,omitempty"`
		Cwd            string `json:"cwd,omitempty"`
		TimeoutSeconds int    `json:"timeout_seconds,omitempty"`

		// WorktreePath / WorktreeBranch describe a git worktree the
		// agent should do its code work in, when Cwd is a parent
		// "monorepo" directory rather than the repo itself. They are
		// purely informational — the agent still launches in Cwd; the
		// worktree path rides into the welcome message so the agent
		// knows where to make edits. Set by the dashboard's spawn
		// modal after it creates the worktree; empty for an ordinary
		// spawn where Cwd already is the repo.
		WorktreePath   string `json:"worktree_path,omitempty"`
		WorktreeBranch string `json:"worktree_branch,omitempty"`

		// AutoFocus, when set, opens a terminal window attached to the
		// new agent once the spawn lands. Opt-in on the wire — the
		// dashboard's spawn modal defaults its checkbox on, CLI / agent
		// callers pass it explicitly.
		AutoFocus bool `json:"auto_focus,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "json", err.Error())
			return
		}
	}

	// The initial message is injected via tmux send-keys, so it must
	// survive the same way a slash follow-up does: no control characters
	// (each newline would land as a premature submit, fragmenting the
	// prompt). The dashboard collapses newlines client-side; this is the
	// backstop for CLI / API callers.
	body.InitialMessage = strings.TrimSpace(body.InitialMessage)
	if body.InitialMessage != "" && !isValidFollowUp(body.InitialMessage) {
		writeError(w, http.StatusBadRequest, "invalid_initial_message",
			"initial_message must be 1-4096 printable characters; tabs, newlines, "+
				"and other control characters are not allowed (each newline would be "+
				"treated as a submit by tmux send-keys, splitting the prompt)")
		return
	}

	timeout := 30 * time.Second
	if body.TimeoutSeconds > 0 {
		timeout = time.Duration(body.TimeoutSeconds) * time.Second
		if timeout > 5*time.Minute {
			timeout = 5 * time.Minute
		}
	}

	// When the request leaves cwd blank, fall back to the group's
	// default_cwd (the "group default start dir" set via the
	// dashboard or `groups set-default-dir`). This makes the default
	// reach every spawn path — CLI, API, dashboard — not just the
	// dashboard's client-side prefill. An empty default_cwd leaves
	// cwd blank, so resolveSpawnCwd keeps its prior behaviour of
	// inheriting the daemon's own cwd.
	if body.Cwd == "" {
		body.Cwd = g.DefaultCwd
	}

	// Validate the requested cwd before doing any work. Expands "~",
	// makes the path absolute, and confirms it exists as a directory.
	// Catching a bad cwd here turns what used to be a silent 30s
	// conv-id-poll timeout into an immediate, actionable error.
	cwd, cwdErr := resolveSpawnCwd(body.Cwd)
	if cwdErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_cwd", cwdErr.Error())
		return
	}

	// Validate the optional worktree dir the same way — it must exist
	// (the dashboard creates it just before spawning). Caught here so
	// a stale path becomes an immediate 400 rather than a welcome
	// message pointing the agent at a directory that isn't there.
	var worktreePath string
	if strings.TrimSpace(body.WorktreePath) != "" {
		wt, wtErr := resolveSpawnCwd(body.WorktreePath)
		if wtErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_worktree", wtErr.Error())
			return
		}
		worktreePath = wt
	}
	worktreeBranch := strings.TrimSpace(body.WorktreeBranch)

	// Generate a label that's unlikely to collide with existing
	// session IDs. Tclaude's GenerateSessionID() uses an 8-char
	// random hex; we mirror that with a "spwn-" prefix so these
	// rows are easy to spot in `tclaude session ls`.
	label := generateSpawnLabel()

	if err := SpawnDetachedTclaudeNew(label, cwd); err != nil {
		writeError(w, http.StatusInternalServerError, "spawn",
			"failed to launch tclaude session new: "+err.Error())
		return
	}

	// Poll the sessions table for the conv-id. The hook callback
	// writes it shortly after CC actually starts inside tmux.
	deadline := time.Now().Add(timeout)
	var convID, tmuxSession string
	for time.Now().Before(deadline) {
		s, err := db.LoadSession(label)
		if err == nil && s != nil {
			tmuxSession = s.TmuxSession
			if s.ConvID != "" {
				convID = s.ConvID
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if convID == "" {
		writeError(w, http.StatusGatewayTimeout, "timeout",
			"spawned session "+label+" but conv-id never materialised within "+timeout.String()+
				" — the session may still come up; check `tclaude session attach "+label+"`")
		return
	}

	// Membership add. Permission gating already happened above; this
	// is just the DB write.
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g.ID,
		ConvID:  convID,
		Alias:   body.Alias,
		Role:    body.Role,
		Descr:   body.Descr,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"spawned conv "+convID+" but failed to add to group: "+err.Error())
		return
	}

	// Auto-focus: when the caller asked for it, open a terminal window
	// attached to the freshly-spawned agent. A detached spawn has no
	// window of its own, so this is what lets the human watch and talk
	// to the new agent right away — via `tclaude session attach`, never
	// raw tmux, so the reattached session keeps its tclaude features.
	//
	// Best-effort: the agent spawned fine regardless, so a failure to
	// pop a window is logged, never bubbled. No extra permission gate —
	// opening a window is strictly less than the groups.spawn this
	// handler already required.
	if body.AutoFocus {
		if err := openTerminal(openAttachCmd(label)); err != nil {
			slog.Warn("spawn: auto-focus terminal failed to open",
				"conv", convID, "label", label, "error", err)
		}
	}

	// Post-spawn injection: rename the new pane to the agent's alias
	// and drop a [system: ...] welcome that describes the agent's
	// identity. Two purposes:
	//
	//   1. Materialise the .jsonl. CC only writes the conversation
	//      file once it has content; a freshly-spawned but never-used
	//      conv leaves an orphan group_members row with no .jsonl,
	//      and `agent resume` then silently fails because
	//      `tclaude session new -r <conv>` has nothing to resume.
	//      The /rename + welcome guarantee at least two writes hit
	//      the file before anyone closes the pane.
	//
	//   2. Give the new agent its name + context up front. The agent
	//      sees its alias in tmux/dashboard immediately, and the
	//      welcome describes its role/descr/group so it can orient
	//      itself before the human starts typing.
	//
	// Runs in a goroutine so the spawn response returns promptly; the
	// goroutine waits for the pane to come alive before injecting.
	goBackground(func() {
		runSpawnPostInit(convID, body.Alias, body.Role, body.Descr, g.Name,
			body.InitialMessage, worktreePath, worktreeBranch)
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"group":        g.Name,
		"conv_id":      convID,
		"label":        label,
		"tmux_session": tmuxSession,
		"attach_cmd":   "tclaude session attach " + label,
	})
}

// runSpawnPostInit fires asynchronously after a successful spawn. It
// waits for the new tmux pane to come online, then injects, in order:
//
//  1. /rename <alias> — when alias is a valid rename title.
//  2. The welcome [system: ...] line orienting the agent.
//  3. The initial message — when one was supplied — as the agent's
//     first real prompt.
//
// Each is its own turn. Failures are logged, never bubbled — the spawn
// already succeeded as far as the caller is concerned.
//
// Why /rename first: it's a slash command CC processes immediately,
// landing a write on the .jsonl before any other turn happens. Even
// if a later injection fails, the file exists and `agent resume` can
// find it.
func runSpawnPostInit(convID, alias, role, descr, groupName, initialMessage, worktreePath, worktreeBranch string) {
	if !waitForConvAlive(convID) {
		slog.Warn("spawn: new conv never came online; post-init injection abandoned",
			"conv", convID)
		return
	}

	sess := pickAliveSession(convID)
	if sess == nil {
		slog.Warn("spawn: no alive tmux session for post-init injection", "conv", convID)
		return
	}
	target := sess.TmuxSession + ":0.0"

	// /rename first — see the doc comment. Skipped when alias is empty
	// or not a valid rename title (some callers pass human-friendly
	// aliases that don't fit the rename charset); the welcome below
	// still materialises the .jsonl in that case.
	if alias != "" && isValidRenameTitle(alias) {
		if err := injectTextAndSubmit(target, "/rename "+alias); err != nil {
			slog.Warn("spawn: /rename injection failed",
				"conv", convID, "alias", alias, "error", err)
		}
	} else if alias != "" {
		slog.Warn("spawn: alias not a valid rename title; skipping /rename",
			"conv", convID, "alias", alias)
	}

	// Welcome: a single-line [system: ...] turn orienting the agent
	// (identity, role, descr, group, and — for a sub-repo worktree —
	// where to make code edits).
	welcome := buildSpawnWelcome(alias, role, descr, groupName, initialMessage != "",
		worktreePath, worktreeBranch)
	if err := injectTextAndSubmit(target, welcome); err != nil {
		slog.Warn("spawn: welcome injection failed", "conv", convID, "error", err)
		return
	}

	// Initial message: the human's first real prompt for the agent.
	// Kept separate from descr — descr is the short dashboard label,
	// this is the (possibly long) task brief — and sent as its own
	// turn after the welcome so the agent reads orientation first,
	// task second.
	if initialMessage != "" {
		if err := injectTextAndSubmit(target, initialMessage); err != nil {
			slog.Warn("spawn: initial-message injection failed",
				"conv", convID, "error", err)
		}
	}
}

// buildSpawnWelcome composes the [system: ...] welcome text. Brackets
// signal "this is metadata from tclaude, not a human prompt" — same
// convention agent-message nudges use. Kept to a single line so it
// renders cleanly in CC's prompt history.
//
// hasInitialMessage flips the trailing instruction: with an initial
// message queued the agent should act on it, not sit idle, so the
// "wait for the first instruction" line is replaced.
func buildSpawnWelcome(alias, role, descr, groupName string, hasInitialMessage bool, worktreePath, worktreeBranch string) string {
	parts := []string{"spawned by the human"}
	if alias != "" {
		parts = append(parts, fmt.Sprintf("as %q", alias))
	}
	if role != "" {
		parts = append(parts, fmt.Sprintf("(role: %s)", role))
	}
	if groupName != "" {
		parts = append(parts, fmt.Sprintf("in group %q", groupName))
	}
	header := strings.Join(parts, " ")
	body := header + "."
	if descr != "" {
		body += " Descr: " + descr + "."
	}
	// When the spawn targeted a sub-repo of a monorepo launch dir, the
	// agent's cwd is the parent dir but its code work belongs in the
	// worktree. Spell that out so it doesn't edit the parent's repos.
	if worktreePath != "" {
		body += " Your git worktree for code changes is at " + worktreePath
		if worktreeBranch != "" {
			body += " (branch " + worktreeBranch + ")"
		}
		body += " — make code edits there, not elsewhere under your start directory."
	}
	body += " Use `tclaude agent` commands (whoami / help / inbox ls) to introspect and coordinate."
	if hasInitialMessage {
		body += " Your first instructions follow in the next message."
	} else {
		body += " Wait for the first instruction."
	}
	return "[system: " + body + "]"
}

// generateSpawnLabel produces a "spwn-XXXXXX" identifier. 6 hex
// chars from crypto/rand gives ~16M values — collisions in the
// session table are vanishingly rare in practice.
func generateSpawnLabel() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return "spwn-" + hex.EncodeToString(b[:])
}

// SpawnDetachedTclaudeNew is a thin facade over Spawn.SpawnNew.
// Tests substitute a behavior-accurate fake by assigning Spawn at
// setup; production keeps the LiveSpawner default.
func SpawnDetachedTclaudeNew(label, cwd string) error {
	return Spawn.SpawnNew(label, cwd)
}

// SpawnDetachedTclaudeResume is a thin facade over Spawn.SpawnResume.
func SpawnDetachedTclaudeResume(convID, cwd string) error {
	return Spawn.SpawnResume(convID, cwd)
}

// liveSpawnNew runs `tclaude session new -d --global --label <label>`
// as a fully-detached subprocess. Same detachment story as
// liveSpawnResume — see its doc comment for the full rationale on
// why this doesn't trip CC's process-ownership checks.
//
// The label is the tclaude-side session ID (used to look up the row
// in SQLite once the conv-id materialises). It must be unique in the
// sessions table.
func liveSpawnNew(label, cwd string) error {
	args := []string{"session", "new", "-d", "--global", "--label", label}
	if cwd != "" {
		args = append(args, "-C", cwd)
	}
	cmd := exec.Command("tclaude", args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	detachSpawn(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	go func() {
		if err := cmd.Wait(); err != nil {
			slog.Debug("spawn subprocess exited",
				"label", label, "pid", pid, "err", err)
		}
	}()
	return nil
}

// liveSpawnResume runs `tclaude session new -r <conv> -d --global`
// as a fully-detached subprocess.
//
// Detachment story:
//   - `tclaude session new -d` only means "don't attach this terminal
//     to the new tmux session." The wrapper still runs in foreground
//     and inherits whatever stdio its parent gave it.
//   - We explicitly null stdio so nothing leaks back into the
//     daemon's logs.
//   - detachSpawn (unix-only) sets Setsid so the wrapper has its own
//     session and process group — no controlling tty inherited from
//     the daemon, and on daemon exit the wrapper gets reparented to
//     init/PID 1 cleanly.
//   - The actual CC process is parented to the long-running tmux
//     server (because `tclaude session new -d` shells out to
//     `tmux new-session -d ...` which forks the command as a child of
//     the tmux server, not of the caller). So the CC process never
//     has *us* in its ancestry chain — important so it doesn't trip
//     CC's own process-ownership / sandbox checks via parent walks.
//
// Errors only surface if exec.Start() itself fails (binary missing
// from PATH, etc.).
func liveSpawnResume(convID, cwd string) error {
	args := []string{"session", "new", "-r", convID, "-d", "--global"}
	if cwd != "" {
		args = append(args, "-C", cwd)
	}
	cmd := exec.Command("tclaude", args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	detachSpawn(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	// Reap the wrapper when it finishes so we don't leak zombies. The
	// wrapper exits quickly (after `tmux new-session -d` returns); the
	// real CC process keeps running under the tmux server.
	go func() {
		if err := cmd.Wait(); err != nil {
			slog.Debug("resume subprocess exited",
				"conv", convID, "pid", pid, "err", err)
		}
	}()
	return nil
}

