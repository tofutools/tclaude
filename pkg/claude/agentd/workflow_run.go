package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/ccworkflows"
)

// --- /v1/agent/{selector}/workflow-run (POST) -----------------------------
//
// EXPERIMENTAL / best-effort. This is the JOH-59 trigger spike: it reuses
// agentd's existing tmux send-keys plumbing (the same path /rename, /compact
// and cron nudges ride) to inject a saved Claude Code workflow's launch
// command — `/<name>` — into a target agent's pane.
//
// It is NOT a deterministic launch. There is no local API or CLI that starts
// a CC workflow run; launching is model-/user-cooperative. Claude Code
// registers each saved workflow as a `/<name>` slash command, so injecting
// `/<name>` initiates the launch flow — but in the target's default
// permission mode that raises an approval prompt the target must accept
// before the run actually starts (it fires immediately only in auto /
// bypass / headless modes). So the honest framing is: this nudges a launch;
// it does not guarantee one. The limits are documented sharply on JOH-59.
//
// Trust boundary: the injected text is literal send-keys, so a permissive
// payload could break out into an arbitrary `/<cmd>` in the target's pane.
// We close that boundary the same way the rename handler does — the only
// variable in the payload is the workflow name, which is strict-validated
// against a slash-command-safe charset AND must be an exact match in the
// target's enumerated saved-script set. There is deliberately NO free-form
// prompt payload.

// isValidWorkflowName reports whether name is safe to inject as `/<name>`
// and shaped like a saved-workflow slash command. The charset is the hard
// keystroke-injection gate (mirrors isValidRenameTitle's rationale): with
// only [A-Za-z0-9._-] there is no space, slash, newline or control char to
// break out of the `/<name>` command. Saved workflows become `/<name>`
// commands in CC, so a name outside this charset could not be a launchable
// command anyway. Exact existence is checked separately against the
// enumerated saved set; this is just the security/shape backstop.
//
// `.` is intentionally allowed (saved names can contain dots), and is not
// a traversal vector: name is never used to build a filesystem path — it
// is only string-compared against enumerated saved-script basenames and
// injected as the literal text `/<name>`. So even `..` can match only a
// real file literally named `...js`, and `/..` is an inert non-command.
func isValidWorkflowName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// savedScriptNames returns the Name of every script in scripts, for
// surfacing the available set in a not-found error.
func savedScriptNames(scripts []ccworkflows.SavedScript) []string {
	out := make([]string, 0, len(scripts))
	for i := range scripts {
		out = append(out, scripts[i].Name)
	}
	return out
}

// handleAgentWorkflowRun injects a saved workflow's `/<name>` launch command
// into ANOTHER agent's CC pane. Routed via handleAgentByConv. Auth:
// workflow.trigger slug OR caller owns a group containing target. See the
// package note above for why this is EXPERIMENTAL / best-effort.
func handleAgentWorkflowRun(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermWorkflowTrigger, targetConv)
	if !ok {
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)

	// Security gate FIRST: the name becomes literal `/<name>` tmux
	// send-keys input. A permissive name (newline, space, slash) could
	// break out into an arbitrary slash command in the target's pane.
	// The strict charset closes that boundary — same hard gate as the
	// rename title validator. Not a style preference; not bypassable.
	if !isValidWorkflowName(name) {
		writeError(w, http.StatusBadRequest, "invalid_workflow_name",
			"REJECTED. Workflow name must be 1-128 characters from [A-Za-z0-9._-] only. "+
				"Spaces, slashes, newlines, control chars and unicode are NOT allowed: the name "+
				"is injected as a literal `/<name>` tmux send-keys command, so this is a hard "+
				"security gate against keystroke injection, not a style preference. There is no "+
				"free-form prompt payload — this endpoint launches a SAVED workflow by name only.")
		return
	}

	// Existence gate: the name must be an exact match in the TARGET's
	// enumerated saved-script set — user-scope (~/.claude/workflows/saved)
	// plus the target's project-local mirror, resolved from where the
	// target's CC was launched. This is the "name-only, reject free-form"
	// boundary: only a real saved workflow can be triggered.
	projectDir := agent.ResolveLocation(targetConv).StartupDir
	scripts, err := ccworkflows.DefaultSavedScripts(projectDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			fmt.Sprintf("could not enumerate saved workflows for target %s: %v", short8(targetConv), err))
		return
	}
	known := false
	for i := range scripts {
		if scripts[i].Name == name {
			known = true
			break
		}
	}
	if !known {
		avail := savedScriptNames(scripts)
		msg := fmt.Sprintf("no saved workflow named %q is visible to target %s.", name, short8(targetConv))
		if len(avail) == 0 {
			msg += " The target has no saved workflows (~/.claude/workflows/saved is empty or absent, and there is no project-local mirror)."
		} else {
			msg += " Available: " + strings.Join(avail, ", ") + "."
		}
		writeError(w, http.StatusNotFound, "unknown_workflow", msg)
		return
	}

	// Best-effort inject. injectSlashCommand resolves an alive tmux pane
	// for the target and types `/<name>` + submit. The launch is
	// model/user-cooperative from here (see the package note).
	launch := "/" + name
	if !injectSlashCommand(targetConv, launch, "") {
		writeError(w, http.StatusServiceUnavailable, "no_tmux",
			"target conv "+short8(targetConv)+" has no live tmux session to inject "+launch+" into")
		return
	}

	resp := map[string]any{
		"conv_id":      targetConv,
		"workflow":     name,
		"injected":     launch,
		"experimental": true,
		"note": "EXPERIMENTAL best-effort: " + launch + " was injected via tmux send-keys. " +
			"Claude Code registers saved workflows as slash commands, but in the target's default " +
			"permission mode this raises an approval prompt the target must accept — the launch is " +
			"model/user-cooperative, not guaranteed-fire. It starts immediately only in auto/bypass/headless modes.",
	}
	if caller != "" && caller != targetConv {
		resp["caller_conv"] = caller
	}
	writeJSON(w, http.StatusOK, resp)
}
