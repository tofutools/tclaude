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

// `tclaude agent notify-human` — send the human a notification that
// lands in the dashboard Messages tab. Permission-gated on human.notify
// (group owners always pass); the human reads it on the dashboard
// instead of scrolling the PO's busy terminal.

type notifyHumanParams struct {
	Body     string `pos:"true" optional:"true" help:"Notification text (or use --file)."`
	Subject  string `long:"subject" short:"s" optional:"true" help:"Optional one-line subject."`
	File     string `long:"file" short:"f" optional:"true" help:"Read the body from this file ('-' reads stdin). Sidesteps shell quoting — best for long, multi-line, or backtick-containing bodies."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func notifyHumanCmd() *cobra.Command {
	return boa.CmdT[notifyHumanParams]{
		Use:   "notify-human",
		Short: "Send the human a notification (shown in the dashboard Messages tab)",
		Long: "Sends a message to the human — it lands in the agentd dashboard's Messages tab, letting a coordinating agent reach the human off the busy terminal.\n\n" +
			"Sending is gated: it passes for the human, for holders of the `human.notify` permission (which the human grants to a trusted coordinating agent such as the PO), and for any group owner (owning a group is a trusted coordinating role, so an owner may send slug or not). Agents with none of these are refused.\n\n" +
			"Give the body inline or with --file (--file - reads stdin). Each message shows the human who sent it and offers a button to focus that agent's terminal window.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *notifyHumanParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *notifyHumanParams, _ *cobra.Command, _ []string) {
			os.Exit(runNotifyHuman(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runNotifyHuman(p *notifyHumanParams, stdin io.Reader, stdout, stderr io.Writer) int {
	body, rc := resolveBodyInput(p.Body, p.File, "the body argument", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if strings.TrimSpace(body) == "" {
		fmt.Fprintln(stderr, "Error: a notification body is required — pass it inline or via --file")
		return rcInvalidArg
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}

	payload := map[string]any{"body": body}
	if s := strings.TrimSpace(p.Subject); s != "" {
		payload["subject"] = s
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/notify-human", payload, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Notified the human (message #%d) — it will show in the dashboard Messages tab.\n", resp.ID)
	return rcOK
}
