package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// SpawnResponse is the daemon's response shape for
// POST /v1/groups/{name}/spawn — used by both `tclaude agent spawn`
// and `tclaude --join-group`. Mirrors the keys handleGroupSpawn writes
// in pkg/claude/agentd/lifecycle.go.
type SpawnResponse struct {
	Group       string `json:"group"`
	ConvID      string `json:"conv_id"`
	Label       string `json:"label"`
	TmuxSession string `json:"tmux_session"`
	AttachCmd   string `json:"attach_cmd"`
}

// SpawnParams drives `tclaude agent spawn <group>`. The daemon does
// the actual spawn + group-join; this struct just shapes the request.
type SpawnParams struct {
	Group          string `pos:"true" help:"Existing group to join the new agent into"`
	Name           string `long:"name" short:"n" optional:"true" help:"Name for the new agent (e.g. 'reviewer'). Becomes its conversation title via /rename"`
	Role           string `long:"role" short:"r" optional:"true" help:"Role tag for the new member (e.g. 'tech-lead')"`
	Descr          string `long:"descr" short:"d" optional:"true" help:"Short one-line description shown on the dashboard. Keep it terse — use --initial-message for the task brief"`
	InitialMessage string `long:"initial-message" short:"m" optional:"true" help:"Task brief delivered to the new agent's inbox. Newlines are preserved — pass a full multi-line brief if you like"`
	File           string `long:"file" short:"f" optional:"true" help:"Read the task brief from this file instead of --initial-message ('-' reads stdin). Sidesteps shell quoting — best for long, multi-line, or backtick-containing briefs. Mutually exclusive with --initial-message; same 16384-byte cap"`
	ReplyTo        string `long:"reply-to" optional:"true" help:"Whom the new agent's reply to its startup brief should reach (conv-id / prefix / title). Defaults to you when you are an agent; empty for a human-initiated spawn"`
	Cwd            string `long:"cwd" short:"C" optional:"true" help:"Working directory for the new CC session (defaults to the caller's cwd)"`
	Timeout        string `long:"timeout" short:"t" optional:"true" help:"How long to wait for the new conv-id to materialise (e.g. 30s, 1m). Default 30s."`

	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

// spawnCmd starts a fresh CC session and registers it in an existing
// group in one shot. Useful for "I want to delegate this in parallel"
// flows where you want the new agent to be reachable by name from the
// existing team without manually wiring up membership after the fact.
func spawnCmd() *cobra.Command {
	return boa.CmdT[SpawnParams]{
		Use:   "spawn",
		Short: "Spawn a fresh CC session and add it to an existing group",
		Long: "Launches `tclaude session new -d --global` with a generated label, " +
			"waits for the new conv-id to materialise, and adds the new conv to <group> " +
			"with the given role/descr. --name becomes the new agent's conversation " +
			"title (injected as /rename on its pane). Prints the attach command for the " +
			"new session. --descr is the short dashboard label; pass --initial-message to " +
			"deliver the new agent its first task brief to its inbox (newlines preserved). " +
			"For a long or multi-line brief, prefer --file <path> (or --file - to read " +
			"stdin) — it reads the brief from a file and so sidesteps shell quoting, " +
			"including backticks the shell would otherwise eat from an inline string. " +
			"Requires the `groups.spawn` permission (default: human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *SpawnParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *SpawnParams, _ *cobra.Command, _ []string) {
			_, rc := RunSpawn(p, os.Stdout, os.Stderr, os.Stdin)
			os.Exit(rc)
		},
	}.ToCobra()
}

// RunSpawn drives `tclaude agent spawn`. Returns the daemon's response
// (nil on failure) alongside an exit code for the CLI wrapper. Flow
// tests use the returned response to assert what the user would see
// printed; the CLI wrapper just propagates the exit code. stdin backs
// `--file -` (read the brief from a pipe).
func RunSpawn(p *SpawnParams, stdout, stderr io.Writer, stdin io.Reader) (*SpawnResponse, int) {
	if p.Group == "" {
		fmt.Fprintln(stderr, "Error: group is required")
		return nil, rcInvalidArg
	}
	rawMessage, rc := resolveBodyInput(p.InitialMessage, p.File, "--initial-message", stdin, stderr)
	if rc != rcOK {
		return nil, rc
	}
	initialMessage := strings.TrimSpace(rawMessage)
	if !isValidInitialMessage(initialMessage) {
		fmt.Fprintf(stderr, "Error: REJECTED. --initial-message must be at most %d characters.\n", MaxInitialMessageBytes)
		fmt.Fprintln(stderr, "Newlines and tabs are allowed (the brief is delivered to the agent's")
		fmt.Fprintln(stderr, "inbox, not typed into its pane), but other control characters are not.")
		return nil, rcInvalidArg
	}
	timeoutSeconds := 30
	if p.Timeout != "" {
		d, err := parseDurationDays(p.Timeout)
		if err != nil || d <= 0 {
			fmt.Fprintf(stderr, "Error: invalid --timeout %q\n", p.Timeout)
			return nil, rcInvalidArg
		}
		// Cap mirrors the daemon's 5-minute hard limit.
		secs := int(d.Seconds())
		if secs > 300 {
			secs = 300
		}
		timeoutSeconds = secs
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return nil, rc
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, rcInvalidArg
	}
	cwd := p.Cwd
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	body := map[string]any{
		"name":            p.Name,
		"role":            p.Role,
		"descr":           p.Descr,
		"initial_message": initialMessage,
		"reply_to":        strings.TrimSpace(p.ReplyTo),
		"cwd":             cwd,
		"timeout_seconds": timeoutSeconds,
	}
	var resp SpawnResponse
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	path := "/v1/groups/" + p.Group + "/spawn"
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Spawned %s in group %q\n", short(resp.ConvID), resp.Group)
	if resp.Label != "" {
		fmt.Fprintf(stdout, "  Label:   %s\n", resp.Label)
	}
	if resp.TmuxSession != "" {
		fmt.Fprintf(stdout, "  Tmux:    %s\n", resp.TmuxSession)
	}
	if resp.AttachCmd != "" {
		fmt.Fprintf(stdout, "  Attach:  %s\n", resp.AttachCmd)
	}
	return &resp, rcOK
}
