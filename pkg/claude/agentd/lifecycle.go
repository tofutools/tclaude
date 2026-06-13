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
	"github.com/tofutools/tclaude/pkg/claude/harness"
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
	if _, ok := requireGroupPermission(w, r, PermGroupsStop, g); !ok {
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
	// Soft stop: inject the harness's exit command (CC's `/exit`). The
	// harness closes the conversation cleanly and the tmux session goes
	// away when it exits. The command is sourced from the harness's
	// Lifecycle so a non-CC pane is never typed `/exit` if that's not its
	// exit command.
	h := harnessForConv(convID)
	if h.SupportsSoftExit() {
		exitCmd := h.Life.SoftExitCommand()
		if injectSlashCommand(convID, exitCmd, "") {
			res.Action = "soft_stopped"
		} else {
			res.Action = "error"
			res.Detail = "send-keys " + exitCmd + " failed"
		}
		return res
	}
	// No soft-exit command for this harness → hard kill so the pane never
	// lingers because we couldn't type a graceful exit.
	if err := clcommon.TmuxCommand("kill-session", "-t", sess.TmuxSession).Run(); err != nil {
		res.Action = "error"
		res.Detail = "kill-session (harness has no soft-exit): " + err.Error()
	} else {
		res.Action = "killed_no_soft_exit"
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
	if _, ok := requireGroupPermission(w, r, PermGroupsResume, g); !ok {
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
	// directory they were last running in — falls back to "" which
	// makes `tclaude session new` use its own default — and the
	// model + effort it last reported running on, so the resumed
	// agent comes back on its own model instead of claude's default
	// (rows are updated_at DESC; [0] is the conv's freshest session).
	cwd, effort, model, harnessName := "", "", "", ""
	if rows, _ := db.FindSessionsByConvID(convID); len(rows) > 0 {
		cwd = rows[0].Cwd
		effort, model = inheritedLaunchFlags(rows[0].ID)
		// Resume under the harness the conv was last running on — a Codex
		// conv must relaunch as `tclaude session new -r --harness codex` so
		// session-new resolves its rollout id (resolveResumeConv, JOH-155)
		// instead of looking in ~/.claude/projects. An untagged/claude row
		// leaves it "" so the flag is omitted.
		harnessName = rows[0].Harness
	}
	if err := SpawnDetachedTclaudeResume(convID, cwd, effort, model, harnessName); err != nil {
		res.Action = "error"
		res.Detail = "spawn: " + err.Error()
	} else {
		res.Action = "resumed"
	}
	return res
}

// groupRetireResp is the response shape of the bulk groups.retire
// endpoint. It mirrors groupOpResp (so the CLI renders the per-member
// table identically to stop/resume) but carries an extra Warnings list
// — retire can leave a group ownerless when it demotes an owner, and
// the human needs to hear about that.
type groupRetireResp struct {
	Group    string           `json:"group"`
	Action   string           `json:"action"`
	Members  []memberOpResult `json:"members"`
	Warnings []string         `json:"warnings,omitempty"`
}

// handleGroupRetire retires every OTHER active-agent member of the
// group in one shot — the bulk parallel of `agent retire`, completing
// the groups.stop / groups.resume lifecycle family (which until now had
// no retire sibling).
//
// "Retire" demotes an agent to a plain conversation: retireAgentConv
// drops every group membership (this group and any others the member
// belongs to), revokes every permission and sudo grant, and flips the
// enrollment bit. The conversation itself — .jsonl, history, conv_index
// row — is left completely intact and reinstatable; this is the
// non-destructive bulk cleanup, never `agent delete`. Unless
// ?shutdown=0, a retired member's running tmux pane is also soft-exited
// (stopOneConv, soft only — never a force-kill), since a retired
// agent's idle process is almost never wanted.
//
// Per-member outcomes (memberOpResult.Action):
//   - retired                  — demoted (Detail summarises what changed)
//   - skipped:self             — the caller's own conv; never self-retire
//   - skipped:no_conv_id       — a placeholder member with no conv yet
//   - skipped:not_active_agent — already retired / never an agent
//   - error                    — the retire failed (Detail has the cause)
//
// The caller's own conv is always skipped: the brief is "retire OTHER
// agents in the group", and an agent demoting itself mid-request would
// revoke its own grants and /exit its own pane out from under the very
// request it is serving. A human caller (caller == "") has no conv to
// skip and retires every member.
//
// Permission: groups.retire (default human-only — retiring agents is a
// sensitive cleanup the human normally drives; the slug delegates it to
// a trusted coordinator). Gated with the same plain requirePermission
// the other bulk group endpoints (stop/resume/spawn) use — the
// group-owner structural bypass is a single-agent-endpoint affordance,
// not a bulk one.
func handleGroupRetire(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	caller, ok := requireGroupPermission(w, r, PermGroupsRetire, g)
	if !ok {
		return
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	shutdown := retireShouldShutdown(r)
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	by := enrollmentActor(caller)

	out := groupRetireResp{Group: g.Name, Action: "retire", Members: []memberOpResult{}}
	// Groups whose owner roster a retire touched — checked once at the
	// end so a bulk retire that demotes a member-owner warns about the
	// now-ownerless group, matching the single-agent cleanup path.
	ownerless := map[int64]bool{}
	for _, m := range members {
		res := memberOpResult{ConvID: m.ConvID, Title: agent.FreshTitle(m.ConvID)}
		switch {
		case m.ConvID == "":
			res.Action = "skipped:no_conv_id"
			res.Detail = "placeholder member (no conv yet)"
		case caller != "" && m.ConvID == caller:
			res.Action = "skipped:self"
			res.Detail = "the caller never retires itself"
		default:
			res = retireGroupMember(m.ConvID, by, reason, shutdown, res, ownerless)
		}
		out.Members = append(out.Members, res)
	}
	out.Warnings = warnOwnerlessGroups(ownerless)
	writeJSON(w, http.StatusOK, out)
}

// retireGroupMember retires one member as part of the bulk groups.retire
// loop. It enforces the "active agent only" guard (a no-op on a conv
// that was never an agent or is already retired comes back as
// skipped:not_active_agent), runs the shared retireAgentConv demotion,
// records any group whose owner roster it touched into the ownerless
// set, and — when shutdown is requested — soft-exits the member's pane.
// Returns the populated result; res arrives pre-seeded with ConvID +
// Title so the caller's table stays consistent across every branch.
func retireGroupMember(convID, by, reason string, shutdown bool, res memberOpResult, ownerless map[int64]bool) memberOpResult {
	state, serr := db.EnrollmentState(convID)
	if serr != nil {
		res.Action = "error"
		res.Detail = "enrollment lookup: " + serr.Error()
		return res
	}
	if state != db.EnrollmentActive {
		res.Action = "skipped:not_active_agent"
		res.Detail = "enrollment: " + state
		return res
	}
	outcome, ownerGroups, rerr := retireAgentConv(convID, by, reason)
	if rerr != nil {
		res.Action = "error"
		res.Detail = rerr.Error()
		return res
	}
	for _, gid := range ownerGroups {
		ownerless[gid] = true
	}
	res.Action = "retired"
	res.Detail = summarizeRetireOutcome(outcome)
	if shutdown {
		sd := stopOneConv(convID, false /* soft exit */)
		res.TmuxSes = sd.TmuxSes
		if sd.Action == "soft_stopped" {
			res.Detail = joinDetail(res.Detail, "/exit sent")
		}
	}
	return res
}

// summarizeRetireOutcome renders the parts of a retireConvOutcome the
// bulk table cares about into a compact, human-readable Detail cell:
// how many groups the member left and how many grants were revoked. An
// outcome that changed nothing beyond the enrollment bit yields "".
func summarizeRetireOutcome(o retireConvOutcome) string {
	var parts []string
	if n := len(o.GroupsLeft); n > 0 {
		parts = append(parts, fmt.Sprintf("left %d group(s)", n))
	}
	if revoked := o.PermsRevoked + o.SudoRevoked; revoked > 0 {
		parts = append(parts, fmt.Sprintf("revoked %d grant(s)", revoked))
	}
	return strings.Join(parts, ", ")
}

// joinDetail appends extra to a Detail string with ", " glue, treating
// an empty base as "no prefix".
func joinDetail(base, extra string) string {
	if base == "" {
		return extra
	}
	return base + ", " + extra
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
	// requireGroupPermission also hands back the caller's conv-id: a real
	// agent (e.g. a PO orchestrating workers) resolves to its conv-id,
	// the human resolves to "". It is the default reply-to target for
	// the startup briefing assembled further down. Owners of g pass
	// without an explicit groups.spawn grant (owner-state default); the
	// spawn guardrails below still bind them (member cap, rate limit) and
	// already treat an owner as allowed for the group restriction.
	spawnerConvID, ok := requireGroupPermission(w, r, PermGroupsSpawn, g)
	if !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	// agent.SpawnRequest is the single shared request shape — the same
	// type `tclaude agent spawn`, `tclaude --join-group`, and the
	// dashboard's spawn modal marshal — so the wire contract can't drift
	// between the CLI and the dashboard. See its doc comment for the
	// per-field semantics.
	var body agent.SpawnRequest
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

	// Resolve the requested harness (default Claude Code). An unknown
	// name is a 400 here rather than a silent failure once the forked
	// session exits. The chosen harness's ModelCatalog then validates
	// effort/model below, so a Codex spawn is checked against Codex's
	// rules (rejects Claude Code slugs, accepts effort levels) instead of
	// Claude Code's.
	h, harnessErr := resolveSpawnHarness(body.Harness)
	if harnessErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_harness", harnessErr.Error())
		return
	}

	// Validate the requested effort before building the spawn params.
	// Empty → "" (downstream omits the flag); a bad level becomes a 400
	// here rather than a silent 504 once the forked session exits.
	effort, effErr := h.Models.ValidateEffort(body.Effort)
	if effErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_effort", effErr.Error())
		return
	}

	// Same treatment for the requested model: empty omits the flag, a
	// bad alias becomes a 400 here rather than a silent 504.
	model, modelErr := h.Models.ValidateModel(body.Model)
	if modelErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_model", modelErr.Error())
		return
	}

	// Hand the validated request to the shared spawn core. executeSpawn
	// owns the label → subprocess → conv-id poll → membership →
	// post-init sequence; the group-template instantiator drives the
	// same function in a loop. handleGroupSpawn keeps only the HTTP
	// shape — decode + validate above, error/JSON mapping below.
	p := spawnParams{
		Name:           body.Name,
		Role:           body.Role,
		Descr:          body.Descr,
		InitialMessage: body.InitialMessage,
		Cwd:            cwd,
		WorktreePath:   worktreePath,
		WorktreeBranch: worktreeBranch,
		AutoFocus:      body.AutoFocus,
		Effort:         effort,
		Model:          model,
		Harness:        h.Name,
		ReplyToConv:    replyToConv,
		SpawnedByConv:  spawnerConvID,
		Timeout:        timeout,
	}
	// An omitted include_group_context flag means opt-in — every spawn
	// path inherits the group context by default, the same way it
	// inherits default_cwd; the dashboard sends false explicitly to opt
	// out.
	if body.IncludeGroupContext == nil || *body.IncludeGroupContext {
		p.GroupContext = g.DefaultContext
	}

	outcome, fail := executeSpawn(g, p)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"group":        g.Name,
		"conv_id":      outcome.ConvID,
		"label":        outcome.Label,
		"tmux_session": outcome.TmuxSession,
		"attach_cmd":   "tclaude session attach " + outcome.Label,
	})
}

// spawnParams is the fully-resolved, validated input to executeSpawn.
// handleGroupSpawn builds one from the decoded HTTP body; the
// group-template instantiator builds one per template agent spec.
// Every field is already validated by the time it reaches executeSpawn
// — cwd absolute and existing, worktree path resolved, initial_message
// length/charset-checked, reply-to resolved to a conv-id — so the
// shared core does no HTTP-shaped validation of its own.
type spawnParams struct {
	Name           string
	Role           string
	Descr          string
	InitialMessage string
	Cwd            string // resolved absolute directory
	WorktreePath   string // resolved absolute directory, or ""
	WorktreeBranch string
	AutoFocus      bool
	// Effort is the validated Claude reasoning effort to forward to the
	// new session's `tclaude session new --effort`, or "" to omit it.
	Effort string
	// Model is the validated Claude model alias to forward to the new
	// session's `tclaude session new --model`. "" falls back to the
	// group's default_model inside executeSpawn; if that is unset too,
	// the flag is omitted entirely.
	Model string
	// Harness is the resolved harness name to launch ("" or "claude" =
	// Claude Code, the default; "codex" = Codex CLI). It forwards to
	// `tclaude session new --harness <h>` and is validated at the spawn
	// boundary (handleGroupSpawn resolves it against the harness registry
	// before building the params).
	Harness string
	// GroupContext is the shared startup context to fold into the
	// briefing, or "" to omit it. The caller has already applied any
	// opt-out, so executeSpawn injects it verbatim.
	GroupContext string
	// ReplyToConv is the resolved sender of the startup briefing —
	// "" for a human-initiated spawn.
	ReplyToConv string
	// SpawnedByConv is the conv-id of the agent that requested the
	// spawn, or "" for a human-initiated spawn. It drives the kickoff
	// welcome's attribution line — "spawned by <title>" for an agent
	// spawner, "spawned by the human" otherwise. Distinct from
	// ReplyToConv: the spawner is *who launched* the agent, the
	// reply-to is *where its brief-replies route*; a coordinator can
	// hand a worker off by setting them apart.
	SpawnedByConv string
	// Timeout bounds the conv-id poll; <= 0 falls back to 30s.
	Timeout time.Duration
}

// spawnOutcome is the success result of executeSpawn.
type spawnOutcome struct {
	ConvID      string
	Label       string
	TmuxSession string
}

// spawnFailure is a typed failure from executeSpawn. The HTTP handler
// maps Status/Kind/Msg straight onto writeError; the template
// instantiator ignores the HTTP-specific fields and reports Msg in its
// per-agent result.
type spawnFailure struct {
	Status int
	Kind   string
	Msg    string
}

// executeSpawn runs the validated spawn sequence: it forks a detached
// `tclaude session new`, polls the sessions table for the conv-id,
// joins the conv to the group, records the pending display name,
// optionally opens a terminal, drops the startup briefing into the new
// agent's inbox, and kicks off the post-init /rename + welcome
// injection. It is the single code path behind both the
// /v1/groups/{name}/spawn endpoint and the group-template instantiator.
//
// Returns either an outcome or a typed failure — never both. The agent
// is fully spawned and group-joined on success; on a post-membership
// best-effort failure (pending name, auto-focus, inbox insert) it still
// succeeds, those steps only log.
func executeSpawn(g *db.AgentGroup, p spawnParams) (*spawnOutcome, *spawnFailure) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	// When the request leaves model blank, fall back to the group's
	// default_model (set via the dashboard's model chip or `groups
	// set-default-model`). Living here — not in the HTTP handler —
	// makes the default reach every spawn path, including the
	// group-template instantiator. The stored value was validated by
	// the write path (handleGroupUpdate); an empty default keeps the
	// prior behaviour of omitting --model so claude resolves its own
	// default (user settings.json, then built-in).
	model := p.Model
	if model == "" {
		model = g.DefaultModel
	}

	// Generate a label that's unlikely to collide with existing
	// session IDs. Tclaude's GenerateSessionID() uses an 8-char
	// random hex; we mirror that with a "spwn-" prefix so these
	// rows are easy to spot in `tclaude session ls`.
	label := generateSpawnLabel()

	if err := SpawnDetachedTclaudeNew(label, p.Cwd, p.Effort, model, p.Harness); err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "spawn",
			"failed to launch tclaude session new: " + err.Error()}
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
		return nil, &spawnFailure{http.StatusGatewayTimeout, "timeout",
			"spawned session " + label + " but conv-id never materialised within " + timeout.String() +
				" — the session may still come up; check `tclaude session attach " + label + "`"}
	}

	// Membership add. Permission gating already happened in the caller;
	// this is just the DB write.
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g.ID,
		ConvID:  convID,
		Role:    p.Role,
		Descr:   p.Descr,
	}); err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io",
			"spawned conv " + convID + " but failed to add to group: " + err.Error()}
	}

	// Record the requested name as the agent's pending display name. The
	// /rename injection in runSpawnPostInit only lands a couple seconds
	// later, and until it does the conversation has no custom title — so
	// without this the dashboard would show "(unknown)" for that whole
	// window. agent.FreshTitle reads pending_name as a fallback; a real
	// /rename supersedes it. AddAgentGroupMember just enrolled the conv,
	// so this UPDATE has a row to hit. Best-effort: a failed write only
	// costs the "(unknown)" window — the pre-feature behaviour — so it is
	// logged, never bubbled. Stored even when the name is not a valid
	// rename title (the /rename is then skipped): the dashboard can still
	// show the intended name.
	if name := strings.TrimSpace(p.Name); name != "" {
		if err := db.SetEnrollmentPendingName(convID, name); err != nil {
			slog.Warn("spawn: failed to record pending name",
				"conv", convID, "name", name, "error", err)
		}
	}

	// Auto-focus: when the caller asked for it, open a terminal window
	// attached to the freshly-spawned agent. A detached spawn has no
	// window of its own, so this is what lets the human watch and talk
	// to the new agent right away — via `tclaude session attach`, never
	// raw tmux, so the reattached session keeps its tclaude features.
	//
	// Best-effort: the agent spawned fine regardless, so a failure to
	// pop a window is logged, never bubbled.
	if p.AutoFocus {
		if err := openTerminal(openAttachCmd(label)); err != nil {
			slog.Warn("spawn: auto-focus terminal failed to open",
				"conv", convID, "label", label, "error", err)
		}
	}

	// Spawn context: assemble the new agent's startup briefing and drop
	// it in its inbox as a single agent_messages row. Two pieces feed in
	// — the (already opt-out-applied) group context and the per-spawn
	// initial_message. They go to the inbox rather than the pane: a
	// large briefing bracketed-pasted into CC's input box risks
	// overflowing its input-size limit, and the inbox keeps newlines
	// verbatim regardless. The welcome line points the agent at the
	// message; runSpawnPostInit marks it delivered once the welcome
	// lands.
	spawnContext := buildSpawnContextBody(g.Name, p.GroupContext, p.InitialMessage)
	var spawnContextMsgID int64
	if spawnContext != "" {
		mid, msgErr := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:      g.ID,
			FromConv:     p.ReplyToConv,
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

	// Post-spawn injection: rename the new pane to the agent's name and
	// drop a [system: ...] welcome describing the agent's identity. It
	// also materialises the .jsonl (CC only writes the file once it has
	// content), so `agent resume` has something to resume. Runs in a
	// goroutine so the caller returns promptly; the goroutine waits for
	// the pane to come alive before injecting.
	goBackground(func() {
		runSpawnPostInit(convID, p.Name, p.Role, p.Descr, g.Name,
			spawnContextMsgID, p.InitialMessage != "", p.WorktreePath, p.WorktreeBranch,
			p.SpawnedByConv)
	})

	return &spawnOutcome{ConvID: convID, Label: label, TmuxSession: tmuxSession}, nil
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
//
// spawnedByConv is the conv-id of the agent that requested the spawn
// ("" for a human-initiated one); it is resolved to a display name
// here so the welcome's attribution line names the real spawner.
func runSpawnPostInit(convID, name, role, descr, groupName string, spawnContextMsgID int64, hasInitialMessage bool, worktreePath, worktreeBranch, spawnedByConv string) {
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

	// Rename first — see the doc comment. Skipped when name is empty or
	// not a valid rename title (some callers pass human-friendly names
	// that don't fit the rename charset); the welcome below still
	// materialises the conversation in that case. deliverRename routes
	// through the harness (CC injects its rename command; a direct-write
	// harness uses its title store) — the charset gate stays here, since
	// deliverRename only routes, it does not validate.
	if name != "" && isValidRenameTitle(name) {
		if !deliverRename(convID, name) {
			slog.Warn("spawn: rename delivery failed",
				"conv", convID, "name", name)
		}
	} else if name != "" {
		slog.Warn("spawn: name not a valid rename title; skipping rename",
			"conv", convID, "name", name)
	}

	// Welcome: a single-line [system: ...] turn orienting the agent
	// (identity, role, descr, group, where its startup briefing waits,
	// and — for a sub-repo worktree — where to make code edits).
	welcome := buildSpawnWelcome(name, role, descr, groupName,
		spawnContextMsgID, hasInitialMessage, worktreePath, worktreeBranch,
		resolveSpawnerTitle(spawnedByConv))
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
// spawnedBy is the attribution shown in the opening clause. "" means a
// human-initiated spawn — the clause stays "spawned by the human". A
// non-empty value is the spawning agent's display name, so an agent
// the PO spawned reads "spawned by <po-name>" rather than being
// misattributed to the human. resolveSpawnerTitle produces it from
// the spawner's conv-id.
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
func buildSpawnWelcome(name, role, descr, groupName string, spawnContextMsgID int64, hasInitialMessage bool, worktreePath, worktreeBranch, spawnedBy string) string {
	attribution := "spawned by the human"
	if spawnedBy != "" {
		attribution = "spawned by " + spawnedBy
	}
	parts := []string{attribution}
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

// resolveSpawnerTitle turns the spawning agent's conv-id into the
// display name buildSpawnWelcome puts in its attribution clause.
//
//   - "" (a human-initiated spawn) stays "" — the welcome then keeps
//     its "spawned by the human" framing.
//   - an agent conv-id resolves through agent.FreshTitle, the same
//     name listing surfaces show.
//   - anything that isn't a clean agent name — FreshTitle's
//     "(unknown)" placeholder, or a title that fails isValidRenameTitle
//     — is downgraded to the generic "another agent".
//
// The isValidRenameTitle gate is load-bearing, not cosmetic.
// FreshTitle falls back to a conversation summary or first prompt when
// a conv has no custom title, and a custom title set via Claude Code's
// own /rename (as opposed to the daemon's gated endpoint) is never
// charset-checked either — so the resolved string can carry newlines
// or other control characters. The welcome is injected into the new
// agent's pane with tmux send-keys, where a raw newline lands as a
// premature submit; routing the title through the same gate every
// tclaude-side rename passes keeps the welcome a safe single line.
// "(unknown)" is rejected explicitly because it happens to satisfy
// isValidRenameTitle.
func resolveSpawnerTitle(spawnedByConv string) string {
	if spawnedByConv == "" {
		return ""
	}
	title := agent.FreshTitle(spawnedByConv)
	if title == "" || title == agent.UnknownTitle || !isValidRenameTitle(title) {
		return "another agent"
	}
	return title
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
func SpawnDetachedTclaudeNew(label, cwd, effort, model, harness string) error {
	return Spawn.SpawnNew(label, cwd, effort, model, harness)
}

// SpawnDetachedTclaudeResume is a thin facade over Spawn.SpawnResume.
// effort and model ("" = omit the flag) ride through to the resumed
// claude invocation — `claude --resume` does NOT restore the
// conversation's previous model on its own, so resume surfaces pass
// the predecessor's inherited flags to keep the agent on its model.
func SpawnDetachedTclaudeResume(convID, cwd, effort, model, harness string) error {
	return Spawn.SpawnResume(convID, cwd, effort, model, harness)
}

// sessionNewArgs builds the argv for the detached `tclaude session new`
// that a spawn forks. --effort and --model are each appended only when
// an explicit value was chosen; an empty value leaves claude on its own
// default. Kept pure so it can be unit-tested without forking a
// subprocess.
func sessionNewArgs(label, cwd, effort, model, harness string) []string {
	args := []string{"session", "new", "-d", "--global", "--label", label}
	if cwd != "" {
		args = append(args, "-C", cwd)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = appendHarnessFlag(args, harness)
	return args
}

// sessionResumeArgs builds the argv for the detached `tclaude session
// new -r <conv>` that a resume forks. Same flag discipline as
// sessionNewArgs: --effort and --model are appended only when a value
// was chosen, so "" keeps claude on its own default. Kept pure so it
// can be unit-tested without forking a subprocess.
func sessionResumeArgs(convID, cwd, effort, model, harness string) []string {
	args := []string{"session", "new", "-r", convID, "-d", "--global"}
	if cwd != "" {
		args = append(args, "-C", cwd)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = appendHarnessFlag(args, harness)
	return args
}

// appendHarnessFlag adds `--harness <h>` to a `tclaude session new` argv
// when h names a non-default harness. The empty string and the default
// harness (Claude Code) both omit the flag, so an untagged spawn keeps the
// exact pre-JOH-160 argv and Claude Code stays the zero-config default.
// h is a registered harness name (validated at the spawn boundary), never
// user free-text, so it is safe as a bare arg.
func appendHarnessFlag(args []string, h string) []string {
	if h != "" && h != harness.DefaultName {
		args = append(args, "--harness", h)
	}
	return args
}

// liveSpawnNew runs `tclaude session new -d --global --label <label>`
// as a fully-detached subprocess. Same detachment story as
// liveSpawnResume — see its doc comment for the full rationale on
// why this doesn't trip CC's process-ownership checks.
//
// The label is the tclaude-side session ID (used to look up the row
// in SQLite once the conv-id materialises). It must be unique in the
// sessions table.
func liveSpawnNew(label, cwd, effort, model, harness string) error {
	// effort and model are validated at the spawn boundary
	// (handleGroupSpawn / the `agent spawn` CLI) before they reach here;
	// the forked `tclaude session new` re-validates too, though by then
	// a bad value would only surface as a non-zero exit in the daemon
	// log. sessionNewArgs omits each flag entirely when its value is "".
	cmd := exec.Command("tclaude", sessionNewArgs(label, cwd, effort, model, harness)...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	// Capture stderr so a silent subprocess failure (PATH issue, bad
	// cwd, broken tmux server, etc.) shows up in the daemon log
	// instead of disappearing into /dev/null. Bounded so a runaway
	// child can't grow the buffer unboundedly.
	stderr := newSpawnStderrCapture()
	cmd.Stderr = stderr
	// Spawned agents must not inherit the human's operator token.
	cmd.Env = spawnEnvWithoutOperatorToken()
	detachSpawn(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	go func() {
		if err := cmd.Wait(); err != nil {
			slog.Error("spawn subprocess exited with error",
				"label", label, "pid", pid, "err", err,
				"stderr", stderr.String(), "stderr_truncated", stderr.Truncated())
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
func liveSpawnResume(convID, cwd, effort, model, harness string) error {
	args := sessionResumeArgs(convID, cwd, effort, model, harness)
	cmd := exec.Command("tclaude", args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	stderr := newSpawnStderrCapture()
	cmd.Stderr = stderr
	// Spawned agents must not inherit the human's operator token.
	cmd.Env = spawnEnvWithoutOperatorToken()
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
			slog.Error("resume subprocess exited with error",
				"conv", convID, "pid", pid, "err", err,
				"stderr", stderr.String(), "stderr_truncated", stderr.Truncated())
		}
	}()
	return nil
}

// spawnStderrCapture is a bounded io.Writer used for capturing
// subprocess stderr of detached `tclaude session new` invocations.
// Caps at spawnStderrMax bytes; further writes are silently dropped
// and Truncated() reports whether truncation happened. Concurrent
// writes are not expected (exec.Cmd has a single writer goroutine)
// but the mutex makes accidental concurrent String() calls safe.
const spawnStderrMax = 8 << 10

type spawnStderrCapture struct {
	buf       []byte
	truncated bool
}

func newSpawnStderrCapture() *spawnStderrCapture {
	return &spawnStderrCapture{buf: make([]byte, 0, 512)}
}

func (c *spawnStderrCapture) Write(p []byte) (int, error) {
	if c == nil {
		return len(p), nil
	}
	remaining := spawnStderrMax - len(c.buf)
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf = append(c.buf, p[:remaining]...)
		c.truncated = true
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *spawnStderrCapture) String() string {
	if c == nil {
		return ""
	}
	return strings.TrimRight(string(c.buf), "\r\n ")
}

func (c *spawnStderrCapture) Truncated() bool {
	if c == nil {
		return false
	}
	return c.truncated
}
