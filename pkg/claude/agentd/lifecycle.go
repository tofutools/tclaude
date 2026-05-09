package agentd

import (
	"log/slog"
	"net/http"
	"os/exec"

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

// spawnDetachedTclaudeResume runs `tclaude session new -r <conv> -d
// --global` as a fully-detached subprocess. The daemon doesn't wait —
// the launched session manages itself, registers via hooks, and we
// see it appear in subsequent `groups members` output.
//
// We deliberately don't capture stdout/stderr; the subprocess is
// long-lived and any output would just accumulate. Errors only
// surface if the exec.Start() itself fails (binary missing, etc.).
func spawnDetachedTclaudeResume(convID, cwd string) error {
	args := []string{"session", "new", "-r", convID, "-d", "--global"}
	if cwd != "" {
		args = append(args, "-C", cwd)
	}
	cmd := exec.Command("tclaude", args...)
	// Detach completely — no stdio inheritance, no PG attachment, so
	// the subprocess survives daemon restart cleanly.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	// Reap the process when it finishes so we don't leak zombies.
	go func() {
		if err := cmd.Wait(); err != nil {
			slog.Debug("resume subprocess exited",
				"conv", convID, "pid", pid, "err", err)
		}
	}()
	return nil
}

