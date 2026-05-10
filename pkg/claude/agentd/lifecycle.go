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
	if err := spawnDetachedTclaudeResume(convID, cwd); err != nil {
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
		Alias          string `json:"alias,omitempty"`
		Role           string `json:"role,omitempty"`
		Descr          string `json:"descr,omitempty"`
		Cwd            string `json:"cwd,omitempty"`
		TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "json", err.Error())
			return
		}
	}
	timeout := 30 * time.Second
	if body.TimeoutSeconds > 0 {
		timeout = time.Duration(body.TimeoutSeconds) * time.Second
		if timeout > 5*time.Minute {
			timeout = 5 * time.Minute
		}
	}

	// Generate a label that's unlikely to collide with existing
	// session IDs. Tclaude's GenerateSessionID() uses an 8-char
	// random hex; we mirror that with a "spwn-" prefix so these
	// rows are easy to spot in `tclaude session ls`.
	label := generateSpawnLabel()

	if err := spawnDetachedTclaudeNew(label, body.Cwd); err != nil {
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
	go runSpawnPostInit(convID, body.Alias, body.Role, body.Descr, g.Name)

	writeJSON(w, http.StatusOK, map[string]any{
		"group":        g.Name,
		"conv_id":      convID,
		"label":        label,
		"tmux_session": tmuxSession,
		"attach_cmd":   "tclaude session attach " + label,
	})
}

// runSpawnPostInit fires asynchronously after a successful spawn. It
// waits for the new tmux pane to come online, then injects /rename
// (when alias is a valid rename title) followed by a welcome system
// message. Failures are logged, never bubbled — the spawn already
// succeeded as far as the caller is concerned.
//
// Why /rename first: it's a slash command CC processes immediately,
// landing a write on the .jsonl before any other turn happens. Even
// if the welcome injection fails afterwards, the file exists and
// `agent resume` can find it.
func runSpawnPostInit(convID, alias, role, descr, groupName string) {
	if !waitForConvAlive(convID) {
		slog.Warn("spawn: new conv never came online; rename + welcome abandoned",
			"conv", convID)
		return
	}

	welcome := buildSpawnWelcome(alias, role, descr, groupName)

	// Prefer the slash + follow-up combo when we can rename. /rename
	// alone covers the .jsonl-materialisation half; the follow-up
	// covers the context-handoff half. Skips rename when alias is
	// empty / invalid (so spawn doesn't break for callers that pass
	// human-friendly aliases that don't fit the rename charset).
	if alias != "" && isValidRenameTitle(alias) {
		if !injectSlashCommand(convID, "/rename "+alias, welcome) {
			slog.Warn("spawn: post-init injection failed", "conv", convID, "alias", alias)
		}
		return
	}
	// No valid alias to rename to — still inject the welcome so the
	// .jsonl gets written and the agent has its context.
	if alias != "" {
		slog.Warn("spawn: alias not a valid rename title; skipping /rename, sending welcome only",
			"conv", convID, "alias", alias)
	}
	candidates, err := db.FindSessionsByConvID(convID)
	if err != nil || len(candidates) == 0 {
		slog.Warn("spawn: cannot resolve tmux session for welcome", "conv", convID)
		return
	}
	var tmuxTarget string
	for _, c := range candidates {
		if c.TmuxSession != "" && session.IsTmuxSessionAlive(c.TmuxSession) {
			tmuxTarget = c.TmuxSession + ":0.0"
			break
		}
	}
	if tmuxTarget == "" {
		slog.Warn("spawn: no alive tmux session for welcome", "conv", convID)
		return
	}
	if err := injectTextAndSubmit(tmuxTarget, welcome); err != nil {
		slog.Warn("spawn: welcome injection failed", "conv", convID, "error", err)
	}
}

// buildSpawnWelcome composes the [system: ...] welcome text. Brackets
// signal "this is metadata from tclaude, not a human prompt" — same
// convention agent-message nudges use. Kept to a single line so it
// renders cleanly in CC's prompt history.
func buildSpawnWelcome(alias, role, descr, groupName string) string {
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
	body += " Use `tclaude agent` commands (whoami / help / inbox ls) to introspect and coordinate. Wait for the first instruction."
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

// spawnDetachedTclaudeNew runs `tclaude session new -d --global
// --label <label>` as a fully-detached subprocess. Same detachment
// story as spawnDetachedTclaudeResume — see its doc comment for the
// full rationale on why this doesn't trip CC's process-ownership
// checks.
//
// The label is the tclaude-side session ID (used to look up the row
// in SQLite once the conv-id materialises). It must be unique in the
// sessions table.
func spawnDetachedTclaudeNew(label, cwd string) error {
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

// spawnDetachedTclaudeResume runs `tclaude session new -r <conv> -d
// --global` as a fully-detached subprocess.
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
func spawnDetachedTclaudeResume(convID, cwd string) error {
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

