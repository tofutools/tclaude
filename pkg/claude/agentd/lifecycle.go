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

	"github.com/tofutools/tclaude/pkg/claude/agent"
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
	Title   string `json:"title,omitempty"`
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
		res.Title = agent.FreshTitle(m.ConvID)
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
		res.Title = agent.FreshTitle(m.ConvID)
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
//  4. Add the conv to the group with the supplied role/descr; the
//     `name` (when set) becomes the new agent's conversation title
//     via the post-spawn /rename injection.
//
// Permission: groups.spawn (default human-only — this lets an agent
// run arbitrary CC instances on the human's machine, blast radius
// matches `agent.spawn` in the design doc).
func handleGroupSpawn(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	// requirePermission also hands back the caller's conv-id: a real
	// agent (e.g. a PO orchestrating workers) resolves to its conv-id,
	// the human resolves to "". It is the default reply-to target for
	// the startup briefing assembled further down.
	spawnerConvID, ok := requirePermission(w, r, PermGroupsSpawn)
	if !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	var body struct {
		// Name, when set, becomes the new agent's conversation title:
		// runSpawnPostInit injects `/rename <name>` into the fresh pane.
		// An agent has exactly one name — its title — so there is no
		// separate per-group handle.
		Name string `json:"name,omitempty"`
		Role string `json:"role,omitempty"`
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
		// IncludeGroupContext controls whether the group's default_context
		// (when set) is injected into the new agent on startup. It's a
		// *bool so an omitted field means opt-in — every spawn path
		// inherits the group context by default, the same way it inherits
		// default_cwd. The dashboard sends false explicitly when the human
		// unticks the "include group default context" checkbox.
		IncludeGroupContext *bool `json:"include_group_context,omitempty"`

		// ReplyTo optionally names whom the spawned agent's `reply` to
		// its startup briefing should reach — any selector
		// agent.ResolveSelector accepts (conv-id / prefix / title).
		// Omitted: the briefing's sender defaults to the
		// spawn requester (spawnerConvID — empty for a human-initiated
		// spawn). Set it to hand a worker off to a coordinator other
		// than the spawner.
		ReplyTo string `json:"reply_to,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "json", err.Error())
			return
		}
	}

	// Spawn guardrails — runaway-prevention for an agent that the human
	// granted `groups.spawn`. Three checks: the group's hard member cap
	// (binds the human too), and — for agent callers only (spawnerConvID
	// != "") — the group restriction and the per-caller rate limit. Run
	// here, before any subprocess is launched, so a rejected spawn costs
	// nothing. See spawn_guardrails.go.
	if !checkSpawnGuardrails(w, g, spawnerConvID) {
		return
	}

	// The initial message is delivered to the new agent's inbox as an
	// agent_messages row — not typed into its tmux pane — so newlines
	// survive verbatim and a multi-line task brief arrives intact. We
	// only cap the length and reject NUL / escape / other non-text
	// control characters that would corrupt an `inbox read` render.
	body.InitialMessage = strings.TrimSpace(body.InitialMessage)
	if !isValidInitialMessage(body.InitialMessage) {
		writeError(w, http.StatusBadRequest, "invalid_initial_message",
			fmt.Sprintf("initial_message must be at most %d characters; newlines and tabs "+
				"are allowed (it is delivered to the agent's inbox, not typed into "+
				"its pane), but other control characters are not", agent.MaxInitialMessageBytes))
		return
	}

	// Resolve the startup briefing's sender. Default: the spawn
	// requester (an agent → its conv-id; a human → ""). An explicit
	// reply_to selector overrides it — the knob a coordinator uses to
	// route a worker's replies to a third agent rather than itself.
	replyToConv := spawnerConvID
	if rt := strings.TrimSpace(body.ReplyTo); rt != "" {
		res, _, rtErr := agent.ResolveSelector(rt)
		if rtErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_reply_to",
				fmt.Sprintf("reply_to %q: %v", rt, rtErr))
			return
		}
		replyToConv = res.ConvID
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

	// Spawn context: assemble the new agent's startup briefing and drop
	// it in its inbox as a single agent_messages row. Two pieces feed in:
	//
	//   - the group's default_context — optional shared guidance the
	//     human attached to the group, included unless this spawn opted
	//     out (IncludeGroupContext == false; an omitted flag means
	//     opt-in, the same way default_cwd is inherited).
	//   - the per-spawn initial_message — this agent's specific task
	//     brief.
	//
	// They go to the inbox rather than the pane: a large briefing
	// bracketed-pasted into CC's input box risks overflowing its
	// input-size limit, and the inbox keeps newlines verbatim
	// regardless. The welcome line points the agent at the message;
	// runSpawnPostInit marks it delivered once the welcome lands.
	groupContext := ""
	if body.IncludeGroupContext == nil || *body.IncludeGroupContext {
		groupContext = g.DefaultContext
	}
	spawnContext := buildSpawnContextBody(g.Name, groupContext, body.InitialMessage)
	var spawnContextMsgID int64
	if spawnContext != "" {
		mid, msgErr := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:      g.ID,
			FromConv:     replyToConv,
			ToConv:       convID,
			Subject:      "Startup context",
			Body:         spawnContext,
			ToRecipients: []string{convID},
		})
		if msgErr != nil {
			// Best-effort: the agent has already spawned and joined the
			// group. A failed insert just means no briefing — logged,
			// not bubbled — and the welcome falls back to "wait".
			slog.Warn("spawn: failed to deliver startup context to inbox",
				"conv", convID, "error", msgErr)
		} else {
			spawnContextMsgID = mid
		}
	}

	// Post-spawn injection: rename the new pane to the agent's name
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
	//      sees its title in tmux/dashboard immediately, the welcome
	//      describes its role/descr/group, and — when a startup
	//      briefing was queued above — points it at the inbox message.
	//
	// Runs in a goroutine so the spawn response returns promptly; the
	// goroutine waits for the pane to come alive before injecting.
	goBackground(func() {
		runSpawnPostInit(convID, body.Name, body.Role, body.Descr, g.Name,
			spawnContextMsgID, body.InitialMessage != "", worktreePath, worktreeBranch)
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
//  1. /rename <name> — when name is a valid rename title. This is the
//     agent's single name; it becomes the conversation title.
//  2. The welcome [system: ...] line orienting the agent.
//
// Each is its own turn. Failures are logged, never bubbled — the spawn
// already succeeded as far as the caller is concerned.
//
// The agent's startup briefing (group context + task brief) is NOT
// typed into the pane — the handler already placed it in the agent's
// inbox as agent_messages row #spawnContextMsgID, which keeps newlines
// verbatim and sidesteps CC's input-box size limit. The welcome line
// names that message id; once the welcome lands we mark the message
// delivered, since the welcome doubles as its inbox nudge.
//
// Why /rename first: it's a slash command CC processes immediately,
// landing a write on the .jsonl before any other turn happens. Even
// if a later injection fails, the file exists and `agent resume` can
// find it.
func runSpawnPostInit(convID, name, role, descr, groupName string, spawnContextMsgID int64, hasInitialMessage bool, worktreePath, worktreeBranch string) {
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

	// /rename first — see the doc comment. Skipped when name is empty
	// or not a valid rename title (some callers pass human-friendly
	// names that don't fit the rename charset); the welcome below
	// still materialises the .jsonl in that case.
	if name != "" && isValidRenameTitle(name) {
		if err := injectTextAndSubmit(target, "/rename "+name); err != nil {
			slog.Warn("spawn: /rename injection failed",
				"conv", convID, "name", name, "error", err)
		}
	} else if name != "" {
		slog.Warn("spawn: name not a valid rename title; skipping /rename",
			"conv", convID, "name", name)
	}

	// Welcome: a single-line [system: ...] turn orienting the agent
	// (identity, role, descr, group, where its startup briefing waits,
	// and — for a sub-repo worktree — where to make code edits).
	welcome := buildSpawnWelcome(name, role, descr, groupName,
		spawnContextMsgID, hasInitialMessage, worktreePath, worktreeBranch)
	if err := injectTextAndSubmit(target, welcome); err != nil {
		slog.Warn("spawn: welcome injection failed", "conv", convID, "error", err)
		return
	}

	// The startup briefing (group context + task brief) already sits in
	// the agent's inbox — the handler inserted the agent_messages row
	// before this goroutine fired. The welcome line above named its
	// message id, so the welcome itself is the inbox nudge: mark the
	// message delivered now that it has landed in the pane.
	if spawnContextMsgID > 0 {
		if err := db.MarkAgentMessageDelivered(spawnContextMsgID); err != nil {
			slog.Warn("spawn: failed to mark startup context delivered",
				"conv", convID, "msg_id", spawnContextMsgID, "error", err)
		}
	}
}

// buildSpawnContextBody assembles the startup briefing delivered to a
// freshly-spawned agent's inbox. It stitches together up to two
// sections — the group's shared startup context and the per-spawn
// task brief — under plain-text headers, with a divider when both are
// present.
//
// Either input may be empty (or whitespace-only); when both are, the
// result is "" and the caller skips the inbox insert entirely, so an
// agent with nothing to brief never gets an empty message.
func buildSpawnContextBody(groupName, groupContext, initialMessage string) string {
	groupContext = strings.TrimSpace(groupContext)
	initialMessage = strings.TrimSpace(initialMessage)

	var sections []string
	if groupContext != "" {
		sections = append(sections, fmt.Sprintf(
			"Group %q startup context — shared guidance for every agent spawned into this group:\n\n%s",
			groupName, groupContext))
	}
	if initialMessage != "" {
		sections = append(sections, "Your task brief:\n\n"+initialMessage)
	}
	return strings.Join(sections, "\n\n---\n\n")
}

// buildSpawnWelcome composes the [system: ...] welcome text. Brackets
// signal "this is metadata from tclaude, not a human prompt" — same
// convention agent-message nudges use. Kept to a single line so it
// renders cleanly in CC's prompt history.
//
// The trailing instruction has three forms, set by the spawn-context
// inbox message the handler may have queued:
//
//   - spawnContextMsgID == 0 — no briefing at all → "wait for the
//     first instruction".
//   - a briefing that includes a task brief (hasInitialMessage) →
//     point the agent at the inbox message and tell it to act.
//   - a briefing with only the group's shared startup context →
//     point at the inbox message, then tell it to wait.
func buildSpawnWelcome(name, role, descr, groupName string, spawnContextMsgID int64, hasInitialMessage bool, worktreePath, worktreeBranch string) string {
	parts := []string{"spawned by the human"}
	if name != "" {
		parts = append(parts, fmt.Sprintf("as %q", name))
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
	switch {
	case spawnContextMsgID <= 0:
		body += " Wait for the first instruction."
	case hasInitialMessage:
		body += fmt.Sprintf(" Your startup context and task brief are waiting in your inbox"+
			" as message #%d — read it with `tclaude agent inbox read %d`, then act on the brief.",
			spawnContextMsgID, spawnContextMsgID)
	default:
		body += fmt.Sprintf(" Your group's startup context is waiting in your inbox as"+
			" message #%d — read it with `tclaude agent inbox read %d`, then wait for the"+
			" first instruction.",
			spawnContextMsgID, spawnContextMsgID)
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
