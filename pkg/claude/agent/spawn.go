package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// spawnParams drives `tclaude agent spawn <group>`. The daemon does
// the actual spawn + group-join; this struct just shapes the request.
type spawnParams struct {
	Group   string `pos:"true" help:"Existing group to join the new agent into"`
	Alias   string `long:"alias" short:"a" optional:"true" help:"Alias for the new member in this group (e.g. 'reviewer')"`
	Role    string `long:"role" short:"r" optional:"true" help:"Role tag for the new member (e.g. 'tech-lead')"`
	Descr   string `long:"descr" short:"d" optional:"true" help:"Description of the new member's purpose in this group"`
	Cwd     string `long:"cwd" short:"C" optional:"true" help:"Working directory for the new CC session (defaults to daemon's cwd)"`
	Timeout string `long:"timeout" short:"t" optional:"true" help:"How long to wait for the new conv-id to materialise (e.g. 30s, 1m). Default 30s."`

	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

// spawnCmd starts a fresh CC session and registers it in an existing
// group in one shot. Useful for "I want to delegate this in parallel"
// flows where you want the new agent to be reachable by name from the
// existing team without manually wiring up membership after the fact.
func spawnCmd() *cobra.Command {
	return boa.CmdT[spawnParams]{
		Use:   "spawn",
		Short: "Spawn a fresh CC session and add it to an existing group",
		Long: "Launches `tclaude session new -d --global` with a generated label, " +
			"waits for the new conv-id to materialise, and adds the new conv to <group> " +
			"with the given alias/role/descr. Prints the attach command for the new session. " +
			"Requires the `groups.spawn` permission (default: human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *spawnParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *spawnParams, _ *cobra.Command, _ []string) {
			os.Exit(runSpawn(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runSpawn(p *spawnParams, stdout, stderr io.Writer) int {
	if p.Group == "" {
		fmt.Fprintln(stderr, "Error: group is required")
		return rcInvalidArg
	}
	timeoutSeconds := 30
	if p.Timeout != "" {
		d, err := parseDurationDays(p.Timeout)
		if err != nil || d <= 0 {
			fmt.Fprintf(stderr, "Error: invalid --timeout %q\n", p.Timeout)
			return rcInvalidArg
		}
		// Cap mirrors the daemon's 5-minute hard limit.
		secs := int(d.Seconds())
		if secs > 300 {
			secs = 300
		}
		timeoutSeconds = secs
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	body := map[string]any{
		"alias":           p.Alias,
		"role":            p.Role,
		"descr":           p.Descr,
		"cwd":             p.Cwd,
		"timeout_seconds": timeoutSeconds,
	}
	var resp struct {
		Group       string `json:"group"`
		ConvID      string `json:"conv_id"`
		Label       string `json:"label"`
		TmuxSession string `json:"tmux_session"`
		AttachCmd   string `json:"attach_cmd"`
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	path := "/v1/groups/" + p.Group + "/spawn"
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
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
	return rcOK
}
