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

type recoverParams struct {
	Held         string   `long:"held" optional:"true" help:"The held snapshot to recover: its offer id from tclaude agent snapshot held."`
	From         string   `long:"from" help:"The peer that sent it: peer:<instance id or label>."`
	Group        string   `long:"group" help:"The group the recovered agent joins."`
	AgentName    string   `long:"agent-name" optional:"true" help:"Name for the recovered agent."`
	Cwd          string   `long:"cwd" optional:"true" help:"Working directory; without it the recorded one is kept."`
	Harness      string   `long:"harness" optional:"true" help:"Landing harness."`
	Model        string   `long:"model" optional:"true" help:"Landing model."`
	Set          []string `long:"set" optional:"true" help:"Placeholder name=value."`
	SkipHistory  bool     `long:"skip-history" help:"Recover the definition only, without the conversation."`
	PreviewToken string   `long:"preview-token" optional:"true" help:"Recover: the token from a preview with the same options."`
	JSON         bool     `long:"json" help:"Print the daemon's response as JSON."`
}

func recoverCmd() *cobra.Command {
	return boa.CmdT[recoverParams]{Use: "recover", Short: "Recover an agent from a resilience snapshot this node holds (human only)",
		Long: "When a peer that sent this node resilience snapshots is lost, recover one of its agents here. Without\n" +
			"--preview-token this previews the agent that would land and prints a single-use token, valid for ten\n" +
			"minutes; run again with the token and the same options to recover it. The recovered agent is a new\n" +
			"agent with a new identity, the receiving group's policy and no permissions from the source; its\n" +
			"briefing marks the recovered conversation as received context. The original identity is not restored.\n\n" +
			"  tclaude agent snapshot held\n" +
			"  tclaude agent recover --held OFFER --from peer:alice --group team\n" +
			"  tclaude agent recover --held OFFER --from peer:alice --group team --preview-token TOKEN",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *recoverParams, _ *cobra.Command, _ []string) {
			os.Exit(runRecover(p, os.Stdout, os.Stderr))
		}}.ToCobra()
}

func runRecover(p *recoverParams, stdout, stderr io.Writer) int {
	if p.Held == "" {
		fmt.Fprintln(stderr, "Error: --held is required: the offer id from tclaude agent snapshot held")
		return rcInvalidArg
	}
	peer, ok := strings.CutPrefix(p.From, "peer:")
	if !ok || peer == "" {
		fmt.Fprintln(stderr, "Error: --from must be peer:<instance id or label>")
		return rcInvalidArg
	}
	op := "preview"
	if p.PreviewToken != "" {
		op = "import"
	}
	body := map[string]any{"group": p.Group, "name": p.AgentName, "harness": p.Harness, "model": p.Model, "set": p.Set, "skip_history": p.SkipHistory, "preview_token": p.PreviewToken}
	if p.Cwd != "" {
		body["cwd"] = p.Cwd
	} else {
		body["keep_paths"] = true
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var out map[string]any
	path := "/v1/resilience/held/" + url.PathEscape(peer) + "/" + url.PathEscape(p.Held) + "/" + op
	if err := DaemonRequest(http.MethodPost, path, body, &out, DaemonOpts{Timeout: boardSnapshotTimeout}); err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return printJSON(stdout, out)
	}
	snap, _ := out["snapshot"].(map[string]any)
	fmt.Fprintf(stdout, "Snapshot of %v (%v) from %v, taken %v\n", snap["source_agent"], termClean(fmt.Sprint(snap["agent_name"])), snap["peer"], snap["taken_at"])
	fmt.Fprintln(stdout, "Identity: new; the original agent's identity is not restored")
	if findings, _ := out["findings"].([]any); len(findings) > 0 {
		fmt.Fprintf(stdout, "Flag: the conversation holds %d kind(s) of suspected credentials\n", len(findings))
	}
	if op == "preview" {
		fmt.Fprintf(stdout, "Group: %s\n\nTo recover, run again with --preview-token %v (valid for ten minutes, once).\n", termClean(p.Group), out["preview_token"])
		return rcOK
	}
	spawn, _ := out["spawn"].(map[string]any)
	fmt.Fprintf(stdout, "Recovered as a new agent in %s (conversation %v)\n", termClean(p.Group), spawn["conv_id"])
	return rcOK
}
