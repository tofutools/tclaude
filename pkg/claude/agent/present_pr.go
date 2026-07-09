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

// presentPRCmd is `tclaude agent present-pr`: an intentional agent-authored PR
// signal for the operator dashboard, separate from best-effort statusline/gh
// discovery.
func presentPRCmd() *cobra.Command {
	return boa.CmdT[presentPRParams]{
		Use:         "present-pr",
		Short:       "Present a pull request in the operator dashboard",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *presentPRParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *presentPRParams, _ *cobra.Command, _ []string) {
			os.Exit(runPresentPR(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type presentPRParams struct {
	URL      string `pos:"true" help:"Pull request URL (http(s)) to show in the dashboard"`
	Summary  string `long:"summary" short:"s" optional:"true" help:"Optional short label/summary for the PR badge"`
	State    string `long:"state" optional:"true" help:"Optional PR state: open, merged, or closed"`
	Handled  bool   `long:"handled" optional:"true" help:"Mark this presented PR handled so it no longer appears in the dashboard"`
	Target   string `long:"target" optional:"true" help:"Act on ANOTHER agent instead of self. Selector: title, full conv-id, or 8+-char prefix. Requires agent.pr, or owning a group containing the target."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny. Self-target only."`
}

func runPresentPR(p *presentPRParams, stdout, stderr io.Writer) int {
	prURL := strings.TrimSpace(p.URL)
	if prURL == "" {
		fmt.Fprintln(stderr, "Error: a PR URL is required.")
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
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	path := "/v1/whoami/prs"
	if target != "" {
		path = "/v1/agent/" + url.PathEscape(target) + "/prs"
	}
	body := map[string]any{
		"url":     prURL,
		"summary": strings.TrimSpace(p.Summary),
		"state":   strings.TrimSpace(p.State),
		"handled": p.Handled,
	}
	var resp presentPRResp
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	printPresentPRResult(stdout, &resp)
	return rcOK
}

type presentPRResp struct {
	ConvID        string          `json:"conv_id"`
	PR            presentedPRWire `json:"pr"`
	Handled       bool            `json:"handled"`
	CallerConv    string          `json:"caller_conv,omitempty"`
	CallerAgentID string          `json:"caller_agent_id,omitempty"`
}

type presentedPRWire struct {
	URL     string `json:"url"`
	Number  int    `json:"number,omitempty"`
	Summary string `json:"summary,omitempty"`
	State   string `json:"state,omitempty"`
}

func printPresentPRResult(stdout io.Writer, resp *presentPRResp) {
	by := ""
	if resp.CallerConv != "" {
		by = " (by " + shortAgentID(resp.CallerAgentID, resp.CallerConv) + ")"
	}
	label := resp.PR.URL
	if resp.PR.Number > 0 {
		label = fmt.Sprintf("#%d %s", resp.PR.Number, resp.PR.URL)
	}
	if resp.Handled {
		fmt.Fprintf(stdout, "Marked PR handled for %s: %s%s\n", short(resp.ConvID), label, by)
		return
	}
	fmt.Fprintf(stdout, "Presented PR for %s: %s%s\n", short(resp.ConvID), label, by)
}
