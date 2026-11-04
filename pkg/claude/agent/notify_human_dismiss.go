package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent notify-human dismiss` — withdraw decisions you put to the
// human that are no longer relevant. Only your own: they are your messages,
// so no permission slug is involved.

type notifyHumanDismissParams struct {
	IDs    []string `pos:"true" optional:"true" help:"Decision message ids (from the notify-human output), e.g. 123 or #123."`
	All    bool     `long:"all" optional:"true" help:"Dismiss every decision of yours that is still open."`
	Reason string   `long:"reason" short:"r" optional:"true" help:"Why it no longer matters; shown to the human on the dismissed decision."`
}

func notifyHumanDismissCmd() *cobra.Command {
	return boa.CmdT[notifyHumanDismissParams]{
		Use:   "dismiss",
		Short: "Dismiss decisions you put to the human that no longer matter",
		Long: "Withdraws open decisions you asked with notify-human --option, without an answer. A dismissed decision leaves " +
			"the human's \"to decide\" queue and stays in Messages, marked dismissed, with your --reason.\n\n" +
			"You can dismiss only decisions you asked; anyone else's reads as not found. A decision the human already " +
			"answered cannot be dismissed (exit code for a conflict), and once dismissed it cannot be answered. " +
			"--all dismisses every decision of yours still open; ones answered meanwhile are skipped.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *notifyHumanDismissParams, _ *cobra.Command, _ []string) {
			os.Exit(runNotifyHumanDismiss(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type dismissResultJSON struct {
	ID        int64  `json:"id"`
	Dismissed bool   `json:"dismissed"`
	Error     string `json:"error"`
}

func runNotifyHumanDismiss(p *notifyHumanDismissParams, stdout, stderr io.Writer) int {
	if p.All == (len(p.IDs) > 0) {
		fmt.Fprintln(stderr, "Error: give either decision ids or --all, exactly one")
		return rcInvalidArg
	}
	ids := make([]int64, 0, len(p.IDs))
	for _, raw := range p.IDs {
		id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(raw), "#"), 10, 64)
		if err != nil || id <= 0 {
			fmt.Fprintf(stderr, "Error: %q is not a decision id\n", raw)
			return rcInvalidArg
		}
		ids = append(ids, id)
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	payload := map[string]any{"reason": strings.TrimSpace(p.Reason)}
	if p.All {
		payload["all"] = true
	} else {
		payload["ids"] = ids
	}
	var resp struct {
		Dismissed int                 `json:"dismissed"`
		Results   []dismissResultJSON `json:"results"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/notify-human/dismiss", payload, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	failed := 0
	for _, r := range resp.Results {
		if r.Dismissed {
			fmt.Fprintf(stdout, "Dismissed decision #%d.\n", r.ID)
		} else {
			failed++
			fmt.Fprintf(stderr, "Not dismissed #%d: %s\n", r.ID, r.Error)
		}
	}
	if p.All && len(resp.Results) == 0 {
		fmt.Fprintln(stdout, "No open decisions of yours to dismiss.")
	}
	if failed > 0 && !p.All {
		return rcInvalidArg
	}
	return rcOK
}
