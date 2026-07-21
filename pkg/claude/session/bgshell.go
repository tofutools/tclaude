package session

import (
	"encoding/json"
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// This file decodes the two Claude Code hook payloads that move the
// background-shell ledger (db.BgShellSet): the PostToolUse of a `Bash`
// call launched with run_in_background, and the PostToolUse of a
// `TaskStop` that killed one.
//
// Both shapes are UNDOCUMENTED upstream and were established empirically
// (TCL-613). Every decoder here is therefore written to fail closed and
// silently: a payload that does not match is simply not evidence, never
// an error that could fail a hook. The ledger's TTL and the daemon's
// liveness reconcile are what keep the badge honest if a harness version
// changes these shapes out from under us.

// bashToolName / taskStopToolName are the tool names the ledger reacts to.
const (
	bashToolName     = "Bash"
	taskStopToolName = "TaskStop"
)

// bgShellToolInput is the subset of a Bash `tool_input` the ledger needs:
// whether the call was backgrounded, and the command itself (which is the
// only thing the liveness reconcile can match a live process against —
// Claude Code exposes no PID for a background task anywhere).
type bgShellToolInput struct {
	Command         string `json:"command"`
	RunInBackground bool   `json:"run_in_background"`
}

// bgShellToolResponse is the subset of a Bash `tool_response` the ledger
// needs. backgroundTaskId is the harness's handle for the launched task —
// the same id a later TaskStop names — and is the ledger key.
type bgShellToolResponse struct {
	BackgroundTaskID string `json:"backgroundTaskId"`
}

// taskStopToolInput is the subset of a TaskStop `tool_input` the ledger
// needs: which background task was killed.
type taskStopToolInput struct {
	TaskID string `json:"task_id"`
}

// harnessTracksBackgroundShells reports whether this session's harness has
// background shell commands tclaude can track. An unknown or unresolvable
// harness folds to FALSE — the opposite of harnessUsesSlashContextControls,
// deliberately: that helper defaults to the Claude Code behaviour because
// injecting a slash command a harness ignores is harmless, whereas adding
// ledger entries for a harness with no background-shell concept would grow
// a count nothing ever retires except the TTL.
func harnessTracksBackgroundShells(name string) bool {
	h, err := harness.Resolve(name)
	if err != nil {
		return false
	}
	return h.SupportsBackgroundShells()
}

// hasBackgroundActivity reports whether anything this session launched is
// believed to still be running past the end of the main thread's turn — a
// sub-agent or a background shell. It is the predicate the Stop /
// SubagentStop arms use to decide between plain idle and main_agent_idle.
func (s *SessionState) hasBackgroundActivity() bool {
	return len(s.Subagents) > 0 || len(s.BgShells) > 0
}

// backgroundActivityDetail renders this session's status_detail for
// main_agent_idle.
func (s *SessionState) backgroundActivityDetail() string {
	return BackgroundActivityDetail(len(s.Subagents), len(s.BgShells))
}

// BackgroundActivityDetail renders the status_detail that accompanies
// main_agent_idle: what is still running now that the main thread's turn
// has ended. The sub-agents-only wording is preserved verbatim from before
// background shells existed, so existing read surfaces and their tests keep
// seeing the exact string they always did.
//
// Exported because the dashboard re-renders it from its own RECONCILED
// counts, which can differ from the stored row's (a ledger entry whose
// process is gone is dropped at read time). Sharing one formatter keeps the
// pill's text and the badges beside it from disagreeing.
func BackgroundActivityDetail(subagents, shells int) string {
	switch {
	case shells == 0:
		return fmt.Sprintf("%d subagents running", subagents)
	case subagents == 0:
		return fmt.Sprintf("%s running", pluralize(shells, "background shell"))
	default:
		return fmt.Sprintf("%d subagents, %s running", subagents, pluralize(shells, "background shell"))
	}
}

// pluralize renders "1 thing" / "N things".
func pluralize(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// bgShellLaunch decodes a PostToolUse payload into a background-shell
// launch. ok is false for anything that is not a successful `Bash` call
// carrying run_in_background — the overwhelmingly common case, since this
// runs on every tool hook.
//
// A missing backgroundTaskId yields an empty id rather than a rejection:
// the launch DID happen (run_in_background said so), and db.BgShellSet.Add
// keys such an entry anonymously. The reconcile retires it on the same
// terms as a keyed one, since it matches on the command, so the degraded
// case still self-heals — it only loses the ability to honour a TaskStop
// naming that id.
func bgShellLaunch(input HookCallbackInput) (id, command string, ok bool) {
	if input.HookEventName != "PostToolUse" || input.ToolName != bashToolName {
		return "", "", false
	}
	var in bgShellToolInput
	if err := json.Unmarshal(input.ToolInput, &in); err != nil || !in.RunInBackground {
		return "", "", false
	}
	var resp bgShellToolResponse
	// A tool_response that is absent, or not an object at all, is not a
	// reason to drop the launch — only to lose the id.
	_ = json.Unmarshal(input.ToolResponse, &resp)
	return resp.BackgroundTaskID, in.Command, true
}

// bgShellStop decodes a PostToolUse payload into "this background task was
// killed". ok is false for anything that is not a `TaskStop` call.
//
// An empty task_id still reports ok: db.BgShellSet.Remove treats it as
// "drop the oldest", which is a better approximation of a kill that
// definitely happened than ignoring the event and leaving a ghost.
func bgShellStop(input HookCallbackInput) (id string, ok bool) {
	if input.HookEventName != "PostToolUse" || input.ToolName != taskStopToolName {
		return "", false
	}
	var in taskStopToolInput
	if err := json.Unmarshal(input.ToolInput, &in); err != nil {
		return "", true
	}
	return in.TaskID, true
}
