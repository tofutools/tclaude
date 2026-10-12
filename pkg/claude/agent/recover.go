package agent

import (
	"encoding/json"
	"errors"
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
	NewID        bool     `long:"new-id" help:"Recover as a new agent with a fresh id instead of keeping the agent's id."`
	ForceKeepID  bool     `long:"force-keep-id" help:"Keep the agent's id despite the conflicts a preview showed."`
	PreviewToken string   `long:"preview-token" optional:"true" help:"Recover: the token from a preview with the same options."`
	JSON         bool     `long:"json" help:"Print the daemon's response as JSON."`
}

func recoverCmd() *cobra.Command {
	return boa.CmdT[recoverParams]{Use: "recover", Short: "Recover an agent from a resilience snapshot this node holds (human only)",
		Long: "When a peer that sent this node resilience snapshots is lost, recover one of its agents here. Without\n" +
			"--preview-token this previews the agent that would land and prints a single-use token, valid for ten\n" +
			"minutes; run again with the token and the same options to recover it. The agent keeps its id and this\n" +
			"node becomes its home, as with a teleport; it gets the receiving group's policy and no permissions from\n" +
			"the source. --new-id recovers it as a new agent instead. When keeping the id conflicts with this node's\n" +
			"records (the agent exists or runs here, is recorded elsewhere, or a newer snapshot is held), the preview\n" +
			"lists the conflicts and you choose: --new-id, or --force-keep-id. A running agent is never taken over.\n\n" +
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
	if p.NewID && p.ForceKeepID {
		fmt.Fprintln(stderr, "Error: choose one of --new-id and --force-keep-id")
		return rcInvalidArg
	}
	body := map[string]any{"group": p.Group, "name": p.AgentName, "harness": p.Harness, "model": p.Model, "set": p.Set, "skip_history": p.SkipHistory, "preview_token": p.PreviewToken, "new_id": p.NewID, "force_keep_id": p.ForceKeepID}
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
		if printIdentityConflicts(stdout, err) {
			return MapDaemonErrorToRC(err)
		}
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return printJSON(stdout, out)
	}
	snap, _ := out["snapshot"].(map[string]any)
	fmt.Fprintf(stdout, "Snapshot of %v (%v) from %v, taken %v\n", snap["source_agent"], termClean(fmt.Sprint(snap["agent_name"])), snap["peer"], snap["taken_at"])
	if snap["identity"] == "new" {
		fmt.Fprintln(stdout, "Identity: new agent id; the original agent's id is not kept")
	} else {
		fmt.Fprintf(stdout, "Identity: keeps agent id %v; this node becomes its home\n", snap["source_agent"])
	}
	if conflicts, _ := snap["identity_conflicts"].([]any); len(conflicts) > 0 {
		fmt.Fprintln(stdout, "Keeping the id anyway (--force-keep-id) despite:")
		for _, c := range conflicts {
			fmt.Fprintf(stdout, "  - %s\n", termClean(fmt.Sprint(c)))
		}
	}
	if findings, _ := out["findings"].([]any); len(findings) > 0 {
		fmt.Fprintf(stdout, "Flag: the conversation holds %d kind(s) of suspected credentials\n", len(findings))
	}
	if op == "preview" {
		fmt.Fprintf(stdout, "Group: %s\n\nTo recover, run again with --preview-token %v (valid for ten minutes, once).\n", termClean(p.Group), out["preview_token"])
		return rcOK
	}
	spawn, _ := out["spawn"].(map[string]any)
	if snap["identity"] == "new" {
		fmt.Fprintf(stdout, "Recovered as a new agent in %s (conversation %v)\n", termClean(p.Group), spawn["conv_id"])
	} else {
		fmt.Fprintf(stdout, "Recovered agent %v in %s (conversation %v)\n", snap["source_agent"], termClean(p.Group), spawn["conv_id"])
	}
	return rcOK
}

// printIdentityConflicts explains a refused kept id and the two ways on.
func printIdentityConflicts(out io.Writer, err error) bool {
	var de *DaemonError
	if !errors.As(err, &de) || de.Code != "identity_conflict" {
		return false
	}
	var body struct {
		Agent     string   `json:"agent"`
		Conflicts []string `json:"conflicts"`
	}
	if json.Unmarshal(de.Raw, &body) != nil || len(body.Conflicts) == 0 {
		return false
	}
	fmt.Fprintf(out, "Keeping agent id %s conflicts with this node's records:\n", body.Agent)
	for _, c := range body.Conflicts {
		fmt.Fprintf(out, "  - %s\n", termClean(c))
	}
	fmt.Fprintln(out, "\nRun again with --new-id to recover a new agent, or with --force-keep-id to keep the id anyway.")
	return true
}
