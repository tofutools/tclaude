package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// remoteControlCmd is `tclaude agent remote-control [on|off|toggle|status]` —
// drives Claude Code's built-in Remote Access (the `/remote-control` slash)
// on the calling agent's own pane via the daemon, gated on
// `self.remote-control`. `--target <selector>` swaps the action to ANOTHER
// agent (the manager pattern; requires `agent.remote-control` or owning a
// group containing the target).
//
// `/remote-control` is a TOGGLE with no programmatic readback, so tclaude
// tracks its own best-known state (JOH-256) and `status` reports that without
// touching the pane. `on`/`off` only act when the best-known state differs;
// `toggle` always flips it. The best-known state can drift if a human toggles
// remote control inside the pane directly.
func remoteControlCmd() *cobra.Command {
	return boa.CmdT[remoteControlParams]{
		Use:   "remote-control",
		Short: "Toggle a conversation's built-in remote access (self by default, or another with --target)",
		Long: "Asks tclaude agentd to inject the harness's `/remote-control` toggle into a CC pane, " +
			"exposing the session to claude.ai/code + the Claude mobile app. " +
			"By default the target is the calling agent itself (requires `self.remote-control`). " +
			"Use --target <selector> to act on ANOTHER agent — the manager pattern (requires " +
			"`agent.remote-control`, or being an owner of a group containing the target).\n\n" +
			"Intent (positional, default `toggle`):\n" +
			"  on      enable remote access (no-op if already on)\n" +
			"  off     disable remote access (sends the confirm Enter CC prompts for)\n" +
			"  toggle  flip the current best-known state\n" +
			"  status  report the best-known state without touching the pane\n\n" +
			"Note: Claude Code exposes no readback of remote-control state, so tclaude tracks its " +
			"own best-known value; it can drift if you toggle remote control inside the pane directly. " +
			"Remote access also requires being logged into claude.ai (OAuth, not an API key).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *remoteControlParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *remoteControlParams, _ *cobra.Command, _ []string) {
			os.Exit(runRemoteControl(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type remoteControlParams struct {
	Intent   string `pos:"true" optional:"true" help:"on | off | toggle | status (default: toggle)"`
	Target   string `long:"target" optional:"true" help:"Act on ANOTHER agent instead of self. Selector: title, full conv-id, or 8+-char prefix. Requires the agent.remote-control permission, or being an owner of a group containing the target."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny. Self-target only."`
}

func runRemoteControl(p *remoteControlParams, stdout, stderr io.Writer) int {
	intent := strings.TrimSpace(strings.ToLower(p.Intent))
	if intent == "" {
		intent = "toggle"
	}
	switch intent {
	case "on", "off", "toggle", "status":
	default:
		fmt.Fprintf(stderr, "Error: intent must be one of on | off | toggle | status (got %q)\n", p.Intent)
		return rcInvalidArg
	}
	target := strings.TrimSpace(p.Target)
	if target != "" && p.AskHuman != "" {
		fmt.Fprintln(stderr, "Error: --ask-human is only supported when targeting self; cross-agent calls require an explicit slug grant or group ownership.")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	path := "/v1/whoami/remote-control"
	if target != "" {
		path = "/v1/agent/" + url.PathEscape(target) + "/remote-control"
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	body := map[string]any{"intent": intent}
	var resp struct {
		ConvID        string `json:"conv_id"`
		CallerConv    string `json:"caller_conv,omitempty"`
		RemoteControl bool   `json:"remote_control"`
		Action        string `json:"action"`
		Note          string `json:"note,omitempty"`
	}
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	state := "off"
	if resp.RemoteControl {
		state = "on"
	}
	suffix := ""
	if resp.CallerConv != "" {
		suffix = fmt.Sprintf(" (called by %s)", short(resp.CallerConv))
	}
	if resp.Action == "status" {
		fmt.Fprintf(stdout, "Remote control is %s for %s%s\n", state, short(resp.ConvID), suffix)
	} else {
		fmt.Fprintf(stdout, "Remote control %s for %s (now %s)%s\n", resp.Action, short(resp.ConvID), state, suffix)
	}
	if resp.Note != "" {
		fmt.Fprintf(stdout, "Note: %s\n", resp.Note)
	}
	return rcOK
}
