package workflows

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

// Exit codes for `workflows run`. Mirror the `tclaude agent` codes so the
// daemon-talking surfaces agree; agent.RequireDaemonOrExit /
// agent.MapDaemonErrorToRC already return these values.
const (
	rcOK         = 0
	rcInvalidArg = 3
)

// RunParams configures `workflows run`.
type RunParams struct {
	Name     string `pos:"true" help:"The saved workflow's name (its /<name> command, e.g. 'review-changes'). Must be an exact, already-saved workflow — no free-form prompt."`
	Target   string `long:"target" help:"The agent/session whose pane to launch the workflow in. Selector: title, full conv-id, or 8+-char prefix. REQUIRED."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

// RunCmd returns the `workflows run` subcommand.
//
// EXPERIMENTAL spike (JOH-59). It asks tclaude agentd to inject a saved
// workflow's `/<name>` launch command into the target agent's CC pane via
// the same tmux send-keys path /rename and /compact ride. Permission-gated
// on the default-denied `workflow.trigger` slug (or group ownership). It is
// best-effort, NOT a deterministic launch — see the long help.
func RunCmd() *cobra.Command {
	return boa.CmdT[RunParams]{
		Use:   "run <name> --target <agent>",
		Short: "EXPERIMENTAL: trigger a saved workflow in another agent's pane",
		Long: "EXPERIMENTAL / best-effort. Inject a saved Claude Code workflow's `/<name>`\n" +
			"launch command into a target agent's pane, via tclaude agentd's tmux\n" +
			"send-keys path (the same one /rename and /compact use).\n\n" +
			"Strict name-only: <name> must be an exact match in the target's enumerated\n" +
			"saved-workflow set (`workflows ls --saved`) and slash-command-safe — there is\n" +
			"NO free-form prompt payload. Permission-gated on the default-denied\n" +
			"`workflow.trigger` slug (the human grants it), or being an owner of a group\n" +
			"containing the target.\n\n" +
			"NOT a guaranteed launch. There is no local API/CLI that deterministically\n" +
			"starts a CC workflow run; launching is model-/user-cooperative. CC registers\n" +
			"saved workflows as `/<name>` commands, so this initiates the launch — but in\n" +
			"the target's default permission mode it raises an approval prompt the target\n" +
			"must accept before the run starts (it fires immediately only in\n" +
			"auto/bypass/headless modes).",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *RunParams, _ *cobra.Command, _ []string) {
			os.Exit(RunRun(params, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// isValidWorkflowName mirrors the daemon-side check in
// agentd/workflow_run.go. Kept in sync deliberately: the daemon is the
// actual security boundary, but we want a fast local error before sending
// a doomed request.
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

// RunRun is the testable core of `workflows run`.
func RunRun(p *RunParams, stdout, stderr io.Writer) int {
	name := strings.TrimSpace(p.Name)
	target := strings.TrimSpace(p.Target)

	if target == "" {
		fmt.Fprintln(stderr, "Error: --target is required (the agent/session whose pane to launch the workflow in).")
		return rcInvalidArg
	}
	if !isValidWorkflowName(name) {
		fmt.Fprintln(stderr, "Error: REJECTED. Workflow name must be 1-128 characters from [A-Za-z0-9._-] only.")
		fmt.Fprintln(stderr, "Spaces, slashes, newlines, control chars and unicode are NOT allowed — the name is")
		fmt.Fprintln(stderr, "injected as a literal `/<name>` command. This endpoint launches a SAVED workflow")
		fmt.Fprintln(stderr, "by name only; there is no free-form prompt payload. See `workflows ls --saved`.")
		return rcInvalidArg
	}

	// Validate the cheap local args (incl. --ask-human) before checking
	// the daemon, so a malformed flag reports the arg error rather than
	// "daemon not running" when the daemon happens to be down.
	ask, err := agent.ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}

	var resp struct {
		ConvID       string `json:"conv_id"`
		CallerConv   string `json:"caller_conv,omitempty"`
		Workflow     string `json:"workflow"`
		Injected     string `json:"injected"`
		Experimental bool   `json:"experimental"`
		Note         string `json:"note,omitempty"`
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	path := "/v1/agent/" + url.PathEscape(target) + "/workflow-run"
	body := map[string]any{"name": name}
	if err := agent.DaemonRequest(http.MethodPost, path, body, &resp, agent.DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return agent.MapDaemonErrorToRC(err)
	}

	fmt.Fprintf(stdout, "[experimental] Injected %s into %s", resp.Injected, short(resp.ConvID))
	if resp.CallerConv != "" {
		fmt.Fprintf(stdout, " (by %s)", short(resp.CallerConv))
	}
	fmt.Fprintln(stdout)
	if resp.Note != "" {
		fmt.Fprintf(stdout, "Note: %s\n", resp.Note)
	}
	return rcOK
}

// short truncates a conv-id for display, mirroring the `agent` package's
// unexported helper (which we can't reach across packages).
func short(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
