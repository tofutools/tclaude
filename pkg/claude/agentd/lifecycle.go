package agentd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os/exec"
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
		res := memberOpResult{ConvID: m.ConvID, Alias: m.Alias}
		sess := pickAliveSession(m.ConvID)
		if sess == nil {
			res.Action = "skipped:already_offline"
			out.Members = append(out.Members, res)
			continue
		}
		res.TmuxSes = sess.TmuxSession
		if force {
			if err := clcommon.TmuxCommand("kill-session", "-t", sess.TmuxSession).Run(); err != nil {
				res.Action = "error"
				res.Detail = "kill-session: " + err.Error()
			} else {
				res.Action = "killed"
			}
		} else {
			// Soft stop: inject `/exit`. Same two-step send-keys the
			// /rename injector uses (see injectSlashCommand). CC closes
			// the conversation cleanly; tmux session goes away when CC
			// exits.
			if injectSlashCommand(m.ConvID, "/exit") {
				res.Action = "soft_stopped"
			} else {
				res.Action = "error"
				res.Detail = "send-keys /exit failed"
			}
		}
		out.Members = append(out.Members, res)
	}
	writeJSON(w, http.StatusOK, out)
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
		res := memberOpResult{ConvID: m.ConvID, Alias: m.Alias}
		if isConvOnline(m.ConvID) {
			res.Action = "skipped:already_online"
			out.Members = append(out.Members, res)
			continue
		}
		if m.ConvID == "" {
			res.Action = "skipped:no_conv_id"
			res.Detail = "placeholder member (no conv yet) — Phase B will support template-based fresh spawn"
			out.Members = append(out.Members, res)
			continue
		}
		// Look up the recorded cwd so resume lands the agent in the
		// directory they were last running in. Falls back to "" which
		// makes `tclaude session new` use its own default.
		cwd := ""
		if rows, _ := db.FindSessionsByConvID(m.ConvID); len(rows) > 0 {
			cwd = rows[0].Cwd
		}
		if err := spawnDetachedTclaudeResume(m.ConvID, cwd); err != nil {
			res.Action = "error"
			res.Detail = "spawn: " + err.Error()
		} else {
			res.Action = "resumed"
		}
		out.Members = append(out.Members, res)
	}
	writeJSON(w, http.StatusOK, out)
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

	writeJSON(w, http.StatusOK, map[string]any{
		"group":        g.Name,
		"conv_id":      convID,
		"label":        label,
		"tmux_session": tmuxSession,
		"attach_cmd":   "tclaude session attach " + label,
	})
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

